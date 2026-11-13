package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const capturePublicRaw = `{"id":"C1","is_channel":true,"is_group":false,"is_im":false,"is_mpim":false,"is_private":false,"is_shared":false}`
const capturePrivateRaw = `{"id":"C1","is_channel":true,"is_group":false,"is_im":false,"is_mpim":false,"is_private":true,"is_shared":false}`

func captureFixture(t *testing.T) (*Store, string, NativeCaptureRequest, context.Context) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native capture physical identity is unsupported on this platform")
	}
	path := filepath.Join(t.TempDir(), "archive.db")
	st, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	now := time.Unix(1710000000, 0).UTC()
	seedBatchCatalog(t, st, "T1", "C1", "U1", now)
	require.NoError(t, st.UpsertChannel(ctx, Channel{ID: "C1", WorkspaceID: "T1", Name: "general", Kind: "public_channel", RawJSON: capturePublicRaw, UpdatedAt: now}))
	require.NoError(t, st.UpsertMessage(ctx, batchMessage("C1", "1.000001", "T1", "baseline", now), nil))
	admission, err := NativeChannelAdmissionSHA256(NativeChannelFacts{ChannelID: "C1", WorkspaceID: "T1", Kind: "public_channel", RawJSON: capturePublicRaw})
	require.NoError(t, err)
	scope := NativeCaptureScope{WorkspaceID: "T1", Channels: []NativeChannelAdmission{{ChannelID: "C1", SHA256: admission}}, SyncStateKeys: []NativeSyncStateKey{{"provider:fixture", "workspace", "T1"}}}
	digest, err := NativeCaptureScopeSHA256(scope)
	require.NoError(t, err)
	status, err := st.Status(ctx)
	require.NoError(t, err)
	file, err := NativeArchiveFileIdentity(path)
	require.NoError(t, err)
	cursor := NativeCursor{status.NativeChanges.GenerationID, status.NativeChanges.HeadSequence, status.NativeChanges.HeadEntryID}
	return st, path, NativeCaptureRequest{Scope: scope, ScopeSHA256: digest, Baseline: NativeCaptureBaseline{File: file, ScopeSHA256: digest, Cursor: cursor}, Limits: NativeCaptureLimits{MaxEvents: 1000, MaxBytes: 1 << 20}}, ctx
}

func captureField(t *testing.T, row NativeCaptureRow, name string) NativeValue {
	t.Helper()
	for i, column := range row.Columns {
		if column == name {
			return row.Values[i]
		}
	}
	t.Fatalf("column %s missing", name)
	return NativeValue{}
}

