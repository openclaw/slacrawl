package slackdesktop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/syndtr/goleveldb/leveldb"

	"github.com/openclaw/slacrawl/internal/admission"
	"github.com/openclaw/slacrawl/internal/store"
)

func admissionState(channelID string, flags map[string]any) map[string]any {
	channel := map[string]any{"id": channelID, "name": "channel", "context_team_id": "T1"}
	for k, v := range flags {
		channel[k] = v
	}
	return map[string]any{
		"selfTeamIds": map[string]any{"teamId": "T1"}, "bootData": map[string]any{"user_id": "U1"},
		"channels": map[string]any{channelID: channel},
		"messages": map[string]any{channelID: map[string]any{"1710000001.000001": map[string]any{"ts": "1710000001.000001", "type": "message", "text": "intake-canary <@UMENTION>"}}},
	}
}

func writeAdmissionBlob(t *testing.T, root, name string, value any) {
	t.Helper()
	requireNode(t)
	body, err := json.Marshal(value)
	require.NoError(t, err)
	cmd := exec.Command("node", "-e", `process.stdout.write(require("v8").serialize(JSON.parse(require("fs").readFileSync(0,"utf8"))))`)
	cmd.Stdin = strings.NewReader(string(body))
	payload, err := cmd.Output()
	require.NoError(t, err)
	writeBlob(t, root, name, payload)
}

func admissionStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	return st
}

var desktopArchiveTables = []string{"workspaces", "channels", "users", "messages", "message_files", "message_events", "message_event_heads", "message_mentions", "message_fts", "embedding_jobs", "sync_state"}

func requireNoDesktopCanary(t *testing.T, st *store.Store, canaries ...string) {
	t.Helper()
	for _, table := range desktopArchiveTables {
		rows, err := st.QueryReadOnly(context.Background(), "select * from "+table)
		require.NoError(t, err)
		data, err := json.Marshal(rows)
		require.NoError(t, err)
		for _, canary := range canaries {
			require.NotContains(t, string(data), canary, table)
		}
	}
}

func TestDesktopAdmissionNativeKinds(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		flags    map[string]any
		allowed  bool
	}{
		{"public", "CPUB", map[string]any{"is_channel": true}, true},
		{"private", "CPRIV", map[string]any{"is_channel": true, "is_private": true}, true},
		{"legacy private", "GPRIV", map[string]any{"is_group": true, "is_private": true}, true},
		{"misleading D prefix", "DPUB", map[string]any{"is_channel": true}, true},
		{"misleading C prefix", "CDM", map[string]any{"is_im": true, "is_channel": true}, false},
		{"mpim", "GDM", map[string]any{"is_mpim": true, "is_group": true, "is_private": true}, false},
		{"private only", "CPRIV", map[string]any{"is_private": true}, false},
		{"conflicting flags", "CBOTH", map[string]any{"is_channel": true, "is_group": true, "is_private": true}, false},
		{"unknown", "CUNKNOWN", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeAdmissionBlob(t, root, "state", admissionState(tc.id, tc.flags))
			for _, policy := range []admission.DMPolicy{admission.Default, admission.Include, admission.Exclude} {
				t.Run(fmt.Sprint(policy), func(t *testing.T) {
					st := admissionStore(t)
					source, err := Ingest(context.Background(), st, root, IngestOptions{DMPolicy: policy})
					require.NoError(t, err)
					rows, err := st.Messages(context.Background(), "", "", "", 100)
					require.NoError(t, err)
					if tc.allowed || policy != admission.Exclude {
						require.Len(t, rows, 1)
					} else {
						require.Empty(t, rows)
						requireNoDesktopCanary(t, st, tc.id, "intake-canary", "UMENTION")
						require.Equal(t, 0, source.Admission.Messages)
					}
				})
			}
		})
	}
}

