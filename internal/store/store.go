package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vincentkoc/slacrawl/internal/store/storedb"

	_ "modernc.org/sqlite"
)

const schemaVersion = 3

const schema = `
pragma foreign_keys = on;
pragma journal_mode = wal;
pragma busy_timeout = 5000;

create table if not exists workspaces (
  id text primary key,
  name text not null,
  domain text,
  enterprise_id text,
  raw_json text not null,
  updated_at text not null
);

create table if not exists channels (
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

create table if not exists users (
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

create table if not exists messages (
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

create index if not exists idx_messages_workspace_ts on messages(workspace_id, ts desc);
create index if not exists idx_messages_workspace_channel_ts on messages(workspace_id, channel_id, ts desc);
create index if not exists idx_messages_workspace_user_ts on messages(workspace_id, user_id, ts desc);
create index if not exists idx_messages_key_expr on messages((channel_id || '|' || ts));

create table if not exists message_files (
  workspace_id text not null,
  channel_id text not null,
  ts text not null,
  file_id text not null,
  user_id text,
  name text not null default '',
  title text not null default '',
  mimetype text,
  filetype text,
  pretty_type text,
  mode text,
  size integer not null default 0,
  url_private text,
  url_private_download text,
  permalink text,
  is_public integer not null default 0,
  plain_text text not null default '',
  preview_plain_text text not null default '',
  media_path text,
  content_sha256 text,
  content_size integer not null default 0,
  fetched_at text,
  fetch_status text not null default '',
  fetch_error text not null default '',
  raw_json text not null,
  updated_at text not null,
  primary key (channel_id, ts, file_id)
);

create index if not exists idx_message_files_workspace_ts on message_files(workspace_id, ts desc);
create index if not exists idx_message_files_file_id on message_files(file_id);
create index if not exists idx_message_files_name on message_files(name);

create table if not exists message_events (
  id integer primary key autoincrement,
  channel_id text not null,
  ts text not null,
  event_type text not null,
  source_name text not null,
  payload_json text not null,
  created_at text not null
);

create table if not exists sync_state (
  source_name text not null,
  entity_type text not null,
  entity_id text not null,
  value text not null,
  updated_at text not null,
  primary key (source_name, entity_type, entity_id)
);

create table if not exists message_mentions (
  channel_id text not null,
  ts text not null,
  mention_type text not null,
  target_id text not null,
  display_text text,
  primary key (channel_id, ts, mention_type, target_id)
);

create index if not exists idx_message_mentions_target_ts on message_mentions(target_id, ts desc);

create table if not exists embedding_jobs (
  id integer primary key autoincrement,
  channel_id text not null,
  ts text not null,
  state text not null,
  created_at text not null
);

create virtual table if not exists message_fts using fts5(message_key unindexed, content);
create index if not exists idx_sync_state_updated on sync_state(updated_at desc);
`

type Store struct {
	db *sql.DB
	q  *storedb.Queries
}

type Workspace struct {
	ID           string
	Name         string
	Domain       string
	EnterpriseID string
	RawJSON      string
	UpdatedAt    time.Time
}

type Channel struct {
	ID          string
	WorkspaceID string
	Name        string
	Kind        string
	Topic       string
	Purpose     string
	IsPrivate   bool
	IsArchived  bool
	IsShared    bool
	IsGeneral   bool
	RawJSON     string
	UpdatedAt   time.Time
}

type User struct {
	ID          string
	WorkspaceID string
	Name        string
	RealName    string
	DisplayName string
	Title       string
	IsBot       bool
	IsDeleted   bool
	RawJSON     string
	UpdatedAt   time.Time
}

type Message struct {
	ChannelID      string
	TS             string
	WorkspaceID    string
	UserID         string
	Subtype        string
	ClientMsgID    string
	ThreadTS       string
	ParentUserID   string
	Text           string
	NormalizedText string
	ReplyCount     int
	LatestReply    string
	EditedTS       string
	DeletedTS      string
	SourceRank     int
	SourceName     string
	RawJSON        string
	UpdatedAt      time.Time
	Files          []MessageFile
}

type Mention struct {
	Type        string
	TargetID    string
	DisplayText string
}

