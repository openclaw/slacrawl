package share

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/media"
	"github.com/openclaw/slacrawl/internal/store"
)

func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()

	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	manifest, err := Export(ctx, source, opts)
	require.NoError(t, err)
	require.Equal(t, 1, manifest.Version)
	require.NotEmpty(t, manifest.Tables)
	require.FileExists(t, filepath.Join(opts.RepoPath, ManifestName))

	reader := seedStore(t, filepath.Join(dir, "reader.db"))
	defer func() { require.NoError(t, reader.Close()) }()

	imported, err := Import(ctx, reader, opts)
	require.NoError(t, err)
	require.Equal(t, manifest.GeneratedAt.UTC().Format(time.RFC3339Nano), imported.GeneratedAt.UTC().Format(time.RFC3339Nano))

	rows, err := reader.Search(ctx, "", "archive", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "git backed archive works", rows[0].Text)
	heads, err := reader.QueryReadOnly(ctx, `select count(*) as count from message_event_heads`)
	require.NoError(t, err)
	require.Equal(t, int64(1), heads[0]["count"])
}

func TestImportMergesWithoutRemovingLocalRowsOrNewerTombstones(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	_, err := Export(ctx, source, opts)
	require.NoError(t, err)

	reader := seedStore(t, filepath.Join(dir, "reader.db"))
	defer func() { require.NoError(t, reader.Close()) }()
	newer := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	_, err = reader.DB().ExecContext(ctx, `
update channels set is_archived = 1, updated_at = ? where id = 'C1';
update users set is_deleted = 1, updated_at = ? where id = 'U1';
update messages set deleted_ts = '124.000', updated_at = ? where channel_id = 'C1' and ts = '123.456';
`, newer, newer, newer)
	require.NoError(t, err)
	require.NoError(t, reader.UpsertMessage(ctx, store.Message{
		ChannelID: "C1", TS: "999.000", WorkspaceID: "T1", UserID: "U1",
		Text: "local only", NormalizedText: "local only", SourceRank: 3,
		SourceName: "desktop-indexeddb", RawJSON: `{"text":"local only"}`, UpdatedAt: time.Now().UTC(),
	}, nil))
	require.NoError(t, reader.SetSyncState(ctx, "local", "cursor", "C1", "keep"))

	_, err = Import(ctx, reader, opts)
	require.NoError(t, err)
	rows, err := reader.QueryReadOnly(ctx, `
select c.is_archived, u.is_deleted, coalesce(m.deleted_ts, '') as deleted_ts
from channels c join users u on u.workspace_id = c.workspace_id
join messages m on m.workspace_id = c.workspace_id
where c.id = 'C1' and u.id = 'U1' and m.channel_id = 'C1' and m.ts = '123.456'
`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"is_archived": int64(1), "is_deleted": int64(1), "deleted_ts": "124.000"}}, rows)
	local, err := reader.QueryReadOnly(ctx, `select text from messages where channel_id = 'C1' and ts = '999.000'`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"text": "local only"}}, local)
	matches, err := reader.Search(ctx, "", "local only", 10)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	state, err := reader.GetSyncState(ctx, "local", "cursor", "C1")
	require.NoError(t, err)
	require.Equal(t, "keep", state)
}

func TestImportRejectsCrossWorkspaceIdentityCollisions(t *testing.T) {
	ctx := context.Background()
	for _, table := range []string{"channels", "users"} {
		t.Run(table, func(t *testing.T) {
			dir := t.TempDir()
			source := seedStore(t, filepath.Join(dir, "source.db"))
			defer func() { require.NoError(t, source.Close()) }()
			_, err := source.DB().ExecContext(ctx, `insert into workspaces (id, name, raw_json, updated_at) values ('T2', 'other', '{}', '2026-07-17T00:00:00Z')`)
			require.NoError(t, err)
			_, err = source.DB().ExecContext(ctx, "update "+table+" set workspace_id = 'T2' where id = ?", map[string]string{"channels": "C1", "users": "U1"}[table])
			require.NoError(t, err)
			opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
			_, err = Export(ctx, source, opts)
			require.NoError(t, err)

			destination := seedStore(t, filepath.Join(dir, "destination.db"))
			defer func() { require.NoError(t, destination.Close()) }()
			_, err = Import(ctx, destination, opts)
			require.ErrorContains(t, err, "workspace collision")
			rows, queryErr := destination.QueryReadOnly(ctx, "select workspace_id from "+table+" where id = '"+map[string]string{"channels": "C1", "users": "U1"}[table]+"'")
			require.NoError(t, queryErr)
			require.Equal(t, "T1", rows[0]["workspace_id"])
		})
	}
}

func TestImportRejectsMessageWorkspaceCollisionWithoutLocalChannel(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	_, err := source.DB().ExecContext(ctx, `
insert into workspaces (id, name, raw_json, updated_at) values ('T2', 'other', '{}', '2026-07-17T00:00:00Z');
update channels set workspace_id = 'T2' where id = 'C1';
update messages set workspace_id = 'T2', updated_at = '2026-07-17T00:00:01Z' where channel_id = 'C1' and ts = '123.456';
`)
	require.NoError(t, err)
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	_, err = Export(ctx, source, opts)
	require.NoError(t, err)

	destination := seedStore(t, filepath.Join(dir, "destination.db"))
	defer func() { require.NoError(t, destination.Close()) }()
	_, err = destination.DB().ExecContext(ctx, `delete from channels where id = 'C1'`)
	require.NoError(t, err)
	_, err = Import(ctx, destination, opts)
	require.ErrorContains(t, err, "workspace collision")
	rows, queryErr := destination.QueryReadOnly(ctx, `select workspace_id from messages where channel_id = 'C1' and ts = '123.456'`)
	require.NoError(t, queryErr)
	require.Equal(t, "T1", rows[0]["workspace_id"])
}

