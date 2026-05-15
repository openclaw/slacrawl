-- name: UpsertWorkspace :exec
insert into workspaces (id, name, domain, enterprise_id, raw_json, updated_at)
values (?, ?, ?, ?, ?, ?)
on conflict(id) do update set
  name=excluded.name,
  domain=excluded.domain,
  enterprise_id=excluded.enterprise_id,
  raw_json=excluded.raw_json,
  updated_at=excluded.updated_at;

-- name: UpsertChannel :exec
insert into channels (id, workspace_id, name, kind, topic, purpose, is_private, is_archived, is_shared, is_general, raw_json, updated_at)
values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
on conflict(id) do update set
  workspace_id=excluded.workspace_id,
  name=excluded.name,
  kind=excluded.kind,
  topic=excluded.topic,
  purpose=excluded.purpose,
  is_private=excluded.is_private,
  is_archived=excluded.is_archived,
  is_shared=excluded.is_shared,
  is_general=excluded.is_general,
  raw_json=excluded.raw_json,
  updated_at=excluded.updated_at;

-- name: UpsertUser :exec
insert into users (id, workspace_id, name, real_name, display_name, title, is_bot, is_deleted, raw_json, updated_at)
values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
on conflict(id) do update set
  workspace_id=excluded.workspace_id,
  name=excluded.name,
  real_name=excluded.real_name,
  display_name=excluded.display_name,
  title=excluded.title,
  is_bot=excluded.is_bot,
  is_deleted=excluded.is_deleted,
  raw_json=excluded.raw_json,
  updated_at=excluded.updated_at;

-- name: UpsertMessage :exec
insert into messages (
  channel_id, ts, workspace_id, user_id, subtype, client_msg_id, thread_ts, parent_user_id,
  text, normalized_text, reply_count, latest_reply, edited_ts, deleted_ts, source_rank,
  source_name, raw_json, updated_at
) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
on conflict(channel_id, ts) do update set
  workspace_id=excluded.workspace_id,
  user_id=excluded.user_id,
  subtype=excluded.subtype,
  client_msg_id=excluded.client_msg_id,
  thread_ts=excluded.thread_ts,
  parent_user_id=excluded.parent_user_id,
  text=excluded.text,
  normalized_text=excluded.normalized_text,
  reply_count=excluded.reply_count,
  latest_reply=excluded.latest_reply,
  edited_ts=excluded.edited_ts,
  deleted_ts=excluded.deleted_ts,
  source_rank=case
    when excluded.source_rank <= messages.source_rank then excluded.source_rank
    else messages.source_rank
  end,
  source_name=case
    when excluded.source_rank <= messages.source_rank then excluded.source_name
    else messages.source_name
  end,
  raw_json=case
    when excluded.source_rank <= messages.source_rank then excluded.raw_json
    else messages.raw_json
  end,
  updated_at=excluded.updated_at;

-- name: DeleteMessageMentions :exec
delete from message_mentions where channel_id = ? and ts = ?;

-- name: UpsertMessageMention :exec
insert into message_mentions (channel_id, ts, mention_type, target_id, display_text)
values (?, ?, ?, ?, ?)
on conflict(channel_id, ts, mention_type, target_id) do update set
  display_text=excluded.display_text;

-- name: ListExistingFileMedia :many
select file_id, coalesce(media_path, '') as media_path,
       coalesce(content_sha256, '') as content_sha256, content_size,
       coalesce(fetched_at, '') as fetched_at, fetch_status, fetch_error
from message_files
where channel_id = ? and ts = ?;

-- name: DeleteMessageFiles :exec
delete from message_files where channel_id = ? and ts = ?;

