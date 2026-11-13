package remotedelivery_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/slacrawl/internal/remotedelivery"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/stretchr/testify/require"
)

// Frozen historical DDL from 0a94bae. Do not derive this fixture from the v2
// schema: old constraints, column order and existing bytes are the upgrade input.
const historicalOutboxV1 = `
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

var v1Columns = []struct{ table, columns, order string }{
	{"binding", "singleton,binding_id,origin,app,archive,profile,scope_sha256,file_platform,file_volume,file_id,initial_native,initial_server_cursor,saved_native,server_cursor", "singleton"},
	{"captures", "capture_id,native_from,native_through,initial_expected_cursor,part_count,completed", "capture_id"},
	{"parts", "capture_id,part_index,body,payload_sha256,receipt_json", "capture_id,part_index"},
}

// Populate the old schema with real capture/delivery bytes from the unchanged
// delta API, not with a second implementation of the packing/receipt protocol.
func historicalOutbox(t *testing.T, current string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(historicalOutboxV1)
	require.NoError(t, err)
	_, err = db.Exec(`attach database ? as current_outbox`, current)
	require.NoError(t, err)
	for _, table := range v1Columns {
		_, err = db.Exec(`insert into ` + table.table + ` select ` + table.columns + ` from current_outbox.` + table.table)
		require.NoError(t, err)
	}
	require.NoError(t, os.Chmod(path, 0o600))
	return path
}

func outboxData(t *testing.T, path string) []byte {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	var result [][][]any
	for _, table := range v1Columns {
		rows, err := db.Query(`select ` + table.columns + ` from ` + table.table + ` order by ` + table.order)
		require.NoError(t, err)
		columns, err := rows.Columns()
		require.NoError(t, err)
		var tableRows [][]any
		for rows.Next() {
			values, targets := make([]any, len(columns)), make([]any, len(columns))
			for i := range targets {
				targets[i] = &values[i]
			}
			require.NoError(t, rows.Scan(targets...))
			for i, value := range values {
				if blob, ok := value.([]byte); ok {
					values[i] = append([]byte(nil), blob...)
				}
			}
			tableRows = append(tableRows, values)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		result = append(result, tableRows)
	}
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	return encoded
}

func TestOutboxV1MigrationPreservesPendingDelivery(t *testing.T) {
	peer, server := newDeliveryPeer(t)
	st, source, request, _ := projectionFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "current.db")
	o, err := remotedelivery.InitOutbox(ctx, path, remotedelivery.Registration{BindingID: "fixture-binding", Origin: server.URL, App: "slacrawl", Archive: "fixture", Profile: remotedelivery.PrivateArchiveProfile, Baseline: request.Baseline, ServerCursor: "seed:fixture"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, o.Close()) })
	writeCaptureMessages(t, st, ctx, 1)
	capture, err := store.CaptureNativeChanges(ctx, source, request)
	require.NoError(t, err)
	_, err = o.Stage(ctx, capture, remotedelivery.StageLimits{MaxEncodedBytes: 8 << 20})
	require.NoError(t, err)
	completedPart, err := o.NextPart(ctx)
	require.NoError(t, err)
	_, err = o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(64))
	require.NoError(t, err)
	completedReceipt := peer.receipts[readBatch(t, completedPart).BatchID()]
	state, err := o.State(ctx)
	require.NoError(t, err)
	request.Baseline = state.Baseline
	request.Limits.MaxEvents, request.Limits.MaxBytes = 600, 8<<20
	writeCaptureMessages(t, st, ctx, 502)
	capture, err = store.CaptureNativeChanges(ctx, source, request)
	require.NoError(t, err)
	_, err = o.Stage(ctx, capture, remotedelivery.StageLimits{MaxEncodedBytes: 8 << 20})
	require.NoError(t, err)
	first, err := o.NextPart(ctx)
	require.NoError(t, err)
	// Intentionally valid noncanonical JSON makes re-encoding detectable.
	first.Body = append(append([]byte("\n "), first.Body...), '\n')
	first.SHA256 = fmt.Sprintf("%x", sha256.Sum256(first.Body))
	deliverySQL(t, path, `update parts set body=?,payload_sha256=? where capture_id=? and part_index=0`, first.Body, first.SHA256, first.Ref.CaptureID)
	result, err := o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(1))
	require.NoError(t, err)
	require.Equal(t, remotedelivery.DeliveryPartLimit, result.Status)
	deliverySQL(t, path, `update parts set receipt_json=cast(char(10)||cast(receipt_json as text)||char(10) as blob) where receipt_json is not null`)
	pending, err := o.NextPart(ctx)
	require.NoError(t, err)
	beforeState, err := o.State(ctx)
	require.NoError(t, err)
	require.NoError(t, o.Close())
	oldPath := historicalOutbox(t, path)
	before := outboxData(t, oldPath)
	o = openDelivery(t, ctx, oldPath)
	require.Equal(t, before, outboxData(t, oldPath), "migration preserves all legacy fields and exact BLOB bytes")
	afterState, err := o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, beforeState, afterState)
	require.Equal(t, remotedelivery.BaselineQualified, afterState.BaselineState)
	afterPart, err := o.NextPart(ctx)
	require.NoError(t, err)
	require.Equal(t, pending, afterPart)
	completedReceipt.Replayed = true
	require.NoError(t, o.RecordReceipt(ctx, completedPart.Ref, completedReceipt))
	require.Equal(t, before, outboxData(t, oldPath), "replay does not replace original receipt bytes")
	result, err = o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(64))
	require.NoError(t, err)
	require.Equal(t, remotedelivery.DeliveryComplete, result.Status)
	state, err = o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, capture.Through, state.Baseline.Cursor)
	request.Baseline = state.Baseline
	message := fixtureMessage()
	message.Text = "new delta after migration"
	require.NoError(t, st.UpsertMessage(ctx, message, nil))
	capture, err = store.CaptureNativeChanges(ctx, source, request)
	require.NoError(t, err)
	_, err = o.Stage(ctx, capture, remotedelivery.StageLimits{MaxEncodedBytes: 8 << 20})
	require.NoError(t, err)
	result, err = o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(64))
	require.NoError(t, err)
	require.Equal(t, remotedelivery.DeliveryComplete, result.Status)
	assertOutboxV2(t, oldPath)
}

func assertOutboxV2(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	var version, tables, indexes int
	require.NoError(t, db.QueryRow(`pragma user_version`).Scan(&version))
	require.Equal(t, 2, version)
	require.NoError(t, db.QueryRow(`select count(*) from sqlite_schema where type='table' and name not like 'sqlite_%'`).Scan(&tables))
	require.Equal(t, 3, tables)
	require.NoError(t, db.QueryRow(`select count(*) from sqlite_schema where type='index' and name in ('one_pending_capture','one_seed_capture')`).Scan(&indexes))
	require.Equal(t, 2, indexes)
	rows, err := db.Query(`pragma foreign_key_check`)
	require.NoError(t, err)
	defer rows.Close()
	require.False(t, rows.Next())
	require.NoError(t, rows.Err())
}

func TestOutboxV1ConcurrentMigrationAndReopen(t *testing.T) {
	o, current, _, ctx := driverFixture(t, "http://127.0.0.1:8123", 501)
	require.NoError(t, o.Close())
	path := historicalOutbox(t, current)
	before := outboxData(t, path)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			o, err := remotedelivery.OpenOutbox(ctx, path)
			if err == nil {
				_, err = o.NextPart(ctx)
				closeErr := o.Close()
				if err == nil {
					err = closeErr
				}
			}
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.Equal(t, before, outboxData(t, path))
	assertOutboxV2(t, path)
	o = openDelivery(t, ctx, path)
	_, err := o.NextPart(ctx)
	require.NoError(t, err)
}

func TestOutboxV1FailedMigrationRollsBack(t *testing.T) {
	o, current, _, ctx := driverFixture(t, "http://127.0.0.1:8123", 1)
	require.NoError(t, o.Close())
	path := historicalOutbox(t, current)
	// A historical orphan makes the real FK-constrained copy fail after DDL and
	// earlier inserts. No migration failure hook or alternate transaction path.
	deliverySQL(t, path, `insert into parts values('missing-capture',0,x'00ff','retained',null)`)
	before := outboxData(t, path)
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	var schemaBefore string
	require.NoError(t, db.QueryRow(`select group_concat(sql,';') from (select sql from sqlite_schema where sql is not null order by name)`).Scan(&schemaBefore))
	o, err = remotedelivery.OpenOutbox(ctx, path)
	require.Error(t, err)
	require.Nil(t, o)
	require.Equal(t, before, outboxData(t, path))
	var schemaAfter string
	var version int
	require.NoError(t, db.QueryRow(`select group_concat(sql,';') from (select sql from sqlite_schema where sql is not null order by name)`).Scan(&schemaAfter))
	require.Equal(t, schemaBefore, schemaAfter)
	require.NoError(t, db.QueryRow(`pragma user_version`).Scan(&version))
	require.Equal(t, 1, version)
	// Explicitly repair only this synthetic corruption, then the supported open
	// can retry. The original pending body and NULL receipt remain intact.
	_, err = db.Exec(`delete from parts where capture_id='missing-capture'`)
	require.NoError(t, err)
	o = openDelivery(t, ctx, path)
	part, err := o.NextPart(ctx)
	require.NoError(t, err)
	require.NotNil(t, part)
	assertOutboxV2(t, path)
}

func TestOutboxV2RejectsUnqualifiedAndSeedPendingState(t *testing.T) {
	for _, mode := range []string{"unqualified", "seed"} {
		t.Run(mode, func(t *testing.T) {
			o, path, _, ctx := driverFixture(t, "http://127.0.0.1:8123", 501)
			if mode == "unqualified" {
				deliverySQL(t, path, `update binding set baseline_state='unqualified',initial_native=null,saved_native=null,initial_server_cursor='',server_cursor='',seed_scope_json=x'7b7d'`)
			} else {
				deliverySQL(t, path, `update captures set kind='seed',native_from=null where completed=0`)
			}
			before := outboxData(t, path)
			_, err := o.NextPart(ctx)
			require.Error(t, err, "reserved seed state must not reach the unchanged delta sender")
			require.NoError(t, o.Close())
			o, err = remotedelivery.OpenOutbox(ctx, path)
			require.Error(t, err)
			require.Nil(t, o)
			require.Equal(t, before, outboxData(t, path))
		})
	}
}

func TestOpenCurrentOutboxRefusesV1BeforeWritableOpen(t *testing.T) {
	o, current, _, ctx := driverFixture(t, "http://127.0.0.1:8123", 1)
	require.NoError(t, o.Close())
	path := historicalOutbox(t, current)
	require.NoError(t, os.Chmod(path, 0o640))
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	_, err = remotedelivery.OpenCurrentOutbox(ctx, path)
	require.ErrorContains(t, err, "explicit upgrade")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	require.NoFileExists(t, path+"-wal")
	require.NoFileExists(t, path+"-shm")
	// The existing migration API remains supported and current-only reopen works.
	o, err = remotedelivery.OpenOutbox(ctx, path)
	require.NoError(t, err)
	require.NoError(t, o.Close())
	o, err = remotedelivery.OpenCurrentOutbox(ctx, path)
	require.NoError(t, err)
	require.NoError(t, o.Close())
}
