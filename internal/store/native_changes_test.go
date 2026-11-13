package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func journalRows(t *testing.T, st *Store, after int64) []map[string]any {
	t.Helper()
	rows, err := st.QueryReadOnly(context.Background(), fmt.Sprintf("select * from native_changes where sequence > %d order by sequence", after))
	require.NoError(t, err)
	return rows
}

func journalHead(t *testing.T, st *Store) int64 {
	t.Helper()
	var head int64
	require.NoError(t, st.DB().QueryRow("select coalesce(max(sequence), 0) from native_changes").Scan(&head))
	return head
}

func journalGeneration(t *testing.T, st *Store) string {
	t.Helper()
	var generation string
	require.NoError(t, st.DB().QueryRow("select generation_id from native_change_state").Scan(&generation))
	require.Len(t, generation, 32)
	return generation
}

func TestNativeJournalWritesAndRollback(t *testing.T) {
	st := openBatchTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	message := batchMessage("C1", "1.000001", "T1", "private body never in journal", now)
	message.Files = []MessageFile{{FileID: "F1", Name: "private-name.txt", RawJSON: "{}"}}
	_, err := st.ApplyWriteBatch(ctx, WriteBatch{
		Workspaces: []Workspace{{ID: "T1", Name: "private workspace", RawJSON: "{}", UpdatedAt: now}},
		Channels:   []Channel{{ID: "C1", WorkspaceID: "T1", Name: "private channel", Kind: "private", IsPrivate: true, RawJSON: "{}", UpdatedAt: now}},
		Users:      []User{{ID: "U1", WorkspaceID: "T1", Name: "private user", RawJSON: "{}", UpdatedAt: now}},
		Messages:   []MessageWrite{{Message: message, Mentions: []Mention{{Type: "user", TargetID: "U1", DisplayText: "private mention"}}}},
		SyncStates: []SyncStateWrite{{SourceName: "provider:test", EntityType: "workspace", EntityID: "T1", Value: "private checkpoint"}},
	})
	require.NoError(t, err)
	rows := journalRows(t, st, 0)
	tables := map[string]bool{}
	entries := map[string]bool{}
	for _, row := range rows {
		tables[row["table_name"].(string)] = true
		id := row["entry_id"].(string)
		require.Len(t, id, 32)
		require.False(t, entries[id])
		entries[id] = true
		require.Equal(t, "i", row["operation"])
		var key map[string]string
		require.NoError(t, json.Unmarshal([]byte(row["key_json"].(string)), &key))
		require.NotEmpty(t, key)
	}
	require.Equal(t, map[string]bool{"workspaces": true, "channels": true, "users": true, "messages": true, "message_files": true, "message_mentions": true, "sync_state": true}, tables)
	encoded, err := json.Marshal(rows)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private")

	head := journalHead(t, st)
	_, err = st.ApplyWriteBatch(ctx, WriteBatch{Messages: []MessageWrite{
		{Message: batchMessage("C1", "2.000002", "T1", "rollback", now)},
		{Message: batchMessage("C1", "1.000001", "OTHER", "collision", now)},
	}, SyncStates: []SyncStateWrite{{SourceName: "provider:test", EntityType: "workspace", EntityID: "T1", Value: "must not commit"}}})
	require.ErrorContains(t, err, "already belongs to workspace")
	require.Equal(t, head, journalHead(t, st))
	checkpoint, err := st.GetSyncState(ctx, "provider:test", "workspace", "T1")
	require.NoError(t, err)
	require.Equal(t, "private checkpoint", checkpoint)

	// Rejected lower-priority writes produce neither rows nor change entries.
	lower := message
	lower.SourceRank = 9
	lower.SourceName = "provider:lower"
	lower.Text = "rejected"
	_, err = st.ApplyWriteBatch(ctx, WriteBatch{Messages: []MessageWrite{{Message: lower, PreserveHigherPriority: true}}})
	require.NoError(t, err)
	require.Equal(t, head, journalHead(t, st))
}

