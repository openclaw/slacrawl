package slackmcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/search"
	"github.com/openclaw/slacrawl/internal/store"
)

const (
	SourceName = "mcp"
	SourceRank = 4
)

var channelIDRE = regexp.MustCompile(`^[CDG][A-Z0-9]+$`)

type Options struct {
	WorkspaceID     string
	Channels        []string
	ExcludeChannels []string
	Since           string
	Full            bool
	LatestOnly      bool
	Logger          *slog.Logger
	HTTPClient      *http.Client
	Config          config.MCPConfig
}

type Summary struct {
	WorkspaceID string `json:"workspace_id,omitempty"`
	Channels    int    `json:"channels"`
	Users       int    `json:"users"`
	Messages    int    `json:"messages"`
	Replies     int    `json:"replies"`
}

func Sync(ctx context.Context, st *store.Store, opts Options) (Summary, error) {
	workspaceID := strings.TrimSpace(opts.WorkspaceID)
	if workspaceID == "" {
		return Summary{}, errors.New("workspace ID is required for MCP sync because the connector does not report one")
	}
	client, err := New(ctx, opts.Config, opts.HTTPClient)
	if err != nil {
		return Summary{}, err
	}
	defer func() { _ = client.Close() }()
	tools, err := client.discover(ctx)
	if err != nil {
		return Summary{}, err
	}
	now := time.Now().UTC()
	if err := st.EnsureWorkspace(ctx, store.Workspace{
		ID:        workspaceID,
		Name:      workspaceID,
		RawJSON:   store.MarshalRaw(map[string]any{"source": SourceName}),
		UpdatedAt: now,
	}); err != nil {
		return Summary{}, err
	}

	channels, err := resolveChannels(ctx, client, tools, opts.Channels)
	if err != nil {
		return Summary{}, err
	}
	channels = filterChannels(channels, opts.ExcludeChannels)
	userCount := 0
	if len(opts.Channels) == 0 {
		users, err := client.users(ctx, tools)
		if err != nil {
			return Summary{}, err
		}
		for _, user := range users {
			if err := st.EnsureUser(ctx, toStoreUser(workspaceID, user, now)); err != nil {
				return Summary{}, err
			}
		}
		userCount = len(users)
	}

	oldestByChannel, selected, err := syncPlan(ctx, st, workspaceID, channels, opts)
	if err != nil {
		return Summary{}, err
	}
	summary := Summary{WorkspaceID: workspaceID, Users: userCount}
	for _, channel := range selected {
		channelResult, err := client.channelMessages(ctx, tools, channel.ID, oldestByChannel[channel.ID])
		if err != nil {
			return summary, fmt.Errorf("read MCP channel %s: %w", channel.ID, err)
		}
		if channel.Name == "" {
			channel.Name = channelResult.ChannelName
		}
		if channel.Kind == "" {
			channel.Kind = "mcp_channel"
		}
		if err := st.EnsureChannel(ctx, toStoreChannel(workspaceID, channel, now)); err != nil {
			return summary, err
		}
		summary.Channels++

		threadRoots := map[string]struct{}{}
		for _, message := range channelResult.Messages {
			if err := upsertMessage(ctx, st, workspaceID, message, now); err != nil {
				return summary, err
			}
			summary.Messages++
			if message.ReplyCount > 0 {
				threadRoots[message.TS] = struct{}{}
			}
		}
		if tools.readThread == "" {
			continue
		}
		storedRoots, err := st.ChannelThreadRoots(ctx, workspaceID, channel.ID)
		if err != nil {
			return summary, err
		}
		for _, root := range storedRoots {
			threadRoots[root.TS] = struct{}{}
		}
		orderedRoots := make([]string, 0, len(threadRoots))
		for threadTS := range threadRoots {
			orderedRoots = append(orderedRoots, threadTS)
		}
		sort.Strings(orderedRoots)
		for _, threadTS := range orderedRoots {
			replies, err := syncThread(ctx, st, client, tools, workspaceID, channel.ID, threadTS, now)
			if err != nil {
				return summary, err
			}
			summary.Replies += replies
		}
	}
	if err := st.SetSyncState(ctx, SourceName, "workspace", workspaceID, now.Format(time.RFC3339)); err != nil {
		return summary, err
	}
	return summary, nil
}

func syncThread(ctx context.Context, st *store.Store, client *Client, tools toolset, workspaceID, channelID, threadTS string, now time.Time) (int, error) {
	thread, err := client.threadMessages(ctx, tools, channelID, threadTS)
	if err != nil {
		return 0, fmt.Errorf("read MCP thread %s/%s: %w", channelID, threadTS, err)
	}
	if thread.Parent != nil && (len(thread.Replies) > 0 || thread.Parent.ReplyCount > 0 || strings.TrimSpace(thread.Parent.LatestReply) != "") {
		thread.Parent.ReplyCount = max(thread.Parent.ReplyCount, len(thread.Replies))
		thread.Parent.LatestReply = latestReplyTS(thread.Parent.LatestReply, thread.Replies)
		if err := upsertMessage(ctx, st, workspaceID, *thread.Parent, now); err != nil {
			return 0, err
		}
	}
	for _, reply := range thread.Replies {
		if err := upsertMessage(ctx, st, workspaceID, reply, now); err != nil {
			return 0, err
		}
	}
	return len(thread.Replies), nil
}

