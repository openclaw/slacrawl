package slackapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openclaw/crawlkit/progress"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/search"
	"github.com/openclaw/slacrawl/internal/store"
)

const (
	SourceUser = "api-user"
	SourceBot  = "api-bot"
)

type Diagnostics struct {
	BotConfigured     bool   `json:"bot_configured"`
	AppConfigured     bool   `json:"app_configured"`
	UserConfigured    bool   `json:"user_configured"`
	ThreadCoverage    string `json:"thread_coverage"`
	DMsIncluded       bool   `json:"dms_included"`
	DMsMissingScope   string `json:"dms_missing_scope,omitempty"`
	BotAuthTeamID     string `json:"bot_auth_team_id,omitempty"`
	BotAuthTeam       string `json:"bot_auth_team,omitempty"`
	UserAuthAvailable bool   `json:"user_auth_available"`
	UserAuthError     string `json:"user_auth_error,omitempty"`
	AppTailAvailable  bool   `json:"app_tail_available"`
}

type SyncOptions struct {
	WorkspaceID     string
	Channels        []string
	ExcludeChannels []string
	Since           string
	Full            bool
	LatestOnly      bool
	Concurrency     int
	AutoJoin        *bool
}

type Client struct {
	bot          *slack.Client
	user         *slack.Client
	tokens       config.Tokens
	appToken     string
	apiURL       string
	httpClient   *http.Client
	includeDMs   bool
	sleep        func(context.Context, time.Duration) error
	now          func() time.Time
	socketModeFn func(*slack.Client) socketModeRunner
	logger       *slog.Logger
}

func New(tokens config.Tokens) *Client {
	return NewWithOptions(tokens, "", nil)
}

func NewWithOptions(tokens config.Tokens, apiURL string, httpClient *http.Client) *Client {
	client := &Client{
		tokens:     tokens,
		appToken:   tokens.App,
		apiURL:     slack.APIURL,
		httpClient: http.DefaultClient,
		includeDMs: tokens.User != "",
		sleep:      sleepContext,
		now:        func() time.Time { return time.Now().UTC() },
	}
	if apiURL != "" {
		client.apiURL = apiURL
	}
	if httpClient != nil {
		client.httpClient = httpClient
	}

	buildOptions := func(includeAppToken bool) []slack.Option {
		var options []slack.Option
		if apiURL != "" {
			options = append(options, slack.OptionAPIURL(apiURL))
		}
		if httpClient != nil {
			options = append(options, slack.OptionHTTPClient(httpClient))
		}
		if includeAppToken && tokens.App != "" {
			options = append(options, slack.OptionAppLevelToken(tokens.App))
		}
		return options
	}

	if tokens.Bot != "" {
		client.bot = slack.New(tokens.Bot, buildOptions(true)...)
	}
	if tokens.User != "" {
		client.user = slack.New(tokens.User, buildOptions(false)...)
	}
	client.socketModeFn = func(api *slack.Client) socketModeRunner {
		return managedSocketMode{client: socketmode.New(api)}
	}
	return client
}

func (c *Client) WithIncludeDMs(include bool) *Client {
	c.includeDMs = include
	return c
}

func (c *Client) WithLogger(logger *slog.Logger) *Client {
	c.logger = logger
	return c
}

func (c *Client) Doctor(ctx context.Context) (Diagnostics, error) {
	diag := Diagnostics{
		BotConfigured:  c.tokens.Bot != "",
		AppConfigured:  c.tokens.App != "",
		UserConfigured: c.tokens.User != "",
		ThreadCoverage: "partial",
	}
	if c.bot == nil {
		return diag, nil
	}

	resp, err := c.authTest(ctx, c.bot)
	if err != nil {
		return diag, err
	}
	diag.BotAuthTeamID = resp.TeamID
	diag.BotAuthTeam = resp.Team
	diag.AppTailAvailable = c.tokens.App != ""

	if c.user != nil {
		if _, err := c.authTest(ctx, c.user); err == nil {
			diag.UserAuthAvailable = true
			diag.ThreadCoverage = "full"
			if c.includeDMs {
				diag.DMsIncluded = true
				diag.DMsMissingScope = c.dmMissingScope(ctx, resp.TeamID)
			}
		} else {
			diag.UserAuthError = authErrorReason(err)
		}
	}
	return diag, nil
}

