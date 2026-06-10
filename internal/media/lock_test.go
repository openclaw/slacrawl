package media

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWithCacheLockRequiresCacheDir(t *testing.T) {
	err := WithCacheLock(context.Background(), "", func() error {
		t.Fatal("callback should not run")
		return nil
	})
	require.ErrorContains(t, err, "cache dir is required")
}

func TestWithCacheLockWaitCanBeCanceled(t *testing.T) {
	cacheDir := t.TempDir()
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithCacheLock(context.Background(), cacheDir, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := WithCacheLock(ctx, cacheDir, func() error {
		t.Fatal("callback should not run")
		return nil
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "got %v", err)

	close(release)
	require.NoError(t, <-done)
}