func TestImportPreservesHigherPriorityMessageOnEqualTimestamp(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	updatedAt := time.Now().UTC().Truncate(time.Second)
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	require.NoError(t, source.UpsertMessage(ctx, store.Message{
		ChannelID: "C1", TS: "123.456", WorkspaceID: "T1", UserID: "U1",
		Text: "lower priority", NormalizedText: "lower priority", SourceRank: 4,
		SourceName: "mcp", RawJSON: `{"source":"mcp"}`, UpdatedAt: updatedAt,
		Files: []store.MessageFile{{FileID: "F-low", Name: "lower.txt", RawJSON: `{}`}},
	}, nil))
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	_, err := Export(ctx, source, opts)
	require.NoError(t, err)

	destination := seedStore(t, filepath.Join(dir, "destination.db"))
	defer func() { require.NoError(t, destination.Close()) }()
	require.NoError(t, destination.UpsertMessage(ctx, store.Message{
		ChannelID: "C1", TS: "123.456", WorkspaceID: "T1", UserID: "U1",
		Text: "higher priority", NormalizedText: "higher priority", SourceRank: 1,
		SourceName: "api-user", RawJSON: `{"source":"api"}`, UpdatedAt: updatedAt,
	}, nil))
	_, err = Import(ctx, destination, opts)
	require.NoError(t, err)
	events, err := destination.QueryReadOnly(ctx, `select count(*) as count from message_events where channel_id = 'C1' and ts = '123.456' and source_name = 'mcp'`)
	require.NoError(t, err)
	require.Equal(t, int64(1), events[0]["count"])
	_, err = source.DB().ExecContext(ctx, `update messages set source_rank = 1 where channel_id = 'C1' and ts = '123.456'`)
	require.NoError(t, err)
	_, err = Export(ctx, source, opts)
	require.NoError(t, err)
	_, err = Import(ctx, destination, opts)
	require.NoError(t, err)
	_, err = source.DB().ExecContext(ctx, `update messages set deleted_ts = '124.000', source_rank = 4 where channel_id = 'C1' and ts = '123.456'`)
	require.NoError(t, err)
	_, err = Export(ctx, source, opts)
	require.NoError(t, err)
	_, err = Import(ctx, destination, opts)
	require.NoError(t, err)
	rows, err := destination.QueryReadOnly(ctx, `
select text, source_rank, coalesce(deleted_ts, '') as deleted_ts,
       (select count(*) from message_files where file_id = 'F-low') as low_files
from messages where channel_id = 'C1' and ts = '123.456'
`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"text": "higher priority", "source_rank": int64(1), "deleted_ts": "", "low_files": int64(0)}}, rows)
}

func TestImportDoesNotCrossLocalRetentionFloor(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	old := time.Unix(123, 456000000).UTC()
	require.NoError(t, source.UpsertMessage(ctx, store.Message{
		ChannelID: "C1", TS: "123.456", WorkspaceID: "T1", UserID: "U1",
		Text: "purged archive", NormalizedText: "purged archive", SourceRank: 2,
		SourceName: "api-bot", RawJSON: `{}`, UpdatedAt: old,
		Files: []store.MessageFile{{FileID: "F1", Name: "purged.txt", RawJSON: `{}`}},
	}, []store.Mention{{Type: "user", TargetID: "U1"}}))
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	_, err := Export(ctx, source, opts)
	require.NoError(t, err)

	destination, err := store.Open(filepath.Join(dir, "destination.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, destination.Close()) }()
	require.NoError(t, destination.SetSyncState(ctx, "retention", "channel_floor", "T1|C1", "200.000000"))
	_, err = Import(ctx, destination, opts)
	require.NoError(t, err)
	rows, err := destination.QueryReadOnly(ctx, `
select (select count(*) from messages) as messages,
       (select count(*) from message_files) as files,
       (select count(*) from message_mentions) as mentions,
       (select count(*) from message_events) as events
`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"messages": int64(0), "files": int64(0), "mentions": int64(0), "events": int64(0)}}, rows)
	matches, err := destination.Search(ctx, "", "purged", 10)
	require.NoError(t, err)
	require.Empty(t, matches)
}

func TestImportRejectsUnseenChildrenBehindNewerParentTombstone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := time.Now().UTC().Truncate(time.Second)
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	require.NoError(t, source.UpsertMessage(ctx, store.Message{
		ChannelID: "C1", TS: "123.456", WorkspaceID: "T1", UserID: "U1",
		Text: "stale", NormalizedText: "stale", SourceRank: 2, SourceName: "api-bot",
		RawJSON: `{}`, UpdatedAt: base,
		Files: []store.MessageFile{{FileID: "F-never-local", Name: "secret.txt", RawJSON: `{}`}},
	}, []store.Mention{{Type: "user", TargetID: "U-never-local"}}))
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	_, err := Export(ctx, source, opts)
	require.NoError(t, err)

	destination := seedStore(t, filepath.Join(dir, "destination.db"))
	defer func() { require.NoError(t, destination.Close()) }()
	require.NoError(t, destination.MarkMessageDeleted(ctx, store.Message{
		ChannelID: "C1", TS: "123.456", WorkspaceID: "T1", DeletedTS: "124.000",
		SourceRank: 2, SourceName: "api-bot", RawJSON: `{"deleted":true}`, UpdatedAt: base.Add(time.Second),
	}, nil))
	_, err = Import(ctx, destination, opts)
	require.NoError(t, err)
	rows, err := destination.QueryReadOnly(ctx, `
select (select count(*) from message_files where file_id = 'F-never-local') as files,
       (select count(*) from message_mentions where target_id = 'U-never-local') as mentions
`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"files": int64(0), "mentions": int64(0)}}, rows)
}

