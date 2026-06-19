package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	crawlstore "github.com/openclaw/crawlkit/store"
	"github.com/openclaw/slacrawl/internal/store/storedb"
)

const schemaVersion = 3

const schemaPragmas = `
pragma foreign_keys = on;
pragma journal_mode = wal;
pragma busy_timeout = 5000;
`

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

const schemaV2Migration = `
create index if not exists idx_messages_workspace_ts on messages(workspace_id, ts desc);
create index if not exists idx_messages_workspace_channel_ts on messages(workspace_id, channel_id, ts desc);
create index if not exists idx_messages_workspace_user_ts on messages(workspace_id, user_id, ts desc);
create index if not exists idx_messages_key_expr on messages((channel_id || '|' || ts));
create index if not exists idx_message_mentions_target_ts on message_mentions(target_id, ts desc);
create index if not exists idx_sync_state_updated on sync_state(updated_at desc);
`

const schemaV3Migration = `
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
	WorkspaceName  string `json:"workspace_name,omitempty"`
	ChannelID      string `json:"channel_id"`
	ChannelName    string `json:"channel_name,omitempty"`
	TS             string `json:"ts"`
	UserID         string `json:"user_id"`
	UserName       string `json:"user_name,omitempty"`
	Text           string `json:"text"`
	NormalizedText string `json:"normalized_text"`
	ThreadTS       string `json:"thread_ts"`
	ReplyCount     int    `json:"reply_count"`
	LatestReply    string `json:"latest_reply"`
	Subtype        string `json:"subtype"`
	SourceName     string `json:"source_name,omitempty"`
}

type SearchMode string

const (
	SearchModeAuto   SearchMode = "auto"
	SearchModePhrase SearchMode = "phrase"
	SearchModeTerms  SearchMode = "terms"
	SearchModeRawFTS SearchMode = "raw-fts"
)

type SearchOptions struct {
	WorkspaceID string
	Query       string
	Limit       int
	Mode        SearchMode
}

type WorkspaceCollisionError struct {
	Entity              string
	ID                  string
	ExistingWorkspaceID string
	WorkspaceID         string
}

func (e *WorkspaceCollisionError) Error() string {
	return fmt.Sprintf("%s %q already belongs to workspace %q, not %q", e.Entity, e.ID, e.ExistingWorkspaceID, e.WorkspaceID)
}

func IsWorkspaceCollision(err error, entity string) bool {
	var collision *WorkspaceCollisionError
	if !errors.As(err, &collision) {
		return false
	}
	return entity == "" || collision.Entity == entity
}

type MentionRow struct {
	WorkspaceID string `json:"workspace_id"`
	ChannelID   string `json:"channel_id"`
	TS          string `json:"ts"`
	MentionType string `json:"mention_type"`
	TargetID    string `json:"target_id"`
	DisplayText string `json:"display_text"`
}

type messageMentionDisplay struct {
	target  string
	display string
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
	ID              string
	LatestTS        string
	RetentionFloor  string
	RetentionSeeded bool
}

func (c ChannelSyncCursor) ApplyRetentionFloor(oldest string) string {
	if c.RetentionFloor == "" {
		return oldest
	}
	if oldest == "" {
		return c.RetentionFloor
	}
	oldestValue, oldestErr := strconv.ParseFloat(oldest, 64)
	floorValue, floorErr := strconv.ParseFloat(c.RetentionFloor, 64)
	if oldestErr != nil || floorErr != nil || oldestValue < floorValue {
		return c.RetentionFloor
	}
	return oldest
}

func ShouldEnforceRetention(oldest, floor string, restoreRequested bool) bool {
	if !restoreRequested {
		return true
	}
	if oldest == "" || floor == "" {
		return false
	}
	oldestValue, oldestOK := parseRetentionTimestamp(oldest)
	floorValue, floorOK := parseRetentionTimestamp(floor)
	if !oldestOK || !floorOK {
		return true
	}
	return oldestValue >= floorValue
}

type ThreadRoot struct {
	ChannelID string
	TS        string
}

type SyncStateRow struct {
	SourceName string `json:"source_name"`
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Value      string `json:"value"`
}

func Open(path string) (*Store, error) {
	base, err := crawlstore.Open(context.Background(), crawlstore.Options{Path: path})
	if err != nil {
		return nil, err
	}
	db := base.DB()
	currentVersion, err := readSchemaVersion(db)
	if err != nil {
		_ = base.Close()
		return nil, err
	}
	if currentVersion > schemaVersion {
		_ = base.Close()
		return nil, fmt.Errorf("database schema version %d is newer than this slacrawl build supports (%d)", currentVersion, schemaVersion)
	}
	if currentVersion == 0 {
		empty, err := storeSchemaEmpty(db)
		if err != nil {
			_ = base.Close()
			return nil, err
		}
		if !empty {
			currentVersion = 1
		}
	}
	if currentVersion == 0 {
		if _, err := db.Exec(schema); err != nil {
			_ = base.Close()
			return nil, err
		}
		if err := writeSchemaVersion(db, schemaVersion); err != nil {
			_ = base.Close()
			return nil, err
		}
	} else if currentVersion < schemaVersion {
		if err := migrateSchema(db, currentVersion); err != nil {
			_ = base.Close()
			return nil, err
		}
	} else {
		if _, err := db.Exec(schema); err != nil {
			_ = base.Close()
			return nil, err
		}
	}
	return &Store{db: db, q: storedb.New(db)}, nil
}

func OpenReadOnly(path string) (*Store, error) {
	base, err := crawlstore.OpenReadOnly(context.Background(), path)
	if err != nil {
		return nil, err
	}
	db := base.DB()
	currentVersion, err := readSchemaVersion(db)
	if err != nil {
		_ = base.Close()
		return nil, err
	}
	if currentVersion > schemaVersion {
		_ = base.Close()
		return nil, fmt.Errorf("database schema version %d is newer than this slacrawl build supports (%d)", currentVersion, schemaVersion)
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

// EnsureWorkspace inserts sparse provider metadata without replacing richer data
// already collected by another source.
func (s *Store) EnsureWorkspace(ctx context.Context, workspace Workspace) error {
	_, err := s.db.ExecContext(ctx, `
