package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/openclaw/slacrawl/internal/admission"
	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/media"
	"github.com/openclaw/slacrawl/internal/slackapi"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/openclaw/slacrawl/internal/syncer"
)

func (a *App) runSync(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	cfg, configErr := loadConfig(configPath)
	if configErr != nil {
		cfg = config.Default()
	}

	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	source := fs.String("source", "api", "api|bot|desktop|wiretap|mcp|connector|all|provider:<name>")
	workspaceID := fs.String("workspace", "", "workspace id")
	channels := fs.String("channels", "", "comma separated channel ids")
	excludeChannels := fs.String("exclude-channels", "", "comma separated channel names to skip during sync")
	since := fs.String("since", "", "oldest slack ts or RFC3339 timestamp")
	full := fs.Bool("full", false, "full sync")
	latestOnly := fs.Bool("latest-only", false, "skip first-time historical backfills")
	incremental := fs.Bool("incremental", false, "require local completed API history and resume only qualified intervals")
	limit := fs.Int("limit", 0, "maximum provider messages (validation imports only)")
	concurrency := fs.Int("concurrency", cfg.Sync.Concurrency, "worker count")
	withMedia := fs.Bool("with-media", cfg.FileMediaEnabled(), "fetch file media after sync")
	autoJoin := fs.Bool("auto-join", cfg.Sync.AutoJoinResolved(), "auto-join public channels during sync")
	if err := a.parseCommandFlags(fs, args); err != nil {
		return err
	}
	if configErr != nil {
		return configErr
	}
	if *limit < 0 {
		return errors.New("limit cannot be negative")
	}

	resolvedSource, err := syncer.ParseSource(*source)
	if err != nil {
		return err
	}
	workspaceSet := flagWasSet(fs, "workspace")
	resolvedWorkspaceID := resolveSyncWorkspaceID(*workspaceID, workspaceSet)
	normalizedSince, err := normalizeSinceTimestamp(*since)
	if err != nil {
		return err
	}
	if *incremental {
		if resolvedSource != syncer.SourceAPI || strings.TrimSpace(*workspaceID) == "" || len(csv(*channels)) == 0 ||
			*full || flagWasSet(fs, "since") || *latestOnly || *autoJoin || *withMedia || flagWasSet(fs, "limit") {
			return errors.New("--incremental requires API source, explicit --workspace and --channels, --auto-join=false and --with-media=false, without --full, --since, --latest-only or --limit")
		}
		if cfg.ShareEnabled() && cfg.Share.AutoUpdate {
			return errors.New("--incremental requires automatic Git-share updates to be disabled")
		}
		info, err := os.Stat(cfg.DBPath)
		if err != nil {
			return fmt.Errorf("--incremental requires an existing native archive: %w", err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return errors.New("--incremental requires an existing nonempty native archive file")
		}
	}
	runOptions := syncer.Options{
		Source:       resolvedSource,
		WorkspaceID:  resolvedWorkspaceID,
		WorkspaceSet: workspaceSet,
		Channels:     csv(*channels),
		ExcludeChannels: mergeStringSlices(
			cfg.Sync.ExcludeChannels,
			csv(*excludeChannels),
		),
		Since:       normalizedSince,
		Full:        *full,
		LatestOnly:  *latestOnly,
		Incremental: *incremental,
		Limit:       *limit,
		Concurrency: *concurrency,
		AutoJoin:    boolPtr(*autoJoin),
		APIURL:      a.apiURL,
		HTTPClient:  a.httpClient,
		Logger:      progressLogger(a.Stderr),
	}
	st, err := a.openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	if runOptions.Source != syncer.SourceDesktop {
		if err := a.autoUpdateShare(ctx, cfg, st); err != nil {
			return err
		}
	}
	summary, err := a.runSyncTargets(ctx, cfg, st, runOptions)
	if err != nil {
		return err
	}
	var mediaStats *media.FetchStats
	if *withMedia {
		stats, err := a.fetchMediaForSync(ctx, cfg, st, runOptions.WorkspaceID, runOptions.Channels)
		if err != nil {
			return err
		}
		mediaStats = &stats
	}
	status, err := st.Status(ctx)
	if err != nil {
		return err
	}
	archiveProfile, err := a.buildArchiveProfile(ctx, cfg, st)
	if err != nil {
		return err
	}
	result := map[string]any{
		"status":          status,
		"archive_profile": archiveProfile,
		"summary":         summary,
	}
	if mediaStats != nil {
		result["media"] = mediaStats
	}
	return a.writeOutput("Sync", result, format, true)
}

func progressLogger(w io.Writer) *slog.Logger {
	if w == nil {
		w = io.Discard
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return attr
		},
	}))
}

