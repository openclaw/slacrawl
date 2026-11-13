package remotedelivery_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/remotedelivery"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/stretchr/testify/require"
)

const channelRaw = `{"id":"C1","is_channel":true,"is_group":false,"is_im":false,"is_mpim":false,"is_private":false,"is_shared":false}`
const evidenceRaw = "{ \"unmodeled\": \"local-cache/retained-evidence\", \"text\": \"snowman ☃\\u0000tail\" }"

func projectionFixture(t *testing.T) (*store.Store, string, store.NativeCaptureRequest, context.Context) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native capture physical identity is unsupported on this platform")
	}
	path := filepath.Join(t.TempDir(), "archive.db")
	st, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	now := time.Unix(1710000000, 0).UTC()
	require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{ID: "T1", Name: "workspace", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C1", WorkspaceID: "T1", Name: "general", Kind: "public_channel", RawJSON: channelRaw, UpdatedAt: now}))
	require.NoError(t, st.UpsertUser(ctx, store.User{ID: "U1", WorkspaceID: "T1", Name: "user", RawJSON: "{}", UpdatedAt: now}))
	admission, err := store.NativeChannelAdmissionSHA256(store.NativeChannelFacts{ChannelID: "C1", WorkspaceID: "T1", Kind: "public_channel", RawJSON: channelRaw})
	require.NoError(t, err)
	scope := store.NativeCaptureScope{WorkspaceID: "T1", Channels: []store.NativeChannelAdmission{{ChannelID: "C1", SHA256: admission}}, IncludeWorkspaceUsers: true, SyncStateKeys: []store.NativeSyncStateKey{{SourceName: "provider:fixture", EntityType: "workspace", EntityID: "T1"}}}
	digest, err := store.NativeCaptureScopeSHA256(scope)
	require.NoError(t, err)
	file, err := store.NativeArchiveFileIdentity(path)
	require.NoError(t, err)
	status, err := st.Status(ctx)
	require.NoError(t, err)
	return st, path, store.NativeCaptureRequest{
		Scope: scope, ScopeSHA256: digest,
		Baseline: store.NativeCaptureBaseline{File: file, ScopeSHA256: digest, Cursor: store.NativeCursor{GenerationID: status.NativeChanges.GenerationID, Sequence: status.NativeChanges.HeadSequence, EntryID: status.NativeChanges.HeadEntryID}},
		Limits:   store.NativeCaptureLimits{MaxEvents: 1000, MaxBytes: 1 << 20},
	}, ctx
}

func fixtureMessage() store.Message {
	return store.Message{ChannelID: "C1", TS: "1710000000.000001", WorkspaceID: "T1", UserID: "U1", Text: "snowman ☃\x00tail", NormalizedText: "snowman ☃\x00tail", SourceRank: 10, SourceName: "provider:fixture", RawJSON: evidenceRaw, UpdatedAt: time.Unix(1710000000, 0).UTC()}
}

func textValue(text string) remote.ChangeValue { return remote.ChangeValue{Kind: "text", Value: &text} }

