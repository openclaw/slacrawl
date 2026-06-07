package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStoreRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	ctx := context.Background()
	require.NoError(t, s.UpsertWorkspace(ctx, Workspace{
		ID:        "T1",
		Name:      "team",
		RawJSON:   "{}",
		UpdatedAt: time.Now().UTC(),
	}))
	require.NoError(t, s.UpsertChannel(ctx, Channel{
		ID:          "C1",
		WorkspaceID: "T1",
		Name:        "eng",
		Kind:        "public_channel",
		RawJSON:     "{}",
		UpdatedAt:   time.Now().UTC(),
	}))
	require.NoError(t, s.UpsertMessage(ctx, Message{
		ChannelID:      "C1",
		TS:             "123.45",
		WorkspaceID:    "T1",
		Text:           "hello world",
		NormalizedText: "hello world",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      time.Now().UTC(),
	}, nil))

	results, err := s.Search(ctx, "", "hello", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "T1", results[0].WorkspaceID)
	status, err := s.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, status.Messages)
}

func TestSearchMessagesAutoEscapesAndFallsBack(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, s.UpsertWorkspace(ctx, Workspace{ID: "T1", Name: "team", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, s.UpsertChannel(ctx, Channel{ID: "D1", WorkspaceID: "T1", Name: "mike", Kind: "desktop_im", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, s.UpsertMessage(ctx, Message{
		ChannelID:      "D1",
		TS:             "1779612300.000100",
		WorkspaceID:    "T1",
		Text:           "What's the best way to coordinate meetings with you or your claw? Email? My EA can handle anything!",
		NormalizedText: "What's the best way to coordinate meetings with you or your claw? Email? My EA can handle anything!",
		SourceRank:     3,
		SourceName:     "desktop-indexeddb",
		RawJSON:        "{}",
		UpdatedAt:      now,
	}, nil))

	rows, err := s.SearchMessages(ctx, SearchOptions{Query: "What's the best way to coordinate meetings", Mode: SearchModeAuto, Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "D1", rows[0].ChannelID)

	rows, err = s.SearchMessages(ctx, SearchOptions{Query: "coordinate anything", Mode: SearchModeAuto, Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

func TestUpsertMessageDeduplicatesMentions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	ctx := context.Background()
	require.NoError(t, s.UpsertMessage(ctx, Message{
		ChannelID:      "C1",
		TS:             "123.45",
		WorkspaceID:    "T1",
		Text:           "<@U1> hello <@U1>",
		NormalizedText: "@u1 hello @u1",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      time.Now().UTC(),
	}, []Mention{
		{Type: "user", TargetID: "U1", DisplayText: "alice"},
		{Type: "user", TargetID: "U1", DisplayText: "alice"},
	}))

	rows, err := s.QueryReadOnly(ctx, "select count(*) as n from message_mentions where channel_id = 'C1' and ts = '123.45'")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(1), rows[0]["n"])
}

func TestUpsertMessagePreservesSourcePrecedenceAndRefreshesSearch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, s.UpsertMessage(ctx, Message{
		ChannelID:      "C1",
		TS:             "123.45",
		WorkspaceID:    "T1",
		Text:           "old alpha",
		NormalizedText: "old alpha",
		SourceRank:     1,
		SourceName:     "api-user",
		RawJSON:        `{"source":"user"}`,
		UpdatedAt:      now,
	}, nil))
	require.NoError(t, s.UpsertMessage(ctx, Message{
		ChannelID:      "C1",
		TS:             "123.45",
		WorkspaceID:    "T1",
		Text:           "new beta",
		NormalizedText: "new beta",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        `{"source":"bot"}`,
		UpdatedAt:      now.Add(time.Second),
	}, nil))

	rows, err := s.QueryReadOnly(ctx, "select source_rank, source_name, raw_json, text, normalized_text from messages where channel_id = 'C1' and ts = '123.45'")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(1), rows[0]["source_rank"])
	require.Equal(t, "api-user", rows[0]["source_name"])
	require.Equal(t, `{"source":"user"}`, rows[0]["raw_json"])
	require.Equal(t, "new beta", rows[0]["text"])
	require.Equal(t, "new beta", rows[0]["normalized_text"])

	matches, err := s.Search(ctx, "", "beta", 10)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	matches, err = s.Search(ctx, "", "alpha", 10)
	require.NoError(t, err)
	require.Empty(t, matches)
}

func TestUpsertMessageByPrioritySkipsLowerPriorityContent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, s.UpsertMessage(ctx, Message{
		ChannelID: "C1", TS: "123.45", WorkspaceID: "T1", Text: "richer",
		NormalizedText: "richer", SourceRank: 1, SourceName: "api-user", RawJSON: `{"source":"api"}`, UpdatedAt: now,
	}, []Mention{{Type: "user", TargetID: "U1"}}))

	written, err := s.UpsertMessageByPriority(ctx, Message{
		ChannelID: "C1", TS: "123.45", WorkspaceID: "T1", Text: "lower priority",
		NormalizedText: "lower priority", SourceRank: 4, SourceName: "mcp", RawJSON: `{"source":"mcp"}`, UpdatedAt: now.Add(time.Second),
	}, []Mention{{Type: "user", TargetID: "U2"}})
	require.NoError(t, err)
	require.False(t, written)

	rows, err := s.QueryReadOnly(ctx, `select text, source_name, source_rank from messages where channel_id = 'C1' and ts = '123.45'`)
	require.NoError(t, err)
	require.Equal(t, "richer", rows[0]["text"])
	require.Equal(t, "api-user", rows[0]["source_name"])
	require.Equal(t, int64(1), rows[0]["source_rank"])
	mentions, err := s.QueryReadOnly(ctx, `select target_id from message_mentions where channel_id = 'C1' and ts = '123.45'`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"target_id": "U1"}}, mentions)
}

