package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/openclaw/slacrawl/internal/media"
	"github.com/openclaw/slacrawl/internal/store"
)

type purgeResponse struct {
	Cutoff              time.Time `json:"cutoff"`
	WorkspaceID         string    `json:"workspace_id,omitempty"`
	DryRun              bool      `json:"dry_run"`
	Messages            int64     `json:"messages"`
	MessageEvents       int64     `json:"message_events"`
	MessageFiles        int64     `json:"message_files"`
	Mentions            int64     `json:"mentions"`
	EmbeddingJobs       int64     `json:"embedding_jobs"`
	FTSEntries          int64     `json:"fts_entries"`
	CachedMediaFiles    int64     `json:"cached_media_files"`
	CachedMediaBytes    int64     `json:"cached_media_bytes"`
	CachedMediaDeleted  int64     `json:"cached_media_deleted"`
	CachedMediaMissing  int64     `json:"cached_media_missing"`
	CachedMediaRetained int64     `json:"cached_media_retained"`
	CachedMediaFailures []string  `json:"cached_media_failures,omitempty"`
	KeepMedia           bool      `json:"keep_media"`
	Vacuumed            bool      `json:"vacuumed"`
}

func (a *App) runPurge(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	if hasHelpArg(args) {
		printPurgeUsage(a.Stdout)
		return nil
	}
	fs := flag.NewFlagSet("purge", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	before := fs.String("before", "", "delete messages before RFC3339 timestamp or YYYY-MM-DD")
	olderThan := fs.String("older-than", "", "delete messages older than duration, such as 90d or 2160h")
	workspaceID := fs.String("workspace", "", "limit purge to workspace id")
	force := fs.Bool("force", false, "execute deletion instead of previewing")
	keepMedia := fs.Bool("keep-media", false, "retain unreferenced cached media files")
	vacuum := fs.Bool("vacuum", false, "compact the SQLite database after deletion")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("purge does not accept positional arguments")
	}
	if (*before == "") == (*olderThan == "") {
		return errors.New("exactly one of --before or --older-than is required")
	}
	if *vacuum && !*force {
		return errors.New("--vacuum requires --force")
	}
	workspaceSet := false
	fs.Visit(func(item *flag.Flag) {
		if item.Name == "workspace" {
			workspaceSet = true
		}
	})
	workspace := strings.TrimSpace(*workspaceID)
	if workspaceSet && workspace == "" {
		return errors.New("--workspace cannot be empty")
	}

	now := a.nowUTC()
	cutoff, err := resolvePurgeCutoff(now, *before, *olderThan)
	if err != nil {
		return err
	}
	if cutoff.After(now) {
		return errors.New("purge cutoff cannot be in the future")
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	st, err := a.openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	opts := store.PurgeOptions{
		Before:      cutoff,
		WorkspaceID: workspace,
		Delete:      *force,
	}
	var report store.PurgeReport
	var mediaDeleted, mediaMissing, mediaRetained int64
	var mediaFailures []string
	var mediaErrors []error
	var postCommitErr error
	databaseCommitted := false
	if *force && !*keepMedia {
		if strings.TrimSpace(cfg.CacheDir) == "" {
			preview, err := st.PurgeMessages(ctx, store.PurgeOptions{
				Before:      cutoff,
				WorkspaceID: opts.WorkspaceID,
			})
			if err != nil {
				return err
			}
			if len(preview.Media) > 0 {
				return errors.New("cache dir is required to remove cached media; use --keep-media to retain it")
			}
			opts.RequireNoMedia = true
			report, err = st.PurgeMessages(ctx, opts)
			if err != nil {
				return fmt.Errorf("purge without cache dir: %w", err)
			}
			databaseCommitted = true
		} else {
			err := media.WithCacheLock(ctx, cfg.CacheDir, func() error {
				preview, err := st.PurgeMessages(ctx, store.PurgeOptions{
					Before:      cutoff,
					WorkspaceID: opts.WorkspaceID,
				})
				if err != nil {
					return err
				}
				for _, item := range preview.Media {
					if err := media.ValidateCachedFile(cfg.CacheDir, item.Path); err != nil {
						return fmt.Errorf("validate cached media %q: %w", item.Path, err)
					}
				}
				report, err = st.PurgeMessages(ctx, opts)
				if err != nil {
					return err
				}
				databaseCommitted = true
				cachedFiles, err := media.ListCachedFiles(cfg.CacheDir)
				if err != nil {
					return fmt.Errorf("list cached media: %w", err)
				}
				cachedCandidates := make([]store.PurgeMedia, 0, len(cachedFiles))
				for _, item := range cachedFiles {
					cachedCandidates = append(cachedCandidates, store.PurgeMedia{Path: item.Path, Size: item.Size})
				}
				candidates := mergePurgeMedia(report.Media, cachedCandidates)
				mediaRetained, err = st.WithUnreferencedPurgeMedia(ctx, candidates, func(items []store.PurgeMedia) error {
					report.Media = append(report.Media[:0], items...)
					mediaDeleted, mediaMissing, mediaFailures, _, mediaErrors = removePurgeMedia(cfg.CacheDir, items)
					return nil
				})
				return err
			})
			if err != nil {
				if !databaseCommitted {
					return err
				}
				postCommitErr = err
			}
		}
	} else {
		report, err = st.PurgeMessages(ctx, opts)
		if err != nil {
			return err
		}
		databaseCommitted = *force
		if !*force && !*keepMedia && strings.TrimSpace(cfg.CacheDir) != "" {
			selectedMedia := append([]store.PurgeMedia(nil), report.Media...)
			err := media.WithCacheLock(ctx, cfg.CacheDir, func() error {
				cachedFiles, err := media.ListCachedFiles(cfg.CacheDir)
				if err != nil {
					return fmt.Errorf("list cached media: %w", err)
				}
				candidates := make([]store.PurgeMedia, 0, len(cachedFiles))
				for _, item := range cachedFiles {
					candidates = append(candidates, store.PurgeMedia{Path: item.Path, Size: item.Size})
				}
				unreferenced, retained, err := st.UnreferencedPurgeMediaAfter(ctx, mergePurgeMedia(selectedMedia, candidates), selectedMedia)
				if err != nil {
					return err
				}
				mediaRetained = retained
				report.Media = unreferenced
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
	response := purgeResponseFromStore(cutoff, opts.WorkspaceID, !*force, *keepMedia, report)
	response.CachedMediaDeleted = mediaDeleted
	response.CachedMediaMissing = mediaMissing
	response.CachedMediaRetained = mediaRetained
	response.CachedMediaFailures = mediaFailures
	if *vacuum {
		if err := st.Vacuum(ctx); err != nil {
			if !databaseCommitted {
				return fmt.Errorf("vacuum database: %w", err)
			}
			postCommitErr = errors.Join(postCommitErr, fmt.Errorf("vacuum database: %w", err))
		} else {
			response.Vacuumed = true
		}
	}
	if err := a.writeOutput("Purge", response, format, true); err != nil {
		if databaseCommitted {
			return fmt.Errorf("database purge committed; write output: %w", err)
		}
		return err
	}
	var finalErrors []error
	if postCommitErr != nil {
		finalErrors = append(finalErrors, postCommitErr)
	}
	if len(mediaErrors) > 0 {
		finalErrors = append(finalErrors, fmt.Errorf("failed to remove %d cached media files: %w", len(mediaErrors), errors.Join(mediaErrors...)))
	}
	if len(finalErrors) > 0 {
		return fmt.Errorf("database purge committed; post-purge steps failed: %w", errors.Join(finalErrors...))
	}
	return nil
}

func mergePurgeMedia(groups ...[]store.PurgeMedia) []store.PurgeMedia {
	byPath := make(map[string]store.PurgeMedia)
	for _, group := range groups {
		for _, item := range group {
			if existing, ok := byPath[item.Path]; !ok || item.Size > existing.Size {
				byPath[item.Path] = item
			}
		}
	}
	items := make([]store.PurgeMedia, 0, len(byPath))
	for _, item := range byPath {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	return items
}

func removePurgeMedia(cacheDir string, items []store.PurgeMedia) (deleted, missing int64, failures, completed []string, errs []error) {
	for _, item := range items {
		removed, err := media.RemoveCachedFile(cacheDir, item.Path)
		if err != nil {
			failures = append(failures, item.Path)
			errs = append(errs, fmt.Errorf("%s: %w", item.Path, err))
			continue
		}
		if removed {
			deleted++
		} else {
			missing++
		}
		completed = append(completed, item.Path)
	}
	return deleted, missing, failures, completed, errs
}

func resolvePurgeCutoff(now time.Time, before, olderThan string) (time.Time, error) {
	if strings.TrimSpace(before) != "" {
		value := strings.TrimSpace(before)
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
			if parsed, err := time.Parse(layout, value); err == nil {
				return parsed.UTC(), nil
			}
		}
		return time.Time{}, fmt.Errorf("invalid --before %q: use RFC3339 or YYYY-MM-DD", before)
	}
	duration, err := parseLookback(olderThan)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --older-than %q: %w", olderThan, err)
	}
	if duration <= 0 {
		return time.Time{}, errors.New("--older-than must be greater than zero")
	}
	return now.Add(-duration), nil
}

func purgeResponseFromStore(cutoff time.Time, workspaceID string, dryRun, keepMedia bool, report store.PurgeReport) purgeResponse {
	response := purgeResponse{
		Cutoff:           cutoff,
		WorkspaceID:      workspaceID,
		DryRun:           dryRun,
		Messages:         report.Messages,
		MessageEvents:    report.MessageEvents,
		MessageFiles:     report.MessageFiles,
		Mentions:         report.Mentions,
		EmbeddingJobs:    report.EmbeddingJobs,
		FTSEntries:       report.FTSEntries,
		CachedMediaFiles: int64(len(report.Media)),
		KeepMedia:        keepMedia,
	}
	for _, item := range report.Media {
		response.CachedMediaBytes += item.Size
	}
	return response
}

func printPurgeUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage:
  slacrawl purge (--before <time> | --older-than <duration>) [flags]

Preview is the default. Pass --force to delete matching messages and all
message-owned database rows in one transaction.

Flags:
  -before string       RFC3339 timestamp or YYYY-MM-DD cutoff
  -older-than string   relative cutoff, such as 90d or 2160h
  -workspace string    limit purge to one workspace
  -force               execute deletion
  -keep-media          retain unreferenced cached media files
  -vacuum              compact the database after deletion; requires --force
`)
}
