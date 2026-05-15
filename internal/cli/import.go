package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/vincentkoc/slacrawl/internal/config"
	"github.com/vincentkoc/slacrawl/internal/importer"
	"github.com/vincentkoc/slacrawl/internal/search"
	"github.com/vincentkoc/slacrawl/internal/store"
)

const (
	slackExportSourceName = "slack-export"
	slackExportSourceRank = 2
)

type ImportReport struct {
	Workspace string        `json:"workspace"`
	Users     int           `json:"users"`
	Channels  int           `json:"channels"`
	DMs       int           `json:"dms"`
	MPIMs     int           `json:"mpims"`
	Messages  int           `json:"messages"`
	Skipped   int           `json:"skipped"`
	DryRun    bool          `json:"dry_run"`
	Elapsed   time.Duration `json:"elapsed"`
}

type importChannelProgress struct {
	ID       string
	Name     string
	Kind     string
	Messages int
	Skipped  int
}

func (a *App) runImport(ctx context.Context, args []string) error {
	configPath := a.configPath
	if strings.TrimSpace(configPath) == "" {
		path, err := config.DefaultConfigPath()
		if err != nil {
			return err
		}
		configPath = path
	}

	defaultFormat := a.outputFormat
	if defaultFormat == "" {
		defaultFormat = FormatText
	}

	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	workspace := fs.String("workspace", "", "workspace id")
	dryRun := fs.Bool("dry-run", false, "walk and count without writing")
	force := fs.Bool("force", false, "overwrite existing slack-export rows at the same rank")
	formatValue := fs.String("format", string(defaultFormat), "output format: text|json|log")
	parseArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		parseArgs = append([]string{}, args[1:]...)
		parseArgs = append(parseArgs, args[0])
	}
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("import requires exactly one path argument")
	}
	if strings.TrimSpace(*workspace) == "" {
		return errors.New("import requires --workspace")
	}

	format, err := resolveOutputFormat(*formatValue, false)
	if err != nil {
		return err
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	st, err := a.openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	ex, err := importer.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer func() { _ = ex.Close() }()

	started := time.Now().UTC()
	report, progress, err := runImportExecution(ctx, st, ex, strings.TrimSpace(*workspace), *dryRun, *force)
	if err != nil {
		return err
	}
	report.Elapsed = time.Since(started)

	if format == FormatText {
		return a.writeImportText(report, progress)
	}
	return a.writeOutput("Import", report, format, true)
}

func runImportExecution(ctx context.Context, st *store.Store, ex *importer.Export, workspaceID string, dryRun, force bool) (ImportReport, []importChannelProgress, error) {
	now := time.Now().UTC()
	report := ImportReport{Workspace: workspaceID, DryRun: dryRun}

	if !dryRun {
		if err := st.UpsertWorkspace(ctx, store.Workspace{
			ID:        workspaceID,
			Name:      workspaceID,
			RawJSON:   store.MarshalRaw(map[string]any{"id": workspaceID, "source": slackExportSourceName}),
			UpdatedAt: now,
		}); err != nil {
			return ImportReport{}, nil, err
		}
	}

	users, err := ex.Users()
	if err != nil {
		return ImportReport{}, nil, err
	}
	report.Users = len(users)
	if !dryRun {
		for _, user := range users {
			if err := st.UpsertUser(ctx, toStoreUser(workspaceID, user, now)); err != nil {
				return ImportReport{}, nil, err
			}
		}
	}

	channels, err := ex.Channels()
	if err != nil {
		return ImportReport{}, nil, err
	}
	report.Channels = len(channels)

	dms, err := ex.DMs()
	if err != nil {
		return ImportReport{}, nil, err
	}
	report.DMs = len(dms)

	mpims, err := ex.MPIMs()
	if err != nil {
		return ImportReport{}, nil, err
	}
	report.MPIMs = len(mpims)

	allChannels := make([]importer.ChannelInfo, 0, len(channels)+len(dms)+len(mpims))
	allChannels = append(allChannels, channels...)
	allChannels = append(allChannels, dms...)
	allChannels = append(allChannels, mpims...)

	if !dryRun {
		for _, channel := range allChannels {
			if err := st.UpsertChannel(ctx, toStoreChannel(workspaceID, channel, now)); err != nil {
				return ImportReport{}, nil, err
			}
		}
	}

	progress := make([]importChannelProgress, 0, len(allChannels))
	for _, channel := range allChannels {
		row := importChannelProgress{ID: channel.ID, Name: channel.Name, Kind: channel.Kind}
		candidates := []string{channel.Name}
		if channel.ID != "" && channel.ID != channel.Name {
			candidates = append(candidates, channel.ID)
		}

		for _, candidate := range candidates {
			channelRows := 0
			for env, iterErr := range ex.Messages(candidate) {
				if iterErr != nil {
					return ImportReport{}, nil, iterErr
				}
				channelRows++
				message, mentions, ok := toStoreMessage(workspaceID, channel.ID, env.Raw, now)
				if !ok {
					row.Skipped++
					report.Skipped++
					continue
				}
				skip, err := shouldSkipMessage(ctx, st, message.ChannelID, message.TS, force)
				if err != nil {
					return ImportReport{}, nil, err
				}
				if skip {
					row.Skipped++
					report.Skipped++
					continue
				}
				row.Messages++
				report.Messages++
				if dryRun {
					continue
				}
				if err := st.UpsertMessage(ctx, message, mentions); err != nil {
					return ImportReport{}, nil, err
				}
			}
			if channelRows > 0 {
				break
			}
		}
		progress = append(progress, row)
	}

	return report, progress, nil
}

