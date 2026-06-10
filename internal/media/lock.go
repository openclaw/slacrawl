package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const cacheLockName = ".slacrawl-media.lock"

func WithCacheLock(ctx context.Context, cacheDir string, fn func() error) error {
	if strings.TrimSpace(cacheDir) == "" {
		return errors.New("cache dir is required")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(cacheDir, cacheLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	unlock, err := lockCacheFile(ctx, file)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}
