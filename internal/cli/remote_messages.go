package cli

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
)

func remoteMessages(ctx context.Context, cfg config.Config, flags slacrawlMessagesArgs) ([]store.MessageRow, error) {
	if flags.Limit < 1 || flags.Limit > 500 {
		return nil, errors.New("cloud messages --limit must be between 1 and 500")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, err := remote.NewClientFromConfig(cfg.Remote, remote.Options{})
	if err != nil {
		return nil, err
	}
	req := remote.QueryRequest{Name: "slacrawl.messages.list", Limit: flags.Limit, Args: map[string]any{"workspace": coalesce(flags.Workspace, cfg.WorkspaceID), "channel": flags.Channel, "author": flags.Author}}
	if flags.Thread != "" {
		req.Name = "slacrawl.messages.thread"
		req.Args["thread"] = flags.Thread
	}
	out := []store.MessageRow{}
	pin := ""
	for len(out) < flags.Limit {
		req.Limit = flags.Limit - len(out)
		result, e := client.Query(ctx, "slacrawl", cfg.Remote.Archive, req)
		if e != nil {
			return nil, e
		}
		if result.Stats.SnapshotID == "" || pin != "" && pin != result.Stats.SnapshotID {
			return nil, errors.New("cloud messages snapshot changed")
		}
		pin = result.Stats.SnapshotID
		expected := []string{"workspace_id", "workspace_name", "channel_id", "channel_name", "ts", "user_id", "user_name", "text", "normalized_text", "thread_ts", "reply_count", "latest_reply", "subtype", "source_name", "mention_displays"}
		if len(result.Columns) != len(expected) || len(result.Rows) > req.Limit {
			return nil, errors.New("cloud messages contract differs")
		}
		for i, c := range expected {
			if result.Columns[i] != c {
				return nil, errors.New("cloud messages columns differ")
			}
		}
		for _, values := range result.Rows {
			if len(values) != len(expected) {
				return nil, errors.New("cloud message row differs")
			}
			fields := make([]string, 14)
			for i := range fields {
				if values[i] == nil {
					continue
				}
				s, ok := values[i].(string)
				if !ok {
					return nil, errors.New("cloud message field is not text")
				}
				fields[i] = s
			}
			count, e := strconv.Atoi(fields[10])
			if e != nil {
				return nil, errors.New("cloud reply count is invalid")
			}
			items, ok := values[14].([]any)
			if !ok {
				return nil, errors.New("cloud mention metadata differs")
			}
			mentions := make([][2]string, 0, len(items))
			for _, item := range items {
				pair, ok := item.([]any)
				if !ok || len(pair) != 2 {
					return nil, errors.New("cloud mention pair differs")
				}
				id, idOK := pair[0].(string)
				display, displayOK := pair[1].(string)
				if !idOK || !displayOK {
					return nil, errors.New("cloud mention field differs")
				}
				mentions = append(mentions, [2]string{id, display})
			}
			out = append(out, store.MessageRow{WorkspaceID: fields[0], WorkspaceName: fields[1], ChannelID: fields[2], ChannelName: fields[3], TS: fields[4], UserID: fields[5], UserName: fields[6], Text: fields[7], NormalizedText: store.ResolveMessageMentions(fields[8], mentions), ThreadTS: fields[9], ReplyCount: count, LatestReply: fields[11], Subtype: fields[12], SourceName: fields[13]})
		}
		if result.Cursor == "" {
			break
		}
		if len(result.Rows) == 0 || result.Cursor == req.Cursor {
			return nil, errors.New("cloud messages cursor did not advance")
		}
		req.Cursor = result.Cursor
		req.SnapshotID = pin
	}
	return out, nil
}