func TestDesktopAdmissionAllBlobEvidence(t *testing.T) {
	for _, kind := range []string{"dm", "unknown", "conflicting", "missing selected", "missing id dm", "missing id unknown", "primitive unknown"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%v", kind, reverse), func(t *testing.T) {
				root := t.TempDir()
				selected := admissionState("CONE", map[string]any{"is_channel": true})
				selected["channels"].(map[string]any)["CFILLER"] = map[string]any{"id": "CFILLER", "is_channel": true}
				flags := map[string]any{}
				switch kind {
				case "dm", "missing id dm":
					flags["is_im"] = true
				case "conflicting":
					flags["is_channel"] = true
					flags["is_private"] = true
				case "missing selected":
					flags["is_channel"] = true
					delete(selected["channels"].(map[string]any), "CONE")
				}
				older := admissionState("CONE", flags)
				older["messages"] = map[string]any{}
				if strings.HasPrefix(kind, "missing id") {
					delete(older["channels"].(map[string]any)["CONE"].(map[string]any), "id")
				} else if kind == "primitive unknown" {
					older["channels"].(map[string]any)["CONE"] = true
				}
				names := []string{"a", "z"}
				if reverse {
					names = []string{"z", "a"}
				}
				writeAdmissionBlob(t, root, names[0], selected)
				writeAdmissionBlob(t, root, names[1], older)
				for _, policy := range []admission.DMPolicy{admission.Default, admission.Include, admission.Exclude} {
					st := admissionStore(t)
					source, err := Ingest(context.Background(), st, root, IngestOptions{DMPolicy: policy})
					require.NoError(t, err)
					require.Equal(t, 2, source.IndexedDB.DecodedBlobCount)
					require.Equal(t, 1, source.IndexedDB.DecodedStateCount)
					rows, err := st.Messages(context.Background(), "", "CONE", "", 10)
					require.NoError(t, err)
					if policy == admission.Exclude {
						require.Empty(t, rows)
						requireNoDesktopCanary(t, st, "CONE", "intake-canary")
					} else {
						require.Len(t, rows, 1)
						require.Contains(t, rows[0].Text, "intake-canary")
					}
				}
			})
		}
	}
}

func TestDesktopAdmissionRejectsRetainedIdentityBeforeWrites(t *testing.T) {
	requireNode(t)
	for _, field := range []string{"map key", "channel", "channel_id", "conversation", "conversation_id", "metadata channel", "metadata channel_id", "metadata conversation", "metadata conversation_id", "nested key", "duplicate", "shared subtree", "outer dm"} {
		t.Run(field, func(t *testing.T) {
			root := t.TempDir()
			state := admissionState("CPUB", map[string]any{"is_channel": true})
			state["channels"].(map[string]any)["COTHER"] = map[string]any{"id": "COTHER", "is_channel": true}
			message := state["messages"].(map[string]any)["CPUB"].(map[string]any)["1710000001.000001"].(map[string]any)
			switch field {
			case "map key":
				state["channels"].(map[string]any)["CPUB"].(map[string]any)["id"] = "COTHER"
			case "metadata channel", "metadata channel_id", "metadata conversation", "metadata conversation_id":
				state["channels"].(map[string]any)["CPUB"].(map[string]any)[strings.TrimPrefix(field, "metadata ")] = "COTHER"
			case "nested key":
				state["messages"] = map[string]any{"CPUB": map[string]any{"COTHER": message}}
			case "duplicate":
				message["channel"] = "CPUB"
				state["threads"] = map[string]any{"CPUB": map[string]any{"1710000001.000001": map[string]any{"messages": []any{map[string]any{"ts": "1710000001.000001", "channel": "CPUB", "channel_id": "COTHER", "text": "discarded payload"}}}}}
			case "outer dm":
				state["channels"].(map[string]any)["CDM"] = map[string]any{"id": "CDM", "is_im": true}
				message["channel"] = "CPUB"
				state["messages"] = map[string]any{"CDM": map[string]any{"1710000001.000001": message}}
			case "shared subtree":
				body, err := json.Marshal(state)
				require.NoError(t, err)
				cmd := exec.Command("node", "-e", `const s=JSON.parse(require("fs").readFileSync(0,"utf8")); const message=s.messages.CPUB["1710000001.000001"]; message.channel="CPUB"; const shared={messages:[message]}; s.messages={CPUB:shared,COTHER:shared}; process.stdout.write(require("v8").serialize(s));`)
				cmd.Stdin = strings.NewReader(string(body))
				payload, err := cmd.Output()
				require.NoError(t, err)
				writeBlob(t, root, "state", payload)
			default:
				message["channel"] = "CPUB"
				message[field] = "COTHER"
			}
			if field != "shared subtree" {
				writeAdmissionBlob(t, root, "state", state)
			}
			for _, policy := range []admission.DMPolicy{admission.Default, admission.Include, admission.Exclude} {
				st := admissionStore(t)
				_, err := Ingest(context.Background(), st, root, IngestOptions{DMPolicy: policy})
				require.ErrorContains(t, err, "identity conflicts")
				for _, table := range desktopArchiveTables {
					rows, err := st.QueryReadOnly(context.Background(), "select * from "+table)
					require.NoError(t, err)
					require.Empty(t, rows, table)
				}
				_, err = Ingest(context.Background(), st, root, IngestOptions{DMPolicy: policy, WorkspaceID: "TEXCLUDED"})
				require.NoError(t, err)
				_, err = Ingest(context.Background(), st, root, IngestOptions{DMPolicy: policy, ExcludeChannels: []string{"CPUB", "COTHER"}})
				require.NoError(t, err)
			}
		})
	}
}

