package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/openclaw/crawlkit/control"
	"github.com/openclaw/slacrawl/internal/admission"
	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/share"
	"github.com/openclaw/slacrawl/internal/slackapi"
	"github.com/openclaw/slacrawl/internal/slackdesktop"
	"github.com/openclaw/slacrawl/internal/store"
)

func (a *App) runInit(configPath string, args []string, format OutputFormat) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	workspaceID := fs.String("workspace", "", "workspace id")
	dbPath := fs.String("db", "", "database path")
	if err := a.parseCommandFlags(fs, args); err != nil {
		return err
	}

	cfg := config.Default()
	if *workspaceID != "" {
		cfg.WorkspaceID = *workspaceID
	}
	if *dbPath != "" {
		cfg.DBPath = *dbPath
	}
	if err := cfg.Normalize(); err != nil {
		return err
	}
	if err := cfg.Save(configPath); err != nil {
		return err
	}
	result := map[string]any{
		"config_path": configPath,
		"db_path":     cfg.DBPath,
	}
	return a.writeOutput("Init", result, format, true)
}

func (a *App) runDoctor(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "write doctor JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *jsonOut {
		format = FormatJSON
	}
	if fs.NArg() != 0 {
		return errors.New("doctor takes flags only")
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	tokens := cfg.ResolveTokens()
	diag, err := slackapi.NewWithOptions(tokens, a.apiURL, a.httpClient).WithDMPolicy(admission.FromConfig(cfg.Sync.IncludeDMs)).Doctor(ctx)
	if err != nil {
		return err
	}
	workspaceAPI, err := a.workspaceDoctorReports(ctx, cfg)
	if err != nil {
		return err
	}
	desktop := slackdesktop.Source{Path: cfg.Slack.Desktop.Path, Available: false}
	if cfg.Slack.Desktop.Enabled {
		desktop, err = slackdesktop.Inspect(ctx, cfg.Slack.Desktop.Path)
		if err != nil {
			return err
		}
	}
	threadCoverage := diag.ThreadCoverage
	if len(workspaceAPI) > 0 {
		threadCoverage = aggregateThreadCoverage(workspaceAPI)
	}
	if threadCoverage == "" {
		threadCoverage = "partial"
	}

	var status store.Status
	var channelSkips []map[string]any
	var tailState []store.SyncStateRow
	archiveProfile := archiveProfileFromConfig(cfg)
	shareState := shareStateFromConfig(cfg)
	st, err := store.OpenReadOnly(cfg.DBPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		defer st.Close()
		if threadCoverage == "full" || diag.ThreadCoverage == "full" {
			var facts store.APIThreadCoverageFacts
			status, facts, err = st.StatusWithAPIThreadCoverage(ctx)
			if err != nil {
				return err
			}
			if facts.ThreadWork || facts.IncompleteHistory {
				threadCoverage = "partial"
				// Keep the global auth diagnosis when only named coverage was full.
				if diag.ThreadCoverage == "full" {
					diag.ThreadCoverageReason = "retained_api_history_work"
					if facts.ThreadWork {
						diag.ThreadCoverageReason = "retained_api_thread_work"
					}
				}
				diag.ThreadCoverage = threadCoverage
			}
		} else {
			status, err = st.Status(ctx)
			if err != nil {
				return err
			}
		}
		archiveProfile, err = a.buildArchiveProfile(ctx, cfg, st)
		if err != nil {
			return err
		}
		shareState, err = a.buildShareState(ctx, cfg, st)
		if err != nil {
			return err
		}
		channelSkips, err = st.QueryReadOnly(ctx, `SELECT source_name, entity_type, entity_id, value
			FROM sync_state WHERE source_name IN ('api-bot', 'api-user') AND entity_type = 'channel_skip'
			ORDER BY updated_at DESC, entity_id ASC LIMIT 20`)
		if err != nil {
			return err
		}
		if channelSkips == nil {
			channelSkips = []map[string]any{}
		}
		tailState, err = st.ListSyncState(ctx, "tail", "", 20)
		if err != nil {
			return err
		}
	}

	report := map[string]any{
		"config_path":   configPath,
		"database_path": cfg.DBPath,
		"tokens": map[string]any{
			"bot_env":      cfg.Slack.Bot.TokenEnv,
			"app_env":      cfg.Slack.App.TokenEnv,
			"user_env":     cfg.Slack.User.TokenEnv,
			"bot_enabled":  cfg.Slack.Bot.Enabled,
			"app_enabled":  cfg.Slack.App.Enabled,
			"user_enabled": cfg.Slack.User.Enabled,
			"bot_set":      tokens.Bot != "",
			"app_set":      tokens.App != "",
			"user_set":     tokens.User != "",
		},
		"slack_api": diag,
		"mcp_source": map[string]any{
			"enabled":          cfg.Slack.MCP.Enabled,
			"transport":        cfg.Slack.MCP.Transport,
			"base_url":         cfg.Slack.MCP.BaseURL,
			"command":          cfg.Slack.MCP.Command,
			"connector_id":     cfg.Slack.MCP.ConnectorID,
			"token_env":        cfg.Slack.MCP.TokenEnv,
			"token_set":        strings.TrimSpace(os.Getenv(cfg.Slack.MCP.TokenEnv)) != "",
			"auth_path":        cfg.Slack.MCP.AuthPath,
			"auth_path_exists": pathExists(cfg.Slack.MCP.AuthPath),
		},
		"workspace_api":     workspaceAPI,
		"thread_coverage":   threadCoverage,
		"desktop_source":    desktop,
		"archive_profile":   archiveProfile,
		"share":             shareState,
		"api_channel_skips": channelSkips,
		"tail_state":        tailState,
		"status":            status,
		"fts_available":     true,
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return a.writeOutput("Doctor", report, format, true)
}

func (a *App) runStatus(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "write crawlkit status JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *jsonOut {
		format = FormatJSON
	}
	if fs.NArg() != 0 {
		return errors.New("status takes flags only")
	}
	cfg, err := loadConfigOrDefault(configPath)
	if err != nil {
		return err
	}
	var status store.Status
	archiveProfile := archiveProfileFromConfig(cfg)
	shareState := shareStateFromConfig(cfg)
	st, err := store.OpenReadOnly(cfg.DBPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		defer st.Close()
		status, err = st.Status(ctx)
		if err != nil {
			return err
		}
		archiveProfile, err = a.buildArchiveProfile(ctx, cfg, st)
		if err != nil {
			return err
		}
		shareState, err = a.buildShareState(ctx, cfg, st)
		if err != nil {
			return err
		}
	}
	if *jsonOut {
		return a.writeJSON(controlStatus("slacrawl", configPath, cfg, status, shareState))
	}
	return a.writeOutput("Status", statusResponse{Status: status, ArchiveProfile: archiveProfile, Share: shareState}, format, true)
}

