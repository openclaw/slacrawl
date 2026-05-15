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