func (c *Client) Sync(ctx context.Context, st *store.Store, opts SyncOptions) error {
	if c.bot == nil {
		return errors.New("SLACK_BOT_TOKEN is required for api sync")
	}

	auth, err := c.authTest(ctx, c.bot)
	if err != nil {
		return err
	}
	workspaceID := auth.TeamID
	if opts.WorkspaceID != "" {
		workspaceID = opts.WorkspaceID
	}

	now := c.now()
	if err := st.UpsertWorkspace(ctx, store.Workspace{
		ID:           workspaceID,
		Name:         auth.Team,
		EnterpriseID: auth.EnterpriseID,
		RawJSON:      store.MarshalRaw(auth),
		UpdatedAt:    now,
	}); err != nil {
		return err
	}
	userRepliesAvailable := c.userAuthAvailable(ctx)
	threadRepliesSkipped := newThreadSkipTracker()

	channels, err := c.fetchChannels(ctx, workspaceID)
	if err != nil {
		return err
	}
	allow := make(map[string]struct{}, len(opts.Channels))
	for _, id := range opts.Channels {
		allow[id] = struct{}{}
	}
	excluded := excludedChannelNames(opts.ExcludeChannels)
	selectedChannels := make([]slack.Channel, 0, len(channels))
	for _, channel := range channels {
		if len(allow) > 0 {
			if _, ok := allow[channel.ID]; !ok {
				continue
			}
		}
		if channelExcluded(channel, excluded) {
			continue
		}
		selectedChannels = append(selectedChannels, channel)
	}
	if err := c.syncChannels(ctx, st, workspaceID, selectedChannels, opts, now, userRepliesAvailable, threadRepliesSkipped); err != nil {
		return err
	}

	var (
		users    []slack.User
		userByID map[string]slack.User
	)
	if c.includeDMs && userRepliesAvailable && c.user != nil {
		users, err = c.getUsers(ctx, c.bot)
		if err != nil {
			return err
		}
		userByID = make(map[string]slack.User, len(users))
		for _, user := range users {
			userByID[user.ID] = user
		}

		dms, err := c.fetchDMs(ctx, workspaceID)
		if err != nil {
			if isMissingScopeError(err) {
				if setErr := st.SetSyncState(ctx, SourceUser, "dms", workspaceID, "missing_scope"); setErr != nil {
					return setErr
				}
			} else {
				return err
			}
		} else {
			selectedDMs := make([]slack.Channel, 0, len(dms))
			for _, channel := range dms {
				if len(allow) > 0 {
					if _, ok := allow[channel.ID]; !ok {
						continue
					}
				}
				channel.Name = dmChannelName(channel, userByID)
				if channelExcluded(channel, excluded) {
					continue
				}
				selectedDMs = append(selectedDMs, channel)
			}
			if err := c.syncChannelsWithSource(ctx, st, workspaceID, selectedDMs, opts, now, userRepliesAvailable, channelSyncSource{
				historyClient:    c.user,
				token:            c.tokens.User,
				sourceName:       SourceUser,
				sourceRank:       1,
				allowJoin:        false,
				skipMissingScope: true,
				threadSkip:       threadRepliesSkipped,
			}); err != nil {
				return err
			}
		}
	}

	if users == nil {
		users, err = c.getUsers(ctx, c.bot)
		if err != nil {
			return err
		}
	}
	for _, user := range users {
		if err := st.UpsertUser(ctx, toStoreUser(workspaceID, user, now)); err != nil {
			return err
		}
	}

	threadCoverage := "partial"
	if userRepliesAvailable && !threadRepliesSkipped.Skipped() {
		threadSkipPrefix := workspaceID + "|"
		if opts.Full && len(opts.Channels) == 0 {
			if err := st.DeleteSyncStateByTypePrefix(ctx, SourceUser, "thread_skip", threadSkipPrefix); err != nil {
				return err
			}
		}
		hasThreadSkips, err := st.HasSyncStateType(ctx, SourceUser, "thread_skip")
		if err != nil {
			return err
		}
		if !hasThreadSkips {
			threadCoverage = "full"
		}
	}
	if err := st.SetSyncState(ctx, "doctor", "threads", "coverage", threadCoverage); err != nil {
		return err
	}
	return st.SetSyncState(ctx, SourceBot, "workspace", workspaceID, now.Format(time.RFC3339))
}

func (c *Client) Tail(ctx context.Context, st *store.Store, workspaceID string, repairEvery time.Duration) error {
	if c.bot == nil {
		return errors.New("SLACK_BOT_TOKEN is required for tail")
	}
	if c.appToken == "" {
		return errors.New("SLACK_APP_TOKEN is required for tail")
	}

	auth, err := c.authTest(ctx, c.bot)
	if err != nil {
		return err
	}
	if workspaceID == "" {
		workspaceID = auth.TeamID
	}

	socketClient := c.socketModeFn(c.bot)
	errCh := make(chan error, 1)
	go func() {
		errCh <- socketClient.Run()
	}()

	var ticker *time.Ticker
	if repairEvery > 0 {
		ticker = time.NewTicker(repairEvery)
		defer ticker.Stop()
	}

	for {
		select {
		case err := <-errCh:
			return err
		case event := <-socketClient.Events():
			if err := c.handleSocketModeEvent(ctx, st, workspaceID, socketClient, event); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-tickerChan(ticker):
			if err := c.repairWorkspace(ctx, st, workspaceID); err != nil {
				return err
			}
			if err := st.SetSyncState(ctx, "tail", "repair", workspaceID, c.now().Format(time.RFC3339)); err != nil {
				return err
			}
		}
	}
}

