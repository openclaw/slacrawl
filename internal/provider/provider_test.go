package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/stretchr/testify/require"
)

func TestSyncAppliesRecordsPreservesPriorityAndPersistsCheckpoint(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{ID: "T1", Name: "rich workspace", RawJSON: `{"rich":true}`, UpdatedAt: now}))
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C1", WorkspaceID: "T1", Name: "rich-channel", Kind: "private_channel", RawJSON: `{"rich":true}`, UpdatedAt: now}))
	require.NoError(t, st.UpsertUser(ctx, store.User{ID: "U1", WorkspaceID: "T1", Name: "rich-user", RawJSON: `{"rich":true}`, UpdatedAt: now}))
	require.NoError(t, st.UpsertMessage(ctx, store.Message{
		ChannelID: "C1", TS: "2.000002", WorkspaceID: "T1", UserID: "U1", Text: "native",
		NormalizedText: "native", SourceRank: 2, SourceName: "api-bot", RawJSON: `{"native":true}`, UpdatedAt: now,
	}, nil))
	require.NoError(t, st.SetSyncState(ctx, "provider:fixture", "workspace", "T1", "old-checkpoint"))

	requestPath := filepath.Join(t.TempDir(), "request.json")
	summary, err := Sync(ctx, st, helperProvider(t, "success", requestPath), Options{
		WorkspaceID: "T1", Channels: []string{"C1"}, ExcludeChannels: []string{"skip"},
		Since: "1.000001", Full: true, Limit: 2,
	})
	require.NoError(t, err)
	require.Equal(t, int64(5), summary.Records)
	require.Equal(t, int64(2), summary.Messages)
	require.Equal(t, int64(1), summary.MessagesWritten)

	var sent request
	require.NoError(t, json.Unmarshal(requireReadFile(t, requestPath), &sent))
	require.Empty(t, sent.Checkpoint)
	require.True(t, sent.Full)
	require.Equal(t, "1.000001", sent.Since)
	require.Equal(t, []string{"C1"}, sent.Channels)
	require.Equal(t, []string{"skip"}, sent.ExcludeChannels)
	require.NotNil(t, sent.Limit)
	require.Equal(t, 2, *sent.Limit)
	require.Equal(t, 7, sent.BatchSize)

	checkpoint, err := st.GetSyncState(ctx, "provider:fixture", "workspace", "T1")
	require.NoError(t, err)
	require.Equal(t, "old-checkpoint", checkpoint)
	boundedKey := stateKey("T1", Options{
		WorkspaceID: "T1", Channels: []string{"C1"}, ExcludeChannels: []string{"skip"},
		Since: "1.000001", Full: true, Limit: 2,
	})
	boundedCheckpoint, err := st.GetSyncState(ctx, "provider:fixture", boundedKey.entityType, boundedKey.entityID)
	require.NoError(t, err)
	require.Equal(t, `{"cursor":"done"}`, boundedCheckpoint)
	rows, err := st.QueryReadOnly(ctx, `
select ts, text, normalized_text, source_name, source_rank
from messages where channel_id = 'C1' order by ts`)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "provider text <@U1>", rows[0]["text"])
	require.Equal(t, "provider:fixture", rows[0]["source_name"])
	require.Equal(t, int64(5), rows[0]["source_rank"])
	require.Contains(t, rows[0]["normalized_text"], "@U1")
	require.Equal(t, "native", rows[1]["text"])
	require.Equal(t, "api-bot", rows[1]["source_name"])

	catalog, err := st.QueryReadOnly(ctx, `
select w.name as workspace_name, c.name as channel_name, u.name as user_name
from workspaces w join channels c on c.workspace_id = w.id join users u on u.workspace_id = w.id
where w.id = 'T1' and c.id = 'C1' and u.id = 'U1'`)
	require.NoError(t, err)
	require.Equal(t, "rich workspace", catalog[0]["workspace_name"])
	require.Equal(t, "rich-channel", catalog[0]["channel_name"])
	require.Equal(t, "rich-user", catalog[0]["user_name"])
}