func TestNativeCaptureMaterializesLocalRowsAndExplicitScope(t *testing.T) {
	st, path, request, ctx := captureFixture(t)
	message := batchMessage("C1", "1.000001", "T1", "snowman ☃\x00tail", time.Unix(1710000100, 0))
	message.Files = []MessageFile{{FileID: "F1", RawJSON: "{}", MediaPath: "local-cache/files/F1", ContentSize: 9007199254740993}}
	require.NoError(t, st.UpsertMessage(ctx, message, []Mention{{Type: "user", TargetID: "U1", DisplayText: "mention"}}))
	_, err := st.DB().Exec(`update messages set user_id='', edited_ts=null, updated_at='2026-01-01T00:00:00.123000Z' where channel_id='C1' and ts='1.000001'`)
	require.NoError(t, err)
	require.NoError(t, st.RenameChannel(ctx, "T1", "C1", "renamed"))
	require.NoError(t, st.UpsertWorkspace(ctx, Workspace{ID: "T1", Name: "renamed workspace", RawJSON: "{}", UpdatedAt: time.Now()}))
	require.NoError(t, st.UpsertUser(ctx, User{ID: "U1", WorkspaceID: "T1", Name: "not opted in", RawJSON: "{}", UpdatedAt: time.Now()}))
	require.NoError(t, st.SetSyncState(ctx, "provider:fixture", "workspace", "T1", "registered checkpoint"))
	require.NoError(t, st.SetSyncState(ctx, "other", "opaque", "T1", "unregistered checkpoint"))
	seedBatchCatalog(t, st, "T2", "C2", "U2", time.Now())
	require.NoError(t, st.UpsertMessage(ctx, batchMessage("C2", "2.000002", "T2", "foreign content", time.Now()), nil))
	before, err := st.Status(ctx)
	require.NoError(t, err)
	captured, err := CaptureNativeChanges(ctx, path, request)
	require.NoError(t, err)
	require.Equal(t, request.Baseline.File, captured.File)
	require.Equal(t, request.Baseline.Cursor, captured.From)
	require.Equal(t, before.NativeChanges.HeadSequence, captured.Through.Sequence)
	require.Equal(t, before.NativeChanges.HeadEntryID, captured.Through.EntryID)
	require.Empty(t, captured.Removals)
	tables := map[NativeTable]NativeCaptureRow{}
	for _, row := range captured.Rows {
		require.NotContains(t, tables, row.Table)
		tables[row.Table] = row
		for _, value := range row.Values {
			require.NotContains(t, value.Text, "foreign content")
			require.NotContains(t, value.Text, "unregistered checkpoint")
		}
	}
	require.Len(t, tables, 6)
	require.NotContains(t, tables, NativeUsers)
	require.Equal(t, NativeValue{Kind: NativeText, Text: "snowman ☃\x00tail"}, captureField(t, tables[NativeMessages], "text"))
	require.Equal(t, NativeValue{Kind: NativeText}, captureField(t, tables[NativeMessages], "user_id"))
	require.Equal(t, NativeValue{Kind: NativeNull}, captureField(t, tables[NativeMessages], "edited_ts"))
	require.Equal(t, NativeValue{Kind: NativeText, Text: "2026-01-01T00:00:00.123000Z"}, captureField(t, tables[NativeMessages], "updated_at"))
	require.Equal(t, NativeValue{Kind: NativeInteger, Integer: 9007199254740993}, captureField(t, tables[NativeMessageFiles], "content_size"))
	require.Equal(t, NativeValue{Kind: NativeText, Text: "local-cache/files/F1"}, captureField(t, tables[NativeMessageFiles], "media_path"), "local capture is not wire-safe")
	require.Equal(t, NativeValue{Kind: NativeText, Text: "registered checkpoint"}, captureField(t, tables[NativeSyncState], "value"))
	after, err := st.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.NoError(t, st.UpsertMessage(ctx, batchMessage("C1", "1.000001", "T1", "later", time.Now()), nil))
	require.Equal(t, "snowman ☃\x00tail", captureField(t, tables[NativeMessages], "text").Text)
	// A separately registered scope can explicitly include workspace user rows.
	request.Scope.IncludeWorkspaceUsers = true
	request.ScopeSHA256, err = NativeCaptureScopeSHA256(request.Scope)
	require.NoError(t, err)
	request.Baseline.ScopeSHA256 = request.ScopeSHA256
	withUsers, err := CaptureNativeChanges(ctx, path, request)
	require.NoError(t, err)
	var users []NativeCaptureRow
	for _, row := range withUsers.Rows {
		if row.Table == NativeUsers {
			users = append(users, row)
		}
	}
	require.Len(t, users, 1)
	require.Equal(t, "U1", captureField(t, users[0], "id").Text)
}

