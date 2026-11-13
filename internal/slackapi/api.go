package slackapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/openclaw/slacrawl/internal/admission"
	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
)

const (
	SourceUser = "api-user"
	SourceBot  = "api-bot"

	// defaultHTTPTimeout bounds Slack API HTTP when NewWithOptions gets a nil client.
	defaultHTTPTimeout = 60 * time.Second
)

func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: defaultHTTPTimeout}
}

type Diagnostics struct {
	BotConfigured        bool   `json:"bot_configured"`
	AppConfigured        bool   `json:"app_configured"`
	UserConfigured       bool   `json:"user_configured"`
	ThreadCoverage       string `json:"thread_coverage"`
	ThreadCoverageReason string `json:"thread_coverage_reason,omitempty"`
	DMsIncluded          bool   `json:"dms_included"`
	DMsMissingScope      string `json:"dms_missing_scope,omitempty"`
	DMProbeError         string `json:"dm_probe_error,omitempty"`
	BotAuthTeamID        string `json:"bot_auth_team_id,omitempty"`
	BotAuthTeam          string `json:"bot_auth_team,omitempty"`
	UserAuthAvailable    bool   `json:"user_auth_available"`
	UserAuthError        string `json:"user_auth_error,omitempty"`
	AppTailAvailable     bool   `json:"app_tail_available"`
}

type SyncOptions struct {
	WorkspaceID      string
	Channels         []string
	ExcludeChannels  []string
	Since            string
	Full             bool
	LatestOnly       bool
	Concurrency      int
	AutoJoin         *bool
	enforceRetention bool
	ordinarySync     bool
}

type Client struct {
	bot          *slack.Client
	tokens       config.Tokens
	appToken     string
	apiURL       string
	httpClient   *http.Client
	dmPolicy     admission.DMPolicy
	sleep        func(context.Context, time.Duration) error
	now          func() time.Time
	socketModeFn func(*slack.Client) socketModeRunner
	logger       *slog.Logger
}

func New(tokens config.Tokens) *Client {
	return NewWithOptions(tokens, "", nil)
}

func NewWithOptions(tokens config.Tokens, apiURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	client := &Client{
		tokens:     tokens,
		appToken:   tokens.App,
		apiURL:     slack.APIURL,
		httpClient: httpClient,
		sleep:      sleepContext,
		now:        func() time.Time { return time.Now().UTC() },
	}
	if apiURL != "" {
		client.apiURL = apiURL
	}

	if tokens.Bot != "" {
		var options []slack.Option
		if apiURL != "" {
			options = append(options, slack.OptionAPIURL(apiURL))
		}
		options = append(options, slack.OptionHTTPClient(httpClient))
		if tokens.App != "" {
			options = append(options, slack.OptionAppLevelToken(tokens.App))
		}
		client.bot = slack.New(tokens.Bot, options...)
	}
	client.socketModeFn = func(api *slack.Client) socketModeRunner {
		return managedSocketMode{client: socketmode.New(api)}
	}
	return client
}

func (c *Client) WithDMPolicy(policy admission.DMPolicy) *Client {
	c.dmPolicy = policy
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
	if err := ctx.Err(); err != nil {
		return diag, err
	}
	if c.bot != nil {
		resp, err := c.authTest(ctx, c.tokens.Bot)
		if ctx.Err() != nil {
			return diag, ctx.Err()
		}
		if err != nil {
			return diag, err
		}
		workspaceID, err := authenticatedWorkspaceID(resp, "")
		if err != nil {
			return diag, err
		}
		diag.BotAuthTeamID = workspaceID
		diag.BotAuthTeam = resp.Team
		diag.AppTailAvailable = c.tokens.App != ""
	}

	if c.tokens.User != "" {
		userAuth, err := c.authTest(ctx, c.tokens.User)
		if ctx.Err() != nil {
			return diag, ctx.Err()
		}
		var workspaceID string
		if err == nil {
			workspaceID, err = authenticatedWorkspaceID(userAuth, diag.BotAuthTeamID)
		}
		if err == nil {
			diag.UserAuthAvailable = true
			diag.ThreadCoverage = "full"
			if c.dmPolicy.Enabled(c.tokens.User != "") {
				diag.DMsIncluded = true
				diag.DMsMissingScope, diag.DMProbeError, err = c.probeDMAccess(ctx, workspaceID)
				if err != nil {
					return diag, err
				}
			}
		} else {
			diag.UserAuthError = err.Error()
		}
	}
	return diag, ctx.Err()
}

