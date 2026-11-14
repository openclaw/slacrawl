package slackapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openclaw/crawlkit/progress"
	"github.com/slack-go/slack"

	"github.com/openclaw/slacrawl/internal/store"
)

func (c *Client) syncChannelMessagesWithSource(ctx context.Context, st *store.Store, workspaceID string, channel slack.Channel, opts store.APIHistoryOptions, now time.Time, replies threadSyncSource, source channelSyncSource) error {
	if source.token == "" {
		return errors.New("history token is required")
	}
	if source.sourceName == "" {
		source.sourceName = SourceBot
	}
	if source.sourceRank == 0 {
		source.sourceRank = 2
	}
	attempt, err := st.BeginAPIHistory(ctx, store.APIHistoryScope{
		SourceName: source.sourceName, WorkspaceID: workspaceID, ChannelID: channel.ID, Since: source.coverageScope,
	}, opts, fmt.Sprintf("%d.%06d", now.Unix(), now.Nanosecond()/1000))
	if err != nil {
		return err
	}
	oldest, horizon := attempt.Oldest, attempt.Latest
	enforceRetention, inclusive := attempt.EnforceRetention, attempt.Inclusive
	checkAttempt := func() error { return st.CheckAPIHistory(ctx, attempt) }
	historyIncomplete := false
	syncedThreads := map[string]struct{}{}
	pendingThreads := map[string]store.ThreadWork{}
	completedThreads := map[string]struct{}{}
	var discovery *store.ThreadWorkDiscovery
	if source.retainedThreads {
		discovery = &store.ThreadWorkDiscovery{SourceName: SourceUser, WorkspaceID: workspaceID, ChannelID: channel.ID, ExcludedTS: completedThreads}
		work, err := st.PrepareThreadWork(ctx, SourceUser, workspaceID, channel.ID, &attempt)
		if err != nil {
			return err
		}
		for _, item := range work {
			pendingThreads[item.TS] = item
		}
	}
	syncThreadOnce := func(threadTS string) error {
		if err := checkAttempt(); err != nil {
			return err
		}
		if _, ok := syncedThreads[threadTS]; ok {
			return nil
		}
		var work *store.ThreadWork
		if pending, ok := pendingThreads[threadTS]; ok {
			work = &pending
		}
		// Page hints may belong to another sync's job. Leave them unattempted
		// until this invocation admits work, which can happen on a later page.
		if source.retainedThreads && work == nil {
			return nil
		}
		threadKey := workspaceID + "|" + channel.ID + "|" + threadTS
		saveSkip := func(reason string) (bool, error) {
			result, err := st.ApplyWriteBatch(ctx, store.WriteBatch{
				HistoryGuard: &attempt,
				ThreadGuard:  work,
				SyncStates:   []store.SyncStateWrite{{SourceName: SourceUser, EntityType: "thread_skip", EntityID: threadKey, Value: reason}},
			})
			return !result.ThreadWorkRevoked, err
		}
		if source.threadSkip != nil {
			if reason, ok := source.threadSkip.SkipReason(channel.ID, threadSkipScope(channel)); ok {
				saved, err := saveSkip(reason)
				if saved && err == nil {
					syncedThreads[threadTS] = struct{}{}
				}
				return err
			}
		}
		outcome, err := c.syncThread(ctx, st, workspaceID, channel.ID, threadTS, enforceRetention, now, replies, work, source.threadSkip, &attempt)
		if outcome == threadSyncIncomplete {
			historyIncomplete = true
		}
		if outcome == threadSyncUnattempted {
			return err
		}
		syncedThreads[threadTS] = struct{}{}
		if err != nil {
			if work != nil && channelSkipReason(err) == "thread_not_found" {
				// An unavailable retained root does not imply a channel-wide access failure.
				source.threadSkip.RecordOmission()
				_, saveErr := saveSkip("thread_not_found")
				return saveErr
			}
			if isThreadRepliesSkipped(err) {
				saved, saveErr := saveSkip(channelSkipReason(err))
				if saveErr != nil {
					return saveErr
				}
				if saved && source.threadSkip != nil {
					source.threadSkip.Record(channel.ID, threadSkipScope(channel), channelSkipReason(err))
				}
				return nil
			}
			return err
		}
		if outcome == threadSyncRevoked || outcome == threadSyncIncomplete {
			return nil
		}
		if work != nil {
			completed, err := st.CompleteThreadWork(ctx, *work, threadKey, &attempt)
			if completed {
				completedThreads[threadTS] = struct{}{}
			}
			return err
		}
		_, err = st.ApplyWriteBatch(ctx, store.WriteBatch{HistoryGuard: &attempt, SyncStateDeletes: []store.SyncStateDelete{{SourceName: SourceUser, EntityType: "thread_skip", EntityID: threadKey}}})
		if err == nil {
			completedThreads[threadTS] = struct{}{}
		}
		return err
	}

	saveChannelState := func(entityType, value string) error {
		_, err := st.ApplyWriteBatch(ctx, store.WriteBatch{HistoryGuard: &attempt, SyncStates: []store.SyncStateWrite{
			{SourceName: source.sourceName, EntityType: entityType, EntityID: channel.ID, Value: value},
		}})
		return err
	}
	cursor := ""
	seen := map[string]bool{}
	joined := false
	historyLimited := false
	for {
		resp, err := c.getConversationHistory(ctx, source.token, &slack.GetConversationHistoryParameters{
			ChannelID: channel.ID,
			Cursor:    cursor,
			Limit:     200,
			Oldest:    oldest,
			Latest:    horizon,
			Inclusive: inclusive,
		}, checkAttempt)
		if err != nil {
			if source.skipMissingScope && isMissingScopeError(err) {
				source.threadSkip.RecordOmission()
				return saveChannelState("channel_skip", "missing_scope")
			}
			if source.allowJoin && !joined && channelSkipReason(err) == "not_in_channel" && !channel.IsPrivate {
				if err := checkAttempt(); err != nil {
					return err
				}
				joinErr := c.joinConversation(ctx, channel.ID)
				if err := checkAttempt(); err != nil {
					return err
				}
				if joinErr == nil {
					joined = true
					if setErr := saveChannelState("channel_join", "joined"); setErr != nil {
						return setErr
					}
					continue
				}
				if setErr := saveChannelState("channel_join", "failed:"+joinErr.Error()); setErr != nil {
					return setErr
				}
			}
			if isChannelHistorySkipped(err) {
				source.threadSkip.RecordOmission()
				return saveChannelState("channel_skip", channelSkipReason(err))
			}
			return fmt.Errorf("channel %s history: %w", channel.ID, err)
		}
		historyLimited = historyLimited || resp.IsLimited
		if err := validateMessagePage(resp.Messages, channel.ID); err != nil {
			return fmt.Errorf("channel %s history: %w", channel.ID, err)
		}
		batch := store.WriteBatch{HistoryGuard: &attempt, Messages: make([]store.MessageWrite, 0, len(resp.Messages)), ThreadDiscovery: discovery}
		threadTSs := make([]string, 0)
		queuedThreads := map[string]struct{}{}
		for _, rawMsg := range resp.Messages {
			msg := rawMsg.Message
			if msg.Channel == "" {
				msg.Channel = channel.ID
			}
			batch.Messages = append(batch.Messages, store.MessageWrite{
				Message:                toStoreMessage(workspaceID, msg, source.sourceName, source.sourceRank, rawMsg.RawPayload, now),
				Mentions:               toStoreMentions(msg),
				EnforceRetention:       enforceRetention,
				SkipWorkspaceCollision: true,
			})
			if source.retainedThreads && msg.ReplyCount > 0 {
				batch.PendingThreads = append(batch.PendingThreads, store.ThreadWork{SourceName: SourceUser, WorkspaceID: workspaceID, ChannelID: channel.ID, TS: msg.Timestamp})
			}
			if msg.ReplyCount > 0 && replies.token != "" {
				if _, synced := syncedThreads[msg.Timestamp]; !synced {
					if _, queued := queuedThreads[msg.Timestamp]; !queued {
						queuedThreads[msg.Timestamp] = struct{}{}
						threadTSs = append(threadTSs, msg.Timestamp)
					}
				}
			}
		}
		collidedTSs := map[string]struct{}{}
		{
			result, err := st.ApplyWriteBatch(ctx, batch)
			if err != nil {
				return err
			}
			historyIncomplete = historyIncomplete || len(result.CollisionsSkipped) > 0
			for _, skip := range result.CollisionsSkipped {
				source.threadSkip.RecordOmission()
				c.skipMessageCollision(skip.Err, workspaceID, skip.ChannelID, skip.TS)
				collidedTSs[skip.TS] = struct{}{}
			}
			for _, work := range result.PendingThreads {
				pendingThreads[work.TS] = work
			}
		}
		for _, threadTS := range threadTSs {
			// A collided parent belongs to another workspace; its thread does too.
			if _, collided := collidedTSs[threadTS]; collided {
				continue
			}
			if err := syncThreadOnce(threadTS); err != nil {
				return err
			}
		}
		if resp.NextCursor == "" {
			if resp.HasMore {
				return errors.New("conversations.history returned has_more without a continuation cursor; scan remains incomplete; slacrawl does not support timestamp pagination")
			}
			break
		}
		if seen[resp.NextCursor] {
			return errors.New("conversations.history repeated cursor")
		}
		seen[resp.NextCursor] = true
		cursor = resp.NextCursor
	}
	// A later accessible page cannot certify an earlier limited response.
	// Keep the old completed horizon and pending interval for a retry.
	if historyLimited {
		return errors.New("Slack reported a history/message limit; completeness of the requested interval is uncertified; review workspace history availability")
	}
	if source.retainedThreads {
		roots, err := st.ChannelThreadRoots(ctx, workspaceID, channel.ID)
		if err != nil {
			return err
		}
		batch := store.WriteBatch{HistoryGuard: &attempt, ThreadDiscovery: discovery}
		for _, root := range roots {
			batch.PendingThreads = append(batch.PendingThreads, store.ThreadWork{SourceName: SourceUser, WorkspaceID: workspaceID, ChannelID: channel.ID, TS: root.TS})
		}
		if len(batch.PendingThreads) > 0 {
			result, err := st.ApplyWriteBatch(ctx, batch)
			if err != nil {
				return err
			}
			for _, work := range result.PendingThreads {
				pendingThreads[work.TS] = work
			}
		}
		if replies.token != "" {
			threads := make([]string, 0, len(pendingThreads))
			for ts := range pendingThreads {
				threads = append(threads, ts)
			}
			sort.Strings(threads)
			for _, ts := range threads {
				if err := syncThreadOnce(ts); err != nil {
					return err
				}
			}
		}
	}
	// Keep the exact attempted interval and its prior completed horizon when
	// any page omitted a collision, without vetoing another channel's progress.
	if historyIncomplete {
		return st.CheckAPIHistory(ctx, attempt)
	}
	return st.CompleteAPIHistory(ctx, attempt)
}

