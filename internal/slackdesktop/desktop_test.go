package slackdesktop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/golang/snappy"
	"github.com/stretchr/testify/require"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"

	"github.com/vincentkoc/slacrawl/internal/store"
)

func TestLoadRootState(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "desktop", "root-state.json")
	root, err := LoadRootState(path)
	require.NoError(t, err)
	require.Equal(t, 2, root.Summary.WorkspaceCount)
	require.Equal(t, 1, root.Summary.TeamsCount)
	require.Equal(t, 2, len(root.Summary.AppTeamsKeys))
	require.Equal(t, 1, root.Summary.DownloadTeamCount)
	require.Equal(t, 1, root.Summary.DownloadItemCount)
}

func TestParseLocalStorage(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "leveldb")
	db, err := leveldb.OpenFile(dbPath, nil)
	require.NoError(t, err)
	require.NoError(t, db.Put([]byte("_https://app.slack.comlocalConfig_v2"), []byte(`{"teams":{"T111":{"id":"T111","name":"Team One","domain":"team-one","user_id":"U111","token":"xoxc-secret"}}}`), nil))
	require.NoError(t, db.Put([]byte("_https://app.slack.compersist-v1::T111::U111::drafts"), []byte(`{"unifiedDrafts":{"draft-1":{"id":"draft-1","client_draft_id":"draft-1","destinations":[{"channel_id":"C111","thread_ts":"1710000000.000100"}],"ops":[{"insert":"hello "},{"insert":"world"}],"last_updated_ts":1710000001.000200}}}`), nil))
	require.NoError(t, db.Put([]byte("_https://app.slack.comactivitySession_T111"), []byte(`{"session-1":{"id":"session-1","startTime":1,"lastActivity":2,"lastLogged":3}}`), nil))
	require.NoError(t, db.Put([]byte("_https://app.slack.compersist-v1::T111::U111::customStatus"), []byte(`{"status-1":{"id":"status-1","user_id":"U111","text":"Heads down","emoji":":spiral_calendar_pad:","is_active":true,"date_created":10}}`), nil))
	require.NoError(t, db.Put([]byte("_https://app.slack.compersist-v1::T111::U111::persistedApiCalls"), []byte(`{"mark-1":{"method":"conversations.mark","persistKey":"mark-1","reason":"viewed","args":{"channel":"C111","ts":"1710000002.000300"}}}`), nil))
	require.NoError(t, db.Put([]byte("_https://app.slack.compersist-v1::T111::U111::expandables"), []byte(`{"attach_text_1710000002.000300Channel":true,"inline_files_msg_1710000002_123Channel":true}`), nil))
	require.NoError(t, db.Put([]byte("_https://app.slack.compersist-v1::T111::U111::recentlyJoinedChannels"), []byte(`{"C222":{},"C333":{}}`), nil))
	require.NoError(t, db.Close())

	data, err := ParseLocalStorage(dbPath)
	require.NoError(t, err)
	require.Equal(t, 1, data.Summary.WorkspaceCount)
	require.Equal(t, 1, data.Summary.DraftCount)
	require.Equal(t, 1, data.Summary.ActivityTeamCount)
	require.Equal(t, 2, data.Summary.RecentChannelCount)
	require.Equal(t, 1, data.Summary.ReadMarkerCount)
	require.Equal(t, 1, data.Summary.CustomStatusCount)
	require.Equal(t, 2, data.Summary.ExpandableCount)
	require.Equal(t, "Team One", data.LocalConfig.Teams["T111"].Name)
	require.Equal(t, "hello world", draftText(data.Drafts[0]))
	require.Equal(t, "T111", data.Drafts[0].WorkspaceID)
	require.Equal(t, "U111", data.Drafts[0].UserID)
	require.Len(t, data.ReadMarkers, 1)
	require.Equal(t, "C111", data.ReadMarkers[0].ChannelID)
	require.Len(t, data.Statuses, 1)
	require.Equal(t, "Heads down", data.Statuses[0].Statuses[0].Text)
	require.Len(t, data.Expandables, 1)
	require.Equal(t, []string{"attach_text_1710000002.000300Channel", "inline_files_msg_1710000002_123Channel"}, data.Expandables[0].Keys)
}

