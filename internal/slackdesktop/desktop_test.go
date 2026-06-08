package slackdesktop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/stretchr/testify/require"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"

	"github.com/openclaw/slacrawl/internal/store"
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

func TestDraftTSIncludesWorkspace(t *testing.T) {
	require.Equal(t, "draft:1710000001.0002:T111:C111", draftTS(Draft{
		WorkspaceID:   "T111",
		ClientDraftID: "C111",
		LastUpdatedTS: 1710000001.000200,
	}))
	require.Equal(t, "draft:1710000001.0002:T222:C111", draftTS(Draft{
		WorkspaceID:   "T222",
		ClientDraftID: "C111",
		LastUpdatedTS: 1710000001.000200,
	}))
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
	source, err := Ingest(context.Background(), st, root, IngestOptions{})
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

func TestIngestDesktopStateRespectsWorkspaceFilter(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "storage"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "Local Storage", "leveldb"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "storage", "root-state.json"), []byte(`{"appTeams":{"T111":{},"T222":{}}}`), 0o600))

	localDB, err := leveldb.OpenFile(filepath.Join(root, "Local Storage", "leveldb"), nil)
	require.NoError(t, err)
	require.NoError(t, localDB.Put([]byte("_https://app.slack.comlocalConfig_v2"), []byte(`{"teams":{"T111":{"id":"T111","name":"Team One","domain":"team-one","user_id":"U111"},"T222":{"id":"T222","name":"Team Two","domain":"team-two","user_id":"U222"}}}`), nil))
	require.NoError(t, localDB.Put([]byte("_https://app.slack.compersist-v1::T111::U111::drafts"), []byte(`{"unifiedDrafts":{"draft-1":{"id":"draft-1","client_draft_id":"draft-1","destinations":[{"channel_id":"C000000111"}],"ops":[{"insert":"wrong workspace"}],"last_updated_ts":1710000001.000200}}}`), nil))
	require.NoError(t, localDB.Put([]byte("_https://app.slack.compersist-v1::T222::U222::drafts"), []byte(`{"unifiedDrafts":{"draft-2":{"id":"draft-2","client_draft_id":"draft-2","destinations":[{"channel_id":"C000000222"}],"ops":[{"insert":"kept draft"}],"last_updated_ts":1710000002.000200},"draft-3":{"id":"draft-3","client_draft_id":"draft-3","destinations":[{"channel_id":"C000000223"}],"ops":[{"insert":"excluded draft"}],"last_updated_ts":1710000003.000200}}}`), nil))
	require.NoError(t, localDB.Put([]byte("_https://app.slack.compersist-v1::T111::U111::recentlyJoinedChannels"), []byte(`{"C000000111":{}}`), nil))
	require.NoError(t, localDB.Put([]byte("_https://app.slack.compersist-v1::T222::U222::recentlyJoinedChannels"), []byte(`{"C000000222":{},"C000000223":{}}`), nil))
	require.NoError(t, localDB.Put([]byte("_https://app.slack.compersist-v1::T111::U111::customStatus"), []byte(`{"status-1":{"id":"status-1","user_id":"U111","text":"Wrong workspace","is_active":true,"date_created":10}}`), nil))
	require.NoError(t, localDB.Put([]byte("_https://app.slack.compersist-v1::T222::U222::customStatus"), []byte(`{"status-2":{"id":"status-2","user_id":"U222","text":"Kept workspace","is_active":true,"date_created":10}}`), nil))
	require.NoError(t, localDB.Put([]byte("_https://app.slack.compersist-v1::T222::U222::persistedApiCalls"), []byte(`{"mark-1":{"method":"conversations.mark","persistKey":"mark-1","reason":"viewed","args":{"channel":"C000000222","ts":"1710000002.000300"}},"mark-2":{"method":"conversations.mark","persistKey":"mark-2","reason":"viewed","args":{"channel":"C000000223","ts":"1710000003.000300"}}}`), nil))
	require.NoError(t, localDB.Close())

	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	source, err := Ingest(context.Background(), st, root, IngestOptions{WorkspaceID: "T222"})
	require.NoError(t, err)
	require.Equal(t, []string{"T222"}, source.Summary.AppTeamsKeys)

	status, err := st.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, status.Workspaces)
	require.Equal(t, 1, status.Users)
	require.Equal(t, 2, status.Messages)

	channels, err := st.Channels(context.Background(), "T222", "", 10)
	require.NoError(t, err)
	require.Len(t, channels, 2)

	channels, err = st.Channels(context.Background(), "T111", "", 10)
	require.NoError(t, err)
	require.Empty(t, channels)

	messages, err := st.Messages(context.Background(), "", "", "", 10)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	for _, message := range messages {
		require.Equal(t, "T222", message.WorkspaceID)
	}

	readTS, err := st.GetSyncState(context.Background(), sourceName, "read_marker", "C000000222")
	require.NoError(t, err)
	require.Equal(t, "1710000002.000300", readTS)
	appTeams, err := st.GetSyncState(context.Background(), sourceName, "root_state", "app_teams")
	require.NoError(t, err)
	require.Equal(t, "T222", appTeams)
	_, err = st.GetSyncState(context.Background(), sourceName, "custom_status", "T111:U111")
	require.Error(t, err)
}