func (a *App) runMetadata(configPath string, args []string, format OutputFormat) error {
	fs := flag.NewFlagSet("metadata", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "write crawlkit metadata JSON")
	formatFlag := fs.String("format", "", "output format: text, json, or log")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("metadata takes flags only")
	}
	if *jsonOut || flagWasSet(fs, "format") {
		parsed, err := resolveOutputFormat(*formatFlag, *jsonOut)
		if err != nil {
			return err
		}
		format = parsed
	}
	return a.writeOutput("Metadata", controlManifest(configPath), format, false)
}

func (a *App) workspaceDoctorReports(ctx context.Context, cfg config.Config) ([]map[string]any, error) {
	workspaceIDs := cfg.WorkspaceIDs()
	if len(workspaceIDs) == 0 {
		return nil, nil
	}
	reports := make([]map[string]any, 0, len(workspaceIDs))
	for _, workspaceID := range workspaceIDs {
		tokens := cfg.ResolveTokensForWorkspace(workspaceID)
		diag, err := slackapi.NewWithOptions(tokens, a.apiURL, a.httpClient).WithDMPolicy(admission.FromConfig(cfg.Sync.IncludeDMs)).Doctor(ctx)
		if err != nil {
			return nil, fmt.Errorf("doctor %s: %w", workspaceID, err)
		}
		reports = append(reports, map[string]any{
			"workspace_id": workspaceID,
			"tokens": map[string]any{
				"bot_set":  tokens.Bot != "",
				"app_set":  tokens.App != "",
				"user_set": tokens.User != "",
			},
			"slack_api": diag,
		})
	}
	return reports, nil
}

func aggregateThreadCoverage(reports []map[string]any) string {
	if len(reports) == 0 {
		return "partial"
	}
	for _, report := range reports {
		slackAPI, ok := report["slack_api"].(slackapi.Diagnostics)
		if !ok || slackAPI.ThreadCoverage != "full" {
			return "partial"
		}
	}
	return "full"
}

type statusResponse struct {
	store.Status
	ArchiveProfile archiveProfileResponse `json:"archive_profile"`
	Share          shareResponse          `json:"share"`
}

type archiveProfileResponse struct {
	Mode    string           `json:"mode"`
	Sources []sourceResponse `json:"sources"`
}