func TestDesktopAdmissionDoesNotTreatAttachmentsAsMessages(t *testing.T) {
	for _, container := range []string{"messages", "threads"} {
		for _, attachmentChannel := range []string{"DOTHER", "CPUB", ""} {
			t.Run(container+"/"+attachmentChannel, func(t *testing.T) {
				root := t.TempDir()
				state := admissionState("CPUB", map[string]any{"is_channel": true})
				message := state["messages"].(map[string]any)["CPUB"].(map[string]any)["1710000001.000001"].(map[string]any)
				attachments := []any{map[string]any{
					"ts": "1710000002.000001", "channel_id": attachmentChannel,
					"text": "attached message", "user": "UATTACHED",
				}}
				message["attachments"] = attachments
				if container == "threads" {
					state["threads"] = map[string]any{"CPUB": map[string]any{"1710000001.000001": map[string]any{"messages": []any{message}}}}
					delete(state, "messages")
				}
				writeAdmissionBlob(t, root, "state", state)
				for _, policy := range []admission.DMPolicy{admission.Default, admission.Include, admission.Exclude} {
					t.Run(fmt.Sprint(policy), func(t *testing.T) {
						st := admissionStore(t)
						for range 2 {
							source, err := Ingest(context.Background(), st, root, IngestOptions{DMPolicy: policy})
							require.NoError(t, err)
							require.Equal(t, 1, source.Admission.Messages)
						}
						rows, err := st.QueryReadOnly(context.Background(), "select channel_id, ts, raw_json from messages")
						require.NoError(t, err)
						require.Len(t, rows, 1)
						require.Equal(t, "CPUB", rows[0]["channel_id"])
						require.Equal(t, "1710000001.000001", rows[0]["ts"])
						var raw map[string]any
						require.NoError(t, json.Unmarshal([]byte(rows[0]["raw_json"].(string)), &raw))
						require.Equal(t, attachments, raw["attachments"])
					})
				}
			})
		}
	}
}

