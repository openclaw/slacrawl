package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

const schemaVersion = 9
const PlaceholderUserRawJSON = `{"slacrawl_provider_placeholder":true}`

const (
	searchIndexMaintenanceSource     = "store"
	searchIndexMaintenanceEntityType = "search_index"
	searchIndexMaintenanceEntityID   = "rowid_rebuild_pending"
)

const messageEventHeadsTableSchema = `
create table if not exists message_event_heads (
  channel_id text not null,
  ts text not null,
  event_type text not null,
  source_name text not null,
  payload_json text not null,
  primary key (channel_id, ts, event_type, source_name)
) without rowid;
`

const messageEventHeadTriggerSchema = `
create trigger if not exists seed_message_event_head_before_update
before update on messages
begin
  insert into message_event_heads (
    channel_id, ts, event_type, source_name, payload_json
  ) values (
    old.channel_id,
    old.ts,
    case
      when trim(coalesce(old.deleted_ts, '')) <> '' then 'message_deleted'
      when trim(coalesce(old.edited_ts, '')) <> '' then 'message_changed'
      else 'message'
    end,
    old.source_name,
    old.raw_json
  ) on conflict (channel_id, ts, event_type, source_name) do nothing;
end;
`

const messageEventHeadsSchema = messageEventHeadsTableSchema + messageEventHeadTriggerSchema

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
create index if not exists idx_messages_channel_thread on messages(channel_id, thread_ts);

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
  deleted_at text,
  deletion_source text,
  deletion_reason text,
  primary key (channel_id, ts, file_id)
);

create index if not exists idx_message_files_workspace_ts on message_files(workspace_id, ts desc);
create index if not exists idx_message_files_file_id on message_files(file_id);
create index if not exists idx_message_files_name on message_files(name);

create table if not exists message_events (
  id integer primary key autoincrement,
  event_key text not null default (lower(hex(randomblob(16)))),
  channel_id text not null,
  ts text not null,
  event_type text not null,
  source_name text not null,
  payload_json text not null,
  created_at text not null
);
create unique index if not exists idx_message_events_identity
on message_events(event_key);
create index if not exists idx_message_events_channel_ts
on message_events(channel_id, ts);
` + messageEventHeadsSchema + `
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
  deleted_at text,
  deletion_source text,
  deletion_reason text,
  updated_at text not null default '',
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

const schemaV4Migration = messageEventHeadsSchema
const schemaV5Migration = messageEventHeadTriggerSchema

// schemaV7Migration adds the thread-lookup and event-lookup indexes. Without
// (channel_id, thread_ts), ChannelThreadRoots' reply-existence probe scans the
// whole channel per message — O(channel²), measured minutes-to-hours on large
// channels; with it the same query is milliseconds.
const schemaV7Migration = `
create index if not exists idx_messages_channel_thread on messages(channel_id, thread_ts);
create index if not exists idx_message_events_channel_ts on message_events(channel_id, ts);
`

// Pre-v8 checkpoints may have come from another archive and cannot prove local coverage.
const schemaV8Migration = `
delete from sync_state
where entity_type = 'history_coverage_v1'
  and source_name in ('api-bot', 'api-user');
`

const schemaV6EventMigration = `
drop index if exists idx_message_events_identity;
create table message_events_v6 (
  id integer primary key autoincrement,
  event_key text not null default (lower(hex(randomblob(16)))),
  channel_id text not null,
  ts text not null,
  event_type text not null,
  source_name text not null,
  payload_json text not null,
  created_at text not null
);
insert into message_events_v6 (id, event_key, channel_id, ts, event_type, source_name, payload_json, created_at)
select id, event_key, channel_id, ts, event_type, source_name, payload_json, created_at
from message_events;
drop table message_events;
alter table message_events_v6 rename to message_events;
create unique index if not exists idx_message_events_identity
on message_events(event_key);
`

