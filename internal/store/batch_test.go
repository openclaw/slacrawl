package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw/slacrawl/internal/store/storedb"
	"github.com/stretchr/testify/require"
)

func TestApplyWriteBatchPreservesHigherPriorityMessageState(t *testing.T) {
	st := openBatchTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedBatchCatalog(t, st, "T1", "C1", "U1", now)
	require.NoError(t, st.UpsertMessage(ctx, Message{
		ChannelID: "C1", TS: "1.000001", WorkspaceID: "T1", UserID: "U1",
		Text: "native", NormalizedText: "native", SourceRank: 2, SourceName: "api-bot",
		RawJSON: `{"source":"native"}`, UpdatedAt: now,
		Files: []MessageFile{{FileID: "F1", Name: "native-file.txt", RawJSON: "{}"}},
	}, []Mention{{Type: "user", TargetID: "U1"}}))

	result, err := st.ApplyWriteBatch(ctx, WriteBatch{
		Messages: []MessageWrite{{
			Message: Message{
				ChannelID: "C1", TS: "1.000001", WorkspaceID: "T1", UserID: "U2",
				Text: "provider", NormalizedText: "provider", SourceRank: 5,
				SourceName: "provider:test", RawJSON: `{"source":"provider"}`, UpdatedAt: now.Add(time.Second),
			},
			Mentions: []Mention{{Type: "user", TargetID: "U2"}}, PreserveHigherPriority: true,
		}},
		SyncStates: []SyncStateWrite{{SourceName: "provider:test", EntityType: "workspace", EntityID: "T1", Value: "cursor-1"}},
	})
	require.NoError(t, err)
	require.Zero(t, result.MessagesWritten)

	rows, err := st.QueryReadOnly(ctx, `select text, source_name, source_rank from messages where channel_id = 'C1' and ts = '1.000001'`)
	require.NoError(t, err)
	require.Equal(t, "native", rows[0]["text"])
	require.Equal(t, "api-bot", rows[0]["source_name"])
	require.Equal(t, int64(2), rows[0]["source_rank"])
	assertBatchCount(t, st, `select count(*) from message_files where channel_id = 'C1' and ts = '1.000001' and file_id = 'F1'`, 1)
	assertBatchCount(t, st, `select count(*) from message_mentions where channel_id = 'C1' and ts = '1.000001' and target_id = 'U1'`, 1)
	assertBatchCount(t, st, `select count(*) from message_mentions where channel_id = 'C1' and ts = '1.000001' and target_id = 'U2'`, 0)
	assertBatchCount(t, st, `select count(*) from message_events where channel_id = 'C1' and ts = '1.000001'`, 1)
	searchRows, err := st.QueryReadOnly(ctx, `select content from message_fts where message_key = 'C1|1.000001'`)
	require.NoError(t, err)
	require.Len(t, searchRows, 1)
	require.Contains(t, searchRows[0]["content"], "native-file.txt")
	checkpoint, err := st.GetSyncState(ctx, "provider:test", "workspace", "T1")
	require.NoError(t, err)
	require.Equal(t, "cursor-1", checkpoint)
}