func TestDesktopAdmissionSupportedShapesAndReplay(t *testing.T) {
	root := t.TempDir()
	state := admissionState("CPUB", map[string]any{"is_channel": true})
	messages := state["messages"].(map[string]any)["CPUB"].(map[string]any)
	message := messages["1710000001.000001"].(map[string]any)
	message["replies"] = map[string]any{"1710000002.000001": map[string]any{"thread_ts": "1710000001.000001", "text": "first reply", "user": "UEXTERNAL"}}
	state["members"] = map[string]any{"UEXTERNAL": map[string]any{"id": "UEXTERNAL", "team_id": "TEXTERNAL", "name": "external"}}
	state["threads"] = map[string]any{"CPUB": map[string]any{"1710000001.000001": map[string]any{"messages": []any{map[string]any{"ts": "1710000002.000001", "thread_ts": "1710000001.000001", "text": "later duplicate"}, map[string]any{"ts": 1710000003, "text": "thread array"}}}}}
	messages["arbitrary"] = map[string]any{"messages": []any{map[string]any{"ts": "1710000004.000001", "text": "heuristic-only"}}}
	writeAdmissionBlob(t, root, "state", state)
	var allowedRaw []map[string]any
	for _, policy := range []admission.DMPolicy{admission.Default, admission.Include, admission.Exclude} {
		st := admissionStore(t)
		for n := 0; n < 2; n++ {
			_, err := Ingest(context.Background(), st, root, IngestOptions{DMPolicy: policy})
			require.NoError(t, err)
		}
		rows, err := st.QueryReadOnly(context.Background(), "select ts, raw_json from messages where ts != '1710000004.000001' order by ts")
		require.NoError(t, err)
		require.Len(t, rows, 3)
		if allowedRaw == nil {
			allowedRaw = rows
		} else {
			require.Equal(t, allowedRaw, rows)
		}
		data, err := json.Marshal(rows)
		require.NoError(t, err)
		require.Contains(t, string(data), "first reply")
		require.NotContains(t, string(data), "later duplicate")
		require.NotContains(t, string(data), "provenance")
		count := 4
		if policy == admission.Exclude {
			count = 3
			requireNoDesktopCanary(t, st, "heuristic-only")
		}
		for _, table := range []string{"messages", "message_event_heads"} {
			rows, err := st.QueryReadOnly(context.Background(), "select count(*) as n from "+table)
			require.NoError(t, err)
			require.Equal(t, int64(count), rows[0]["n"], table)
		}
	}
}

func TestDesktopAdmissionHeuristicKeysPreserveLegacyPayload(t *testing.T) {
	for _, key := range []string{"Cached", "COTHER"} {
		t.Run(key, func(t *testing.T) {
			root := t.TempDir()
			state := admissionState("CPUB", map[string]any{"is_channel": true})
			state["channels"].(map[string]any)["COTHER"] = map[string]any{"id": "COTHER", "is_channel": true}
			state["messages"] = map[string]any{"CPUB": map[string]any{key: map[string]any{"channel": "CPUB", "ts": "1", "text": "legacy heuristic"}}}
			writeAdmissionBlob(t, root, "state", state)
			var raw []map[string]any
			for _, policy := range []admission.DMPolicy{admission.Default, admission.Include, admission.Exclude} {
				st := admissionStore(t)
				source, err := Ingest(context.Background(), st, root, IngestOptions{DMPolicy: policy})
				require.NoError(t, err)
				rows, err := st.QueryReadOnly(context.Background(), "select channel_id, ts, raw_json from messages")
				require.NoError(t, err)
				if policy == admission.Exclude {
					require.Empty(t, rows)
					require.Equal(t, 1, source.Admission.UnsupportedMessage)
					requireNoDesktopCanary(t, st, "legacy heuristic")
				} else {
					require.Len(t, rows, 1)
					require.Equal(t, "CPUB", rows[0]["channel_id"])
					if raw == nil {
						raw = rows
					} else {
						require.Equal(t, raw, rows)
					}
				}
			}
		})
	}
}