func resolveChannels(ctx context.Context, client *Client, tools toolset, selectors []string) ([]ChannelRecord, error) {
	if len(selectors) == 0 {
		return client.channels(ctx, tools, "")
	}
	seen := map[string]struct{}{}
	var channels []ChannelRecord
	for _, selector := range selectors {
		selector = strings.TrimSpace(strings.TrimPrefix(selector, "#"))
		if selector == "" {
			continue
		}
		if channelIDRE.MatchString(selector) {
			if _, ok := seen[selector]; !ok {
				seen[selector] = struct{}{}
				channels = append(channels, ChannelRecord{ID: selector})
			}
			continue
		}
		matches, err := client.channels(ctx, tools, selector)
		if err != nil {
			return nil, err
		}
		found := false
		for _, channel := range matches {
			if !strings.EqualFold(channel.Name, selector) && !strings.EqualFold(channel.ID, selector) {
				continue
			}
			found = true
			if _, ok := seen[channel.ID]; !ok {
				seen[channel.ID] = struct{}{}
				channels = append(channels, channel)
			}
		}
		if !found {
			return nil, fmt.Errorf("MCP channel %q not found", selector)
		}
	}
	return channels, nil
}

func filterChannels(channels []ChannelRecord, excluded []string) []ChannelRecord {
	if len(excluded) == 0 {
		return channels
	}
	set := make(map[string]struct{}, len(excluded))
	for _, value := range excluded {
		set[strings.ToLower(strings.TrimPrefix(strings.TrimSpace(value), "#"))] = struct{}{}
	}
	filtered := make([]ChannelRecord, 0, len(channels))
	for _, channel := range channels {
		if _, ok := set[strings.ToLower(channel.ID)]; ok {
			continue
		}
		if _, ok := set[strings.ToLower(channel.Name)]; ok {
			continue
		}
		filtered = append(filtered, channel)
	}
	return filtered
}

func syncPlan(ctx context.Context, st *store.Store, workspaceID string, channels []ChannelRecord, opts Options) (map[string]string, []ChannelRecord, error) {
	oldest := make(map[string]string, len(channels))
	if opts.Since != "" {
		since := normalizeTimestamp(opts.Since)
		for _, channel := range channels {
			oldest[channel.ID] = since
		}
		return oldest, channels, nil
	}
	if opts.Full {
		return oldest, channels, nil
	}
	cursors, err := st.ChannelSyncCursors(ctx, workspaceID)
	if err != nil {
		return nil, nil, err
	}
	latest := make(map[string]string, len(cursors))
	for _, cursor := range cursors {
		latest[cursor.ID] = cursor.LatestTS
	}
	selected := make([]ChannelRecord, 0, len(channels))
	for _, channel := range channels {
		cursor := latest[channel.ID]
		if opts.LatestOnly && cursor == "" {
			continue
		}
		selected = append(selected, channel)
		oldest[channel.ID] = overlapTimestamp(cursor, time.Hour)
	}
	return oldest, selected, nil
}

func toStoreChannel(workspaceID string, channel ChannelRecord, now time.Time) store.Channel {
	return store.Channel{
		ID:          channel.ID,
		WorkspaceID: workspaceID,
		Name:        channel.Name,
		Kind:        channel.Kind,
		Topic:       channel.Topic,
		Purpose:     channel.Purpose,
		IsPrivate:   channel.IsPrivate,
		IsArchived:  channel.IsArchived,
		RawJSON:     store.MarshalRaw(channel),
		UpdatedAt:   now,
	}
}

func toStoreUser(workspaceID string, user UserRecord, now time.Time) store.User {
	return store.User{
		ID:          user.ID,
		WorkspaceID: workspaceID,
		Name:        user.Name,
		RealName:    user.RealName,
		DisplayName: user.Name,
		Title:       user.Title,
		IsBot:       user.IsBot,
		RawJSON:     store.MarshalRaw(user),
		UpdatedAt:   now,
	}
}

func upsertMessage(ctx context.Context, st *store.Store, workspaceID string, message MessageRecord, now time.Time) error {
	threadTS := message.ThreadTS
	if threadTS == message.TS {
		threadTS = ""
	}
	slackMessage := slack.Message{Msg: slack.Msg{
		Channel:         message.ChannelID,
		Timestamp:       message.TS,
		ThreadTimestamp: threadTS,
		User:            message.AuthorID,
		Text:            message.Text,
		ReplyCount:      message.ReplyCount,
		LatestReply:     normalizeTimestamp(message.LatestReply),
	}}
	mentions := search.ExtractMentions(message.Text)
	storedMentions := make([]store.Mention, 0, len(mentions))
	for _, mention := range mentions {
		storedMentions = append(storedMentions, store.Mention{
			Type:        mention.Type,
			TargetID:    mention.TargetID,
			DisplayText: mention.DisplayText,
		})
	}
	_, err := st.UpsertMessageByPriority(ctx, store.Message{
		ChannelID:      message.ChannelID,
		TS:             message.TS,
		WorkspaceID:    workspaceID,
		UserID:         message.AuthorID,
		ThreadTS:       threadTS,
		Text:           message.Text,
		NormalizedText: search.NormalizeMessage(slackMessage),
		ReplyCount:     message.ReplyCount,
		LatestReply:    normalizeTimestamp(message.LatestReply),
		SourceRank:     SourceRank,
		SourceName:     SourceName,
		RawJSON:        store.MarshalRaw(message),
		UpdatedAt:      now,
		Files:          nil,
	}, storedMentions)
	return err
}

func latestReplyTS(current string, replies []MessageRecord) string {
	latest := normalizeTimestamp(current)
	for _, reply := range replies {
		if reply.TS > latest {
			latest = reply.TS
		}
	}
	return latest
}

func normalizeTimestamp(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return value
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return fmt.Sprintf("%d.%06d", parsed.Unix(), parsed.Nanosecond()/1000)
	}
	return value
}

func overlapTimestamp(value string, overlap time.Duration) string {
	if value == "" {
		return ""
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return value
	}
	return strconv.FormatFloat(math.Max(parsed-overlap.Seconds(), 0), 'f', 6, 64)
}
