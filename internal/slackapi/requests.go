package slackapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

func (c *Client) authTest(ctx context.Context, token string) (*slack.AuthTestResponse, error) {
	return retry(ctx, c.sleep, 3, func() (*slack.AuthTestResponse, error) {
		var response struct {
			slack.SlackResponse
			slack.AuthTestResponse
		}
		headers, err := c.postSlackForm(ctx, token, "auth.test", url.Values{}, &response)
		if err != nil {
			return nil, err
		}
		if err := nativeResponseSuccess("auth.test", response.SlackResponse); err != nil {
			return nil, err
		}
		response.AuthTestResponse.Header = headers.Clone()
		return &response.AuthTestResponse, nil
	})
}

func (c *Client) getConversationInfo(ctx context.Context, channelID string) (*slack.Channel, error) {
	if channelID == "" {
		return nil, errors.New("ChannelID must be defined")
	}
	return retry(ctx, c.sleep, 3, func() (*slack.Channel, error) {
		values := url.Values{"channel": {channelID}, "include_locale": {"false"}, "include_num_members": {"false"}}
		var response struct {
			Channel      slack.Channel   `json:"channel"`
			Channels     []slack.Channel `json:"channels"`
			Purpose      string          `json:"purpose"`
			Topic        string          `json:"topic"`
			NotInChannel bool            `json:"not_in_channel"`
			slack.History
			slack.SlackResponse
			Metadata slack.ResponseMetadata `json:"response_metadata"`
		}
		if _, err := c.postSlackForm(ctx, c.tokens.Bot, "conversations.info", values, &response); err != nil {
			return nil, err
		}
		if err := nativeResponseSuccess("conversations.info", response.SlackResponse); err != nil {
			return nil, err
		}
		return &response.Channel, nil
	})
}

func (c *Client) joinConversation(ctx context.Context, channelID string) error {
	if c.bot == nil {
		return errors.New("SLACK_BOT_TOKEN is required for join")
	}
	_, err := retry(ctx, c.sleep, 3, func() (struct{}, error) {
		var response struct {
			Channel  *slack.Channel `json:"channel"`
			Warning  string         `json:"warning"`
			Metadata *struct {
				Warnings []string `json:"warnings"`
			} `json:"response_metadata"`
			slack.SlackResponse
		}
		if _, err := c.postSlackForm(ctx, c.tokens.Bot, "conversations.join", url.Values{"channel": {channelID}}, &response); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nativeResponseSuccess("conversations.join", response.SlackResponse)
	})
	return err
}

