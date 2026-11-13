package share

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw/slacrawl/internal/store"
	"github.com/stretchr/testify/require"
)

func TestNativeJournalShareMergeAndRestore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer source.Close()
	opts := Options{RepoPath: filepath.Join(dir, "share")}
	manifest, err := Export(ctx, source, opts)
	require.NoError(t, err)
	for _, table := range manifest.Tables {
		require.NotEqual(t, "native_changes", table.Name)
		require.NotEqual(t, "native_change_state", table.Name)
	}
	reader, err := store.Open(filepath.Join(dir, "reader.db"))
	require.NoError(t, err)
	defer reader.Close()
	initial, err := reader.Status(ctx)
	require.NoError(t, err)
	_, err = Import(ctx, reader, opts)
	require.NoError(t, err)
	merged, err := reader.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, initial.NativeChanges.GenerationID, merged.NativeChanges.GenerationID)
	require.Greater(t, merged.NativeChanges.HeadSequence, initial.NativeChanges.HeadSequence)
	rows, err := reader.QueryReadOnly(ctx, `select operation, key_json from native_changes where table_name = 'messages'`)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "i", rows[0]["operation"])
	require.JSONEq(t, `{"channel_id":"C1","ts":"123.456"}`, rows[0]["key_json"].(string))
	require.NoError(t, reader.UpsertMessage(ctx, store.Message{ChannelID: "C1", TS: "456.789", WorkspaceID: "T1", Text: "local", NormalizedText: "local", SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: time.Now().UTC()}, nil))
	before, err := reader.Status(ctx)
	require.NoError(t, err)
	_, err = reader.DB().Exec(`create trigger reject_restore before insert on messages begin select raise(abort, 'fixture restore rejected'); end`)
	require.NoError(t, err)
	_, err = Restore(ctx, reader, opts)
	require.ErrorContains(t, err, "fixture restore rejected")
	failed, err := reader.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, before.NativeChanges, failed.NativeChanges)
	require.Equal(t, before.Messages, failed.Messages)
	_, err = reader.DB().Exec(`drop trigger reject_restore`)
	require.NoError(t, err)
	_, err = Restore(ctx, reader, opts)
	require.NoError(t, err)
	restored, err := reader.Status(ctx)
	require.NoError(t, err)
	require.NotEqual(t, before.NativeChanges.GenerationID, restored.NativeChanges.GenerationID)
	require.Greater(t, restored.NativeChanges.FloorSequence, before.NativeChanges.HeadSequence)
	require.Equal(t, 1, restored.Messages)
	// Only post-restore bookkeeping can be above the baseline floor.
	rows, err = reader.QueryReadOnly(ctx, `select table_name from native_changes where sequence > (select floor_sequence from native_change_state)`)
	require.NoError(t, err)
	for _, row := range rows {
		require.Equal(t, "sync_state", row["table_name"])
	}
	// A foreign journal cannot be imported even when a snapshot supplies it.
	manifest.Tables = append(manifest.Tables, TableManifest{Name: "native_changes"})
	writeManifest(t, opts.RepoPath, manifest)
	_, err = Import(ctx, reader, opts)
	require.ErrorContains(t, err, `unknown table "native_changes"`)
	after, err := reader.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, restored.NativeChanges, after.NativeChanges)
}
