package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func seedRequest(request NativeCaptureRequest) NativeSeedRequest {
	return NativeSeedRequest{File: request.Baseline.File, Scope: request.Scope, ScopeSHA256: request.ScopeSHA256,
		Limits: NativeSeedLimits{MaxRows: 1000, MaxBytes: 1 << 20}}
}

func seedTable(t *testing.T, seed NativeSeed, name NativeTable) NativeSeedTable {
	t.Helper()
	for _, table := range seed.Tables {
		if table.Table == name {
			return table
		}
	}
	t.Fatalf("missing seed table %s", name)
	return NativeSeedTable{}
}

func seedField(t *testing.T, table NativeSeedTable, row int, column string) NativeValue {
	t.Helper()
	for i, name := range table.Columns {
		if name == column {
			return table.Rows[row][i]
		}
	}
	t.Fatalf("missing seed column %s", column)
	return NativeValue{}
}

func TestNativeSeedIncludesPreJournalRowsAtZeroHead(t *testing.T) {
	_, _, capture, ctx := captureFixture(t)
	path := filepath.Join(t.TempDir(), "v8.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(schema + "; pragma user_version = 8;")
	require.NoError(t, err)
	_, err = db.Exec(`insert into workspaces values ('T1','retained',null,null,'{}','2026-01-01T00:00:00Z');
 insert into channels(id,workspace_id,name,kind,raw_json,updated_at) values ('C1','T1','general','public_channel',?,'2026-01-01T00:00:00Z');
 insert into messages(channel_id,ts,workspace_id,text,normalized_text,source_rank,source_name,raw_json,updated_at)
 values ('C1','1.000001','T1','retained before journal','retained before journal',2,'api-bot','{}','2026-01-01T00:00:00Z');`, capturePublicRaw)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	st, err := Open(path)
	require.NoError(t, err)
	status, err := st.Status(ctx)
	require.NoError(t, err)
	require.Zero(t, status.NativeChanges.HeadSequence)
	require.NoError(t, st.Close())
	request := seedRequest(capture)
	request.File, err = NativeArchiveFileIdentity(path)
	require.NoError(t, err)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	seed, err := CaptureNativeSeed(ctx, path, request)
	require.NoError(t, err)
	require.Equal(t, NativeCursor{status.NativeChanges.GenerationID, 0, status.NativeChanges.HeadEntryID}, seed.Through)
	require.Equal(t, request.File, seed.File)
	require.Equal(t, request.ScopeSHA256, seed.ScopeSHA256)
	require.Len(t, seed.Tables, 6)
	require.Equal(t, "retained before journal", seedField(t, seedTable(t, seed, NativeMessages), 0, "text").Text)
	for _, name := range []NativeTable{NativeUsers, NativeMessageFiles, NativeMessageMentions} {
		table := seedTable(t, seed, name)
		require.Empty(t, table.Rows)
		require.NotEmpty(t, table.Columns)
	}
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256(before), sha256.Sum256(after))
}

func TestNativeSeedScopeTypesAndOrdering(t *testing.T) {
	st, path, capture, ctx := captureFixture(t)
	request := seedRequest(capture)
	now := time.Unix(1710000100, 0).UTC()
	message := batchMessage("C1", "2.000002", "T1", "snowman ☃\x00tail", now)
	message.Files = []MessageFile{{FileID: "F1", RawJSON: "{ \"opaque\": true }", MediaPath: "local-cache/file", ContentSize: 9223372036854775807}}
	require.NoError(t, st.UpsertMessage(ctx, message, []Mention{{Type: "user", TargetID: "U1"}}))
	_, err := st.DB().Exec(`update messages set user_id='',edited_ts=null,updated_at='2026-01-01T00:00:00.123000Z' where ts='2.000002'`)
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessage(ctx, batchMessage("C1", "10.000010", "T1", "binary order", now), nil))
	seedBatchCatalog(t, st, "T2", "C2", "U2", now)
	require.NoError(t, st.UpsertMessage(ctx, batchMessage("C2", "1.000001", "T2", "foreign", now), nil))
	require.NoError(t, st.SetSyncState(ctx, "provider:fixture", "workspace", "T1", "private acquisition cursor"))
	// Mentions inherit the admitted channel only when no foreign parent exists.
	_, err = st.DB().Exec(`insert into message_mentions(channel_id,ts,mention_type,target_id) values ('C1','orphan','user','U1');
 insert into messages(channel_id,ts,workspace_id,text,normalized_text,source_rank,source_name,raw_json,updated_at)
 values ('C1','foreign','T2','foreign body','foreign body',1,'fixture','{}','now');
 insert into message_mentions(channel_id,ts,mention_type,target_id) values ('C1','foreign','user','U2');`)
	require.NoError(t, err)
	before := journalRows(t, st, 0)
	seed, err := CaptureNativeSeed(ctx, path, request)
	require.NoError(t, err)
	require.Empty(t, seedTable(t, seed, NativeUsers).Rows)
	messages := seedTable(t, seed, NativeMessages)
	require.Len(t, messages.Rows, 3)
	require.Equal(t, "10.000010", seedField(t, messages, 1, "ts").Text)
	require.Equal(t, NativeValue{Kind: NativeText, Text: "snowman ☃\x00tail"}, seedField(t, messages, 2, "text"))
	require.Equal(t, NativeValue{Kind: NativeText}, seedField(t, messages, 2, "user_id"))
	require.Equal(t, NativeValue{Kind: NativeNull}, seedField(t, messages, 2, "edited_ts"))
	require.Equal(t, "2026-01-01T00:00:00.123000Z", seedField(t, messages, 2, "updated_at").Text)
	files := seedTable(t, seed, NativeMessageFiles)
	require.Equal(t, int64(9223372036854775807), seedField(t, files, 0, "content_size").Integer)
	require.Equal(t, "local-cache/file", seedField(t, files, 0, "media_path").Text, "seed is local data, not an egress payload")
	require.Equal(t, "{ \"opaque\": true }", seedField(t, files, 0, "raw_json").Text)
	require.Len(t, seedTable(t, seed, NativeMessageMentions).Rows, 2)
	require.Equal(t, before, journalRows(t, st, 0))

	request.Scope.IncludeWorkspaceUsers = true
	request.ScopeSHA256, err = NativeCaptureScopeSHA256(request.Scope)
	require.NoError(t, err)
	// Nullable native text primary keys can have multiple physical rows.
	_, err = st.DB().Exec(`insert into users(id,workspace_id,name,raw_json,updated_at) values
 (null,'T1','first','{}','now'),(null,'T1','second','{}','now')`)
	require.NoError(t, err)
	seed, err = CaptureNativeSeed(ctx, path, request)
	require.NoError(t, err)
	users := seedTable(t, seed, NativeUsers)
	require.Len(t, users.Rows, 3)
	require.Equal(t, NativeValue{Kind: NativeNull}, seedField(t, users, 0, "id"))
	require.Equal(t, "first", seedField(t, users, 0, "name").Text)
	require.Equal(t, "second", seedField(t, users, 1, "name").Text)
}