func (a *App) runTail(ctx context.Context, configPath string, args []string) error {
	cfg, configErr := loadConfig(configPath)
	if configErr != nil {
		cfg = config.Default()
	}
	fs := flag.NewFlagSet("tail", flag.ContinueOnError)
	workspaceID := fs.String("workspace", "", "workspace id")
	repairEvery := fs.String("repair-every", cfg.Sync.RepairEvery, "repair interval")
	if err := a.parseCommandFlags(fs, args); err != nil {
		return err
	}
	if configErr != nil {
		return configErr
	}
	st, err := a.openAutoUpdatingStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	repairDuration, err := time.ParseDuration(*repairEvery)
	if err != nil {
		return err
	}
	targets := resolveWorkspaceTargets(cfg, *workspaceID)
	if len(targets) == 0 {
		targets = []string{coalesce(*workspaceID, cfg.WorkspaceID)}
	}
	if len(targets) == 1 {
		return slackapi.NewWithOptions(cfg.ResolveTokensForWorkspace(targets[0]), a.apiURL, a.httpClient).WithDMPolicy(admission.FromConfig(cfg.Sync.IncludeDMs)).Tail(ctx, st, targets[0], repairDuration)
	}
	return a.runTailTargets(ctx, st, cfg, targets, repairDuration)
}

func (a *App) runWatch(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	cfg, configErr := loadConfig(configPath)
	if configErr != nil {
		cfg = config.Default()
	}
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	desktopEvery := fs.String("desktop-every", cfg.Sync.DesktopRefreshEvery, "desktop refresh interval")
	workspaceID := fs.String("workspace", "", "workspace id")
	if err := a.parseCommandFlags(fs, args); err != nil {
		return err
	}
	if configErr != nil {
		return configErr
	}
	if !cfg.Slack.Desktop.Enabled {
		return errors.New("desktop sync is disabled in config")
	}
	interval, err := time.ParseDuration(*desktopEvery)
	if err != nil {
		return err
	}
	if interval <= 0 {
		return errors.New("desktop refresh interval must be greater than zero")
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	syncOnce := func() error {
		summary, err := syncer.Run(ctx, cfg, st, syncer.Options{
			Source:          syncer.SourceDesktop,
			WorkspaceID:     strings.TrimSpace(*workspaceID),
			ExcludeChannels: cfg.Sync.ExcludeChannels,
		})
		if err != nil {
			return err
		}
		status, err := st.Status(ctx)
		if err != nil {
			return err
		}
		return a.writeOutput("Watch", map[string]any{
			"status":  status,
			"summary": summary,
		}, format, true)
	}
	if err := syncOnce(); err != nil {
		return err
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := syncOnce(); err != nil {
				return err
			}
		}
	}
}

func resolveWorkspaceTargets(cfg config.Config, requested string) []string {
	if strings.TrimSpace(requested) != "" {
		return []string{strings.TrimSpace(requested)}
	}
	if ids := cfg.WorkspaceIDs(); len(ids) > 0 {
		return ids
	}
	if cfg.WorkspaceID != "" {
		return []string{cfg.WorkspaceID}
	}
	return nil
}