func TestImportPreservesEqualVersionSubordinateTombstones(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	message := store.Message{
		ChannelID: "C1", TS: "600.000", WorkspaceID: "T1", UserID: "U1",
		Text: "deleted children", NormalizedText: "deleted children", SourceRank: 2,
		SourceName: "api-bot", RawJSON: `{}`, UpdatedAt: now,
		Files: []store.MessageFile{{FileID: "F-deleted", Name: "deleted.txt", RawJSON: `{}`}},
	}
	require.NoError(t, source.UpsertMessage(ctx, message, []store.Mention{{Type: "user", TargetID: "U-deleted"}}))
	message.DeletedTS = "601.000"
	message.UpdatedAt = now.Add(time.Second)
	require.NoError(t, source.MarkMessageDeleted(ctx, message, nil))
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	_, err := Export(ctx, source, opts)
	require.NoError(t, err)

	destination, err := store.Open(filepath.Join(dir, "destination.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, destination.Close()) }()
	_, err = Import(ctx, destination, opts)
	require.NoError(t, err)
	rows, err := destination.QueryReadOnly(ctx, `
select (select count(*) from message_files where file_id = 'F-deleted' and deleted_at is not null and deletion_source = 'api-bot') as files,
       (select count(*) from message_mentions where target_id = 'U-deleted' and deleted_at is not null and deletion_source = 'api-bot') as mentions
`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"files": int64(1), "mentions": int64(1)}}, rows)
}

func TestImportRejectsUnseenChildrenBehindNewerActiveParent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := time.Now().UTC().Truncate(time.Second)
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	message := store.Message{
		ChannelID: "C1", TS: "123.456", WorkspaceID: "T1", UserID: "U1",
		Text: "stale child", NormalizedText: "stale child", SourceRank: 2,
		SourceName: "api-bot", RawJSON: `{"version":"old"}`, UpdatedAt: base,
		Files: []store.MessageFile{{FileID: "F-stale", Name: "stale-secret.txt", RawJSON: `{}`}},
	}
	require.NoError(t, source.UpsertMessage(ctx, message, []store.Mention{{Type: "user", TargetID: "U-stale"}}))
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	_, err := Export(ctx, source, opts)
	require.NoError(t, err)

	destination := seedStore(t, filepath.Join(dir, "destination.db"))
	defer func() { require.NoError(t, destination.Close()) }()
	message.Text = "new canonical"
	message.NormalizedText = "new canonical"
	message.RawJSON = `{"version":"new"}`
	message.UpdatedAt = base.Add(time.Second)
	message.Files = nil
	require.NoError(t, destination.UpsertMessage(ctx, message, nil))
	_, err = Import(ctx, destination, opts)
	require.NoError(t, err)
	rows, err := destination.QueryReadOnly(ctx, `
select (select count(*) from message_files where file_id = 'F-stale') as files,
       (select count(*) from message_mentions where target_id = 'U-stale') as mentions
`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"files": int64(0), "mentions": int64(0)}}, rows)
	matches, err := destination.Search(ctx, "", "stale", 10)
	require.NoError(t, err)
	require.Empty(t, matches)
}

func TestRestoreExactlyReplacesSnapshotTables(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	_, err := Export(ctx, source, opts)
	require.NoError(t, err)

	reader := seedStore(t, filepath.Join(dir, "reader.db"))
	defer func() { require.NoError(t, reader.Close()) }()
	require.NoError(t, reader.UpsertMessage(ctx, store.Message{
		ChannelID: "C1", TS: "999.000", WorkspaceID: "T1", Text: "local only",
		NormalizedText: "local only", SourceRank: 3, SourceName: "desktop-indexeddb",
		RawJSON: `{}`, UpdatedAt: time.Now().UTC(),
	}, nil))
	_, err = reader.DB().ExecContext(ctx, `update users set is_deleted = 1 where id = 'U1'`)
	require.NoError(t, err)

	_, err = Restore(ctx, reader, opts)
	require.NoError(t, err)
	rows, err := reader.QueryReadOnly(ctx, `select count(*) as count from messages where channel_id = 'C1' and ts = '999.000'`)
	require.NoError(t, err)
	require.Equal(t, int64(0), rows[0]["count"])
	rows, err = reader.QueryReadOnly(ctx, `select is_deleted from users where id = 'U1'`)
	require.NoError(t, err)
	require.Equal(t, int64(0), rows[0]["is_deleted"])
}

func TestImportMergesVersionedSubordinateTombstonesAndResurrections(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	reader, err := store.Open(filepath.Join(dir, "reader.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	base := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	message := store.Message{
		ChannelID: "C1", TS: "200.000", WorkspaceID: "T1", UserID: "U1",
		Text: "versioned", NormalizedText: "versioned", SourceRank: 2, SourceName: "api-bot",
		RawJSON: `{}`, UpdatedAt: base,
		Files: []store.MessageFile{{FileID: "F1", Name: "one.txt", RawJSON: `{}`}},
	}
	mention := store.Mention{Type: "user", TargetID: "U1", DisplayText: "alice"}
	require.NoError(t, source.UpsertMessage(ctx, message, []store.Mention{mention}))
	_, err = Export(ctx, source, opts)
	require.NoError(t, err)
	_, err = Import(ctx, reader, opts)
	require.NoError(t, err)

	message.UpdatedAt = base.Add(time.Second)
	message.Files = []store.MessageFile{}
	require.NoError(t, source.UpsertMessage(ctx, message, []store.Mention{}))
	_, err = Export(ctx, source, opts)
	require.NoError(t, err)
	_, err = Import(ctx, reader, opts)
	require.NoError(t, err)
	rows, err := reader.QueryReadOnly(ctx, `
select (select count(*) from message_files where channel_id = 'C1' and ts = '200.000' and deleted_at is not null) as files,
       (select count(*) from message_mentions where channel_id = 'C1' and ts = '200.000' and deleted_at is not null) as mentions
`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"files": int64(1), "mentions": int64(1)}}, rows)
	matches, err := reader.Search(ctx, "", "one", 10)
	require.NoError(t, err)
	require.Empty(t, matches)

	message.UpdatedAt = base.Add(2 * time.Second)
	message.Files = []store.MessageFile{{FileID: "F1", Name: "one.txt", RawJSON: `{}`}}
	require.NoError(t, source.UpsertMessage(ctx, message, []store.Mention{mention}))
	_, err = Export(ctx, source, opts)
	require.NoError(t, err)
	_, err = Import(ctx, reader, opts)
	require.NoError(t, err)
	rows, err = reader.QueryReadOnly(ctx, `
select (select count(*) from message_files where channel_id = 'C1' and ts = '200.000' and deleted_at is null) as files,
       (select count(*) from message_mentions where channel_id = 'C1' and ts = '200.000' and deleted_at is null) as mentions
`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"files": int64(1), "mentions": int64(1)}}, rows)
	matches, err = reader.Search(ctx, "", "one", 10)
	require.NoError(t, err)
	require.Len(t, matches, 1)
}

