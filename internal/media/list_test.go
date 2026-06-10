package media

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListCachedFiles(t *testing.T) {
	cacheDir := t.TempDir()
	first, err := LocalPath(cacheDir, "files/bb/second.txt")
	require.NoError(t, err)
	second, err := LocalPath(cacheDir, "files/aa/first.txt")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(first), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(second), 0o755))
	require.NoError(t, os.WriteFile(first, []byte("22"), 0o600))
	require.NoError(t, os.WriteFile(second, []byte("1"), 0o600))

	files, err := ListCachedFiles(cacheDir)
	require.NoError(t, err)
	require.Equal(t, []CachedFile{
		{Path: "files/aa/first.txt", Size: 1},
		{Path: "files/bb/second.txt", Size: 2},
	}, files)
}

func TestListCachedFilesAllowsMissingRoot(t *testing.T) {
	files, err := ListCachedFiles(t.TempDir())
	require.NoError(t, err)
	require.Empty(t, files)
}