func TestInterruptedFullResumesAndCompletedFullSeedsIncremental(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	firstRequest := filepath.Join(t.TempDir(), "first.json")
	_, err := Sync(ctx, st, helperProvider(t, "fail", firstRequest), Options{WorkspaceID: "T1", Full: true})
	require.Error(t, err)

	fullKey := stateKey("T1", Options{WorkspaceID: "T1", Full: true})
	checkpoint, stateErr := st.GetSyncState(ctx, "provider:fixture", fullKey.entityType, fullKey.entityID)
	require.NoError(t, stateErr)
	require.Equal(t, `{"cursor":"partial"}`, checkpoint)

	secondRequest := filepath.Join(t.TempDir(), "second.json")
	_, err = Sync(ctx, st, helperProvider(t, "success", secondRequest), Options{WorkspaceID: "T1", Full: true})
	require.NoError(t, err)
	var sent request
	require.NoError(t, json.Unmarshal(requireReadFile(t, secondRequest), &sent))
	require.Equal(t, `{"cursor":"partial"}`, sent.Checkpoint)

	incrementalCheckpoint, stateErr := st.GetSyncState(ctx, "provider:fixture", "workspace", "T1")
	require.NoError(t, stateErr)
	require.Equal(t, `{"cursor":"done"}`, incrementalCheckpoint)
}

func TestFailedProcessDoesNotPromoteCompletedFullCheckpoint(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	require.NoError(t, st.SetSyncState(ctx, "provider:fixture", "workspace", "T1", "old-incremental"))

	_, err := Sync(ctx, st, helperProvider(t, "done-fail", filepath.Join(t.TempDir(), "request.json")), Options{WorkspaceID: "T1", Full: true})
	require.ErrorContains(t, err, "provider exploded after done")

	checkpoint, stateErr := st.GetSyncState(ctx, "provider:fixture", "workspace", "T1")
	require.NoError(t, stateErr)
	require.Equal(t, "old-incremental", checkpoint)
}

func TestSyncReturnsProcessErrorAfterSavingAppliedCheckpoint(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	requestPath := filepath.Join(t.TempDir(), "request.json")
	_, err := Sync(ctx, st, helperProvider(t, "fail", requestPath), Options{WorkspaceID: "T1"})
	require.ErrorContains(t, err, "provider exploded")

	checkpoint, stateErr := st.GetSyncState(ctx, "provider:fixture", "workspace", "T1")
	require.NoError(t, stateErr)
	require.Equal(t, `{"cursor":"partial"}`, checkpoint)

	var sent request
	require.NoError(t, json.Unmarshal(requireReadFile(t, requestPath), &sent))
	require.Empty(t, sent.Checkpoint)
}

func TestSyncRejectsCrossWorkspaceRecord(t *testing.T) {
	st := openTestStore(t)
	_, err := Sync(context.Background(), st, helperProvider(t, "wrong-workspace", filepath.Join(t.TempDir(), "request.json")), Options{WorkspaceID: "T1"})
	require.ErrorContains(t, err, `belongs to workspace "T2"`)
	status, statusErr := st.Status(context.Background())
	require.NoError(t, statusErr)
	require.Zero(t, status.Messages)
}

func TestSyncRejectsProviderIgnoringMessageLimit(t *testing.T) {
	st := openTestStore(t)
	_, err := Sync(context.Background(), st, helperProvider(t, "too-many-messages", filepath.Join(t.TempDir(), "request.json")), Options{
		WorkspaceID: "T1",
		Limit:       2,
	})
	require.ErrorContains(t, err, "more than requested message limit 2")
	status, statusErr := st.Status(context.Background())
	require.NoError(t, statusErr)
	require.Equal(t, 2, status.Messages)
}

func TestStateKeySeparatesBoundedLimits(t *testing.T) {
	limitTwo := stateKey("T1", Options{WorkspaceID: "T1", Limit: 2})
	limitHundred := stateKey("T1", Options{WorkspaceID: "T1", Limit: 100})
	require.NotEqual(t, limitTwo, limitHundred)
}