func TestImportRebuildsMessageEventHeadsFromMergedHistory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	reader := seedStore(t, filepath.Join(dir, "reader.db"))
	defer func() { require.NoError(t, reader.Close()) }()
	message := store.Message{
		ChannelID: "C1", TS: "123.456", WorkspaceID: "T1", UserID: "U1",
		Text: "new", NormalizedText: "new", SourceRank: 2, SourceName: "api-bot",
		RawJSON: `{"version":"new"}`, UpdatedAt: time.Now().UTC().Add(time.Hour),
	}
	require.NoError(t, source.UpsertMessage(ctx, message, nil))
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	_, err := Export(ctx, source, opts)
	require.NoError(t, err)
	_, err = Import(ctx, reader, opts)
	require.NoError(t, err)
	rows, err := reader.QueryReadOnly(ctx, `select payload_json from message_event_heads where channel_id = 'C1' and ts = '123.456' and event_type = 'message' and source_name = 'api-bot'`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"payload_json": `{"version":"new"}`}}, rows)

	message.Text = "reverted"
	message.NormalizedText = "reverted"
	message.RawJSON = `{}`
	message.UpdatedAt = message.UpdatedAt.Add(time.Second)
	require.NoError(t, reader.UpsertMessage(ctx, message, nil))
	rows, err = reader.QueryReadOnly(ctx, `select payload_json from message_events where channel_id = 'C1' and ts = '123.456' order by created_at, id`)
	require.NoError(t, err)
	require.Len(t, rows, 4)
	require.Equal(t, `{}`, rows[3]["payload_json"])
	_, err = Import(ctx, reader, opts)
	require.NoError(t, err)
	rows, err = reader.QueryReadOnly(ctx, `select payload_json from message_events where channel_id = 'C1' and ts = '123.456' order by created_at, id`)
	require.NoError(t, err)
	require.Len(t, rows, 4)
}

func TestImportPreservesCompactedNewerEventHead(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	_, err := source.DB().ExecContext(ctx, `
delete from message_events;
insert into message_events (channel_id, ts, event_type, source_name, payload_json, created_at)
values ('C1', '123.456', 'message', 'api-bot', '{"stale":true}', '2020-01-01T00:00:00Z');
update messages set raw_json = '{"stale":true}', updated_at = '2020-01-01T00:00:00Z'
where channel_id = 'C1' and ts = '123.456';
`)
	require.NoError(t, err)
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	_, err = Export(ctx, source, opts)
	require.NoError(t, err)

	destination := seedStore(t, filepath.Join(dir, "destination.db"))
	defer func() { require.NoError(t, destination.Close()) }()
	_, err = destination.DB().ExecContext(ctx, `delete from message_events`)
	require.NoError(t, err)
	_, err = Import(ctx, destination, opts)
	require.NoError(t, err)
	rows, err := destination.QueryReadOnly(ctx, `select payload_json from message_event_heads where channel_id = 'C1' and ts = '123.456' and event_type = 'message' and source_name = 'api-bot'`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"payload_json": `{}`}}, rows)
}

func TestImportPreservesAmbiguousCompactedEventHeadAcrossPayloadReversion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	_, err := source.DB().ExecContext(ctx, `
insert into message_events (channel_id, ts, event_type, source_name, payload_json, created_at) values
  ('C1', '123.456', 'message_changed', 'api-bot', '{"state":"A"}', '2026-07-17T00:00:00Z'),
  ('C1', '123.456', 'message_changed', 'api-bot', '{"state":"B"}', '2026-07-17T00:00:01Z');
insert into message_event_heads (channel_id, ts, event_type, source_name, payload_json)
values ('C1', '123.456', 'message_changed', 'api-bot', '{"state":"B"}')
on conflict(channel_id, ts, event_type, source_name) do update set payload_json = excluded.payload_json;
`)
	require.NoError(t, err)
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	_, err = Export(ctx, source, opts)
	require.NoError(t, err)

	destination := seedStore(t, filepath.Join(dir, "destination.db"))
	defer func() { require.NoError(t, destination.Close()) }()
	_, err = destination.DB().ExecContext(ctx, `
delete from message_events where channel_id = 'C1' and ts = '123.456';
insert into message_event_heads (channel_id, ts, event_type, source_name, payload_json)
values ('C1', '123.456', 'message_changed', 'api-bot', '{"state":"A"}')
on conflict(channel_id, ts, event_type, source_name) do update set payload_json = excluded.payload_json;
`)
	require.NoError(t, err)
	_, err = Import(ctx, destination, opts)
	require.NoError(t, err)
	rows, err := destination.QueryReadOnly(ctx, `select payload_json from message_event_heads where channel_id = 'C1' and ts = '123.456' and event_type = 'message_changed' and source_name = 'api-bot'`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"payload_json": `{"state":"A"}`}}, rows)
}

func TestCompareSnapshotTimesPreservesNanosecondsAndOffsets(t *testing.T) {
	require.Equal(t, -1, compareSnapshotTimes("2026-07-17T12:00:00.000000100Z", "2026-07-17T12:00:00.000000200Z"))
	require.Equal(t, 1, compareSnapshotTimes("2026-07-17T05:00:00.000000300-07:00", "2026-07-17T12:00:00.000000200Z"))
	require.Zero(t, compareSnapshotTimes("2026-07-17T05:00:00-07:00", "2026-07-17T12:00:00Z"))
}

