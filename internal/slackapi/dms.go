package slackapi

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/slack-go/slack"
)

func (c *Client) fetchDMs(ctx context.Context, workspaceID string) ([]slack.Channel, error) {
	if c.user == nil {
		return nil, nil
	}

	var (
		cursor string
		out    []slack.Channel
		seen   = map[string]bool{}
	)
	for {
		type result struct {
			channels   []slack.Channel
			nextCursor string
		}
		page, err := retry(ctx, c.sleep, 3, func() (result, error) {
			channels, nextCursor, callErr := c.user.GetConversationsContext(ctx, &slack.GetConversationsParameters{
				Cursor:          cursor,
				ExcludeArchived: false,
				Limit:           200,
				Types:           []string{"im", "mpim"},
				TeamID:          workspaceID,
			})
			return result{channels: channels, nextCursor: nextCursor}, callErr
		})
		if err != nil {
			return nil, err
		}
		for _, channel := range page.channels {
			if dmChannelKind(channel) == "" {
				continue
			}
			out = append(out, channel)
		}
		if page.nextCursor == "" {
			return out, nil
		}
		if seen[page.nextCursor] {
			return nil, fmt.Errorf("conversations.list repeated cursor %q", page.nextCursor)
		}
		seen[page.nextCursor] = true
		cursor = page.nextCursor
	}
}

func dmChannelKind(ch slack.Channel) string {
	switch {
	case ch.IsIM:
		return "im"
	case ch.IsMpIM:
		return "mpim"
	default:
		return ""
	}
}

func dmChannelName(ch slack.Channel, userByID map[string]slack.User) string {
	switch dmChannelKind(ch) {
	case "im":
		return dmUserDisplayName(ch.User, userByID)
	case "mpim":
		parts := make([]string, 0, len(ch.Members))
		for _, memberID := range ch.Members {
			parts = append(parts, dmUserDisplayName(memberID, userByID))
		}
		sort.Strings(parts)
		return truncateWithEllipsis(strings.Join(parts, ","), 40)
	default:
		if strings.TrimSpace(ch.Name) != "" {
			return ch.Name
		}
		return ch.ID
	}
}

func dmUserDisplayName(userID string, userByID map[string]slack.User) string {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ""
	}
	user, ok := userByID[userID]
	if !ok {
		return userID
	}
	if value := strings.TrimSpace(user.Profile.DisplayName); value != "" {
		return value
	}
	if value := strings.TrimSpace(user.RealName); value != "" {
		return value
	}
	if value := strings.TrimSpace(user.Name); value != "" {
		return value
	}
	return userID
}

func truncateWithEllipsis(value string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maxChars {
		return value
	}
	if maxChars <= 3 {
		return strings.Repeat(".", maxChars)
	}
	return string(runes[:maxChars-3]) + "..."
}