func TestPrivateProjectionFromNativeCapture(t *testing.T) {
	st, path, request, ctx := projectionFixture(t)
	now := time.Unix(1710000100, 0).UTC()
	require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{ID: "T1", Name: "renamed", RawJSON: evidenceRaw, UpdatedAt: now}))
	require.NoError(t, st.RenameChannel(ctx, "T1", "C1", "renamed channel"))
	require.NoError(t, st.UpsertUser(ctx, store.User{ID: "U1", WorkspaceID: "T1", Name: "renamed user", IsBot: true, RawJSON: evidenceRaw, UpdatedAt: now}))
	message := fixtureMessage()
	message.Files = []store.MessageFile{{FileID: "F1", Name: "notes", RawJSON: evidenceRaw, Size: 9007199254740993, ContentSize: 9223372036854775807, ContentSHA256: "recorded-hash", FetchedAt: "2026-01-01T00:00:00.123000Z", MediaPath: "local-cache/excluded-path", FetchStatus: "excluded-status", FetchError: "excluded-error"}}
	require.NoError(t, st.UpsertMessage(ctx, message, []store.Mention{{Type: "user", TargetID: "U1", DisplayText: "mention"}}))
	// Native writers normally normalize these spellings; capture/projection must
	// preserve the actual stored NULL, empty text and timestamp without parsing.
	_, err := st.DB().ExecContext(ctx, `update messages set user_id='', edited_ts=null, updated_at='2026-01-01T00:00:00.123000Z'`)
	require.NoError(t, err)
	require.NoError(t, st.SetSyncState(ctx, "provider:fixture", "workspace", "T1", "excluded-checkpoint"))
	capture, err := store.CaptureNativeChanges(ctx, path, request)
	require.NoError(t, err)
	changes, err := remotedelivery.ProjectPrivateArchive(capture)
	require.NoError(t, err)
	require.Len(t, changes, 6)

	// These SQL projections independently specify the positional service schema.
	// Reading the fixture DB directly catches reorderings and dropped values
	// without deriving expectations from the projector or the capture itself.
	columns := map[string]string{
		"workspaces":       "id,name,domain,enterprise_id,raw_json,updated_at",
		"channels":         "id,workspace_id,name,kind,topic,purpose,is_private,is_archived,is_shared,is_general,raw_json,updated_at",
		"users":            "id,workspace_id,name,real_name,display_name,title,is_bot,is_deleted,raw_json,updated_at",
		"messages":         "channel_id,ts,workspace_id,user_id,subtype,client_msg_id,thread_ts,parent_user_id,text,normalized_text,reply_count,latest_reply,edited_ts,deleted_ts,source_rank,source_name,raw_json,updated_at",
		"message_files":    "workspace_id,channel_id,ts,file_id,user_id,name,title,mimetype,filetype,pretty_type,mode,size,url_private,url_private_download,permalink,is_public,plain_text,preview_plain_text,content_sha256,content_size,fetched_at,raw_json,updated_at,deleted_at,deletion_source,deletion_reason",
		"message_mentions": "channel_id,ts,mention_type,target_id,display_text,deleted_at,deletion_source,deletion_reason,updated_at",
	}
	keys := map[string][]remote.ChangeValue{
		"workspaces": {textValue("T1")}, "channels": {textValue("C1")}, "users": {textValue("U1")},
		"messages":         {textValue("C1"), textValue(message.TS)},
		"message_files":    {textValue("C1"), textValue(message.TS), textValue("F1")},
		"message_mentions": {textValue("C1"), textValue(message.TS), textValue("user"), textValue("U1")},
	}
	byTable := map[string]remote.Change{}
	for _, change := range changes {
		require.NotContains(t, byTable, change.Table)
		byTable[change.Table] = change
		require.Equal(t, "upsert", change.Operation)
		require.Equal(t, keys[change.Table], change.Key)
		require.Contains(t, columns, change.Table)
		values := make([]any, len(strings.Split(columns[change.Table], ",")))
		targets := make([]any, len(values))
		for i := range values {
			targets[i] = &values[i]
		}
		require.NoError(t, st.DB().QueryRowContext(ctx, "select "+columns[change.Table]+" from "+change.Table).Scan(targets...))
		want := make([]remote.ChangeValue, len(values))
		for i, value := range values {
			switch value := value.(type) {
			case nil:
				want[i] = remote.ChangeValue{Kind: "null"}
			case string:
				want[i] = textValue(value)
			case int64:
				decimal := strconv.FormatInt(value, 10)
				want[i] = remote.ChangeValue{Kind: "integer", Value: &decimal}
			default:
				t.Fatalf("unexpected fixture scalar %T", value)
			}
		}
		require.Equal(t, want, change.Row, change.Table)
	}
	require.Equal(t, textValue(message.Text), byTable["messages"].Row[8])
	require.Equal(t, textValue(""), byTable["messages"].Row[3])
	require.Equal(t, remote.ChangeValue{Kind: "null"}, byTable["messages"].Row[12])
	require.Equal(t, textValue("2026-01-01T00:00:00.123000Z"), byTable["messages"].Row[17])
	require.Equal(t, "9007199254740993", *byTable["message_files"].Row[11].Value)
	require.Equal(t, "9223372036854775807", *byTable["message_files"].Row[19].Value)
	require.Equal(t, textValue("recorded-hash"), byTable["message_files"].Row[18])
	require.Equal(t, textValue("2026-01-01T00:00:00.123000Z"), byTable["message_files"].Row[20])
	require.Equal(t, textValue(evidenceRaw), byTable["message_files"].Row[21])
	require.Equal(t, textValue(evidenceRaw), byTable["messages"].Row[16])
	encoded, err := remote.EncodeChangeBatch(remote.ChangeBatch{Contract: remote.ChangeContract, App: "slacrawl", Archive: "fixture", BindingID: "private-fixture", CaptureID: strings.Repeat("a", 32), PartCount: 1, NativeFrom: "baseline", NativeThrough: "head", Changes: changes})
	require.NoError(t, err)
	for _, excluded := range []string{"excluded-path", "excluded-status", "excluded-error", "excluded-checkpoint", capture.ScopeSHA256} {
		require.NotContains(t, string(encoded), excluded)
	}
	var decoded remote.ChangeBatch
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, changes, decoded.Changes)
}