func TestSyncRejectsSourceRankTie(t *testing.T) {
	st := openTestStore(t)
	providerConfig := helperProvider(t, "success", filepath.Join(t.TempDir(), "request.json"))
	providerConfig.SourceRank = 2
	_, err := Sync(context.Background(), st, providerConfig, Options{WorkspaceID: "T1"})
	require.ErrorContains(t, err, "greater than 2")
}

func TestApplyMessageRequiresCatalogReferencesInWorkspace(t *testing.T) {
	tests := []struct {
		name    string
		channel *store.Channel
		user    *store.User
		userID  string
		message string
	}{
		{name: "missing channel", message: `missing channel "C1"`},
		{name: "wrong channel workspace", channel: &store.Channel{ID: "C1", WorkspaceID: "T2", Name: "other", Kind: "channel"}, message: `channel "C1" belongs to workspace "T2"`},
		{name: "wrong user workspace", channel: &store.Channel{ID: "C1", WorkspaceID: "T1", Name: "one", Kind: "channel"}, user: &store.User{ID: "U1", WorkspaceID: "T2", Name: "other"}, userID: "U1", message: `user "U1" belongs to workspace "T2"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st := openTestStore(t)
			now := time.Now().UTC()
			require.NoError(t, st.UpsertWorkspace(context.Background(), store.Workspace{ID: "T1", Name: "one", RawJSON: "{}", UpdatedAt: now}))
			require.NoError(t, st.UpsertWorkspace(context.Background(), store.Workspace{ID: "T2", Name: "two", RawJSON: "{}", UpdatedAt: now}))
			if test.channel != nil {
				test.channel.RawJSON = "{}"
				test.channel.UpdatedAt = now
				require.NoError(t, st.UpsertChannel(context.Background(), *test.channel))
			}
			if test.user != nil {
				test.user.RawJSON = "{}"
				test.user.UpdatedAt = now
				require.NoError(t, st.UpsertUser(context.Background(), *test.user))
			}
			consumer := consumer{
				store: st, config: config.Provider{Name: "fixture", SourceRank: 5},
				workspaceID: "T1", sourceName: "provider:fixture", now: now,
				validChannels: make(map[string]struct{}), validUsers: make(map[string]struct{}),
			}
			err := consumer.applyMessage(context.Background(), record{
				WorkspaceID: "T1", ChannelID: "C1", TS: "1.000001", UserID: test.userID, Text: "message",
			})
			require.ErrorContains(t, err, test.message)
			status, statusErr := st.Status(context.Background())
			require.NoError(t, statusErr)
			require.Zero(t, status.Messages)
		})
	}
}

func TestApplyMessagePreservesUncataloguedUserID(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{ID: "T1", Name: "one", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{ID: "T2", Name: "two", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C1", WorkspaceID: "T1", Name: "one", Kind: "channel", RawJSON: "{}", UpdatedAt: now}))
	consumer := consumer{
		store: st, config: config.Provider{Name: "fixture", SourceRank: 5},
		workspaceID: "T1", sourceName: "provider:fixture", now: now,
		validChannels: make(map[string]struct{}), validUsers: make(map[string]struct{}),
	}

	require.NoError(t, consumer.applyMessage(ctx, record{
		WorkspaceID: "T1", ChannelID: "C1", TS: "1.000001", UserID: "U-missing", Text: "message",
	}))
	require.NoError(t, consumer.flush(ctx))
	rows, err := st.QueryReadOnly(ctx, `select user_id from messages where channel_id = 'C1' and ts = '1.000001'`)
	require.NoError(t, err)
	require.Equal(t, "U-missing", rows[0]["user_id"])
	workspaceID, err := st.UserWorkspaceID(ctx, "U-missing")
	require.NoError(t, err)
	require.Equal(t, "T1", workspaceID)
	require.ErrorContains(t, st.EnsureUser(ctx, store.User{ID: "U-missing", WorkspaceID: "T2", Name: "wrong", RawJSON: "{}", UpdatedAt: now}), `belongs to workspace "T1"`)
	require.NoError(t, st.EnsureUser(ctx, store.User{ID: "U-missing", WorkspaceID: "T1", Name: "later profile", RawJSON: "{}", UpdatedAt: now}))
	workspaceID, err = st.UserWorkspaceID(ctx, "U-missing")
	require.NoError(t, err)
	require.Equal(t, "T1", workspaceID)
	profile, err := st.QueryReadOnly(ctx, `select name from users where id = 'U-missing'`)
	require.NoError(t, err)
	require.Equal(t, "later profile", profile[0]["name"])
}

func TestProviderHelperProcess(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || len(os.Args) <= separator+2 {
		return
	}
	mode := os.Args[separator+1]
	requestPath := os.Args[separator+2]
	var sent request
	if err := json.NewDecoder(os.Stdin).Decode(&sent); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	data, _ := json.Marshal(sent)
	_ = os.WriteFile(requestPath, data, 0o600)
	emit := func(value any) {
		_ = json.NewEncoder(os.Stdout).Encode(value)
	}
	emit(map[string]any{"type": "hello", "protocol": ProtocolVersion, "provider": "fixture-cache"})
	switch mode {
	case "success":
		emit(map[string]any{"type": "workspace", "id": "T1", "name": "sparse", "raw_json": map[string]any{}})
		emit(map[string]any{"type": "user", "workspace_id": "T1", "id": "U1", "name": "sparse", "raw_json": map[string]any{}})
		emit(map[string]any{"type": "channel", "workspace_id": "T1", "id": "C1", "name": "sparse", "kind": "channel", "raw_json": map[string]any{}})
		emit(map[string]any{"type": "message", "workspace_id": "T1", "channel_id": "C1", "ts": "1.000001", "user_id": "U1", "text": "provider text <@U1>", "raw_json": map[string]any{}})
		emit(map[string]any{"type": "message", "workspace_id": "T1", "channel_id": "C1", "ts": "2.000002", "user_id": "U1", "text": "lower priority", "raw_json": map[string]any{}})
		emit(map[string]any{"type": "checkpoint", "entity_type": "workspace", "entity_id": "T1", "value": `{"cursor":"done"}`})
		emit(map[string]any{"type": "done", "records": 5})
		os.Exit(0)
	case "fail":
		emit(map[string]any{"type": "workspace", "id": "T1", "name": "T1", "raw_json": map[string]any{}})
		emit(map[string]any{"type": "checkpoint", "entity_type": "workspace", "entity_id": "T1", "value": `{"cursor":"partial"}`})
		fmt.Fprintln(os.Stderr, "provider exploded")
		os.Exit(7)
	case "done-fail":
		emit(map[string]any{"type": "workspace", "id": "T1", "name": "T1", "raw_json": map[string]any{}})
		emit(map[string]any{"type": "checkpoint", "entity_type": "workspace", "entity_id": "T1", "value": `{"cursor":"done"}`})
		emit(map[string]any{"type": "done", "records": 1})
		fmt.Fprintln(os.Stderr, "provider exploded after done")
		os.Exit(7)
	case "wrong-workspace":
		emit(map[string]any{"type": "message", "workspace_id": "T2", "channel_id": "C1", "ts": "1.000001", "text": "wrong"})
		os.Exit(0)
	case "too-many-messages":
		emit(map[string]any{"type": "workspace", "id": "T1", "name": "T1", "raw_json": map[string]any{}})
		emit(map[string]any{"type": "user", "workspace_id": "T1", "id": "U1", "name": "one", "raw_json": map[string]any{}})
		emit(map[string]any{"type": "channel", "workspace_id": "T1", "id": "C1", "name": "one", "kind": "channel", "raw_json": map[string]any{}})
		for i := 1; i <= 3; i++ {
			emit(map[string]any{"type": "message", "workspace_id": "T1", "channel_id": "C1", "ts": fmt.Sprintf("%d.000001", i), "user_id": "U1", "text": "message"})
		}
		emit(map[string]any{"type": "done", "records": 6})
		os.Exit(0)
	default:
		os.Exit(3)
	}
}

func helperProvider(t *testing.T, mode, requestPath string) config.Provider {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	return config.Provider{
		Name: "fixture", Command: executable,
		Args:       []string{"-test.run=TestProviderHelperProcess", "--", mode, requestPath},
		SourceRank: 5, BatchSize: 7,
	}
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	return st
}

func requireReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}