type threadSyncResult uint8

const (
	threadSyncComplete threadSyncResult = iota
	threadSyncRevoked
	threadSyncUnattempted
	threadSyncIncomplete
)

func (c *Client) syncThread(ctx context.Context, st *store.Store, workspaceID string, channelID string, threadTS string, enforceRetention bool, now time.Time, source threadSyncSource, work *store.ThreadWork, skips *threadSkipTracker, history *store.APIHistoryAttempt) (threadSyncResult, error) {
	cursor := ""
	seen := map[string]bool{}
	// Rejection before the first request must leave a later page free to
	// process newly admitted work; revocation after a request still counts.
	revokedResult := threadSyncUnattempted
	outcome := threadSyncComplete
	revokedOutcome := func(fallback threadSyncResult) threadSyncResult {
		if outcome == threadSyncIncomplete {
			return outcome
		}
		return fallback
	}
	checkAttempt := func() error {
		if history != nil {
			return st.CheckAPIHistory(ctx, *history)
		}
		return ctx.Err()
	}
	for {
		if err := checkAttempt(); err != nil {
			return revokedOutcome(revokedResult), err
		}
		if work != nil {
			current, err := st.ThreadWorkCurrent(ctx, *work)
			if err != nil {
				return revokedOutcome(revokedResult), err
			}
			if !current {
				return revokedOutcome(revokedResult), nil
			}
		}
		revokedResult = threadSyncRevoked
		resp, err := c.getConversationReplies(ctx, source.token, &slack.GetConversationRepliesParameters{
			ChannelID: channelID,
			Timestamp: threadTS,
			Cursor:    cursor,
			Limit:     200,
		}, checkAttempt)
		if err := checkAttempt(); err != nil {
			return revokedOutcome(revokedResult), err
		}
		if work != nil {
			current, checkErr := st.ThreadWorkCurrent(ctx, *work)
			if checkErr != nil {
				return outcome, checkErr
			}
			if !current {
				return revokedOutcome(threadSyncRevoked), nil
			}
		}
		if err != nil {
			return outcome, err
		}
		if err := validateMessagePage(resp.Messages, channelID); err != nil {
			return outcome, fmt.Errorf("channel %s replies: %w", channelID, err)
		}
		batch := store.WriteBatch{HistoryGuard: history, Messages: make([]store.MessageWrite, 0, len(resp.Messages)), ThreadGuard: work}
		if work == nil {
			batch.PendingThreadOnCollision = &store.ThreadWork{SourceName: SourceUser, WorkspaceID: workspaceID, ChannelID: channelID, TS: threadTS}
		}
		for _, rawMsg := range resp.Messages {
			msg := rawMsg.Message
			if msg.Channel == "" {
				msg.Channel = channelID
			}
			batch.Messages = append(batch.Messages, store.MessageWrite{
				Message:                toStoreMessage(workspaceID, msg, source.sourceName, source.sourceRank, rawMsg.RawPayload, now),
				Mentions:               toStoreMentions(msg),
				EnforceRetention:       enforceRetention,
				SkipWorkspaceCollision: true,
			})
		}
		// Empty guarded pages also establish whether this generation still owns
		// the response before cursor traversal or completion.
		if len(batch.Messages) > 0 || work != nil || history != nil {
			result, err := st.ApplyWriteBatch(ctx, batch)
			if err != nil {
				return outcome, err
			}
			if result.ThreadWorkRevoked {
				return revokedOutcome(threadSyncRevoked), nil
			}
			if len(result.CollisionsSkipped) > 0 {
				outcome = threadSyncIncomplete
			}
			for _, skip := range result.CollisionsSkipped {
				skips.RecordOmission()
				c.skipMessageCollision(skip.Err, workspaceID, skip.ChannelID, skip.TS)
			}
		}
		if resp.NextCursor == "" {
			if resp.HasMore {
				return outcome, errors.New("conversations.replies returned has_more without a continuation cursor; scan remains incomplete; slacrawl does not support timestamp pagination")
			}
			return outcome, nil
		}
		if seen[resp.NextCursor] {
			return outcome, errors.New("conversations.replies repeated cursor")
		}
		seen[resp.NextCursor] = true
		cursor = resp.NextCursor
	}
}

