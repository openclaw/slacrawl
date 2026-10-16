package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
)

func TestUsersAndChannelsHonorFlagsAfterTheQuery(t *testing.T) {
	t.Setenv("SLACRAWL_NO_UPDATE_CHECK", "1")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	dbPath := filepath.Join(dir, "slacrawl.db")
	cfg := config.Default()
	cfg.DBPath = dbPath
	cfg.CacheDir = filepath.Join(dir, "cache")
	cfg.WorkspaceID = "T1"
	cfg.Share.Remote = ""
	cfg.Share.AutoUpdate = false
	cfg.Slack.Bot.Enabled = false
	cfg.Slack.App.Enabled = false
	cfg.Slack.User.Enabled = false
	cfg.Slack.Desktop.Enabled = false
	require.NoError(t, cfg.Save(configPath))

	ctx := context.Background()
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	now := mustTime(t, "2026-03-08T18:20:43Z")
	require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{ID: "T1", Name: "one", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{ID: "T2", Name: "two", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C1", WorkspaceID: "T1", Name: "general", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C2", WorkspaceID: "T2", Name: "general", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertUser(ctx, store.User{ID: "U1", WorkspaceID: "T1", Name: "alice", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertUser(ctx, store.User{ID: "U2", WorkspaceID: "T2", Name: "bob", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.Close())

	app := &App{}
	aliceOnT2 := runJSONRows(t, app, "--config", configPath, "--json", "users", "alice", "--workspace", "T2")
	require.Empty(t, aliceOnT2)

	bob := runJSONRows(t, app, "--config", configPath, "--json", "users", "bob", "--workspace", "T2", "--limit", "5")
	require.Len(t, bob, 1)
	require.Equal(t, "T2", bob[0]["workspace_id"])
	require.Equal(t, "bob", bob[0]["name"])

	bobFirst := runJSONRows(t, app, "--config", configPath, "--json", "users", "--workspace", "T2", "bob")
	require.Equal(t, bob, bobFirst)

	generalOnT2 := runJSONRows(t, app, "--config", configPath, "--json", "channels", "general", "--workspace", "T2")
	require.Len(t, generalOnT2, 1)
	require.Equal(t, "T2", generalOnT2[0]["workspace_id"])
	require.Equal(t, "C2", generalOnT2[0]["id"])

	generalOnT1 := runJSONRows(t, app, "--config", configPath, "--json", "channels", "--workspace", "T1", "--kind", "public_channel", "general")
	require.Len(t, generalOnT1, 1)
	require.Equal(t, "T1", generalOnT1[0]["workspace_id"])
	require.Equal(t, "C1", generalOnT1[0]["id"])

	for _, args := range [][]string{
		{"users", "alice", "--workspace"},
		{"users", "5", "--limit"},
		{"channels", "general", "--workspace"},
		{"channels", "general", "--kind"},
	} {
		var stdout bytes.Buffer
		app.Stdout = &stdout
		app.Stderr = &stdout
		err := app.Run(context.Background(), append([]string{"--config", configPath}, args...))
		require.ErrorContains(t, err, "flag needs an argument: -"+strings.TrimLeft(args[len(args)-1], "-"))
		require.NotContains(t, stdout.String(), "alice")
		require.NotContains(t, stdout.String(), "bob")
	}
}

func TestLeadingHelpWinsBeforeAnIncompleteTrailingFlag(t *testing.T) {
	t.Setenv("SLACRAWL_NO_UPDATE_CHECK", "1")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	dbPath := filepath.Join(dir, "slacrawl.db")
	cfg := config.Default()
	cfg.DBPath = dbPath
	cfg.CacheDir = filepath.Join(dir, "cache")
	cfg.WorkspaceID = "T1"
	cfg.Share.Remote = ""
	cfg.Share.AutoUpdate = false
	cfg.Slack.Bot.Enabled = false
	cfg.Slack.App.Enabled = false
	cfg.Slack.User.Enabled = false
	cfg.Slack.Desktop.Enabled = false
	require.NoError(t, cfg.Save(configPath))
	ctx := context.Background()
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	now := mustTime(t, "2026-03-08T18:20:43Z")
	require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{ID: "T1", Name: "one", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertUser(ctx, store.User{ID: "U1", WorkspaceID: "T1", Name: "alice", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.Close())

	for _, args := range [][]string{
		{"users", "--help", "alice", "--workspace"},
		{"users", "-help", "alice", "--workspace"},
		{"users", "--h", "alice", "--limit"},
		{"channels", "--help", "general", "--kind"},
		{"channels", "-help", "general", "--kind"},
	} {
		t.Run(args[0], func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			app := &App{Stdout: &stdout, Stderr: &stderr}
			err := app.Run(ctx, append([]string{"--config", configPath}, args...))
			require.NoError(t, err)
			require.Contains(t, stdout.String(), "Usage of "+args[0]+":")
			require.Empty(t, stderr.String())
		})
	}

	var stdout bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stdout}
	err = app.Run(ctx, []string{"--config", configPath, "users", "alice", "--workspace"})
	require.ErrorContains(t, err, "flag needs an argument: -workspace")

	stdout.Reset()
	err = app.Run(ctx, []string{"--config", configPath, "--json", "users", "--workspace", "-help"})
	require.NoError(t, err)
	require.NotContains(t, stdout.String(), "Usage of users:")
	require.NotContains(t, stdout.String(), "alice")
}

func TestLeadingHelpWinsBeforeAValueFlag(t *testing.T) {
	t.Setenv("SLACRAWL_NO_UPDATE_CHECK", "1")
	configPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("this is not valid TOML"), 0o600))
	for _, args := range [][]string{
		{"users", "--help", "--workspace"},
		{"users", "-h", "--limit"},
		{"channels", "--help", "--kind"},
		{"channels", "-h", "--workspace"},
	} {
		t.Run(args[0]+"/"+args[1], func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			app := &App{Stdout: &stdout, Stderr: &stderr}
			err := app.Run(context.Background(), append([]string{"--config", configPath}, args...))
			require.NoError(t, err)
			require.Contains(t, stdout.String(), "Usage of "+args[0]+":")
			require.Empty(t, stderr.String())
		})
	}
}

func runJSONRows(t *testing.T, app *App, args ...string) []map[string]any {
	t.Helper()
	var stdout bytes.Buffer
	app.Stdout = &stdout
	app.Stderr = &stdout
	require.NoError(t, app.Run(context.Background(), args))
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &rows))
	return rows
}
