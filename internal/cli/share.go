package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/openclaw/slacrawl/internal/admission"
	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/share"
)

func (a *App) runPublish(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	cfg, configErr := loadConfig(configPath)
	if configErr != nil {
		cfg = config.Default()
	}
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	repoPath := fs.String("repo", cfg.Share.RepoPath, "git repo path")
	remote := fs.String("remote", cfg.Share.Remote, "git remote")
	branch := fs.String("branch", cfg.Share.Branch, "git branch")
	message := fs.String("message", "", "commit message")
	tag := fs.String("tag", "", "immutable snapshot tag")
	noCommit := fs.Bool("no-commit", false, "skip git commit")
	push := fs.Bool("push", false, "push to origin")
	noMedia := fs.Bool("no-media", !cfg.ShareMediaEnabled(), "omit cached media files")
	if err := a.parseCommandFlags(fs, args); err != nil {
		return err
	}
	if configErr != nil {
		return configErr
	}
	if fs.NArg() != 0 {
		return errors.New("publish takes no positional arguments")
	}
	if *noCommit && strings.TrimSpace(*tag) != "" {
		return errors.New("publish --tag requires a commit")
	}
	if *noCommit && *push {
		return errors.New("publish --push requires a commit")
	}
	policy := admission.FromConfig(cfg.Sync.IncludeDMs)
	if err := share.ValidateExportPolicy(policy); err != nil {
		return err
	}
	st, err := a.openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	opts, err := shareOptions(*repoPath, *remote, *branch, cfg.CacheDir, !*noMedia)
	if err != nil {
		return err
	}
	opts.Tag = strings.TrimSpace(*tag)
	opts.DMPolicy = policy
	if err := share.ValidateTag(ctx, opts); err != nil {
		return err
	}
	manifest, err := share.Export(ctx, st, opts)
	if err != nil {
		return err
	}
	committed := false
	if !*noCommit {
		committed, err = share.Commit(ctx, opts, *message)
		if err != nil {
			return err
		}
	}
	createdTag, err := share.CreateImmutableTag(ctx, opts)
	if err != nil {
		return err
	}
	if *push {
		if err := share.Push(ctx, opts); err != nil {
			return err
		}
		if err := share.MarkImported(ctx, st, manifest); err != nil {
			return err
		}
	}
	return a.writeOutput("Publish", map[string]any{
		"repo_path":    opts.RepoPath,
		"remote":       opts.Remote,
		"generated_at": manifest.GeneratedAt,
		"tables":       manifest.Tables,
		"media":        manifest.Media,
		"committed":    committed,
		"tag":          createdTag,
		"pushed":       *push,
	}, format, true)
}

func (a *App) runSubscribe(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	cfg, configErr := loadConfigOrDefault(configPath)
	if configErr != nil {
		cfg = config.Default()
	}
	fs := flag.NewFlagSet("subscribe", flag.ContinueOnError)
	repoPath := fs.String("repo", cfg.Share.RepoPath, "local clone path")
	dbPath := fs.String("db", cfg.DBPath, "database path")
	remote := fs.String("remote", cfg.Share.Remote, "git remote")
	branch := fs.String("branch", cfg.Share.Branch, "git branch")
	staleAfter := fs.String("stale-after", cfg.Share.StaleAfter, "auto-refresh age threshold")
	noAutoUpdate := fs.Bool("no-auto-update", false, "disable read-time auto refresh")
	noImport := fs.Bool("no-import", false, "skip initial import")
	noMedia := fs.Bool("no-media", !cfg.ShareMediaEnabled(), "skip restoring cached media")
	if err := a.parseCommandFlags(fs, args); err != nil {
		return err
	}
	if configErr != nil {
		return configErr
	}
	if fs.NArg() > 1 {
		return errors.New("subscribe takes at most one remote")
	}
	if fs.NArg() == 1 {
		*remote = fs.Arg(0)
	}
	if strings.TrimSpace(*remote) == "" {
		return errors.New("subscribe requires a remote")
	}
	if err := rejectUnparseableStaleAfter(*staleAfter); err != nil {
		return err
	}
	policy := admission.FromConfig(cfg.Sync.IncludeDMs)
	if !*noImport {
		if err := share.ValidateImportPolicy(policy); err != nil {
			return err
		}
	}

	cfg.Share.Remote = strings.TrimSpace(*remote)
	cfg.Share.RepoPath = *repoPath
	cfg.DBPath = *dbPath
	cfg.Share.Branch = *branch
	cfg.Share.AutoUpdate = !*noAutoUpdate
	cfg.Share.StaleAfter = *staleAfter
	shareMedia := !*noMedia
	cfg.Share.Media = &shareMedia
	cfg.Slack.Bot.Enabled = false
	cfg.Slack.App.Enabled = false
	cfg.Slack.User.Enabled = false
	cfg.Slack.Desktop.Enabled = false
	cfg.Slack.Desktop.Path = ""
	if *noImport {
		if err := cfg.Save(configPath); err != nil {
			return err
		}
		return a.writeOutput("Subscribe", map[string]any{
			"config_path": configPath,
			"repo_path":   cfg.Share.RepoPath,
			"remote":      cfg.Share.Remote,
		}, format, true)
	}

	opts, err := shareOptions(cfg.Share.RepoPath, cfg.Share.Remote, cfg.Share.Branch, cfg.CacheDir, !*noMedia)
	if err != nil {
		return err
	}
	opts.DMPolicy = policy
	if err := share.Pull(ctx, opts); err != nil {
		return err
	}
	st, err := a.openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	manifest, imported, err := share.ImportIfChanged(ctx, st, opts)
	if err != nil {
		return err
	}
	if err := cfg.Save(configPath); err != nil {
		return err
	}
	return a.writeOutput("Subscribe", map[string]any{
		"config_path":  configPath,
		"repo_path":    opts.RepoPath,
		"remote":       opts.Remote,
		"generated_at": manifest.GeneratedAt,
		"tables":       manifest.Tables,
		"media":        manifest.Media,
		"imported":     imported,
	}, format, true)
}