func TestIngestDesktopState(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "storage"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "Local Storage", "leveldb"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "IndexedDB", "https_app.slack.com_0.indexeddb.leveldb"), 0o750))

	rootStatePath := filepath.Join(root, "storage", "root-state.json")
	rootStateData, err := os.ReadFile(filepath.Join("..", "..", "testdata", "desktop", "root-state.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(rootStatePath, rootStateData, 0o600))

	localDB, err := leveldb.OpenFile(filepath.Join(root, "Local Storage", "leveldb"), nil)
	require.NoError(t, err)
	require.NoError(t, localDB.Put([]byte("_https://app.slack.comlocalConfig_v2"), []byte(`{"teams":{"T111":{"id":"T111","name":"Team One","domain":"team-one","user_id":"U111","token":"xoxc-secret"}}}`), nil))
	require.NoError(t, localDB.Put([]byte("_https://app.slack.compersist-v1::T111::U111::drafts"), []byte(`{"unifiedDrafts":{"draft-1":{"id":"draft-1","client_draft_id":"draft-1","destinations":[{"channel_id":"C111"}],"ops":[{"insert":"draft body"}],"last_updated_ts":1710000001.000200}}}`), nil))
	require.NoError(t, localDB.Put([]byte("_https://app.slack.compersist-v1::T111::U111::recentlyJoinedChannels"), []byte(`{"C222":{}}`), nil))
	require.NoError(t, localDB.Put([]byte("_https://app.slack.compersist-v1::T111::U111::customStatus"), []byte(`{"status-1":{"id":"status-1","user_id":"U111","text":"Travel","emoji":":airplane:","is_active":true,"date_created":10}}`), nil))
	require.NoError(t, localDB.Put([]byte("_https://app.slack.compersist-v1::T111::U111::persistedApiCalls"), []byte(`{"mark-1":{"method":"conversations.mark","persistKey":"mark-1","reason":"viewed","args":{"channel":"C333","ts":"1710000003.000400"}}}`), nil))
	require.NoError(t, localDB.Put([]byte("_https://app.slack.compersist-v1::T111::U111::expandables"), []byte(`{"attach_text_1710000003.000400Channel":true}`), nil))
	require.NoError(t, localDB.Close())

	indexDB, err := leveldb.OpenFile(filepath.Join(root, "IndexedDB", "https_app.slack.com_0.indexeddb.leveldb"), &opt.Options{Comparer: indexedDBComparer{}})
	require.NoError(t, err)
	require.NoError(t, indexDB.Put([]byte("https_app.slack.com_0@1#objectStore-T111-U111"), []byte("A"), nil))
	require.NoError(t, indexDB.Close())

	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	snapshotParent := withSnapshotTempParent(t)
	source, err := Ingest(context.Background(), st, root)
	require.NoError(t, err)
	require.True(t, source.Available)
	require.Empty(t, source.Snapshot)
	requireEmptyDir(t, snapshotParent)
	require.Equal(t, 1, source.Local.DraftCount)
	require.Len(t, source.IndexedDB.ObjectStores, 1)

	status, err := st.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, status.Workspaces)
	require.Equal(t, 1, status.Users)
	require.Equal(t, 1, status.Messages)

	channels, err := st.Channels(context.Background(), "", "", 10)
	require.NoError(t, err)
	require.Len(t, channels, 3)

	users, err := st.Users(context.Background(), "", "", 10)
	require.NoError(t, err)
	require.Len(t, users, 1)
	require.Equal(t, "desktop_local_user | :airplane: Travel", users[0].Title)

	readTS, err := st.GetSyncState(context.Background(), sourceName, "read_marker", "C333")
	require.NoError(t, err)
	require.Equal(t, "1710000003.000400", readTS)

	expandableCount, err := st.GetSyncState(context.Background(), sourceName, "expandables", "T111:U111")
	require.NoError(t, err)
	require.Equal(t, "1", expandableCount)
}