func TestDesktopAdmissionSharedDAGHasBoundedTraversal(t *testing.T) {
	requireNode(t)
	root := t.TempDir()
	// V8 preserves this compact 36-node graph. Revisiting every identical-context
	// path would expand it into billions of walks; the deadline bounds regressions.
	cmd := exec.Command("node", "-e", `let node={ts:"1",channel:"CPUB",text:"shared leaf"}; for(let i=0;i<36;i++) node={a:node,b:node}; process.stdout.write(require("v8").serialize({selfTeamIds:{teamId:"T1"},channels:{CPUB:{id:"CPUB",is_channel:true}},messages:{CPUB:node}}));`)
	payload, err := cmd.Output()
	require.NoError(t, err)
	require.Less(t, len(payload), 2048)
	writeBlob(t, root, "state", payload)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, policy := range []admission.DMPolicy{admission.Default, admission.Include, admission.Exclude} {
		st := admissionStore(t)
		source, err := Ingest(ctx, st, root, IngestOptions{DMPolicy: policy})
		require.NoError(t, err)
		require.Equal(t, 1, source.IndexedDB.DecodedBlobCount)
		want := 1
		if policy == admission.Exclude {
			want = 0
			require.Equal(t, 1, source.Admission.UnsupportedMessage)
		}
		require.Equal(t, want, source.Admission.Messages)
	}
}

