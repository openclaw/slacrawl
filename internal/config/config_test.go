package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExpandPath(t *testing.T) {
	expanded, err := ExpandPath("~/tmp/slacrawl")
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(expanded))
}

func TestResolveTokens(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("SLACK_USER_TOKEN", "xoxp-test")

	cfg := Default()
	tokens := cfg.ResolveTokens()
	require.Equal(t, "xoxb-test", tokens.Bot)
	require.Equal(t, "xapp-test", tokens.App)
	require.Equal(t, "xoxp-test", tokens.User)
}

func TestResolveTokensHonorsEnabledFlags(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("SLACK_USER_TOKEN", "xoxp-test")

	cfg := Default()
	cfg.Slack.App.Enabled = false
	cfg.Slack.User.Enabled = false

	tokens := cfg.ResolveTokens()
	require.Equal(t, "xoxb-test", tokens.Bot)
	require.Equal(t, "", tokens.App)
	require.Equal(t, "", tokens.User)
}

//nolint:gosec // Test values and environment variable names are not live secrets.
func TestResolveTokensForWorkspace(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-default")
	t.Setenv("SLACK_APP_TOKEN", "xapp-default")
	t.Setenv("SLACK_USER_TOKEN", "xoxp-default")
	t.Setenv("SLACK_ALPHA_BOT_TOKEN", "xoxb-alpha")
	t.Setenv("SLACK_ALPHA_APP_TOKEN", "xapp-alpha")
	t.Setenv("SLACK_ALPHA_USER_TOKEN", "xoxp-alpha")

	cfg := Default()
	cfg.Workspaces = []Workspace{{
		ID:           "TALPHA",
		BotTokenEnv:  "SLACK_ALPHA_BOT_TOKEN",
		AppTokenEnv:  "SLACK_ALPHA_APP_TOKEN",
		UserTokenEnv: "SLACK_ALPHA_USER_TOKEN",
	}}

	tokens := cfg.ResolveTokensForWorkspace("TALPHA")
	require.Equal(t, "xoxb-alpha", tokens.Bot)
	require.Equal(t, "xapp-alpha", tokens.App)
	require.Equal(t, "xoxp-alpha", tokens.User)
	require.Equal(t, "xoxb-default", cfg.ResolveTokensForWorkspace("TUNKNOWN").Bot)
}

func TestResolveTokensForWorkspaceUsesImplicitWorkspaceEnvNames(t *testing.T) {
	t.Setenv("SLACK_TALPHA_BOT_TOKEN", "xoxb-alpha")
	t.Setenv("SLACK_TALPHA_APP_TOKEN", "xapp-alpha")
	t.Setenv("SLACK_TALPHA_USER_TOKEN", "xoxp-alpha")

	cfg := Default()
	cfg.Workspaces = []Workspace{{ID: "TALPHA"}}

	tokens := cfg.ResolveTokensForWorkspace("TALPHA")
	require.Equal(t, "xoxb-alpha", tokens.Bot)
	require.Equal(t, "xapp-alpha", tokens.App)
	require.Equal(t, "xoxp-alpha", tokens.User)
}

func TestIncludeDMsResolved(t *testing.T) {
	cfg := Default()
	require.True(t, cfg.IncludeDMsResolved(true))
	require.False(t, cfg.IncludeDMsResolved(false))

	enabled := true
	cfg.Sync.IncludeDMs = &enabled
	require.True(t, cfg.IncludeDMsResolved(false))

	disabled := false
	cfg.Sync.IncludeDMs = &disabled
	require.False(t, cfg.IncludeDMsResolved(true))
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cfg := Default()
	cfg.WorkspaceID = "T123"
	cfg.Share.Remote = "https://example.com/private/slacrawl.git"
	cfg.Slack.MCP.Enabled = true
	cfg.Slack.MCP.BaseURL = "https://mcp.example.test"
	cfg.Slack.MCP.AuthPath = filepath.Join(dir, "auth.json")
	require.NoError(t, cfg.Save(path))

	loaded, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "T123", loaded.WorkspaceID)
	require.True(t, loaded.Slack.MCP.Enabled)
	require.Equal(t, "https://mcp.example.test", loaded.Slack.MCP.BaseURL)
	require.True(t, filepath.IsAbs(loaded.Slack.MCP.AuthPath))
	require.Equal(t, 100, loaded.Slack.MCP.PageSize)
	require.Equal(t, 20, loaded.Slack.MCP.SearchLimit)
	require.True(t, filepath.IsAbs(loaded.DBPath))
	require.True(t, filepath.IsAbs(loaded.Share.RepoPath))
	require.Equal(t, "https://example.com/private/slacrawl.git", loaded.Share.Remote)
}

func TestSaveAndLoadStdioMCPConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg := Default()
	cfg.Slack.MCP.Transport = "stdio"
	cfg.Slack.MCP.Command = "npx"
	cfg.Slack.MCP.Args = []string{"-y", "@modelcontextprotocol/server-slack"}
	require.NoError(t, cfg.Save(path))

	loaded, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "stdio", loaded.Slack.MCP.Transport)
	require.Equal(t, "npx", loaded.Slack.MCP.Command)
	require.Equal(t, []string{"-y", "@modelcontextprotocol/server-slack"}, loaded.Slack.MCP.Args)
}