func TestIngestDesktopStateSkipsUnknownChannelsForNameExcludes(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "Local Storage", "leveldb"), 0o750))

	localDB, err := leveldb.OpenFile(filepath.Join(root, "Local Storage", "leveldb"), nil)
	require.NoError(t, err)
	require.NoError(t, localDB.Put([]byte("_https://app.slack.comlocalConfig_v2"), []byte(`{"teams":{"T111":{"id":"T111","name":"Team One","domain":"team-one","user_id":"U111"}}}`), nil))
	require.NoError(t, localDB.Put([]byte("_https://app.slack.compersist-v1::T111::U111::drafts"), []byte(`{"unifiedDrafts":{"draft-1":{"id":"draft-1","client_draft_id":"draft-1","destinations":[{"channel_id":"C111"}],"ops":[{"insert":"unknown channel draft"}],"last_updated_ts":1710000001.000200}}}`), nil))
	require.NoError(t, localDB.Put([]byte("_https://app.slack.compersist-v1::T111::U111::recentlyJoinedChannels"), []byte(`{"C111":{}}`), nil))
	require.NoError(t, localDB.Put([]byte("_https://app.slack.compersist-v1::T111::U111::persistedApiCalls"), []byte(`{"mark-1":{"method":"conversations.mark","persistKey":"mark-1","reason":"viewed","args":{"channel":"C111","ts":"1710000001.000300"}}}`), nil))
	require.NoError(t, localDB.Close())

	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	source, err := Ingest(context.Background(), st, root, IngestOptions{
		ExcludeChannels: []string{"GENERAL"},
	})
	require.NoError(t, err)
	require.True(t, source.Available)

	status, err := st.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, status.Workspaces)
	require.Equal(t, 1, status.Users)
	require.Equal(t, 0, status.Channels)
	require.Equal(t, 0, status.Messages)

	_, err = st.GetSyncState(context.Background(), sourceName, "read_marker", "C111")
	require.Error(t, err)
}

func TestIngestReduxStatesSkipsUnknownChannelsForNameExcludesWithWorkspaceFilter(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	filter := newIngestFilter(IngestOptions{
		WorkspaceID:     "T111",
		ExcludeChannels: []string{"general"},
	})
	require.NoError(t, ingestReduxStates(ctx, st, []ReduxDecodedState{{
		WorkspaceID: "T111",
		Messages: []ReduxMessage{{
			Channel: "C111",
			TS:      "1710000001.000200",
			Type:    "message",
			Text:    "metadata-less message",
		}},
	}}, time.Now().UTC(), filter))

	status, err := st.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, status.Workspaces)
	require.Equal(t, 0, status.Messages)
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

	_, err = Ingest(context.Background(), st, root, IngestOptions{})
	require.NoError(t, err)

	messages, err := st.Messages(context.Background(), "", "C111", "", 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "T222", messages[0].WorkspaceID)
	require.Equal(t, "U222", messages[0].UserID)
}