const rebuildMessageFTSRowsSQL = `
insert into message_fts (rowid, message_key, content)
select m.rowid,
       m.channel_id || '|' || m.ts,
       trim(m.normalized_text || ' ' || coalesce((
         select group_concat(trim(f.name || ' ' || f.title || ' ' || f.plain_text || ' ' || f.preview_plain_text), ' ')
         from message_files f
         where f.channel_id = m.channel_id and f.ts = m.ts and f.deleted_at is null
       ), ''))
from messages m
`

const upsertMessageFTSRowSQL = `
insert or replace into message_fts (rowid, message_key, content)
select rowid, channel_id || '|' || ts, ?
from messages
where channel_id = ? and ts = ?
`

const deleteMessageFTSRowSQL = `
delete from message_fts
where rowid = (select rowid from messages where channel_id = ? and ts = ?)
`

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
	if currentVersion < 4 {
		if _, err := tx.Exec(schemaV4Migration); err != nil {
			return fmt.Errorf("migrate sqlite schema to v4: %w", err)
		}
		currentVersion = 4
	}
	if currentVersion < 5 {
		if _, err := tx.Exec(schemaV5Migration); err != nil {
			return fmt.Errorf("migrate sqlite schema to v5: %w", err)
		}
		currentVersion = 5
	}
	if currentVersion < 6 {
		if err := migrateTombstonesV6(tx); err != nil {
			return fmt.Errorf("migrate sqlite schema to v6: %w", err)
		}
		currentVersion = 6
	}
	if currentVersion < 7 {
		if _, err := tx.Exec(schemaV7Migration); err != nil {
			return fmt.Errorf("migrate sqlite schema to v7: %w", err)
		}
		currentVersion = 7
	}
	if currentVersion < 8 {
		if _, err := tx.Exec(schemaV8Migration); err != nil {
			return fmt.Errorf("migrate sqlite schema to v8: %w", err)
		}
		currentVersion = 8
	}
	if currentVersion < 9 {
		if err := ensureNativeChanges(context.Background(), tx); err != nil {
			return fmt.Errorf("migrate sqlite schema to v9: %w", err)
		}
		currentVersion = 9
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

func migrateTombstonesV6(tx *sql.Tx) error {
	for _, column := range []struct {
		table string
		name  string
	}{
		{"message_files", "deleted_at"},
		{"message_files", "deletion_source"},
		{"message_files", "deletion_reason"},
		{"message_mentions", "deleted_at"},
		{"message_mentions", "deletion_source"},
		{"message_mentions", "deletion_reason"},
		{"message_mentions", "updated_at"},
	} {
		exists, err := schemaColumnExists(tx, column.table, column.name)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		definition := " text"
		if column.name == "updated_at" {
			definition = " text not null default ''"
		}
		if _, err := tx.Exec("alter table " + column.table + " add column " + column.name + definition); err != nil { //nolint:gosec // Fixed migration identifiers.
			return err
		}
	}
	eventKeyExists, err := schemaColumnExists(tx, "message_events", "event_key")
	if err != nil {
		return err
	}
	if !eventKeyExists {
		if _, err := tx.Exec(`alter table message_events add column event_key text`); err != nil {
			return err
		}
	}
	type legacyEvent struct {
		id                                                       int64
		channelID, ts, eventType, sourceName, payload, createdAt string
	}
	const eventMigrationBatchSize = 500
	lastID := int64(-1)
	for {
		eventRows, err := tx.Query(`
select id, channel_id, ts, event_type, source_name, payload_json, created_at
from message_events
where (event_key is null or trim(event_key) = '') and id > ?
order by id
limit ?
`, lastID, eventMigrationBatchSize)
		if err != nil {
			return err
		}
		legacyEvents := make([]legacyEvent, 0, eventMigrationBatchSize)
		for eventRows.Next() {
			var event legacyEvent
			if err := eventRows.Scan(&event.id, &event.channelID, &event.ts, &event.eventType, &event.sourceName, &event.payload, &event.createdAt); err != nil {
				_ = eventRows.Close()
				return err
			}
			legacyEvents = append(legacyEvents, event)
		}
		if err := eventRows.Close(); err != nil {
			return err
		}
		if err := eventRows.Err(); err != nil {
			return err
		}
		if len(legacyEvents) == 0 {
			break
		}
		for _, event := range legacyEvents {
			parts := []string{strconv.FormatInt(event.id, 10), event.channelID, event.ts, event.eventType, event.sourceName, event.payload, event.createdAt}
			sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
			key := "legacy-" + hex.EncodeToString(sum[:])
			if _, err := tx.Exec(`update message_events set event_key = ? where id = ?`, key, event.id); err != nil {
				return err
			}
		}
		lastID = legacyEvents[len(legacyEvents)-1].id
	}
	if _, err := tx.Exec(`
update message_mentions
set updated_at = coalesce((
  select m.updated_at from messages m
  where m.channel_id = message_mentions.channel_id and m.ts = message_mentions.ts
), '')
where updated_at = '';
`); err != nil {
		return err
	}
	if err := BackfillDeletedSubordinates(context.Background(), tx); err != nil {
		return err
	}
	if _, err := tx.Exec(`delete from message_fts;` + rebuildMessageFTSRowsSQL + `;
delete from sync_state
where source_name = '` + searchIndexMaintenanceSource + `'
  and entity_type = '` + searchIndexMaintenanceEntityType + `'
  and entity_id = '` + searchIndexMaintenanceEntityID + `';
`); err != nil {
		return err
	}
	_, err = tx.Exec(schemaV6EventMigration)
	return err
}

func schemaColumnExists(q schemaQueryer, table, column string) (bool, error) {
	rows, err := q.QueryContext(context.Background(), fmt.Sprintf("pragma table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

type schemaQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func validateCurrentSchema(q schemaQueryer) error {
	required := map[string][]string{
		"workspaces":          {"id", "name", "domain", "enterprise_id", "raw_json", "updated_at"},
		"channels":            {"id", "workspace_id", "name", "kind", "topic", "purpose", "is_private", "is_archived", "is_shared", "is_general", "raw_json", "updated_at"},
		"users":               {"id", "workspace_id", "name", "real_name", "display_name", "title", "is_bot", "is_deleted", "raw_json", "updated_at"},
		"messages":            {"channel_id", "ts", "workspace_id", "user_id", "subtype", "client_msg_id", "thread_ts", "parent_user_id", "text", "normalized_text", "reply_count", "latest_reply", "edited_ts", "deleted_ts", "source_rank", "source_name", "raw_json", "updated_at"},
		"message_files":       {"workspace_id", "channel_id", "ts", "file_id", "user_id", "name", "title", "mimetype", "filetype", "pretty_type", "mode", "size", "url_private", "url_private_download", "permalink", "is_public", "plain_text", "preview_plain_text", "media_path", "content_sha256", "content_size", "fetched_at", "fetch_status", "fetch_error", "raw_json", "updated_at", "deleted_at", "deletion_source", "deletion_reason"},
		"message_events":      {"id", "event_key", "channel_id", "ts", "event_type", "source_name", "payload_json", "created_at"},
		"message_event_heads": {"channel_id", "ts", "event_type", "source_name", "payload_json"},
		"sync_state":          {"source_name", "entity_type", "entity_id", "value", "updated_at"},
		"message_mentions":    {"channel_id", "ts", "mention_type", "target_id", "display_text", "deleted_at", "deletion_source", "deletion_reason", "updated_at"},
		"embedding_jobs":      {"id", "channel_id", "ts", "state", "created_at"},
		"message_fts":         {"message_key", "content"},
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

func writeSchemaVersionTx(tx *sql.Tx, version int) error {
	if _, err := tx.Exec(fmt.Sprintf("pragma user_version = %d", version)); err != nil {
		return fmt.Errorf("write sqlite schema version: %w", err)
	}
	return nil
}
