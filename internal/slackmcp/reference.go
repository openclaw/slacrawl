package slackmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type referenceResponse struct {
	OK               bool                      `json:"ok"`
	Error            string                    `json:"error"`
	Channels         []referenceChannel        `json:"channels"`
	Members          []referenceUser           `json:"members"`
	Messages         []referenceMessage        `json:"messages"`
	ResponseMetadata referenceResponseMetadata `json:"response_metadata"`
}

type referenceResponseMetadata struct {
	NextCursor string `json:"next_cursor"`
}

type referenceChannel struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IsPrivate  bool   `json:"is_private"`
	IsArchived bool   `json:"is_archived"`
	Topic      struct {
		Value string `json:"value"`
	} `json:"topic"`
	Purpose struct {
		Value string `json:"value"`
	} `json:"purpose"`
}

type referenceUser struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RealName string `json:"real_name"`
	IsBot    bool   `json:"is_bot"`
	TZ       string `json:"tz"`
	Profile  struct {
		RealName    string `json:"real_name"`
		DisplayName string `json:"display_name"`
		Title       string `json:"title"`
		Email       string `json:"email"`
	} `json:"profile"`
}

type referenceMessage struct {
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Username    string `json:"username"`
	Text        string `json:"text"`
	ReplyCount  int    `json:"reply_count"`
	LatestReply string `json:"latest_reply"`
	Reactions   []struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	} `json:"reactions"`
	Files []struct {
		Name     string `json:"name"`
		Title    string `json:"title"`
		Mimetype string `json:"mimetype"`
	} `json:"files"`
}

func (c *Client) referenceChannels(ctx context.Context, tools toolset) ([]ChannelRecord, error) {
	return collectPages(c.maxPages, func(cursor string) (page[ChannelRecord], error) {
		raw, err := c.mcp.CallToolText(ctx, tools.searchChannels, map[string]any{
			"cursor": cursor,
			"limit":  min(c.searchLimit, 200),
		})
		if err != nil {
			return page[ChannelRecord]{}, err
		}
		var response referenceResponse
		if err := decodeReferenceResponse(raw, &response); err != nil {
			return page[ChannelRecord]{}, fmt.Errorf("decode reference Slack channels: %w", err)
		}
		channels := make([]ChannelRecord, 0, len(response.Channels))
		for _, channel := range response.Channels {
			kind := "public_channel"
			if channel.IsPrivate {
				kind = "private_channel"
			}
			channels = append(channels, ChannelRecord{
				ID:         channel.ID,
				Name:       channel.Name,
				Kind:       kind,
				Topic:      channel.Topic.Value,
				Purpose:    channel.Purpose.Value,
				IsPrivate:  channel.IsPrivate,
				IsArchived: channel.IsArchived,
			})
		}
		return page[ChannelRecord]{Items: channels, NextCursor: response.ResponseMetadata.NextCursor}, nil
	})
}

func (c *Client) referenceUsers(ctx context.Context, tools toolset) ([]UserRecord, error) {
	return collectPages(c.maxPages, func(cursor string) (page[UserRecord], error) {
		raw, err := c.mcp.CallToolText(ctx, tools.searchUsers, map[string]any{
			"cursor": cursor,
			"limit":  min(c.searchLimit, 200),
		})
		if err != nil {
			return page[UserRecord]{}, err
		}
		var response referenceResponse
		if err := decodeReferenceResponse(raw, &response); err != nil {
			return page[UserRecord]{}, fmt.Errorf("decode reference Slack users: %w", err)
		}
		users := make([]UserRecord, 0, len(response.Members))
		for _, user := range response.Members {
			users = append(users, UserRecord{
				ID:       user.ID,
				Name:     firstNonEmpty(user.Profile.DisplayName, user.Name),
				RealName: firstNonEmpty(user.Profile.RealName, user.RealName),
				Title:    user.Profile.Title,
				Email:    user.Profile.Email,
				Timezone: user.TZ,
				IsBot:    user.IsBot,
			})
		}
		return page[UserRecord]{Items: users, NextCursor: response.ResponseMetadata.NextCursor}, nil
	})
}

func (c *Client) referenceChannelMessages(ctx context.Context, tools toolset, channelID, oldest string) (channelPage, error) {
	raw, err := c.mcp.CallToolText(ctx, tools.readChannel, map[string]any{
		"channel_id": channelID,
		"limit":      c.pageSize,
	})
	if err != nil {
		return channelPage{}, err
	}
	var response referenceResponse
	if err := decodeReferenceResponse(raw, &response); err != nil {
		return channelPage{}, fmt.Errorf("decode reference Slack channel history: %w", err)
	}
	messages := make([]MessageRecord, 0, len(response.Messages))
	for _, message := range response.Messages {
		if !timestampAtLeast(message.TS, oldest) {
			continue
		}
		messages = append(messages, referenceMessageRecord(channelID, message))
	}
	return channelPage{ChannelID: channelID, Messages: messages}, nil
}

func (c *Client) referenceThreadMessages(ctx context.Context, tools toolset, channelID, threadTS string) (threadPage, error) {
	raw, err := c.mcp.CallToolText(ctx, tools.readThread, map[string]any{
		"channel_id": channelID,
		"thread_ts":  threadTS,
	})
	if err != nil {
		return threadPage{}, err
	}
	var response referenceResponse
	if err := decodeReferenceResponse(raw, &response); err != nil {
		return threadPage{}, fmt.Errorf("decode reference Slack thread: %w", err)
	}
	result := threadPage{}
	for i, message := range response.Messages {
		record := referenceMessageRecord(channelID, message)
		if message.TS == threadTS || i == 0 && result.Parent == nil {
			record.ThreadTS = ""
			copy := record
			result.Parent = &copy
			continue
		}
		record.ThreadTS = threadTS
		result.Replies = append(result.Replies, record)
	}
	return result, nil
}

func decodeReferenceResponse(raw string, response *referenceResponse) error {
	if err := json.Unmarshal([]byte(raw), response); err != nil {
		return err
	}
	if response.Error != "" {
		return fmt.Errorf("Slack API error: %s", response.Error)
	}
	return nil
}

func referenceMessageRecord(channelID string, message referenceMessage) MessageRecord {
	authorID := firstNonEmpty(message.User, message.BotID)
	reactions := make([]string, 0, len(message.Reactions))
	for _, reaction := range message.Reactions {
		value := reaction.Name
		if reaction.Count > 0 {
			value += ":" + strconv.Itoa(reaction.Count)
		}
		reactions = append(reactions, value)
	}
	files := make([]string, 0, len(message.Files))
	for _, file := range message.Files {
		value := firstNonEmpty(file.Title, file.Name, file.Mimetype)
		if value != "" {
			files = append(files, value)
		}
	}
	return MessageRecord{
		ChannelID:     channelID,
		TS:            message.TS,
		ThreadTS:      message.ThreadTS,
		AuthorID:      authorID,
		AuthorName:    message.Username,
		Text:          message.Text,
		ReplyCount:    message.ReplyCount,
		LatestReply:   message.LatestReply,
		Reactions:     reactions,
		FileSummaries: files,
	}
}

func timestampAtLeast(value, oldest string) bool {
	oldest = strings.TrimSpace(oldest)
	if oldest == "" {
		return true
	}
	valueNumber, valueErr := strconv.ParseFloat(value, 64)
	oldestNumber, oldestErr := strconv.ParseFloat(oldest, 64)
	if valueErr == nil && oldestErr == nil {
		return valueNumber >= oldestNumber
	}
	return value >= oldest
}