func TestRestorePreservesDistinctLegacyMessageEvents(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	manifest, err := Export(ctx, source, opts)
	require.NoError(t, err)
	for i := range manifest.Tables {
		if manifest.Tables[i].Name != "message_events" {
			continue
		}
		manifest.Tables[i].Columns = withoutString(manifest.Tables[i].Columns, "event_key")
		filePath := filepath.Join(opts.RepoPath, filepath.FromSlash(manifest.Tables[i].Files[0]))
		file, err := os.Open(filePath)
		require.NoError(t, err)
		reader, err := gzip.NewReader(file)
		require.NoError(t, err)
		body, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		require.NoError(t, file.Close())
		var duplicate map[string]any
		require.NoError(t, json.Unmarshal(bytes.TrimSpace(body), &duplicate))
		delete(duplicate, "event_key")
		duplicate["id"] = 999
		bodyRows := bytes.Split(bytes.TrimSpace(body), []byte{'\n'})
		for i := range bodyRows {
			var row map[string]any
			require.NoError(t, json.Unmarshal(bodyRows[i], &row))
			delete(row, "event_key")
			bodyRows[i], err = json.Marshal(row)
			require.NoError(t, err)
		}
		body = append(bytes.Join(bodyRows, []byte{'\n'}), '\n')
		duplicateBody, err := json.Marshal(duplicate)
		require.NoError(t, err)
		duplicateBody = append(duplicateBody, '\n')
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		_, err = writer.Write(append(append([]byte{}, body...), duplicateBody...))
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		require.NoError(t, os.WriteFile(filePath, compressed.Bytes(), 0o600))
		manifest.Tables[i].Rows *= 2
	}
	writeManifest(t, opts.RepoPath, manifest)

	destination, err := store.Open(filepath.Join(dir, "destination.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, destination.Close()) }()
	_, err = Restore(ctx, destination, opts)
	require.NoError(t, err)
	rows, err := destination.QueryReadOnly(ctx, `select count(*) as count, min(id) as min_id from message_events`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"count": int64(2), "min_id": int64(1)}}, rows)
}

func TestLegacySnapshotBackfillsDeletedSubordinates(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	now := time.Now().UTC().Add(time.Hour)
	message := store.Message{
		ChannelID: "C1", TS: "300.000", WorkspaceID: "T1", UserID: "U1",
		Text: "deleted legacy", NormalizedText: "deleted legacy", SourceRank: 2,
		SourceName: "api-bot", RawJSON: `{}`, UpdatedAt: now,
		Files: []store.MessageFile{{FileID: "F-legacy", Name: "confidential.txt", RawJSON: `{}`}},
	}
	require.NoError(t, source.UpsertMessage(ctx, message, []store.Mention{{Type: "user", TargetID: "U1"}}))
	message.DeletedTS = "301.000"
	message.UpdatedAt = now.Add(time.Second)
	require.NoError(t, source.MarkMessageDeleted(ctx, message, nil))
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	manifest, err := Export(ctx, source, opts)
	require.NoError(t, err)
	for i := range manifest.Tables {
		switch manifest.Tables[i].Name {
		case "message_files":
			for _, column := range []string{"deleted_at", "deletion_source", "deletion_reason"} {
				manifest.Tables[i].Columns = withoutString(manifest.Tables[i].Columns, column)
			}
			rewriteSnapshotTableRows(t, opts.RepoPath, manifest.Tables[i], func(row map[string]any) {
				delete(row, "deleted_at")
				delete(row, "deletion_source")
				delete(row, "deletion_reason")
			})
		case "message_mentions":
			for _, column := range []string{"deleted_at", "deletion_source", "deletion_reason", "updated_at"} {
				manifest.Tables[i].Columns = withoutString(manifest.Tables[i].Columns, column)
			}
			rewriteSnapshotTableRows(t, opts.RepoPath, manifest.Tables[i], func(row map[string]any) {
				delete(row, "deleted_at")
				delete(row, "deletion_source")
				delete(row, "deletion_reason")
				delete(row, "updated_at")
			})
		}
	}
	writeManifest(t, opts.RepoPath, manifest)

	destination, err := store.Open(filepath.Join(dir, "destination.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, destination.Close()) }()
	_, err = Restore(ctx, destination, opts)
	require.NoError(t, err)
	rows, err := destination.QueryReadOnly(ctx, `
select (select count(*) from message_files where file_id = 'F-legacy' and deletion_reason = 'parent_message_deleted') as files,
       (select count(*) from message_mentions where channel_id = 'C1' and ts = '300.000' and deletion_reason = 'parent_message_deleted') as mentions
`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"files": int64(1), "mentions": int64(1)}}, rows)
	matches, err := destination.Search(ctx, "", "confidential", 10)
	require.NoError(t, err)
	require.Empty(t, matches)

	mergeDestination, err := store.Open(filepath.Join(dir, "merge-destination.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, mergeDestination.Close()) }()
	_, err = Import(ctx, mergeDestination, opts)
	require.NoError(t, err)
	rows, err = mergeDestination.QueryReadOnly(ctx, `
select (select count(*) from message_files where file_id = 'F-legacy' and deletion_reason = 'parent_message_deleted' and deletion_source = 'api-bot') as files,
       (select count(*) from message_mentions where channel_id = 'C1' and ts = '300.000' and deletion_reason = 'parent_message_deleted' and deletion_source = 'api-bot') as mentions
`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"files": int64(1), "mentions": int64(1)}}, rows)
	matches, err = mergeDestination.Search(ctx, "", "confidential", 10)
	require.NoError(t, err)
	require.Empty(t, matches)
}