func TestNativeCaptureDeleteReinsertAndMove(t *testing.T) {
	for _, mode := range []string{"delete", "reinsert", "key-move", "workspace-move", "unexplained-missing"} {
		t.Run(mode, func(t *testing.T) {
			st, path, request, ctx := captureFixture(t)
			switch mode {
			case "delete", "reinsert":
				removed, err := st.DeleteMessageBySource(ctx, "T1", "C1", "1.000001", "provider:test")
				require.NoError(t, err)
				require.True(t, removed)
				if mode == "reinsert" {
					require.NoError(t, st.UpsertMessage(ctx, batchMessage("C1", "1.000001", "T1", "recreated", time.Now()), nil))
				}
			case "key-move":
				_, err := st.DB().Exec(`update messages set ts='2.000002' where channel_id='C1' and ts='1.000001'`)
				require.NoError(t, err)
			case "workspace-move":
				// Invalid foreign text proves no excluded body is materialized.
				_, err := st.DB().Exec(`update messages set workspace_id='T2', text=cast(x'ff' as text) where channel_id='C1' and ts='1.000001'`)
				require.NoError(t, err)
			case "unexplained-missing":
				require.NoError(t, st.UpsertMessage(ctx, batchMessage("C1", "1.000001", "T1", "observed update", time.Now()), nil))
				var definition string
				require.NoError(t, st.DB().QueryRow(`select sql from sqlite_schema where name='native_changes_messages_d'`).Scan(&definition))
				_, err := st.DB().Exec(`drop trigger native_changes_messages_d; delete from messages where channel_id='C1' and ts='1.000001'`)
				require.NoError(t, err)
				_, err = st.DB().Exec(definition)
				require.NoError(t, err)
			}
			captured, err := CaptureNativeChanges(ctx, path, request)
			if mode == "unexplained-missing" {
				require.ErrorIs(t, err, ErrNativeData)
				require.Equal(t, NativeCapture{}, captured)
				return
			}
			require.NoError(t, err)
			if mode == "reinsert" {
				require.Empty(t, captured.Removals)
				require.Len(t, captured.Rows, 1)
				require.Equal(t, "recreated", captureField(t, captured.Rows[0], "text").Text)
			} else {
				require.Len(t, captured.Removals, 1)
				removal := captured.Removals[0]
				require.Equal(t, NativeMessages, removal.Table)
				require.Equal(t, []NativeValue{{Kind: NativeText, Text: "C1"}, {Kind: NativeText, Text: "1.000001"}}, removal.Key.Values)
				require.Equal(t, "T1", *removal.WorkspaceID)
				require.Greater(t, removal.Sequence, request.Baseline.Cursor.Sequence)
				if mode == "key-move" {
					require.Len(t, captured.Rows, 1)
					require.Equal(t, "2.000002", captureField(t, captured.Rows[0], "ts").Text)
				} else {
					require.Empty(t, captured.Rows)
				}
			}
		})
	}
}

func TestNativeCaptureRefusesChangedBindingAndBounds(t *testing.T) {
	for _, mode := range []string{"missing-baseline", "generation", "entry", "missing-anchor", "beyond-head", "floor", "file", "scope-hash", "baseline-scope", "channel-admission", "legacy-channel", "missing-channel", "missing-trigger", "events", "bytes", "encoding", "storage-class", "cancelled", "no-deadline"} {
		t.Run(mode, func(t *testing.T) {
			st, path, request, ctx := captureFixture(t)
			require.NoError(t, st.UpsertMessage(ctx, batchMessage("C1", "1.000001", "T1", "changed", time.Now()), nil))
			require.NoError(t, st.SetSyncState(ctx, "provider:fixture", "workspace", "T1", "next"))
			want := ErrNativeBaseline
			switch mode {
			case "missing-baseline":
				request.Baseline = NativeCaptureBaseline{}
				want = ErrNativeScope
			case "generation":
				request.Baseline.Cursor.GenerationID = strings.Repeat("0", 32)
			case "entry":
				request.Baseline.Cursor.EntryID = strings.Repeat("0", 32)
			case "missing-anchor":
				_, err := st.DB().Exec(`delete from native_changes where sequence = ?`, request.Baseline.Cursor.Sequence)
				require.NoError(t, err)
			case "beyond-head":
				request.Baseline.Cursor.Sequence = 1 << 60
			case "floor":
				_, err := st.DB().Exec(`update native_change_state set floor_sequence=(select max(sequence) from native_changes), floor_entry_id=(select entry_id from native_changes order by sequence desc limit 1)`)
				require.NoError(t, err)
			case "file":
				request.Baseline.File.FileID += "-different"
			case "scope-hash":
				request.ScopeSHA256 = strings.Repeat("0", 64)
				want = ErrNativeScope
			case "baseline-scope":
				request.Baseline.ScopeSHA256 = strings.Repeat("0", 64)
				want = ErrNativeScope
			case "channel-admission":
				_, err := st.DB().Exec(`update channels set is_private=1,kind='private_channel',raw_json=? where id='C1'`, capturePrivateRaw)
				require.NoError(t, err)
				want = ErrNativeScope
			case "legacy-channel":
				_, err := st.DB().Exec(`update channels set kind='desktop_channel', raw_json='{"id":"C1","name":"legacy"}' where id='C1'`)
				require.NoError(t, err)
				want = ErrNativeScope
			case "missing-channel":
				_, err := st.DB().Exec(`delete from channels where id='C1'`)
				require.NoError(t, err)
				want = ErrNativeScope
			case "missing-trigger":
				_, err := st.DB().Exec(`drop trigger native_changes_messages_u`)
				require.NoError(t, err)
			case "events":
				request.Limits.MaxEvents = 1
				want = ErrNativeLimit
			case "bytes":
				request.Limits.MaxBytes = 1
				want = ErrNativeLimit
			case "encoding":
				_, err := st.DB().Exec(`update messages set text=cast(x'ff' as text)`)
				require.NoError(t, err)
				want = ErrNativeData
			case "storage-class":
				_, err := st.DB().Exec(`update messages set text=x'6162'`)
				require.NoError(t, err)
				want = ErrNativeData
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "no-deadline":
				ctx = context.Background()
				want = nil
			}
			captured, err := CaptureNativeChanges(ctx, path, request)
			require.Error(t, err)
			if want != nil {
				require.ErrorIs(t, err, want)
			}
			require.Equal(t, NativeCapture{}, captured)
		})
	}
}

