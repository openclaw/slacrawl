package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kong"
	"github.com/vincentkoc/slacrawl/internal/config"
	"github.com/vincentkoc/slacrawl/internal/media"
	"github.com/vincentkoc/slacrawl/internal/report"
	"github.com/vincentkoc/slacrawl/internal/share"
	"github.com/vincentkoc/slacrawl/internal/slackapi"
	"github.com/vincentkoc/slacrawl/internal/slackdesktop"
	"github.com/vincentkoc/slacrawl/internal/store"
	"github.com/vincentkoc/slacrawl/internal/syncer"
)

type App struct {
	Stdout io.Writer
	Stderr io.Writer

	configPath   string
	outputFormat OutputFormat
	now          func() time.Time
	httpClient   *http.Client
}

type OutputFormat string

const (
	FormatText OutputFormat = "text"
	FormatJSON OutputFormat = "json"
	FormatLog  OutputFormat = "log"
)

func New() *App {
	return &App{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}

func (a *App) nowUTC() time.Time {
	if a.now != nil {
		return a.now().UTC()
	}
	return time.Now().UTC()
}

func (a *App) Run(ctx context.Context, args []string) error {
	if len(args) == 0 || rootHelpRequested(args, "config", "format") {
		a.setColorEnabled(FormatText, false)
		a.printHelp()
		return nil
	}
	var global slacrawlRootArgs
	if err := parseKongArgs(&global, args, "slacrawl", a.Stdout, a.Stderr); err != nil {
		return err
	}
	rest := global.Args
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "--help" || rest[0] == "-h" {
		a.setColorEnabled(FormatText, global.NoColor)
		a.printHelp()
		return nil
	}

	configPath := global.Config
	if configPath == "" {
		path, err := config.DefaultConfigPath()
		if err != nil {
			return err
		}
		configPath = path
	}

	outputFormat, err := resolveOutputFormat(global.Format, global.JSON)
	if err != nil {
		return err
	}
	a.configPath = configPath
	a.outputFormat = outputFormat
	a.setColorEnabled(outputFormat, global.NoColor)

	switch rest[0] {
	case "init":
		return a.runInit(configPath, rest[1:], outputFormat)
	case "doctor":
		return a.runDoctor(ctx, configPath, outputFormat)
	case "report":
		return a.runReport(ctx, configPath, outputFormat)
	case "digest":
		return a.runDigest(ctx, configPath, rest[1:], outputFormat)
	case "analytics":
		return a.runAnalytics(ctx, configPath, rest[1:], outputFormat)
	case "publish":
		return a.runPublish(ctx, configPath, rest[1:], outputFormat)
	case "subscribe":
		return a.runSubscribe(ctx, configPath, rest[1:], outputFormat)
	case "update":
		return a.runUpdate(ctx, configPath, rest[1:], outputFormat)
	case "status":
		return a.runStatus(ctx, configPath, outputFormat)
	case "sync":
		return a.runSync(ctx, configPath, rest[1:], outputFormat)
	case "import":
		return a.runImport(ctx, rest[1:])
	case "search":
		return a.runSearch(ctx, configPath, rest[1:], outputFormat)
	case "messages":
		return a.runMessages(ctx, configPath, rest[1:], outputFormat)
	case "files":
		return a.runFiles(ctx, configPath, rest[1:], outputFormat)
	case "mentions":
		return a.runMentions(ctx, configPath, rest[1:], outputFormat)
	case "sql":
		return a.runSQL(ctx, configPath, rest[1:], outputFormat)
	case "users":
		return a.runUsers(ctx, configPath, rest[1:], outputFormat)
	case "channels":
		return a.runChannels(ctx, configPath, rest[1:], outputFormat)
	case "completion":
		return a.runCompletion(rest[1:])
	case "tail":
		return a.runTail(ctx, configPath, rest[1:])
	case "watch":
		return a.runWatch(ctx, configPath, rest[1:], outputFormat)
	default:
		return fmt.Errorf("unknown command: %s", rest[0])
	}
}

type slacrawlRootArgs struct {
	Config  string   `help:"Config path."`
	Format  string   `default:"text" help:"Output format: text, json, or log."`
	JSON    bool     `name:"json" help:"Compatibility alias for --format json."`
	NoColor bool     `name:"no-color" help:"Disable ANSI color in text output."`
	Args    []string `arg:"" optional:"" passthrough:"partial" name:"command" help:"Command and arguments."`
}

func rootHelpRequested(args []string, valueFlags ...string) bool {
	valueFlagSet := make(map[string]struct{}, len(valueFlags))
	for _, flag := range valueFlags {
		valueFlagSet[flag] = struct{}{}
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--help" || arg == "-h" || (arg == "help" && i == len(args)-1) {
			return true
		}
		if !strings.HasPrefix(arg, "-") {
			return false
		}
		if name, ok := strings.CutPrefix(arg, "--"); ok {
			if strings.Contains(name, "=") {
				continue
			}
			if _, ok := valueFlagSet[name]; ok {
				i++
			}
		}
	}
	return false
}

