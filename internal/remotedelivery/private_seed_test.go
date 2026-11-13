package remotedelivery_test

import (
	"encoding/json"
	"testing"

	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/remotedelivery"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/stretchr/testify/require"
)

func TestPrivateSeedFromNativeSnapshot(t *testing.T) {
	st, path, request, ctx := projectionFixture(t)
	message := fixtureMessage()
	message.Files = []store.MessageFile{{FileID: "F1", RawJSON: evidenceRaw, Size: 9007199254740993,
		ContentSize: 9223372036854775807, MediaPath: "excluded-cache-path", FetchStatus: "excluded-status", FetchError: "excluded-error"}}
	require.NoError(t, st.UpsertMessage(ctx, message, []store.Mention{{Type: "user", TargetID: "U1", DisplayText: "mention"}}))
	_, err := st.DB().Exec(`update messages set user_id='',edited_ts=null,updated_at='2026-01-01T00:00:00.123000Z'`)
	require.NoError(t, err)
	require.NoError(t, st.SetSyncState(ctx, "provider:fixture", "workspace", "T1", "excluded-checkpoint"))
	seed, err := store.CaptureNativeSeed(ctx, path, store.NativeSeedRequest{File: request.Baseline.File, Scope: request.Scope,
		ScopeSHA256: request.ScopeSHA256, Limits: store.NativeSeedLimits{MaxRows: 100, MaxBytes: 1 << 20}})
	require.NoError(t, err)
	changes, err := remotedelivery.ProjectPrivateSeed(seed)
	require.NoError(t, err)
	require.Len(t, changes, 6)
	wantKeys := [][]remote.ChangeValue{
		{textValue("T1")}, {textValue("C1")}, {textValue("U1")},
		{textValue("C1"), textValue(message.TS)},
		{textValue("C1"), textValue(message.TS), textValue("F1")},
		{textValue("C1"), textValue(message.TS), textValue("user"), textValue("U1")},
	}
	for i, change := range changes {
		require.Equal(t, "upsert", change.Operation)
		require.Equal(t, wantKeys[i], change.Key)
	}
	require.Equal(t, textValue(""), changes[3].Row[3])
	require.Equal(t, textValue("snowman ☃\x00tail"), changes[3].Row[8])
	require.Equal(t, remote.ChangeValue{Kind: "null"}, changes[3].Row[12])
	require.Equal(t, textValue(evidenceRaw), changes[3].Row[16])
	require.Equal(t, textValue("2026-01-01T00:00:00.123000Z"), changes[3].Row[17])
	require.Equal(t, "9007199254740993", *changes[4].Row[11].Value)
	require.Equal(t, "9223372036854775807", *changes[4].Row[19].Value)
	require.Equal(t, textValue(evidenceRaw), changes[4].Row[21])
	encoded, err := json.Marshal(changes)
	require.NoError(t, err)
	for _, excluded := range []string{"excluded-cache-path", "excluded-status", "excluded-error", "excluded-checkpoint", path, seed.ScopeSHA256, seed.Through.GenerationID} {
		require.NotContains(t, string(encoded), excluded)
	}
	require.Contains(t, string(encoded), "local-cache/retained-evidence", "opaque raw content is not regex-redacted")
	var decoded []remote.Change
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, changes, decoded)
}

func TestPrivateSeedValidatesEmptyTablesAndRefusesPartialProjection(t *testing.T) {
	_, path, request, ctx := projectionFixture(t)
	seed, err := store.CaptureNativeSeed(ctx, path, store.NativeSeedRequest{File: request.Baseline.File, Scope: request.Scope,
		ScopeSHA256: request.ScopeSHA256, Limits: store.NativeSeedLimits{MaxRows: 100, MaxBytes: 1 << 20}})
	require.NoError(t, err)
	require.Empty(t, seed.Tables[5].Rows)
	changes, err := remotedelivery.ProjectPrivateSeed(seed)
	require.NoError(t, err)
	require.Len(t, changes, 3)
	for _, mode := range []string{"missing-table", "extra-table", "table-order", "unknown-table", "missing-column", "extra-column", "duplicate-column", "replaced-column", "row-width", "invalid-utf8", "integer-as-text"} {
		t.Run(mode, func(t *testing.T) {
			bad := seed
			bad.Tables = append([]store.NativeSeedTable(nil), seed.Tables...)
			last := &bad.Tables[5]
			last.Columns = append([]string(nil), last.Columns...)
			switch mode {
			case "missing-table":
				bad.Tables = bad.Tables[:5]
			case "extra-table":
				bad.Tables = append(bad.Tables, seed.Tables[5])
			case "table-order":
				bad.Tables[4], bad.Tables[5] = bad.Tables[5], bad.Tables[4]
			case "unknown-table":
				last.Table = store.NativeSyncState
			case "missing-column":
				last.Columns = last.Columns[1:]
			case "extra-column":
				last.Columns = append(last.Columns, "future_column")
			case "duplicate-column":
				last.Columns[1] = last.Columns[0]
			case "replaced-column":
				last.Columns[1] = "future_column"
			case "row-width":
				last.Rows = [][]store.NativeValue{{}}
			case "invalid-utf8":
				values := make([]store.NativeValue, len(last.Columns))
				values[4] = store.NativeValue{Kind: store.NativeText, Text: "\xff"}
				last.Rows = [][]store.NativeValue{values}
			case "integer-as-text":
				users := &bad.Tables[2]
				values := append([]store.NativeValue(nil), users.Rows[0]...)
				for i, column := range users.Columns {
					if column == "is_bot" {
						values[i] = store.NativeValue{Kind: store.NativeText, Text: "0"}
					}
				}
				users.Rows = [][]store.NativeValue{values}
			}
			changes, err := remotedelivery.ProjectPrivateSeed(bad)
			require.Error(t, err)
			require.Nil(t, changes)
		})
	}
}