func TestDesktopAdmissionDraftsHintsAndExistingRows(t *testing.T) {
	for _, later := range []string{"CDM", "CUNKNOWN", "CEXCLUDED", "CFOREIGN", "CPUB"} {
		t.Run(later, func(t *testing.T) {
			root := t.TempDir()
			state := admissionState("CPUB", map[string]any{"is_channel": true})
			channels := state["channels"].(map[string]any)
			channels["CDM"] = map[string]any{"id": "CDM", "is_im": true}
			channels["CUNKNOWN"] = map[string]any{"id": "CUNKNOWN"}
			channels["CEXCLUDED"] = map[string]any{"id": "CEXCLUDED", "name": "excluded", "is_channel": true}
			channels["CFOREIGN"] = map[string]any{"id": "CFOREIGN", "context_team_id": "T2", "is_channel": true}
			writeAdmissionBlob(t, root, "state", state)
			db, err := leveldb.OpenFile(filepath.Join(root, localStorageDir), nil)
			require.NoError(t, err)
			put := func(key, body string) {
				require.NoError(t, db.Put([]byte("_https://app.slack.compersist-v1::T1::U1::"+key), []byte(body), nil))
			}
			put("drafts", fmt.Sprintf(`{"unifiedDrafts":{"draft":{"destinations":[{"channel_id":"CPUB"},{"channel_id":%q}],"ops":[{"insert":"draft-canary"}],"last_updated_ts":1710000000}}}`, later))
			put("recentlyJoinedChannels", `{"CPUB":{},"CDM":{}}`)
			put("persistedApiCalls", `{"public":{"method":"conversations.mark","args":{"channel":"CPUB","ts":"1"}},"dm":{"method":"conversations.mark","args":{"channel":"CDM","ts":"2"}}}`)
			put("expandables", `{"unattributed-canary":true}`)
			require.NoError(t, db.Close())
			require.NoError(t, os.MkdirAll(filepath.Join(root, "storage"), 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(root, rootStateFile), []byte(`{"downloads":{"T1":{"item":{"id":"download-canary"}}}}`), 0o600))
			st := admissionStore(t)
			ctx := context.Background()
			legacyChannel := "CPUB"
			if later == "CPUB" {
				legacyChannel = "CDM"
			}
			legacy := store.Message{WorkspaceID: "T1", ChannelID: legacyChannel, TS: "draft:171:T1:draft", Text: "retained <@UOLD>", NormalizedText: "retained", SourceName: draftSourceName, SourceRank: 3, RawJSON: `{"retained":true}`, UpdatedAt: time.Now().UTC(), Files: []store.MessageFile{{FileID: "FRETAINED", Name: "retained.txt", RawJSON: `{}`}}}
			require.NoError(t, st.UpsertMessage(ctx, legacy, reduxMentions(legacy.Text)))
			require.NoError(t, st.SetSyncState(ctx, sourceName, "read_marker", "CDM", "old"))
			legacyMarkers, err := st.QueryReadOnly(ctx, "select * from sync_state where source_name='desktop' and entity_type='read_marker'")
			require.NoError(t, err)
			require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "CDM", WorkspaceID: "T1", Name: "retained DM", Kind: "im", RawJSON: `{"retained":true}`, UpdatedAt: legacy.UpdatedAt}))
			oldChannels, err := st.QueryReadOnly(ctx, "select * from channels where id = 'CDM'")
			require.NoError(t, err)
			before := map[string][]map[string]any{}
			for _, table := range desktopArchiveTables[3:] {
				if table == "sync_state" {
					continue
				}
				before[table], err = st.QueryReadOnly(ctx, "select * from "+table)
				require.NoError(t, err)
			}
			for n := 0; n < 2; n++ {
				source, err := Ingest(ctx, st, root, IngestOptions{DMPolicy: admission.Exclude, ExcludeChannels: []string{"excluded"}})
				require.NoError(t, err)
				require.Equal(t, 1, source.Local.RecentChannelCount)
				require.Equal(t, 1, source.Local.ReadMarkerCount)
				require.Equal(t, 0, source.Local.ExpandableCount)
				require.Equal(t, 0, source.Summary.DownloadItemCount)
				wantDrafts := 0
				if later == "CPUB" {
					wantDrafts = 1
				}
				require.Equal(t, wantDrafts, source.Local.DraftCount)
				if wantDrafts == 0 {
					requireNoDesktopCanary(t, st, "draft-canary")
				}
				requireNoDesktopCanary(t, st, "unattributed-canary", "download-canary")
				marker, err := st.GetSyncState(ctx, sourceName, "read_marker", "CDM")
				require.NoError(t, err)
				require.Equal(t, "old", marker)
				retainedMarkers, err := st.QueryReadOnly(ctx, "select * from sync_state where source_name='desktop' and entity_type='read_marker'")
				require.NoError(t, err)
				require.Equal(t, legacyMarkers, retainedMarkers)
				qualified, err := st.QueryReadOnly(ctx, "select entity_id,value from sync_state where source_name='desktop' and entity_type='read_marker_v1'")
				require.NoError(t, err)
				require.Equal(t, []map[string]any{{"entity_id": `["T1","CPUB"]`, "value": "1"}}, qualified)
				channels, err := st.QueryReadOnly(ctx, "select * from channels where id = 'CDM'")
				require.NoError(t, err)
				require.Equal(t, oldChannels, channels)
				for table, oldRows := range before {
					rows, err := st.QueryReadOnly(ctx, "select * from "+table)
					require.NoError(t, err)
					for _, old := range oldRows {
						require.Contains(t, rows, old, table)
					}
				}
			}
		})
	}
}

func TestDesktopAdmissionKeepsUnfilteredWorkspaceCandidates(t *testing.T) {
	root := t.TempDir()
	for _, workspace := range []string{"T2", "T3"} {
		state := admissionState("CSHARED", map[string]any{"is_channel": true})
		state["selfTeamIds"] = map[string]any{"teamId": workspace}
		state["channels"].(map[string]any)["CSHARED"].(map[string]any)["context_team_id"] = workspace
		state["messages"] = map[string]any{}
		writeAdmissionBlob(t, root, workspace, state)
	}
	state := admissionState("CSHARED", nil)
	state["channels"] = map[string]any{}
	writeAdmissionBlob(t, root, "T1", state)
	st := admissionStore(t)
	source, err := Ingest(context.Background(), st, root, IngestOptions{DMPolicy: admission.Exclude, WorkspaceID: "T2"})
	require.NoError(t, err)
	require.Positive(t, source.Admission.AmbiguousWorkspace)
	requireNoDesktopCanary(t, st, "intake-canary")
}