type MessageFile struct {
	WorkspaceID        string
	ChannelID          string
	TS                 string
	FileID             string
	UserID             string
	Name               string
	Title              string
	Mimetype           string
	Filetype           string
	PrettyType         string
	Mode               string
	Size               int64
	URLPrivate         string
	URLPrivateDownload string
	Permalink          string
	IsPublic           bool
	PlainText          string
	PreviewPlainText   string
	MediaPath          string
	ContentSHA256      string
	ContentSize        int64
	FetchedAt          string
	FetchStatus        string
	FetchError         string
	RawJSON            string
	UpdatedAt          time.Time
}

type Status struct {
	Workspaces  int       `json:"workspaces"`
	Channels    int       `json:"channels"`
	Users       int       `json:"users"`
	Messages    int       `json:"messages"`
	LastSyncAt  time.Time `json:"last_sync_at"`
	ThreadState string    `json:"thread_state"`
}

type MessageRow struct {
	WorkspaceID    string `json:"workspace_id"`
	ChannelID      string `json:"channel_id"`
	TS             string `json:"ts"`
	UserID         string `json:"user_id"`
	Text           string `json:"text"`
	NormalizedText string `json:"normalized_text"`
	ThreadTS       string `json:"thread_ts"`
	Subtype        string `json:"subtype"`
}

type MentionRow struct {
	WorkspaceID string `json:"workspace_id"`
	ChannelID   string `json:"channel_id"`
	TS          string `json:"ts"`
	MentionType string `json:"mention_type"`
	TargetID    string `json:"target_id"`
	DisplayText string `json:"display_text"`
}

type UserRow struct {
	WorkspaceID string `json:"workspace_id"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	RealName    string `json:"real_name"`
	DisplayName string `json:"display_name"`
	Title       string `json:"title"`
}

type ChannelRow struct {
	WorkspaceID string `json:"workspace_id"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
}

type FileListOptions struct {
	WorkspaceID string
	ChannelID   string
	UserID      string
	FileID      string
	Filename    string
	ContentType string
	Since       time.Time
	Before      time.Time
	Limit       int
	MissingOnly bool
}