func (c *Client) Sync(ctx context.Context, st *store.Store, opts SyncOptions) error {
	// Empty Since alone also describes Tail repair; only ordinary Sync owns
	// the durable retained-thread backlog.
	opts.ordinarySync = true
	// Selection is invocation-local: Tail and its repair path still own the bot.
	source := channelSyncSource{
		token: c.tokens.Bot, sourceName: SourceBot,
		sourceRank: 2, allowJoin: syncAutoJoin(opts),
	}
	if source.token == "" {
		source = channelSyncSource{
			token: c.tokens.User, sourceName: SourceUser, sourceRank: 1,
		}
	}
	if source.token == "" {
		return errors.New("SLACK_BOT_TOKEN or SLACK_USER_TOKEN is required for api sync")
	}

	auth, err := c.authTest(ctx, source.token)
	if err != nil {
		return err
	}
	workspaceID, err := authenticatedWorkspaceID(auth, opts.WorkspaceID)
	if err != nil {
		return err
	}
	userRepliesAvailable := source.sourceName == SourceUser
	if !userRepliesAvailable {
		userRepliesAvailable, err = c.userAuthAvailable(ctx, workspaceID)
		if err != nil {
			return err
		}
	}

	replies := threadSyncSource{}
	if userRepliesAvailable {
		replies = threadSyncSource{token: c.tokens.User, sourceName: SourceUser, sourceRank: 1}
	} else if c.tokens.User == "" {
		// Bot replies use the already authenticated workspace. A rejected
		// configured user credential must not silently change reply identity.
		replies = threadSyncSource{token: source.token, sourceName: source.sourceName, sourceRank: source.sourceRank}
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
	threadRepliesSkipped := newThreadSkipTracker()
	source.threadSkip = threadRepliesSkipped

	channels, err := c.fetchChannelsWithToken(ctx, source.token, workspaceID)
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
	if err := c.syncChannelsWithSource(ctx, st, workspaceID, selectedChannels, opts, now, replies, source); err != nil {
		return err
	}

	var (
		users             []slack.User
		userByID          map[string]slack.User
		dmCatalogComplete bool
	)
	if c.dmPolicy.Enabled(c.tokens.User != "") && userRepliesAvailable && c.tokens.User != "" {
		users, err = c.getUsers(ctx, source.token)
		if err != nil {
			return err
		}
		userByID = make(map[string]slack.User, len(users))
		for _, user := range users {
			userByID[user.ID] = user
		}

		dms, err := c.fetchDMs(ctx, workspaceID, threadRepliesSkipped)
		if err != nil {
			if isMissingScopeError(err) {
				threadRepliesSkipped.RecordOmission()
				if setErr := st.SetSyncState(ctx, SourceUser, "dms", workspaceID, "missing_scope"); setErr != nil {
					return setErr
				}
			} else {
				return err
			}
		} else {
			dmCatalogComplete = true
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
			if err := c.syncChannelsWithSource(ctx, st, workspaceID, selectedDMs, opts, now, replies, channelSyncSource{
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
		users, err = c.getUsers(ctx, source.token)
		if err != nil {
			return err
		}
	}
	for _, user := range users {
		if _, err := c.skipUserCollision(ctx, st, workspaceID, user, now); err != nil {
			return err
		}
	}

	publication := store.APIThreadCoveragePublication{
		FullEligible: userRepliesAvailable && !threadRepliesSkipped.Skipped() && !threadRepliesSkipped.Omitted(),
	}
	// Only an unrestricted scan of both catalogs can retire unknown legacy
	// skips. Scoped runs still clear individual successfully completed threads.
	if publication.FullEligible && opts.Full && opts.Since == "" && len(opts.Channels) == 0 && len(excluded) == 0 && dmCatalogComplete {
		publication.CleanupWorkspaceID = workspaceID
	}
	if err := st.PublishAPIThreadCoverage(ctx, publication); err != nil {
		return err
	}
	return st.SetSyncState(ctx, source.sourceName, "workspace", workspaceID, now.Format(time.RFC3339))
}

func (c *Client) fetchChannels(ctx context.Context, workspaceID string) ([]slack.Channel, error) {
	return c.fetchChannelsWithToken(ctx, c.tokens.Bot, workspaceID)
}

func (c *Client) fetchChannelsWithToken(ctx context.Context, token string, workspaceID string) ([]slack.Channel, error) {
	var (
		cursor   string
		channels []slack.Channel
		seen     = map[string]bool{}
	)
	for {
		page, nextCursor, err := c.getConversations(ctx, token, &slack.GetConversationsParameters{
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
		if seen[nextCursor] {
			return nil, errors.New("conversations.list repeated cursor")
		}
		seen[nextCursor] = true
		cursor = nextCursor
	}
}

func authenticatedWorkspaceID(auth *slack.AuthTestResponse, requested string) (string, error) {
	authTeamID := strings.TrimSpace(auth.TeamID)
	if authTeamID == "" {
		return "", errors.New("auth.test did not identify a workspace; use a workspace-scoped bot or user token")
	}
	requested = strings.TrimSpace(requested)
	if requested != "" {
		if authTeamID != requested {
			return "", fmt.Errorf("authenticated workspace %s does not match requested workspace %s", authTeamID, requested)
		}
		return requested, nil
	}
	return authTeamID, nil
}

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