func TestNativeCaptureUsesOneSQLiteSnapshot(t *testing.T) {
	st, path, request, ctx := captureFixture(t)
	require.NoError(t, st.UpsertMessage(ctx, batchMessage("C1", "1.000001", "T1", "snapshot text", time.Now()), nil))
	reader, err := OpenReadOnly(path)
	require.NoError(t, err)
	defer reader.Close()
	tx, err := reader.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer tx.Rollback()
	pinned, err := readNativeChangeStatus(ctx, tx)
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessage(ctx, batchMessage("C1", "1.000001", "T1", "later text", time.Now()), nil))
	_, err = st.DB().Exec(`update channels set is_private=1,kind='private_channel',raw_json=? where id='C1'`, capturePrivateRaw)
	require.NoError(t, err)
	captured, err := readNativeCapture(ctx, tx, request)
	require.NoError(t, err)
	require.Equal(t, pinned.HeadSequence, captured.Through.Sequence)
	require.Len(t, captured.Rows, 1)
	require.Equal(t, "snapshot text", captureField(t, captured.Rows[0], "text").Text)
	require.NoError(t, tx.Commit())
	fresh, err := CaptureNativeChanges(ctx, path, request)
	require.ErrorIs(t, err, ErrNativeScope)
	require.Equal(t, NativeCapture{}, fresh)
}

func TestNativeCaptureRefusesJournalGap(t *testing.T) {
	st, path, request, ctx := captureFixture(t)
	// A rolled-back native write must not leave a gap in committed entries.
	tx, err := st.DB().BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `update messages set text='rolled back' where channel_id='C1'`)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	require.NoError(t, st.UpsertMessage(ctx, batchMessage("C1", "1.000001", "T1", "first", time.Now()), nil))
	require.NoError(t, st.SetSyncState(ctx, "outside", "opaque", "foreign", "not selected"))
	require.NoError(t, st.UpsertMessage(ctx, batchMessage("C1", "1.000001", "T1", "last", time.Now()), nil))
	complete, err := CaptureNativeChanges(ctx, path, request)
	require.NoError(t, err)
	require.Equal(t, int64(complete.EventsRead), complete.Through.Sequence-complete.From.Sequence)
	// Removing an unselected middle event must still invalidate the whole interval.
	removed, err := st.DB().Exec(`delete from native_changes where table_name='sync_state' and sequence > ? and sequence < ?`, request.Baseline.Cursor.Sequence, complete.Through.Sequence)
	require.NoError(t, err)
	count, err := removed.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	captured, err := CaptureNativeChanges(ctx, path, request)
	require.ErrorIs(t, err, ErrNativeBaseline)
	require.Equal(t, NativeCapture{}, captured)
}