type FileRow struct {
	WorkspaceID        string    `json:"workspace_id"`
	ChannelID          string    `json:"channel_id"`
	TS                 string    `json:"ts"`
	FileID             string    `json:"file_id"`
	UserID             string    `json:"user_id,omitempty"`
	Name               string    `json:"name"`
	Title              string    `json:"title,omitempty"`
	Mimetype           string    `json:"mimetype,omitempty"`
	Filetype           string    `json:"filetype,omitempty"`
	PrettyType         string    `json:"pretty_type,omitempty"`
	Mode               string    `json:"mode,omitempty"`
	Size               int64     `json:"size"`
	URLPrivate         string    `json:"url_private,omitempty"`
	URLPrivateDownload string    `json:"url_private_download,omitempty"`
	Permalink          string    `json:"permalink,omitempty"`
	IsPublic           bool      `json:"is_public"`
	PlainText          string    `json:"plain_text,omitempty"`
	PreviewPlainText   string    `json:"preview_plain_text,omitempty"`
	MediaPath          string    `json:"media_path,omitempty"`
	ContentSHA256      string    `json:"content_sha256,omitempty"`
	ContentSize        int64     `json:"content_size,omitempty"`
	FetchedAt          time.Time `json:"fetched_at,omitzero"`
	FetchStatus        string    `json:"fetch_status,omitempty"`
	FetchError         string    `json:"fetch_error,omitempty"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type FileMediaUpdate struct {
	ChannelID     string
	TS            string
	FileID        string
	MediaPath     string
	ContentSHA256 string
	ContentSize   int64
	FetchedAt     string
	FetchStatus   string
	FetchError    string
}

type ChannelSyncCursor struct {
	ID       string
	LatestTS string
}

type SyncStateRow struct {
	SourceName string `json:"source_name"`
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Value      string `json:"value"`
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	currentVersion, err := readSchemaVersion(db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if currentVersion > schemaVersion {
		_ = db.Close()
		return nil, fmt.Errorf("database schema version %d is newer than this slacrawl build supports (%d)", currentVersion, schemaVersion)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	if currentVersion != schemaVersion {
		if err := writeSchemaVersion(db, schemaVersion); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return &Store{db: db, q: storedb.New(db)}, nil
}

func (s *Store) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) UpsertWorkspace(ctx context.Context, workspace Workspace) error {
	return s.q.UpsertWorkspace(ctx, storedb.UpsertWorkspaceParams{
		ID:           workspace.ID,
		Name:         workspace.Name,
		Domain:       dbText(workspace.Domain),
		EnterpriseID: dbText(workspace.EnterpriseID),
		RawJson:      workspace.RawJSON,
		UpdatedAt:    formatDBTime(workspace.UpdatedAt),
	})
}

func (s *Store) UpsertChannel(ctx context.Context, channel Channel) error {
	return s.q.UpsertChannel(ctx, storedb.UpsertChannelParams{
		ID:          channel.ID,
		WorkspaceID: channel.WorkspaceID,
		Name:        channel.Name,
		Kind:        channel.Kind,
		Topic:       dbText(channel.Topic),
		Purpose:     dbText(channel.Purpose),
		IsPrivate:   boolInt(channel.IsPrivate),
		IsArchived:  boolInt(channel.IsArchived),
		IsShared:    boolInt(channel.IsShared),
		IsGeneral:   boolInt(channel.IsGeneral),
		RawJson:     channel.RawJSON,
		UpdatedAt:   formatDBTime(channel.UpdatedAt),
	})
}

func (s *Store) UpsertUser(ctx context.Context, user User) error {
	return s.q.UpsertUser(ctx, storedb.UpsertUserParams{
		ID:          user.ID,
		WorkspaceID: user.WorkspaceID,
		Name:        user.Name,
		RealName:    dbText(user.RealName),
		DisplayName: dbText(user.DisplayName),
		Title:       dbText(user.Title),
		IsBot:       boolInt(user.IsBot),
		IsDeleted:   boolInt(user.IsDeleted),
		RawJson:     user.RawJSON,
		UpdatedAt:   formatDBTime(user.UpdatedAt),
	})
}

func (s *Store) UpsertMessage(ctx context.Context, message Message, mentions []Mention) error {
	key := messageKey(message.ChannelID, message.TS)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	qtx := s.q.WithTx(tx)

	if err := qtx.UpsertMessage(ctx, storedb.UpsertMessageParams{
		ChannelID:      message.ChannelID,
		Ts:             message.TS,
		WorkspaceID:    message.WorkspaceID,
		UserID:         dbText(message.UserID),
		Subtype:        dbText(message.Subtype),
		ClientMsgID:    dbText(message.ClientMsgID),
		ThreadTs:       dbText(message.ThreadTS),
		ParentUserID:   dbText(message.ParentUserID),
		Text:           message.Text,
		NormalizedText: message.NormalizedText,
		ReplyCount:     int64(message.ReplyCount),
		LatestReply:    dbText(message.LatestReply),
		EditedTs:       dbText(message.EditedTS),
		DeletedTs:      dbText(message.DeletedTS),
		SourceRank:     int64(message.SourceRank),
		SourceName:     message.SourceName,
		RawJson:        message.RawJSON,
		UpdatedAt:      formatDBTime(message.UpdatedAt),
	}); err != nil {
		return err
	}

	if err := qtx.DeleteMessageMentions(ctx, storedb.DeleteMessageMentionsParams{ChannelID: message.ChannelID, Ts: message.TS}); err != nil {
		return err
	}
	seenMentions := map[string]struct{}{}
	for _, mention := range mentions {
		key := mention.Type + "|" + mention.TargetID + "|" + mention.DisplayText
		if _, ok := seenMentions[key]; ok {
			continue
		}
		seenMentions[key] = struct{}{}
		if err := qtx.UpsertMessageMention(ctx, storedb.UpsertMessageMentionParams{
			ChannelID:   message.ChannelID,
			Ts:          message.TS,
			MentionType: mention.Type,
			TargetID:    mention.TargetID,
			DisplayText: dbText(mention.DisplayText),
		}); err != nil {
			return err
		}
	}

	filesForSearch := message.Files
	if message.Files != nil {
		existingMedia, err := existingFileMedia(ctx, qtx, message.ChannelID, message.TS)
		if err != nil {
			return err
		}
		if err := qtx.DeleteMessageFiles(ctx, storedb.DeleteMessageFilesParams{ChannelID: message.ChannelID, Ts: message.TS}); err != nil {
			return err
		}
		for i, file := range message.Files {
			if file.WorkspaceID == "" {
				file.WorkspaceID = message.WorkspaceID
			}
			if file.ChannelID == "" {
				file.ChannelID = message.ChannelID
			}
			if file.TS == "" {
				file.TS = message.TS
			}
			if file.UserID == "" {
				file.UserID = message.UserID
			}
			if file.UpdatedAt.IsZero() {
				file.UpdatedAt = message.UpdatedAt
			}
			if media, ok := existingMedia[file.FileID]; ok && file.MediaPath == "" {
				file.MediaPath = media.MediaPath
				file.ContentSHA256 = media.ContentSHA256
				file.ContentSize = media.ContentSize
				file.FetchedAt = media.FetchedAt
				file.FetchStatus = media.FetchStatus
				file.FetchError = media.FetchError
			}
			message.Files[i] = file
			if err := qtx.InsertMessageFile(ctx, insertMessageFileParams(file)); err != nil {
				return err
			}
		}
		filesForSearch = message.Files
	} else {
		filesForSearch, err = existingFilesForSearch(ctx, tx, message.ChannelID, message.TS)
		if err != nil {
			return err
		}
	}

	if err := qtx.DeleteMessageFTS(ctx, key); err != nil {
		return err
	}
	searchMessage := message
	searchMessage.Files = filesForSearch
	if err := qtx.InsertMessageFTS(ctx, storedb.InsertMessageFTSParams{MessageKey: key, Content: messageSearchContent(searchMessage)}); err != nil {
		return err
	}

	if err := qtx.InsertMessageEvent(ctx, storedb.InsertMessageEventParams{
		ChannelID:   message.ChannelID,
		Ts:          message.TS,
		EventType:   eventType(message),
		SourceName:  message.SourceName,
		PayloadJson: message.RawJSON,
		CreatedAt:   formatDBTime(message.UpdatedAt),
	}); err != nil {
		return err
	}

	return tx.Commit()
}

func existingFileMedia(ctx context.Context, qtx *storedb.Queries, channelID, ts string) (map[string]MessageFile, error) {
	rows, err := qtx.ListExistingFileMedia(ctx, storedb.ListExistingFileMediaParams{ChannelID: channelID, Ts: ts})
	if err != nil {
		return nil, err
	}
	out := map[string]MessageFile{}
	for _, row := range rows {
		out[row.FileID] = MessageFile{
			FileID:        row.FileID,
			MediaPath:     row.MediaPath,
			ContentSHA256: row.ContentSha256,
			ContentSize:   row.ContentSize,
			FetchedAt:     row.FetchedAt,
			FetchStatus:   row.FetchStatus,
			FetchError:    row.FetchError,
		}
	}
	return out, nil
}

func existingFilesForSearch(ctx context.Context, tx *sql.Tx, channelID, ts string) ([]MessageFile, error) {
	rows, err := tx.QueryContext(ctx, `
select file_id, name, title, plain_text, preview_plain_text
from message_files
where channel_id = ? and ts = ?
`, channelID, ts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	files := []MessageFile{}
	for rows.Next() {
		var file MessageFile
		if err := rows.Scan(&file.FileID, &file.Name, &file.Title, &file.PlainText, &file.PreviewPlainText); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

func insertMessageFileParams(file MessageFile) storedb.InsertMessageFileParams {
	return storedb.InsertMessageFileParams{
		WorkspaceID:        file.WorkspaceID,
		ChannelID:          file.ChannelID,
		Ts:                 file.TS,
		FileID:             file.FileID,
		UserID:             dbText(file.UserID),
		Name:               file.Name,
		Title:              file.Title,
		Mimetype:           dbText(file.Mimetype),
		Filetype:           dbText(file.Filetype),
		PrettyType:         dbText(file.PrettyType),
		Mode:               dbText(file.Mode),
		Size:               file.Size,
		UrlPrivate:         dbText(file.URLPrivate),
		UrlPrivateDownload: dbText(file.URLPrivateDownload),
		Permalink:          dbText(file.Permalink),
		IsPublic:           boolInt(file.IsPublic),
		PlainText:          file.PlainText,
		PreviewPlainText:   file.PreviewPlainText,
		MediaPath:          dbText(file.MediaPath),
		ContentSha256:      dbText(file.ContentSHA256),
		ContentSize:        file.ContentSize,
		FetchedAt:          dbText(file.FetchedAt),
		FetchStatus:        file.FetchStatus,
		FetchError:         file.FetchError,
		RawJson:            file.RawJSON,
		UpdatedAt:          formatDBTime(file.UpdatedAt),
	}
}

func (s *Store) SetSyncState(ctx context.Context, source, entityType, entityID, value string) error {
	return s.q.SetSyncState(ctx, storedb.SetSyncStateParams{
		SourceName: source,
		EntityType: entityType,
		EntityID:   entityID,
		Value:      value,
		UpdatedAt:  formatDBTime(time.Now().UTC()),
	})
}

func (s *Store) Status(ctx context.Context) (Status, error) {
	status := Status{}
	countWorkspaces, err := s.q.CountWorkspaces(ctx)
	if err != nil {
		return Status{}, err
	}
	countChannels, err := s.q.CountChannels(ctx)
	if err != nil {
		return Status{}, err
	}
	countUsers, err := s.q.CountUsers(ctx)
	if err != nil {
		return Status{}, err
	}
	countMessages, err := s.q.CountMessages(ctx)
	if err != nil {
		return Status{}, err
	}
	status.Workspaces = int(countWorkspaces)
	status.Channels = int(countChannels)
	status.Users = int(countUsers)
	status.Messages = int(countMessages)

	lastSync, err := s.q.LastSyncAt(ctx)
	if err != nil {
		return Status{}, err
	}
	if lastSync != "" {
		parsed, err := time.Parse(time.RFC3339, lastSync)
		if err == nil {
			status.LastSyncAt = parsed
		}
	}

	status.ThreadState = "partial"
	threadState, err := s.q.ThreadCoverageState(ctx)
	if err == nil {
		status.ThreadState = threadState
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Status{}, err
	}

	return status, nil
}

func (s *Store) Search(ctx context.Context, workspaceID string, query string, limit int) ([]MessageRow, error) {
	sqlQuery := `
select m.workspace_id, m.channel_id, m.ts, m.user_id, m.text, m.normalized_text, m.thread_ts, m.subtype
from message_fts f
join messages m on f.message_key = m.channel_id || '|' || m.ts
where message_fts match ?
  and (? = '' or m.workspace_id = ?)
order by m.ts desc
limit ?
`
	rows, err := s.db.QueryContext(ctx, sqlQuery, query, workspaceID, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanMessageRows(rows)
}

func (s *Store) Messages(ctx context.Context, workspaceID string, channelID string, userID string, limit int) ([]MessageRow, error) {
	out := make([]MessageRow, 0)
	appendRow := func(workspaceID, channelID, ts, userID, text, normalizedText, threadTS, subtype string) {
		out = append(out, MessageRow{
			WorkspaceID:    workspaceID,
			ChannelID:      channelID,
			TS:             ts,
			UserID:         userID,
			Text:           text,
			NormalizedText: normalizedText,
			ThreadTS:       threadTS,
			Subtype:        subtype,
		})
	}

	switch {
	case workspaceID != "" && channelID != "" && userID != "":
		rows, err := s.q.ListMessagesByWorkspaceChannelUser(ctx, storedb.ListMessagesByWorkspaceChannelUserParams{
			WorkspaceID: workspaceID,
			ChannelID:   channelID,
			UserID:      dbText(userID),
			Limit:       int64(limit),
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			appendRow(row.WorkspaceID, row.ChannelID, row.Ts, row.UserID, row.Text, row.NormalizedText, row.ThreadTs, row.Subtype)
		}
	case workspaceID != "" && channelID != "":
		rows, err := s.q.ListMessagesByWorkspaceChannel(ctx, storedb.ListMessagesByWorkspaceChannelParams{
			WorkspaceID: workspaceID,
			ChannelID:   channelID,
			Limit:       int64(limit),
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			appendRow(row.WorkspaceID, row.ChannelID, row.Ts, row.UserID, row.Text, row.NormalizedText, row.ThreadTs, row.Subtype)
		}
	case workspaceID != "" && userID != "":
		rows, err := s.q.ListMessagesByWorkspaceUser(ctx, storedb.ListMessagesByWorkspaceUserParams{
			WorkspaceID: workspaceID,
			UserID:      dbText(userID),
			Limit:       int64(limit),
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			appendRow(row.WorkspaceID, row.ChannelID, row.Ts, row.UserID, row.Text, row.NormalizedText, row.ThreadTs, row.Subtype)
		}
	case channelID != "" && userID != "":
		rows, err := s.q.ListMessagesByChannelUser(ctx, storedb.ListMessagesByChannelUserParams{
			ChannelID: channelID,
			UserID:    dbText(userID),
			Limit:     int64(limit),
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			appendRow(row.WorkspaceID, row.ChannelID, row.Ts, row.UserID, row.Text, row.NormalizedText, row.ThreadTs, row.Subtype)
		}
	case workspaceID != "":
		rows, err := s.q.ListMessagesByWorkspace(ctx, storedb.ListMessagesByWorkspaceParams{
			WorkspaceID: workspaceID,
			Limit:       int64(limit),
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			appendRow(row.WorkspaceID, row.ChannelID, row.Ts, row.UserID, row.Text, row.NormalizedText, row.ThreadTs, row.Subtype)
		}
	case channelID != "":
		rows, err := s.q.ListMessagesByChannel(ctx, storedb.ListMessagesByChannelParams{
			ChannelID: channelID,
			Limit:     int64(limit),
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			appendRow(row.WorkspaceID, row.ChannelID, row.Ts, row.UserID, row.Text, row.NormalizedText, row.ThreadTs, row.Subtype)
		}
	case userID != "":
		rows, err := s.q.ListMessagesByUser(ctx, storedb.ListMessagesByUserParams{
			UserID: dbText(userID),
			Limit:  int64(limit),
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			appendRow(row.WorkspaceID, row.ChannelID, row.Ts, row.UserID, row.Text, row.NormalizedText, row.ThreadTs, row.Subtype)
		}
	default:
		rows, err := s.q.ListMessagesAll(ctx, int64(limit))
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			appendRow(row.WorkspaceID, row.ChannelID, row.Ts, row.UserID, row.Text, row.NormalizedText, row.ThreadTs, row.Subtype)
		}
	}
	return out, nil
}

func (s *Store) Mentions(ctx context.Context, workspaceID string, target string, limit int) ([]MentionRow, error) {
	rows, err := s.q.ListMentions(ctx, storedb.ListMentionsParams{
		WorkspaceID: workspaceID,
		Target:      target,
		TargetLike:  dbText("%" + target + "%"),
		Limit:       int64(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]MentionRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, MentionRow{
			WorkspaceID: row.WorkspaceID,
			ChannelID:   row.ChannelID,
			TS:          row.Ts,
			MentionType: row.MentionType,
			TargetID:    row.TargetID,
			DisplayText: row.DisplayText,
		})
	}
	return out, nil
}

func (s *Store) QueryReadOnly(ctx context.Context, query string) ([]map[string]any, error) {
	trimmed := strings.TrimSpace(strings.ToLower(query))
	if !strings.HasPrefix(trimmed, "select") && !strings.HasPrefix(trimmed, "with") {
		return nil, errors.New("only read-only select statements are allowed")
	}
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var results []map[string]any
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := map[string]any{}
		for i, col := range cols {
			row[col] = stringifyDBValue(values[i])
		}
		results = append(results, row)
	}
	return results, rows.Err()
}

func (s *Store) Users(ctx context.Context, workspaceID string, query string, limit int) ([]UserRow, error) {
	rows, err := s.q.ListUsers(ctx, storedb.ListUsersParams{
		WorkspaceID: workspaceID,
		Query:       query,
		QueryLike:   "%" + query + "%",
		Limit:       int64(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]UserRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, UserRow{
			WorkspaceID: row.WorkspaceID,
			ID:          row.ID,
			Name:        row.Name,
			RealName:    row.RealName,
			DisplayName: row.DisplayName,
			Title:       row.Title,
		})
	}
	return out, nil
}

func (s *Store) Channels(ctx context.Context, workspaceID string, query string, limit int) ([]ChannelRow, error) {
	return s.ChannelsByKind(ctx, workspaceID, query, "", limit)
}

func (s *Store) ChannelsByKind(ctx context.Context, workspaceID string, query string, kind string, limit int) ([]ChannelRow, error) {
	rows, err := s.q.ListChannelsByKind(ctx, storedb.ListChannelsByKindParams{
		WorkspaceID: workspaceID,
		Query:       query,
		QueryLike:   "%" + query + "%",
		Kind:        kind,
		Limit:       int64(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]ChannelRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, ChannelRow{
			WorkspaceID: row.WorkspaceID,
			ID:          row.ID,
			Name:        row.Name,
			Kind:        row.Kind,
		})
	}
	return out, nil
}

func (s *Store) Files(ctx context.Context, opts FileListOptions) ([]FileRow, error) {
	args := []any{}
	clauses := []string{"1=1"}
	if opts.WorkspaceID != "" {
		clauses = append(clauses, "workspace_id = ?")
		args = append(args, opts.WorkspaceID)
	}
	if opts.ChannelID != "" {
		clauses = append(clauses, "channel_id = ?")
		args = append(args, opts.ChannelID)
	}
	if opts.UserID != "" {
		clauses = append(clauses, "coalesce(user_id, '') = ?")
		args = append(args, opts.UserID)
	}
	if opts.FileID != "" {
		clauses = append(clauses, "file_id = ?")
		args = append(args, opts.FileID)
	}
	if opts.Filename != "" {
		clauses = append(clauses, "(name like ? or title like ?)")
		like := "%" + opts.Filename + "%"
		args = append(args, like, like)
	}
	if opts.ContentType != "" {
		clauses = append(clauses, "(coalesce(mimetype, '') like ? or coalesce(filetype, '') like ?)")
		like := "%" + opts.ContentType + "%"
		args = append(args, like, like)
	}
	if !opts.Since.IsZero() {
		clauses = append(clauses, "ts >= ?")
		args = append(args, slackTSFromTime(opts.Since))
	}
	if !opts.Before.IsZero() {
		clauses = append(clauses, "ts < ?")
		args = append(args, slackTSFromTime(opts.Before))
	}
	query := `
select workspace_id, channel_id, ts, file_id, coalesce(user_id, ''), name, title,
       coalesce(mimetype, ''), coalesce(filetype, ''), coalesce(pretty_type, ''),
       coalesce(mode, ''), size, coalesce(url_private, ''), coalesce(url_private_download, ''),
       coalesce(permalink, ''), is_public, plain_text, preview_plain_text,
       coalesce(media_path, ''), coalesce(content_sha256, ''), content_size,
       coalesce(fetched_at, ''), fetch_status, fetch_error, updated_at
from message_files
where ` + strings.Join(clauses, " and ") + `
order by ts desc, file_id asc
`
	if opts.Limit > 0 {
		query += ` limit ?`
		args = append(args, opts.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []FileRow{}
	for rows.Next() {
		var row FileRow
		var isPublic int64
		var fetchedAt, updatedAt string
		if err := rows.Scan(
			&row.WorkspaceID,
			&row.ChannelID,
			&row.TS,
			&row.FileID,
			&row.UserID,
			&row.Name,
			&row.Title,
			&row.Mimetype,
			&row.Filetype,
			&row.PrettyType,
			&row.Mode,
			&row.Size,
			&row.URLPrivate,
			&row.URLPrivateDownload,
			&row.Permalink,
			&isPublic,
			&row.PlainText,
			&row.PreviewPlainText,
			&row.MediaPath,
			&row.ContentSHA256,
			&row.ContentSize,
			&fetchedAt,
			&row.FetchStatus,
			&row.FetchError,
			&updatedAt,
		); err != nil {
			return nil, err
		}
		row.IsPublic = isPublic != 0
		row.FetchedAt = parseDBTime(fetchedAt)
		row.UpdatedAt = parseDBTime(updatedAt)
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) UpdateFileMedia(ctx context.Context, update FileMediaUpdate) error {
	return s.q.UpdateFileMedia(ctx, storedb.UpdateFileMediaParams{
		MediaPath:     dbText(update.MediaPath),
		ContentSha256: dbText(update.ContentSHA256),
		ContentSize:   update.ContentSize,
		FetchedAt:     dbText(update.FetchedAt),
		FetchStatus:   update.FetchStatus,
		FetchError:    update.FetchError,
		UpdatedAt:     formatDBTime(time.Now().UTC()),
		ChannelID:     update.ChannelID,
		Ts:            update.TS,
		FileID:        update.FileID,
	})
}

func (s *Store) UpdateFileFetchStatus(ctx context.Context, channelID, ts, fileID, fetchedAt, status, message string) error {
	return s.q.UpdateFileFetchStatus(ctx, storedb.UpdateFileFetchStatusParams{
		FetchedAt:   dbText(fetchedAt),
		FetchStatus: status,
		FetchError:  message,
		UpdatedAt:   formatDBTime(time.Now().UTC()),
		ChannelID:   channelID,
		Ts:          ts,
		FileID:      fileID,
	})
}

func (s *Store) ChannelSyncCursors(ctx context.Context, workspaceID string) ([]ChannelSyncCursor, error) {
	rows, err := s.q.ChannelSyncCursors(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelSyncCursor, 0, len(rows))
	for _, row := range rows {
		out = append(out, ChannelSyncCursor{ID: row.ID, LatestTS: row.LatestTs})
	}
	return out, nil
}

func (s *Store) RenameChannel(ctx context.Context, channelID string, name string) error {
	return s.q.RenameChannel(ctx, storedb.RenameChannelParams{
		Name:      name,
		UpdatedAt: formatDBTime(time.Now().UTC()),
		ID:        channelID,
	})
}

func (s *Store) SetChannelArchived(ctx context.Context, channelID string, archived bool) error {
	return s.q.SetChannelArchived(ctx, storedb.SetChannelArchivedParams{
		IsArchived: boolInt(archived),
		UpdatedAt:  formatDBTime(time.Now().UTC()),
		ID:         channelID,
	})
}

func (s *Store) GetSyncState(ctx context.Context, source, entityType, entityID string) (string, error) {
	value, err := s.q.GetSyncState(ctx, storedb.GetSyncStateParams{
		SourceName: source,
		EntityType: entityType,
		EntityID:   entityID,
	})
	if err != nil {
		return "", err
	}
	return value, nil
}

func (s *Store) ListSyncState(ctx context.Context, source, entityType string, limit int) ([]SyncStateRow, error) {
	rows, err := s.q.ListSyncState(ctx, storedb.ListSyncStateParams{
		SourceName: source,
		EntityType: entityType,
		Limit:      int64(RequireLimit(limit)),
	})
	if err != nil {
		return nil, err
	}
	out := make([]SyncStateRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, SyncStateRow{
			SourceName: row.SourceName,
			EntityType: row.EntityType,
			EntityID:   row.EntityID,
			Value:      row.Value,
		})
	}
	return out, nil
}

func (s *Store) RebuildSearchIndexes(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `delete from message_fts`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
insert into message_fts (message_key, content)
select m.channel_id || '|' || m.ts,
       trim(m.normalized_text || ' ' || coalesce((
         select group_concat(trim(f.name || ' ' || f.title || ' ' || f.plain_text || ' ' || f.preview_plain_text), ' ')
         from message_files f
         where f.channel_id = m.channel_id and f.ts = m.ts
       ), ''))
from messages m
`); err != nil {
		return err
	}
	return tx.Commit()
}