func (c *Client) HandleEventsAPIEvent(ctx context.Context, st *store.Store, workspaceID string, event slackevents.EventsAPIEvent) error {
	now := c.now()
	switch ev := event.InnerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		msg := messageFromEvent(ev)
		stored := toStoreMessage(workspaceID, msg, SourceBot, 2, rawMessagePayload(event), now)
		if msg.SubType == "message_deleted" || msg.DeletedTimestamp != "" {
			return st.MarkMessageDeleted(ctx, stored, toStoreMentions(msg))
		}
		return st.UpsertMessage(ctx, stored, toStoreMentions(msg))
	case *slackevents.ChannelRenameEvent:
		return st.RenameChannel(ctx, ev.Channel.ID, ev.Channel.Name)
	case *slackevents.ChannelArchiveEvent:
		return st.SetChannelArchived(ctx, ev.Channel, true)
	case *slackevents.ChannelUnarchiveEvent:
		return st.SetChannelArchived(ctx, ev.Channel, false)
	default:
		return nil
	}
}

func rawMessagePayload(event slackevents.EventsAPIEvent) any {
	callback, ok := event.Data.(*slackevents.EventsAPICallbackEvent)
	if !ok || callback.InnerEvent == nil {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal(*callback.InnerEvent, &raw); err != nil {
		return nil
	}
	return rawMessageFieldsPayload(raw)
}

func rawMessageFieldsPayload(raw map[string]any) any {
	if nested, ok := raw["message"].(map[string]any); ok {
		if payload := rawMessageFieldsPayload(nested); payload != nil {
			return payload
		}
	}
	blocks, hasBlocks := raw["blocks"]
	attachments, hasAttachments := raw["attachments"]
	if !hasBlocks && !hasAttachments {
		return nil
	}
	return []any{blocks, attachments}
}

func (c *Client) fetchChannels(ctx context.Context, workspaceID string) ([]slack.Channel, error) {
	var (
		cursor   string
		channels []slack.Channel
	)
	for {
		page, nextCursor, err := c.getConversations(ctx, &slack.GetConversationsParameters{
			Cursor:          cursor,
			ExcludeArchived: false,
			Limit:           200,
			Types:           []string{"public_channel", "private_channel"},
			TeamID:          workspaceID,
		})
		if err != nil {
			return nil, err
		}
		channels = append(channels, page...)
		if nextCursor == "" {
			return channels, nil
		}
		cursor = nextCursor
	}
}

func (c *Client) syncChannelMessagesWithSource(ctx context.Context, st *store.Store, workspaceID string, channel slack.Channel, oldest string, now time.Time, userRepliesAvailable bool, source channelSyncSource) error {
	if source.historyClient == nil {
		return errors.New("history client is required")
	}
	if source.sourceName == "" {
		source.sourceName = SourceBot
	}
	if source.sourceRank == 0 {
		source.sourceRank = 2
	}

	cursor := ""
	joined := false
	for {
		resp, err := c.getConversationHistory(ctx, source.token, &slack.GetConversationHistoryParameters{
			ChannelID: channel.ID,
			Cursor:    cursor,
			Limit:     200,
			Oldest:    oldest,
		})
		if err != nil {
			if source.skipMissingScope && isMissingScopeError(err) {
				return st.SetSyncState(ctx, source.sourceName, "channel_skip", channel.ID, "missing_scope")
			}
			if source.allowJoin && !joined && channelSkipReason(err) == "not_in_channel" && !channel.IsPrivate {
				joinErr := c.joinConversation(ctx, channel.ID)
				if joinErr == nil {
					joined = true
					if setErr := st.SetSyncState(ctx, source.sourceName, "channel_join", channel.ID, "joined"); setErr != nil {
						return setErr
					}
					continue
				}
				if setErr := st.SetSyncState(ctx, source.sourceName, "channel_join", channel.ID, "failed:"+authErrorReason(joinErr)); setErr != nil {
					return setErr
				}
			}
			if isChannelHistorySkipped(err) {
				return st.SetSyncState(ctx, source.sourceName, "channel_skip", channel.ID, channelSkipReason(err))
			}
			return fmt.Errorf("channel %s history: %w", channel.ID, err)
		}
		for _, rawMsg := range resp.Messages {
			msg := rawMsg.Message
			if msg.Channel == "" {
				msg.Channel = channel.ID
			}
			if err := st.UpsertMessage(ctx, toStoreMessage(workspaceID, msg, source.sourceName, source.sourceRank, rawMsg.RawPayload, now), toStoreMentions(msg)); err != nil {
				return err
			}
			if msg.ReplyCount > 0 && userRepliesAvailable {
				threadKey := workspaceID + "|" + channel.ID + "|" + msg.Timestamp
				if source.threadSkip != nil {
					if reason, ok := source.threadSkip.SkipReason(channel.ID, threadSkipScope(channel)); ok {
						if setErr := st.SetSyncState(ctx, SourceUser, "thread_skip", threadKey, reason); setErr != nil {
							return setErr
						}
						continue
					}
				}
				if err := c.syncThread(ctx, st, workspaceID, channel.ID, msg.Timestamp, now); err != nil {
					if isThreadRepliesSkipped(err) {
						if source.threadSkip != nil {
							source.threadSkip.Record(channel.ID, threadSkipScope(channel), channelSkipReason(err))
						}
						if setErr := st.SetSyncState(ctx, SourceUser, "thread_skip", threadKey, channelSkipReason(err)); setErr != nil {
							return setErr
						}
						continue
					}
					return err
				}
				if err := st.DeleteSyncState(ctx, SourceUser, "thread_skip", threadKey); err != nil {
					return err
				}
			}
		}
		if resp.NextCursor == "" {
			return nil
		}
		cursor = resp.NextCursor
	}
}

func (c *Client) syncThread(ctx context.Context, st *store.Store, workspaceID string, channelID string, threadTS string, now time.Time) error {
	cursor := ""
	for {
		resp, err := c.getConversationReplies(ctx, &slack.GetConversationRepliesParameters{
			ChannelID: channelID,
			Timestamp: threadTS,
			Cursor:    cursor,
			Limit:     200,
		})
		if err != nil {
			return err
		}
		for _, rawMsg := range resp.Messages {
			msg := rawMsg.Message
			if msg.Channel == "" {
				msg.Channel = channelID
			}
			if err := st.UpsertMessage(ctx, toStoreMessage(workspaceID, msg, SourceUser, 1, rawMsg.RawPayload, now), toStoreMentions(msg)); err != nil {
				return err
			}
		}
		if resp.NextCursor == "" {
			return nil
		}
		cursor = resp.NextCursor
	}
}

func (c *Client) handleSocketModeEvent(ctx context.Context, st *store.Store, workspaceID string, socketClient socketModeRunner, event socketmode.Event) error {
	switch event.Type {
	case socketmode.EventTypeConnected:
		return st.SetSyncState(ctx, "tail", "connection", workspaceID, c.now().Format(time.RFC3339))
	case socketmode.EventTypeEventsAPI:
		eventsAPIEvent, ok := event.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return nil
		}
		if err := c.HandleEventsAPIEvent(ctx, st, workspaceID, eventsAPIEvent); err != nil {
			return err
		}
		if event.Request != nil {
			socketClient.Ack(*event.Request)
		}
		return nil
	default:
		return nil
	}
}

