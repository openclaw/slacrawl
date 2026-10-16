package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
)

func (a *App) runSearch(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	if hasHelpArg(args) {
		printSearchUsage(a.Stdout)
		return nil
	}
	var parsed slacrawlSearchArgs
	if err := parseKongArgs(&parsed, normalizeSingleDashLongFlags(args, "workspace", "limit", "mode", "raw-fts"), "slacrawl search", a.Stdout, a.Stderr); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(parsed.Query, " "))
	if query == "" {
		return errors.New("search query required")
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	mode, err := resolveSearchMode(parsed.Mode, cfg.Search.DefaultMode, parsed.RawFTS)
	if err != nil {
		return err
	}
	st, err := a.openReadableStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	results, err := st.SearchMessages(ctx, store.SearchOptions{
		WorkspaceID: coalesce(parsed.Workspace, cfg.WorkspaceID),
		Query:       query,
		Limit:       store.RequireLimit(parsed.Limit),
		Mode:        mode,
	})
	if err != nil {
		return err
	}
	if err := a.writeOutput("Search", results, format, false); err != nil {
		return err
	}
	if len(results) == 0 && format == FormatText {
		a.writeSearchNoRowsHint(ctx, st)
	}
	return nil
}

func resolveSearchMode(flagMode string, configMode string, rawFTS bool) (store.SearchMode, error) {
	if rawFTS {
		return store.SearchModeRawFTS, nil
	}
	mode := strings.ToLower(strings.TrimSpace(coalesce(flagMode, configMode)))
	switch mode {
	case "", "auto", "fts":
		return store.SearchModeAuto, nil
	case "phrase":
		return store.SearchModePhrase, nil
	case "terms", "literal":
		return store.SearchModeTerms, nil
	case "raw-fts", "raw":
		return store.SearchModeRawFTS, nil
	default:
		return "", fmt.Errorf("invalid search mode %q: use auto, phrase, terms, or raw-fts", mode)
	}
}

func (a *App) writeSearchNoRowsHint(ctx context.Context, st *store.Store) {
	status, err := st.Status(ctx)
	if err != nil {
		return
	}
	lastSync := "-"
	if !status.LastSyncAt.IsZero() {
		lastSync = status.LastSyncAt.Format(time.RFC3339)
	}
	_, _ = fmt.Fprintf(a.Stdout, "\nhint: no matches in %d local messages; last sync %s; for recent Slack Desktop data run `slacrawl sync --source wiretap`\n", status.Messages, lastSync)
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

func (a *App) runMentions(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	cfg, configErr := loadConfig(configPath)
	if configErr != nil {
		cfg = config.Default()
	}
	fs := flag.NewFlagSet("mentions", flag.ContinueOnError)
	workspaceID := fs.String("workspace", "", "workspace id")
	target := fs.String("target", "", "target id or label")
	limit := fs.Int("limit", 50, "row limit")
	if err := a.parseCommandFlags(fs, args); err != nil {
		return err
	}
	if configErr != nil {
		return configErr
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
		data, err := readAllContext(ctx, os.Stdin)
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
	Mode      string   `help:"Search mode: auto, phrase, terms, or raw-fts."`
	RawFTS    bool     `name:"raw-fts" help:"Treat query as raw SQLite FTS5 MATCH syntax."`
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
	cfg, configErr := loadConfig(configPath)
	if configErr != nil {
		cfg = config.Default()
	}
	fs := flag.NewFlagSet("users", flag.ContinueOnError)
	workspaceID := fs.String("workspace", "", "workspace id")
	limit := fs.Int("limit", 100, "row limit")
	ordered, err := reorderFlagsBeforePositionals(args, map[string]bool{
		"workspace": true,
		"limit":     true,
	})
	if err != nil {
		return err
	}
	if err := a.parseCommandFlags(fs, ordered); err != nil {
		return err
	}
	if configErr != nil {
		return configErr
	}
	if *limit <= 0 {
		return errors.New("users --limit must be positive")
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
	results, err := st.Users(ctx, coalesce(*workspaceID, cfg.WorkspaceID), query, *limit)
	if err != nil {
		return err
	}
	return a.writeOutput("Users", results, format, false)
}

func (a *App) runChannels(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	cfg, configErr := loadConfig(configPath)
	if configErr != nil {
		cfg = config.Default()
	}
	fs := flag.NewFlagSet("channels", flag.ContinueOnError)
	workspaceID := fs.String("workspace", "", "workspace id")
	kind := fs.String("kind", "", "channel kind")
	limit := fs.Int("limit", 100, "row limit")
	ordered, err := reorderFlagsBeforePositionals(args, map[string]bool{
		"workspace": true,
		"kind":      true,
		"limit":     true,
	})
	if err != nil {
		return err
	}
	if err := a.parseCommandFlags(fs, ordered); err != nil {
		return err
	}
	if configErr != nil {
		return configErr
	}
	if *limit <= 0 {
		return errors.New("channels --limit must be positive")
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
	results, err := st.ChannelsByKind(ctx, coalesce(*workspaceID, cfg.WorkspaceID), query, resolvedKind, *limit)
	if err != nil {
		return err
	}
	return a.writeOutput("Channels", results, format, false)
}

func readAllContext(ctx context.Context, r io.Reader) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(r)
		done <- result{data: data, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-done:
		if res.err != nil {
			return nil, res.err
		}
		return res.data, nil
	}
}

func printSearchUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage:
  slacrawl search [flags] <query>

Flags:
  -workspace string  workspace id
  -limit int         row limit (default 50)
  -mode string       search mode: auto, phrase, terms, or raw-fts
  -raw-fts           treat query as raw SQLite FTS5 MATCH syntax
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

func printSQLUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage:
  slacrawl sql <select query>

Runs a read-only SELECT query against the local archive.
`)
}