func TestNormalizeRejectsStdioMCPWithoutCommand(t *testing.T) {
	cfg := Default()
	cfg.Slack.MCP.Transport = "stdio"
	cfg.Slack.MCP.Command = ""
	require.ErrorContains(t, cfg.Normalize(), "command is required")
}

func TestNormalizeRejectsMCPSearchLimitAboveConnectorMaximum(t *testing.T) {
	cfg := Default()
	cfg.Slack.MCP.SearchLimit = 21
	require.ErrorContains(t, cfg.Normalize(), "cannot exceed 20")
}

func TestNormalizeAllowsReferenceMCPSearchLimit(t *testing.T) {
	cfg := Default()
	cfg.Slack.MCP.Transport = "stdio"
	cfg.Slack.MCP.Command = "server-slack"
	cfg.Slack.MCP.SearchLimit = 100
	require.NoError(t, cfg.Normalize())
}

func TestDefaultConfigPath(t *testing.T) {
	path, err := DefaultConfigPath()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(mustHome(t), ".slacrawl", "config.toml"), path)
}

func TestNormalizeAutoDetectsDesktopPath(t *testing.T) {
	withRuntimeGOOS(t, "darwin")
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, "Library", "Application Support", "Slack"), 0o750))

	cfg := Default()
	cfg.Slack.Desktop.Path = ""
	require.NoError(t, cfg.Normalize())
	require.Equal(t, filepath.Join(home, "Library", "Application Support", "Slack"), cfg.Slack.Desktop.Path)
}

func TestNormalizeAutoDetectsLinuxDesktopPathFromXDGConfigHome(t *testing.T) {
	withRuntimeGOOS(t, "linux")
	home := t.TempDir()
	xdgConfigHome := filepath.Join(home, ".xdg-config")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)
	require.NoError(t, os.MkdirAll(filepath.Join(xdgConfigHome, "Slack"), 0o750))

	cfg := Default()
	cfg.Slack.Desktop.Path = ""
	require.NoError(t, cfg.Normalize())
	require.Equal(t, filepath.Join(xdgConfigHome, "Slack"), cfg.Slack.Desktop.Path)
}

func TestNormalizeAutoDetectsLinuxDesktopPathFromHomeConfig(t *testing.T) {
	withRuntimeGOOS(t, "linux")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".missing-xdg-config"))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".config", "Slack"), 0o750))

	cfg := Default()
	cfg.Slack.Desktop.Path = ""
	require.NoError(t, cfg.Normalize())
	require.Equal(t, filepath.Join(home, ".config", "Slack"), cfg.Slack.Desktop.Path)
}

func TestNormalizeIgnoresRelativeXDGConfigHome(t *testing.T) {
	withRuntimeGOOS(t, "linux")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", ".xdg-config")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".config", "Slack"), 0o750))

	cfg := Default()
	cfg.Slack.Desktop.Path = ""
	require.NoError(t, cfg.Normalize())
	require.Equal(t, filepath.Join(home, ".config", "Slack"), cfg.Slack.Desktop.Path)
}

func TestNormalizeLeavesDesktopPathEmptyWhenNoInstallFound(t *testing.T) {
	withRuntimeGOOS(t, "linux")
	t.Setenv("HOME", t.TempDir())

	cfg := Default()
	cfg.Slack.Desktop.Path = ""
	require.NoError(t, cfg.Normalize())
	require.Empty(t, cfg.Slack.Desktop.Path)
}

func TestNormalizeSetsDefaultWorkspaceFromWorkspaceList(t *testing.T) {
	cfg := Default()
	cfg.Workspaces = []Workspace{
		{ID: "T123"},
		{ID: "T456", Default: true},
	}
	require.NoError(t, cfg.Normalize())
	require.Equal(t, "T456", cfg.WorkspaceID)
	require.Equal(t, []string{"T123", "T456"}, cfg.WorkspaceIDs())
}

func TestNormalizeTrimsExplicitWorkspaceID(t *testing.T) {
	cfg := Default()
	cfg.WorkspaceID = " T123 "
	cfg.Workspaces = []Workspace{{ID: "T123"}}
	require.NoError(t, cfg.Normalize())
	require.Equal(t, "T123", cfg.WorkspaceID)

	_, ok := cfg.Workspace(cfg.WorkspaceID)
	require.True(t, ok)
}

func TestEnsureRuntimeDirsCreatesShareParent(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.DBPath = filepath.Join(dir, "state", "slacrawl.db")
	cfg.CacheDir = filepath.Join(dir, "cache")
	cfg.LogDir = filepath.Join(dir, "logs")
	cfg.Share.RepoPath = filepath.Join(dir, "repos", "share")
	require.NoError(t, EnsureRuntimeDirs(cfg))

	require.DirExists(t, filepath.Dir(cfg.DBPath))
	require.DirExists(t, cfg.CacheDir)
	require.DirExists(t, cfg.LogDir)
	require.DirExists(t, filepath.Dir(cfg.Share.RepoPath))
}

func mustHome(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	return home
}

func withRuntimeGOOS(t *testing.T, goos string) {
	t.Helper()
	old := runtimeGOOS
	runtimeGOOS = goos
	t.Cleanup(func() {
		runtimeGOOS = old
	})
}
