package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	crawlconfig "github.com/openclaw/crawlkit/config"
)

const (
	defaultDirName     = ".slacrawl"
	defaultDesktopPath = "~/Library/Containers/com.tinyspeck.slackmacgap/Data/Library/Application Support/Slack"
	directDesktopPath  = "~/Library/Application Support/Slack"
)

type Config struct {
	Version     int          `toml:"version"`
	WorkspaceID string       `toml:"workspace_id"`
	Workspaces  []Workspace  `toml:"workspaces"`
	DBPath      string       `toml:"db_path"`
	CacheDir    string       `toml:"cache_dir"`
	LogDir      string       `toml:"log_dir"`
	Slack       SlackConfig  `toml:"slack"`
	Sync        SyncConfig   `toml:"sync"`
	Search      SearchConfig `toml:"search"`
	Share       ShareConfig  `toml:"share"`
}

type SlackConfig struct {
	Bot     TokenConfig   `toml:"bot"`
	App     TokenConfig   `toml:"app"`
	User    TokenConfig   `toml:"user"`
	Desktop DesktopConfig `toml:"desktop"`
	MCP     MCPConfig     `toml:"mcp"`
}

type Workspace struct {
	ID           string      `toml:"id"`
	Default      bool        `toml:"default"`
	BotTokenEnv  string      `toml:"bot_token_env"`
	AppTokenEnv  string      `toml:"app_token_env"`
	UserTokenEnv string      `toml:"user_token_env"`
	Slack        SlackConfig `toml:"slack"`
}

type TokenConfig struct {
	Enabled  bool   `toml:"enabled"`
	TokenEnv string `toml:"token_env"`
}

type DesktopConfig struct {
	Enabled bool   `toml:"enabled"`
	Path    string `toml:"path"`
}

type MCPConfig struct {
	Enabled         bool     `toml:"enabled"`
	Transport       string   `toml:"transport"`
	BaseURL         string   `toml:"base_url"`
	Command         string   `toml:"command"`
	Args            []string `toml:"args"`
	AuthPath        string   `toml:"auth_path"`
	TokenEnv        string   `toml:"token_env"`
	AccountIDEnv    string   `toml:"account_id_env"`
	ConnectorID     string   `toml:"connector_id"`
	ChannelTypes    string   `toml:"channel_types"`
	PageSize        int      `toml:"page_size"`
	SearchLimit     int      `toml:"search_limit"`
	MaxPages        int      `toml:"max_pages"`
	ProtocolVersion string   `toml:"protocol_version"`
}

type SyncConfig struct {
	Concurrency         int      `toml:"concurrency"`
	RepairEvery         string   `toml:"repair_every"`
	DesktopRefreshEvery string   `toml:"desktop_refresh_every"`
	FullHistory         bool     `toml:"full_history"`
	IncludeDMs          *bool    `toml:"include_dms"`
	AutoJoin            *bool    `toml:"auto_join"`
	ExcludeChannels     []string `toml:"exclude_channels"`
	FileMedia           *bool    `toml:"file_media"`
	MaxFileBytes        int64    `toml:"max_file_bytes"`
}

// AutoJoinResolved returns whether the bot should auto-join public channels
// it encounters during sync. Defaults to true for backwards compatibility.
func (s SyncConfig) AutoJoinResolved() bool {
	if s.AutoJoin == nil {
		return true
	}
	return *s.AutoJoin
}

type SearchConfig struct {
	DefaultMode string `toml:"default_mode"`
}

type ShareConfig struct {
	Remote     string `toml:"remote"`
	RepoPath   string `toml:"repo_path"`
	Branch     string `toml:"branch"`
	AutoUpdate bool   `toml:"auto_update"`
	StaleAfter string `toml:"stale_after"`
	Media      *bool  `toml:"media"`
}

type Tokens struct {
	Bot  string
	App  string
	User string
}

var appConfig = crawlconfig.App{Name: "slacrawl", BaseDir: "~/" + defaultDirName, LegacyBaseDir: "~/" + defaultDirName}

