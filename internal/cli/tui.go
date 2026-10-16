package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/openclaw/crawlkit/tui"
	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
)

func (a *App) runTUI(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), "Usage of tui:")
		fs.PrintDefaults()
		_, _ = fmt.Fprintln(fs.Output())
		_, _ = fmt.Fprintln(fs.Output(), tui.ControlsHelp())
	}
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			fs.SetOutput(a.Stdout)
			break
		}
	}
	workspaceID := fs.String("workspace", "", "workspace id")
	channelID := fs.String("channel", "", "channel id")
	userID := fs.String("author", "", "user id")
	limit := fs.Int("limit", 200, "row limit")
	includeDrafts := fs.Bool("include-drafts", false, "include local desktop draft messages")
	includeSystem := fs.Bool("include-system", false, "include Slack join/leave/topic system messages")
	jsonOut := fs.Bool("json", false, "write browser rows as JSON")
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
		return errors.New("tui takes flags only")
	}
	if *limit <= 0 {
		return errors.New("tui --limit must be positive")
	}
	cfg, err := loadConfigOrDefault(configPath)
	if err != nil {
		return err
	}
	loadRows := func(ctx context.Context) ([]tui.Row, error) {
		st, err := store.OpenReadOnly(cfg.DBPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
		defer st.Close()
		queryLimit := store.RequireLimit(*limit)
		if !*includeDrafts || !*includeSystem {
			queryLimit = store.RequireLimit(*limit * 10)
		}
		rows, err := st.MessagesWithThreadContext(ctx, coalesce(*workspaceID, cfg.WorkspaceID), *channelID, *userID, queryLimit)
		if err != nil {
			return nil, err
		}
		return slackTUIRows(rows, *includeDrafts, *includeSystem, *limit), nil
	}
	archiveRows, err := loadRows(ctx)
	if err != nil {
		return err
	}
	return tui.Browse(ctx, tui.BrowseOptions{
		AppName:        "slacrawl",
		Title:          "slacrawl archive",
		EmptyMessage:   "slacrawl has no local messages yet",
		Rows:           archiveRows,
		Refresh:        loadRows,
		JSON:           format == FormatJSON,
		Layout:         tui.LayoutChat,
		SourceKind:     archiveSourceKind(cfg.Share.Remote),
		SourceLocation: archiveSourceLocation(cfg),
		Stdout:         a.Stdout,
	})
}

func archiveSourceKind(remote string) string {
	if strings.TrimSpace(remote) != "" {
		return tui.SourceRemote
	}
	return tui.SourceLocal
}

func archiveSourceLocation(cfg config.Config) string {
	if strings.TrimSpace(cfg.Share.Remote) != "" {
		return cfg.Share.Remote
	}
	return cfg.DBPath
}

func slackTUIRows(rows []store.MessageRow, includeDrafts bool, includeSystem bool, limit int) []tui.Row {
	type visibleRow struct {
		row  store.MessageRow
		item tui.Row
	}
	visible := make([]visibleRow, 0, len(rows))
	for _, row := range rows {
		if !includeDrafts && slackIsDraft(row) {
			continue
		}
		if !includeSystem && slackIsNoisySystem(row) {
			continue
		}
		visible = append(visible, visibleRow{row: row, item: slackMessageTUIRow(row)})
	}
	if limit <= 0 || limit > len(visible) {
		limit = len(visible)
	}
	byKey := make(map[string]visibleRow, len(visible))
	for _, row := range visible {
		byKey[slackRowKey(row.row)] = row
	}
	emitted := make(map[string]struct{}, limit)
	items := make([]tui.Row, 0, limit)
	emit := func(row visibleRow) {
		key := slackRowKey(row.row)
		if _, ok := emitted[key]; ok {
			return
		}
		emitted[key] = struct{}{}
		items = append(items, row.item)
	}
	for _, row := range visible[:limit] {
		if parentTS := slackParentTS(row.row); parentTS != "" {
			parentKey := row.row.WorkspaceID + "\x00" + row.row.ChannelID + "\x00" + parentTS
			if parent, ok := byKey[parentKey]; ok {
				emit(parent)
			}
		}
		emit(row)
	}
	return items
}

func slackRowKey(row store.MessageRow) string {
	return row.WorkspaceID + "\x00" + row.ChannelID + "\x00" + strings.TrimSpace(row.TS)
}