func TestDiscoverEmptyPathIsUnavailable(t *testing.T) {
	source, err := Discover("")
	require.NoError(t, err)
	require.False(t, source.Available)
	require.Empty(t, source.Path)
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
    C111: { id: "C111", name: "general", is_channel: true, is_private: false, is_archived: false, is_general: true, context_team_id: "T111", topic: { value: "hello" }, purpose: { value: "world" } },
    D111: { id: "D111", user: "U222", is_im: true, is_private: true, context_team_id: "T111" }
  },
  members: {
    U111: { id: "U111", name: "vincent", team_id: "T111", real_name: "Vincent", is_bot: false, deleted: false, profile: { real_name: "Vincent", display_name: "Vin", title: "Founder" } },
    U222: { id: "U222", name: "mike", team_id: "T111", real_name: "Mike", is_bot: false, deleted: false, profile: { real_name: "Mike", display_name: "mike", title: "EA wrangler" } }
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
    },
    D111: {
      "1710000003.000400": {
        channel: "D111",
        ts: "1710000003.000400",
        type: "message",
        user: "U222",
        text: "What's the best way to coordinate meetings?"
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
	require.Len(t, states[0].Channels, 2)
	require.Len(t, states[0].Members, 2)
	require.Len(t, states[0].Messages, 3)
	require.Equal(t, "general", states[0].Channels[0].Name)
	byTS := map[string]ReduxMessage{}
	for _, message := range states[0].Messages {
		byTS[message.TS] = message
	}
	require.Equal(t, "hello <@U222|alice>", byTS["1710000001.000200"].Text)
	require.Equal(t, "1710000001.000200", byTS["1710000002.000300"].ThreadTS)
	require.Equal(t, "thread reply", byTS["1710000002.000300"].Text)
	require.Equal(t, "D111", byTS["1710000003.000400"].Channel)
}

func TestIngestReduxStatesIncludesIMs(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, ingestReduxStates(ctx, st, []ReduxDecodedState{{
		WorkspaceID: "T111",
		UserID:      "U111",
		Channels: []ReduxChannel{{
			ID:            "D111",
			User:          "U222",
			IsIM:          true,
			IsPrivate:     true,
			ContextTeamID: "T111",
		}},
		Members: []ReduxMember{{
			ID:     "U222",
			Name:   "mike",
			TeamID: "T111",
			Profile: ReduxMemberProfile{
				RealName:    "Mike",
				DisplayName: "mike",
			},
		}},
		Messages: []ReduxMessage{{
			Channel: "D111",
			TS:      "1710000003.000400",
			Type:    "message",
			User:    "U222",
			Text:    "What's the best way to coordinate meetings?",
		}},
	}}, now, ingestFilter{}))

	channels, err := st.Channels(context.Background(), "", "", 10)
	require.NoError(t, err)
	require.Len(t, channels, 1)
	require.Equal(t, "desktop_im", channels[0].Kind)
	require.Equal(t, "mike", channels[0].Name)

	rows, err := st.SearchMessages(ctx, store.SearchOptions{Query: "What's the best way", Mode: store.SearchModeAuto, Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "D111", rows[0].ChannelID)
	require.Equal(t, "mike", rows[0].ChannelName)
}

func TestIngestReduxStatesRespectsWorkspaceAndChannelFilters(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	now := time.Now().UTC()
	filter := newIngestFilter(IngestOptions{
		WorkspaceID:     "T111",
		ExcludeChannels: []string{"#EXCLUDED"},
	})
	require.NoError(t, ingestReduxStates(ctx, st, []ReduxDecodedState{
		{
			WorkspaceID: "T111",
			UserID:      "U111",
			Channels: []ReduxChannel{
				{ID: "C111", Name: "kept", IsChannel: true, ContextTeamID: "T111"},
				{ID: "C222", Name: "excluded", IsChannel: true, ContextTeamID: "T111"},
			},
			Members: []ReduxMember{
				{ID: "U111", Name: "alice", TeamID: "T111"},
			},
			Messages: []ReduxMessage{
				{Channel: "C111", TS: "1710000001.000200", Type: "message", User: "U111", Text: "kept message"},
				{Channel: "C222", TS: "1710000002.000200", Type: "message", User: "U111", Text: "excluded message"},
			},
		},
		{
			WorkspaceID: "T222",
			UserID:      "U222",
			Channels: []ReduxChannel{
				{ID: "C333", Name: "wrong-workspace", IsChannel: true, ContextTeamID: "T222"},
			},
			Members: []ReduxMember{
				{ID: "U222", Name: "bob", TeamID: "T222"},
			},
			Messages: []ReduxMessage{
				{Channel: "C333", TS: "1710000003.000200", Type: "message", User: "U222", Text: "wrong workspace"},
			},
		},
		{
			WorkspaceID: "T222",
			UserID:      "U555",
			Channels: []ReduxChannel{
				{ID: "C555", Name: "cross-context", IsChannel: true, ContextTeamID: "T111"},
			},
			Members: []ReduxMember{
				{ID: "U555", Name: "cross-user", TeamID: "T111"},
				{ID: "U_EXTERNAL", Name: "external-user", TeamID: "T_EXTERNAL"},
			},
			Messages: []ReduxMessage{
				{Channel: "C555", TS: "1710000005.000200", Type: "message", User: "U555", Text: "cross workspace message"},
				{Channel: "C555", TS: "1710000006.000200", Type: "message", User: "U_EXTERNAL", Text: "external user message"},
			},
		},
	}, now, filter))

	status, err := st.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, status.Workspaces)
	require.Equal(t, 2, status.Channels)
	require.Equal(t, 3, status.Users)
	require.Equal(t, 3, status.Messages)

	messages, err := st.Messages(ctx, "", "", "", 10)
	require.NoError(t, err)
	require.Len(t, messages, 3)
	byChannel := map[string]store.MessageRow{}
	for _, message := range messages {
		byChannel[message.ChannelID+":"+message.TS] = message
	}
	require.Equal(t, "T111", byChannel["C111:1710000001.000200"].WorkspaceID)
	require.Equal(t, "kept message", byChannel["C111:1710000001.000200"].Text)
	require.Equal(t, "T111", byChannel["C555:1710000005.000200"].WorkspaceID)
	require.Equal(t, "cross workspace message", byChannel["C555:1710000005.000200"].Text)
	require.Equal(t, "T111", byChannel["C555:1710000006.000200"].WorkspaceID)
	require.Equal(t, "external user message", byChannel["C555:1710000006.000200"].Text)

	users, err := st.Users(ctx, "", "external-user", 10)
	require.NoError(t, err)
	require.Len(t, users, 1)
	require.Equal(t, "T_EXTERNAL", users[0].WorkspaceID)

	channels, err := st.Channels(ctx, "", "", 10)
	require.NoError(t, err)
	require.Len(t, channels, 2)
}