func TestApplyWriteBatchPreservesSequentialMessageSemantics(t *testing.T) {
	st := openBatchTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	result, err := st.ApplyWriteBatch(ctx, WriteBatch{
		Workspaces: []Workspace{{ID: "T1", Name: "team", RawJSON: "{}", UpdatedAt: now}},
		Channels:   []Channel{{ID: "C1", WorkspaceID: "T1", Name: "general", Kind: "channel", RawJSON: "{}", UpdatedAt: now}},
		Users:      []User{{ID: "U1", WorkspaceID: "T1", Name: "one", RawJSON: "{}", UpdatedAt: now}, {ID: "U2", WorkspaceID: "T1", Name: "two", RawJSON: "{}", UpdatedAt: now}},
		Messages: []MessageWrite{
			{
				Message: Message{
					ChannelID: "C1", TS: "1.000001", WorkspaceID: "T1", UserID: "U1",
					Text: "first", NormalizedText: "first", SourceRank: 5, SourceName: "provider:test",
					RawJSON: `{"version":1}`, UpdatedAt: now,
					Files: []MessageFile{{FileID: "F1", Name: "retained-file.txt", RawJSON: "{}"}},
				},
				Mentions: []Mention{{Type: "user", TargetID: "U1"}}, PreserveHigherPriority: true,
			},
			{
				Message: Message{
					ChannelID: "C1", TS: "1.000001", WorkspaceID: "T1", UserID: "U2",
					Text: "second", NormalizedText: "second", SourceRank: 5, SourceName: "provider:test",
					RawJSON: `{"version":2}`, UpdatedAt: now.Add(time.Second), Files: nil,
				},
				Mentions: []Mention{{Type: "user", TargetID: "U2"}}, PreserveHigherPriority: true,
			},
		},
		SyncStates: []SyncStateWrite{{SourceName: "provider:test", EntityType: "workspace", EntityID: "T1", Value: "cursor-2"}},
	})
	require.NoError(t, err)
	require.Equal(t, 2, result.MessagesWritten)

	rows, err := st.QueryReadOnly(ctx, `select text, user_id, raw_json from messages where channel_id = 'C1' and ts = '1.000001'`)
	require.NoError(t, err)
	require.Equal(t, "second", rows[0]["text"])
	require.Equal(t, "U2", rows[0]["user_id"])
	require.Equal(t, `{"version":2}`, rows[0]["raw_json"])
	assertBatchCount(t, st, `select count(*) from message_events where channel_id = 'C1' and ts = '1.000001' and source_name = 'provider:test'`, 2)
	assertBatchCount(t, st, `select count(*) from message_mentions where channel_id = 'C1' and ts = '1.000001' and target_id = 'U1'`, 0)
	assertBatchCount(t, st, `select count(*) from message_mentions where channel_id = 'C1' and ts = '1.000001' and target_id = 'U2'`, 1)
	assertBatchCount(t, st, `select count(*) from message_files where channel_id = 'C1' and ts = '1.000001' and file_id = 'F1'`, 1)
	searchRows, err := st.QueryReadOnly(ctx, `select content from message_fts where message_key = 'C1|1.000001'`)
	require.NoError(t, err)
	require.Len(t, searchRows, 1)
	require.Contains(t, searchRows[0]["content"], "retained-file.txt")
}

