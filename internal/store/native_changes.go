package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/openclaw/slacrawl/internal/store/storedb"
)

const nativeChangesSchema = `CREATE TABLE native_changes (
 sequence integer primary key autoincrement,
 entry_id text not null default (lower(hex(randomblob(16)))) check(length(entry_id) = 32),
 table_name text not null,
 operation text not null check(operation in ('i', 'u', 'd')),
 key_json text not null check(json_valid(key_json)),
 workspace_id text,
 recorded_at text not null default (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
)`

const nativeChangeStateSchema = `CREATE TABLE native_change_state (
 singleton integer primary key check(singleton = 1),
 contract_version integer not null check(contract_version = 1),
 generation_id text not null check(length(generation_id) = 32),
 floor_sequence integer not null check(floor_sequence >= 0),
 floor_entry_id text not null check(length(floor_entry_id) = 32)
)`

// NativeChangeStatus describes a local journal, not upstream coverage or delivery.
// A consumer must reconcile when its generation or retained entry anchor differs.
type NativeChangeStatus struct {
	ContractVersion int    `json:"contract_version"`
	GenerationID    string `json:"generation_id"`
	FloorSequence   int64  `json:"floor_sequence"`
	FloorEntryID    string `json:"floor_entry_id"`
	HeadSequence    int64  `json:"head_sequence"`
	HeadEntryID     string `json:"head_entry_id"`
}

type nativeChangeTable struct {
	name      string
	keys      []string
	workspace string
}

var nativeChangeTables = []nativeChangeTable{
	{"workspaces", []string{"id"}, "%s.id"},
	{"channels", []string{"id"}, "%s.workspace_id"},
	{"users", []string{"id"}, "%s.workspace_id"},
	{"messages", []string{"channel_id", "ts"}, "%s.workspace_id"},
	{"message_files", []string{"channel_id", "ts", "file_id"}, "%s.workspace_id"},
	// Mentions have no native workspace column. Channel identity remains in the
	// key even when neither parent nor catalog can supply workspace context.
	{"message_mentions", []string{"channel_id", "ts", "mention_type", "target_id"}, ""},
	{"sync_state", []string{"source_name", "entity_type", "entity_id"}, ""},
}

func (t nativeChangeTable) scope(alias string) string {
	if t.workspace != "" {
		return fmt.Sprintf(t.workspace, alias)
	}
	if t.name == "message_mentions" {
		return fmt.Sprintf(`coalesce((select workspace_id from messages where channel_id = %[1]s.channel_id and ts = %[1]s.ts), (select workspace_id from channels where id = %[1]s.channel_id))`, alias)
	}
	return "null"
}

func (t nativeChangeTable) key(alias string) string {
	fields := make([]string, 0, len(t.keys)*2)
	for _, key := range t.keys {
		fields = append(fields, "'"+key+"'", alias+"."+key)
	}
	return "json_object(" + strings.Join(fields, ", ") + ")"
}

func (t nativeChangeTable) appendSQL(alias, operation, condition string) string {
	return fmt.Sprintf(`insert into native_changes(table_name, operation, key_json, workspace_id)
 select '%s', '%s', %s, %s where %s;`, t.name, operation, t.key(alias), t.scope(alias), condition)
}

// The source schema owns the column list. Include every native column (including
// timestamps) so an admitted update cannot disappear from the journal.
func nativeChangeTriggers(ctx context.Context, q storedb.DBTX) (map[string]string, error) {
	triggers := make(map[string]string)
	for _, table := range nativeChangeTables {
		rows, err := q.QueryContext(ctx, "pragma table_info("+table.name+")")
		if err != nil {
			return nil, err
		}
		var changed []string
		for rows.Next() {
			var cid, notNull, pk int
			var name, typ string
			var def any
			if err := rows.Scan(&cid, &name, &typ, &notNull, &def, &pk); err != nil {
				_ = rows.Close()
				return nil, err
			}
			quoted := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
			changed = append(changed, "old."+quoted+" is not new."+quoted)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		if len(changed) == 0 {
			return nil, fmt.Errorf("native journal source table missing: %s", table.name)
		}
		var moved []string
		for _, key := range table.keys {
			moved = append(moved, "old."+key+" is not new."+key)
		}
		if table.workspace != "" {
			moved = append(moved, table.scope("old")+" is not "+table.scope("new"))
		}
		for _, op := range []string{"i", "u", "d"} {
			name := "native_changes_" + table.name + "_" + op
			var event, when, body string
			switch op {
			case "i":
				event, body = "insert", table.appendSQL("new", op, "1")
			case "d":
				event, body = "delete", table.appendSQL("old", op, "1")
			case "u":
				event = "update"
				when = " when " + strings.Join(changed, " or ")
				body = table.appendSQL("old", "d", strings.Join(moved, " or ")) + table.appendSQL("new", "u", "1")
			}
			triggers[name] = "CREATE TRIGGER " + name + " after " + event + " on " + table.name + when + " begin " + body + " end"
		}
	}
	return triggers, nil
}

func normalizedNativeSQL(value string) string { return strings.Join(strings.Fields(value), " ") }

func nativeObjectMatches(ctx context.Context, q storedb.DBTX, name, expected string) (bool, error) {
	var actual string
	err := q.QueryRowContext(ctx, "select sql from sqlite_schema where name = ?", name).Scan(&actual)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return normalizedNativeSQL(actual) == normalizedNativeSQL(expected), err
}

// validateNativeChanges runs in the caller's snapshot. Read-only opens/status
// never silently recreate a missing trigger and certify the old generation.
func validateNativeChanges(ctx context.Context, q storedb.DBTX) error {
	objects, err := nativeChangeTriggers(ctx, q)
	if err != nil {
		return err
	}
	objects["native_changes"] = nativeChangesSchema
	objects["native_change_state"] = nativeChangeStateSchema
	for name, expected := range objects {
		matches, err := nativeObjectMatches(ctx, q, name, expected)
		if err != nil {
			return err
		}
		if !matches {
			return fmt.Errorf("native journal structure %s differs; open writable to repair and reconcile the new generation", name)
		}
	}
	var count int
	if err := q.QueryRowContext(ctx, `select count(*) from sqlite_schema where type = 'trigger' and name glob 'native_changes_*'`).Scan(&count); err != nil {
		return err
	}
	if count != len(objects)-2 {
		return errors.New("native journal has unexpected triggers; open writable to repair")
	}
	var valid bool
	if err := q.QueryRowContext(ctx, `select count(*) = 1 and coalesce(min(singleton = 1 and contract_version = 1
 and length(generation_id) = 32 and generation_id not glob '*[^0-9a-f]*'
 and floor_sequence >= 0 and length(floor_entry_id) = 32 and floor_entry_id not glob '*[^0-9a-f]*'
 and (floor_sequence = 0 or exists (select 1 from native_changes where sequence = floor_sequence and entry_id = floor_entry_id))), 0) from native_change_state`).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return errors.New("native journal state missing or invalid; open writable to repair")
	}
	return nil
}

