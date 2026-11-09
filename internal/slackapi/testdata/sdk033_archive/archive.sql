-- Actual slacrawl v0.10.6 Go sources; slack-go v0.33.0; SQLite schema 8.
-- Producer af7814cc2f246d6a29998465e5416c6f0927e136 has identical Go/mod/sum to v0.10.6.
-- Generated offline by real Client.Sync (old-a, old-b), config.Save and store.Open.
-- Tables, rowids, raw bytes, event keys, heads, sync state and FTS were old-writer roundtrip-verified.
BEGIN;
CREATE TABLE channels (
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
CREATE TABLE embedding_jobs (
  id integer primary key autoincrement,
  channel_id text not null,
  ts text not null,
  state text not null,
  created_at text not null
);
CREATE TABLE message_event_heads (
  channel_id text not null,
  ts text not null,
  event_type text not null,
  source_name text not null,
  payload_json text not null,
  primary key (channel_id, ts, event_type, source_name)
) without rowid;
CREATE TABLE message_events (
  id integer primary key autoincrement,
  event_key text not null default (lower(hex(randomblob(16)))),
  channel_id text not null,
  ts text not null,
  event_type text not null,
  source_name text not null,
  payload_json text not null,
  created_at text not null
);
CREATE TABLE message_files (
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
CREATE VIRTUAL TABLE message_fts using fts5(message_key unindexed, content);
CREATE TABLE message_mentions (
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
CREATE TABLE messages (
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
CREATE TABLE sync_state (
  source_name text not null,
  entity_type text not null,
  entity_id text not null,
  value text not null,
  updated_at text not null,
  primary key (source_name, entity_type, entity_id)
);
CREATE TABLE users (
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
CREATE TABLE workspaces (
  id text primary key,
  name text not null,
  domain text,
  enterprise_id text,
  raw_json text not null,
  updated_at text not null
);
INSERT INTO "channels" ("rowid","id","workspace_id","name","kind","topic","purpose","is_private","is_archived","is_shared","is_general","raw_json","updated_at") VALUES (1,'CSDKFIXTURE','TSDKFIXTURE','sdk-fixture','public_channel','synthetic','archive compatibility',0,0,0,0,'{"id":"CSDKFIXTURE","created":0,"is_open":false,"is_group":false,"is_shared":false,"is_im":false,"is_ext_shared":false,"is_org_shared":false,"is_global_shared":false,"is_pending_ext_shared":false,"is_private":false,"is_read_only":false,"is_mpim":false,"is_user_deleted":false,"unlinked":0,"name_normalized":"","num_members":0,"priority":0,"user":"","name":"sdk-fixture","creator":"","is_archived":false,"members":null,"topic":{"value":"synthetic","creator":"","last_set":0},"purpose":{"value":"archive compatibility","creator":"","last_set":0},"is_channel":true,"is_general":false,"is_member":true,"locale":"","properties":null}','2024-03-09T16:03:20Z');
INSERT INTO "message_event_heads" ("channel_id","ts","event_type","source_name","payload_json") VALUES ('CSDKFIXTURE','1710000000.000100','message','api-user','{"type":"message","channel":"CSDKFIXTURE","user":"UROOT","text":"alpha \u003c@UMENTION\u003e","ts":"1710000000.000100","thread_ts":"1710000000.000100","reply_count":1,"latest_reply":"1710000001.000200","files":[{"id":"FTEST01","created":0,"timestamp":0,"name":"filemarker.txt","title":"filemarker","mimetype":"text/plain","image_exif_rotation":0,"filetype":"text","pretty_type":"","user":"UROOT","mode":"hosted","editable":false,"is_external":false,"external_type":"","size":10,"url":"","url_download":"","url_private":"https://files.example.test/filemarker.txt","url_private_download":"","original_h":0,"original_w":0,"thumb_64":"","thumb_80":"","thumb_160":"","thumb_360":"","thumb_360_gif":"","thumb_360_w":0,"thumb_360_h":0,"thumb_480":"","thumb_480_w":0,"thumb_480_h":0,"thumb_720":"","thumb_720_w":0,"thumb_720_h":0,"thumb_960":"","thumb_960_w":0,"thumb_960_h":0,"thumb_1024":"","thumb_1024_w":0,"thumb_1024_h":0,"thumb_video":"","thumb_video_w":0,"thumb_video_h":0,"thumb_video_ts":"","duration_ms":0,"hls":"","hls_embed":"","hls_preview":"","media_display_type":"","mp4":"","mp4_low":"","vtt":"","permalink":"","permalink_public":"","edit_link":"","preview":"","preview_highlight":"","lines":0,"lines_more":0,"is_public":false,"public_url_shared":false,"channels":null,"groups":null,"ims":null,"initial_comment":{},"comments_count":0,"num_stars":0,"is_starred":false,"shares":{"public":null,"private":null},"subject":"","to":null,"from":null,"cc":null,"headers":{"date":"","in_reply_to":"","reply_to":"","message_id":""},"plain_text":"filemarker","preview_plain_text":""}],"replace_original":false,"delete_original":false,"metadata":{"event_type":"","event_payload":null},"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"blockmarker"},"block_id":"fixture-section"},{"type":"context","block_id":"fixture-context","elements":[{"type":"mrkdwn","text":"contextmarker"}]}]}');
INSERT INTO "message_event_heads" ("channel_id","ts","event_type","source_name","payload_json") VALUES ('CSDKFIXTURE','1710000000.000100','message_changed','api-user','{"type":"message","channel":"CSDKFIXTURE","user":"UROOT","text":"beta \u003c@UMENTION\u003e","ts":"1710000000.000100","thread_ts":"1710000000.000100","edited":{"user":"UROOT","ts":"1710000002.000300"},"reply_count":1,"latest_reply":"1710000001.000200","files":[{"id":"FTEST01","created":0,"timestamp":0,"name":"filemarker.txt","title":"filemarker","mimetype":"text/plain","image_exif_rotation":0,"filetype":"text","pretty_type":"","user":"UROOT","mode":"hosted","editable":false,"is_external":false,"external_type":"","size":10,"url":"","url_download":"","url_private":"https://files.example.test/filemarker.txt","url_private_download":"","original_h":0,"original_w":0,"thumb_64":"","thumb_80":"","thumb_160":"","thumb_360":"","thumb_360_gif":"","thumb_360_w":0,"thumb_360_h":0,"thumb_480":"","thumb_480_w":0,"thumb_480_h":0,"thumb_720":"","thumb_720_w":0,"thumb_720_h":0,"thumb_960":"","thumb_960_w":0,"thumb_960_h":0,"thumb_1024":"","thumb_1024_w":0,"thumb_1024_h":0,"thumb_video":"","thumb_video_w":0,"thumb_video_h":0,"thumb_video_ts":"","duration_ms":0,"hls":"","hls_embed":"","hls_preview":"","media_display_type":"","mp4":"","mp4_low":"","vtt":"","permalink":"","permalink_public":"","edit_link":"","preview":"","preview_highlight":"","lines":0,"lines_more":0,"is_public":false,"public_url_shared":false,"channels":null,"groups":null,"ims":null,"initial_comment":{},"comments_count":0,"num_stars":0,"is_starred":false,"shares":{"public":null,"private":null},"subject":"","to":null,"from":null,"cc":null,"headers":{"date":"","in_reply_to":"","reply_to":"","message_id":""},"plain_text":"filemarker","preview_plain_text":""}],"replace_original":false,"delete_original":false,"metadata":{"event_type":"","event_payload":null},"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"blockmarker"},"block_id":"fixture-section"},{"type":"context","block_id":"fixture-context","elements":[{"type":"mrkdwn","text":"contextmarker"}]}]}');
INSERT INTO "message_event_heads" ("channel_id","ts","event_type","source_name","payload_json") VALUES ('CSDKFIXTURE','1710000001.000200','message','api-user','{"type":"message","channel":"CSDKFIXTURE","user":"UREPLY","text":"replymarker","ts":"1710000001.000200","thread_ts":"1710000000.000100","parent_user_id":"UROOT","replace_original":false,"delete_original":false,"metadata":{"event_type":"","event_payload":null},"blocks":null}');
INSERT INTO "message_events" ("rowid","id","event_key","channel_id","ts","event_type","source_name","payload_json","created_at") VALUES (1,1,'7ed8dd74daf1e91664fabf829966bdc9','CSDKFIXTURE','1710000000.000100','message','api-user','{"type":"message","channel":"CSDKFIXTURE","user":"UROOT","text":"alpha \u003c@UMENTION\u003e","ts":"1710000000.000100","thread_ts":"1710000000.000100","reply_count":1,"latest_reply":"1710000001.000200","files":[{"id":"FTEST01","created":0,"timestamp":0,"name":"filemarker.txt","title":"filemarker","mimetype":"text/plain","image_exif_rotation":0,"filetype":"text","pretty_type":"","user":"UROOT","mode":"hosted","editable":false,"is_external":false,"external_type":"","size":10,"url":"","url_download":"","url_private":"https://files.example.test/filemarker.txt","url_private_download":"","original_h":0,"original_w":0,"thumb_64":"","thumb_80":"","thumb_160":"","thumb_360":"","thumb_360_gif":"","thumb_360_w":0,"thumb_360_h":0,"thumb_480":"","thumb_480_w":0,"thumb_480_h":0,"thumb_720":"","thumb_720_w":0,"thumb_720_h":0,"thumb_960":"","thumb_960_w":0,"thumb_960_h":0,"thumb_1024":"","thumb_1024_w":0,"thumb_1024_h":0,"thumb_video":"","thumb_video_w":0,"thumb_video_h":0,"thumb_video_ts":"","duration_ms":0,"hls":"","hls_embed":"","hls_preview":"","media_display_type":"","mp4":"","mp4_low":"","vtt":"","permalink":"","permalink_public":"","edit_link":"","preview":"","preview_highlight":"","lines":0,"lines_more":0,"is_public":false,"public_url_shared":false,"channels":null,"groups":null,"ims":null,"initial_comment":{},"comments_count":0,"num_stars":0,"is_starred":false,"shares":{"public":null,"private":null},"subject":"","to":null,"from":null,"cc":null,"headers":{"date":"","in_reply_to":"","reply_to":"","message_id":""},"plain_text":"filemarker","preview_plain_text":""}],"replace_original":false,"delete_original":false,"metadata":{"event_type":"","event_payload":null},"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"blockmarker"},"block_id":"fixture-section"},{"type":"context","block_id":"fixture-context","elements":[{"type":"mrkdwn","text":"contextmarker"}]}]}','2024-03-09T16:01:40Z');
INSERT INTO "message_events" ("rowid","id","event_key","channel_id","ts","event_type","source_name","payload_json","created_at") VALUES (2,2,'6bf1aa035dca890483a9e1520fe3e2fa','CSDKFIXTURE','1710000001.000200','message','api-user','{"type":"message","channel":"CSDKFIXTURE","user":"UREPLY","text":"replymarker","ts":"1710000001.000200","thread_ts":"1710000000.000100","parent_user_id":"UROOT","replace_original":false,"delete_original":false,"metadata":{"event_type":"","event_payload":null},"blocks":null}','2024-03-09T16:01:40Z');
INSERT INTO "message_events" ("rowid","id","event_key","channel_id","ts","event_type","source_name","payload_json","created_at") VALUES (3,3,'031b46ccc2b47bd2dbabde52aa2c9b53','CSDKFIXTURE','1710000000.000100','message_changed','api-user','{"type":"message","channel":"CSDKFIXTURE","user":"UROOT","text":"beta \u003c@UMENTION\u003e","ts":"1710000000.000100","thread_ts":"1710000000.000100","edited":{"user":"UROOT","ts":"1710000002.000300"},"reply_count":1,"latest_reply":"1710000001.000200","files":[{"id":"FTEST01","created":0,"timestamp":0,"name":"filemarker.txt","title":"filemarker","mimetype":"text/plain","image_exif_rotation":0,"filetype":"text","pretty_type":"","user":"UROOT","mode":"hosted","editable":false,"is_external":false,"external_type":"","size":10,"url":"","url_download":"","url_private":"https://files.example.test/filemarker.txt","url_private_download":"","original_h":0,"original_w":0,"thumb_64":"","thumb_80":"","thumb_160":"","thumb_360":"","thumb_360_gif":"","thumb_360_w":0,"thumb_360_h":0,"thumb_480":"","thumb_480_w":0,"thumb_480_h":0,"thumb_720":"","thumb_720_w":0,"thumb_720_h":0,"thumb_960":"","thumb_960_w":0,"thumb_960_h":0,"thumb_1024":"","thumb_1024_w":0,"thumb_1024_h":0,"thumb_video":"","thumb_video_w":0,"thumb_video_h":0,"thumb_video_ts":"","duration_ms":0,"hls":"","hls_embed":"","hls_preview":"","media_display_type":"","mp4":"","mp4_low":"","vtt":"","permalink":"","permalink_public":"","edit_link":"","preview":"","preview_highlight":"","lines":0,"lines_more":0,"is_public":false,"public_url_shared":false,"channels":null,"groups":null,"ims":null,"initial_comment":{},"comments_count":0,"num_stars":0,"is_starred":false,"shares":{"public":null,"private":null},"subject":"","to":null,"from":null,"cc":null,"headers":{"date":"","in_reply_to":"","reply_to":"","message_id":""},"plain_text":"filemarker","preview_plain_text":""}],"replace_original":false,"delete_original":false,"metadata":{"event_type":"","event_payload":null},"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"blockmarker"},"block_id":"fixture-section"},{"type":"context","block_id":"fixture-context","elements":[{"type":"mrkdwn","text":"contextmarker"}]}]}','2024-03-09T16:03:20Z');
INSERT INTO "message_files" ("rowid","workspace_id","channel_id","ts","file_id","user_id","name","title","mimetype","filetype","pretty_type","mode","size","url_private","url_private_download","permalink","is_public","plain_text","preview_plain_text","media_path","content_sha256","content_size","fetched_at","fetch_status","fetch_error","raw_json","updated_at","deleted_at","deletion_source","deletion_reason") VALUES (1,'TSDKFIXTURE','CSDKFIXTURE','1710000000.000100','FTEST01','UROOT','filemarker.txt','filemarker','text/plain','text','','hosted',10,'https://files.example.test/filemarker.txt','','',0,'filemarker','','','',0,'','','','{"id":"FTEST01","created":0,"timestamp":0,"name":"filemarker.txt","title":"filemarker","mimetype":"text/plain","image_exif_rotation":0,"filetype":"text","pretty_type":"","user":"UROOT","mode":"hosted","editable":false,"is_external":false,"external_type":"","size":10,"url":"","url_download":"","url_private":"https://files.example.test/filemarker.txt","url_private_download":"","original_h":0,"original_w":0,"thumb_64":"","thumb_80":"","thumb_160":"","thumb_360":"","thumb_360_gif":"","thumb_360_w":0,"thumb_360_h":0,"thumb_480":"","thumb_480_w":0,"thumb_480_h":0,"thumb_720":"","thumb_720_w":0,"thumb_720_h":0,"thumb_960":"","thumb_960_w":0,"thumb_960_h":0,"thumb_1024":"","thumb_1024_w":0,"thumb_1024_h":0,"thumb_video":"","thumb_video_w":0,"thumb_video_h":0,"thumb_video_ts":"","duration_ms":0,"hls":"","hls_embed":"","hls_preview":"","media_display_type":"","mp4":"","mp4_low":"","vtt":"","permalink":"","permalink_public":"","edit_link":"","preview":"","preview_highlight":"","lines":0,"lines_more":0,"is_public":false,"public_url_shared":false,"channels":null,"groups":null,"ims":null,"initial_comment":{},"comments_count":0,"num_stars":0,"is_starred":false,"shares":{"public":null,"private":null},"subject":"","to":null,"from":null,"cc":null,"headers":{"date":"","in_reply_to":"","reply_to":"","message_id":""},"plain_text":"filemarker","preview_plain_text":""}','2024-03-09T16:03:20Z',NULL,NULL,NULL);
INSERT INTO "message_fts" ("rowid","message_key","content") VALUES (1,'CSDKFIXTURE|1710000000.000100','beta @UMENTION blockmarker contextmarker filemarker filemarker.txt [edited] blockmarker contextmarker filemarker.txt filemarker filemarker');
INSERT INTO "message_fts" ("rowid","message_key","content") VALUES (2,'CSDKFIXTURE|1710000001.000200','replymarker [thread-reply]');
INSERT INTO "message_mentions" ("rowid","channel_id","ts","mention_type","target_id","display_text","deleted_at","deletion_source","deletion_reason","updated_at") VALUES (1,'CSDKFIXTURE','1710000000.000100','user','UMENTION','UMENTION',NULL,NULL,NULL,'2024-03-09T16:03:20Z');
INSERT INTO "messages" ("rowid","channel_id","ts","workspace_id","user_id","subtype","client_msg_id","thread_ts","parent_user_id","text","normalized_text","reply_count","latest_reply","edited_ts","deleted_ts","source_rank","source_name","raw_json","updated_at") VALUES (1,'CSDKFIXTURE','1710000000.000100','TSDKFIXTURE','UROOT','','','1710000000.000100','','beta <@UMENTION>','beta @UMENTION blockmarker contextmarker filemarker filemarker.txt [edited] blockmarker contextmarker',1,'1710000001.000200','1710000002.000300','',1,'api-user','{"type":"message","channel":"CSDKFIXTURE","user":"UROOT","text":"beta \u003c@UMENTION\u003e","ts":"1710000000.000100","thread_ts":"1710000000.000100","edited":{"user":"UROOT","ts":"1710000002.000300"},"reply_count":1,"latest_reply":"1710000001.000200","files":[{"id":"FTEST01","created":0,"timestamp":0,"name":"filemarker.txt","title":"filemarker","mimetype":"text/plain","image_exif_rotation":0,"filetype":"text","pretty_type":"","user":"UROOT","mode":"hosted","editable":false,"is_external":false,"external_type":"","size":10,"url":"","url_download":"","url_private":"https://files.example.test/filemarker.txt","url_private_download":"","original_h":0,"original_w":0,"thumb_64":"","thumb_80":"","thumb_160":"","thumb_360":"","thumb_360_gif":"","thumb_360_w":0,"thumb_360_h":0,"thumb_480":"","thumb_480_w":0,"thumb_480_h":0,"thumb_720":"","thumb_720_w":0,"thumb_720_h":0,"thumb_960":"","thumb_960_w":0,"thumb_960_h":0,"thumb_1024":"","thumb_1024_w":0,"thumb_1024_h":0,"thumb_video":"","thumb_video_w":0,"thumb_video_h":0,"thumb_video_ts":"","duration_ms":0,"hls":"","hls_embed":"","hls_preview":"","media_display_type":"","mp4":"","mp4_low":"","vtt":"","permalink":"","permalink_public":"","edit_link":"","preview":"","preview_highlight":"","lines":0,"lines_more":0,"is_public":false,"public_url_shared":false,"channels":null,"groups":null,"ims":null,"initial_comment":{},"comments_count":0,"num_stars":0,"is_starred":false,"shares":{"public":null,"private":null},"subject":"","to":null,"from":null,"cc":null,"headers":{"date":"","in_reply_to":"","reply_to":"","message_id":""},"plain_text":"filemarker","preview_plain_text":""}],"replace_original":false,"delete_original":false,"metadata":{"event_type":"","event_payload":null},"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"blockmarker"},"block_id":"fixture-section"},{"type":"context","block_id":"fixture-context","elements":[{"type":"mrkdwn","text":"contextmarker"}]}]}','2024-03-09T16:03:20Z');
INSERT INTO "messages" ("rowid","channel_id","ts","workspace_id","user_id","subtype","client_msg_id","thread_ts","parent_user_id","text","normalized_text","reply_count","latest_reply","edited_ts","deleted_ts","source_rank","source_name","raw_json","updated_at") VALUES (2,'CSDKFIXTURE','1710000001.000200','TSDKFIXTURE','UREPLY','','','1710000000.000100','UROOT','replymarker','replymarker [thread-reply]',0,'','','',1,'api-user','{"type":"message","channel":"CSDKFIXTURE","user":"UREPLY","text":"replymarker","ts":"1710000001.000200","thread_ts":"1710000000.000100","parent_user_id":"UROOT","replace_original":false,"delete_original":false,"metadata":{"event_type":"","event_payload":null},"blocks":null}','2024-03-09T16:03:20Z');
INSERT INTO "sync_state" ("rowid","source_name","entity_type","entity_id","value","updated_at") VALUES (1,'api-user','history_coverage_v1','["TSDKFIXTURE","CSDKFIXTURE",""]','{"complete":true,"latest":"1710000200.000000"}','2026-11-09T06:37:27.707327Z');
INSERT INTO "sync_state" ("rowid","source_name","entity_type","entity_id","value","updated_at") VALUES (2,'doctor','threads','coverage','full','2026-11-09T06:37:27.709078Z');
INSERT INTO "sync_state" ("rowid","source_name","entity_type","entity_id","value","updated_at") VALUES (3,'api-user','workspace','TSDKFIXTURE','2024-03-09T16:03:20Z','2026-11-09T06:37:27.709409Z');
INSERT INTO "users" ("rowid","id","workspace_id","name","real_name","display_name","title","is_bot","is_deleted","raw_json","updated_at") VALUES (1,'UROOT','TSDKFIXTURE','uroot','Synthetic UROOT','uroot','Fixture',0,0,'{"id":"UROOT","team_id":"","name":"uroot","deleted":false,"color":"","real_name":"Synthetic UROOT","tz_label":"","tz_offset":0,"profile":{"real_name":"","real_name_normalized":"","display_name":"uroot","display_name_normalized":"","avatar_hash":"","image_24":"","image_32":"","image_48":"","image_72":"","image_192":"","image_512":"","title":"Fixture","team":"","fields":{}},"is_bot":false,"is_admin":false,"is_owner":false,"is_primary_owner":false,"is_restricted":false,"is_ultra_restricted":false,"is_stranger":false,"is_app_user":false,"is_connector_bot":false,"is_workflow_bot":false,"is_invited_user":false,"is_email_confirmed":false,"two_factor_type":null,"has_files":false,"presence":"","locale":"","updated":0,"enterprise_user":{"id":"","enterprise_id":"","enterprise_name":"","is_admin":false,"is_owner":false,"is_primary_owner":false,"teams":null}}','2024-03-09T16:03:20Z');
INSERT INTO "users" ("rowid","id","workspace_id","name","real_name","display_name","title","is_bot","is_deleted","raw_json","updated_at") VALUES (2,'UMENTION','TSDKFIXTURE','umention','Synthetic UMENTION','umention','Fixture',0,0,'{"id":"UMENTION","team_id":"","name":"umention","deleted":false,"color":"","real_name":"Synthetic UMENTION","tz_label":"","tz_offset":0,"profile":{"real_name":"","real_name_normalized":"","display_name":"umention","display_name_normalized":"","avatar_hash":"","image_24":"","image_32":"","image_48":"","image_72":"","image_192":"","image_512":"","title":"Fixture","team":"","fields":{}},"is_bot":false,"is_admin":false,"is_owner":false,"is_primary_owner":false,"is_restricted":false,"is_ultra_restricted":false,"is_stranger":false,"is_app_user":false,"is_connector_bot":false,"is_workflow_bot":false,"is_invited_user":false,"is_email_confirmed":false,"two_factor_type":null,"has_files":false,"presence":"","locale":"","updated":0,"enterprise_user":{"id":"","enterprise_id":"","enterprise_name":"","is_admin":false,"is_owner":false,"is_primary_owner":false,"teams":null}}','2024-03-09T16:03:20Z');
INSERT INTO "users" ("rowid","id","workspace_id","name","real_name","display_name","title","is_bot","is_deleted","raw_json","updated_at") VALUES (3,'UREPLY','TSDKFIXTURE','ureply','Synthetic UREPLY','ureply','Fixture',0,0,'{"id":"UREPLY","team_id":"","name":"ureply","deleted":false,"color":"","real_name":"Synthetic UREPLY","tz_label":"","tz_offset":0,"profile":{"real_name":"","real_name_normalized":"","display_name":"ureply","display_name_normalized":"","avatar_hash":"","image_24":"","image_32":"","image_48":"","image_72":"","image_192":"","image_512":"","title":"Fixture","team":"","fields":{}},"is_bot":false,"is_admin":false,"is_owner":false,"is_primary_owner":false,"is_restricted":false,"is_ultra_restricted":false,"is_stranger":false,"is_app_user":false,"is_connector_bot":false,"is_workflow_bot":false,"is_invited_user":false,"is_email_confirmed":false,"two_factor_type":null,"has_files":false,"presence":"","locale":"","updated":0,"enterprise_user":{"id":"","enterprise_id":"","enterprise_name":"","is_admin":false,"is_owner":false,"is_primary_owner":false,"teams":null}}','2024-03-09T16:03:20Z');
INSERT INTO "workspaces" ("rowid","id","name","domain","enterprise_id","raw_json","updated_at") VALUES (1,'TSDKFIXTURE','SDK fixture','','','{"url":"https://fixture.example.test/","team":"SDK fixture","user":"fixture","team_id":"TSDKFIXTURE","user_id":"UROOT","bot_id":""}','2024-03-09T16:03:20Z');
DELETE FROM sqlite_sequence;
INSERT INTO sqlite_sequence (name,seq) VALUES ('message_events',3);
CREATE INDEX idx_message_events_channel_ts
on message_events(channel_id, ts);
CREATE UNIQUE INDEX idx_message_events_identity
on message_events(event_key);
CREATE INDEX idx_message_files_file_id on message_files(file_id);
CREATE INDEX idx_message_files_name on message_files(name);
CREATE INDEX idx_message_files_workspace_ts on message_files(workspace_id, ts desc);
CREATE INDEX idx_message_mentions_target_ts on message_mentions(target_id, ts desc);
CREATE INDEX idx_messages_channel_thread on messages(channel_id, thread_ts);
CREATE INDEX idx_messages_key_expr on messages((channel_id || '|' || ts));
CREATE INDEX idx_messages_workspace_channel_ts on messages(workspace_id, channel_id, ts desc);
CREATE INDEX idx_messages_workspace_ts on messages(workspace_id, ts desc);
CREATE INDEX idx_messages_workspace_user_ts on messages(workspace_id, user_id, ts desc);
CREATE INDEX idx_sync_state_updated on sync_state(updated_at desc);
CREATE TRIGGER seed_message_event_head_before_update
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
PRAGMA user_version=8;
COMMIT;