func TestIngestReduxStatesUsesGlobalChannelWorkspaceForMetadataLessMessages(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, ingestReduxStates(ctx, st, []ReduxDecodedState{
		{
			WorkspaceID: "T222",
			Channels: []ReduxChannel{{
				ID:            "C999",
				Name:          "connect",
				IsChannel:     true,
				ContextTeamID: "T222",
			}},
		},
		{
			WorkspaceID: "T111",
			Messages: []ReduxMessage{{
				Channel: "C999",
				TS:      "1710000009.000200",
				Type:    "message",
				User:    "U111",
				Text:    "metadata in another state",
			}},
		},
	}, now, ingestFilter{}))

	messages, err := st.Messages(ctx, "", "C999", "", 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "T222", messages[0].WorkspaceID)

	filtered, err := store.Open(filepath.Join(t.TempDir(), "filtered.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, filtered.Close()) }()
	filter := newIngestFilter(IngestOptions{WorkspaceID: "T111"})
	require.NoError(t, ingestReduxStates(ctx, filtered, []ReduxDecodedState{
		{
			WorkspaceID: "T222",
			Channels: []ReduxChannel{{
				ID:            "C999",
				Name:          "connect",
				IsChannel:     true,
				ContextTeamID: "T222",
			}},
		},
		{
			WorkspaceID: "T111",
			Messages: []ReduxMessage{{
				Channel: "C999",
				TS:      "1710000009.000200",
				Type:    "message",
				User:    "U111",
				Text:    "metadata in another state",
			}},
		},
	}, now, filter))
	status, err := filtered.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, status.Messages)
}