func (c *Client) getConversations(ctx context.Context, token string, params *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	type result struct {
		channels   []slack.Channel
		nextCursor string
	}
	res, err := retry(ctx, c.sleep, 3, func() (result, error) {
		values := url.Values{}
		if params.Cursor != "" {
			values.Set("cursor", params.Cursor)
		}
		if params.Limit != 0 {
			values.Set("limit", strconv.Itoa(params.Limit))
		}
		if params.Types != nil {
			values.Set("types", strings.Join(params.Types, ","))
		}
		if params.ExcludeArchived {
			values.Set("exclude_archived", "true")
		}
		if params.TeamID != "" {
			values.Set("team_id", params.TeamID)
		}
		var response struct {
			slack.SlackResponse
			Channels []slack.Channel `json:"channels"`
			Metadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if _, err := c.postSlackForm(ctx, token, "conversations.list", values, &response); err != nil {
			return result{}, err
		}
		if err := nativePageSuccess("conversations.list", response.SlackResponse, response.Channels != nil); err != nil {
			return result{}, err
		}
		return result{channels: response.Channels, nextCursor: response.Metadata.NextCursor}, nil
	})
	return res.channels, res.nextCursor, err
}

type rawConversationMessage struct {
	Message    slack.Message
	RawPayload any
}

type conversationHistoryPage struct {
	Messages   []rawConversationMessage
	HasMore    bool
	IsLimited  bool
	NextCursor string
}

type conversationRepliesPage struct {
	Messages   []rawConversationMessage
	HasMore    bool
	NextCursor string
}

type rawConversationHistoryResponse struct {
	slack.SlackResponse
	HasMore          bool   `json:"has_more"`
	IsLimited        bool   `json:"is_limited"`
	PinCount         int    `json:"pin_count"`
	Latest           string `json:"latest"`
	ResponseMetaData struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
	Messages []json.RawMessage `json:"messages"`
}

type rawConversationRepliesResponse struct {
	slack.SlackResponse
	HasMore          bool `json:"has_more"`
	ResponseMetaData struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
	Messages []json.RawMessage `json:"messages"`
}

func (c *Client) getConversationHistory(ctx context.Context, token string, params *slack.GetConversationHistoryParameters, check func() error) (*conversationHistoryPage, error) {
	return retry(ctx, c.sleep, 3, func() (*conversationHistoryPage, error) {
		if err := checkHistoryRequest(ctx, check); err != nil {
			return nil, err
		}
		values := conversationHistoryValues(params)
		resp := rawConversationHistoryResponse{}
		_, requestErr := c.postSlackForm(ctx, token, "conversations.history", values, &resp)
		if err := checkHistoryRequest(ctx, check); err != nil {
			return nil, err
		}
		if requestErr != nil {
			return nil, requestErr
		}
		if err := nativePageSuccess("conversations.history", resp.SlackResponse, resp.Messages != nil); err != nil {
			return nil, err
		}
		messages, err := rawConversationMessages(resp.Messages)
		if err != nil {
			return nil, &nativeDiagnosticError{method: "conversations.history", phase: "message decode", cause: err}
		}
		return &conversationHistoryPage{
			Messages:   messages,
			HasMore:    resp.HasMore,
			IsLimited:  resp.IsLimited,
			NextCursor: resp.ResponseMetaData.NextCursor,
		}, nil
	})
}

func (c *Client) getConversationReplies(ctx context.Context, token string, params *slack.GetConversationRepliesParameters, check func() error) (*conversationRepliesPage, error) {
	return retry(ctx, c.sleep, 3, func() (*conversationRepliesPage, error) {
		if err := checkHistoryRequest(ctx, check); err != nil {
			return nil, err
		}
		values := conversationRepliesValues(params)
		resp := rawConversationRepliesResponse{}
		_, requestErr := c.postSlackForm(ctx, token, "conversations.replies", values, &resp)
		if err := checkHistoryRequest(ctx, check); err != nil {
			return nil, err
		}
		if requestErr != nil {
			return nil, requestErr
		}
		if err := nativePageSuccess("conversations.replies", resp.SlackResponse, resp.Messages != nil); err != nil {
			return nil, err
		}
		messages, err := rawConversationMessages(resp.Messages)
		if err != nil {
			return nil, &nativeDiagnosticError{method: "conversations.replies", phase: "message decode", cause: err}
		}
		return &conversationRepliesPage{
			Messages:   messages,
			HasMore:    resp.HasMore,
			NextCursor: resp.ResponseMetaData.NextCursor,
		}, nil
	})
}

func nativePageSuccess(method string, response slack.SlackResponse, collectionPresent bool) error {
	if err := nativeResponseSuccess(method, response); err != nil {
		return err
	}
	// Freshly decoded slices distinguish an explicit empty array from missing
	// or null data, which cannot certify an empty archive page.
	if !collectionPresent {
		return fmt.Errorf("%s response did not provide a collection array; page remains uncertified", method)
	}
	return nil
}

func nativeResponseSuccess(method string, response slack.SlackResponse) error {
	if err := response.Err(); err != nil {
		return &nativeDiagnosticError{method: method, phase: "API response", safeCode: safeNativeErrorCode(response.Error), cause: err}
	}
	// Slack's Err permits blank-error responses from non-JSON methods. Native
	// API responses must affirm success before callers can use their payloads.
	if !response.Ok {
		return fmt.Errorf("%s response did not report success", method)
	}
	return nil
}

func safeNativeErrorCode(code string) string {
	switch code {
	case "missing_scope", "not_in_channel", "channel_not_found", "invalid_auth", "not_authed",
		"account_inactive", "token_expired", "token_revoked", "is_archived", "thread_not_found":
		return code
	}
	return ""
}

// Causes can contain private response text or URLs. Render only exact selected
// codes or caller-owned labels; explicit cause inspection exposes the original.
type nativeDiagnosticError struct {
	method   string
	phase    string
	status   int
	cause    error
	safeCode string
}

func (e *nativeDiagnosticError) Error() string {
	if e.safeCode != "" {
		return e.safeCode
	}
	if e.status != 0 {
		return fmt.Sprintf("slack %s %s failed (HTTP %d)", e.method, e.phase, e.status)
	}
	return fmt.Sprintf("slack %s %s failed", e.method, e.phase)
}

func (e *nativeDiagnosticError) Unwrap() error { return e.cause }

func (c *Client) postSlackForm(ctx context.Context, token string, method string, values url.Values, target any) (http.Header, error) {
	values.Set("token", token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+method, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, &nativeDiagnosticError{method: method, phase: "request construction", cause: err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &nativeDiagnosticError{method: method, phase: "request execution", cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests && resp.Header.Get("Retry-After") != "" {
		seconds, err := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64)
		if err != nil {
			return nil, &nativeDiagnosticError{method: method, phase: "rate-limit header", cause: err}
		}
		return nil, &slack.RateLimitedError{RetryAfter: time.Duration(seconds) * time.Second}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &nativeDiagnosticError{method: method, phase: "HTTP status", status: resp.StatusCode, cause: slack.StatusCodeError{Code: resp.StatusCode, Status: resp.Status}}
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &nativeDiagnosticError{method: method, phase: "response body", cause: err}
	}
	if err := json.Unmarshal(body, target); err != nil {
		return nil, &nativeDiagnosticError{method: method, phase: "response decode", cause: err}
	}
	return resp.Header, nil
}

func conversationHistoryValues(params *slack.GetConversationHistoryParameters) url.Values {
	values := url.Values{"channel": {params.ChannelID}}
	if params.Cursor != "" {
		values.Set("cursor", params.Cursor)
	}
	if params.Inclusive {
		values.Set("inclusive", "1")
	} else {
		values.Set("inclusive", "0")
	}
	if params.Latest != "" {
		values.Set("latest", params.Latest)
	}
	if params.Limit != 0 {
		values.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Oldest != "" {
		values.Set("oldest", params.Oldest)
	}
	if params.IncludeAllMetadata {
		values.Set("include_all_metadata", "1")
	} else {
		values.Set("include_all_metadata", "0")
	}
	return values
}

func conversationRepliesValues(params *slack.GetConversationRepliesParameters) url.Values {
	values := url.Values{
		"channel": {params.ChannelID},
		"ts":      {params.Timestamp},
	}
	if params.Cursor != "" {
		values.Set("cursor", params.Cursor)
	}
	if params.Latest != "" {
		values.Set("latest", params.Latest)
	}
	if params.Limit != 0 {
		values.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Oldest != "" {
		values.Set("oldest", params.Oldest)
	}
	if params.Inclusive {
		values.Set("inclusive", "1")
	} else {
		values.Set("inclusive", "0")
	}
	if params.IncludeAllMetadata {
		values.Set("include_all_metadata", "1")
	} else {
		values.Set("include_all_metadata", "0")
	}
	return values
}

func rawConversationMessages(rawMessages []json.RawMessage) ([]rawConversationMessage, error) {
	messages := make([]rawConversationMessage, 0, len(rawMessages))
	for _, raw := range rawMessages {
		var msg slack.Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			return nil, err
		}
		messages = append(messages, rawConversationMessage{
			Message:    msg,
			RawPayload: rawPayloadFromMessageJSON(raw),
		})
	}
	return messages, nil
}

func rawPayloadFromMessageJSON(rawMessage json.RawMessage) any {
	var raw map[string]any
	if err := json.Unmarshal(rawMessage, &raw); err != nil {
		return nil
	}
	return rawMessageFieldsPayload(raw)
}

func (c *Client) getUsers(ctx context.Context, token string) ([]slack.User, error) {
	var (
		cursor string
		users  []slack.User
		seen   = map[string]bool{}
	)
	for {
		type result struct {
			users      []slack.User
			nextCursor string
		}
		page, err := retry(ctx, c.sleep, 3, func() (result, error) {
			values := url.Values{
				"limit": {"200"}, "presence": {"false"}, "cursor": {cursor},
				"team_id": {""}, "include_locale": {"true"},
			}
			var response struct {
				slack.SlackResponse
				Members  []slack.User           `json:"members"`
				Metadata slack.ResponseMetadata `json:"response_metadata"`
			}
			if _, err := c.postSlackForm(ctx, token, "users.list", values, &response); err != nil {
				return result{}, err
			}
			if err := nativePageSuccess("users.list", response.SlackResponse, response.Members != nil); err != nil {
				return result{}, err
			}
			return result{users: response.Members, nextCursor: response.Metadata.Cursor}, nil
		})
		if err != nil {
			return nil, err
		}
		users = append(users, page.users...)
		if page.nextCursor == "" {
			return users, nil
		}
		if seen[page.nextCursor] {
			return nil, errors.New("users.list repeated cursor")
		}
		seen[page.nextCursor] = true
		cursor = page.nextCursor
	}
}

// Retries remain rate-limit-only, but every physical attempt rechecks ownership.
func checkHistoryRequest(ctx context.Context, check func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if check != nil {
		return check()
	}
	return nil
}