insert into workspaces (id, name, domain, enterprise_id, raw_json, updated_at)
values (?, ?, ?, ?, ?, ?)
on conflict(id) do nothing
`, workspace.ID, workspace.Name, dbText(workspace.Domain), dbText(workspace.EnterpriseID), workspace.RawJSON, formatDBTime(workspace.UpdatedAt))
	return err
}

func (s *Store) UpsertChannel(ctx context.Context, channel Channel) error {
	rows, err := s.q.UpsertChannel(ctx, storedb.UpsertChannelParams{
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
	if err != nil {
		return err
	}
	if rows == 0 {
		existing, err := s.getChannelWorkspaceKind(ctx, channel.ID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("channel %q upsert affected no rows", channel.ID)
			}
			return err
		}
		if isDesktopHintKind(channel.Kind) {
			return nil
		}
		if isDesktopHintKind(existing.Kind) && strings.HasPrefix(channel.Kind, "desktop_") {
			return s.replaceChannel(ctx, channel)
		}
		if strings.HasPrefix(existing.Kind, "desktop_") && strings.HasPrefix(channel.Kind, "desktop_") {
			return nil
		}
		if existing.WorkspaceID != channel.WorkspaceID {
			return &WorkspaceCollisionError{Entity: "channel", ID: channel.ID, ExistingWorkspaceID: existing.WorkspaceID, WorkspaceID: channel.WorkspaceID}
		}
		if err := rejectWorkspaceCollision(ctx, channel.WorkspaceID, "channel", channel.ID, s.q.GetChannelWorkspace); err != nil {
			return err
		}
		return fmt.Errorf("channel %q upsert affected no rows", channel.ID)
	}
	return nil
}

// EnsureChannel inserts lower-fidelity provider metadata without replacing an
// existing channel collected by a richer source.
func (s *Store) EnsureChannel(ctx context.Context, channel Channel) error {
	result, err := s.db.ExecContext(ctx, `
insert into channels (id, workspace_id, name, kind, topic, purpose, is_private, is_archived, is_shared, is_general, raw_json, updated_at)
values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
on conflict(id) do nothing
`, channel.ID, channel.WorkspaceID, channel.Name, channel.Kind, dbText(channel.Topic), dbText(channel.Purpose), boolInt(channel.IsPrivate), boolInt(channel.IsArchived), boolInt(channel.IsShared), boolInt(channel.IsGeneral), channel.RawJSON, formatDBTime(channel.UpdatedAt))
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows > 0 {
		return err
	}
	existing, err := s.getChannelWorkspaceKind(ctx, channel.ID)
	if err != nil {
		return err
	}
	if existing.WorkspaceID != channel.WorkspaceID {
		return &WorkspaceCollisionError{Entity: "channel", ID: channel.ID, ExistingWorkspaceID: existing.WorkspaceID, WorkspaceID: channel.WorkspaceID}
	}
	return nil
}

type channelWorkspaceKind struct {
	WorkspaceID string
	Kind        string
}

func (s *Store) getChannelWorkspaceKind(ctx context.Context, channelID string) (channelWorkspaceKind, error) {
	var row channelWorkspaceKind
	err := s.db.QueryRowContext(ctx, `select workspace_id, kind from channels where id = ?`, channelID).Scan(&row.WorkspaceID, &row.Kind)
	return row, err
}

func (s *Store) replaceChannel(ctx context.Context, channel Channel) error {
	_, err := s.db.ExecContext(ctx, `
update channels
set workspace_id = ?, name = ?, kind = ?, topic = ?, purpose = ?, is_private = ?, is_archived = ?, is_shared = ?, is_general = ?, raw_json = ?, updated_at = ?
where id = ?
`, channel.WorkspaceID, channel.Name, channel.Kind, dbText(channel.Topic), dbText(channel.Purpose), boolInt(channel.IsPrivate), boolInt(channel.IsArchived), boolInt(channel.IsShared), boolInt(channel.IsGeneral), channel.RawJSON, formatDBTime(channel.UpdatedAt), channel.ID)
	return err
}

func isDesktopHintKind(kind string) bool {
	switch kind {
	case "desktop_draft", "desktop_recent", "desktop_mark":
		return true
	default:
		return false
	}
}

func (s *Store) UpsertUser(ctx context.Context, user User) error {
	rows, err := s.q.UpsertUser(ctx, storedb.UpsertUserParams{
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
	if err != nil {
		return err
	}
	if rows == 0 {
		if err := rejectWorkspaceCollision(ctx, user.WorkspaceID, "user", user.ID, s.q.GetUserWorkspace); err != nil {
			return err
		}
		return fmt.Errorf("user %q upsert affected no rows", user.ID)
	}
	return nil
}

// EnsureUser inserts lower-fidelity provider metadata without replacing an
// existing user collected by a richer source.
func (s *Store) EnsureUser(ctx context.Context, user User) error {
	result, err := s.db.ExecContext(ctx, `
