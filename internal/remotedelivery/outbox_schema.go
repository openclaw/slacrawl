package remotedelivery

const outboxApplicationID = 0x534c444f // SLDO
const outboxSchemaVersion = 1

const outboxSchema = `
pragma application_id = 1397507151;
pragma user_version = 1;
create table binding (
 singleton integer primary key check(singleton=1),
 binding_id text not null, origin text not null, app text not null,
 archive text not null, profile text not null, scope_sha256 text not null,
 file_platform text not null, file_volume text not null, file_id text not null,
 initial_native text not null, initial_server_cursor text not null,
 saved_native text not null, server_cursor text not null
);
create table captures (
 capture_id text primary key not null,
 native_from text not null, native_through text not null,
 initial_expected_cursor text not null,
 part_count integer not null check(part_count between 1 and 64),
 completed integer not null default 0 check(completed in (0,1))
);
create unique index one_pending_capture on captures((1)) where completed=0;
create table parts (
 capture_id text not null references captures(capture_id),
 part_index integer not null check(part_index between 0 and 63),
 body blob not null, payload_sha256 text not null,
 receipt_json blob,
 primary key(capture_id,part_index)
);
`