func TestIngestReduxStatesUsesStateWorkspaceForEnterpriseContext(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	now := time.Now().UTC()
	state := ReduxDecodedState{
		WorkspaceID: "T111",
		Channels: []ReduxChannel{{
			ID:            "C111",
			Name:          "grid-channel",
			IsChannel:     true,
			ContextTeamID: "E111",
		}},
		Messages: []ReduxMessage{{
			Channel: "C111",
			TS:      "1710000001.000200",
			Type:    "message",
			Text:    "enterprise context message",
		}},
	}
	filter := newIngestFilter(IngestOptions{WorkspaceID: "T111"})
	require.NoError(t, ingestReduxStates(ctx, st, []ReduxDecodedState{state}, now, filter))

	messages, err := st.Messages(ctx, "", "C111", "", 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "T111", messages[0].WorkspaceID)

	hints := channelNamesByWorkspaceID([]ReduxDecodedState{state})
	require.Equal(t, []string{"grid-channel"}, hints.get("T111", "C111"))
	require.Empty(t, hints.get("E111", "C111"))
}

func TestIngestReduxStatesUsesMatchingStateWorkspaceForConflictedChannel(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, ingestReduxStates(ctx, st, []ReduxDecodedState{
		{
			WorkspaceID: "T222",
			Channels: []ReduxChannel{{
				ID:            "C999",
				Name:          "connect",
				IsChannel:     true,
				ContextTeamID: "T222",
			}},
		},
		{
			WorkspaceID: "T333",
			Channels: []ReduxChannel{{
				ID:            "C999",
				Name:          "connect",
				IsChannel:     true,
				ContextTeamID: "T333",
			}},
		},
		{
			WorkspaceID: "T222",
			Messages: []ReduxMessage{{
				Channel: "C999",
				TS:      "1710000008.000200",
				Type:    "message",
				User:    "U222",
				Text:    "matching conflicted workspace",
			}},
		},
		{
			WorkspaceID: "T111",
			Messages: []ReduxMessage{{
				Channel: "C999",
				TS:      "1710000009.000200",
				Type:    "message",
				User:    "U111",
				Text:    "conflicted workspace",
			}},
		},
	}, now, ingestFilter{}))

	status, err := st.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, status.Messages)
	messages, err := st.Messages(ctx, "", "C999", "", 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "T222", messages[0].WorkspaceID)
	require.Equal(t, "matching conflicted workspace", messages[0].Text)
}

func TestResolveChannelWorkspaceUsesContextCandidates(t *testing.T) {
	candidates := channelWorkspaceCandidates([]ReduxDecodedState{
		{
			WorkspaceID: "T222",
			Channels: []ReduxChannel{{
				ID:            "C111",
				ContextTeamID: "T111",
			}},
		},
		{
			WorkspaceID: "T333",
			Channels: []ReduxChannel{{
				ID:            "C222",
				ContextTeamID: "T333",
			}},
		},
		{
			WorkspaceID: "T444",
			Channels: []ReduxChannel{{
				ID:            "C222",
				ContextTeamID: "T444",
			}},
		},
	})

	workspaceID, ok := resolveChannelWorkspace("C111", "T222", candidates)
	require.True(t, ok)
	require.Equal(t, "T111", workspaceID)

	workspaceID, ok = resolveChannelWorkspace("C222", "T333", candidates)
	require.True(t, ok)
	require.Equal(t, "T333", workspaceID)

	_, ok = resolveChannelWorkspace("C222", "T111", candidates)
	require.False(t, ok)

	workspaceID, ok = resolveChannelWorkspace("C333", "T555", candidates)
	require.True(t, ok)
	require.Equal(t, "T555", workspaceID)
}