func (c *Client) repairWorkspace(ctx context.Context, st *store.Store, workspaceID string) error {
	if c.bot == nil {
		return errors.New("SLACK_BOT_TOKEN is required for repair")
	}
	channels, err := c.fetchChannels(ctx, workspaceID)
	if err != nil {
		return err
	}
	cursors, err := st.ChannelSyncCursors(ctx, workspaceID)
	if err != nil {
		return err
	}
	latestByChannel := make(map[string]string, len(cursors))
	for _, cursor := range cursors {
		latestByChannel[cursor.ID] = cursor.LatestTS
	}
	now := c.now()
	threadRepliesSkipped := newThreadSkipTracker()
	for _, channel := range channels {
		repairSince := repairOldest(latestByChannel[channel.ID], time.Hour)
		if err := c.syncChannels(ctx, st, workspaceID, []slack.Channel{channel}, SyncOptions{Since: repairSince}, now, c.userAuthAvailable(ctx), threadRepliesSkipped); err != nil {
			return err
		}
	}
	if threadRepliesSkipped.Skipped() {
		return st.SetSyncState(ctx, "doctor", "threads", "coverage", "partial")
	}
	return nil
}

func messageFromEvent(event *slackevents.MessageEvent) slack.Message {
	msg := slack.Message{}
	if event.Message != nil {
		msg.Msg = *event.Message
	}
	if event.PreviousMessage != nil {
		if msg.Text == "" {
			msg.Text = event.PreviousMessage.Text
		}
		if msg.Timestamp == "" || event.SubType == "message_deleted" {
			msg.Timestamp = event.PreviousMessage.Timestamp
		}
		if msg.ThreadTimestamp == "" || event.SubType == "message_deleted" {
			msg.ThreadTimestamp = event.PreviousMessage.ThreadTimestamp
		}
		if msg.User == "" {
			msg.User = event.PreviousMessage.User
		}
	}
	if msg.Channel == "" {
		msg.Channel = event.Channel
	}
	if msg.User == "" {
		msg.User = event.User
	}
	if msg.Text == "" {
		msg.Text = event.Text
	}
	if msg.Timestamp == "" {
		msg.Timestamp = event.TimeStamp
	}
	if event.SubType == "message_deleted" && event.DeletedTimeStamp != "" {
		msg.Timestamp = event.DeletedTimeStamp
	}
	if msg.ThreadTimestamp == "" {
		msg.ThreadTimestamp = event.ThreadTimeStamp
	}
	if msg.SubType == "" {
		msg.SubType = event.SubType
	}
	if event.SubType == "message_deleted" && msg.DeletedTimestamp == "" {
		msg.DeletedTimestamp = event.DeletedTimeStamp
	}
	return msg
}