insert into users (id, workspace_id, name, real_name, display_name, title, is_bot, is_deleted, raw_json, updated_at)
values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
on conflict(id) do nothing
`, user.ID, user.WorkspaceID, user.Name, dbText(user.RealName), dbText(user.DisplayName), dbText(user.Title), boolInt(user.IsBot), boolInt(user.IsDeleted), user.RawJSON, formatDBTime(user.UpdatedAt))
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows > 0 {
		return err
	}
	existingWorkspaceID, err := s.q.GetUserWorkspace(ctx, user.ID)
	if err != nil {
		return err
	}
	if existingWorkspaceID != user.WorkspaceID {
		return &WorkspaceCollisionError{Entity: "user", ID: user.ID, ExistingWorkspaceID: existingWorkspaceID, WorkspaceID: user.WorkspaceID}
	}
	return nil
}

func (s *Store) UpsertMessage(ctx context.Context, message Message, mentions []Mention) error {
	_, err := s.upsertMessage(ctx, message, mentions, false, false)
	return err
}

// UpsertMessageByPriority atomically skips updates from a lower-priority source.
func (s *Store) UpsertMessageByPriority(ctx context.Context, message Message, mentions []Mention) (bool, error) {
	return s.upsertMessage(ctx, message, mentions, true, false)
}

func (s *Store) UpsertMessageWithRetention(ctx context.Context, message Message, mentions []Mention) (bool, error) {
	return s.upsertMessage(ctx, message, mentions, false, true)
}

func (s *Store) UpsertMessageByPriorityWithRetention(ctx context.Context, message Message, mentions []Mention) (bool, error) {
	return s.upsertMessage(ctx, message, mentions, true, true)
}

func (s *Store) upsertMessage(ctx context.Context, message Message, mentions []Mention, preserveHigherPriority, enforceRetention bool) (bool, error) {
	key := messageKey(message.ChannelID, message.TS)
	dbtx, commit, rollback, err := s.beginMessageTransaction(ctx, enforceRetention)
	if err != nil {
		return false, err
	}
	defer rollback()
	if enforceRetention {
		allowed, err := messageAllowedByRetention(ctx, dbtx, message)
		if err != nil {
			return false, err
		}
		if !allowed {
			return false, nil
		}
	}
	qtx := storedb.New(dbtx)

	var rows int64
	if preserveHigherPriority {
		rows, err = qtx.UpsertMessageByPriority(ctx, storedb.UpsertMessageByPriorityParams{
			ChannelID: message.ChannelID, Ts: message.TS, WorkspaceID: message.WorkspaceID,
			UserID: dbText(message.UserID), Subtype: dbText(message.Subtype), ClientMsgID: dbText(message.ClientMsgID),
			ThreadTs: dbText(message.ThreadTS), ParentUserID: dbText(message.ParentUserID), Text: message.Text,
			NormalizedText: message.NormalizedText, ReplyCount: int64(message.ReplyCount), LatestReply: dbText(message.LatestReply),
			EditedTs: dbText(message.EditedTS), DeletedTs: dbText(message.DeletedTS), SourceRank: int64(message.SourceRank),
			SourceName: message.SourceName, RawJson: message.RawJSON, UpdatedAt: formatDBTime(message.UpdatedAt),
		})
	} else {
		rows, err = qtx.UpsertMessage(ctx, storedb.UpsertMessageParams{
			ChannelID: message.ChannelID, Ts: message.TS, WorkspaceID: message.WorkspaceID,
			UserID: dbText(message.UserID), Subtype: dbText(message.Subtype), ClientMsgID: dbText(message.ClientMsgID),
			ThreadTs: dbText(message.ThreadTS), ParentUserID: dbText(message.ParentUserID), Text: message.Text,
			NormalizedText: message.NormalizedText, ReplyCount: int64(message.ReplyCount), LatestReply: dbText(message.LatestReply),
			EditedTs: dbText(message.EditedTS), DeletedTs: dbText(message.DeletedTS), SourceRank: int64(message.SourceRank),
			SourceName: message.SourceName, RawJson: message.RawJSON, UpdatedAt: formatDBTime(message.UpdatedAt),
		})
	}
	if err != nil {
		return false, err
	}
	if rows == 0 {
		if err := rejectMessageWorkspaceCollision(ctx, qtx, message); err != nil {
			return false, err
		}
		if preserveHigherPriority {
			return false, nil
		}
		return false, fmt.Errorf("message %q upsert affected no rows", key)
	}

	if err := replaceMessageMentions(ctx, qtx, message.ChannelID, message.TS, mentions); err != nil {
		return false, err
	}

	filesForSearch := message.Files
	if message.Files != nil {
		existingMedia, err := existingFileMedia(ctx, qtx, message.ChannelID, message.TS)
		if err != nil {
			return false, err
		}
		if err := qtx.DeleteMessageFiles(ctx, storedb.DeleteMessageFilesParams{ChannelID: message.ChannelID, Ts: message.TS}); err != nil {
			return false, err
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
				return false, err
			}
		}
		filesForSearch = message.Files
	} else {
		filesForSearch, err = existingFilesForSearch(ctx, dbtx, message.ChannelID, message.TS)
		if err != nil {
			return false, err
		}
	}

	if err := qtx.DeleteMessageFTS(ctx, key); err != nil {
		return false, err
	}
	searchMessage := message
	searchMessage.Files = filesForSearch
	if err := qtx.InsertMessageFTS(ctx, storedb.InsertMessageFTSParams{MessageKey: key, Content: messageSearchContent(searchMessage)}); err != nil {
		return false, err
	}

	if err := qtx.InsertMessageEvent(ctx, storedb.InsertMessageEventParams{
		ChannelID:   message.ChannelID,
		Ts:          message.TS,
		EventType:   eventType(message),
		SourceName:  message.SourceName,
		PayloadJson: message.RawJSON,
		CreatedAt:   formatDBTime(message.UpdatedAt),
	}); err != nil {
		return false, err
	}

	if err := commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) MarkMessageDeleted(ctx context.Context, message Message, mentions []Mention) error {
	_, err := s.markMessageDeleted(ctx, message, mentions, false)
	return err
}

func (s *Store) MarkMessageDeletedWithRetention(ctx context.Context, message Message, mentions []Mention) (bool, error) {
	return s.markMessageDeleted(ctx, message, mentions, true)
}

func (s *Store) DeleteMessageBySource(ctx context.Context, workspaceID, channelID, ts, sourceName string) (bool, error) {
	dbtx, commit, rollback, err := s.beginMessageTransaction(ctx, true)
	if err != nil {
		return false, err
	}
	defer rollback()

	var exists bool
	if err := dbtx.QueryRowContext(ctx, `
select exists (
  select 1
  from messages
  where workspace_id = ?
    and channel_id = ?
    and ts = ?
    and source_name = ?
)
`, workspaceID, channelID, ts, sourceName).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	for _, query := range []string{
		`delete from message_events where channel_id = ? and ts = ?`,
		`delete from message_files where channel_id = ? and ts = ?`,
		`delete from message_mentions where channel_id = ? and ts = ?`,
		`delete from embedding_jobs where channel_id = ? and ts = ?`,
	} {
		if _, err := dbtx.ExecContext(ctx, query, channelID, ts); err != nil {
			return false, err
		}
	}
	if _, err := dbtx.ExecContext(ctx, `delete from message_fts where message_key = ?`, messageKey(channelID, ts)); err != nil {
		return false, err
	}
	if _, err := dbtx.ExecContext(ctx, `