type threadSyncSource struct {
	token      string
	sourceName string
	sourceRank int
}

type channelSyncSource struct {
	token            string
	sourceName       string
	sourceRank       int
	allowJoin        bool
	skipMissingScope bool
	threadSkip       *threadSkipTracker
	coverageScope    string
	retainedThreads  bool
}

func (c *Client) syncChannels(ctx context.Context, st *store.Store, workspaceID string, channels []slack.Channel, opts SyncOptions, now time.Time, userRepliesAvailable bool, threadSkip *threadSkipTracker) error {
	// Tail repair keeps its existing user-token reply capability.
	replies := threadSyncSource{}
	if userRepliesAvailable {
		replies = threadSyncSource{token: c.tokens.User, sourceName: SourceUser, sourceRank: 1}
	}
	return c.syncChannelsWithSource(ctx, st, workspaceID, channels, opts, now, replies, channelSyncSource{
		token:      c.tokens.Bot,
		sourceName: SourceBot,
		sourceRank: 2,
		allowJoin:  syncAutoJoin(opts),
		threadSkip: threadSkip,
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

// skipChannelCollision upserts the channel and reports whether it must be
// skipped because another workspace already owns it. Slack Connect shared
// channels legitimately appear under multiple workspaces, so a collision must
// not abort the whole sync — the workspace that recorded the channel first
// keeps it, and the channel is skipped here with a warning. Any other upsert
// error is returned for the caller's fatal path.
func (c *Client) skipChannelCollision(ctx context.Context, st *store.Store, workspaceID string, channel slack.Channel, now time.Time) (bool, error) {
	err := st.UpsertChannel(ctx, toStoreChannel(workspaceID, channel, now))
	if err == nil {
		return false, nil
	}
	if store.IsWorkspaceCollision(err, "channel") {
		c.warnLogger().Warn("skipping channel owned by another workspace",
			"workspace_id", workspaceID,
			"channel_id", channel.ID,
			"channel_name", channel.Name,
			"err", err,
		)
		return true, nil
	}
	return false, err
}

// skipUserCollision upserts the user and reports whether it must be skipped
// because another workspace already owns it. Enterprise Grid shares user IDs
// org-wide and Slack Connect surfaces external members, so the same user can be
// listed by several workspaces. The workspace that recorded the user first
// keeps it, and later workspaces skip with a warning instead of aborting.
func (c *Client) skipUserCollision(ctx context.Context, st *store.Store, workspaceID string, user slack.User, now time.Time) (bool, error) {
	err := st.UpsertUser(ctx, ToStoreUser(workspaceID, user, now))
	if err == nil {
		return false, nil
	}
	if store.IsWorkspaceCollision(err, "user") {
		c.warnLogger().Warn("skipping user owned by another workspace",
			"workspace_id", workspaceID,
			"user_id", user.ID,
			"user_name", user.Name,
			"err", err,
		)
		return true, nil
	}
	return false, err
}

// skipMessageCollision reports whether a message upsert error is a
// cross-workspace collision the caller should skip rather than propagate.
// A Slack Connect channel is owned by whichever workspace recorded it first, so
// its messages keep arriving for every other member workspace — during history
// sync that must not abort the run, and during tail it must not kill the
// socket-mode loop.
func (c *Client) skipMessageCollision(err error, workspaceID, channelID, ts string) bool {
	if !store.IsWorkspaceCollision(err, "message") {
		return false
	}
	c.warnLogger().Warn("skipping message owned by another workspace",
		"workspace_id", workspaceID,
		"channel_id", channelID,
		"ts", ts,
		"err", err,
	)
	return true
}

func (c *Client) warnLogger() *slog.Logger {
	if c.logger != nil {
		return c.logger
	}
	return slog.Default()
}

func (c *Client) syncChannelsWithSource(ctx context.Context, st *store.Store, workspaceID string, channels []slack.Channel, opts SyncOptions, now time.Time, replies threadSyncSource, source channelSyncSource) error {
	if len(channels) == 0 {
		return nil
	}
	channels, err := c.admitChannels(workspaceID, channels, source.threadSkip)
	if err != nil || len(channels) == 0 {
		return err
	}
	source.coverageScope = opts.Since
	source.retainedThreads = opts.ordinarySync && opts.Since == ""
	channels, historyOptions, err := c.channelSyncPlan(ctx, st, workspaceID, channels, opts)
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
			skip, err := c.skipChannelCollision(ctx, st, workspaceID, channel, now)
			if err != nil {
				tracker.Finish(err)
				return err
			}
			if skip {
				source.threadSkip.RecordOmission()
				continue
			}
			if err := c.syncChannelMessagesWithSource(ctx, st, workspaceID, channel, historyOptions, now, replies, source); err != nil {
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
			skip, err := c.skipChannelCollision(ctx, st, workspaceID, channel, now)
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				cancel()
				return
			}
			if skip {
				source.threadSkip.RecordOmission()
				continue
			}
			if err := c.syncChannelMessagesWithSource(ctx, st, workspaceID, channel, historyOptions, now, replies, source); err != nil {
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
				if ctx.Err() != nil {
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
		if ctx.Err() != nil {
			tracker.Finish(ctx.Err())
			return ctx.Err()
		}
		tracker.Finish(nil)
		return nil
	}
}

func (c *Client) channelSyncPlan(ctx context.Context, st *store.Store, workspaceID string, channels []slack.Channel, opts SyncOptions) ([]slack.Channel, store.APIHistoryOptions, error) {
	options := store.APIHistoryOptions{Full: opts.Full, RestoreRequested: !opts.enforceRetention && (opts.Since != "" || opts.Full), RequireComplete: opts.Incremental}
	if opts.Since != "" || opts.Full || !opts.LatestOnly {
		return channels, options, nil
	}
	cursors, err := st.ChannelSyncCursors(ctx, workspaceID)
	if err != nil {
		return nil, options, err
	}
	latestByChannel := make(map[string]store.ChannelSyncCursor, len(cursors))
	for _, cursor := range cursors {
		latestByChannel[cursor.ID] = cursor
	}
	selected := make([]slack.Channel, 0, len(channels))
	for _, channel := range channels {
		cursor, ok := latestByChannel[channel.ID]
		if !ok {
			seeded, err := st.ChannelRetentionSeeded(ctx, workspaceID, channel.ID)
			if err != nil {
				return nil, options, err
			}
			cursor.RetentionSeeded = seeded
		}
		if cursor.LatestTS != "" || cursor.RetentionSeeded {
			selected = append(selected, channel)
		}
	}
	return selected, options, nil
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
	omitted  bool
}

func newThreadSkipTracker() *threadSkipTracker {
	return &threadSkipTracker{
		scopes:   make(map[string]string),
		channels: make(map[string]string),
	}
}

// Omitted work vetoes full coverage without entering the replies skip cache.
// Workers share this monotonic fact; later successful pages cannot clear it.
func (t *threadSkipTracker) RecordOmission() {
	if t != nil {
		t.mu.Lock()
		t.omitted = true
		t.mu.Unlock()
	}
}

func (t *threadSkipTracker) Omitted() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.omitted
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

// Return the original code only for exact machine comparisons. Diagnostics
// must use err.Error() so arbitrary provider text cannot bypass redaction.
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

func (c *Client) probeDMAccess(ctx context.Context, workspaceID string) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if !c.dmPolicy.Enabled(c.tokens.User != "") || c.tokens.User == "" {
		return "", "", nil
	}
	missing := make(map[string]struct{})
	addMissing := func(scope string) {
		if scope == "" {
			return
		}
		missing[scope] = struct{}{}
	}

	dms, err := c.fetchDMs(ctx, workspaceID, nil)
	if ctx.Err() != nil {
		return "", "", ctx.Err()
	}
	if err != nil {
		if isMissingScopeError(err) {
			addMissing("im:read")
			addMissing("mpim:read")
			return joinScopes(missing), "", nil
		}
		return "", "catalog_failed", nil
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

	// Probe one available conversation per kind. A later successful sample must
	// not erase a failure or missing scope from the other kind.
	failure := ""
	for _, sample := range []struct{ channelID, scope string }{
		{sampleIM, "im:history"}, {sampleMPIM, "mpim:history"},
	} {
		if sample.channelID == "" {
			continue
		}
		_, historyErr := c.getConversationHistory(ctx, c.tokens.User, &slack.GetConversationHistoryParameters{
			ChannelID: sample.channelID,
			Limit:     1,
		}, nil)
		if ctx.Err() != nil {
			return joinScopes(missing), failure, ctx.Err()
		}
		if isMissingScopeError(historyErr) {
			addMissing(sample.scope)
		} else if historyErr != nil {
			failure = "history_failed"
		}
	}
	return joinScopes(missing), failure, ctx.Err()
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

func (c *Client) userAuthAvailable(ctx context.Context, workspaceID string) (bool, error) {
	if c.tokens.User == "" {
		return false, nil
	}
	auth, err := c.authTest(ctx, c.tokens.User)
	if err != nil {
		return false, nil
	}
	// Invalid optional auth keeps bot-only coverage; valid auth from another
	// workspace must stop before its replies can be stored under this workspace.
	if _, err := authenticatedWorkspaceID(auth, workspaceID); err != nil {
		return false, fmt.Errorf("user token: %w", err)
	}
	return true, nil
}
