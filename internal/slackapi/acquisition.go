package slackapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/openclaw/crawlkit/remote"
	"github.com/slack-go/slack"
)

// AcquisitionPage retains original JSON alongside the existing SDK mapping.
// No local store is opened by this provider path.
type AcquisitionPage struct {
	Messages   []json.RawMessage
	HasMore    bool
	IsLimited  bool
	NextCursor string
}

func (c *Client) AcquisitionIdentity(ctx context.Context, sourceName, workspace string) error {
	token, err := c.acquisitionToken(sourceName)
	if err != nil {
		return err
	}
	identity, err := c.authTest(ctx, token)
	if err != nil {
		return err
	}
	if identity.TeamID != workspace {
		return errors.New("slack token workspace differs from requested workspace")
	}
	return nil
}

func (c *Client) acquisitionToken(source string) (string, error) {
	token := ""
	switch source {
	case SourceBot:
		token = c.tokens.Bot
	case SourceUser:
		token = c.tokens.User
	}
	if token == "" {
		return "", errors.New("selected Slack API credential is not configured")
	}
	return token, nil
}

func (c *Client) AcquisitionHistory(ctx context.Context, source, channel string, since, horizon time.Time, cursor string) (AcquisitionPage, error) {
	token, err := c.acquisitionToken(source)
	if err != nil {
		return AcquisitionPage{}, err
	}
	return retry(ctx, c.sleep, 3, func() (AcquisitionPage, error) {
		var response rawConversationHistoryResponse
		values := conversationHistoryValues(&slack.GetConversationHistoryParameters{ChannelID: channel, Cursor: cursor, Oldest: slackTimestamp(since), Latest: slackTimestamp(horizon), Inclusive: true, Limit: 100, IncludeAllMetadata: true})
		if _, err := c.postSlackFormLimit(ctx, token, "conversations.history", values, &response, 4<<20); err != nil {
			return AcquisitionPage{}, err
		}
		if err := nativePageSuccess("conversations.history", response.SlackResponse, response.Messages != nil); err != nil {
			return AcquisitionPage{}, err
		}
		return AcquisitionPage{response.Messages, response.HasMore, response.IsLimited, response.ResponseMetaData.NextCursor}, nil
	})
}

func slackTimestamp(t time.Time) string { return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000) }

func AcquisitionTimestamp(s string) (time.Time, error) {
	if len(s) < 8 || s[len(s)-7] != '.' {
		return time.Time{}, errors.New("invalid Slack timestamp")
	}
	whole, fraction := s[:len(s)-7], s[len(s)-6:]
	sec, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || sec < 0 || strconv.FormatInt(sec, 10) != whole {
		return time.Time{}, errors.New("invalid Slack timestamp")
	}
	for _, v := range fraction {
		if v < '0' || v > '9' {
			return time.Time{}, errors.New("invalid Slack timestamp")
		}
	}
	micro, _ := strconv.ParseInt(fraction, 10, 64)
	return time.Unix(sec, micro*1000).UTC(), nil
}
func text(s string) remote.ChangeValue { return remote.ChangeValue{Kind: "text", Value: &s} }
func nullable(s string) remote.ChangeValue {
	if s == "" {
		return remote.ChangeValue{Kind: "null"}
	}
	return text(s)
}

func integer(n int64) remote.ChangeValue {
	s := strconv.FormatInt(n, 10)
	return remote.ChangeValue{Kind: "integer", Value: &s}
}

func boolean(b bool) remote.ChangeValue {
	if b {
		return integer(1)
	}
	return integer(0)
}

// AcquisitionChanges shares native normalization but preserves raw response
// bytes. Existing revisions are reconciled by the API collector before writing.
func AcquisitionChanges(raw json.RawMessage, workspace, channel, source string, now time.Time) ([]remote.Change, error) {
	var msg slack.Message
	if json.Unmarshal(raw, &msg) != nil || msg.Timestamp == "" || (msg.Channel != "" && msg.Channel != channel) {
		return nil, errors.New("invalid Slack history message")
	}
	if _, err := AcquisitionTimestamp(msg.Timestamp); err != nil {
		return nil, err
	}
	if msg.DeletedTimestamp != "" || msg.SubType == "message_deleted" || msg.SubType == "message_changed" {
		return nil, errors.New("slack edits and deletions require the refresh protocol")
	}
	rank := 1
	if source == SourceBot {
		rank = 2
	} else if source != SourceUser {
		return nil, errors.New("invalid Slack API source")
	}
	msg.Channel = channel
	m := toStoreMessage(workspace, msg, source, rank, rawPayloadFromMessageJSON(raw), now)
	key := []remote.ChangeValue{text(channel), text(m.TS)}
	row := []remote.ChangeValue{text(channel), text(m.TS), text(workspace), nullable(m.UserID), nullable(m.Subtype), nullable(m.ClientMsgID), nullable(m.ThreadTS), nullable(m.ParentUserID), text(m.Text), text(m.NormalizedText), integer(int64(m.ReplyCount)), nullable(m.LatestReply), nullable(m.EditedTS), nullable(m.DeletedTS), integer(int64(m.SourceRank)), text(source), text(string(raw)), text(now.Format(time.RFC3339Nano))}
	changes := []remote.Change{{Table: "messages", Operation: "upsert", Key: key, Row: row}}
	var fields struct {
		Files []json.RawMessage `json:"files"`
	}
	if json.Unmarshal(raw, &fields) != nil {
		return nil, errors.New("invalid Slack file metadata")
	}
	fileRaw := map[string]string{}
	for _, f := range fields.Files {
		var id struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(f, &id) != nil {
			return nil, errors.New("invalid Slack file metadata")
		}
		fileRaw[id.ID] = string(f)
	}
	for _, f := range m.Files {
		r := []remote.ChangeValue{text(workspace), text(channel), text(m.TS), text(f.FileID), nullable(f.UserID), nullable(f.Name), nullable(f.Title), nullable(f.Mimetype), nullable(f.Filetype), nullable(f.PrettyType), nullable(f.Mode), integer(f.Size), nullable(f.URLPrivate), nullable(f.URLPrivateDownload), nullable(f.Permalink), boolean(f.IsPublic), nullable(f.PlainText), nullable(f.PreviewPlainText), nullable(f.ContentSHA256), integer(f.ContentSize), nullable(f.FetchedAt), text(fileRaw[f.FileID]), text(now.Format(time.RFC3339Nano)), nullable(""), nullable(""), nullable("")}
		changes = append(changes, remote.Change{Table: "message_files", Operation: "upsert", Key: []remote.ChangeValue{text(channel), text(m.TS), text(f.FileID)}, Row: r})
	}
	for _, mtn := range toStoreMentions(msg) {
		changes = append(changes, remote.Change{Table: "message_mentions", Operation: "upsert", Key: []remote.ChangeValue{text(channel), text(m.TS), text(mtn.Type), text(mtn.TargetID)}, Row: []remote.ChangeValue{text(channel), text(m.TS), text(mtn.Type), text(mtn.TargetID), nullable(mtn.DisplayText), nullable(""), nullable(""), nullable(""), text(now.Format(time.RFC3339Nano))}})
	}
	return changes, nil
}