func parseKongArgs(target any, args []string, name string, stdout, stderr io.Writer, options ...kong.Option) error {
	opts := []kong.Option{
		kong.Name(name),
		kong.NoDefaultHelp(),
		kong.Writers(stdout, stderr),
		kong.Exit(func(int) {}),
	}
	opts = append(opts, options...)
	parser, err := kong.New(target, opts...)
	if err != nil {
		return err
	}
	_, err = parser.Parse(args)
	return err
}

func (a *App) setColorEnabled(format OutputFormat, noColor bool) {
	ansiEnabled = format == FormatText && !noColor && colorAllowedByEnv() && writerIsTTY(a.Stdout)
}

func colorAllowedByEnv() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	return true
}

func writerIsTTY(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

func resolveOutputFormat(value string, jsonOut bool) (OutputFormat, error) {
	if jsonOut {
		return FormatJSON, nil
	}
	switch OutputFormat(strings.ToLower(strings.TrimSpace(value))) {
	case "", FormatText:
		return FormatText, nil
	case FormatJSON:
		return FormatJSON, nil
	case FormatLog:
		return FormatLog, nil
	default:
		return "", fmt.Errorf("unsupported format %q: use text, json, or log", value)
	}
}

func (a *App) runInit(configPath string, args []string, format OutputFormat) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	workspaceID := fs.String("workspace", "", "workspace id")
	dbPath := fs.String("db", "", "database path")
	if err := fs.Parse(args); err != nil {
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

func (a *App) runDoctor(ctx context.Context, configPath string, format OutputFormat) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	tokens := cfg.ResolveTokens()
	diag, err := slackapi.New(tokens).WithIncludeDMs(cfg.IncludeDMsResolved(tokens.User != "")).Doctor(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	workspaceAPI, err := a.workspaceDoctorReports(ctx, cfg)
	if err != nil {
		return err
	}
	desktop := slackdesktop.Source{Path: cfg.Slack.Desktop.Path, Available: false}
	if cfg.Slack.Desktop.Enabled {
		desktop, err = slackdesktop.Inspect(cfg.Slack.Desktop.Path)
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
	if err := st.SetSyncState(ctx, "doctor", "threads", "coverage", threadCoverage); err != nil {
		return err
	}
	status, err := st.Status(ctx)
	if err != nil {
		return err
	}
	archiveProfile, err := a.buildArchiveProfile(ctx, cfg, st)
	if err != nil {
		return err
	}
	shareState, err := a.buildShareState(ctx, cfg, st)
	if err != nil {
		return err
	}
	channelSkips, err := st.ListSyncState(ctx, "api-bot", "channel_skip", 20)
	if err != nil {
		return err
	}
	tailState, err := st.ListSyncState(ctx, "tail", "", 20)
	if err != nil {
		return err
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
		"slack_api":         diag,
		"workspace_api":     workspaceAPI,
		"desktop_source":    desktop,
		"archive_profile":   archiveProfile,
		"share":             shareState,
		"api_channel_skips": channelSkips,
		"tail_state":        tailState,
		"status":            status,
		"fts_available":     true,
	}
	return a.writeOutput("Doctor", report, format, true)
}

func (a *App) runStatus(ctx context.Context, configPath string, format OutputFormat) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	st, err := a.openReadableStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	status, err := st.Status(ctx)
	if err != nil {
		return err
	}
	archiveProfile, err := a.buildArchiveProfile(ctx, cfg, st)
	if err != nil {
		return err
	}
	shareState, err := a.buildShareState(ctx, cfg, st)
	if err != nil {
		return err
	}
	return a.writeOutput("Status", statusResponse{Status: status, ArchiveProfile: archiveProfile, Share: shareState}, format, true)
}

func (a *App) runSync(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	source := fs.String("source", "api", "api|bot|desktop|wiretap|all")
	workspaceID := fs.String("workspace", "", "workspace id")
	channels := fs.String("channels", "", "comma separated channel ids")
	since := fs.String("since", "", "oldest slack ts or RFC3339 timestamp")
	full := fs.Bool("full", false, "full sync")
	latestOnly := fs.Bool("latest-only", false, "skip first-time historical backfills")
	concurrency := fs.Int("concurrency", cfg.Sync.Concurrency, "worker count")
	withMedia := fs.Bool("with-media", cfg.FileMediaEnabled(), "fetch file media after sync")
	if err := fs.Parse(args); err != nil {
		return err
	}

	resolvedSource, err := syncer.ParseSource(*source)
	if err != nil {
		return err
	}
	runOptions := syncer.Options{
		Source:      resolvedSource,
		WorkspaceID: coalesce(*workspaceID, cfg.WorkspaceID),
		Channels:    csv(*channels),
		Since:       *since,
		Full:        *full,
		LatestOnly:  *latestOnly,
		Concurrency: *concurrency,
	}
	st, err := a.openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	if runOptions.Source != syncer.SourceDesktop {
		if err := a.autoUpdateShare(ctx, cfg, st); err != nil {
			return err
		}
	}
	summary, err := a.runSyncTargets(ctx, cfg, st, runOptions)
	if err != nil {
		return err
	}
	var mediaStats *media.FetchStats
	if *withMedia {
		stats, err := a.fetchMediaForSync(ctx, cfg, st, runOptions.WorkspaceID, runOptions.Channels)
		if err != nil {
			return err
		}
		mediaStats = &stats
	}
	status, err := st.Status(ctx)
	if err != nil {
		return err
	}
	archiveProfile, err := a.buildArchiveProfile(ctx, cfg, st)
	if err != nil {
		return err
	}
	result := map[string]any{
		"status":          status,
		"archive_profile": archiveProfile,
		"summary":         summary,
	}
	if mediaStats != nil {
		result["media"] = mediaStats
	}
	return a.writeOutput("Sync", result, format, true)
}

func (a *App) runSearch(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	if hasHelpArg(args) {
		printSearchUsage(a.Stdout)
		return nil
	}
	var parsed slacrawlSearchArgs
	if err := parseKongArgs(&parsed, normalizeSingleDashLongFlags(args, "workspace", "limit"), "slacrawl search", a.Stdout, a.Stderr); err != nil {
		return err
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	st, err := a.openReadableStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	results, err := st.Search(ctx, coalesce(parsed.Workspace, cfg.WorkspaceID), strings.Join(parsed.Query, " "), store.RequireLimit(parsed.Limit))
	if err != nil {
		return err
	}
	return a.writeOutput("Search", results, format, false)
}

func (a *App) runMessages(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	if hasHelpArg(args) {
		printMessagesUsage(a.Stdout)
		return nil
	}
	var parsed slacrawlMessagesArgs
	if err := parseKongArgs(&parsed, normalizeSingleDashLongFlags(args, "workspace", "channel", "author", "limit"), "slacrawl messages", a.Stdout, a.Stderr); err != nil {
		return err
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	st, err := a.openReadableStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	results, err := st.Messages(ctx, coalesce(parsed.Workspace, cfg.WorkspaceID), parsed.Channel, parsed.Author, store.RequireLimit(parsed.Limit))
	if err != nil {
		return err
	}
	return a.writeOutput("Messages", results, format, false)
}

func (a *App) runFiles(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	if len(args) > 0 && args[0] == "fetch" {
		return a.runFilesFetch(ctx, configPath, args[1:], format)
	}
	if hasHelpArg(args) {
		printFilesUsage(a.Stdout)
		return nil
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	opts, err := a.parseFileListOptions(args, cfg, 50)
	if err != nil {
		return err
	}
	st, err := a.openReadableStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	queryOpts := opts
	if opts.MissingOnly {
		queryOpts.MissingOnly = false
		queryOpts.Limit = 0
	}
	rows, err := st.Files(ctx, queryOpts)
	if err != nil {
		return err
	}
	if opts.MissingOnly {
		limit := opts.Limit
		rows = filterMissingFileMedia(cfg.CacheDir, rows)
		if limit > 0 && len(rows) > limit {
			rows = rows[:limit]
		}
	}
	return a.writeOutput("Files", rows, format, false)
}

func (a *App) runFilesFetch(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	if hasHelpArg(args) {
		printFilesUsage(a.Stdout)
		return nil
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	listArgs := stripKnownFlags(args, map[string]bool{"force": false, "max-bytes": true})
	opts, err := a.parseFileListOptions(listArgs, cfg, 50)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("files fetch", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	force := fs.Bool("force", false, "redownload cached files")
	maxBytes := fs.Int64("max-bytes", cfg.Sync.MaxFileBytes, "per-file download cap")
	if err := parseOnlyKnown(fs, args, fileListFlagNames()); err != nil {
		return err
	}
	if *maxBytes <= 0 {
		return errors.New("--max-bytes must be positive")
	}
	st, err := a.openReadableStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	stats, err := a.fetchFiles(ctx, cfg, st, opts, *maxBytes, *force)
	if err != nil {
		return err
	}
	return a.writeOutput("Files", stats, format, false)
}

func (a *App) parseFileListOptions(args []string, cfg config.Config, defaultLimit int) (store.FileListOptions, error) {
	fs := flag.NewFlagSet("files", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	workspaceID := fs.String("workspace", "", "workspace id")
	channelID := fs.String("channel", "", "channel id")
	userID := fs.String("user", "", "user id")
	fileID := fs.String("file", "", "file id")
	filename := fs.String("filename", "", "filename filter")
	contentType := fs.String("type", "", "content type or filetype")
	since := fs.String("since", "", "RFC3339 lower bound")
	before := fs.String("before", "", "RFC3339 upper bound")
	limit := fs.Int("limit", defaultLimit, "row limit")
	all := fs.Bool("all", false, "return all rows")
	missing := fs.Bool("missing", false, "only files without cached media")
	if err := fs.Parse(args); err != nil {
		return store.FileListOptions{}, err
	}
	if fs.NArg() != 0 {
		return store.FileListOptions{}, errors.New("files takes flags only")
	}
	if *limit < 0 {
		return store.FileListOptions{}, errors.New("--limit must be >= 0")
	}
	var sinceTime, beforeTime time.Time
	var err error
	if strings.TrimSpace(*since) != "" {
		sinceTime, err = time.Parse(time.RFC3339, *since)
		if err != nil {
			return store.FileListOptions{}, fmt.Errorf("invalid --since: %w", err)
		}
	}
	if strings.TrimSpace(*before) != "" {
		beforeTime, err = time.Parse(time.RFC3339, *before)
		if err != nil {
			return store.FileListOptions{}, fmt.Errorf("invalid --before: %w", err)
		}
	}
	if *all {
		*limit = 0
	}
	return store.FileListOptions{
		WorkspaceID: coalesce(*workspaceID, cfg.WorkspaceID),
		ChannelID:   strings.TrimSpace(*channelID),
		UserID:      strings.TrimSpace(*userID),
		FileID:      strings.TrimSpace(*fileID),
		Filename:    strings.TrimSpace(*filename),
		ContentType: strings.TrimSpace(*contentType),
		Since:       sinceTime,
		Before:      beforeTime,
		Limit:       *limit,
		MissingOnly: *missing,
	}, nil
}

func filterMissingFileMedia(cacheDir string, rows []store.FileRow) []store.FileRow {
	out := rows[:0]
	for _, row := range rows {
		if row.MediaPath == "" {
			out = append(out, row)
			continue
		}
		path, err := media.LocalPath(cacheDir, row.MediaPath)
		if err != nil {
			out = append(out, row)
			continue
		}
		if _, err := os.Stat(path); err != nil {
			out = append(out, row)
		}
	}
	return out
}

func (a *App) fetchFiles(ctx context.Context, cfg config.Config, st *store.Store, opts store.FileListOptions, maxBytes int64, force bool) (media.FetchStats, error) {
	workspaceID := strings.TrimSpace(opts.WorkspaceID)
	if workspaceID != "" {
		return media.Fetch(ctx, st, media.FetchOptions{
			CacheDir:     cfg.CacheDir,
			List:         opts,
			MaxBytes:     maxBytes,
			Force:        force,
			Token:        mediaToken(cfg, workspaceID),
			HTTPClient:   a.httpClient,
			StatusUpdate: true,
			Now:          a.nowUTC,
		})
	}
	targets := resolveWorkspaceTargets(cfg, "")
	if len(targets) == 0 {
		return media.Fetch(ctx, st, media.FetchOptions{
			CacheDir:     cfg.CacheDir,
			List:         opts,
			MaxBytes:     maxBytes,
			Force:        force,
			Token:        mediaToken(cfg, ""),
			HTTPClient:   a.httpClient,
			StatusUpdate: true,
			Now:          a.nowUTC,
		})
	}
	remaining := opts.Limit
	var total media.FetchStats
	for _, target := range targets {
		batch := opts
		batch.WorkspaceID = target
		if remaining > 0 {
			batch.Limit = remaining
		}
		stats, err := media.Fetch(ctx, st, media.FetchOptions{
			CacheDir:     cfg.CacheDir,
			List:         batch,
			MaxBytes:     maxBytes,
			Force:        force,
			Token:        mediaToken(cfg, target),
			HTTPClient:   a.httpClient,
			StatusUpdate: true,
			Now:          a.nowUTC,
		})
		if err != nil {
			return media.FetchStats{}, err
		}
		addMediaStats(&total, stats)
		if remaining > 0 {
			remaining -= stats.Files
			if remaining <= 0 {
				break
			}
		}
	}
	return total, nil
}

func (a *App) runMentions(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("mentions", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	workspaceID := fs.String("workspace", "", "workspace id")
	target := fs.String("target", "", "target id or label")
	limit := fs.Int("limit", 50, "row limit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := a.openReadableStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	results, err := st.Mentions(ctx, coalesce(*workspaceID, cfg.WorkspaceID), *target, store.RequireLimit(*limit))
	if err != nil {
		return err
	}
	return a.writeOutput("Mentions", results, format, false)
}

func (a *App) runSQL(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	if hasHelpArg(args) {
		printSQLUsage(a.Stdout)
		return nil
	}
	var parsed slacrawlSQLArgs
	if err := parseKongArgs(&parsed, args, "slacrawl sql", a.Stdout, a.Stderr); err != nil {
		return err
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(parsed.Query, " "))
	if query == "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		query = strings.TrimSpace(string(data))
	}
	if query == "" {
		return errors.New("sql query required")
	}
	st, err := a.openReadableStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	results, err := st.QueryReadOnly(ctx, query)
	if err != nil {
		return err
	}
	return a.writeOutput("SQL", results, format, false)
}

type slacrawlSearchArgs struct {
	Workspace string   `help:"Workspace id."`
	Limit     int      `default:"50" help:"Row limit."`
	Query     []string `arg:"" name:"query" help:"Search query."`
}

type slacrawlMessagesArgs struct {
	Workspace string `help:"Workspace id."`
	Channel   string `help:"Channel id."`
	Author    string `help:"User id."`
	Limit     int    `default:"50" help:"Row limit."`
}

type slacrawlSQLArgs struct {
	Query []string `arg:"" optional:"" passthrough:"all" name:"query" help:"Read-only SQL query."`
}

func (a *App) runUsers(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("users", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	workspaceID := fs.String("workspace", "", "workspace id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := ""
	if fs.NArg() > 0 {
		query = fs.Arg(0)
	}
	st, err := a.openReadableStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	results, err := st.Users(ctx, coalesce(*workspaceID, cfg.WorkspaceID), query, 100)
	if err != nil {
		return err
	}
	return a.writeOutput("Users", results, format, false)
}

func (a *App) runChannels(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("channels", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	workspaceID := fs.String("workspace", "", "workspace id")
	kind := fs.String("kind", "", "channel kind")
	if err := fs.Parse(args); err != nil {
		return err
	}
	resolvedKind := normalizeChannelKind(*kind)
	if resolvedKind != "" && !isValidChannelKind(resolvedKind) {
		return fmt.Errorf("invalid channel kind %q: use im, mpim, public, private, public_channel, or private_channel", *kind)
	}
	query := ""
	if fs.NArg() > 0 {
		query = fs.Arg(0)
	}
	st, err := a.openReadableStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	results, err := st.ChannelsByKind(ctx, coalesce(*workspaceID, cfg.WorkspaceID), query, resolvedKind, 100)
	if err != nil {
		return err
	}
	return a.writeOutput("Channels", results, format, false)
}

func (a *App) runTail(ctx context.Context, configPath string, args []string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("tail", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	workspaceID := fs.String("workspace", "", "workspace id")
	repairEvery := fs.String("repair-every", cfg.Sync.RepairEvery, "repair interval")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := a.openReadableStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	repairDuration, err := time.ParseDuration(*repairEvery)
	if err != nil {
		return err
	}
	targets := resolveWorkspaceTargets(cfg, *workspaceID)
	if len(targets) == 0 {
		targets = []string{coalesce(*workspaceID, cfg.WorkspaceID)}
	}
	if len(targets) == 1 {
		return slackapi.New(cfg.ResolveTokensForWorkspace(targets[0])).Tail(ctx, st, targets[0], repairDuration)
	}
	return a.runTailTargets(ctx, st, cfg, targets, repairDuration)
}

func (a *App) runWatch(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	desktopEvery := fs.String("desktop-every", cfg.Sync.DesktopRefreshEvery, "desktop refresh interval")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !cfg.Slack.Desktop.Enabled {
		return errors.New("desktop sync is disabled in config")
	}
	interval, err := time.ParseDuration(*desktopEvery)
	if err != nil {
		return err
	}
	if interval <= 0 {
		return errors.New("desktop refresh interval must be greater than zero")
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	syncOnce := func() error {
		summary, err := syncer.Run(ctx, cfg, st, syncer.Options{Source: syncer.SourceDesktop})
		if err != nil {
			return err
		}
		status, err := st.Status(ctx)
		if err != nil {
			return err
		}
		return a.writeOutput("Watch", map[string]any{
			"status":  status,
			"summary": summary,
		}, format, true)
	}
	if err := syncOnce(); err != nil {
		return err
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := syncOnce(); err != nil {
				return err
			}
		}
	}
}

func (a *App) runDigest(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("digest", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	since := fs.String("since", "7d", "lookback window, e.g. 24h, 7d, 30d")
	workspaceID := fs.String("workspace", "", "workspace id")
	channel := fs.String("channel", "", "channel id or name")
	topN := fs.Int("top-n", 1, "number of top posters and mentions per channel")
	if err := fs.Parse(args); err != nil {
		return err
	}
	lookback, err := parseLookback(*since)
	if err != nil {
		return fmt.Errorf("parse --since: %w", err)
	}
	st, err := a.openReadableStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	digest, err := report.BuildDigest(ctx, st, report.DigestOptions{
		Now:         a.nowUTC(),
		Since:       lookback,
		WorkspaceID: coalesce(*workspaceID, cfg.WorkspaceID),
		Channel:     *channel,
		TopN:        *topN,
	})
	if err != nil {
		return err
	}
	return a.writeOutput("Digest", digest, format, true)
}

// parseLookback accepts Go durations (72h) plus the shorthand Nd for N days.
func parseLookback(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("empty duration")
	}
	if strings.HasSuffix(value, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(value, "d"))
		if err != nil {
			return 0, fmt.Errorf("invalid day count: %w", err)
		}
		if days < 0 {
			return 0, errors.New("negative duration")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, errors.New("negative duration")
	}
	return d, nil
}

func (a *App) runReport(ctx context.Context, configPath string, format OutputFormat) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	st, err := a.openReadableStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	activity, err := report.Build(ctx, st, report.Options{Now: a.nowUTC()})
	if err != nil {
		return err
	}
	shareState, err := a.buildShareState(ctx, cfg, st)
	if err != nil {
		return err
	}
	return a.writeOutput("Report", map[string]any{
		"activity": activity,
		"share":    shareState,
	}, format, true)
}

func (a *App) writeJSON(value any) error {
	enc := json.NewEncoder(a.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func loadConfig(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func loadConfigOrDefault(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, nil
	}
	if !os.IsNotExist(err) {
		return config.Config{}, err
	}
	cfg = config.Default()
	if err := cfg.Normalize(); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func csv(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func isValidChannelKind(kind string) bool {
	switch kind {
	case "im", "mpim", "public_channel", "private_channel":
		return true
	default:
		return false
	}
}

func normalizeChannelKind(kind string) string {
	switch strings.TrimSpace(kind) {
	case "public":
		return "public_channel"
	case "private":
		return "private_channel"
	default:
		return strings.TrimSpace(kind)
	}
}

func resolveWorkspaceTargets(cfg config.Config, requested string) []string {
	if strings.TrimSpace(requested) != "" {
		return []string{strings.TrimSpace(requested)}
	}
	if ids := cfg.WorkspaceIDs(); len(ids) > 0 {
		return ids
	}
	if cfg.WorkspaceID != "" {
		return []string{cfg.WorkspaceID}
	}
	return nil
}

func (a *App) runSyncTargets(ctx context.Context, cfg config.Config, st *store.Store, opts syncer.Options) (syncer.Summary, error) {
	targets := resolveWorkspaceTargets(cfg, opts.WorkspaceID)
	if opts.Source == syncer.SourceDesktop {
		return syncer.Run(ctx, cfg, st, opts)
	}
	if len(targets) == 0 {
		return syncer.Run(ctx, cfg, st, opts)
	}

	var last syncer.Summary
	for _, workspaceID := range targets {
		runOpts := opts
		runOpts.WorkspaceID = workspaceID
		summary, err := syncer.RunWithTokens(ctx, cfg, st, runOpts, cfg.ResolveTokensForWorkspace(workspaceID))
		if err != nil {
			return syncer.Summary{}, err
		}
		last = summary
	}
	return last, nil
}

func (a *App) fetchMediaForSync(ctx context.Context, cfg config.Config, st *store.Store, workspaceID string, channels []string) (media.FetchStats, error) {
	targets := resolveWorkspaceTargets(cfg, workspaceID)
	if len(targets) == 0 {
		targets = []string{workspaceID}
	}
	var total media.FetchStats
	for _, target := range targets {
		channelTargets := channels
		if len(channelTargets) == 0 {
			channelTargets = []string{""}
		}
		for _, channelID := range channelTargets {
			stats, err := media.Fetch(ctx, st, media.FetchOptions{
				CacheDir:     cfg.CacheDir,
				List:         store.FileListOptions{WorkspaceID: target, ChannelID: channelID},
				MaxBytes:     cfg.Sync.MaxFileBytes,
				Token:        mediaToken(cfg, target),
				StatusUpdate: true,
				Now:          a.nowUTC,
			})
			if err != nil {
				return media.FetchStats{}, err
			}
			addMediaStats(&total, stats)
		}
	}
	return total, nil
}

func addMediaStats(total *media.FetchStats, stats media.FetchStats) {
	total.Files += stats.Files
	total.Fetched += stats.Fetched
	total.Reused += stats.Reused
	total.Skipped += stats.Skipped
	total.Failed += stats.Failed
	total.Bytes += stats.Bytes
}

func (a *App) runTailTargets(ctx context.Context, st *store.Store, cfg config.Config, workspaceIDs []string, repairEvery time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, len(workspaceIDs))
	var wg sync.WaitGroup
	for _, workspaceID := range workspaceIDs {
		workspaceID := workspaceID
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := slackapi.New(cfg.ResolveTokensForWorkspace(workspaceID)).Tail(ctx, st, workspaceID, repairEvery)
			if err != nil && !errors.Is(err, context.Canceled) {
				errCh <- fmt.Errorf("tail %s: %w", workspaceID, err)
				cancel()
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case err := <-errCh:
		return err
	case <-done:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *App) workspaceDoctorReports(ctx context.Context, cfg config.Config) ([]map[string]any, error) {
	workspaceIDs := cfg.WorkspaceIDs()
	if len(workspaceIDs) == 0 {
		return nil, nil
	}
	reports := make([]map[string]any, 0, len(workspaceIDs))
	for _, workspaceID := range workspaceIDs {
		tokens := cfg.ResolveTokensForWorkspace(workspaceID)
		diag, err := slackapi.New(tokens).WithIncludeDMs(cfg.IncludeDMsResolved(tokens.User != "")).Doctor(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
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

func coalesce(primary string, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}

func mediaToken(cfg config.Config, workspaceID string) string {
	tokens := cfg.ResolveTokensForWorkspace(workspaceID)
	if tokens.User != "" {
		return tokens.User
	}
	return tokens.Bot
}

func fileListFlagNames() map[string]bool {
	return map[string]bool{
		"workspace": true, "channel": true, "user": true, "file": true,
		"filename": true, "type": true, "since": true, "before": true,
		"limit": true, "all": false, "missing": false,
	}
}

func parseOnlyKnown(fs *flag.FlagSet, args []string, listFlags map[string]bool) error {
	filtered := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			filtered = append(filtered, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if key, _, ok := strings.Cut(name, "="); ok {
			name = key
		}
		if hasValue, ok := listFlags[name]; ok {
			if hasValue && !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
			}
			continue
		}
		filtered = append(filtered, arg)
	}
	return fs.Parse(filtered)
}

func stripKnownFlags(args []string, strip map[string]bool) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			out = append(out, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if key, _, ok := strings.Cut(name, "="); ok {
			name = key
		}
		if hasValue, ok := strip[name]; ok {
			if hasValue && !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
			}
			continue
		}
		out = append(out, arg)
	}
	return out
}

func WithTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 30*time.Second)
}

func (a *App) runPublish(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	repoPath := fs.String("repo", cfg.Share.RepoPath, "git repo path")
	remote := fs.String("remote", cfg.Share.Remote, "git remote")
	branch := fs.String("branch", cfg.Share.Branch, "git branch")
	message := fs.String("message", "", "commit message")
	noCommit := fs.Bool("no-commit", false, "skip git commit")
	push := fs.Bool("push", false, "push to origin")
	noMedia := fs.Bool("no-media", !cfg.ShareMediaEnabled(), "omit cached media files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("publish takes no positional arguments")
	}
	st, err := a.openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	opts, err := shareOptions(*repoPath, *remote, *branch, cfg.CacheDir, !*noMedia)
	if err != nil {
		return err
	}
	manifest, err := share.Export(ctx, st, opts)
	if err != nil {
		return err
	}
	committed := false
	if !*noCommit {
		committed, err = share.Commit(ctx, opts, *message)
		if err != nil {
			return err
		}
	}
	if *push {
		if err := share.Push(ctx, opts); err != nil {
			return err
		}
		if err := share.MarkImported(ctx, st, manifest); err != nil {
			return err
		}
	}
	return a.writeOutput("Publish", map[string]any{
		"repo_path":    opts.RepoPath,
		"remote":       opts.Remote,
		"generated_at": manifest.GeneratedAt,
		"tables":       manifest.Tables,
		"media":        manifest.Media,
		"committed":    committed,
		"pushed":       *push,
	}, format, true)
}

func (a *App) runSubscribe(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	cfg, err := loadConfigOrDefault(configPath)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("subscribe", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	repoPath := fs.String("repo", cfg.Share.RepoPath, "local clone path")
	dbPath := fs.String("db", cfg.DBPath, "database path")
	remote := fs.String("remote", cfg.Share.Remote, "git remote")
	branch := fs.String("branch", cfg.Share.Branch, "git branch")
	staleAfter := fs.String("stale-after", cfg.Share.StaleAfter, "auto-refresh age threshold")
	noAutoUpdate := fs.Bool("no-auto-update", false, "disable read-time auto refresh")
	noImport := fs.Bool("no-import", false, "skip initial import")
	noMedia := fs.Bool("no-media", !cfg.ShareMediaEnabled(), "skip restoring cached media")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errors.New("subscribe takes at most one remote")
	}
	if fs.NArg() == 1 {
		*remote = fs.Arg(0)
	}
	if strings.TrimSpace(*remote) == "" {
		return errors.New("subscribe requires a remote")
	}

	cfg.Share.Remote = strings.TrimSpace(*remote)
	cfg.Share.RepoPath = *repoPath
	cfg.DBPath = *dbPath
	cfg.Share.Branch = *branch
	cfg.Share.AutoUpdate = !*noAutoUpdate
	cfg.Share.StaleAfter = *staleAfter
	shareMedia := !*noMedia
	cfg.Share.Media = &shareMedia
	cfg.Slack.Bot.Enabled = false
	cfg.Slack.App.Enabled = false
	cfg.Slack.User.Enabled = false
	cfg.Slack.Desktop.Enabled = false
	cfg.Slack.Desktop.Path = ""
	if err := cfg.Save(configPath); err != nil {
		return err
	}
	if *noImport {
		return a.writeOutput("Subscribe", map[string]any{
			"config_path": configPath,
			"repo_path":   cfg.Share.RepoPath,
			"remote":      cfg.Share.Remote,
		}, format, true)
	}

	st, err := a.openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	opts, err := shareOptions(cfg.Share.RepoPath, cfg.Share.Remote, cfg.Share.Branch, cfg.CacheDir, !*noMedia)
	if err != nil {
		return err
	}
	if err := share.Pull(ctx, opts); err != nil {
		return err
	}
	manifest, imported, err := share.ImportIfChanged(ctx, st, opts)
	if err != nil {
		return err
	}
	return a.writeOutput("Subscribe", map[string]any{
		"config_path":  configPath,
		"repo_path":    opts.RepoPath,
		"remote":       opts.Remote,
		"generated_at": manifest.GeneratedAt,
		"tables":       manifest.Tables,
		"media":        manifest.Media,
		"imported":     imported,
	}, format, true)
}

func (a *App) runUpdate(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	repoPath := fs.String("repo", cfg.Share.RepoPath, "local clone path")
	remote := fs.String("remote", cfg.Share.Remote, "git remote")
	branch := fs.String("branch", cfg.Share.Branch, "git branch")
	noMedia := fs.Bool("no-media", !cfg.ShareMediaEnabled(), "skip restoring cached media")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("update takes no positional arguments")
	}
	st, err := a.openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	opts, err := shareOptions(*repoPath, *remote, *branch, cfg.CacheDir, !*noMedia)
	if err != nil {
		return err
	}
	if err := share.Pull(ctx, opts); err != nil {
		return err
	}
	manifest, imported, err := share.ImportIfChanged(ctx, st, opts)
	if err != nil {
		return err
	}
	return a.writeOutput("Update", map[string]any{
		"repo_path":    opts.RepoPath,
		"remote":       opts.Remote,
		"generated_at": manifest.GeneratedAt,
		"tables":       manifest.Tables,
		"media":        manifest.Media,
		"imported":     imported,
	}, format, true)
}

func (a *App) openStore(cfg config.Config) (*store.Store, error) {
	if err := config.EnsureRuntimeDirs(cfg); err != nil {
		return nil, err
	}
	return store.Open(cfg.DBPath)
}

func (a *App) openReadableStore(ctx context.Context, cfg config.Config) (*store.Store, error) {
	st, err := a.openStore(cfg)
	if err != nil {
		return nil, err
	}
	if err := a.autoUpdateShare(ctx, cfg, st); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}

func (a *App) autoUpdateShare(ctx context.Context, cfg config.Config, st *store.Store) error {
	if !cfg.ShareEnabled() || !cfg.Share.AutoUpdate {
		return nil
	}
	staleAfter, err := time.ParseDuration(cfg.Share.StaleAfter)
	if err != nil {
		return fmt.Errorf("invalid share.stale_after: %w", err)
	}
	if !share.NeedsImport(ctx, st, staleAfter) {
		return nil
	}
	opts, err := shareOptions(cfg.Share.RepoPath, cfg.Share.Remote, cfg.Share.Branch, cfg.CacheDir, cfg.ShareMediaEnabled())
	if err != nil {
		return err
	}
	if err := share.Pull(ctx, opts); err != nil {
		return err
	}
	_, _, err = share.ImportIfChanged(ctx, st, opts)
	if errors.Is(err, share.ErrNoManifest) {
		return nil
	}
	return err
}

func shareOptions(repoPath, remote, branch, cacheDir string, includeMedia bool) (share.Options, error) {
	expandedRepo, err := config.ExpandPath(repoPath)
	if err != nil {
		return share.Options{}, err
	}
	expandedCache, err := config.ExpandPath(cacheDir)
	if err != nil {
		return share.Options{}, err
	}
	if strings.TrimSpace(branch) == "" {
		branch = "main"
	}
	return share.Options{
		RepoPath:     expandedRepo,
		Remote:       strings.TrimSpace(remote),
		Branch:       branch,
		CacheDir:     expandedCache,
		IncludeMedia: includeMedia,
	}, nil
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

func (a *App) buildArchiveProfile(ctx context.Context, cfg config.Config, st *store.Store) (archiveProfileResponse, error) {
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
			Name:       "backup",
			Label:      "Git archive backup/restore",
			Enabled:    cfg.ShareEnabled(),
			Configured: strings.TrimSpace(cfg.Share.Remote) != "",
		},
	}
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
  max(updated_at) as last_seen_at,
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
	var bot, wiretap, backup, imported bool
	for _, source := range sources {
		hasData := source.Messages > 0 || source.LastSeenAt != "" || source.SyncEntries > 0
		switch source.Name {
		case "bot":
			bot = hasData
		case "wiretap":
			wiretap = hasData
		case "backup":
			backup = hasData
		case "import":
			imported = hasData
		}
	}
	switch {
	case bot && wiretap:
		return "hybrid"
	case bot:
		return "bot"
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

func (a *App) buildShareState(ctx context.Context, cfg config.Config, st *store.Store) (shareResponse, error) {
	state := shareResponse{
		Enabled:    cfg.ShareEnabled(),
		AutoUpdate: cfg.Share.AutoUpdate,
		Remote:     cfg.Share.Remote,
		RepoPath:   cfg.Share.RepoPath,
		Branch:     cfg.Share.Branch,
		StaleAfter: cfg.Share.StaleAfter,
	}
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

func hasHelpArg(args []string) bool {
	for _, arg := range args {
		if arg == "help" || arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func normalizeSingleDashLongFlags(args []string, names ...string) []string {
	allowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		allowed[name] = struct{}{}
	}
	out := make([]string, len(args))
	for i, arg := range args {
		if strings.HasPrefix(arg, "--") || !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "-=") {
			out[i] = arg
			continue
		}
		name := strings.TrimPrefix(arg, "-")
		if before, _, ok := strings.Cut(name, "="); ok {
			name = before
		}
		if _, ok := allowed[name]; ok {
			out[i] = "-" + arg
			continue
		}
		out[i] = arg
	}
	return out
}

func printSearchUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage:
  slacrawl search [flags] <query>

Flags:
  -workspace string  workspace id
  -limit int         row limit (default 50)
`)
}

func printMessagesUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage:
  slacrawl messages [flags]

Flags:
  -workspace string  workspace id
  -channel string    channel id
  -author string     user id
  -limit int         row limit (default 50)
`)
}

func printFilesUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage:
  slacrawl files [flags]
  slacrawl files fetch [flags]

Flags:
  -workspace string  workspace id
  -channel string    channel id
  -user string       user id
  -file string       file id
  -filename string   filename/title filter
  -type string       mimetype or filetype filter
  -since string      RFC3339 lower bound
  -before string     RFC3339 upper bound
  -missing           only files without cached media
  -limit int         row limit (default 50)
  -all               return all rows

Fetch flags:
  -force             redownload cached files
  -max-bytes int     per-file download cap
`)
}

func printSQLUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage:
  slacrawl sql <select query>

Runs a read-only SELECT query against the local archive.
`)
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