func TestImportVersionsLegacyMentionsFromParent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	message := store.Message{
		ChannelID: "C1", TS: "400.000", WorkspaceID: "T1", UserID: "U1",
		Text: "active mention", NormalizedText: "active mention", SourceRank: 2,
		SourceName: "api-bot", RawJSON: `{}`, UpdatedAt: base.Add(2 * time.Second),
	}
	mention := store.Mention{Type: "user", TargetID: "U1"}
	require.NoError(t, source.UpsertMessage(ctx, message, []store.Mention{mention}))
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	manifest, err := Export(ctx, source, opts)
	require.NoError(t, err)
	for i := range manifest.Tables {
		if manifest.Tables[i].Name != "message_mentions" {
			continue
		}
		for _, column := range []string{"deleted_at", "deletion_source", "deletion_reason", "updated_at"} {
			manifest.Tables[i].Columns = withoutString(manifest.Tables[i].Columns, column)
		}
		rewriteSnapshotTableRows(t, opts.RepoPath, manifest.Tables[i], func(row map[string]any) {
			delete(row, "deleted_at")
			delete(row, "deletion_source")
			delete(row, "deletion_reason")
			delete(row, "updated_at")
		})
	}
	writeManifest(t, opts.RepoPath, manifest)

	destination := seedStore(t, filepath.Join(dir, "destination.db"))
	defer func() { require.NoError(t, destination.Close()) }()
	message.UpdatedAt = base
	require.NoError(t, destination.UpsertMessage(ctx, message, []store.Mention{mention}))
	message.UpdatedAt = base.Add(time.Second)
	require.NoError(t, destination.UpsertMessage(ctx, message, []store.Mention{}))
	_, err = Import(ctx, destination, opts)
	require.NoError(t, err)
	rows, err := destination.QueryReadOnly(ctx, `select count(*) as count from message_mentions where channel_id = 'C1' and ts = '400.000' and deleted_at is null`)
	require.NoError(t, err)
	require.Equal(t, int64(1), rows[0]["count"])
}

func TestImportRejectsIncompleteManifestBeforeClearing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	manifest, err := Export(ctx, source, opts)
	require.NoError(t, err)
	manifest.Tables = manifest.Tables[:1]
	writeManifest(t, opts.RepoPath, manifest)

	reader := seedStore(t, filepath.Join(dir, "reader.db"))
	defer func() { require.NoError(t, reader.Close()) }()

	_, err = Import(ctx, reader, opts)
	require.ErrorContains(t, err, "manifest missing table")
	assertArchiveStillPresent(t, ctx, reader)
}

func TestImportRejectsTableRowCountMismatch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	manifest, err := Export(ctx, source, opts)
	require.NoError(t, err)
	for i := range manifest.Tables {
		if manifest.Tables[i].Name == "messages" {
			manifest.Tables[i].Rows++
			break
		}
	}
	writeManifest(t, opts.RepoPath, manifest)

	reader := seedStore(t, filepath.Join(dir, "reader.db"))
	defer func() { require.NoError(t, reader.Close()) }()

	_, err = Import(ctx, reader, opts)
	require.ErrorContains(t, err, "row count mismatch")
	assertArchiveStillPresent(t, ctx, reader)
}

func TestImportRejectsEscapedManifestTablePath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	manifest, err := Export(ctx, source, opts)
	require.NoError(t, err)
	for i := range manifest.Tables {
		if manifest.Tables[i].Name == "messages" {
			manifest.Tables[i].Files = []string{"../outside.jsonl.gz"}
			manifest.Tables[i].File = ""
			break
		}
	}
	writeManifest(t, opts.RepoPath, manifest)

	reader := seedStore(t, filepath.Join(dir, "reader.db"))
	defer func() { require.NoError(t, reader.Close()) }()

	_, err = Import(ctx, reader, opts)
	require.ErrorContains(t, err, "path escapes share repo")
	assertArchiveStillPresent(t, ctx, reader)
}

func TestImportRejectsSymlinkedManifestTableDir(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	_, err := Export(ctx, source, opts)
	require.NoError(t, err)

	outside := filepath.Join(dir, "outside-messages")
	require.NoError(t, os.MkdirAll(outside, 0o750))
	tableDir := filepath.Join(opts.RepoPath, "tables", "messages")
	require.NoError(t, os.RemoveAll(tableDir))
	if err := os.Symlink(outside, tableDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	reader := seedStore(t, filepath.Join(dir, "reader.db"))
	defer func() { require.NoError(t, reader.Close()) }()

	_, err = Import(ctx, reader, opts)
	require.ErrorContains(t, err, "path escapes share repo")
	assertArchiveStillPresent(t, ctx, reader)
}

func TestPullPreservesLocalCommitsAheadOfOrigin(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	remoteWork := filepath.Join(dir, "remote-work")
	remoteRepo := filepath.Join(dir, "remote.git")
	shareRepo := filepath.Join(dir, "share")

	require.NoError(t, testGitRun(ctx, "", "init", "-b", "main", remoteWork))
	require.NoError(t, os.WriteFile(filepath.Join(remoteWork, "manifest.json"), []byte("{}\n"), 0o600))
	testGitCommit(t, ctx, remoteWork, "seed")
	require.NoError(t, testGitRun(ctx, "", "clone", "--bare", remoteWork, remoteRepo))

	opts := Options{RepoPath: shareRepo, Remote: remoteRepo, Branch: "main"}
	require.NoError(t, Pull(ctx, opts))
	require.NoError(t, os.WriteFile(filepath.Join(shareRepo, "local.txt"), []byte("local\n"), 0o600))
	testGitCommit(t, ctx, shareRepo, "local")
	localHead, err := testGitOutput(ctx, shareRepo, "rev-parse", "HEAD")
	require.NoError(t, err)
	originHead, err := testGitOutput(ctx, shareRepo, "rev-parse", "origin/main")
	require.NoError(t, err)
	require.NotEqual(t, strings.TrimSpace(originHead), strings.TrimSpace(localHead))

	require.NoError(t, Pull(ctx, opts))
	afterHead, err := testGitOutput(ctx, shareRepo, "rev-parse", "HEAD")
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(localHead), strings.TrimSpace(afterHead))
}

func TestPullInitializesRequestedRemoteBranchOnClone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	remoteWork := filepath.Join(dir, "remote-work")
	remoteRepo := filepath.Join(dir, "remote.git")
	shareRepo := filepath.Join(dir, "share")

	require.NoError(t, testGitRun(ctx, "", "init", "-b", "main", remoteWork))
	require.NoError(t, os.WriteFile(filepath.Join(remoteWork, "manifest.json"), []byte("release\n"), 0o600))
	testGitCommit(t, ctx, remoteWork, "release")
	require.NoError(t, testGitRun(ctx, remoteWork, "branch", "release"))
	require.NoError(t, os.WriteFile(filepath.Join(remoteWork, "manifest.json"), []byte("main\n"), 0o600))
	testGitCommit(t, ctx, remoteWork, "main")
	require.NoError(t, testGitRun(ctx, "", "clone", "--bare", remoteWork, remoteRepo))

	opts := Options{RepoPath: shareRepo, Remote: remoteRepo, Branch: "release"}
	require.NoError(t, Pull(ctx, opts))
	body, err := os.ReadFile(filepath.Join(shareRepo, "manifest.json"))
	require.NoError(t, err)
	require.Equal(t, "release\n", string(body))
}