type sourceResponse struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Enabled     bool   `json:"enabled"`
	Configured  bool   `json:"configured"`
	LastSeenAt  string `json:"last_seen_at,omitempty"`
	Messages    int64  `json:"messages"`
	SyncEntries int64  `json:"sync_entries"`
}

type shareResponse struct {
	Enabled                 bool       `json:"enabled"`
	AutoUpdate              bool       `json:"auto_update"`
	Remote                  string     `json:"remote,omitempty"`
	RepoPath                string     `json:"repo_path,omitempty"`
	Branch                  string     `json:"branch,omitempty"`
	StaleAfter              string     `json:"stale_after,omitempty"`
	LastImportAt            *time.Time `json:"last_import_at,omitempty"`
	LastManifestGeneratedAt *time.Time `json:"last_manifest_generated_at,omitempty"`
	NeedsImport             bool       `json:"needs_import"`
}

func archiveProfileFromConfig(cfg config.Config) archiveProfileResponse {
	sources := []sourceResponse{
		{
			Name:       "bot",
			Label:      "Slack API bot/user visibility",
			Enabled:    cfg.Slack.Bot.Enabled || cfg.Slack.User.Enabled,
			Configured: hasAPITokens(cfg),
		},
		{
			Name:       "wiretap",
			Label:      "Slack Desktop local cache visibility",
			Enabled:    cfg.Slack.Desktop.Enabled,
			Configured: strings.TrimSpace(cfg.Slack.Desktop.Path) != "",
		},
		{
			Name:       "mcp",
			Label:      "Slack MCP connector visibility",
			Enabled:    cfg.Slack.MCP.Enabled,
			Configured: mcpConfigured(cfg.Slack.MCP),
		},
		{
			Name:       "backup",
			Label:      "Git archive backup/restore",
			Enabled:    cfg.ShareEnabled(),
			Configured: strings.TrimSpace(cfg.Share.Remote) != "",
		},
	}
	return archiveProfileResponse{
		Mode:    archiveMode(sources),
		Sources: sources,
	}
}

func mcpConfigured(cfg config.MCPConfig) bool {
	if strings.EqualFold(strings.TrimSpace(cfg.Transport), "stdio") {
		return strings.TrimSpace(cfg.Command) != ""
	}
	return strings.TrimSpace(cfg.BaseURL) != ""
}

func (a *App) buildArchiveProfile(ctx context.Context, cfg config.Config, st *store.Store) (archiveProfileResponse, error) {
	sources := archiveProfileFromConfig(cfg).Sources
	index := map[string]int{}
	for i := range sources {
		index[sources[i].Name] = i
	}

	syncRows, err := st.QueryReadOnly(ctx, `
select
  case
    when source_name in ('api-bot', 'api-user', 'tail') then 'bot'
    when source_name = 'desktop' or source_name like 'desktop-%' then 'wiretap'
    when source_name = 'share' then 'backup'
    else source_name
  end as source,
  coalesce(max(case
    when entity_type = 'thread_pending_v1' and source_name in ('api-user', 'mcp') then null
    when source_name = 'mcp' and entity_type = 'history_work_v1' then null
    else updated_at
  end), '') as last_seen_at,
  count(*) as sync_entries
from sync_state
where source_name != 'doctor'
group by source
`)
	if err != nil {
		return archiveProfileResponse{}, err
	}
	for _, row := range syncRows {
		source := fmt.Sprint(row["source"])
		i, ok := index[source]
		if !ok {
			continue
		}
		sources[i].LastSeenAt = fmt.Sprint(row["last_seen_at"])
		sources[i].SyncEntries = int64Value(row["sync_entries"])
	}

	messageRows, err := st.QueryReadOnly(ctx, `
select
  case
    when source_name in ('api-bot', 'api-user') then 'bot'
    when source_name = 'desktop' or source_name like 'desktop-%' then 'wiretap'
    when source_name = 'slack-export' then 'import'
    else source_name
  end as source,
  count(*) as messages
from messages
group by source
`)
	if err != nil {
		return archiveProfileResponse{}, err
	}
	importMessages := int64(0)
	for _, row := range messageRows {
		source := fmt.Sprint(row["source"])
		if source == "import" {
			importMessages += int64Value(row["messages"])
			continue
		}
		i, ok := index[source]
		if !ok {
			continue
		}
		sources[i].Messages = int64Value(row["messages"])
	}
	if importMessages > 0 {
		sources = append(sources, sourceResponse{
			Name:       "import",
			Label:      "Slack export import",
			Enabled:    true,
			Configured: true,
			Messages:   importMessages,
		})
	}

	return archiveProfileResponse{
		Mode:    archiveMode(sources),
		Sources: sources,
	}, nil
}