func rejectUnparseableStaleAfter(value string) error {
	if _, err := time.ParseDuration(value); err != nil {
		hint := "15m or 2h"
		if strings.Contains(err.Error(), `unit "d"`) {
			hint = "2160h for 90 days"
		}
		return fmt.Errorf("invalid --stale-after %q: use a duration such as %s: %w", value, hint, err)
	}
	return nil
}

func (a *App) runUpdate(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	cfg, configErr := loadConfig(configPath)
	if configErr != nil {
		cfg = config.Default()
	}
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	repoPath := fs.String("repo", cfg.Share.RepoPath, "local clone path")
	remote := fs.String("remote", cfg.Share.Remote, "git remote")
	branch := fs.String("branch", cfg.Share.Branch, "git branch")
	ref := fs.String("ref", "", "historical git ref to import")
	restore := fs.Bool("restore", false, "exactly replace snapshot tables instead of merging")
	noMedia := fs.Bool("no-media", !cfg.ShareMediaEnabled(), "skip restoring cached media")
	if err := a.parseCommandFlags(fs, args); err != nil {
		return err
	}
	if configErr != nil {
		return configErr
	}
	if fs.NArg() != 0 {
		return errors.New("update takes no positional arguments")
	}
	if strings.TrimSpace(*ref) != "" && !*restore {
		return errors.New("update --ref requires --restore because historical snapshots replace local rows")
	}
	policy := admission.FromConfig(cfg.Sync.IncludeDMs)
	if err := share.ValidateImportPolicy(policy); err != nil {
		return err
	}
	st, err := a.openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	opts, err := shareOptions(*repoPath, *remote, *branch, cfg.CacheDir, !*noMedia)
	if err != nil {
		return err
	}
	opts.DMPolicy = policy
	var manifest share.Manifest
	var imported bool
	if strings.TrimSpace(*ref) == "" {
		if err := share.Pull(ctx, opts); err != nil {
			return err
		}
		if *restore {
			manifest, err = share.Restore(ctx, st, opts)
			imported = err == nil
		} else {
			manifest, imported, err = share.ImportIfChanged(ctx, st, opts)
		}
		if err != nil {
			return err
		}
	} else {
		manifest, err = share.RestoreAt(ctx, st, opts, *ref)
		if err != nil {
			return err
		}
		imported = true
	}
	return a.writeOutput("Update", map[string]any{
		"repo_path":    opts.RepoPath,
		"remote":       opts.Remote,
		"generated_at": manifest.GeneratedAt,
		"tables":       manifest.Tables,
		"media":        manifest.Media,
		"imported":     imported,
		"ref":          strings.TrimSpace(*ref),
		"restore":      *restore,
	}, format, true)
}

func shareOptions(repoPath, remote, branch, cacheDir string, includeMedia bool) (share.Options, error) {
	expandedRepo, err := config.ExpandPath(repoPath)
	if err != nil {
		return share.Options{}, err
	}
	expandedCache, err := config.ExpandPath(cacheDir)
	if err != nil {
		return share.Options{}, err
	}
	if strings.TrimSpace(branch) == "" {
		branch = "main"
	}
	return share.Options{
		RepoPath:     expandedRepo,
		Remote:       strings.TrimSpace(remote),
		Branch:       branch,
		CacheDir:     expandedCache,
		IncludeMedia: includeMedia,
	}, nil
}