func TestNativeSeedUsesOneSQLiteSnapshot(t *testing.T) {
	st, path, capture, ctx := captureFixture(t)
	request := seedRequest(capture)
	reader, err := OpenReadOnly(path)
	require.NoError(t, err)
	defer reader.Close()
	tx, err := reader.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer tx.Rollback()
	pinned, err := readNativeChangeStatus(ctx, tx)
	require.NoError(t, err)
	require.NoError(t, st.RenameChannel(ctx, "T1", "C1", "after snapshot"))
	require.NoError(t, st.UpsertMessage(ctx, batchMessage("C1", "1.000001", "T1", "after snapshot", time.Now()), nil))
	seed, err := readNativeSeed(ctx, tx, request)
	require.NoError(t, err)
	require.Equal(t, NativeCursor{pinned.GenerationID, pinned.HeadSequence, pinned.HeadEntryID}, seed.Through)
	require.Equal(t, "general", seedField(t, seedTable(t, seed, NativeChannels), 0, "name").Text)
	require.Equal(t, "baseline", seedField(t, seedTable(t, seed, NativeMessages), 0, "text").Text)
	require.NoError(t, tx.Commit())
	fresh, err := CaptureNativeSeed(ctx, path, request)
	require.NoError(t, err)
	require.Greater(t, fresh.Through.Sequence, seed.Through.Sequence)
	require.Equal(t, "after snapshot", seedField(t, seedTable(t, fresh, NativeMessages), 0, "text").Text)
}

func TestNativeSeedRefusesBoundsScopeAndInvalidData(t *testing.T) {
	for _, mode := range []string{"rows", "bytes", "zero rows", "zero bytes", "scope", "file", "admission", "trigger", "UTF8", "blob", "cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			st, path, capture, ctx := captureFixture(t)
			request := seedRequest(capture)
			want := ErrNativeLimit
			switch mode {
			case "rows":
				request.Limits.MaxRows = 1
			case "bytes":
				require.NoError(t, st.UpsertMessage(ctx, batchMessage("C1", "1.000001", "T1", strings.Repeat("body", 1000), time.Now()), nil))
				request.Limits.MaxBytes = 1000
			case "zero rows":
				request.Limits.MaxRows = 0
			case "zero bytes":
				request.Limits.MaxBytes = 0
			case "scope":
				request.ScopeSHA256 = strings.Repeat("a", 64)
				want = ErrNativeScope
			case "file":
				request.File.FileID = "other"
				want = ErrNativeBaseline
			case "admission":
				_, err := st.DB().Exec(`update channels set raw_json='{}'`)
				require.NoError(t, err)
				want = ErrNativeScope
			case "trigger":
				_, err := st.DB().Exec(`drop trigger native_changes_users_i`)
				require.NoError(t, err)
				want = ErrNativeBaseline
			case "UTF8", "blob":
				value := "cast(x'ff' as text)"
				if mode == "blob" {
					value = "x'00'"
				}
				_, err := st.DB().Exec("update workspaces set raw_json=" + value)
				require.NoError(t, err)
				want = ErrNativeData
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "deadline":
				ctx = context.Background()
			}
			before := journalRows(t, st, 0)
			seed, err := CaptureNativeSeed(ctx, path, request)
			require.Error(t, err)
			if mode != "deadline" {
				require.ErrorIs(t, err, want)
			}
			require.Equal(t, NativeSeed{}, seed)
			require.Equal(t, before, journalRows(t, st, 0))
		})
	}
}
