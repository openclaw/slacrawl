package remotedelivery

import (
	"context"
	"database/sql"
	"errors"
)

const outboxApplicationID = 0x534c444f // SLDO
const outboxSchemaVersion = 2

// One schema owns qualified delta delivery and unqualified seed staging.
const outboxTables = `
create table binding (
 singleton integer primary key check(singleton=1),
 binding_id text not null, origin text not null, app text not null,
 archive text not null, profile text not null, scope_sha256 text not null,
 file_platform text not null, file_volume text not null, file_id text not null,
 initial_native text, initial_server_cursor text not null,
 saved_native text, server_cursor text not null,
 baseline_state text not null check(baseline_state in ('unqualified','qualified')),
 seed_scope_json blob,
 check ((baseline_state='qualified' and initial_native is not null and saved_native is not null)
     or (baseline_state='unqualified' and initial_native is null and saved_native is null
         and initial_server_cursor='' and server_cursor='' and seed_scope_json is not null))
);
create table captures (
 capture_id text primary key not null,
 native_from text, native_through text not null,
 initial_expected_cursor text not null,
 part_count integer not null,
 completed integer not null default 0 check(completed in (0,1)),
 kind text not null default 'changes' check(kind in ('changes','seed')),
 baseline_hold text not null default '' check(baseline_hold in ('','ambiguous_keys')),
 check ((kind='changes' and native_from is not null and part_count between 1 and 64 and baseline_hold='')
     or (kind='seed' and native_from is null and part_count between 2 and 8194)),
 check (baseline_hold='' or completed=0)
);
create unique index one_pending_capture on captures((1)) where completed=0;
create unique index one_seed_capture on captures(kind) where kind='seed';
create table parts (
 capture_id text not null references captures(capture_id),
 part_index integer not null check(part_index between 0 and 8193),
 body blob not null, payload_sha256 text not null,
 receipt_json blob,
 primary key(capture_id,part_index)
);
`

const outboxSchema = `
pragma application_id = 1397507151;
pragma user_version = 2;
` + outboxTables

// Rebuild both constrained capture/part tables. Copy bytes with SQL, never
// through the JSON codecs. Renaming keeps the old parts FK on captures_v1 until
// that child table is dropped; all DDL and data copies share one transaction.
const migrateOutboxV1 = `
alter table binding rename to binding_v1;
alter table captures rename to captures_v1;
alter table parts rename to parts_v1;
drop index one_pending_capture;
` + outboxTables + `
insert into binding select *, 'qualified', null from binding_v1;
insert into captures select *, 'changes', '' from captures_v1;
insert into parts select * from parts_v1;
drop table parts_v1;
drop table captures_v1;
drop table binding_v1;
pragma user_version = 2;
`

func outboxVersion(ctx context.Context, q outboxQuery) (int, error) {
	var appID, version, tables int
	if err := q.QueryRowContext(ctx, `pragma application_id`).Scan(&appID); err != nil {
		return 0, err
	}
	if err := q.QueryRowContext(ctx, `pragma user_version`).Scan(&version); err != nil {
		return 0, err
	}
	if err := q.QueryRowContext(ctx, `select count(*) from sqlite_schema where type='table' and name not like 'sqlite_%'`).Scan(&tables); err != nil {
		return 0, err
	}
	if appID != outboxApplicationID || (version != 1 && version != outboxSchemaVersion) || tables != 3 {
		return 0, errors.New("file is not a supported delivery outbox")
	}
	return version, nil
}

func (o *Outbox) upgrade(ctx context.Context) error {
	return o.write(ctx, func(conn *sql.Conn) error {
		version, err := outboxVersion(ctx, conn)
		if err != nil {
			return err
		}
		// Another opener can have completed the migration while we waited.
		if version == outboxSchemaVersion {
			return nil
		}
		_, err = conn.ExecContext(ctx, migrateOutboxV1)
		return err
	})
}
