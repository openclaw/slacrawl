package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/search"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/slack-go/slack"
)

const ProtocolVersion = "slacrawl-provider-v1"

const maxStoreBatchRecords = 1_000

type Options struct {
	WorkspaceID     string
	Channels        []string
	ExcludeChannels []string
	Since           string
	Full            bool
	LatestOnly      bool
	Limit           int
	Logger          *slog.Logger
}

type Summary struct {
	Name                     string `json:"name"`
	Provider                 string `json:"provider,omitempty"`
	WorkspaceID              string `json:"workspace_id"`
	Records                  int64  `json:"records"`
	Workspaces               int64  `json:"workspaces"`
	Channels                 int64  `json:"channels"`
	Users                    int64  `json:"users"`
	Messages                 int64  `json:"messages"`
	MessagesWritten          int64  `json:"messages_written"`
	Checkpoints              int64  `json:"checkpoints"`
	SkippedUsers             int64  `json:"skipped_users,omitempty"`
	SkippedChannels          int64  `json:"skipped_channels,omitempty"`
	SkippedBadMessageID      int64  `json:"skipped_bad_message_id,omitempty"`
	SkippedDuplicateIdentity int64  `json:"skipped_duplicate_identity,omitempty"`
	SkippedMissingChannel    int64  `json:"skipped_missing_channel,omitempty"`
	SkippedMissingSortKey    int64  `json:"skipped_missing_timestamp_sort_key,omitempty"`
	RoundedThreadMessages    int64  `json:"rounded_thread_messages,omitempty"`
	RoundedThreads           int64  `json:"rounded_threads,omitempty"`
	UnresolvedThreadMessages int64  `json:"unresolved_thread_messages,omitempty"`
	UnresolvedThreads        int64  `json:"unresolved_threads,omitempty"`
}

type request struct {
	Type            string   `json:"type"`
	Protocol        string   `json:"protocol"`
	WorkspaceID     string   `json:"workspace_id"`
	Since           string   `json:"since,omitempty"`
	Full            bool     `json:"full,omitempty"`
	LatestOnly      bool     `json:"latest_only,omitempty"`
	Channels        []string `json:"channels,omitempty"`
	ExcludeChannels []string `json:"exclude_channels,omitempty"`
	Checkpoint      string   `json:"checkpoint,omitempty"`
	BatchSize       int      `json:"batch_size"`
	Limit           *int     `json:"limit,omitempty"`
}

type record struct {
	Type         string          `json:"type"`
	Protocol     string          `json:"protocol"`
	Provider     string          `json:"provider"`
	WorkspaceID  string          `json:"workspace_id"`
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Domain       string          `json:"domain"`
	EnterpriseID string          `json:"enterprise_id"`
	RealName     string          `json:"real_name"`
	DisplayName  string          `json:"display_name"`
	Title        string          `json:"title"`
	Kind         string          `json:"kind"`
	Topic        string          `json:"topic"`
	Purpose      string          `json:"purpose"`
	IsPrivate    bool            `json:"is_private"`
	IsArchived   bool            `json:"is_archived"`
	IsShared     bool            `json:"is_shared"`
	IsGeneral    bool            `json:"is_general"`
	IsBot        bool            `json:"is_bot"`
	IsDeleted    bool            `json:"is_deleted"`
	ChannelID    string          `json:"channel_id"`
	TS           string          `json:"ts"`
	UserID       string          `json:"user_id"`
	ThreadTS     string          `json:"thread_ts"`
	Text         string          `json:"text"`
	RawJSON      json.RawMessage `json:"raw_json"`
	EntityType   string          `json:"entity_type"`
	EntityID     string          `json:"entity_id"`
	Value        string          `json:"value"`
	Records      int64           `json:"records"`

	SkippedUsers             int64 `json:"skipped_users"`
	SkippedChannels          int64 `json:"skipped_channels"`
	SkippedBadMessageID      int64 `json:"skipped_bad_message_id"`
	SkippedDuplicateIdentity int64 `json:"skipped_duplicate_identity"`
	SkippedMissingChannel    int64 `json:"skipped_missing_channel"`
	SkippedMissingSortKey    int64 `json:"skipped_missing_timestamp_sort_key"`
	RoundedThreadMessages    int64 `json:"rounded_thread_messages"`
	RoundedThreads           int64 `json:"rounded_threads"`
	UnresolvedThreadMessages int64 `json:"unresolved_thread_messages"`
	UnresolvedThreads        int64 `json:"unresolved_threads"`
}