func TestImportIfChangedSkipsCurrentManifest(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()

	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	manifest, err := Export(ctx, source, opts)
	require.NoError(t, err)

	reader, err := store.Open(filepath.Join(dir, "reader.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()

	imported, changed, err := ImportIfChanged(ctx, reader, opts)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, manifest.GeneratedAt.UTC().Format(time.RFC3339Nano), imported.GeneratedAt.UTC().Format(time.RFC3339Nano))

	_, changed, err = ImportIfChanged(ctx, reader, opts)
	require.NoError(t, err)
	require.False(t, changed)
}

func TestImportAtRestoresTaggedSnapshotWithoutMovingCheckout(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()

	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main", Tag: "snapshot-old"}
	_, err := Export(ctx, source, opts)
	require.NoError(t, err)
	committed, err := Commit(ctx, opts, "old snapshot")
	require.NoError(t, err)
	require.True(t, committed)
	tag, err := CreateImmutableTag(ctx, opts)
	require.NoError(t, err)
	require.Equal(t, "snapshot-old", tag)

	_, err = source.DB().ExecContext(ctx, `update messages set text = 'new snapshot', normalized_text = 'new snapshot'`)
	require.NoError(t, err)
	opts.Tag = ""
	_, err = Export(ctx, source, opts)
	require.NoError(t, err)
	committed, err = Commit(ctx, opts, "new snapshot")
	require.NoError(t, err)
	require.True(t, committed)
	headBefore, err := testGitOutput(ctx, opts.RepoPath, "rev-parse", "HEAD")
	require.NoError(t, err)

	reader, err := store.Open(filepath.Join(dir, "reader.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	manifest, err := RestoreAt(ctx, reader, opts, "snapshot-old")
	require.NoError(t, err)
	require.False(t, manifest.GeneratedAt.IsZero())
	rows, err := reader.Search(ctx, "", "archive", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "git backed archive works", rows[0].Text)
	headAfter, err := testGitOutput(ctx, opts.RepoPath, "rev-parse", "HEAD")
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(headBefore), strings.TrimSpace(headAfter))
}

func TestExportImportRestoresMediaFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	body := []byte("cached file")
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	mediaPath := "files/" + hash[:2] + "/" + hash + "-incident.txt"
	fullPath, err := media.LocalPath(cacheDir, mediaPath)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o750))
	require.NoError(t, os.WriteFile(fullPath, body, 0o600))

	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	require.NoError(t, source.UpsertChannel(ctx, store.Channel{
		ID:          "C1",
		WorkspaceID: "T1",
		Name:        "general",
		Kind:        "desktop_channel",
		RawJSON:     "{}",
		UpdatedAt:   time.Now().UTC(),
	}))
	require.NoError(t, source.UpsertMessage(ctx, store.Message{
		ChannelID:      "C1",
		TS:             "123.789",
		WorkspaceID:    "T1",
		UserID:         "U1",
		Text:           "media backup",
		NormalizedText: "media backup incident.txt",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      time.Now().UTC(),
		Files: []store.MessageFile{{
			FileID:        "F1",
			Name:          "incident.txt",
			Mimetype:      "text/plain",
			MediaPath:     mediaPath,
			ContentSHA256: hash,
			ContentSize:   int64(len(body)),
			FetchStatus:   "fetched",
			RawJSON:       "{}",
		}},
	}, nil))

	opts := Options{RepoPath: filepath.Join(dir, "share"), CacheDir: cacheDir, Branch: "main", IncludeMedia: true}
	manifest, err := Export(ctx, source, opts)
	require.NoError(t, err)
	require.NotNil(t, manifest.Media)
	require.Len(t, manifest.Media.Items, 1)
	require.FileExists(t, filepath.Join(opts.RepoPath, filepath.FromSlash(manifest.Media.Items[0].Path)))

	reader, err := store.Open(filepath.Join(dir, "reader.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	dstCache := filepath.Join(dir, "dst-cache")
	imported, err := Import(ctx, reader, Options{RepoPath: opts.RepoPath, CacheDir: dstCache, Branch: "main", IncludeMedia: false})
	require.NoError(t, err)
	require.NotNil(t, imported.Media)
	rows, err := reader.Files(ctx, store.FileListOptions{FileID: "F1"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Empty(t, rows[0].MediaPath)

	imported, changed, err := ImportIfChanged(ctx, reader, Options{RepoPath: opts.RepoPath, CacheDir: dstCache, Branch: "main", IncludeMedia: true})
	require.NoError(t, err)
	require.False(t, changed)
	require.NotNil(t, imported.Media)
	rows, err = reader.Files(ctx, store.FileListOptions{FileID: "F1"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, mediaPath, rows[0].MediaPath)
	dstPath, err := media.LocalPath(dstCache, mediaPath)
	require.NoError(t, err)
	require.FileExists(t, dstPath)

	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- media.WithCacheLock(ctx, dstCache, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, _, err = ImportIfChanged(waitCtx, reader, Options{
		RepoPath: opts.RepoPath, CacheDir: dstCache, Branch: "main", IncludeMedia: true,
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	close(release)
	require.NoError(t, <-lockDone)
}

func TestMergeClearsUnmanifestedIncomingMediaAndPreservesLocalCache(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := time.Now().UTC().Truncate(time.Second)
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	message := store.Message{
		ChannelID: "C1", TS: "500.000", WorkspaceID: "T1", UserID: "U1",
		Text: "shared media", NormalizedText: "shared media", SourceRank: 2,
		SourceName: "api-bot", RawJSON: `{}`, UpdatedAt: base.Add(time.Second),
		Files: []store.MessageFile{{
			FileID: "F-shared", Name: "shared.txt", MediaPath: "files/source-only.txt",
			ContentSHA256: "source-hash", ContentSize: 20, FetchStatus: "fetched", RawJSON: `{}`,
		}},
	}
	require.NoError(t, source.UpsertMessage(ctx, message, nil))
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	manifest, err := Export(ctx, source, opts)
	require.NoError(t, err)
	require.Nil(t, manifest.Media)

	destination := seedStore(t, filepath.Join(dir, "destination.db"))
	defer func() { require.NoError(t, destination.Close()) }()
	message.UpdatedAt = base
	message.Files[0].MediaPath = "files/local-cache.txt"
	message.Files[0].ContentSHA256 = "local-hash"
	message.Files[0].ContentSize = 10
	require.NoError(t, destination.UpsertMessage(ctx, message, nil))
	require.NoError(t, destination.UpsertMessage(ctx, store.Message{
		ChannelID: "C1", TS: "501.000", WorkspaceID: "T1", UserID: "U1",
		Text: "local media", NormalizedText: "local media", SourceRank: 2,
		SourceName: "api-bot", RawJSON: `{}`, UpdatedAt: base,
		Files: []store.MessageFile{{
			FileID: "F-local", Name: "local.txt", MediaPath: "files/local-only.txt",
			ContentSHA256: "local-only-hash", ContentSize: 30, FetchStatus: "fetched", RawJSON: `{}`,
		}},
	}, nil))

	_, err = Import(ctx, destination, Options{RepoPath: opts.RepoPath, CacheDir: filepath.Join(dir, "cache"), Branch: "main", IncludeMedia: true})
	require.NoError(t, err)
	rows, err := destination.QueryReadOnly(ctx, `
select file_id, coalesce(media_path, '') as media_path, coalesce(content_sha256, '') as content_sha256
from message_files
where file_id in ('F-shared', 'F-local')
order by file_id
`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{
		{"file_id": "F-local", "media_path": "files/local-only.txt", "content_sha256": "local-only-hash"},
		{"file_id": "F-shared", "media_path": "files/local-cache.txt", "content_sha256": "local-hash"},
	}, rows)
}

func TestNeedsImportUsesLastImportTime(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, s.Close()) }()

	require.True(t, NeedsImport(ctx, s, time.Hour))

	require.NoError(t, s.SetSyncState(ctx, importSyncSource, importSyncEntityType, lastImportEntityID, time.Now().UTC().Format(time.RFC3339Nano)))
	require.False(t, NeedsImport(ctx, s, time.Hour))
	require.NoError(t, s.SetSyncState(ctx, importSyncSource, importSyncEntityType, lastImportEntityID, time.Now().UTC().Add(-2*time.Hour).Format(time.RFC3339Nano)))
	require.True(t, NeedsImport(ctx, s, time.Hour))
}

func writeManifest(t *testing.T, repoPath string, manifest Manifest) {
	t.Helper()
	body, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	body = append(body, '\n')
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, ManifestName), body, 0o600))
}

func rewriteSnapshotTableRows(t *testing.T, repoPath string, table TableManifest, transform func(map[string]any)) {
	t.Helper()
	for _, relative := range tableManifestFiles(table) {
		filePath := filepath.Join(repoPath, filepath.FromSlash(relative))
		file, err := os.Open(filePath)
		require.NoError(t, err)
		reader, err := gzip.NewReader(file)
		require.NoError(t, err)
		decoder := json.NewDecoder(reader)
		decoder.UseNumber()
		var rows []map[string]any
		for {
			row := map[string]any{}
			err := decoder.Decode(&row)
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			transform(row)
			rows = append(rows, row)
		}
		require.NoError(t, reader.Close())
		require.NoError(t, file.Close())
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		encoder := json.NewEncoder(writer)
		for _, row := range rows {
			require.NoError(t, encoder.Encode(row))
		}
		require.NoError(t, writer.Close())
		require.NoError(t, os.WriteFile(filePath, compressed.Bytes(), 0o600))
	}
}

func assertArchiveStillPresent(t *testing.T, ctx context.Context, s *store.Store) {
	t.Helper()
	rows, err := s.Search(ctx, "", "archive", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "git backed archive works", rows[0].Text)
}

func testGitCommit(t *testing.T, ctx context.Context, repoPath string, message string) {
	t.Helper()
	require.NoError(t, testGitRun(ctx, repoPath, "add", "."))
	require.NoError(t, testGitRun(ctx, repoPath,
		"-c", "commit.gpgsign=false",
		"-c", "user.name=slacrawl-test",
		"-c", "user.email=slacrawl-test@example.invalid",
		"commit", "-m", message,
	))
}

func testGitRun(ctx context.Context, dir string, args ...string) error {
	_, err := testGitOutput(ctx, dir, args...)
	return err
}

func testGitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	body, err := cmd.CombinedOutput()
	if err != nil {
		return string(body), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

func seedStore(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(path)
	require.NoError(t, err)

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s.UpsertWorkspace(ctx, store.Workspace{
		ID:        "T1",
		Name:      "team",
		RawJSON:   "{}",
		UpdatedAt: now,
	}))
	require.NoError(t, s.UpsertChannel(ctx, store.Channel{
		ID:          "C1",
		WorkspaceID: "T1",
		Name:        "general",
		Kind:        "public_channel",
		RawJSON:     "{}",
		UpdatedAt:   now,
	}))
	require.NoError(t, s.UpsertUser(ctx, store.User{
		ID:          "U1",
		WorkspaceID: "T1",
		Name:        "alice",
		RawJSON:     "{}",
		UpdatedAt:   now,
	}))
	require.NoError(t, s.UpsertMessage(ctx, store.Message{
		ChannelID:      "C1",
		TS:             "123.456",
		WorkspaceID:    "T1",
		UserID:         "U1",
		Text:           "git backed archive works",
		NormalizedText: "git backed archive works",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      now,
	}, []store.Mention{{Type: "user", TargetID: "U1", DisplayText: "alice"}}))
	require.NoError(t, s.SetSyncState(ctx, "api-bot", "workspace", "T1", now.Format(time.RFC3339Nano)))
	return s
}
