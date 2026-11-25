package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/openclaw/slacrawl/internal/admission"
	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/share"
	"github.com/openclaw/slacrawl/internal/store"
)

func loadConfig(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func loadConfigOrDefault(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, nil
	}
	if !os.IsNotExist(err) {
		return config.Config{}, err
	}
	cfg = config.Default()
	if err := cfg.Normalize(); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func (a *App) openStore(cfg config.Config) (*store.Store, error) {
	if cfg.Remote.Mode == "cloud" {
		return nil, errors.New("local archive access is disabled in cloud mode")
	}
	if err := config.EnsureRuntimeDirs(cfg); err != nil {
		return nil, err
	}
	return store.Open(cfg.DBPath)
}

func (a *App) openAutoUpdatingStore(ctx context.Context, cfg config.Config) (*store.Store, error) {
	st, err := a.openStore(cfg)
	if err != nil {
		return nil, err
	}
	if err := a.autoUpdateShare(ctx, cfg, st); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}

// openReadableStore preserves automatic share imports when configured, but keeps
// ordinary local archive queries from changing database state or permissions.
func (a *App) openReadableStore(ctx context.Context, cfg config.Config) (*store.Store, error) {
	if cfg.Remote.Mode == "cloud" {
		return nil, errors.New("local archive access is disabled in cloud mode")
	}
	if cfg.ShareEnabled() && cfg.Share.AutoUpdate {
		st, err := a.openAutoUpdatingStore(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if err := st.Close(); err != nil {
			return nil, fmt.Errorf("close store after automatic share update: %w", err)
		}
		return store.OpenReadOnly(cfg.DBPath)
	}
	if _, err := os.Stat(cfg.DBPath); errors.Is(err, os.ErrNotExist) {
		return a.openStore(cfg)
	} else if err != nil {
		return nil, err
	}
	return store.OpenReadOnly(cfg.DBPath)
}

func (a *App) autoUpdateShare(ctx context.Context, cfg config.Config, st *store.Store) error {
	if !cfg.ShareEnabled() || !cfg.Share.AutoUpdate {
		return nil
	}
	staleAfter, err := time.ParseDuration(cfg.Share.StaleAfter)
	if err != nil {
		return fmt.Errorf("invalid share.stale_after: %w", err)
	}
	if !share.NeedsImport(ctx, st, staleAfter) {
		return nil
	}
	policy := admission.FromConfig(cfg.Sync.IncludeDMs)
	if err := share.ValidateImportPolicy(policy); err != nil {
		return err
	}
	opts, err := shareOptions(cfg.Share.RepoPath, cfg.Share.Remote, cfg.Share.Branch, cfg.CacheDir, cfg.ShareMediaEnabled())
	if err != nil {
		return err
	}
	opts.DMPolicy = policy
	if err := share.Pull(ctx, opts); err != nil {
		return err
	}
	_, _, err = share.ImportIfChanged(ctx, st, opts)
	if errors.Is(err, share.ErrNoManifest) {
		return nil
	}
	return err
}