func ensureNativeChanges(ctx context.Context, q storedb.DBTX) error {
	if err := validateNativeChanges(ctx, q); err == nil {
		return nil
	}
	triggers, err := nativeChangeTriggers(ctx, q)
	if err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx, `select name from sqlite_schema where type = 'trigger' and name glob 'native_changes_*'`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return err
		}
		names = append(names, name)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, err := q.ExecContext(ctx, `drop trigger "`+strings.ReplaceAll(name, `"`, `""`)+`"`); err != nil {
			return err
		}
	}
	for name, definition := range map[string]string{"native_changes": nativeChangesSchema, "native_change_state": nativeChangeStateSchema} {
		matches, err := nativeObjectMatches(ctx, q, name, definition)
		if err != nil {
			return err
		}
		if !matches {
			// Only derived journal structures are replaced. Native archive rows and
			// acquisition checkpoints remain untouched; a new generation requires a baseline.
			if _, err := q.ExecContext(ctx, "drop table if exists "+name); err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, definition); err != nil {
				return err
			}
		}
	}
	for _, definition := range triggers {
		if _, err := q.ExecContext(ctx, definition); err != nil {
			return err
		}
	}
	return resetNativeChanges(ctx, q)
}

// ResetNativeChangesInTransaction invalidates delivery cursors after an exact
// archive restore. The caller commits the restored rows and generation together.
func ResetNativeChangesInTransaction(ctx context.Context, tx *sql.Tx) error {
	return resetNativeChanges(ctx, tx)
}

func resetNativeChanges(ctx context.Context, q storedb.DBTX) error {
	_, err := q.ExecContext(ctx, `delete from native_change_state;
 insert into native_change_state(singleton, contract_version, generation_id, floor_sequence, floor_entry_id)
 values (1, 1, lower(hex(randomblob(16))),
 coalesce((select sequence from native_changes order by sequence desc limit 1), 0),
 coalesce((select entry_id from native_changes order by sequence desc limit 1), lower(hex(randomblob(16)))));`)
	return err
}

func readNativeChangeStatus(ctx context.Context, q storedb.DBTX) (*NativeChangeStatus, error) {
	var version int
	if err := q.QueryRowContext(ctx, "pragma user_version").Scan(&version); err != nil {
		return nil, err
	}
	if version < 9 {
		return nil, nil
	}
	if err := validateNativeChanges(ctx, q); err != nil {
		return nil, err
	}
	var state NativeChangeStatus
	err := q.QueryRowContext(ctx, `select contract_version, generation_id, floor_sequence, floor_entry_id,
 coalesce((select sequence from native_changes where sequence > floor_sequence order by sequence desc limit 1), floor_sequence),
 coalesce((select entry_id from native_changes where sequence > floor_sequence order by sequence desc limit 1), floor_entry_id)
 from native_change_state where singleton = 1`).Scan(&state.ContractVersion, &state.GenerationID, &state.FloorSequence, &state.FloorEntryID, &state.HeadSequence, &state.HeadEntryID)
	return &state, err
}

func initializeNativeChanges(db *sql.DB, fresh bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := ensureNativeChanges(context.Background(), tx); err != nil {
		return err
	}
	if fresh {
		if err := writeSchemaVersionTx(tx, schemaVersion); err != nil {
			return err
		}
	}
	return tx.Commit()
}