func Default() Config {
	paths, err := appConfig.DefaultPaths()
	if err != nil {
		base := "~/" + defaultDirName
		paths = crawlconfig.Paths{
			DBPath:   filepath.ToSlash(filepath.Join(base, "slacrawl.db")),
			CacheDir: filepath.ToSlash(filepath.Join(base, "cache")),
			LogDir:   filepath.ToSlash(filepath.Join(base, "logs")),
			ShareDir: filepath.ToSlash(filepath.Join(base, "share")),
		}
	}
	return Config{
		Version:  1,
		DBPath:   filepath.ToSlash(paths.DBPath),
		CacheDir: filepath.ToSlash(paths.CacheDir),
		LogDir:   filepath.ToSlash(paths.LogDir),
		Slack: SlackConfig{
			Bot:  TokenConfig{Enabled: true, TokenEnv: "SLACK_BOT_TOKEN"},
			App:  TokenConfig{Enabled: true, TokenEnv: "SLACK_APP_TOKEN"},
			User: TokenConfig{Enabled: true, TokenEnv: "SLACK_USER_TOKEN"},
			Desktop: DesktopConfig{
				Enabled: true,
				Path:    "",
			},
			MCP: MCPConfig{
				Enabled:         true,
				Transport:       "http",
				BaseURL:         "https://chatgpt.com/backend-api/wham/apps",
				AuthPath:        "~/.codex/auth.json",
				TokenEnv:        "CODEX_APPS_ACCESS_TOKEN",
				AccountIDEnv:    "CODEX_APPS_ACCOUNT_ID",
				ChannelTypes:    "public_channel,private_channel",
				PageSize:        100,
				SearchLimit:     20,
				MaxPages:        250,
				ProtocolVersion: "2025-03-26",
			},
		},
		Sync: SyncConfig{
			Concurrency:         4,
			RepairEvery:         "30m",
			DesktopRefreshEvery: "5m",
			FullHistory:         true,
			MaxFileBytes:        100 << 20,
		},
		Search: SearchConfig{
			DefaultMode: "auto",
		},
		Share: ShareConfig{
			RepoPath:   filepath.ToSlash(paths.ShareDir),
			Branch:     "main",
			AutoUpdate: true,
			StaleAfter: "15m",
		},
	}
}

func DefaultConfigPath() (string, error) {
	paths, err := appConfig.DefaultPaths()
	return paths.ConfigPath, err
}