func TestApplyWriteBatchRollsBackMessagesAndCheckpointOnFailure(t *testing.T) {
	st := openBatchTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedBatchCatalog(t, st, "T1", "C1", "U1", now)
	require.NoError(t, st.UpsertWorkspace(ctx, Workspace{ID: "T2", Name: "two", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertChannel(ctx, Channel{ID: "C2", WorkspaceID: "T2", Name: "two", Kind: "channel", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertMessage(ctx, Message{
		ChannelID: "C2", TS: "2.000002", WorkspaceID: "T2", Text: "existing",
		NormalizedText: "existing", SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: now,
	}, nil))

	_, err := st.ApplyWriteBatch(ctx, WriteBatch{
		Messages: []MessageWrite{
			{Message: batchMessage("C1", "1.000001", "T1", "first", now), PreserveHigherPriority: true},
			{Message: batchMessage("C2", "2.000002", "T1", "collision", now), PreserveHigherPriority: true},
		},
		SyncStates: []SyncStateWrite{{SourceName: "provider:test", EntityType: "workspace", EntityID: "T1", Value: "must-not-commit"}},
	})
	require.ErrorContains(t, err, "already belongs to workspace")
	assertBatchCount(t, st, `select count(*) from messages where channel_id = 'C1' and ts = '1.000001'`, 0)
	assertBatchCount(t, st, `select count(*) from sync_state where source_name = 'provider:test'`, 0)
}

func TestApplyWriteBatchRollsBackWhenCheckpointWriteFails(t *testing.T) {
	st := openBatchTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedBatchCatalog(t, st, "T1", "C1", "U1", now)
	_, err := st.DB().ExecContext(ctx, `
create trigger reject_batch_checkpoint before insert on sync_state
when new.source_name = 'provider:test'
begin select raise(abort, 'checkpoint rejected'); end`)
	require.NoError(t, err)

	_, err = st.ApplyWriteBatch(ctx, WriteBatch{
		Messages:   []MessageWrite{{Message: batchMessage("C1", "1.000001", "T1", "message", now), PreserveHigherPriority: true}},
		SyncStates: []SyncStateWrite{{SourceName: "provider:test", EntityType: "workspace", EntityID: "T1", Value: "cursor"}},
	})
	require.ErrorContains(t, err, "checkpoint rejected")
	assertBatchCount(t, st, `select count(*) from messages where channel_id = 'C1' and ts = '1.000001'`, 0)
	assertBatchCount(t, st, `select count(*) from message_fts where message_key = 'C1|1.000001'`, 0)
	assertBatchCount(t, st, `select count(*) from message_events where channel_id = 'C1' and ts = '1.000001'`, 0)
}

func TestApplyWriteBatchHonorsPerMessageRetention(t *testing.T) {
	st := openBatchTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedBatchCatalog(t, st, "T1", "C1", "U1", now)
	require.NoError(t, st.UpsertMessage(ctx, batchMessage("C1", "50.000001", "T1", "existing old", now), nil))
	require.NoError(t, st.SetSyncState(ctx, retentionFloorSource, retentionFloorEntityType, "T1|C1", "100.000000"))

	result, err := st.ApplyWriteBatch(ctx, WriteBatch{Messages: []MessageWrite{
		{Message: batchMessage("C1", "50.000001", "T1", "updated old", now.Add(time.Second)), PreserveHigherPriority: true, EnforceRetention: true},
		{Message: batchMessage("C1", "60.000001", "T1", "blocked old", now), PreserveHigherPriority: true, EnforceRetention: true},
		{Message: batchMessage("C1", "70.000001", "T1", "explicit restore", now), PreserveHigherPriority: true, EnforceRetention: false},
		{Message: batchMessage("C1", "150.000001", "T1", "new", now), PreserveHigherPriority: true, EnforceRetention: true},
	}})
	require.NoError(t, err)
	require.Equal(t, 3, result.MessagesWritten)
	assertBatchCount(t, st, `select count(*) from messages where channel_id = 'C1' and ts = '60.000001'`, 0)
	assertBatchCount(t, st, `select count(*) from messages where channel_id = 'C1' and ts in ('50.000001', '70.000001', '150.000001')`, 3)
}

func TestWriteMessageFTSGatesDeleteOnExistingMessage(t *testing.T) {
	ctx := context.Background()
	t.Run("insert", func(t *testing.T) {
		writer := &recordingFTSWriter{}
		require.NoError(t, writeMessageFTS(ctx, writer, "C1|1.000001", "new", false))
		require.Zero(t, writer.deletes)
		require.Equal(t, []storedb.InsertMessageFTSParams{{MessageKey: "C1|1.000001", Content: "new"}}, writer.inserts)
	})
	t.Run("update", func(t *testing.T) {
		writer := &recordingFTSWriter{}
		require.NoError(t, writeMessageFTS(ctx, writer, "C1|1.000001", "updated", true))
		require.Equal(t, []string{"C1|1.000001"}, writer.deletes)
		require.Equal(t, []storedb.InsertMessageFTSParams{{MessageKey: "C1|1.000001", Content: "updated"}}, writer.inserts)
	})
}

func TestApplyWriteBatchFTSInsertAndUpdateParity(t *testing.T) {
	st := openBatchTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedBatchCatalog(t, st, "T1", "C1", "U1", now)
	require.NoError(t, st.UpsertMessage(ctx, batchMessage("C1", "1.000001", "T1", "unrelated", now), nil))

	insertResult, err := st.ApplyWriteBatch(ctx, WriteBatch{Messages: []MessageWrite{{
		Message: batchMessage("C1", "2.000002", "T1", "inserted", now), PreserveHigherPriority: true,
	}}})
	require.NoError(t, err)
	require.Equal(t, 1, insertResult.MessagesWritten)
	assertBatchFTSContent(t, st, "C1|1.000001", "unrelated")
	assertBatchFTSContent(t, st, "C1|2.000002", "inserted")
	assertBatchCount(t, st, `select count(*) from message_fts where message_key = 'C1|2.000002'`, 1)

	updateResult, err := st.ApplyWriteBatch(ctx, WriteBatch{Messages: []MessageWrite{{
		Message: batchMessage("C1", "2.000002", "T1", "updated", now.Add(time.Second)), PreserveHigherPriority: true,
	}}})
	require.NoError(t, err)
	require.Equal(t, 1, updateResult.MessagesWritten)
	assertBatchFTSContent(t, st, "C1|1.000001", "unrelated")
	assertBatchFTSContent(t, st, "C1|2.000002", "updated")
	assertBatchCount(t, st, `select count(*) from message_fts where message_key = 'C1|2.000002'`, 1)
}

func TestApplyWriteBatchProviderFastPathChecksRowsAndMentions(t *testing.T) {
	st := openBatchTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedBatchCatalog(t, st, "T1", "C1", "U1", now)
	message := batchMessage("C1", "1.000001", "T1", "original", now)
	require.NoError(t, st.UpsertMessage(ctx, message, nil))

	unchanged := message
	unchanged.UpdatedAt = now.Add(time.Second)
	result, err := st.ApplyWriteBatch(ctx, WriteBatch{Messages: []MessageWrite{{
		Message: unchanged, PreserveHigherPriority: true, SkipUnchangedProviderRow: true,
	}}})
	require.NoError(t, err)
	require.Zero(t, result.MessagesWritten)
	assertBatchCount(t, st, `select count(*) from message_events where channel_id = 'C1' and ts = '1.000001'`, 1)

	edited := unchanged
	edited.Text = "edited"
	edited.NormalizedText = "edited"
	result, err = st.ApplyWriteBatch(ctx, WriteBatch{Messages: []MessageWrite{{
		Message: edited, PreserveHigherPriority: true, SkipUnchangedProviderRow: true,
	}}})
	require.NoError(t, err)
	require.Equal(t, 1, result.MessagesWritten)
	assertBatchFTSContent(t, st, "C1|1.000001", "edited")
	assertBatchCount(t, st, `select count(*) from message_events where channel_id = 'C1' and ts = '1.000001'`, 1)

	result, err = st.ApplyWriteBatch(ctx, WriteBatch{Messages: []MessageWrite{{
		Message: edited, Mentions: []Mention{{Type: "user", TargetID: "U2"}},
		PreserveHigherPriority: true, SkipUnchangedProviderRow: true,
	}}})
	require.NoError(t, err)
	require.Equal(t, 1, result.MessagesWritten)
	assertBatchCount(t, st, `select count(*) from message_mentions where channel_id = 'C1' and ts = '1.000001' and target_id = 'U2'`, 1)
}

type recordingFTSWriter struct {
	deletes []string
	inserts []storedb.InsertMessageFTSParams
}

func (w *recordingFTSWriter) DeleteMessageFTS(_ context.Context, key string) error {
	w.deletes = append(w.deletes, key)
	return nil
}

func (w *recordingFTSWriter) InsertMessageFTS(_ context.Context, params storedb.InsertMessageFTSParams) error {
	w.inserts = append(w.inserts, params)
	return nil
}

func openBatchTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "batch.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	return st
}

func seedBatchCatalog(t *testing.T, st *Store, workspaceID, channelID, userID string, now time.Time) {
	t.Helper()
	require.NoError(t, st.UpsertWorkspace(context.Background(), Workspace{ID: workspaceID, Name: workspaceID, RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertChannel(context.Background(), Channel{ID: channelID, WorkspaceID: workspaceID, Name: channelID, Kind: "channel", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertUser(context.Background(), User{ID: userID, WorkspaceID: workspaceID, Name: userID, RawJSON: "{}", UpdatedAt: now}))
}

func batchMessage(channelID, ts, workspaceID, text string, now time.Time) Message {
	return Message{
		ChannelID: channelID, TS: ts, WorkspaceID: workspaceID, UserID: "U1",
		Text: text, NormalizedText: text, SourceRank: 5, SourceName: "provider:test",
		RawJSON: `{"text":"` + text + `"}`, UpdatedAt: now,
	}
}

func assertBatchCount(t *testing.T, st *Store, query string, expected int64) {
	t.Helper()
	rows, err := st.QueryReadOnly(context.Background(), query)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, expected, rows[0]["count(*)"])
}

func assertBatchFTSContent(t *testing.T, st *Store, key, expected string) {
	t.Helper()
	rows, err := st.QueryReadOnly(context.Background(), `select content from message_fts where message_key = '`+key+`'`)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, expected, rows[0]["content"])
}