func hasAPITokens(cfg config.Config) bool {
	tokens := cfg.ResolveTokens()
	if tokens.Bot != "" || tokens.User != "" {
		return true
	}
	for _, workspaceID := range cfg.WorkspaceIDs() {
		tokens := cfg.ResolveTokensForWorkspace(workspaceID)
		if tokens.Bot != "" || tokens.User != "" {
			return true
		}
	}
	return false
}

func archiveMode(sources []sourceResponse) string {
	var bot, mcp, wiretap, backup, imported bool
	for _, source := range sources {
		hasData := source.Messages > 0 || source.LastSeenAt != "" || source.SyncEntries > 0
		switch source.Name {
		case "bot":
			bot = hasData
		case "wiretap":
			wiretap = hasData
		case "mcp":
			mcp = hasData
		case "backup":
			backup = hasData
		case "import":
			imported = hasData
		}
	}
	switch {
	case boolCount(bot, mcp, wiretap, backup, imported) > 1:
		return "hybrid"
	case bot:
		return "bot"
	case mcp:
		return "mcp"
	case wiretap:
		return "wiretap"
	case backup:
		return "backup"
	case imported:
		return "import"
	default:
		return "empty"
	}
}

func boolCount(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}

func pathExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func shareStateFromConfig(cfg config.Config) shareResponse {
	return shareResponse{
		Enabled:    cfg.ShareEnabled(),
		AutoUpdate: cfg.Share.AutoUpdate,
		Remote:     cfg.Share.Remote,
		RepoPath:   cfg.Share.RepoPath,
		Branch:     cfg.Share.Branch,
		StaleAfter: cfg.Share.StaleAfter,
	}
}

func (a *App) buildShareState(ctx context.Context, cfg config.Config, st *store.Store) (shareResponse, error) {
	state := shareStateFromConfig(cfg)
	syncState, err := share.ReadSyncState(ctx, st)
	if err != nil {
		return shareResponse{}, err
	}
	if !syncState.LastImportAt.IsZero() {
		lastImport := syncState.LastImportAt
		state.LastImportAt = &lastImport
	}
	if !syncState.LastManifestGeneratedAt.IsZero() {
		lastManifest := syncState.LastManifestGeneratedAt
		state.LastManifestGeneratedAt = &lastManifest
	}
	if !cfg.ShareEnabled() {
		return state, nil
	}
	staleAfter, err := time.ParseDuration(cfg.Share.StaleAfter)
	if err != nil {
		return shareResponse{}, fmt.Errorf("invalid share.stale_after: %w", err)
	}
	state.NeedsImport = share.NeedsImport(ctx, st, staleAfter)
	return state, nil
}

func int64Value(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case float64:
		return int64(typed)
	case []byte:
		parsed, _ := strconv.ParseInt(string(typed), 10, 64)
		return parsed
	case string:
		parsed, _ := strconv.ParseInt(typed, 10, 64)
		return parsed
	default:
		return 0
	}
}

func controlStatus(appID, configPath string, cfg config.Config, status store.Status, shareState shareResponse) control.Status {
	counts := []control.Count{
		control.NewCount("workspaces", "Workspaces", int64(status.Workspaces)),
		control.NewCount("channels", "Channels", int64(status.Channels)),
		control.NewCount("users", "Users", int64(status.Users)),
		control.NewCount("messages", "Messages", int64(status.Messages)),
	}
	summary := fmt.Sprintf("%d messages across %d channels", status.Messages, status.Channels)
	state := control.NewStatus(appID, summary)
	state.State = "current"
	state.ConfigPath = configPath
	state.DatabasePath = cfg.DBPath
	state.Counts = counts
	if !status.LastSyncAt.IsZero() {
		state.LastSyncAt = status.LastSyncAt.UTC().Format(time.RFC3339)
	}
	db := control.SQLiteDatabase("primary", "Slack archive", "archive", cfg.DBPath, true, counts)
	state.DatabaseBytes = db.Bytes
	state.WALBytes = fileSize(cfg.DBPath + "-wal")
	state.Databases = []control.Database{db}
	state.Share = &control.Share{
		Enabled:     shareState.Enabled,
		RepoPath:    shareState.RepoPath,
		Remote:      shareState.Remote,
		Branch:      shareState.Branch,
		NeedsUpdate: shareState.NeedsImport,
	}
	return state
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