func slackMessageTUIRow(row store.MessageRow) tui.Row {
	title := strings.TrimSpace(row.NormalizedText)
	if title == "" {
		title = strings.TrimSpace(row.Text)
	}
	if title == "" {
		title = row.ChannelID + " " + row.TS
	}
	detail := strings.TrimSpace(row.Text)
	if detail == "" {
		detail = row.NormalizedText
	}
	readableDetail := strings.TrimSpace(row.NormalizedText)
	if readableDetail == "" {
		readableDetail = detail
	}
	return tui.Row{
		Source:    "slack",
		Kind:      "message",
		ID:        strings.TrimSpace(row.ChannelID + "/" + row.TS),
		ParentID:  slackParentTS(row),
		Scope:     slackWorkspaceScope(row),
		Container: coalesce(row.ChannelName, row.ChannelID),
		Author:    slackAuthorName(row),
		Title:     title,
		Text:      detail,
		Detail:    readableDetail,
		URL:       slackMessageURL(row),
		CreatedAt: formatSlackTimestamp(row.TS),
		Tags:      []string{row.WorkspaceID, row.ChannelID, row.UserID},
		Fields: map[string]string{
			"channel_id":   row.ChannelID,
			"latest_reply": row.LatestReply,
			"reply_count":  strconv.Itoa(row.ReplyCount),
			"subtype":      row.Subtype,
			"thread":       row.ThreadTS,
			"ts":           row.TS,
			"user_id":      row.UserID,
		},
	}
}

func slackIsDraft(row store.MessageRow) bool {
	return strings.EqualFold(strings.TrimSpace(row.Subtype), "desktop_draft") || strings.HasPrefix(strings.TrimSpace(row.TS), "draft:")
}

func slackIsNoisySystem(row store.MessageRow) bool {
	switch strings.ToLower(strings.TrimSpace(row.Subtype)) {
	case "channel_archive", "channel_join", "channel_leave", "channel_name", "channel_purpose", "channel_topic", "channel_unarchive", "group_join", "group_leave":
		return true
	default:
		return false
	}
}

func slackWorkspaceScope(row store.MessageRow) string {
	name := strings.TrimSpace(row.WorkspaceName)
	id := strings.TrimSpace(row.WorkspaceID)
	if name == "" || name == id {
		return ""
	}
	return name
}

func slackAuthorName(row store.MessageRow) string {
	if name := strings.TrimSpace(row.UserName); name != "" {
		return name
	}
	if label := slackSourceLabel(row); label != "" {
		return label
	}
	if strings.TrimSpace(row.UserID) != "" {
		return "Slack user"
	}
	return ""
}

func slackSourceLabel(row store.MessageRow) string {
	subtype := strings.ToLower(strings.TrimSpace(row.Subtype))
	if subtype != "" {
		return "Slack"
	}
	text := strings.ToLower(strings.TrimSpace(coalesce(row.NormalizedText, row.Text)))
	switch {
	case strings.Contains(text, "new course started"),
		strings.Contains(text, "course completed"),
		strings.Contains(text, "new project created"),
		strings.Contains(text, "new build update"):
		return "Build Club"
	}
	switch strings.ToLower(strings.TrimSpace(row.SourceName)) {
	case "desktop-indexeddb":
		return "Slack desktop"
	case "slack-export":
		return "Slack export"
	case "api-bot", "slack-api":
		return "Slack API"
	default:
		return ""
	}
}

func slackMessageURL(row store.MessageRow) string {
	workspaceID := strings.TrimSpace(row.WorkspaceID)
	channelID := strings.TrimSpace(row.ChannelID)
	ts := strings.TrimSpace(row.TS)
	if workspaceID == "" || channelID == "" || ts == "" {
		return ""
	}
	values := url.Values{}
	values.Set("team", workspaceID)
	values.Set("id", channelID)
	values.Set("message", ts)
	return "slack://channel?" + values.Encode()
}

func slackParentTS(row store.MessageRow) string {
	threadTS := strings.TrimSpace(row.ThreadTS)
	if threadTS == "" || threadTS == strings.TrimSpace(row.TS) {
		return ""
	}
	return threadTS
}

func formatSlackTimestamp(ts string) string {
	seconds, fraction, ok := strings.Cut(slackTimestampValue(ts), ".")
	if !ok || seconds == "" {
		return ts
	}
	sec, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil {
		return ts
	}
	fraction = (fraction + "000000000")[:9]
	nsec, err := strconv.ParseInt(fraction, 10, 64)
	if err != nil {
		nsec = 0
	}
	return time.Unix(sec, nsec).UTC().Format(time.RFC3339Nano)
}

func slackTimestampValue(ts string) string {
	value := strings.TrimSpace(ts)
	if strings.HasPrefix(value, "draft:") {
		value = strings.TrimPrefix(value, "draft:")
		if before, _, ok := strings.Cut(value, ":"); ok {
			return before
		}
	}
	return value
}