func (a *App) runSyncTargets(ctx context.Context, cfg config.Config, st *store.Store, opts syncer.Options) (syncer.Summary, error) {
	targets := resolveWorkspaceTargets(cfg, opts.WorkspaceID)
	if opts.Source == syncer.SourceDesktop {
		return syncer.Run(ctx, cfg, st, opts)
	}
	if opts.Source == syncer.SourceMCP {
		workspaceID, err := resolveMCPWorkspaceID(targets)
		if err != nil {
			return syncer.Summary{}, err
		}
		opts.WorkspaceID = workspaceID
		return syncer.Run(ctx, cfg, st, opts)
	}
	if opts.Source == syncer.SourceAll && !opts.WorkspaceSet && len(targets) > 0 {
		var last syncer.Summary
		for _, workspaceID := range targets {
			runOpts := opts
			runOpts.Source = syncer.SourceAPI
			runOpts.WorkspaceID = workspaceID
			summary, err := syncer.RunWithTokens(ctx, cfg, st, runOpts, cfg.ResolveTokensForWorkspace(workspaceID))
			if err != nil {
				return syncer.Summary{}, fmt.Errorf("sync workspace %s: %w", workspaceID, err)
			}
			last = summary
		}
		runOpts := opts
		runOpts.Source = syncer.SourceDesktop
		runOpts.WorkspaceID = ""
		summary, err := syncer.Run(ctx, cfg, st, runOpts)
		if err != nil {
			return syncer.Summary{}, err
		}
		last.Desktop = summary.Desktop
		return last, nil
	}
	if len(targets) == 0 {
		return syncer.Run(ctx, cfg, st, opts)
	}

	var last syncer.Summary
	for _, workspaceID := range targets {
		runOpts := opts
		runOpts.WorkspaceID = workspaceID
		summary, err := syncer.RunWithTokens(ctx, cfg, st, runOpts, cfg.ResolveTokensForWorkspace(workspaceID))
		if err != nil {
			return syncer.Summary{}, fmt.Errorf("sync workspace %s: %w", workspaceID, err)
		}
		last = summary
	}
	return last, nil
}

func resolveMCPWorkspaceID(targets []string) (string, error) {
	switch len(targets) {
	case 1:
		return targets[0], nil
	case 0:
		return "", errors.New("workspace ID is required for MCP sync; set workspace_id or pass --workspace")
	default:
		return "", errors.New("MCP sync requires one workspace; pass --workspace when multiple workspaces are configured")
	}
}

func (a *App) runTailTargets(ctx context.Context, st *store.Store, cfg config.Config, workspaceIDs []string, repairEvery time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, len(workspaceIDs))
	var wg sync.WaitGroup
	for _, workspaceID := range workspaceIDs {
		workspaceID := workspaceID
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := slackapi.NewWithOptions(cfg.ResolveTokensForWorkspace(workspaceID), a.apiURL, a.httpClient).WithDMPolicy(admission.FromConfig(cfg.Sync.IncludeDMs)).Tail(ctx, st, workspaceID, repairEvery)
			if err != nil && !errors.Is(err, context.Canceled) {
				errCh <- fmt.Errorf("tail %s: %w", workspaceID, err)
				cancel()
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case err := <-errCh:
		return err
	case <-done:
	case <-ctx.Done():
		<-done
	}
	// A failing tail sends its error and then cancels the others, so by the
	// time done closes, errCh and ctx.Done() are both ready and a bare select
	// would pick randomly — returning "context canceled" instead of the real
	// error for a measurable fraction of failures. Drain the error first.
	select {
	case err := <-errCh:
		return err
	default:
		return ctx.Err()
	}
}

func resolveSyncWorkspaceID(workspaceID string, workspaceSet bool) string {
	if workspaceSet {
		return strings.TrimSpace(workspaceID)
	}
	return ""
}