func TestIngestDesktopDraftUsesPersistWorkspace(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "Local Storage", "leveldb"), 0o750))

	localDB, err := leveldb.OpenFile(filepath.Join(root, "Local Storage", "leveldb"), nil)
	require.NoError(t, err)
	require.NoError(t, localDB.Put([]byte("_https://app.slack.comlocalConfig_v2"), []byte(`{"teams":{"T111":{"id":"T111","name":"Team One","domain":"team-one","user_id":"U111","token":"xoxc-secret"}}}`), nil))
	require.NoError(t, localDB.Put([]byte("_https://app.slack.compersist-v1::T222::U222::drafts"), []byte(`{"unifiedDrafts":{"draft-1":{"id":"draft-1","client_draft_id":"draft-1","destinations":[{"channel_id":"C111"}],"ops":[{"insert":"draft body"}],"last_updated_ts":1710000001.000200}}}`), nil))
	require.NoError(t, localDB.Close())

	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	_, err = Ingest(context.Background(), st, root)
	require.NoError(t, err)

	messages, err := st.Messages(context.Background(), "", "C111", "", 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "T222", messages[0].WorkspaceID)
	require.Equal(t, "U222", messages[0].UserID)
}

func TestSnapshotPathRemovesPartialSnapshotOnError(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "storage"), 0o750))
	loopPath := filepath.Join(root, "storage", "root-state.json")
	if err := os.Symlink("root-state.json", loopPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	snapshotParent := withSnapshotTempParent(t)
	_, err := SnapshotPath(root)
	require.Error(t, err)
	requireEmptyDir(t, snapshotParent)
}

func TestExtractIndexedDBStates(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for redux blob decoding")
	}

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "IndexedDB", "https_app.slack.com_0.indexeddb.leveldb"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "IndexedDB", "https_app.slack.com_0.indexeddb.blob", "1", "cd"), 0o750))

	payloadPath := filepath.Join(root, "redux.bin")
	//nolint:gosec // Test builds a controlled V8 fixture with node.
	cmd := exec.Command("node", "-e", `
const fs = require("fs");
const v8 = require("v8");
const value = {
  selfTeamIds: {
    teamId: "T111",
    defaultWorkspaceId: "T111"
  },
  bootData: {
    user_id: "U111"
  },
  channels: {
    C111: { id: "C111", name: "general", is_channel: true, is_private: false, is_archived: false, is_general: true, context_team_id: "T111", topic: { value: "hello" }, purpose: { value: "world" } }
  },
  members: {
    U111: { id: "U111", name: "vincent", team_id: "T111", real_name: "Vincent", is_bot: false, deleted: false, profile: { real_name: "Vincent", display_name: "Vin", title: "Founder" } }
  },
  messages: {
    C111: {
      "1710000001.000200": {
        channel: "C111",
        ts: "1710000001.000200",
        type: "message",
        user: "U111",
        text: "hello <@U222|alice>",
        reply_count: 1,
        latest_reply: "1710000002.000300",
        replies: {
          "1710000002.000300": {
            user: "U111",
            thread_ts: "1710000001.000200",
            parent_user_id: "U111",
            text: "thread reply"
          }
        }
      }
    }
  },
  threads: {
    C111: {
      "1710000001.000200": {
        messages: [
          {
            channel: "C111",
            ts: "1710000002.000300",
            type: "message",
            user: "U111",
            thread_ts: "1710000001.000200",
            parent_user_id: "U111",
            text: "thread reply"
          }
        ]
      }
    }
  }
};
fs.writeFileSync(process.argv[1], v8.serialize(value));
`, payloadPath)
	require.NoError(t, cmd.Run())

	serialized, err := os.ReadFile(payloadPath) //nolint:gosec // Test reads the payload it just wrote to t.TempDir.
	require.NoError(t, err)
	blobPayload := append([]byte{0xff, 0x11, 0x02}, snappy.Encode(nil, serialized)...)
	require.NoError(t, os.WriteFile(filepath.Join(root, "IndexedDB", "https_app.slack.com_0.indexeddb.blob", "1", "cd", "cd9a"), blobPayload, 0o600))

	states, err := ExtractIndexedDBStates(root)
	require.NoError(t, err)
	require.Len(t, states, 1)
	require.Equal(t, "T111", states[0].WorkspaceID)
	require.Equal(t, "U111", states[0].UserID)
	require.Len(t, states[0].Channels, 1)
	require.Len(t, states[0].Members, 1)
	require.Len(t, states[0].Messages, 2)
	require.Equal(t, "general", states[0].Channels[0].Name)
	byTS := map[string]ReduxMessage{}
	for _, message := range states[0].Messages {
		byTS[message.TS] = message
	}
	require.Equal(t, "hello <@U222|alice>", byTS["1710000001.000200"].Text)
	require.Equal(t, "1710000001.000200", byTS["1710000002.000300"].ThreadTS)
	require.Equal(t, "thread reply", byTS["1710000002.000300"].Text)
}