func TestNativeCaptureLeavesSourceBytesUnchanged(t *testing.T) {
	st, path, request, ctx := captureFixture(t)
	require.NoError(t, st.UpsertMessage(ctx, batchMessage("C1", "1.000001", "T1", "capture me", time.Now()), nil))
	require.NoError(t, st.Close())
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	captured, err := CaptureNativeChanges(ctx, path, request)
	require.NoError(t, err)
	require.Len(t, captured.Rows, 1)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256(before), sha256.Sum256(after))
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	require.NoError(t, os.WriteFile(copyPath, before, 0600))
	zero, err := CaptureNativeChanges(ctx, copyPath, request)
	require.ErrorIs(t, err, ErrNativeBaseline)
	require.Equal(t, NativeCapture{}, zero)
}

func TestNativeChannelAdmissionFacts(t *testing.T) {
	facts := NativeChannelFacts{ChannelID: "C1", WorkspaceID: "T1", Kind: "public_channel", RawJSON: capturePublicRaw}
	baseline, err := NativeChannelAdmissionSHA256(facts)
	require.NoError(t, err)
	renamed := facts
	renamed.RawJSON = strings.TrimSuffix(capturePublicRaw, "}") + `,"name":"renamed","topic":{"value":"changed"}}`
	same, err := NativeChannelAdmissionSHA256(renamed)
	require.NoError(t, err)
	require.Equal(t, baseline, same)
	private := facts
	private.Kind = "private_channel"
	private.IsPrivate = 1
	private.RawJSON = capturePrivateRaw
	changed, err := NativeChannelAdmissionSHA256(private)
	require.NoError(t, err)
	require.NotEqual(t, baseline, changed)
	for _, raw := range []string{`{}`, strings.Replace(capturePublicRaw, `"is_private":false`, `"is_private":null`, 1), strings.Replace(capturePublicRaw, `"id":"C1"`, `"id":"OTHER"`, 1), strings.TrimSuffix(capturePublicRaw, "}") + `,"IS_PRIVATE":false}`, strings.Replace(capturePublicRaw, `"is_im":false`, `"is_im":true`, 1)} {
		invalid := facts
		invalid.RawJSON = raw
		_, err := NativeChannelAdmissionSHA256(invalid)
		require.ErrorIs(t, err, ErrNativeScope)
	}
	facts.Kind = "uninterpretable"
	_, err = NativeChannelAdmissionSHA256(facts)
	require.ErrorIs(t, err, ErrNativeScope)
}

func TestNativeCaptureFloorAnchorAndEmptyInterval(t *testing.T) {
	st, path, request, ctx := captureFixture(t)
	tx, err := st.DB().BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, ResetNativeChangesInTransaction(ctx, tx))
	require.NoError(t, tx.Commit())
	status, err := st.Status(ctx)
	require.NoError(t, err)
	// A fixture-qualified baseline at the restored generation's exact floor.
	request.Baseline.Cursor = NativeCursor{status.NativeChanges.GenerationID, status.NativeChanges.FloorSequence, status.NativeChanges.FloorEntryID}
	empty, err := CaptureNativeChanges(ctx, path, request)
	require.NoError(t, err)
	require.Equal(t, empty.From, empty.Through)
	require.Zero(t, empty.EventsRead)
	require.Empty(t, empty.Rows)
	require.Empty(t, empty.Removals)
	require.NoError(t, st.UpsertMessage(ctx, batchMessage("C1", "1.000001", "T1", "after baseline", time.Now()), nil))
	next, err := CaptureNativeChanges(ctx, path, request)
	require.NoError(t, err)
	require.Len(t, next.Rows, 1)
	request.Baseline.Cursor.EntryID = strings.Repeat("0", 32)
	zero, err := CaptureNativeChanges(ctx, path, request)
	require.ErrorIs(t, err, ErrNativeBaseline)
	require.Equal(t, NativeCapture{}, zero)
}