func (c *Client) authTest(ctx context.Context, client *slack.Client) (*slack.AuthTestResponse, error) {
	return retry(ctx, c.sleep, 3, func() (*slack.AuthTestResponse, error) {
		return client.AuthTestContext(ctx)
	})
}

func (c *Client) getConversations(ctx context.Context, params *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	type result struct {
		channels   []slack.Channel
		nextCursor string
	}
	res, err := retry(ctx, c.sleep, 3, func() (result, error) {
		channels, nextCursor, err := c.bot.GetConversationsContext(ctx, params)
		return result{channels: channels, nextCursor: nextCursor}, err
	})
	return res.channels, res.nextCursor, err
}

type rawConversationMessage struct {
	Message    slack.Message
	RawPayload any
}

type conversationHistoryPage struct {
	Messages   []rawConversationMessage
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

func (c *Client) getConversationHistory(ctx context.Context, token string, params *slack.GetConversationHistoryParameters) (*conversationHistoryPage, error) {
	return retry(ctx, c.sleep, 3, func() (*conversationHistoryPage, error) {
		values := conversationHistoryValues(params)
		resp := rawConversationHistoryResponse{}
		if err := c.postSlackForm(ctx, token, "conversations.history", values, &resp); err != nil {
			return nil, err
		}
		if err := resp.Err(); err != nil {
			return nil, err
		}
		messages, err := rawConversationMessages(resp.Messages)
		if err != nil {
			return nil, err
		}
		return &conversationHistoryPage{Messages: messages, NextCursor: resp.ResponseMetaData.NextCursor}, nil
	})
}

func (c *Client) getConversationReplies(ctx context.Context, params *slack.GetConversationRepliesParameters) (*conversationRepliesPage, error) {
	return retry(ctx, c.sleep, 3, func() (*conversationRepliesPage, error) {
		values := conversationRepliesValues(params)
		resp := rawConversationRepliesResponse{}
		if err := c.postSlackForm(ctx, c.tokens.User, "conversations.replies", values, &resp); err != nil {
			return nil, err
		}
		if err := resp.Err(); err != nil {
			return nil, err
		}
		messages, err := rawConversationMessages(resp.Messages)
		if err != nil {
			return nil, err
		}
		return &conversationRepliesPage{
			Messages:   messages,
			HasMore:    resp.HasMore,
			NextCursor: resp.ResponseMetaData.NextCursor,
		}, nil
	})
}

func (c *Client) postSlackForm(ctx context.Context, token string, method string, values url.Values, target any) error {
	values.Set("token", token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+method, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests && resp.Header.Get("Retry-After") != "" {
		seconds, err := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64)
		if err != nil {
			return err
		}
		return &slack.RateLimitedError{RetryAfter: time.Duration(seconds) * time.Second}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack %s: %s", method, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("slack %s response: %w", method, err)
	}
	return nil
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

func (c *Client) getUsers(ctx context.Context, client *slack.Client) ([]slack.User, error) {
	return retry(ctx, c.sleep, 3, func() ([]slack.User, error) {
		return client.GetUsersContext(ctx)
	})
}

func (c *Client) joinConversation(ctx context.Context, channelID string) error {
	if c.bot == nil {
		return errors.New("SLACK_BOT_TOKEN is required for join")
	}
	_, err := retry(ctx, c.sleep, 3, func() (struct{}, error) {
		_, _, _, err := c.bot.JoinConversationContext(ctx, channelID)
		return struct{}{}, err
	})
	return err
}

func toStoreChannel(workspaceID string, channel slack.Channel, now time.Time) store.Channel {
	kind := "public_channel"
	switch dmChannelKind(channel) {
	case "im", "mpim":
		kind = dmChannelKind(channel)
	case "":
		if channel.IsPrivate {
			kind = "private_channel"
		}
	}
	return store.Channel{
		ID:          channel.ID,
		WorkspaceID: workspaceID,
		Name:        channel.Name,
		Kind:        kind,
		Topic:       channel.Topic.Value,
		Purpose:     channel.Purpose.Value,
		IsPrivate:   channel.IsPrivate,
		IsArchived:  channel.IsArchived,
		IsShared:    channel.IsShared,
		IsGeneral:   channel.IsGeneral,
		RawJSON:     store.MarshalRaw(channel),
		UpdatedAt:   now,
	}
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

func toStoreMessage(workspaceID string, msg slack.Message, sourceName string, sourceRank int, rawPayload any, now time.Time) store.Message {
	editedTS := ""
	if msg.Edited != nil {
		editedTS = msg.Edited.Timestamp
	}
	normalizedText := search.NormalizeMessage(msg)
	if rawPayload != nil {
		normalizedText = search.NormalizeMessageWithRawPayload(msg, rawPayload)
	}
	return store.Message{
		ChannelID:      msg.Channel,
		TS:             msg.Timestamp,
		WorkspaceID:    workspaceID,
		UserID:         msg.User,
		Subtype:        msg.SubType,
		ClientMsgID:    msg.ClientMsgID,
		ThreadTS:       msg.ThreadTimestamp,
		ParentUserID:   msg.ParentUserId,
		Text:           msg.Text,
		NormalizedText: normalizedText,
		ReplyCount:     msg.ReplyCount,
		LatestReply:    msg.LatestReply,
		EditedTS:       editedTS,
		DeletedTS:      msg.DeletedTimestamp,
		SourceRank:     sourceRank,
		SourceName:     sourceName,
		RawJSON:        store.MarshalRaw(msg),
		UpdatedAt:      now,
		Files:          toStoreFiles(workspaceID, msg, now),
	}
}

func toStoreFiles(workspaceID string, msg slack.Message, now time.Time) []store.MessageFile {
	files := make([]store.MessageFile, 0, len(msg.Files))
	for _, file := range msg.Files {
		if file.ID == "" {
			continue
		}
		files = append(files, store.MessageFile{
			WorkspaceID:        workspaceID,
			ChannelID:          msg.Channel,
			TS:                 msg.Timestamp,
			FileID:             file.ID,
			UserID:             firstNonEmpty(file.User, msg.User),
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func toStoreMentions(msg slack.Message) []store.Mention {
	raw := search.ExtractMentions(msg.Text)
	mentions := make([]store.Mention, 0, len(raw))
	for _, mention := range raw {
		mentions = append(mentions, store.Mention{
			Type:        mention.Type,
			TargetID:    mention.TargetID,
			DisplayText: mention.DisplayText,
		})
	}
	return mentions
}

type socketModeRunner interface {
	Run() error
	Ack(req socketmode.Request, payload ...interface{})
	Events() <-chan socketmode.Event
}

type managedSocketMode struct {
	client *socketmode.Client
}

func (m managedSocketMode) Run() error { return m.client.Run() }
func (m managedSocketMode) Ack(req socketmode.Request, payload ...interface{}) {
	_ = m.client.Ack(req, payload...)
}
func (m managedSocketMode) Events() <-chan socketmode.Event { return m.client.Events }

func retry[T any](ctx context.Context, sleeper func(context.Context, time.Duration) error, attempts int, fn func() (T, error)) (T, error) {
	var zero T
	for attempt := 0; attempt < attempts; attempt++ {
		value, err := fn()
		if err == nil {
			return value, nil
		}
		var rateLimited *slack.RateLimitedError
		if !errors.As(err, &rateLimited) || attempt == attempts-1 {
			return zero, err
		}
		if err := sleeper(ctx, rateLimited.RetryAfter); err != nil {
			return zero, err
		}
	}
	return zero, errors.New("retry exhausted")
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func repairOldest(latestTS string, overlap time.Duration) string {
	if latestTS == "" {
		return ""
	}
	parsed, err := strconv.ParseFloat(latestTS, 64)
	if err != nil {
		return latestTS
	}
	adjusted := math.Max(parsed-overlap.Seconds(), 0)
	return strconv.FormatFloat(adjusted, 'f', 6, 64)
}

func tickerChan(ticker *time.Ticker) <-chan time.Time {
	if ticker == nil {
		return nil
	}
	return ticker.C
}

type channelSyncSource struct {
	historyClient    *slack.Client
	token            string
	sourceName       string
	sourceRank       int
	allowJoin        bool
	skipMissingScope bool
	threadSkip       *threadSkipTracker
}

func (c *Client) syncChannels(ctx context.Context, st *store.Store, workspaceID string, channels []slack.Channel, opts SyncOptions, now time.Time, userRepliesAvailable bool, threadSkip *threadSkipTracker) error {
	return c.syncChannelsWithSource(ctx, st, workspaceID, channels, opts, now, userRepliesAvailable, channelSyncSource{
		historyClient: c.bot,
		token:         c.tokens.Bot,
		sourceName:    SourceBot,
		sourceRank:    2,
		allowJoin:     syncAutoJoin(opts),
		threadSkip:    threadSkip,
	})
}

func syncAutoJoin(opts SyncOptions) bool {
	return opts.AutoJoin == nil || *opts.AutoJoin
}

func excludedChannelNames(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	excluded := make(map[string]struct{}, len(values))
	for _, value := range values {
		name := normalizeChannelName(value)
		if name != "" {
			excluded[name] = struct{}{}
		}
	}
	return excluded
}

func channelExcluded(channel slack.Channel, excluded map[string]struct{}) bool {
	if len(excluded) == 0 {
		return false
	}
	if _, ok := excluded[normalizeChannelName(channel.Name)]; ok {
		return true
	}
	_, ok := excluded[normalizeChannelName(channel.ID)]
	return ok
}

func normalizeChannelName(value string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(value), "#"))
}

func (c *Client) syncChannelsWithSource(ctx context.Context, st *store.Store, workspaceID string, channels []slack.Channel, opts SyncOptions, now time.Time, userRepliesAvailable bool, source channelSyncSource) error {
	if len(channels) == 0 {
		return nil
	}
	channels, oldestByChannel, err := c.channelSyncPlan(ctx, st, workspaceID, channels, opts)
	if err != nil {
		return err
	}
	if len(channels) == 0 {
		return nil
	}
	workerCount := opts.Concurrency
	if workerCount <= 0 {
		workerCount = 1
	}
	if workerCount > len(channels) {
		workerCount = len(channels)
	}
	tracker := progress.New(c.logger, progress.Options{
		Name:  "sync",
		Unit:  "channels",
		Total: int64(len(channels)),
		Attrs: []any{"workspace_id", workspaceID, "source", source.sourceName},
	})
	if workerCount == 1 {
		for _, channel := range channels {
			if err := st.UpsertChannel(ctx, toStoreChannel(workspaceID, channel, now)); err != nil {
				tracker.Finish(err)
				return err
			}
			if err := c.syncChannelMessagesWithSource(ctx, st, workspaceID, channel, oldestByChannel[channel.ID], now, userRepliesAvailable, source); err != nil {
				tracker.Finish(err)
				return err
			}
			tracker.Add(1, "channel_id", channel.ID, "channel_name", channel.Name)
		}
		tracker.Finish(nil)
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	workCh := make(chan slack.Channel)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	worker := func() {
		defer wg.Done()
		for channel := range workCh {
			if err := st.UpsertChannel(ctx, toStoreChannel(workspaceID, channel, now)); err != nil {
				select {
				case errCh <- err:
				default:
				}
				cancel()
				return
			}
			if err := c.syncChannelMessagesWithSource(ctx, st, workspaceID, channel, oldestByChannel[channel.ID], now, userRepliesAvailable, source); err != nil {
				select {
				case errCh <- err:
				default:
				}
				cancel()
				return
			}
			tracker.Add(1, "channel_id", channel.ID, "channel_name", channel.Name)
		}
	}
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go worker()
	}
	for _, channel := range channels {
		select {
		case <-ctx.Done():
			close(workCh)
			wg.Wait()
			select {
			case err := <-errCh:
				tracker.Finish(err)
				return err
			default:
				if ctx.Err() != nil && !errors.Is(ctx.Err(), context.Canceled) {
					tracker.Finish(ctx.Err())
					return ctx.Err()
				}
				tracker.Finish(nil)
				return nil
			}
		case workCh <- channel:
		}
	}
	close(workCh)
	wg.Wait()

	select {
	case err := <-errCh:
		tracker.Finish(err)
		return err
	default:
		if ctx.Err() != nil && !errors.Is(ctx.Err(), context.Canceled) {
			tracker.Finish(ctx.Err())
			return ctx.Err()
		}
		tracker.Finish(nil)
		return nil
	}
}

func (c *Client) channelSyncPlan(ctx context.Context, st *store.Store, workspaceID string, channels []slack.Channel, opts SyncOptions) ([]slack.Channel, map[string]string, error) {
	out := make(map[string]string, len(channels))
	if opts.Since != "" {
		for _, channel := range channels {
			out[channel.ID] = opts.Since
		}
		return channels, out, nil
	}
	if opts.Full {
		return channels, out, nil
	}

	cursors, err := st.ChannelSyncCursors(ctx, workspaceID)
	if err != nil {
		return nil, nil, err
	}
	latestByChannel := make(map[string]string, len(cursors))
	for _, cursor := range cursors {
		latestByChannel[cursor.ID] = cursor.LatestTS
	}
	selected := make([]slack.Channel, 0, len(channels))
	for _, channel := range channels {
		latest := latestByChannel[channel.ID]
		if opts.LatestOnly && latest == "" {
			continue
		}
		selected = append(selected, channel)
		out[channel.ID] = repairOldest(latest, time.Hour)
	}
	return selected, out, nil
}

func isChannelHistorySkipped(err error) bool {
	reason := channelSkipReason(err)
	return reason == "not_in_channel" || reason == "channel_not_found"
}

func isThreadRepliesSkipped(err error) bool {
	reason := channelSkipReason(err)
	return reason == "missing_scope" || reason == "not_in_channel" || reason == "channel_not_found"
}

type threadSkipTracker struct {
	mu       sync.Mutex
	scopes   map[string]string
	channels map[string]string
}

func newThreadSkipTracker() *threadSkipTracker {
	return &threadSkipTracker{
		scopes:   make(map[string]string),
		channels: make(map[string]string),
	}
}

func (t *threadSkipTracker) Record(channelID, scope, reason string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if reason == "missing_scope" {
		t.scopes[scope] = reason
		return
	}
	t.channels[channelID] = reason
}

func (t *threadSkipTracker) SkipReason(channelID, scope string) (string, bool) {
	if t == nil {
		return "", false
	}
	t.mu.Lock()
	scopeReason, scopeSkipped := t.scopes[scope]
	channelReason, channelSkipped := t.channels[channelID]
	t.mu.Unlock()
	if scopeSkipped {
		return scopeReason, true
	}
	return channelReason, channelSkipped
}

func (t *threadSkipTracker) Skipped() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	skipped := len(t.scopes) > 0 || len(t.channels) > 0
	t.mu.Unlock()
	return skipped
}

func threadSkipScope(channel slack.Channel) string {
	if kind := dmChannelKind(channel); kind != "" {
		return kind
	}
	if channel.IsPrivate {
		return "private_channel"
	}
	return "public_channel"
}

func channelSkipReason(err error) string {
	var slackErr slack.SlackErrorResponse
	if errors.As(err, &slackErr) && slackErr.Err != "" {
		return slackErr.Err
	}
	return ""
}

func isMissingScopeError(err error) bool {
	return channelSkipReason(err) == "missing_scope"
}

func (c *Client) dmMissingScope(ctx context.Context, workspaceID string) string {
	if !c.includeDMs || c.user == nil {
		return ""
	}
	missing := make(map[string]struct{})
	addMissing := func(scope string) {
		if scope == "" {
			return
		}
		missing[scope] = struct{}{}
	}

	dms, err := c.fetchDMs(ctx, workspaceID)
	if err != nil {
		if isMissingScopeError(err) {
			addMissing("im:read")
			addMissing("mpim:read")
		}
		return joinScopes(missing)
	}

	var sampleIM, sampleMPIM string
	for _, dm := range dms {
		switch dmChannelKind(dm) {
		case "im":
			if sampleIM == "" {
				sampleIM = dm.ID
			}
		case "mpim":
			if sampleMPIM == "" {
				sampleMPIM = dm.ID
			}
		}
		if sampleIM != "" && sampleMPIM != "" {
			break
		}
	}

	if sampleIM != "" {
		_, historyErr := c.getConversationHistory(ctx, c.tokens.User, &slack.GetConversationHistoryParameters{
			ChannelID: sampleIM,
			Limit:     1,
		})
		if isMissingScopeError(historyErr) {
			addMissing("im:history")
		}
	}
	if sampleMPIM != "" {
		_, historyErr := c.getConversationHistory(ctx, c.tokens.User, &slack.GetConversationHistoryParameters{
			ChannelID: sampleMPIM,
			Limit:     1,
		})
		if isMissingScopeError(historyErr) {
			addMissing("mpim:history")
		}
	}

	return joinScopes(missing)
}

func joinScopes(scopes map[string]struct{}) string {
	if len(scopes) == 0 {
		return ""
	}
	out := make([]string, 0, len(scopes))
	for scope := range scopes {
		out = append(out, scope)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func (c *Client) userAuthAvailable(ctx context.Context) bool {
	if c.user == nil {
		return false
	}
	_, err := c.authTest(ctx, c.user)
	return err == nil
}

func authErrorReason(err error) string {
	if reason := channelSkipReason(err); reason != "" {
		return reason
	}
	return err.Error()
}