func TestChannelNameHintsAreWorkspaceQualifiedAndDerived(t *testing.T) {
	hints := channelNamesByWorkspaceID([]ReduxDecodedState{
		{
			WorkspaceID: "T111",
			Channels: []ReduxChannel{{
				ID:            "D111",
				User:          "U111",
				IsIM:          true,
				ContextTeamID: "T111",
			}},
			Members: []ReduxMember{{
				ID:     "U111",
				Name:   "alice",
				TeamID: "T111",
			}},
		},
		{
			WorkspaceID: "T222",
			Channels: []ReduxChannel{{
				ID:            "D111",
				Name:          "ops",
				IsChannel:     true,
				ContextTeamID: "T222",
			}},
		},
		{
			WorkspaceID: "T333",
			Channels: []ReduxChannel{{
				ID:            "G111",
				Name:          "mpdm-alice--bob-1",
				IsMPIM:        true,
				Members:       []string{"U2", "U1"},
				ContextTeamID: "T333",
			}},
			Members: []ReduxMember{
				{ID: "U1", Name: "alice", TeamID: "T333"},
				{ID: "U2", Name: "bob", TeamID: "T333"},
			},
		},
		{
			WorkspaceID: "T444",
			Channels: []ReduxChannel{{
				ID:            "G222",
				Name:          "mpdm-alice--unknown-1",
				IsMPIM:        true,
				Members:       []string{"U1", "U_MISSING"},
				ContextTeamID: "T444",
			}},
			Members: []ReduxMember{
				{ID: "U1", Name: "alice", TeamID: "T444"},
			},
		},
		{
			WorkspaceID: "T666",
			Channels: []ReduxChannel{{
				ID:            "C777",
				Name:          "ops",
				IsChannel:     true,
				ContextTeamID: "T666",
			}},
		},
		{
			WorkspaceID: "T666",
			Channels: []ReduxChannel{{
				ID:            "C777",
				Name:          "ops-archive",
				IsChannel:     true,
				ContextTeamID: "T666",
			}},
		},
	})

	require.Equal(t, []string{"alice"}, hints.get("T111", "D111"))
	require.Equal(t, []string{"ops"}, hints.get("T222", "D111"))
	require.Equal(t, []string{"mpdm-alice--bob-1", "alice,bob", "alice, bob"}, hints.get("T333", "G111"))
	require.Empty(t, hints.get("T444", "G222"))
	require.Equal(t, []string{"ops", "ops-archive"}, hints.get("T666", "C777"))

	filter := newIngestFilter(IngestOptions{ExcludeChannels: []string{"#ALICE"}})
	require.False(t, filter.allowChannelNames("T111", "D111", hints.get("T111", "D111")))
	require.True(t, filter.allowChannelNames("T222", "D111", hints.get("T222", "D111")))

	filter = newIngestFilter(IngestOptions{ExcludeChannels: []string{"alice,bob"}})
	require.False(t, filter.allowChannelNames("T333", "G111", hints.get("T333", "G111")))

	filter = newIngestFilter(IngestOptions{ExcludeChannels: []string{"c2"}})
	filter.resolveKnownChannelIDs(map[string]struct{}{"c555": {}})
	require.True(t, filter.hasNameExclude)
	require.False(t, filter.allowChannelNames("T555", "C555", nil))
	require.True(t, filter.allowChannelNames("T555", "C555", []string{"kept"}))

	filter = newIngestFilter(IngestOptions{ExcludeChannels: []string{"C000000223"}})
	filter.resolveKnownChannelIDs(map[string]struct{}{"c000000222": {}, "c000000223": {}})
	require.False(t, filter.hasNameExclude)
	require.True(t, filter.allowChannelNames("T555", "C000000222", nil))
	require.True(t, filter.allowChannelNames("T555", "C000000222", []string{"kept"}))
	require.False(t, filter.allowChannelNames("T555", "C000000223", []string{"excluded"}))

	filter = newIngestFilter(IngestOptions{ExcludeChannels: []string{"c000000223"}})
	filter.resolveKnownChannelIDs(map[string]struct{}{"c000000222": {}, "c000000223": {}})
	require.False(t, filter.hasNameExclude)
	require.True(t, filter.allowChannelNames("T555", "C000000222", nil))
	require.False(t, filter.allowChannelNames("T555", "C000000223", nil))

	filter = newIngestFilter(IngestOptions{ExcludeChannels: []string{"id:C000000223"}})
	filter.resolveKnownChannelIDs(map[string]struct{}{"c000000222": {}})
	require.False(t, filter.hasNameExclude)
	require.True(t, filter.allowChannelNames("T555", "C000000222", nil))
	require.False(t, filter.allowChannelNames("T555", "C000000223", nil))

	filter = newIngestFilter(IngestOptions{ExcludeChannels: []string{"C000000223"}})
	filter.resolveKnownChannelIDs(map[string]struct{}{"c000000222": {}})
	require.True(t, filter.hasNameExclude)
	require.False(t, filter.allowChannelNames("T555", "C000000222", nil))
	require.True(t, filter.allowChannelNames("T555", "C000000222", []string{"kept"}))

	filter = newIngestFilter(IngestOptions{ExcludeChannels: []string{"C123"}})
	filter.resolveKnownChannelIDs(map[string]struct{}{"c999": {}})
	require.True(t, filter.hasNameExclude)
	require.False(t, filter.allowChannelNames("T555", "C999", nil))
	require.True(t, filter.allowChannelNames("T555", "C999", []string{"kept"}))

	filter = newIngestFilter(IngestOptions{ExcludeChannels: []string{"DEV2"}})
	filter.resolveKnownChannelIDs(map[string]struct{}{"c999": {}})
	require.True(t, filter.hasNameExclude)
	require.False(t, filter.allowChannelNames("T555", "C999", nil))
	require.True(t, filter.allowChannelNames("T555", "C999", []string{"kept"}))
}