delete from messages
where workspace_id = ? and channel_id = ? and ts = ? and source_name = ?
`, workspaceID, channelID, ts, sourceName); err != nil {
		return false, err
	}
	if err := commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) markMessageDeleted(ctx context.Context, message Message, mentions []Mention, enforceRetention bool) (bool, error) {
	key := messageKey(message.ChannelID, message.TS)
	dbtx, commit, rollback, err := s.beginMessageTransaction(ctx, enforceRetention)
	if err != nil {
		return false, err
	}
	defer rollback()
	if enforceRetention {
		allowed, err := messageAllowedByRetention(ctx, dbtx, message)
		if err != nil {
			return false, err
		}
		if !allowed {
			return false, nil
		}
	}
	qtx := storedb.New(dbtx)

	updatedAt := formatDBTime(message.UpdatedAt)
	rows, err := qtx.MarkMessageDeleted(ctx, storedb.MarkMessageDeletedParams{
		DeletedTs:   dbText(message.DeletedTS),
		UpdatedAt:   updatedAt,
		ChannelID:   message.ChannelID,
		Ts:          message.TS,
		WorkspaceID: message.WorkspaceID,
	})
	if err != nil {
		return false, err
	}
	switch rows {
	case 0:
		rows, err := qtx.UpsertMessage(ctx, storedb.UpsertMessageParams{
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
			UpdatedAt:      updatedAt,
		})
		if err != nil {
			return false, err
		}
		if rows == 0 {
			if err := rejectMessageWorkspaceCollision(ctx, qtx, message); err != nil {
				return false, err
			}
			return false, fmt.Errorf("message %q upsert affected no rows", key)
		}
		if err := qtx.DeleteMessageFTS(ctx, key); err != nil {
			return false, err
		}
		if err := qtx.InsertMessageFTS(ctx, storedb.InsertMessageFTSParams{MessageKey: key, Content: messageSearchContent(message)}); err != nil {
			return false, err
		}
		if err := replaceMessageMentions(ctx, qtx, message.ChannelID, message.TS, mentions); err != nil {
			return false, err
		}
	default:
		normalizedText, err := qtx.GetMessageSearchText(ctx, storedb.GetMessageSearchTextParams{ChannelID: message.ChannelID, Ts: message.TS})
		if err != nil {
			return false, err
		}
		filesForSearch, err := existingFilesForSearch(ctx, dbtx, message.ChannelID, message.TS)
		if err != nil {
			return false, err
		}
		searchMessage := message
		searchMessage.NormalizedText = normalizedText
		searchMessage.Files = filesForSearch
		if err := qtx.DeleteMessageFTS(ctx, key); err != nil {
			return false, err
		}
		if err := qtx.InsertMessageFTS(ctx, storedb.InsertMessageFTSParams{MessageKey: key, Content: messageSearchContent(searchMessage)}); err != nil {
			return false, err
		}
	}
	if err := qtx.InsertMessageEvent(ctx, storedb.InsertMessageEventParams{
		ChannelID:   message.ChannelID,
		Ts:          message.TS,
		EventType:   eventType(message),
		SourceName:  message.SourceName,
		PayloadJson: message.RawJSON,
		CreatedAt:   updatedAt,
	}); err != nil {
		return false, err
	}
	if err := commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) beginMessageTransaction(ctx context.Context, immediate bool) (storedb.DBTX, func() error, func(), error) {
	if !immediate {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, nil, nil, err
		}
		return tx, tx.Commit, func() { _ = tx.Rollback() }, nil
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if _, err := conn.ExecContext(ctx, "begin immediate"); err != nil {
		_ = conn.Close()
		return nil, nil, nil, err
	}
	done := false
	commit := func() error {
		if _, err := conn.ExecContext(ctx, "commit"); err != nil {
			return err
		}
		done = true
		return conn.Close()
	}
	rollback := func() {
		if !done {
			_, _ = conn.ExecContext(context.Background(), "rollback")
		}
		_ = conn.Close()
	}
	return conn, commit, rollback, nil
}

func rejectMessageWorkspaceCollision(ctx context.Context, q *storedb.Queries, message Message) error {
	existing, err := q.GetMessageWorkspace(ctx, storedb.GetMessageWorkspaceParams{ChannelID: message.ChannelID, Ts: message.TS})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if existing != message.WorkspaceID {
		return &WorkspaceCollisionError{Entity: "message", ID: messageKey(message.ChannelID, message.TS), ExistingWorkspaceID: existing, WorkspaceID: message.WorkspaceID}
	}
	return nil
}

func rejectWorkspaceCollision(ctx context.Context, workspaceID, entity, id string, lookup func(context.Context, string) (string, error)) error {
	existing, err := lookup(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if existing != workspaceID {
		return &WorkspaceCollisionError{Entity: entity, ID: id, ExistingWorkspaceID: existing, WorkspaceID: workspaceID}
	}
	return nil
}

func replaceMessageMentions(ctx context.Context, qtx *storedb.Queries, channelID, ts string, mentions []Mention) error {
	if err := qtx.DeleteMessageMentions(ctx, storedb.DeleteMessageMentionsParams{ChannelID: channelID, Ts: ts}); err != nil {
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
			ChannelID:   channelID,
			Ts:          ts,
			MentionType: mention.Type,
			TargetID:    mention.TargetID,
			DisplayText: dbText(mention.DisplayText),
		}); err != nil {
			return err
		}
	}
	return nil
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

func existingFilesForSearch(ctx context.Context, q storedb.DBTX, channelID, ts string) ([]MessageFile, error) {
	rows, err := q.QueryContext(ctx, `
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

func (s *Store) DeleteSyncState(ctx context.Context, source, entityType, entityID string) error {
	return s.q.DeleteSyncState(ctx, storedb.DeleteSyncStateParams{
		SourceName: source,
		EntityType: entityType,
		EntityID:   entityID,
	})
}

