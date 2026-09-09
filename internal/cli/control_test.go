package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/openclaw/crawlkit/control"
	"github.com/openclaw/slacrawl/internal/config"
	"github.com/stretchr/testify/require"
)

func TestControlManifest(t *testing.T) {
	manifest := controlManifest("/tmp/slacrawl-config.toml")
	defaults := config.Default()

	require.Equal(t, control.SchemaVersion, manifest.SchemaVersion)
	require.Equal(t, "slacrawl", manifest.ID)
	require.Equal(t, "Slack Crawl", manifest.DisplayName)
	require.Equal(t, "Local-first Slack archive crawler.", manifest.Description)
	require.Equal(t, "number", manifest.Branding.SymbolName)
	require.Equal(t, "#4a154b", manifest.Branding.AccentColor)
	require.Equal(t, "com.tinyspeck.slackmacgap", manifest.Branding.BundleIdentifier)
	require.Equal(t, "/tmp/slacrawl-config.toml", manifest.Paths.DefaultConfig)
	require.Equal(t, "SLACRAWL_CONFIG", manifest.Paths.ConfigEnv)
	require.Equal(t, defaults.DBPath, manifest.Paths.DefaultDatabase)
	require.Equal(t, defaults.CacheDir, manifest.Paths.DefaultCache)
	require.Equal(t, defaults.LogDir, manifest.Paths.DefaultLogs)
	require.Equal(t, defaults.Share.RepoPath, manifest.Paths.DefaultShare)
	require.Equal(t, []string{"metadata", "doctor", "status", "sync", "search", "watch", "tap", "tui", "sql", "git-share"}, manifest.Capabilities)
	require.Equal(t, control.Privacy{
		ContainsPrivateMessages: true,
		LocalOnlyScopes:         []string{"slack", "desktop-cache", "sqlite", "git-share"},
	}, manifest.Privacy)
	require.Equal(t, map[string]control.Command{
		"doctor":      {Title: "Doctor", Argv: []string{"slacrawl", "--json", "doctor"}, JSON: true},
		"status":      {Title: "Status", Argv: []string{"slacrawl", "status", "--json"}, JSON: true},
		"sync":        {Title: "Sync", Argv: []string{"slacrawl", "--json", "sync", "--source", "all", "--latest-only"}, JSON: true, Mutates: true},
		"search":      {Title: "Search", Argv: []string{"slacrawl", "--json", "search"}, JSON: true},
		"tap":         {Title: "Import desktop cache", Argv: []string{"slacrawl", "--json", "sync", "--source", "desktop"}, JSON: true, Mutates: true},
		"tui":         {Title: "Terminal browser", Argv: []string{"slacrawl", "tui"}},
		"tui-json":    {Title: "Terminal browser rows", Argv: []string{"slacrawl", "tui", "--json"}, JSON: true},
		"publish":     {Title: "Publish share", Argv: []string{"slacrawl", "--json", "publish"}, JSON: true, Mutates: true},
		"subscribe":   {Title: "Subscribe share", Argv: []string{"slacrawl", "--json", "subscribe"}, JSON: true, Mutates: true},
		"update":      {Title: "Update share", Argv: []string{"slacrawl", "--json", "update"}, JSON: true, Mutates: true},
		"legacy-json": {Title: "Legacy JSON flag", Argv: []string{"slacrawl", "--json"}, JSON: true, Legacy: true},
	}, manifest.Commands)
}

func TestManifestStatusExecutesNormalizedContract(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	var stdout bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stdout}
	ctx := context.Background()
	require.NoError(t, app.Run(ctx, []string{"--config", configPath, "init", "--db", filepath.Join(dir, "archive.db")}))
	stdout.Reset()
	argv := controlManifest(configPath).Commands["status"].Argv
	args := append([]string{"--config", configPath}, argv[1:]...)
	require.NoError(t, app.Run(ctx, args))
	var normalized map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &normalized))
	require.Equal(t, control.SchemaVersion, normalized["schema_version"])
	require.Equal(t, "slacrawl", normalized["app_id"])

	stdout.Reset()
	require.NoError(t, app.Run(ctx, []string{"--config", configPath, "--json", "status"}))
	var legacy map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &legacy))
	require.Equal(t, float64(0), legacy["messages"])
	require.NotContains(t, legacy, "schema_version")
}