func shouldSkipMessage(ctx context.Context, st *store.Store, channelID, ts string, force bool) (bool, error) {
	rank, source, exists, err := existingMessageSource(ctx, st, channelID, ts)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	if rank < slackExportSourceRank {
		return true, nil
	}
	if rank > slackExportSourceRank {
		return false, nil
	}
	if source == slackExportSourceName && force {
		return false, nil
	}
	return true, nil
}

func existingMessageSource(ctx context.Context, st *store.Store, channelID, ts string) (int, string, bool, error) {
	var rank int
	var source string
	err := st.DB().QueryRowContext(ctx, `
select source_rank, source_name
from messages
where channel_id = ? and ts = ?
`, channelID, ts).Scan(&rank, &source)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	return rank, source, true, nil
}

func toStoreUser(workspaceID string, user slack.User, now time.Time) store.User {
	return store.User{
		ID:          user.ID,
		WorkspaceID: workspaceID,
		Name:        user.Name,
		RealName:    user.RealName,
		DisplayName: user.Profile.DisplayName,
		Title:       user.Profile.Title,
		IsBot:       user.IsBot,
		IsDeleted:   user.Deleted,
		RawJSON:     store.MarshalRaw(user),
		UpdatedAt:   now,
	}
}

func toStoreChannel(workspaceID string, channel importer.ChannelInfo, now time.Time) store.Channel {
	return store.Channel{
		ID:          channel.ID,
		WorkspaceID: workspaceID,
		Name:        channel.Name,
		Kind:        toStoreChannelKind(channel.Kind),
		IsPrivate:   channel.IsPrivate,
		RawJSON:     string(channel.RawJSON),
		UpdatedAt:   now,
	}
}

func toStoreChannelKind(kind string) string {
	switch kind {
	case "public":
		return "public_channel"
	case "private":
		return "private_channel"
	default:
		return kind
	}
}