func (s *Store) DeleteSyncStateByTypePrefix(ctx context.Context, source, entityType, entityIDPrefix string) error {
	return s.q.DeleteSyncStateByTypePrefix(ctx, storedb.DeleteSyncStateByTypePrefixParams{
		SourceName:   source,
		EntityType:   entityType,
		EntityIDLike: entityIDPrefix + "%",
	})
}

func (s *Store) HasSyncStateType(ctx context.Context, source, entityType string) (bool, error) {
	count, err := s.q.CountSyncStateByType(ctx, storedb.CountSyncStateByTypeParams{
		SourceName: source,
		EntityType: entityType,
	})
	return count > 0, err
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
	return s.searchFTS(ctx, workspaceID, query, limit)
}

func (s *Store) SearchMessages(ctx context.Context, opts SearchOptions) ([]MessageRow, error) {
	query := strings.TrimSpace(opts.Query)
	if query == "" {
		return nil, nil
	}
	mode := opts.Mode
	if mode == "" {
		mode = SearchModeAuto
	}
	switch mode {
	case SearchModeRawFTS:
		return s.searchFTS(ctx, opts.WorkspaceID, query, opts.Limit)
	case SearchModePhrase:
		return s.searchFTS(ctx, opts.WorkspaceID, crawlstore.FTS5Phrase(query), opts.Limit)
	case SearchModeTerms:
		return s.searchFTS(ctx, opts.WorkspaceID, termsFTS5Query(query), opts.Limit)
	case SearchModeAuto:
		return s.searchAuto(ctx, opts.WorkspaceID, query, opts.Limit)
	default:
		return nil, fmt.Errorf("unsupported search mode %q", mode)
	}
}

func (s *Store) searchAuto(ctx context.Context, workspaceID string, query string, limit int) ([]MessageRow, error) {
	candidates := []string{crawlstore.FTS5Phrase(query)}
	if terms := termsFTS5Query(query); terms != "" && terms != candidates[0] {
		candidates = append(candidates, terms)
	}

	for _, candidate := range candidates {
		rows, err := s.searchFTS(ctx, workspaceID, candidate, limit)
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			return rows, nil
		}
	}
	return s.searchLike(ctx, workspaceID, query, limit)
}

func (s *Store) searchFTS(ctx context.Context, workspaceID string, query string, limit int) ([]MessageRow, error) {
	sqlQuery := `
select m.workspace_id, coalesce(w.name, ''), m.channel_id, coalesce(c.name, ''), m.ts, m.user_id,
       coalesce(nullif(u.display_name, ''), nullif(u.real_name, ''), nullif(u.name, ''), ''),
       m.text, m.normalized_text, m.thread_ts, m.reply_count, m.latest_reply, m.subtype, m.source_name
from message_fts f
join messages m on f.message_key = m.channel_id || '|' || m.ts
left join workspaces w on w.id = m.workspace_id
left join channels c on c.id = m.channel_id
left join users u on u.id = m.user_id
where message_fts match ?
  and (? = '' or m.workspace_id = ?)
order by m.ts desc
limit ?
`
	rows, err := s.db.QueryContext(ctx, sqlQuery, query, workspaceID, workspaceID, RequireLimit(limit))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out, err := scanMessageRows(rows)
	if err != nil {
		return nil, err
	}
	return out, s.resolveMessageRowMentions(ctx, out)
}

func (s *Store) searchLike(ctx context.Context, workspaceID string, query string, limit int) ([]MessageRow, error) {
	pattern := "%" + escapeLike(strings.ToLower(strings.TrimSpace(query))) + "%"
	sqlQuery := `
select m.workspace_id, coalesce(w.name, ''), m.channel_id, coalesce(c.name, ''), m.ts, m.user_id,
       coalesce(nullif(u.display_name, ''), nullif(u.real_name, ''), nullif(u.name, ''), ''),
       m.text, m.normalized_text, m.thread_ts, m.reply_count, m.latest_reply, m.subtype, m.source_name
from messages m
left join workspaces w on w.id = m.workspace_id
left join channels c on c.id = m.channel_id
left join users u on u.id = m.user_id
where (? = '' or m.workspace_id = ?)
  and (lower(m.text) like ? escape '\' or lower(m.normalized_text) like ? escape '\')
order by m.ts desc
limit ?
`
	rows, err := s.db.QueryContext(ctx, sqlQuery, workspaceID, workspaceID, pattern, pattern, RequireLimit(limit))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out, err := scanMessageRows(rows)
	if err != nil {
		return nil, err
	}
	return out, s.resolveMessageRowMentions(ctx, out)
}

func termsFTS5Query(query string) string {
	terms := searchTerms(query)
	if len(terms) == 0 {
		return crawlstore.FTS5Phrase(query)
	}
	quoted := make([]string, 0, len(terms))
	for _, term := range terms {
		quoted = append(quoted, crawlstore.FTS5Phrase(term))
	}
	return strings.Join(quoted, " AND ")
}

func searchTerms(query string) []string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return !(r == '_' || r == '-' || r == '@' || r == '#' || r == '.' || unicode.IsLetter(r) || unicode.IsDigit(r))
	})
	out := make([]string, 0, len(fields))
	seen := map[string]struct{}{}
	for _, field := range fields {
		field = strings.Trim(field, "_-.@#")
		if field == "" {
			continue
		}
		key := strings.ToLower(field)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, field)
	}
	return out
}