func SourceName(name string) string {
	return "provider:" + strings.ToLower(strings.TrimSpace(name))
}

func Sync(ctx context.Context, st *store.Store, providerConfig config.Provider, opts Options) (Summary, error) {
	workspaceID := strings.TrimSpace(opts.WorkspaceID)
	if workspaceID == "" {
		return Summary{}, errors.New("workspace ID is required for provider sync")
	}
	if opts.Limit < 0 {
		return Summary{}, errors.New("provider limit cannot be negative")
	}
	if providerConfig.SourceRank <= 2 {
		return Summary{}, errors.New("provider source rank must be greater than 2")
	}
	sourceName := SourceName(providerConfig.Name)
	checkpointKey := stateKey(workspaceID, opts)
	checkpoint, err := st.GetSyncState(ctx, sourceName, checkpointKey.entityType, checkpointKey.entityID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Summary{}, fmt.Errorf("read provider checkpoint: %w", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		checkpoint = ""
	}
	providerRequest := request{
		Type:            "request",
		Protocol:        ProtocolVersion,
		WorkspaceID:     workspaceID,
		Since:           strings.TrimSpace(opts.Since),
		Full:            opts.Full,
		LatestOnly:      opts.LatestOnly,
		Channels:        opts.Channels,
		ExcludeChannels: opts.ExcludeChannels,
		Checkpoint:      checkpoint,
		BatchSize:       providerConfig.BatchSize,
	}
	if opts.Limit > 0 {
		providerRequest.Limit = &opts.Limit
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(runCtx, providerConfig.Command, providerConfig.Args...)
	cmd.Env = providerEnvironment(providerConfig.EnvAllowlist)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Summary{}, fmt.Errorf("open provider stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return Summary{}, fmt.Errorf("open provider stdout: %w", err)
	}
	var stderr cappedBuffer
	stderr.limit = 64 << 10
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return Summary{}, fmt.Errorf("start provider %q: %w", providerConfig.Name, err)
	}
	if err := json.NewEncoder(stdin).Encode(providerRequest); err != nil {
		_ = stdin.Close()
		cancel()
		_ = cmd.Wait()
		return Summary{}, fmt.Errorf("write provider request: %w", err)
	}
	if err := stdin.Close(); err != nil {
		cancel()
		_ = cmd.Wait()
		return Summary{}, fmt.Errorf("close provider request: %w", err)
	}

	consumer := consumer{
		store:         st,
		config:        providerConfig,
		opts:          opts,
		sourceName:    sourceName,
		workspaceID:   workspaceID,
		checkpointKey: checkpointKey,
		now:           time.Now().UTC(),
		validChannels: make(map[string]struct{}),
		validUsers:    make(map[string]struct{}),
		retention:     make(map[string]bool),
		summary: Summary{
			Name:        providerConfig.Name,
			WorkspaceID: workspaceID,
		},
	}
	consumeErr := consumer.consume(ctx, stdout)
	if consumeErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if consumeErr != nil {
		return consumer.summary, withStderr(fmt.Errorf("consume provider %q: %w", providerConfig.Name, consumeErr), stderr.String())
	}
	if waitErr != nil {
		return consumer.summary, withStderr(fmt.Errorf("provider %q exited: %w", providerConfig.Name, waitErr), stderr.String())
	}
	if !consumer.done {
		return consumer.summary, withStderr(fmt.Errorf("provider %q ended without done record", providerConfig.Name), stderr.String())
	}
	if err := consumer.promoteCompletedFullCheckpoint(ctx); err != nil {
		return consumer.summary, err
	}
	return consumer.summary, nil
}

type consumer struct {
	store          *store.Store
	config         config.Provider
	opts           Options
	sourceName     string
	workspaceID    string
	checkpointKey  checkpointStateKey
	now            time.Time
	summary        Summary
	hello          bool
	done           bool
	counted        int64
	lastCheckpoint string
	validChannels  map[string]struct{}
	validUsers     map[string]struct{}
	retention      map[string]bool
	batch          store.WriteBatch
	seenMessages   int64
}

func (c *consumer) consume(ctx context.Context, input io.Reader) error {
	decoder := json.NewDecoder(input)
	for {
		var item record
		if err := decoder.Decode(&item); errors.Is(err, io.EOF) {
			return c.flush(ctx)
		} else if err != nil {
			return fmt.Errorf("decode provider record: %w", err)
		}
		if c.done {
			return errors.New("provider emitted a record after done")
		}
		if !c.hello && item.Type != "hello" {
			return fmt.Errorf("provider first record must be hello, got %q", item.Type)
		}
		switch item.Type {
		case "hello":
			if c.hello {
				return errors.New("provider emitted duplicate hello")
			}
			if item.Protocol != ProtocolVersion {
				return fmt.Errorf("provider protocol mismatch: expected %q, got %q", ProtocolVersion, item.Protocol)
			}
			c.hello = true
			c.summary.Provider = item.Provider
		case "workspace":
			if item.ID != c.workspaceID {
				return fmt.Errorf("provider workspace record %q does not match requested workspace %q", item.ID, c.workspaceID)
			}
			name := strings.TrimSpace(item.Name)
			if name == "" {
				name = item.ID
			}
			c.batch.Workspaces = append(c.batch.Workspaces, store.Workspace{
				ID: item.ID, Name: name, Domain: item.Domain, EnterpriseID: item.EnterpriseID,
				RawJSON: rawJSON(item.RawJSON), UpdatedAt: c.now,
			})
			c.counted++
			if err := c.flushIfFull(ctx); err != nil {
				return err
			}
		case "channel":
			if err := c.validateWorkspace(item.WorkspaceID, "channel", item.ID); err != nil {
				return err
			}
			name := strings.TrimSpace(item.Name)
			if name == "" {
				name = item.ID
			}
			kind := strings.TrimSpace(item.Kind)
			if kind == "" {
				kind = "provider_channel"
			}
			c.batch.Channels = append(c.batch.Channels, store.Channel{
				ID: item.ID, WorkspaceID: item.WorkspaceID, Name: name, Kind: kind,
				Topic: item.Topic, Purpose: item.Purpose, IsPrivate: item.IsPrivate,
				IsArchived: item.IsArchived, IsShared: item.IsShared, IsGeneral: item.IsGeneral,
				RawJSON: rawJSON(item.RawJSON), UpdatedAt: c.now,
			})
			c.validChannels[item.ID] = struct{}{}
			c.counted++
			if err := c.flushIfFull(ctx); err != nil {
				return err
			}
		case "user":
			if err := c.validateWorkspace(item.WorkspaceID, "user", item.ID); err != nil {
				return err
			}
			name := strings.TrimSpace(item.Name)
			if name == "" {
				name = item.ID
			}
			c.batch.Users = append(c.batch.Users, store.User{
				ID: item.ID, WorkspaceID: item.WorkspaceID, Name: name, RealName: item.RealName,
				DisplayName: item.DisplayName, Title: item.Title, IsBot: item.IsBot,
				IsDeleted: item.IsDeleted, RawJSON: rawJSON(item.RawJSON), UpdatedAt: c.now,
			})
			c.validUsers[item.ID] = struct{}{}
			c.counted++
			if err := c.flushIfFull(ctx); err != nil {
				return err
			}
		case "message":
			if c.opts.Limit > 0 && c.seenMessages >= int64(c.opts.Limit) {
				if err := c.flush(ctx); err != nil {
					return err
				}
				return fmt.Errorf("provider emitted more than requested message limit %d", c.opts.Limit)
			}
			if err := c.applyMessage(ctx, item); err != nil {
				return err
			}
			c.seenMessages++
			c.counted++
			if err := c.flushIfFull(ctx); err != nil {
				return err
			}
			if c.opts.Logger != nil && c.seenMessages%100_000 == 0 {
				c.opts.Logger.Info("provider sync progress", "provider", c.config.Name, "messages", c.seenMessages)
			}
		case "checkpoint":
			if item.EntityType != "workspace" || item.EntityID != c.workspaceID {
				return fmt.Errorf("provider checkpoint must target workspace %q", c.workspaceID)
			}
			if strings.TrimSpace(item.Value) == "" {
				return errors.New("provider checkpoint value is required")
			}
			c.batch.SyncStates = append(c.batch.SyncStates, store.SyncStateWrite{
				SourceName: c.sourceName, EntityType: c.checkpointKey.entityType,
				EntityID: c.checkpointKey.entityID, Value: item.Value,
			})
			if err := c.flush(ctx); err != nil {
				return fmt.Errorf("apply provider checkpoint: %w", err)
			}
		case "done":
			if item.Records != c.counted {
				return fmt.Errorf("provider record count mismatch: done reported %d, consumed %d", item.Records, c.counted)
			}
			if err := c.flush(ctx); err != nil {
				return fmt.Errorf("flush provider records: %w", err)
			}
			c.summary.Records = item.Records
			c.summary.SkippedUsers = item.SkippedUsers
			c.summary.SkippedChannels = item.SkippedChannels
			c.summary.SkippedBadMessageID = item.SkippedBadMessageID
			c.summary.SkippedDuplicateIdentity = item.SkippedDuplicateIdentity
			c.summary.SkippedMissingChannel = item.SkippedMissingChannel
			c.summary.SkippedMissingSortKey = item.SkippedMissingSortKey
			c.summary.RoundedThreadMessages = item.RoundedThreadMessages
			c.summary.RoundedThreads = item.RoundedThreads
			c.summary.UnresolvedThreadMessages = item.UnresolvedThreadMessages
			c.summary.UnresolvedThreads = item.UnresolvedThreads
			c.done = true
		default:
			return fmt.Errorf("unsupported provider record type %q", item.Type)
		}
	}
}

type checkpointStateKey struct {
	entityType string
	entityID   string
}

func stateKey(workspaceID string, opts Options) checkpointStateKey {
	scope := struct {
		Since           string   `json:"since"`
		Full            bool     `json:"full"`
		LatestOnly      bool     `json:"latest_only"`
		Channels        []string `json:"channels"`
		ExcludeChannels []string `json:"exclude_channels"`
		Limit           int      `json:"limit"`
	}{
		Since: strings.TrimSpace(opts.Since), Full: opts.Full, LatestOnly: opts.LatestOnly,
		Channels:        normalizedScopeValues(opts.Channels),
		ExcludeChannels: normalizedScopeValues(opts.ExcludeChannels),
		Limit:           opts.Limit,
	}
	if scope.Since == "" && !scope.Full && !scope.LatestOnly && len(scope.Channels) == 0 && len(scope.ExcludeChannels) == 0 && scope.Limit == 0 {
		return checkpointStateKey{entityType: "workspace", entityID: workspaceID}
	}
	raw, _ := json.Marshal(scope)
	digest := sha256.Sum256(raw)
	return checkpointStateKey{
		entityType: "workspace_scope",
		entityID:   fmt.Sprintf("%s|%x", workspaceID, digest[:16]),
	}
}

func normalizedScopeValues(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (c *consumer) promoteCompletedFullCheckpoint(ctx context.Context) error {
	if !c.opts.Full || c.opts.Limit > 0 || c.lastCheckpoint == "" {
		return nil
	}
	incremental := c.opts
	incremental.Full = false
	key := stateKey(c.workspaceID, incremental)
	if err := c.store.SetSyncState(ctx, c.sourceName, key.entityType, key.entityID, c.lastCheckpoint); err != nil {
		return fmt.Errorf("promote completed full provider checkpoint: %w", err)
	}
	return nil
}

func (c *consumer) validateWorkspace(workspaceID, entity, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("provider %s id is required", entity)
	}
	if workspaceID != c.workspaceID {
		return fmt.Errorf("provider %s %q belongs to workspace %q, not requested workspace %q", entity, id, workspaceID, c.workspaceID)
	}
	return nil
}

func (c *consumer) applyMessage(ctx context.Context, item record) error {
	if strings.TrimSpace(item.ChannelID) == "" || strings.TrimSpace(item.TS) == "" {
		return errors.New("provider message channel_id and ts are required")
	}
	if item.WorkspaceID != c.workspaceID {
		return fmt.Errorf("provider message %q belongs to workspace %q, not requested workspace %q", item.ChannelID+"|"+item.TS, item.WorkspaceID, c.workspaceID)
	}
	if err := c.validateMessageReferences(ctx, item.ChannelID, item.UserID); err != nil {
		return err
	}
	threadTS := strings.TrimSpace(item.ThreadTS)
	if threadTS == item.TS {
		threadTS = ""
	}
	slackMessage := slack.Message{Msg: slack.Msg{
		Channel: item.ChannelID, Timestamp: item.TS, ThreadTimestamp: threadTS,
		User: item.UserID, Text: item.Text,
	}}
	mentions := search.ExtractMentions(item.Text)
	storedMentions := make([]store.Mention, 0, len(mentions))
	for _, mention := range mentions {
		storedMentions = append(storedMentions, store.Mention{
			Type: mention.Type, TargetID: mention.TargetID, DisplayText: mention.DisplayText,
		})
	}
	message := store.Message{
		ChannelID: item.ChannelID, TS: item.TS, WorkspaceID: item.WorkspaceID,
		UserID: item.UserID, ThreadTS: threadTS, Text: item.Text,
		NormalizedText: search.NormalizeMessage(slackMessage), SourceRank: c.config.SourceRank,
		SourceName: c.sourceName, RawJSON: rawJSON(item.RawJSON), UpdatedAt: c.now,
		Files: nil,
	}
	enforceRetention, err := c.enforceRetention(ctx, item.ChannelID)
	if err != nil {
		return fmt.Errorf("read provider message retention for %s: %w", item.ChannelID, err)
	}
	c.batch.Messages = append(c.batch.Messages, store.MessageWrite{
		Message: message, Mentions: storedMentions,
		PreserveHigherPriority: true, EnforceRetention: enforceRetention, SkipUnchangedProviderRow: true,
	})
	return nil
}

func (c *consumer) validateMessageReferences(ctx context.Context, channelID, userID string) error {
	if _, ok := c.validChannels[channelID]; !ok {
		workspaceID, err := c.store.ChannelWorkspaceID(ctx, channelID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("provider message references missing channel %q", channelID)
		}
		if err != nil {
			return fmt.Errorf("read provider message channel %q: %w", channelID, err)
		}
		if workspaceID != c.workspaceID {
			return fmt.Errorf("provider message channel %q belongs to workspace %q, not requested workspace %q", channelID, workspaceID, c.workspaceID)
		}
		c.validChannels[channelID] = struct{}{}
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil
	}
	if _, ok := c.validUsers[userID]; ok {
		return nil
	}
	workspaceID, err := c.store.UserWorkspaceID(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		// Reserve the global Slack ID in this workspace. A later real catalog record
		// replaces this marker without letting another workspace claim the author.
		c.batch.Users = append(c.batch.Users, store.User{
			ID: userID, WorkspaceID: c.workspaceID, Name: userID,
			RawJSON: store.PlaceholderUserRawJSON, UpdatedAt: c.now,
		})
		c.validUsers[userID] = struct{}{}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read provider message user %q: %w", userID, err)
	}
	if workspaceID != c.workspaceID {
		return fmt.Errorf("provider message user %q belongs to workspace %q, not requested workspace %q", userID, workspaceID, c.workspaceID)
	}
	c.validUsers[userID] = struct{}{}
	return nil
}

func (c *consumer) enforceRetention(ctx context.Context, channelID string) (bool, error) {
	if c.opts.Full {
		return false, nil
	}
	if strings.TrimSpace(c.opts.Since) == "" {
		return true, nil
	}
	if enforce, ok := c.retention[channelID]; ok {
		return enforce, nil
	}
	floor, err := c.store.ChannelRetentionFloor(ctx, c.workspaceID, channelID)
	if err != nil {
		return false, err
	}
	enforce := store.ShouldEnforceRetention(c.opts.Since, floor, true)
	c.retention[channelID] = enforce
	return enforce, nil
}

func (c *consumer) pendingRecords() int {
	return len(c.batch.Workspaces) + len(c.batch.Channels) + len(c.batch.Users) + len(c.batch.Messages)
}

func (c *consumer) flushIfFull(ctx context.Context) error {
	if c.pendingRecords() < maxStoreBatchRecords {
		return nil
	}
	return c.flush(ctx)
}

func (c *consumer) flush(ctx context.Context) error {
	if c.pendingRecords() == 0 && len(c.batch.SyncStates) == 0 {
		return nil
	}
	workspaces := len(c.batch.Workspaces)
	channels := len(c.batch.Channels)
	users := len(c.batch.Users)
	messages := len(c.batch.Messages)
	checkpoints := len(c.batch.SyncStates)
	lastCheckpoint := ""
	if checkpoints > 0 {
		lastCheckpoint = c.batch.SyncStates[checkpoints-1].Value
	}
	result, err := c.store.ApplyWriteBatch(ctx, c.batch)
	if err != nil {
		return err
	}
	c.summary.Workspaces += int64(workspaces)
	c.summary.Channels += int64(channels)
	c.summary.Users += int64(users)
	c.summary.Messages += int64(messages)
	c.summary.MessagesWritten += int64(result.MessagesWritten)
	c.summary.Checkpoints += int64(checkpoints)
	if lastCheckpoint != "" {
		c.lastCheckpoint = lastCheckpoint
	}
	c.batch = store.WriteBatch{}
	return nil
}

func rawJSON(value json.RawMessage) string {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "{}"
	}
	return string(trimmed)
}

func providerEnvironment(allowlist []string) []string {
	keys := append([]string{
		"COMSPEC", "HOME", "LOGNAME", "PATH", "PATHEXT", "SHELL", "SYSTEMROOT",
		"TEMP", "TMP", "TMPDIR", "USER", "USERNAME", "WINDIR",
	}, allowlist...)
	seen := make(map[string]struct{}, len(keys))
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	if b.limit <= 0 {
		return original, nil
	}
	if len(value) >= b.limit {
		b.Buffer.Reset()
		_, _ = b.Buffer.Write(value[len(value)-b.limit:])
		return original, nil
	}
	if b.Buffer.Len()+len(value) > b.limit {
		current := b.Buffer.Bytes()
		keep := b.limit - len(value)
		copy(current, current[len(current)-keep:])
		b.Buffer.Truncate(keep)
	}
	_, _ = b.Buffer.Write(value)
	return original, nil
}

func withStderr(err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, stderr)
}
