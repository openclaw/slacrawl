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

const schemaVersion = 2

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
}

type Mention struct {
	Type        string
	TargetID    string
	DisplayText string
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

	if err := qtx.DeleteMessageFTS(ctx, key); err != nil {
		return err
	}
	if err := qtx.InsertMessageFTS(ctx, storedb.InsertMessageFTSParams{MessageKey: key, Content: message.NormalizedText}); err != nil {
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
	rows, err := s.q.ListMessages(ctx, storedb.ListMessagesParams{
		WorkspaceID: workspaceID,
		ChannelID:   channelID,
		UserID:      userID,
		Limit:       int64(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]MessageRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, MessageRow{
			WorkspaceID:    row.WorkspaceID,
			ChannelID:      row.ChannelID,
			TS:             row.Ts,
			UserID:         row.UserID,
			Text:           row.Text,
			NormalizedText: row.NormalizedText,
			ThreadTS:       row.ThreadTs,
			Subtype:        row.Subtype,
		})
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
select channel_id || '|' || ts, normalized_text
from messages
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