func TestIngestReduxStatesSkipsDuplicateDesktopUsers(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, st.UpsertUser(ctx, store.User{ID: "USLACKBOT", WorkspaceID: "T1", Name: "slackbot", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, ingestReduxStates(ctx, st, []ReduxDecodedState{{
		WorkspaceID: "T2",
		UserID:      "U2",
		Members: []ReduxMember{{
			ID:   "USLACKBOT",
			Name: "slackbot",
			Profile: ReduxMemberProfile{
				DisplayName: "Slackbot",
			},
		}},
	}}, now, ingestFilter{}))
}

func TestIngestReduxStatesDoesNotPromoteMemberTeamWorkspaces(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, ingestReduxStates(ctx, st, []ReduxDecodedState{{
		WorkspaceID: "T1",
		UserID:      "U1",
		Members: []ReduxMember{{
			ID:     "U_EXTERNAL",
			Name:   "external-user",
			TeamID: "T_EXTERNAL",
		}},
	}}, now, ingestFilter{}))

	status, err := st.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, status.Workspaces)
	require.Equal(t, 1, status.Users)
}

func TestIngestReduxStatesSkipsDuplicateDesktopMessages(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, st.UpsertMessage(ctx, store.Message{
		ChannelID:      "C1",
		TS:             "1710000003.000400",
		WorkspaceID:    "T1",
		Text:           "already imported",
		NormalizedText: "already imported",
		SourceRank:     3,
		SourceName:     "desktop-indexeddb",
		RawJSON:        "{}",
		UpdatedAt:      now,
	}, nil))
	require.NoError(t, ingestReduxStates(ctx, st, []ReduxDecodedState{{
		WorkspaceID: "T2",
		UserID:      "U2",
		Messages: []ReduxMessage{{
			Channel: "C1",
			TS:      "1710000003.000400",
			Type:    "message",
			User:    "U2",
			Text:    "same slack-connect message",
		}},
	}}, now, ingestFilter{}))
}

func TestIngestReduxStatesSkipsDuplicateDesktopChannels(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C1", WorkspaceID: "T1", Name: "general", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, ingestReduxStates(ctx, st, []ReduxDecodedState{{
		WorkspaceID: "T2",
		UserID:      "U2",
		Channels: []ReduxChannel{{
			ID:            "C1",
			Name:          "general",
			IsChannel:     true,
			ContextTeamID: "T2",
		}},
		Messages: []ReduxMessage{{
			Channel: "C1",
			TS:      "1710000003.000400",
			Type:    "message",
			User:    "U2",
			Text:    "same channel unique message",
		}},
	}}, now, ingestFilter{}))

	messages, err := st.Messages(ctx, "", "C1", "", 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "T2", messages[0].WorkspaceID)
	require.Equal(t, "same channel unique message", messages[0].Text)
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