-- name: InsertMessageFile :exec
insert into message_files (
  workspace_id, channel_id, ts, file_id, user_id, name, title, mimetype, filetype,
  pretty_type, mode, size, url_private, url_private_download, permalink, is_public,
  plain_text, preview_plain_text, media_path, content_sha256, content_size, fetched_at,
  fetch_status, fetch_error, raw_json, updated_at
) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
on conflict(channel_id, ts, file_id) do update set
  workspace_id=excluded.workspace_id,
  user_id=excluded.user_id,
  name=excluded.name,
  title=excluded.title,
  mimetype=excluded.mimetype,
  filetype=excluded.filetype,
  pretty_type=excluded.pretty_type,
  mode=excluded.mode,
  size=excluded.size,
  url_private=excluded.url_private,
  url_private_download=excluded.url_private_download,
  permalink=excluded.permalink,
  is_public=excluded.is_public,
  plain_text=excluded.plain_text,
  preview_plain_text=excluded.preview_plain_text,
  media_path=excluded.media_path,
  content_sha256=excluded.content_sha256,
  content_size=excluded.content_size,
  fetched_at=excluded.fetched_at,
  fetch_status=excluded.fetch_status,
  fetch_error=excluded.fetch_error,
  raw_json=excluded.raw_json,
  updated_at=excluded.updated_at;

-- name: UpdateFileMedia :exec
update message_files
set media_path = ?,
    content_sha256 = ?,
    content_size = ?,
    fetched_at = ?,
    fetch_status = ?,
    fetch_error = ?,
    updated_at = ?
where channel_id = ? and ts = ? and file_id = ?;

-- name: UpdateFileFetchStatus :exec
update message_files
set fetched_at = ?,
    fetch_status = ?,
    fetch_error = ?,
    updated_at = ?
where channel_id = ? and ts = ? and file_id = ?;

-- name: DeleteMessageFTS :exec
delete from message_fts where message_key = ?;

-- name: InsertMessageFTS :exec
insert into message_fts (message_key, content) values (?, ?);

-- name: InsertMessageEvent :exec
insert into message_events (channel_id, ts, event_type, source_name, payload_json, created_at)
values (?, ?, ?, ?, ?, ?);

-- name: SetSyncState :exec
insert into sync_state (source_name, entity_type, entity_id, value, updated_at)
values (?, ?, ?, ?, ?)
on conflict(source_name, entity_type, entity_id) do update set
  value=excluded.value,
  updated_at=excluded.updated_at;

-- name: CountWorkspaces :one
select count(*) from workspaces;

-- name: CountChannels :one
select count(*) from channels;

-- name: CountUsers :one
select count(*) from users;

-- name: CountMessages :one
select count(*) from messages;

-- name: LastSyncAt :one
select cast(coalesce(max(updated_at), '') as text) as updated_at from sync_state where source_name != 'doctor';

-- name: ThreadCoverageState :one
select value from sync_state where source_name = 'doctor' and entity_type = 'threads' and entity_id = 'coverage';

-- name: ListMessagesAll :many
select workspace_id, channel_id, ts, coalesce(user_id, '') as user_id, text, normalized_text, coalesce(thread_ts, '') as thread_ts, coalesce(subtype, '') as subtype
from messages
order by ts desc
limit sqlc.arg(limit);

-- name: ListMessagesByWorkspace :many
select workspace_id, channel_id, ts, coalesce(user_id, '') as user_id, text, normalized_text, coalesce(thread_ts, '') as thread_ts, coalesce(subtype, '') as subtype
from messages
where workspace_id = sqlc.arg(workspace_id)
order by ts desc
limit sqlc.arg(limit);

-- name: ListMessagesByChannel :many
select workspace_id, channel_id, ts, coalesce(user_id, '') as user_id, text, normalized_text, coalesce(thread_ts, '') as thread_ts, coalesce(subtype, '') as subtype
from messages
where channel_id = sqlc.arg(channel_id)
order by ts desc
limit sqlc.arg(limit);

-- name: ListMessagesByUser :many
select workspace_id, channel_id, ts, coalesce(user_id, '') as user_id, text, normalized_text, coalesce(thread_ts, '') as thread_ts, coalesce(subtype, '') as subtype
from messages
where user_id = sqlc.arg(user_id)
order by ts desc
limit sqlc.arg(limit);

-- name: ListMessagesByWorkspaceChannel :many
select workspace_id, channel_id, ts, coalesce(user_id, '') as user_id, text, normalized_text, coalesce(thread_ts, '') as thread_ts, coalesce(subtype, '') as subtype
from messages
where workspace_id = sqlc.arg(workspace_id)
  and channel_id = sqlc.arg(channel_id)