func TestDesktopAdmissionPreservesResolvedWorkspace(t *testing.T) {
	for _, shape := range []string{"team context", "enterprise context", "default workspace", "sole candidate", "matching fallback"} {
		t.Run(shape, func(t *testing.T) {
			root := t.TempDir()
			state := admissionState("CPUB", map[string]any{"is_channel": true})
			channel := state["channels"].(map[string]any)["CPUB"].(map[string]any)
			workspace := "T1"
			switch shape {
			case "team context":
				workspace = "T2"
				channel["context_team_id"] = workspace
			case "enterprise context":
				channel["context_team_id"] = "E1"
			case "default workspace":
				state["selfTeamIds"] = map[string]any{"defaultWorkspaceId": workspace}
			case "sole candidate", "matching fallback":
				state["channels"] = map[string]any{}
				metadata := admissionState("CPUB", map[string]any{"is_channel": true, "context_team_id": "T2"})
				metadata["selfTeamIds"] = map[string]any{"teamId": "T2"}
				metadata["messages"] = map[string]any{}
				writeAdmissionBlob(t, root, "metadata2", metadata)
				if shape == "sole candidate" {
					workspace = "T2"
				} else {
					metadata["selfTeamIds"] = map[string]any{"teamId": "T1"}
					metadata["bootData"] = map[string]any{"user_id": "U2"}
					metadata["channels"].(map[string]any)["CPUB"].(map[string]any)["context_team_id"] = "T1"
					writeAdmissionBlob(t, root, "metadata1", metadata)
				}
			}
			state["messages"].(map[string]any)["CPUB"].(map[string]any)["1710000001.000001"].(map[string]any)["user"] = "UEXTERNAL"
			state["members"] = map[string]any{"UEXTERNAL": map[string]any{"id": "UEXTERNAL", "team_id": "TEXTERNAL", "name": "external"}}
			writeAdmissionBlob(t, root, "state", state)
			db, err := leveldb.OpenFile(filepath.Join(root, localStorageDir), nil)
			require.NoError(t, err)
			require.NoError(t, db.Put([]byte("_https://app.slack.compersist-v1::T1::U1::recentlyJoinedChannels"), []byte(`{"CPUB":{}}`), nil))
			require.NoError(t, db.Put([]byte("_https://app.slack.compersist-v1::T1::U1::persistedApiCalls"), []byte(`{"mark":{"method":"conversations.mark","args":{"channel":"CPUB","ts":"123"}}}`), nil))
			require.NoError(t, db.Close())
			for _, policy := range []admission.DMPolicy{admission.Default, admission.Include, admission.Exclude} {
				st := admissionStore(t)
				source, err := Ingest(context.Background(), st, root, IngestOptions{DMPolicy: policy, WorkspaceID: workspace})
				require.NoError(t, err)
				require.Equal(t, 1, source.Local.RecentChannelCount)
				require.Equal(t, 1, source.Local.ReadMarkerCount)
				rows, err := st.Messages(context.Background(), "", "CPUB", "", 10)
				require.NoError(t, err)
				require.Len(t, rows, 1)
				require.Equal(t, workspace, rows[0].WorkspaceID)
				require.Equal(t, "UEXTERNAL", rows[0].UserID)
				owner, err := st.ChannelWorkspaceID(context.Background(), "CPUB")
				require.NoError(t, err)
				require.Equal(t, workspace, owner)
				key := `["T1","CPUB"]`
				if workspace == "T2" {
					key = `["T2","CPUB"]`
				}
				marker, err := st.GetSyncState(context.Background(), sourceName, "read_marker_v1", key)
				require.NoError(t, err)
				require.Equal(t, "123", marker)
				markerRows, err := st.QueryReadOnly(context.Background(), "select entity_type,entity_id,value from sync_state where source_name='desktop' and entity_type in ('read_marker','read_marker_v1')")
				require.NoError(t, err)
				require.Equal(t, []map[string]any{{"entity_type": "read_marker_v1", "entity_id": key, "value": "123"}}, markerRows)
				users, err := st.Users(context.Background(), "", "external", 10)
				require.NoError(t, err)
				require.Len(t, users, 1)
				require.Equal(t, "TEXTERNAL", users[0].WorkspaceID)
			}
		})
	}
}

