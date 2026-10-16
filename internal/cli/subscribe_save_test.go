package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/config"
)

func TestSubscribeLeavesConfigUntouchedWhenPullFails(t *testing.T) {
	t.Setenv("SLACRAWL_NO_UPDATE_CHECK", "1")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	cfg := config.Default()
	cfg.DBPath = filepath.Join(dir, "slacrawl.db")
	cfg.CacheDir = filepath.Join(dir, "cache")
	cfg.WorkspaceID = "TKEEP"
	cfg.Share.Remote = ""
	cfg.Share.AutoUpdate = false
	cfg.Slack.Bot.Enabled = true
	cfg.Slack.App.Enabled = true
	cfg.Slack.User.Enabled = true
	cfg.Slack.Desktop.Enabled = true
	cfg.Slack.Desktop.Path = filepath.Join(dir, "Slack")
	require.NoError(t, cfg.Save(configPath))
	before, err := os.ReadFile(configPath)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var output bytes.Buffer
	app := &App{Stdout: &output, Stderr: &output}
	err = app.Run(ctx, []string{
		"--config", configPath, "subscribe",
		"--repo", filepath.Join(dir, "share"),
		"--db", filepath.Join(dir, "reader.db"),
		filepath.Join(dir, "missing.git"),
	})
	require.Error(t, err)
	after, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))

	loaded, err := config.Load(configPath)
	require.NoError(t, err)
	require.Equal(t, "TKEEP", loaded.WorkspaceID)
	require.True(t, loaded.Slack.Bot.Enabled)
	require.True(t, loaded.Slack.App.Enabled)
	require.True(t, loaded.Slack.User.Enabled)
	require.True(t, loaded.Slack.Desktop.Enabled)
	require.Equal(t, filepath.Join(dir, "Slack"), loaded.Slack.Desktop.Path)
	require.Empty(t, loaded.Share.Remote)
}
