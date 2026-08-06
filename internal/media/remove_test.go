package media

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRemoveCachedFile(t *testing.T) {
	cacheDir := t.TempDir()
	mediaPath := "files/aa/file.txt"
	target, err := LocalPath(cacheDir, mediaPath)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(target, []byte("cached"), 0o600))

	require.NoError(t, ValidateCachedFile(cacheDir, mediaPath))
	deleted, err := RemoveCachedFile(cacheDir, mediaPath)
	require.NoError(t, err)
	require.True(t, deleted)
	require.NoFileExists(t, target)

	deleted, err = RemoveCachedFile(cacheDir, mediaPath)
	require.NoError(t, err)
	require.False(t, deleted)
}

func TestValidateCachedFileRequiresCacheDir(t *testing.T) {
	err := ValidateCachedFile("", "files/aa/file.txt")
	require.ErrorContains(t, err, "media base dir is required")
}

func TestRemoveCachedFileRejectsSymlinkedParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional privileges on Windows")
	}
	cacheDir := t.TempDir()
	outside := t.TempDir()
	root := filepath.Join(cacheDir, cacheSubdir)
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "files")))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "file.txt"), []byte("outside"), 0o600))

	err := ValidateCachedFile(cacheDir, "files/file.txt")
	require.ErrorContains(t, err, "unsafe media path component")
	_, err = RemoveCachedFile(cacheDir, "files/file.txt")
	require.ErrorContains(t, err, "unsafe media path component")
	require.FileExists(t, filepath.Join(outside, "file.txt"))
}

func TestCachedFileAllowsSymlinkedMediaRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional privileges on Windows")
	}
	// A large attachment cache is often parked on another volume behind a
	// symlinked media root. Fetch has always written through one, so rejecting
	// it on the read side left purge failing forever on a cache slacrawl
	// itself keeps filling.
	cacheDir := t.TempDir()
	storage := t.TempDir()
	require.NoError(t, os.Symlink(storage, filepath.Join(cacheDir, cacheSubdir)))

	mediaPath := "files/aa/file.txt"
	target, err := LocalPath(cacheDir, mediaPath)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(target, []byte("cached"), 0o600))

	listed, err := ListCachedFiles(cacheDir)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, mediaPath, listed[0].Path)

	require.NoError(t, ValidateCachedFile(cacheDir, mediaPath))
	deleted, err := RemoveCachedFile(cacheDir, mediaPath)
	require.NoError(t, err)
	require.True(t, deleted)
	require.NoFileExists(t, filepath.Join(storage, "files", "aa", "file.txt"))
}