func TestDesktopAdmissionDraftDoesNotGuessFirstWorkspace(t *testing.T) {
	root := t.TempDir()
	for _, workspace := range []string{"T1", "T2"} {
		state := admissionState("CPUB", map[string]any{"is_channel": true, "context_team_id": workspace})
		state["selfTeamIds"] = map[string]any{"teamId": workspace}
		state["messages"] = map[string]any{}
		writeAdmissionBlob(t, root, workspace, state)
	}
	db, err := leveldb.OpenFile(filepath.Join(root, localStorageDir), nil)
	require.NoError(t, err)
	require.NoError(t, db.Put([]byte("_https://app.slack.comlocalConfig_v2"), []byte(`{"teams":{"T1":{"id":"T1","user_id":"U1"},"T2":{"id":"T2","user_id":"U2"}}}`), nil))
	require.NoError(t, db.Put([]byte("_https://app.slack.compersist-v1::::U1::drafts"), []byte(`{"unifiedDrafts":{"draft":{"destinations":[{"channel_id":"CPUB"}],"ops":[{"insert":"ambiguous-draft-canary"}],"last_updated_ts":1710000000}}}`), nil))
	require.NoError(t, db.Close())
	for _, policy := range []admission.DMPolicy{admission.Default, admission.Include, admission.Exclude} {
		st := admissionStore(t)
		source, err := Ingest(context.Background(), st, root, IngestOptions{DMPolicy: policy})
		require.NoError(t, err)
		if policy == admission.Exclude {
			require.Zero(t, source.Local.DraftCount)
			require.Positive(t, source.Admission.AmbiguousWorkspace)
			requireNoDesktopCanary(t, st, "ambiguous-draft-canary")
		} else {
			require.Equal(t, 1, source.Local.DraftCount)
			rows, err := st.Messages(context.Background(), "", "CPUB", "", 10)
			require.NoError(t, err)
			require.Len(t, rows, 1)
		}
	}
}

func TestDesktopAdmissionReportsDecodeOmissions(t *testing.T) {
	root := t.TempDir()
	writeAdmissionBlob(t, root, "valid", admissionState("CPUB", map[string]any{"is_channel": true}))
	writeBlob(t, root, "invalid", []byte{0xff, 0x11, 0x02, 0xff})
	st := admissionStore(t)
	source, err := Ingest(context.Background(), st, root, IngestOptions{DMPolicy: admission.Exclude})
	require.NoError(t, err)
	require.Equal(t, 1, source.Admission.DecodeFailures)
	require.Equal(t, 1, source.Admission.Messages)
	t.Setenv("PATH", t.TempDir())
	st = admissionStore(t)
	source, err = Ingest(context.Background(), st, root, IngestOptions{DMPolicy: admission.Exclude})
	require.NoError(t, err)
	require.Positive(t, source.Admission.DecoderUnavailable)
	require.Equal(t, 0, source.Admission.Messages)
	requireNoDesktopCanary(t, st, "CPUB", "intake-canary")
}

func ingestReduxStates(ctx context.Context, st *store.Store, states []ReduxDecodedState, now time.Time, filter ingestFilter) error {
	summary := AdmissionSummary{}
	a := newDesktopAdmission(states, filter, admission.Default, &summary)
	prepared, err := a.prepareRedux(states)
	if err != nil {
		return err
	}
	return ingestPreparedReduxStates(ctx, st, prepared, now, filter)
}