func TestNativeJournalDirectUpdatesAndRemovals(t *testing.T) {
	st := openBatchTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedBatchCatalog(t, st, "T1", "C1", "U1", now)
	message := batchMessage("C1", "1.000001", "T1", "message", now)
	message.Files = []MessageFile{{FileID: "F1", RawJSON: "{}"}}
	require.NoError(t, st.UpsertMessage(ctx, message, []Mention{{Type: "user", TargetID: "U1"}}))
	head := journalHead(t, st)
	require.NoError(t, st.RenameChannel(ctx, "T1", "C1", "renamed"))
	require.NoError(t, st.SetChannelArchived(ctx, "T1", "C1", true))
	require.NoError(t, st.UpdateFileFetchStatus(ctx, "C1", "1.000001", "F1", "", "failed", "fixture"))
	require.NoError(t, st.UpdateFileMedia(ctx, FileMediaUpdate{ChannelID: "C1", TS: "1.000001", FileID: "F1", MediaPath: "fixture/file", ContentSHA256: "abc", ContentSize: 3}))
	_, err := st.DB().Exec(`update message_mentions set display_text = 'changed' where channel_id = 'C1'`)
	require.NoError(t, err)
	rows := journalRows(t, st, head)
	require.Len(t, rows, 5)
	for _, row := range rows {
		require.Equal(t, "u", row["operation"])
		require.Equal(t, "T1", row["workspace_id"])
	}
	head = journalHead(t, st)
	_, err = st.DB().Exec(`update message_mentions set display_text = display_text`)
	require.NoError(t, err)
	require.Equal(t, head, journalHead(t, st))

	// Native direct SQL identity changes must remove the old key before exposing
	// the new key. Channel workspace replacement uses the same trigger boundary.
	_, err = st.DB().Exec(`update message_mentions set target_id = 'U2'; update channels set workspace_id = 'T2' where id = 'C1'`)
	require.NoError(t, err)
	rows = journalRows(t, st, head)
	require.Len(t, rows, 4)
	require.Equal(t, "d", rows[0]["operation"])
	require.JSONEq(t, `{"channel_id":"C1","ts":"1.000001","mention_type":"user","target_id":"U1"}`, rows[0]["key_json"].(string))
	require.Equal(t, "u", rows[1]["operation"])
	require.JSONEq(t, `{"channel_id":"C1","ts":"1.000001","mention_type":"user","target_id":"U2"}`, rows[1]["key_json"].(string))
	require.Equal(t, "d", rows[2]["operation"])
	require.Equal(t, "T1", rows[2]["workspace_id"])
	require.Equal(t, "u", rows[3]["operation"])
	require.Equal(t, "T2", rows[3]["workspace_id"])

	head = journalHead(t, st)
	deleted, err := st.DeleteMessageBySource(ctx, "T1", "C1", "1.000001", message.SourceName)
	require.NoError(t, err)
	require.True(t, deleted)
	rows = journalRows(t, st, head)
	require.Len(t, rows, 3)
	for _, row := range rows {
		require.Equal(t, "d", row["operation"])
		require.Equal(t, "T1", row["workspace_id"])
	}
	requireTableCount(t, st, "message_events", 0)
}

func TestNativeJournalPurgePreservesRemovalObservations(t *testing.T) {
	st := openBatchTestStore(t)
	old := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	upsertPurgeTestMessage(t, st, "T1", "C1", old, "expired", "F1", "", 0)
	head := journalHead(t, st)
	_, err := st.PurgeMessages(context.Background(), PurgeOptions{Before: old.Add(time.Hour), WorkspaceID: "T1", Delete: true})
	require.NoError(t, err)
	requireTableCount(t, st, "messages", 0)
	requireTableCount(t, st, "message_events", 0)
	removed := map[string]bool{}
	for _, row := range journalRows(t, st, head) {
		if row["operation"] == "d" {
			removed[row["table_name"].(string)] = true
			require.Equal(t, "T1", row["workspace_id"])
		}
	}
	require.Equal(t, map[string]bool{"messages": true, "message_files": true, "message_mentions": true}, removed)
}

func TestNativeJournalUpgradeAndRepair(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "archive.db")
	// A real v8 schema without the journal upgrades without inventing historical
	// events or changing native rows. Future writes start the new local generation.
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(schema + "; pragma user_version = 8;")
	require.NoError(t, err)
	_, err = db.Exec(`insert into workspaces values ('T1','retained',null,null,'{}','2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	st, err := Open(path)
	require.NoError(t, err)
	generation := journalGeneration(t, st)
	_, err = st.Status(ctx)
	require.NoError(t, err)
	require.Zero(t, journalHead(t, st))
	requireTableCount(t, st, "workspaces", 1)
	require.NoError(t, st.SetSyncState(ctx, "fixture", "workspace", "T1", "checkpoint"))
	head := journalHead(t, st)
	require.NoError(t, st.Close())
	beforeOpen, err := os.ReadFile(path)
	require.NoError(t, err)
	st, err = Open(path)
	require.NoError(t, err)
	require.Equal(t, generation, journalGeneration(t, st))
	require.Equal(t, head, journalHead(t, st))
	require.NoError(t, st.Close())
	afterOpen, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, beforeOpen, afterOpen, "healthy reopen must not rewrite the database")
	st, err = Open(path)
	require.NoError(t, err)
	for _, damage := range []string{
		`drop trigger native_changes_sync_state_u`,
		`drop trigger native_changes_sync_state_u; create trigger native_changes_sync_state_u after update on sync_state begin select 1; end`,
		`delete from native_change_state`,
		`drop table native_changes`,
	} {
		_, err = st.DB().Exec(damage)
		require.NoError(t, err)
		_, err = st.Status(ctx)
		require.ErrorContains(t, err, "native journal")
		require.NoError(t, st.Close())
		st, err = Open(path)
		require.NoError(t, err)
		next := journalGeneration(t, st)
		require.NotEqual(t, generation, next)
		generation = next
		requireTableCount(t, st, "workspaces", 1)
		checkpoint, err := st.GetSyncState(ctx, "fixture", "workspace", "T1")
		require.NoError(t, err)
		require.Equal(t, "checkpoint", checkpoint)
		status, err := st.Status(ctx)
		require.NoError(t, err)
		body, err := json.Marshal(status)
		require.NoError(t, err)
		require.Contains(t, string(body), `"generation_id":"`+next+`"`)
	}
	require.NoError(t, st.Close())
}