order by ts desc
limit sqlc.arg(limit);

-- name: ListMessagesByWorkspaceUser :many
select workspace_id, channel_id, ts, coalesce(user_id, '') as user_id, text, normalized_text, coalesce(thread_ts, '') as thread_ts, coalesce(subtype, '') as subtype
from messages
where workspace_id = sqlc.arg(workspace_id)
  and user_id = sqlc.arg(user_id)
order by ts desc
limit sqlc.arg(limit);

-- name: ListMessagesByChannelUser :many
select workspace_id, channel_id, ts, coalesce(user_id, '') as user_id, text, normalized_text, coalesce(thread_ts, '') as thread_ts, coalesce(subtype, '') as subtype
from messages
where channel_id = sqlc.arg(channel_id)
  and user_id = sqlc.arg(user_id)
order by ts desc
limit sqlc.arg(limit);

-- name: ListMessagesByWorkspaceChannelUser :many
select workspace_id, channel_id, ts, coalesce(user_id, '') as user_id, text, normalized_text, coalesce(thread_ts, '') as thread_ts, coalesce(subtype, '') as subtype
from messages
where workspace_id = sqlc.arg(workspace_id)
  and channel_id = sqlc.arg(channel_id)
  and user_id = sqlc.arg(user_id)
order by ts desc
limit sqlc.arg(limit);

-- name: ListMentions :many
select m.workspace_id, mm.channel_id, mm.ts, mm.mention_type, mm.target_id, coalesce(mm.display_text, '') as display_text
from message_mentions mm
join messages m on m.channel_id = mm.channel_id and m.ts = mm.ts
where (sqlc.arg(workspace_id) = '' or m.workspace_id = sqlc.arg(workspace_id))
  and (sqlc.arg(target) = '' or mm.target_id = sqlc.arg(target) or mm.display_text like sqlc.arg(target_like))
order by mm.ts desc
limit sqlc.arg(limit);

-- name: ListUsers :many
select workspace_id, id, name, coalesce(real_name, '') as real_name, coalesce(display_name, '') as display_name, coalesce(title, '') as title
from users
where (sqlc.arg(workspace_id) = '' or workspace_id = sqlc.arg(workspace_id))
  and (sqlc.arg(query) = '' or id = sqlc.arg(query) or name like sqlc.arg(query_like) or real_name like sqlc.arg(query_like) or display_name like sqlc.arg(query_like))
order by name asc
limit sqlc.arg(limit);

-- name: ListChannelsByKind :many
select workspace_id, id, name, kind
from channels
where (sqlc.arg(workspace_id) = '' or workspace_id = sqlc.arg(workspace_id))
  and (sqlc.arg(query) = '' or id = sqlc.arg(query) or name like sqlc.arg(query_like))
  and (sqlc.arg(kind) = '' or kind = sqlc.arg(kind))
order by name asc
limit sqlc.arg(limit);

-- name: ChannelSyncCursors :many
select c.id, cast(coalesce(max(case when m.ts not like 'draft:%' then m.ts end), '') as text) as latest_ts
from channels c
left join messages m on m.channel_id = c.id and m.workspace_id = c.workspace_id
where c.workspace_id = sqlc.arg(workspace_id)
group by c.id
order by c.id asc;

-- name: RenameChannel :exec
update channels
set name = ?, updated_at = ?
where id = ?;

-- name: SetChannelArchived :exec
update channels
set is_archived = ?, updated_at = ?
where id = ?;

-- name: GetSyncState :one
select value from sync_state
where source_name = ? and entity_type = ? and entity_id = ?;

-- name: ListSyncState :many
select source_name, entity_type, entity_id, value
from sync_state
where (sqlc.arg(source_name) = '' or source_name = sqlc.arg(source_name))
  and (sqlc.arg(entity_type) = '' or entity_type = sqlc.arg(entity_type))
order by updated_at desc, entity_id asc
limit sqlc.arg(limit);