func toStoreMessage(workspaceID, channelID string, raw map[string]any, now time.Time) (store.Message, []store.Mention, bool) {
	ts := stringValue(raw["ts"])
	if ts == "" {
		return store.Message{}, nil, false
	}
	text := stringValue(raw["text"])
	subtype := stringValue(raw["subtype"])
	if subtype == "" {
		if messageType := stringValue(raw["type"]); messageType != "" && messageType != "message" {
			subtype = messageType
		}
	}
	editedTS := editedTimestamp(raw["edited"])
	message := slack.Message{Msg: slack.Msg{
		Channel:          channelID,
		Timestamp:        ts,
		User:             stringValue(raw["user"]),
		SubType:          subtype,
		ClientMsgID:      stringValue(raw["client_msg_id"]),
		ThreadTimestamp:  stringValue(raw["thread_ts"]),
		ParentUserId:     stringValue(raw["parent_user_id"]),
		Text:             text,
		ReplyCount:       intValue(raw["reply_count"]),
		LatestReply:      stringValue(raw["latest_reply"]),
		DeletedTimestamp: stringValue(raw["deleted_ts"]),
	}}
	if editedTS != "" {
		message.Edited = &slack.Edited{Timestamp: editedTS}
	}
	message.Files = slackFilesFromRaw(raw["files"])

	rawMentions := search.ExtractMentions(text)
	mentions := make([]store.Mention, 0, len(rawMentions))
	for _, mention := range rawMentions {
		mentions = append(mentions, store.Mention{
			Type:        mention.Type,
			TargetID:    mention.TargetID,
			DisplayText: mention.DisplayText,
		})
	}

	return store.Message{
		ChannelID:      channelID,
		TS:             ts,
		WorkspaceID:    workspaceID,
		UserID:         message.User,
		Subtype:        message.SubType,
		ClientMsgID:    message.ClientMsgID,
		ThreadTS:       message.ThreadTimestamp,
		ParentUserID:   message.ParentUserId,
		Text:           text,
		NormalizedText: search.NormalizeMessage(message),
		ReplyCount:     message.ReplyCount,
		LatestReply:    message.LatestReply,
		EditedTS:       editedTS,
		DeletedTS:      message.DeletedTimestamp,
		SourceRank:     slackExportSourceRank,
		SourceName:     slackExportSourceName,
		RawJSON:        store.MarshalRaw(raw),
		UpdatedAt:      now,
		Files:          toStoreFilesFromSlack(workspaceID, message, now),
	}, mentions, true
}

func slackFilesFromRaw(value any) []slack.File {
	if value == nil {
		return nil
	}
	body, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var files []slack.File
	if err := json.Unmarshal(body, &files); err != nil {
		return nil
	}
	return files
}

func toStoreFilesFromSlack(workspaceID string, msg slack.Message, now time.Time) []store.MessageFile {
	files := make([]store.MessageFile, 0, len(msg.Files))
	for _, file := range msg.Files {
		if file.ID == "" {
			continue
		}
		userID := file.User
		if userID == "" {
			userID = msg.User
		}
		files = append(files, store.MessageFile{
			WorkspaceID:        workspaceID,
			ChannelID:          msg.Channel,
			TS:                 msg.Timestamp,
			FileID:             file.ID,
			UserID:             userID,
			Name:               file.Name,
			Title:              file.Title,
			Mimetype:           file.Mimetype,
			Filetype:           file.Filetype,
			PrettyType:         file.PrettyType,
			Mode:               file.Mode,
			Size:               int64(file.Size),
			URLPrivate:         file.URLPrivate,
			URLPrivateDownload: file.URLPrivateDownload,
			Permalink:          file.Permalink,
			IsPublic:           file.IsPublic,
			PlainText:          file.PlainText,
			PreviewPlainText:   file.PreviewPlainText,
			RawJSON:            store.MarshalRaw(file),
			UpdatedAt:          now,
		})
	}
	return files
}

func editedTimestamp(value any) string {
	edited, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	return stringValue(edited["ts"])
}

func stringValue(value any) string {
	s, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

func intValue(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err == nil {
			return parsed
		}
	}
	return 0
}

func (a *App) writeImportText(report ImportReport, progress []importChannelProgress) error {
	var b strings.Builder
	writeBanner(&b, "Import")

	fmt.Fprintf(&b, "Workspace: %s\n", report.Workspace)
	fmt.Fprintf(&b, "Dry run:   %t\n\n", report.DryRun)

	b.WriteString("Per-channel messages\n")
	b.WriteString("channel              kind          imported  skipped\n")
	b.WriteString("-------------------- ------------ --------- --------\n")
	for _, row := range progress {
		fmt.Fprintf(&b, "%-20s %-12s %9d %8d\n", row.Name, row.Kind, row.Messages, row.Skipped)
	}

	b.WriteString("\nTotals\n")
	fmt.Fprintf(&b, "users:    %d\n", report.Users)
	fmt.Fprintf(&b, "channels: %d\n", report.Channels)
	fmt.Fprintf(&b, "dms:      %d\n", report.DMs)
	fmt.Fprintf(&b, "mpims:    %d\n", report.MPIMs)
	fmt.Fprintf(&b, "messages: %d\n", report.Messages)
	fmt.Fprintf(&b, "skipped:  %d\n", report.Skipped)
	fmt.Fprintf(&b, "elapsed:  %s\n", report.Elapsed)

	_, err := io.WriteString(a.Stdout, b.String())
	return err
}
