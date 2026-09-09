//go:build unix

package config

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestSaveOwnerOnlyExisting(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o644} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			dir := t.TempDir()
			for key, value := range configPermissionsEnv(dir) {
				t.Setenv(key, value)
			}
			path := filepath.Join(dir, "config.toml")
			cfg := Default()
			cfg.Slack.Desktop.Enabled = false
			cfg.WorkspaceID = "TBEFORE"
			require.NoError(t, cfg.Save(path))
			require.NoError(t, os.Chmod(path, mode))
			before, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, mode, before.Mode().Perm())

			cfg.WorkspaceID = "TAFTER"
			require.NoError(t, cfg.Save(path))
			loaded, err := Load(path)
			require.NoError(t, err)
			require.Equal(t, "TAFTER", loaded.WorkspaceID)
			after, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o600), after.Mode().Perm())
		})
	}
}

func TestSaveOwnerOnlyNew(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	for _, mask := range []string{"0077", "0022"} {
		t.Run(mask, func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable,
				"-test.run=^TestSaveOwnerOnlyUmaskHelper$", "-test.v", "-test.timeout=20s")
			cmd.Dir = dir
			cmd.Env = []string{
				"SLACRAWL_CONFIG_PERMISSIONS_HELPER=1",
				"SLACRAWL_TEST_UMASK=" + mask,
				"GOMAXPROCS=2",
			}
			for key, value := range configPermissionsEnv(dir) {
				cmd.Env = append(cmd.Env, key+"="+value)
			}
			output, err := cmd.CombinedOutput()
			t.Logf("umask child output:\n%s", output)
			require.NoError(t, err)
		})
	}
}

func TestSaveOwnerOnlyUmaskHelper(t *testing.T) {
	if os.Getenv("SLACRAWL_CONFIG_PERMISSIONS_HELPER") != "1" {
		t.Skip("selected only by the child-process umask test")
	}
	require.Equal(t, "^TestSaveOwnerOnlyUmaskHelper$", flag.Lookup("test.run").Value.String())
	maskText := os.Getenv("SLACRAWL_TEST_UMASK")
	require.Contains(t, []string{"0077", "0022"}, maskText)
	mask, err := strconv.ParseInt(maskText, 8, 32)
	require.NoError(t, err)

	// Umask is process-wide: only this explicitly selected child changes it.
	oldMask := unix.Umask(int(mask))
	defer unix.Umask(oldMask)
	dir := os.Getenv("HOME")
	require.True(t, filepath.IsAbs(dir))
	path := filepath.Join(dir, "config.toml")
	require.NoFileExists(t, path)
	control := filepath.Join(dir, "creation-control")
	require.NoError(t, os.WriteFile(control, []byte("synthetic control"), 0o644))
	controlInfo, err := os.Stat(control)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644)&^os.FileMode(mask), controlInfo.Mode().Perm())

	cfg := Default()
	cfg.Slack.Desktop.Enabled = false
	cfg.WorkspaceID = "TNEW"
	require.NoError(t, cfg.Save(path))
	loaded, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "TNEW", loaded.WorkspaceID)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func configPermissionsEnv(dir string) map[string]string {
	return map[string]string{
		"HOME":            dir,
		"USERPROFILE":     dir,
		"APPDATA":         filepath.Join(dir, "appdata"),
		"LOCALAPPDATA":    filepath.Join(dir, "localappdata"),
		"XDG_CONFIG_HOME": filepath.Join(dir, "config"),
		"XDG_CACHE_HOME":  filepath.Join(dir, "cache"),
		"XDG_DATA_HOME":   filepath.Join(dir, "data"),
		"XDG_STATE_HOME":  filepath.Join(dir, "state"),
		"TMPDIR":          dir,
		"TMP":             dir,
		"TEMP":            dir,
	}
}