func MarshalRaw(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func scanMessageRows(rows *sql.Rows) ([]MessageRow, error) {
	var out []MessageRow
	for rows.Next() {
		var row MessageRow
		if err := rows.Scan(&row.WorkspaceID, &row.ChannelID, &row.TS, &row.UserID, &row.Text, &row.NormalizedText, &row.ThreadTS, &row.Subtype); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func boolInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func dbText(value string) sql.NullString {
	return sql.NullString{String: value, Valid: true}
}

func formatDBTime(value time.Time) string {
	return value.Format(time.RFC3339)
}

func parseDBTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed
	}
	return time.Time{}
}

func slackTSFromTime(value time.Time) string {
	return fmt.Sprintf("%d.%06d", value.UTC().Unix(), value.UTC().Nanosecond()/1000)
}

func eventType(message Message) string {
	switch {
	case message.DeletedTS != "":
		return "message_deleted"
	case message.EditedTS != "":
		return "message_changed"
	default:
		return "message"
	}
}

func messageKey(channelID string, ts string) string {
	return channelID + "|" + ts
}

func messageSearchContent(message Message) string {
	parts := []string{message.NormalizedText}
	for _, file := range message.Files {
		parts = append(parts, file.Name, file.Title, file.PlainText, file.PreviewPlainText)
	}
	return strings.Join(filterNonEmpty(parts), " ")
}

func filterNonEmpty(parts []string) []string {
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			filtered = append(filtered, strings.TrimSpace(part))
		}
	}
	return filtered
}

func stringifyDBValue(value any) any {
	switch typed := value.(type) {
	case []byte:
		return string(typed)
	default:
		return typed
	}
}

func DebugJSON(value any) string {
	data, _ := json.MarshalIndent(value, "", "  ")
	return string(data)
}

func ParseTime(value string) string {
	if value == "" {
		return ""
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.Format(time.RFC3339)
	}
	return value
}

func RequireLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	return limit
}

func PrettyStatus(status Status) string {
	last := "never"
	if !status.LastSyncAt.IsZero() {
		last = status.LastSyncAt.Format(time.RFC3339)
	}
	return fmt.Sprintf("workspaces=%d channels=%d users=%d messages=%d last_sync=%s thread_state=%s",
		status.Workspaces, status.Channels, status.Users, status.Messages, last, status.ThreadState)
}

func readSchemaVersion(db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRow(`pragma user_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read sqlite schema version: %w", err)
	}
	return version, nil
}

func writeSchemaVersion(db *sql.DB, version int) error {
	if _, err := db.Exec(fmt.Sprintf("pragma user_version = %d", version)); err != nil {
		return fmt.Errorf("write sqlite schema version: %w", err)
	}
	return nil
}