func TestNormalizeReduxMessageIncludesBlocksAndAttachments(t *testing.T) {
	normalized := normalizeReduxMessage(ReduxMessage{
		Channel: "C111",
		TS:      "1710000001.000200",
		Type:    "message",
		Blocks: []any{
			map[string]any{
				"type": "section",
				"text": map[string]any{"type": "mrkdwn", "text": "block body <@U123|ada>"},
			},
		},
		Attachments: []any{
			map[string]any{
				"fallback": "attachment fallback",
				"fields": []any{
					map[string]any{"title": "impact", "value": "customer visible"},
				},
			},
		},
	})

	require.Contains(t, normalized, "block body @ada")
	require.Contains(t, normalized, "attachment fallback")
	require.Contains(t, normalized, "customer visible")
}

func TestInspectIncludesSnapshotDerivedDesktopSummaries(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "storage"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "Local Storage", "leveldb"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "IndexedDB", "https_app.slack.com_0.indexeddb.leveldb"), 0o750))

	rootStateData, err := os.ReadFile(filepath.Join("..", "..", "testdata", "desktop", "root-state.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "storage", "root-state.json"), rootStateData, 0o600))

	localDB, err := leveldb.OpenFile(filepath.Join(root, "Local Storage", "leveldb"), nil)
	require.NoError(t, err)
	require.NoError(t, localDB.Put([]byte("_https://app.slack.comlocalConfig_v2"), []byte(`{"teams":{"T111":{"id":"T111","name":"Team One","domain":"team-one","user_id":"U111"}}}`), nil))
	require.NoError(t, localDB.Put([]byte("_https://app.slack.compersist-v1::T111::U111::drafts"), []byte(`{"unifiedDrafts":{"draft-1":{"id":"draft-1","client_draft_id":"draft-1","destinations":[{"channel_id":"C111"}],"ops":[{"insert":"draft body"}],"last_updated_ts":1710000001.000200}}}`), nil))
	require.NoError(t, localDB.Close())

	indexDB, err := leveldb.OpenFile(filepath.Join(root, "IndexedDB", "https_app.slack.com_0.indexeddb.leveldb"), &opt.Options{Comparer: indexedDBComparer{}})
	require.NoError(t, err)
	require.NoError(t, indexDB.Put([]byte("https_app.slack.com_0@1#objectStore-T111-U111"), []byte("A"), nil))
	require.NoError(t, indexDB.Close())

	source, err := Inspect(root)
	require.NoError(t, err)
	require.True(t, source.Available)
	require.Equal(t, 1, source.Local.WorkspaceCount)
	require.Equal(t, 1, source.Local.DraftCount)
	require.Len(t, source.IndexedDB.ObjectStores, 1)
}

func withSnapshotTempParent(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	previous := makeSnapshotTempDir
	makeSnapshotTempDir = func(_ string, pattern string) (string, error) {
		return os.MkdirTemp(parent, pattern)
	}
	t.Cleanup(func() {
		makeSnapshotTempDir = previous
	})
	return parent
}

func requireEmptyDir(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	require.NoError(t, err)
	require.Empty(t, entries)
}