func Load(path string) (Config, error) {
	cfg := Default()
	if err := crawlconfig.LoadTOML(path, &cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Normalize(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Save(path string) error {
	if err := c.Normalize(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return crawlconfig.WriteTOML(path, c, 0o644)
}

func (c *Config) Normalize() error {
	if c.Version == 0 {
		c.Version = 1
	}
	if c.Sync.Concurrency <= 0 {
		c.Sync.Concurrency = 4
	}
	if c.Sync.MaxFileBytes <= 0 {
		c.Sync.MaxFileBytes = 100 << 20
	}
	if c.Search.DefaultMode == "" {
		c.Search.DefaultMode = "auto"
	}
	if c.Share.RepoPath == "" {
		c.Share.RepoPath = Default().Share.RepoPath
	}
	if c.Share.Branch == "" {
		c.Share.Branch = "main"
	}
	if c.Share.StaleAfter == "" {
		c.Share.StaleAfter = "15m"
	}
	if c.Sync.DesktopRefreshEvery == "" {
		c.Sync.DesktopRefreshEvery = "5m"
	}
	defaults := Default().Slack.MCP
	if c.Slack.MCP.Transport == "" {
		c.Slack.MCP.Transport = defaults.Transport
	}
	c.Slack.MCP.Transport = strings.ToLower(strings.TrimSpace(c.Slack.MCP.Transport))
	if c.Slack.MCP.Transport != "http" && c.Slack.MCP.Transport != "stdio" {
		return fmt.Errorf("slack.mcp.transport must be http or stdio, got %q", c.Slack.MCP.Transport)
	}
	if c.Slack.MCP.BaseURL == "" {
		c.Slack.MCP.BaseURL = defaults.BaseURL
	}
	if c.Slack.MCP.Transport == "stdio" && strings.TrimSpace(c.Slack.MCP.Command) == "" {
		return errors.New("slack.mcp.command is required for stdio transport")
	}
	if c.Slack.MCP.AuthPath == "" {
		c.Slack.MCP.AuthPath = defaults.AuthPath
	}
	if c.Slack.MCP.TokenEnv == "" {
		c.Slack.MCP.TokenEnv = defaults.TokenEnv
	}
	if c.Slack.MCP.AccountIDEnv == "" {
		c.Slack.MCP.AccountIDEnv = defaults.AccountIDEnv
	}
	if c.Slack.MCP.ChannelTypes == "" {
		c.Slack.MCP.ChannelTypes = defaults.ChannelTypes
	}
	if c.Slack.MCP.PageSize <= 0 {
		c.Slack.MCP.PageSize = defaults.PageSize
	}
	if c.Slack.MCP.SearchLimit <= 0 {
		c.Slack.MCP.SearchLimit = defaults.SearchLimit
	}
	if c.Slack.MCP.Transport == "http" && c.Slack.MCP.SearchLimit > 20 {
		return errors.New("slack.mcp.search_limit cannot exceed 20")
	}
	if c.Slack.MCP.MaxPages < 0 {
		return errors.New("slack.mcp.max_pages cannot be negative")
	}
	if c.Slack.MCP.ProtocolVersion == "" {
		c.Slack.MCP.ProtocolVersion = defaults.ProtocolVersion
	}
	if c.Slack.Desktop.Enabled && strings.TrimSpace(c.Slack.Desktop.Path) == "" {
		detected, err := DetectDesktopPath()
		if err != nil {
			return err
		}
		c.Slack.Desktop.Path = detected
	}

	paths := []*string{&c.DBPath, &c.CacheDir, &c.LogDir, &c.Slack.Desktop.Path, &c.Slack.MCP.AuthPath, &c.Share.RepoPath}
	for _, candidate := range paths {
		expanded, err := ExpandPath(*candidate)
		if err != nil {
			return err
		}
		*candidate = expanded
	}
	for i := range c.Workspaces {
		if strings.TrimSpace(c.Workspaces[i].ID) == "" {
			return fmt.Errorf("workspaces[%d].id is required", i)
		}
		c.Workspaces[i].ID = strings.TrimSpace(c.Workspaces[i].ID)
	}
	c.WorkspaceID = strings.TrimSpace(c.WorkspaceID)
	if c.WorkspaceID == "" {
		c.WorkspaceID = c.DefaultWorkspaceID()
	}
	return nil
}

func ExpandPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	return filepath.Clean(crawlconfig.ExpandHome(path)), nil
}

func (c Config) ResolveTokens() Tokens {
	return resolveTokens(c.Slack)
}

func (c Config) ResolveTokensForWorkspace(workspaceID string) Tokens {
	if workspaceID == "" {
		return c.ResolveTokens()
	}
	if workspace, ok := c.Workspace(workspaceID); ok {
		return Tokens{
			Bot:  c.resolveWorkspaceToken(c.Slack.Bot, workspace, "bot"),
			App:  c.resolveWorkspaceToken(c.Slack.App, workspace, "app"),
			User: c.resolveWorkspaceToken(c.Slack.User, workspace, "user"),
		}
	}
	return c.ResolveTokens()
}

func (c Config) Workspace(workspaceID string) (Workspace, bool) {
	for _, workspace := range c.Workspaces {
		if workspace.ID == workspaceID {
			return workspace, true
		}
	}
	return Workspace{}, false
}

func (c Config) DefaultWorkspaceID() string {
	if strings.TrimSpace(c.WorkspaceID) != "" {
		return strings.TrimSpace(c.WorkspaceID)
	}
	for _, workspace := range c.Workspaces {
		if workspace.Default {
			return workspace.ID
		}
	}
	if len(c.Workspaces) == 1 {
		return c.Workspaces[0].ID
	}
	return ""
}

func (c Config) WorkspaceIDs() []string {
	ids := make([]string, 0, len(c.Workspaces))
	for _, workspace := range c.Workspaces {
		ids = append(ids, workspace.ID)
	}
	return ids
}

func (c Config) ShareEnabled() bool {
	return strings.TrimSpace(c.Share.Remote) != ""
}

func (c Config) FileMediaEnabled() bool {
	if c.Sync.FileMedia == nil {
		return false
	}
	return *c.Sync.FileMedia
}

func (c Config) ShareMediaEnabled() bool {
	if c.Share.Media == nil {
		return true
	}
	return *c.Share.Media
}

func (c Config) IncludeDMsResolved(hasUserToken bool) bool {
	if c.Sync.IncludeDMs != nil {
		return *c.Sync.IncludeDMs
	}
	return hasUserToken
}

func EnsureRuntimeDirs(c Config) error {
	paths := []string{
		filepath.Dir(c.DBPath),
		c.CacheDir,
		c.LogDir,
		filepath.Dir(c.Share.RepoPath),
	}
	for _, raw := range paths {
		path, err := ExpandPath(raw)
		if err != nil {
			return err
		}
		if path == "" {
			continue
		}
		if err := os.MkdirAll(path, 0o750); err != nil {
			return err
		}
	}
	return nil
}

func resolveTokens(cfg SlackConfig) Tokens {
	tokens := Tokens{}
	if cfg.Bot.Enabled {
		tokens.Bot = os.Getenv(cfg.Bot.TokenEnv)
	}
	if cfg.App.Enabled {
		tokens.App = os.Getenv(cfg.App.TokenEnv)
	}
	if cfg.User.Enabled {
		tokens.User = os.Getenv(cfg.User.TokenEnv)
	}
	return tokens
}

func (c Config) resolveWorkspaceToken(global TokenConfig, workspace Workspace, kind string) string {
	if !global.Enabled {
		return ""
	}
	for _, envName := range []string{
		workspaceTokenEnvOverride(workspace, kind),
		workspace.SlackTokenEnv(kind),
		workspaceTokenEnvName(workspace.ID, kind),
		global.TokenEnv,
	} {
		if envName == "" {
			continue
		}
		if value := os.Getenv(envName); value != "" {
			return value
		}
	}
	return ""
}

func (w Workspace) SlackTokenEnv(kind string) string {
	switch kind {
	case "bot":
		return w.Slack.Bot.TokenEnv
	case "app":
		return w.Slack.App.TokenEnv
	case "user":
		return w.Slack.User.TokenEnv
	default:
		return ""
	}
}

func workspaceTokenEnvOverride(workspace Workspace, kind string) string {
	switch kind {
	case "bot":
		return workspace.BotTokenEnv
	case "app":
		return workspace.AppTokenEnv
	case "user":
		return workspace.UserTokenEnv
	default:
		return ""
	}
}

func workspaceTokenEnvName(workspaceID string, kind string) string {
	if workspaceID == "" {
		return ""
	}
	return "SLACK_" + sanitizeEnvSegment(workspaceID) + "_" + strings.ToUpper(kind) + "_TOKEN"
}

func sanitizeEnvSegment(value string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(value)) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return b.String()
}

func DetectDesktopPath() (string, error) {
	candidates := []string{defaultDesktopPath, directDesktopPath}
	for _, candidate := range candidates {
		expanded, err := ExpandPath(candidate)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(expanded); err == nil {
			return expanded, nil
		}
	}
	return "", nil
}

func ValidateTokenShape(value string, prefix string) error {
	if value == "" {
		return errors.New("token missing")
	}
	if !strings.HasPrefix(value, prefix) {
		return fmt.Errorf("token must start with %s", prefix)
	}
	return nil
}

func Redact(value string) string {
	if value == "" {
		return ""
	}
	if len(value) <= 8 {
		return "****"
	}
	return value[:4] + "…" + value[len(value)-4:]
}