func escapeLike(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '\\', '%', '_':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *Store) Messages(ctx context.Context, workspaceID string, channelID string, userID string, limit int) ([]MessageRow, error) {
	query := `
select m.workspace_id, coalesce(w.name, ''), m.channel_id, coalesce(c.name, ''), m.ts, m.user_id,
       coalesce(nullif(u.display_name, ''), nullif(u.real_name, ''), nullif(u.name, ''), ''),
       m.text, m.normalized_text, m.thread_ts, m.reply_count, m.latest_reply, m.subtype, m.source_name
from messages m
left join workspaces w on w.id = m.workspace_id
left join channels c on c.id = m.channel_id
left join users u on u.id = m.user_id
where 1=1`
	args := []any{}
	if workspaceID != "" {
		query += ` and m.workspace_id = ?`
		args = append(args, workspaceID)
	}
	if channelID != "" {
		query += ` and m.channel_id = ?`
		args = append(args, channelID)
	}
	if userID != "" {
		query += ` and m.user_id = ?`
		args = append(args, userID)
	}
	query += ` order by m.ts desc limit ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanMessageRows(rows)
	if err != nil {
		return nil, err
	}
	return out, s.resolveMessageRowMentions(ctx, out)
}

func (s *Store) MessagesWithThreadContext(ctx context.Context, workspaceID string, channelID string, userID string, limit int) ([]MessageRow, error) {
	rows, err := s.Messages(ctx, workspaceID, channelID, userID, limit)
	if err != nil {
		return nil, err
	}
	return s.hydrateThreadContext(ctx, rows, limit)
}

func (s *Store) hydrateThreadContext(ctx context.Context, rows []MessageRow, limit int) ([]MessageRow, error) {
	if len(rows) == 0 {
		return rows, nil
	}
	type threadRef struct {
		workspaceID string
		channelID   string
		threadTS    string
	}
	refs := make([]threadRef, 0)
	seenRefs := map[string]struct{}{}
	for _, row := range rows {
		threadTS := slackThreadRootTS(row)
		if threadTS == "" {
			continue
		}
		key := row.WorkspaceID + "\x00" + row.ChannelID + "\x00" + threadTS
		if _, ok := seenRefs[key]; ok {
			continue
		}
		seenRefs[key] = struct{}{}
		refs = append(refs, threadRef{workspaceID: row.WorkspaceID, channelID: row.ChannelID, threadTS: threadTS})
	}
	if len(refs) == 0 {
		return rows, nil
	}
	clauses := make([]string, 0, len(refs))
	args := make([]any, 0, len(refs)*4+1)
	for _, ref := range refs {
		clauses = append(clauses, `(m.workspace_id = ? and m.channel_id = ? and (m.ts = ? or m.thread_ts = ?))`)
		args = append(args, ref.workspaceID, ref.channelID, ref.threadTS, ref.threadTS)
	}
	contextLimit := limit * 5
	if contextLimit < len(rows) {
		contextLimit = len(rows)
	}
	if contextLimit < 200 {
		contextLimit = 200
	}
	if contextLimit > 2000 {
		contextLimit = 2000
	}
	query := `
select m.workspace_id, coalesce(w.name, ''), m.channel_id, coalesce(c.name, ''), m.ts, m.user_id,
       coalesce(nullif(u.display_name, ''), nullif(u.real_name, ''), nullif(u.name, ''), ''),
       m.text, m.normalized_text, m.thread_ts, m.reply_count, m.latest_reply, m.subtype, m.source_name
from messages m
left join workspaces w on w.id = m.workspace_id
left join channels c on c.id = m.channel_id
left join users u on u.id = m.user_id
where ` + strings.Join(clauses, " or ") + `
order by m.ts desc
limit ?`
	args = append(args, contextLimit)
	contextRows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer contextRows.Close()
	extra, err := scanMessageRows(contextRows)
	if err != nil {
		return nil, err
	}
	if err := s.resolveMessageRowMentions(ctx, extra); err != nil {
		return nil, err
	}
	return mergeMessageRows(rows, extra), nil
}

func slackThreadRootTS(row MessageRow) string {
	threadTS := strings.TrimSpace(row.ThreadTS)
	ts := strings.TrimSpace(row.TS)
	if threadTS != "" {
		return threadTS
	}
	if row.ReplyCount > 0 || strings.TrimSpace(row.LatestReply) != "" {
		return ts
	}
	return ""
}

func mergeMessageRows(primary, extra []MessageRow) []MessageRow {
	out := make([]MessageRow, 0, len(primary)+len(extra))
	seen := map[string]struct{}{}
	appendRow := func(row MessageRow) {
		key := row.WorkspaceID + "\x00" + row.ChannelID + "\x00" + row.TS
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, row)
	}
	for _, row := range primary {
		appendRow(row)
	}
	for _, row := range extra {
		appendRow(row)
	}
	return out
}

func (s *Store) resolveMessageRowMentions(ctx context.Context, rows []MessageRow) error {
	if len(rows) == 0 {
		return nil
	}
	byKey := map[string][]messageMentionDisplay{}
	keys := make([]string, 0, len(rows))
	seen := map[string]struct{}{}
	for _, row := range rows {
		key := messageKey(row.ChannelID, row.TS)
		if strings.TrimSpace(key) == "|" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for start := 0; start < len(keys); start += 400 {
		end := min(start+400, len(keys))
		placeholders := strings.TrimRight(strings.Repeat("?,", end-start), ",")
		query := `
select mm.channel_id, mm.ts, mm.target_id,
       coalesce(nullif(u.display_name, ''), nullif(u.real_name, ''), nullif(u.name, ''), nullif(mm.display_text, ''), '')
from message_mentions mm
left join users u on u.id = mm.target_id
where mm.mention_type = 'user'
  and (mm.channel_id || '|' || mm.ts) in (` + placeholders + `)
`
		args := make([]any, 0, end-start)
		for _, key := range keys[start:end] {
			args = append(args, key)
		}
		mentionRows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		for mentionRows.Next() {
			var channelID, ts, target, display string
			if err := mentionRows.Scan(&channelID, &ts, &target, &display); err != nil {
				_ = mentionRows.Close()
				return err
			}
			display = strings.TrimSpace(display)
			target = strings.TrimSpace(target)
			if target == "" || display == "" || strings.EqualFold(display, target) {
				continue
			}
			key := messageKey(channelID, ts)
			byKey[key] = append(byKey[key], messageMentionDisplay{target: target, display: display})
		}
		if err := mentionRows.Close(); err != nil {
			return err
		}
	}
	for index := range rows {
		key := messageKey(rows[index].ChannelID, rows[index].TS)
		mentions := byKey[key]
		if len(mentions) == 0 {
			continue
		}
		rows[index].NormalizedText = replaceUserMentions(rows[index].NormalizedText, mentions)
	}
	return nil
}

func replaceUserMentions(value string, mentions []messageMentionDisplay) string {
	value = strings.TrimSpace(value)
	if value == "" || len(mentions) == 0 {
		return value
	}
	ordered := append([]messageMentionDisplay(nil), mentions...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return len(ordered[i].target) > len(ordered[j].target)
	})
	for _, mention := range ordered {
		target := strings.TrimSpace(mention.target)
		display := strings.TrimSpace(mention.display)
		if target == "" || display == "" || strings.EqualFold(target, display) {
			continue
		}
		if !strings.HasPrefix(display, "@") {
			display = "@" + display
		}
		value = regexp.MustCompile(`<@`+regexp.QuoteMeta(target)+`(?:\|[^>]+)?>`).ReplaceAllString(value, display)
		value = strings.ReplaceAll(value, "@"+target, display)
		value = strings.ReplaceAll(value, "@"+strings.ToLower(target), display)
	}
	return value
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
	if err := validateReadOnlyQuery(query); err != nil {
		return nil, err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "pragma query_only = on"); err != nil {
		return nil, err
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "pragma query_only = off")
	}()
	rows, err := conn.QueryContext(ctx, query)
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

func validateReadOnlyQuery(query string) error {
	trimmed := strings.TrimSpace(query)
	if !startsWithSQLKeyword(trimmed, "select") && !startsWithSQLKeyword(trimmed, "with") {
		return errors.New("only read-only select statements are allowed")
	}
	if hasAdditionalSQLStatement(trimmed) {
		return errors.New("only a single read-only select statement is allowed")
	}
	return nil
}

func startsWithSQLKeyword(query, keyword string) bool {
	if len(query) < len(keyword) {
		return false
	}
	if !strings.EqualFold(query[:len(keyword)], keyword) {
		return false
	}
	return len(query) == len(keyword) || !isSQLIdentChar(query[len(keyword)])
}

func hasAdditionalSQLStatement(query string) bool {
	for i := 0; i < len(query); i++ {
		switch query[i] {
		case '\'':
			i = scanSQLQuoted(query, i, '\'')
		case '"':
			i = scanSQLQuoted(query, i, '"')
		case '-':
			if i+1 < len(query) && query[i+1] == '-' {
				i = scanSQLLineComment(query, i+2)
			}
		case '/':
			if i+1 < len(query) && query[i+1] == '*' {
				i = scanSQLBlockComment(query, i+2)
			}
		case ';':
			return strings.TrimSpace(stripSQLLeadingComments(query[i+1:])) != ""
		}
	}
	return false
}

func scanSQLQuoted(query string, start int, quote byte) int {
	for i := start + 1; i < len(query); i++ {
		if query[i] != quote {
			continue
		}
		if i+1 < len(query) && query[i+1] == quote {
			i++
			continue
		}
		return i
	}
	return len(query) - 1
}

func scanSQLLineComment(query string, start int) int {
	for i := start; i < len(query); i++ {
		if query[i] == '\n' || query[i] == '\r' {
			return i
		}
	}
	return len(query) - 1
}

func scanSQLBlockComment(query string, start int) int {
	for i := start; i+1 < len(query); i++ {
		if query[i] == '*' && query[i+1] == '/' {
			return i + 1
		}
	}
	return len(query) - 1
}

func stripSQLLeadingComments(query string) string {
	for {
		query = strings.TrimSpace(query)
		switch {
		case strings.HasPrefix(query, "--"):
			end := strings.IndexAny(query, "\r\n")
			if end < 0 {
				return ""
			}
			query = query[end+1:]
		case strings.HasPrefix(query, "/*"):
			end := strings.Index(query[2:], "*/")
			if end < 0 {
				return ""
			}
			query = query[end+4:]
		default:
			return query
		}
	}
}

func isSQLIdentChar(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
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
		out = append(out, ChannelSyncCursor{
			ID:              row.ID,
			LatestTS:        row.LatestTs,
			RetentionFloor:  row.RetentionFloor,
			RetentionSeeded: row.RetentionSeeded != 0,
		})
	}
	return out, nil
}

func (s *Store) ChannelRetentionFloor(ctx context.Context, workspaceID, channelID string) (string, error) {
	return retentionFloor(ctx, s.db, workspaceID, channelID)
}

func (s *Store) ChannelRetentionSeeded(ctx context.Context, workspaceID, channelID string) (bool, error) {
	var seeded bool
	err := s.db.QueryRowContext(ctx, `
select exists (
  select 1
  from sync_state
  where source_name = ?
    and entity_type = ?
    and entity_id = ?
)
`, retentionFloorSource, retentionSeedEntityType, workspaceID+"|"+channelID).Scan(&seeded)
	return seeded, err
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func retentionFloor(ctx context.Context, q queryRower, workspaceID, channelID string) (string, error) {
	var value string
	err := q.QueryRowContext(ctx, `
select value
from sync_state
where source_name = ?
  and (
    (entity_type = ? and entity_id = ?)
    or (entity_type = ? and entity_id in (?, '*'))
  )
order by cast(value as real) desc
limit 1
`,
		retentionFloorSource,
		retentionFloorEntityType,
		workspaceID+"|"+channelID,
		retentionScopeEntityType,
		workspaceID,
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func messageAllowedByRetention(ctx context.Context, q storedb.DBTX, message Message) (bool, error) {
	floor, err := retentionFloor(ctx, q, message.WorkspaceID, message.ChannelID)
	if err != nil {
		return false, err
	}
	retentionTS := strings.TrimSpace(message.ThreadTS)
	if retentionTS == "" {
		retentionTS = message.TS
	}
	if floor == "" || retentionTimestampAtLeast(retentionTS, floor) {
		return true, nil
	}
	var exists bool
	err = q.QueryRowContext(ctx, `
select exists (
  select 1
  from messages
  where workspace_id = ? and channel_id = ? and ts = ?
)
`, message.WorkspaceID, message.ChannelID, message.TS).Scan(&exists)
	return exists, err
}

func retentionTimestampAtLeast(value, floor string) bool {
	valueNumber, valueOK := parseRetentionTimestamp(value)
	floorNumber, floorOK := parseRetentionTimestamp(floor)
	if valueOK && floorOK {
		return valueNumber >= floorNumber
	}
	return value >= floor
}

func parseRetentionTimestamp(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if parsed, err := strconv.ParseFloat(value, 64); err == nil {
		return parsed, true
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return float64(parsed.Unix()) + float64(parsed.Nanosecond())/float64(time.Second), true
	}
	return 0, false
}

func (s *Store) ChannelThreadRoots(ctx context.Context, workspaceID, channelID string) ([]ThreadRoot, error) {
	rows, err := s.db.QueryContext(ctx, `
select m.channel_id, m.ts
from messages m
where m.workspace_id = ?
  and m.channel_id = ?
  and coalesce(m.thread_ts, '') = ''
  and (
    m.reply_count > 0
    or exists (
      select 1 from messages r
      where r.workspace_id = m.workspace_id
        and r.channel_id = m.channel_id
        and r.thread_ts = m.ts
    )
  )
order by m.ts
`, workspaceID, channelID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var roots []ThreadRoot
	for rows.Next() {
		var root ThreadRoot
		if err := rows.Scan(&root.ChannelID, &root.TS); err != nil {
			return nil, err
		}
		roots = append(roots, root)
	}
	return roots, rows.Err()
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
		if err := rows.Scan(&row.WorkspaceID, &row.WorkspaceName, &row.ChannelID, &row.ChannelName, &row.TS, &row.UserID, &row.UserName, &row.Text, &row.NormalizedText, &row.ThreadTS, &row.ReplyCount, &row.LatestReply, &row.Subtype, &row.SourceName); err != nil {
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
	return strings.TrimSpace(channelID) + "|" + strings.TrimSpace(ts)
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

func RequireLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	return limit
}

func readSchemaVersion(db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRow(`pragma user_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read sqlite schema version: %w", err)
	}
	return version, nil
}