func TestPrivateProjectionRemovalsAndExcludedSyncState(t *testing.T) {
	for _, mode := range []string{"delete", "move", "reinsert", "sync-update", "sync-delete"} {
		t.Run(mode, func(t *testing.T) {
			st, path, request, ctx := projectionFixture(t)
			message := fixtureMessage()
			var mentions []store.Mention
			if mode == "delete" || mode == "reinsert" {
				message.Files = []store.MessageFile{{FileID: "F1", RawJSON: "{}"}}
				mentions = []store.Mention{{Type: "user", TargetID: "U1"}}
			}
			require.NoError(t, st.UpsertMessage(ctx, message, mentions))
			require.NoError(t, st.SetSyncState(ctx, "provider:fixture", "workspace", "T1", "before"))
			status, err := st.Status(ctx)
			require.NoError(t, err)
			request.Baseline.Cursor = store.NativeCursor{GenerationID: status.NativeChanges.GenerationID, Sequence: status.NativeChanges.HeadSequence, EntryID: status.NativeChanges.HeadEntryID}
			switch mode {
			case "delete", "reinsert":
				removed, err := st.DeleteMessageBySource(ctx, "T1", "C1", message.TS, message.SourceName)
				require.NoError(t, err)
				require.True(t, removed)
				if mode == "reinsert" {
					require.NoError(t, st.UpsertMessage(ctx, message, mentions))
				}
			case "move":
				_, err := st.DB().ExecContext(ctx, `update messages set ts='1710000000.000002'`)
				require.NoError(t, err)
			case "sync-update":
				require.NoError(t, st.SetSyncState(ctx, "provider:fixture", "workspace", "T1", "after"))
			case "sync-delete":
				require.NoError(t, st.DeleteSyncState(ctx, "provider:fixture", "workspace", "T1"))
			}
			capture, err := store.CaptureNativeChanges(ctx, path, request)
			require.NoError(t, err)
			require.Greater(t, capture.Through.Sequence, capture.From.Sequence)
			through := capture.Through
			changes, err := remotedelivery.ProjectPrivateArchive(capture)
			require.NoError(t, err)
			require.NotNil(t, changes)
			require.Equal(t, through, capture.Through)
			switch mode {
			case "sync-update", "sync-delete":
				require.Empty(t, changes)
			case "reinsert":
				require.Len(t, changes, 3)
				for _, change := range changes {
					require.Equal(t, "upsert", change.Operation)
				}
			case "delete":
				require.ElementsMatch(t, []remote.Change{
					{Table: "messages", Operation: "remove", Key: []remote.ChangeValue{textValue("C1"), textValue(message.TS)}},
					{Table: "message_files", Operation: "remove", Key: []remote.ChangeValue{textValue("C1"), textValue(message.TS), textValue("F1")}},
					{Table: "message_mentions", Operation: "remove", Key: []remote.ChangeValue{textValue("C1"), textValue(message.TS), textValue("user"), textValue("U1")}},
				}, changes)
			case "move":
				require.Len(t, changes, 2)
				require.Equal(t, "upsert", changes[0].Operation)
				require.Equal(t, []remote.ChangeValue{textValue("C1"), textValue("1710000000.000002")}, changes[0].Key)
				require.Equal(t, remote.Change{Table: "messages", Operation: "remove", Key: []remote.ChangeValue{textValue("C1"), textValue(message.TS)}}, changes[len(changes)-1])
			}
		})
	}
}

