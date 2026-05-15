-- Compile-time schema for sqlc. Runtime schema in internal/store/store.go remains authoritative.
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

create index idx_messages_workspace_ts on messages(workspace_id, ts desc);
create index idx_messages_workspace_channel_ts on messages(workspace_id, channel_id, ts desc);
create index idx_messages_workspace_user_ts on messages(workspace_id, user_id, ts desc);
create index idx_messages_key_expr on messages((channel_id || '|' || ts));

create table message_files (
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

create index idx_message_files_workspace_ts on message_files(workspace_id, ts desc);
create index idx_message_files_file_id on message_files(file_id);
create index idx_message_files_name on message_files(name);

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

create index idx_message_mentions_target_ts on message_mentions(target_id, ts desc);

create table embedding_jobs (
  id integer primary key autoincrement,
  channel_id text not null,
  ts text not null,
  state text not null,
  created_at text not null
);

create table message_fts (
  message_key text not null,
  content text not null
);

create index idx_sync_state_updated on sync_state(updated_at desc);