func storeSchemaEmpty(db *sql.DB) (bool, error) {
	var count int
	err := db.QueryRow(`
select count(*)
from sqlite_master
where type in ('table', 'view')
  and name in ('workspaces', 'channels', 'users', 'messages', 'sync_state', 'message_fts')
`).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("inspect sqlite schema: %w", err)
	}
	return count == 0, nil
}

func migrateSchema(db *sql.DB, currentVersion int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if currentVersion < 2 {
		if _, err := tx.Exec(schemaV2Migration); err != nil {
			return fmt.Errorf("migrate sqlite schema to v2: %w", err)
		}
		currentVersion = 2
	}
	if currentVersion < 3 {
		if _, err := tx.Exec(schemaV3Migration); err != nil {
			return fmt.Errorf("migrate sqlite schema to v3: %w", err)
		}
		currentVersion = 3
	}
	if currentVersion != schemaVersion {
		return fmt.Errorf("no migration path from sqlite schema version %d to %d", currentVersion, schemaVersion)
	}
	if err := validateCurrentSchema(tx); err != nil {
		return err
	}
	if err := writeSchemaVersionTx(tx, schemaVersion); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sqlite schema migration: %w", err)
	}
	return nil
}

type schemaQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func validateCurrentSchema(q schemaQueryer) error {
	required := map[string][]string{
		"workspaces":       {"id", "name", "domain", "enterprise_id", "raw_json", "updated_at"},
		"channels":         {"id", "workspace_id", "name", "kind", "topic", "purpose", "is_private", "is_archived", "is_shared", "is_general", "raw_json", "updated_at"},
		"users":            {"id", "workspace_id", "name", "real_name", "display_name", "title", "is_bot", "is_deleted", "raw_json", "updated_at"},
		"messages":         {"channel_id", "ts", "workspace_id", "user_id", "subtype", "client_msg_id", "thread_ts", "parent_user_id", "text", "normalized_text", "reply_count", "latest_reply", "edited_ts", "deleted_ts", "source_rank", "source_name", "raw_json", "updated_at"},
		"message_files":    {"workspace_id", "channel_id", "ts", "file_id", "user_id", "name", "title", "mimetype", "filetype", "pretty_type", "mode", "size", "url_private", "url_private_download", "permalink", "is_public", "plain_text", "preview_plain_text", "media_path", "content_sha256", "content_size", "fetched_at", "fetch_status", "fetch_error", "raw_json", "updated_at"},
		"message_events":   {"id", "channel_id", "ts", "event_type", "source_name", "payload_json", "created_at"},
		"sync_state":       {"source_name", "entity_type", "entity_id", "value", "updated_at"},
		"message_mentions": {"channel_id", "ts", "mention_type", "target_id", "display_text"},
		"embedding_jobs":   {"id", "channel_id", "ts", "state", "created_at"},
		"message_fts":      {"message_key", "content"},
	}
	for table, columns := range required {
		if err := requireSchemaColumns(q, table, columns); err != nil {
			return err
		}
	}
	return nil
}

func requireSchemaColumns(q schemaQueryer, table string, required []string) error {
	rows, err := q.QueryContext(context.Background(), fmt.Sprintf("pragma table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("inspect sqlite table %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return fmt.Errorf("inspect sqlite table %s: %w", table, err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect sqlite table %s: %w", table, err)
	}
	if len(columns) == 0 {
		return fmt.Errorf("sqlite schema missing table %s", table)
	}
	for _, column := range required {
		if !columns[column] {
			return fmt.Errorf("sqlite schema table %s missing column %s", table, column)
		}
	}
	return nil
}

func writeSchemaVersion(db *sql.DB, version int) error {
	if _, err := db.Exec(fmt.Sprintf("pragma user_version = %d", version)); err != nil {
		return fmt.Errorf("write sqlite schema version: %w", err)
	}
	return nil
}

func writeSchemaVersionTx(tx *sql.Tx, version int) error {
	if _, err := tx.Exec(fmt.Sprintf("pragma user_version = %d", version)); err != nil {
		return fmt.Errorf("write sqlite schema version: %w", err)
	}
	return nil
}