func TestPrivateProjectionRejectsSchemaAndScalarDrift(t *testing.T) {
	st, path, request, ctx := projectionFixture(t)
	require.NoError(t, st.UpsertMessage(ctx, fixtureMessage(), nil))
	capture, err := store.CaptureNativeChanges(ctx, path, request)
	require.NoError(t, err)
	require.Len(t, capture.Rows, 1)
	for _, mode := range []string{"table", "extra-column", "missing-column", "duplicate-column", "replaced-column", "value-count", "integer-as-text", "text-as-integer", "invalid-utf8", "key-width", "key-row-mismatch", "removal-table", "removal-key-width", "removal-key-type"} {
		t.Run(mode, func(t *testing.T) {
			row := capture.Rows[0]
			row.Columns = append([]string(nil), row.Columns...)
			row.Values = append([]store.NativeValue(nil), row.Values...)
			row.Key.Values = append([]store.NativeValue(nil), row.Key.Values...)
			bad := store.NativeCapture{Rows: []store.NativeCaptureRow{capture.Rows[0], row}}
			switch mode {
			case "table":
				row.Table = "future_table"
			case "extra-column":
				row.Columns = append(row.Columns, "future_column")
				row.Values = append(row.Values, store.NativeValue{})
			case "missing-column":
				row.Columns = row.Columns[1:]
				row.Values = row.Values[1:]
			case "duplicate-column":
				row.Columns[1] = row.Columns[0]
			case "replaced-column":
				row.Columns[1] = "future_column"
			case "value-count":
				row.Values = row.Values[1:]
			case "integer-as-text":
				row.Values[10] = store.NativeValue{Kind: store.NativeText, Text: "0"}
			case "text-as-integer":
				row.Values[8] = store.NativeValue{Kind: store.NativeInteger, Integer: 1}
			case "invalid-utf8":
				row.Values[8] = store.NativeValue{Kind: store.NativeText, Text: "\xff"}
			case "key-width":
				row.Key.Values = row.Key.Values[:1]
			case "key-row-mismatch":
				row.Key.Values[0].Text = "other"
			case "removal-table":
				bad.Removals = []store.NativeRemoval{{Table: "future_table", Key: row.Key}}
			case "removal-key-width":
				bad.Removals = []store.NativeRemoval{{Table: store.NativeMessages, Key: store.NativeKey{Values: row.Key.Values[:1]}}}
			case "removal-key-type":
				bad.Removals = []store.NativeRemoval{{Table: store.NativeMessages, Key: store.NativeKey{Values: []store.NativeValue{{Kind: store.NativeInteger}, {Kind: store.NativeText, Text: "ts"}}}}}
			}
			bad.Rows[1] = row
			changes, err := remotedelivery.ProjectPrivateArchive(bad)
			require.Error(t, err)
			require.Nil(t, changes, "a malformed capture must not return partial service data")
		})
	}
}