func TestQueryReadOnlyRejectsWritableCTE(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	ctx := context.Background()
	require.NoError(t, s.UpsertMessage(ctx, Message{
		ChannelID:      "C1",
		TS:             "123.45",
		WorkspaceID:    "T1",
		Text:           "keep me",
		NormalizedText: "keep me",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      time.Now().UTC(),
	}, nil))

	_, err = s.QueryReadOnly(ctx, "with x as (select 1) delete from messages where channel_id = 'C1' returning 1")
	require.Error(t, err)

	status, err := s.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, status.Messages)
}

func TestQueryReadOnlyRejectsAdditionalStatements(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	ctx := context.Background()
	_, err = s.QueryReadOnly(ctx, "select ';' as literal; select 2")
	require.Error(t, err)

	rows, err := s.QueryReadOnly(ctx, "select ';' as literal; -- trailing comment")
	require.NoError(t, err)
	require.Equal(t, ";", rows[0]["literal"])
}

func TestUpsertMessageStoresFilesPreservesMediaAndRefreshesSearch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	ctx := context.Background()
	now := time.Now().UTC()
	message := Message{
		ChannelID:      "C1",
		TS:             "123.45",
		WorkspaceID:    "T1",
		UserID:         "U1",
		Text:           "file share",
		NormalizedText: "file share",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      now,
		Files: []MessageFile{{
			FileID:     "F1",
			Name:       "incident.pdf",
			Title:      "incident report",
			Mimetype:   "application/pdf",
			PlainText:  "searchable appendix",
			URLPrivate: "https://files.example/F1",
			RawJSON:    "{}",
		}},
	}
	require.NoError(t, s.UpsertMessage(ctx, message, nil))

	matches, err := s.Search(ctx, "", "appendix", 10)
	require.NoError(t, err)
	require.Len(t, matches, 1)

	require.NoError(t, s.UpdateFileMedia(ctx, FileMediaUpdate{
		ChannelID:     "C1",
		TS:            "123.45",
		FileID:        "F1",
		MediaPath:     "files/aa/hash-incident.pdf",
		ContentSHA256: "hash",
		ContentSize:   42,
		FetchedAt:     now.Format(time.RFC3339Nano),
		FetchStatus:   "fetched",
	}))
	message.Files[0].Title = "renamed incident report"
	message.Files[0].MediaPath = ""
	require.NoError(t, s.UpsertMessage(ctx, message, nil))

	files, err := s.Files(ctx, FileListOptions{Filename: "incident", Limit: 10})
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, "files/aa/hash-incident.pdf", files[0].MediaPath)
	require.Equal(t, "fetched", files[0].FetchStatus)

	desktopMessage := message
	desktopMessage.Text = "desktop copy"
	desktopMessage.NormalizedText = "desktop copy"
	desktopMessage.Files = nil
	require.NoError(t, s.UpsertMessage(ctx, desktopMessage, nil))
	files, err = s.Files(ctx, FileListOptions{Filename: "incident", Limit: 10})
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, "files/aa/hash-incident.pdf", files[0].MediaPath)
	matches, err = s.Search(ctx, "", "appendix", 10)
	require.NoError(t, err)
	require.Len(t, matches, 1)
}

