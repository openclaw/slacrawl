package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
)

func TestSubscribeRejectsStaleAfterBeforeSaving(t *testing.T) {
	t.Setenv("SLACRAWL_NO_UPDATE_CHECK", "1")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	cfg := config.Default()
	cfg.DBPath = filepath.Join(dir, "slacrawl.db")
	cfg.CacheDir = filepath.Join(dir, "cache")
	cfg.WorkspaceID = "TKEEP"
	cfg.Share.StaleAfter = "15m"
	cfg.Slack.Desktop.Enabled = true
	cfg.Slack.Desktop.Path = filepath.Join(dir, "Slack")
	require.NoError(t, cfg.Save(configPath))
	before, err := os.ReadFile(configPath)
	require.NoError(t, err)

	var output bytes.Buffer
	app := &App{Stdout: &output, Stderr: &output}
	err = app.Run(context.Background(), []string{
		"--config", configPath, "subscribe",
		"--stale-after", "90d",
		"--repo", filepath.Join(dir, "share"),
		filepath.Join(dir, "missing.git"),
	})
	require.ErrorContains(t, err, `invalid --stale-after "90d"`)
	require.ErrorContains(t, err, "2160h for 90 days")
	after, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))

	loaded, err := config.Load(configPath)
	require.NoError(t, err)
	require.Equal(t, "15m", loaded.Share.StaleAfter)
	require.Equal(t, filepath.Join(dir, "Slack"), loaded.Slack.Desktop.Path)
}

func TestSubscribeRejectsStoredDayDurationWithoutRewritingArchive(t *testing.T) {
	t.Setenv("SLACRAWL_NO_UPDATE_CHECK", "1")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	dbPath := filepath.Join(dir, "archive.db")
	seedArchiveStore(t, dbPath, "upgrade-row")
	cfg := config.Default()
	cfg.DBPath = dbPath
	cfg.CacheDir = filepath.Join(dir, "cache")
	cfg.WorkspaceID = "TKEEP"
	cfg.Share.StaleAfter = "90d"
	cfg.Share.AutoUpdate = false
	cfg.Slack.Desktop.Enabled = true
	cfg.Slack.Desktop.Path = filepath.Join(dir, "Slack")
	require.NoError(t, cfg.Save(configPath))
	before, err := os.ReadFile(configPath)
	require.NoError(t, err)

	var output bytes.Buffer
	app := &App{Stdout: &output, Stderr: &output}
	err = app.Run(context.Background(), []string{
		"--config", configPath, "subscribe",
		"--repo", filepath.Join(dir, "share"),
		filepath.Join(dir, "missing.git"),
	})
	require.ErrorContains(t, err, `invalid --stale-after "90d"`)
	require.ErrorContains(t, err, "2160h for 90 days")
	after, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))

	st, err := store.OpenReadOnly(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	rows, err := st.QueryReadOnly(context.Background(), "select text from messages")
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"text": "upgrade-row"}}, rows)
}