func TestWorkspaceFiltersApplyToReadQueries(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, s.UpsertWorkspace(ctx, Workspace{ID: "T1", Name: "one", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, s.UpsertWorkspace(ctx, Workspace{ID: "T2", Name: "two", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, s.UpsertChannel(ctx, Channel{ID: "C1", WorkspaceID: "T1", Name: "eng", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, s.UpsertChannel(ctx, Channel{ID: "C2", WorkspaceID: "T2", Name: "ops", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, s.UpsertUser(ctx, User{ID: "U1", WorkspaceID: "T1", Name: "alice", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, s.UpsertUser(ctx, User{ID: "U2", WorkspaceID: "T2", Name: "bob", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, s.UpsertMessage(ctx, Message{
		ChannelID:      "C1",
		TS:             "1.0",
		WorkspaceID:    "T1",
		UserID:         "U1",
		Text:           "hello alpha",
		NormalizedText: "hello alpha",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      now,
	}, []Mention{{Type: "user", TargetID: "U1", DisplayText: "alice"}}))
	require.NoError(t, s.UpsertMessage(ctx, Message{
		ChannelID:      "C2",
		TS:             "2.0",
		WorkspaceID:    "T2",
		UserID:         "U2",
		Text:           "hello beta",
		NormalizedText: "hello beta",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      now,
	}, []Mention{{Type: "user", TargetID: "U2", DisplayText: "bob"}}))

	messages, err := s.Messages(ctx, "T1", "", "", 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "T1", messages[0].WorkspaceID)

	search, err := s.Search(ctx, "T2", "hello", 10)
	require.NoError(t, err)
	require.Len(t, search, 1)
	require.Equal(t, "T2", search[0].WorkspaceID)

	mentions, err := s.Mentions(ctx, "T1", "U1", 10)
	require.NoError(t, err)
	require.Len(t, mentions, 1)
	require.Equal(t, "T1", mentions[0].WorkspaceID)

	users, err := s.Users(ctx, "T2", "", 10)
	require.NoError(t, err)
	require.Len(t, users, 1)
	require.Equal(t, "T2", users[0].WorkspaceID)

	channels, err := s.Channels(ctx, "T1", "", 10)
	require.NoError(t, err)
	require.Len(t, channels, 1)
	require.Equal(t, "T1", channels[0].WorkspaceID)
}

func TestStoreRejectsCrossWorkspaceKeyCollisions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, s.UpsertChannel(ctx, Channel{ID: "C1", WorkspaceID: "T1", Name: "eng", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now}))
	err = s.UpsertChannel(ctx, Channel{ID: "C1", WorkspaceID: "T2", Name: "ops", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now})
	require.Error(t, err)
	require.Contains(t, err.Error(), "channel")

	require.NoError(t, s.UpsertUser(ctx, User{ID: "U1", WorkspaceID: "T1", Name: "alice", RawJSON: "{}", UpdatedAt: now}))
	err = s.UpsertUser(ctx, User{ID: "U1", WorkspaceID: "T2", Name: "bob", RawJSON: "{}", UpdatedAt: now})
	require.Error(t, err)
	require.Contains(t, err.Error(), "user")

	require.NoError(t, s.UpsertMessage(ctx, Message{
		ChannelID:      "CSAME",
		TS:             "1.0",
		WorkspaceID:    "T1",
		Text:           "hello alpha",
		NormalizedText: "hello alpha",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      now,
	}, nil))
	err = s.UpsertMessage(ctx, Message{
		ChannelID:      "CSAME",
		TS:             "1.0",
		WorkspaceID:    "T2",
		Text:           "hello beta",
		NormalizedText: "hello beta",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      now,
	}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "message")

	err = s.MarkMessageDeleted(ctx, Message{
		ChannelID:      "CSAME",
		TS:             "1.0",
		WorkspaceID:    "T2",
		Text:           "deleted",
		NormalizedText: "deleted",
		DeletedTS:      "1.1",
		SourceRank:     2,
		SourceName:     "tail",
		RawJSON:        "{}",
		UpdatedAt:      now,
	}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "message")

	messages, err := s.Messages(ctx, "T1", "", "", 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "hello alpha", messages[0].Text)
	messages, err = s.Messages(ctx, "T2", "", "", 10)
	require.NoError(t, err)
	require.Empty(t, messages)
}

func TestDesktopChannelHintsDoNotBlockDecodedChannels(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, s.UpsertChannel(ctx, Channel{ID: "C1", WorkspaceID: "T1", Name: "C1", Kind: "desktop_draft", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, s.UpsertChannel(ctx, Channel{ID: "C1", WorkspaceID: "T2", Name: "general", Kind: "desktop_channel", RawJSON: "{}", UpdatedAt: now}))
	rows, err := s.Channels(ctx, "", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "T2", rows[0].WorkspaceID)
	require.Equal(t, "general", rows[0].Name)

	require.NoError(t, s.UpsertChannel(ctx, Channel{ID: "C1", WorkspaceID: "T3", Name: "stale", Kind: "desktop_recent", RawJSON: "{}", UpdatedAt: now}))
	rows, err = s.Channels(ctx, "", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "T2", rows[0].WorkspaceID)
	require.Equal(t, "general", rows[0].Name)

	require.NoError(t, s.UpsertChannel(ctx, Channel{ID: "C1", WorkspaceID: "T4", Name: "shared", Kind: "desktop_private_channel", RawJSON: "{}", UpdatedAt: now}))
	rows, err = s.Channels(ctx, "", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "T2", rows[0].WorkspaceID)
	require.Equal(t, "general", rows[0].Name)
}

func TestOpenStampsSchemaVersion(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	var version int
	require.NoError(t, s.DB().QueryRow("pragma user_version").Scan(&version))
	require.Equal(t, schemaVersion, version)
}

func TestOpenFailsForNewerSchemaVersion(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec("pragma user_version = 99")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = Open(dbPath)
	require.Error(t, err)
	require.Contains(t, err.Error(), "newer than this slacrawl build supports")
}

func TestOpenCreatesReadPathIndexes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	rows, err := s.QueryReadOnly(context.Background(), `
select name
from sqlite_master
where type = 'index'
  and name in (
    'idx_messages_workspace_ts',
    'idx_messages_workspace_channel_ts',
    'idx_messages_workspace_user_ts',
    'idx_messages_key_expr',
    'idx_message_mentions_target_ts',
    'idx_sync_state_updated'
  )
order by name asc`)
	require.NoError(t, err)
	require.Len(t, rows, 6)
}

func TestOpenMigratesVersion2Schema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(legacyStoreSchemaV2)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	s, err := Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	var version int
	require.NoError(t, s.DB().QueryRow("pragma user_version").Scan(&version))
	require.Equal(t, schemaVersion, version)

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, s.UpsertMessage(ctx, Message{
		ChannelID:      "C1",
		TS:             "123.45",
		WorkspaceID:    "T1",
		Text:           "file share",
		NormalizedText: "file share",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      now,
		Files: []MessageFile{{
			FileID:    "F1",
			Name:      "legacy.txt",
			PlainText: "migrated appendix",
			RawJSON:   "{}",
		}},
	}, nil))
	matches, err := s.Search(ctx, "", "appendix", 10)
	require.NoError(t, err)
	require.Len(t, matches, 1)
}

func TestOpenDoesNotStampInvalidOldSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`
create table messages (
  channel_id text not null,
  ts text not null,
  workspace_id text not null,
  primary key (channel_id, ts)
);
pragma user_version = 2;
`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = Open(dbPath)
	require.Error(t, err)

	db, err = sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	var version int
	require.NoError(t, db.QueryRow("pragma user_version").Scan(&version))
	require.Equal(t, 2, version)
}

const legacyStoreSchemaV2 = `
create table workspaces (
  id text primary key,
  name text not null,
  domain text,
  enterprise_id text,
  raw_json text not null,
  updated_at text not null
);

create table channels (
  id text primary key,
  workspace_id text not null,
  name text not null,
  kind text not null,
  topic text,
  purpose text,
  is_private integer not null default 0,
  is_archived integer not null default 0,
  is_shared integer not null default 0,
  is_general integer not null default 0,
  raw_json text not null,
  updated_at text not null
);

create table users (
  id text primary key,
  workspace_id text not null,
  name text not null,
  real_name text,
  display_name text,
  title text,
  is_bot integer not null default 0,
  is_deleted integer not null default 0,
  raw_json text not null,
  updated_at text not null
);

create table messages (
  channel_id text not null,
  ts text not null,
  workspace_id text not null,
  user_id text,
  subtype text,
  client_msg_id text,
  thread_ts text,
  parent_user_id text,
  text text not null,
  normalized_text text not null,
  reply_count integer not null default 0,
  latest_reply text,
  edited_ts text,
  deleted_ts text,
  source_rank integer not null,
  source_name text not null,
  raw_json text not null,
  updated_at text not null,
  primary key (channel_id, ts)
);

create table message_events (
  id integer primary key autoincrement,
  channel_id text not null,
  ts text not null,
  event_type text not null,
  source_name text not null,
  payload_json text not null,
  created_at text not null
);

create table sync_state (
  source_name text not null,
  entity_type text not null,
  entity_id text not null,
  value text not null,
  updated_at text not null,
  primary key (source_name, entity_type, entity_id)
);

create table message_mentions (
  channel_id text not null,
  ts text not null,
  mention_type text not null,
  target_id text not null,
  display_text text,
  primary key (channel_id, ts, mention_type, target_id)
);

create table embedding_jobs (
  id integer primary key autoincrement,
  channel_id text not null,
  ts text not null,
  state text not null,
  created_at text not null
);

create virtual table message_fts using fts5(message_key unindexed, content);
pragma user_version = 2;
`
