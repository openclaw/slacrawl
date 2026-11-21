package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFreshSchemaFailureRollsBack(t *testing.T) {
	for _, tc := range []struct {
		table   string
		wantErr string
	}{
		{"message_files", "workspace_id"},
		{"idx_sync_state_updated", "idx_sync_state_updated"},
	} {
		t.Run(tc.table, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "archive.db")
			db, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			_, err = db.Exec("create table " + tc.table + " (sentinel text); insert into " + tc.table + " values ('retained');")
			require.NoError(t, err)

			_, err = Open(path)
			require.ErrorContains(t, err, tc.wantErr)
			var objects int
			require.NoError(t, db.QueryRow(`select count(*) from sqlite_schema
where name not like 'sqlite_%' and name <> ?`, tc.table).Scan(&objects))
			require.Zero(t, objects, "failed initialization must not leave partial schema objects")
			var sentinel string
			require.NoError(t, db.QueryRow("select sentinel from "+tc.table).Scan(&sentinel))
			require.Equal(t, "retained", sentinel)
			var version int
			require.NoError(t, db.QueryRow(`pragma user_version`).Scan(&version))
			require.Zero(t, version)

			_, err = db.Exec("drop table " + tc.table)
			require.NoError(t, err)
			require.NoError(t, db.Close())
			st, err := Open(path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, st.Close()) })
			require.NoError(t, st.DB().QueryRow(`pragma user_version`).Scan(&version))
			require.Equal(t, schemaVersion, version)
			var foreignKeys, busyTimeout int
			var journalMode string
			require.NoError(t, st.DB().QueryRow(`pragma foreign_keys`).Scan(&foreignKeys))
			require.Equal(t, 1, foreignKeys)
			require.NoError(t, st.DB().QueryRow(`pragma busy_timeout`).Scan(&busyTimeout))
			require.Equal(t, 5000, busyTimeout)
			require.NoError(t, st.DB().QueryRow(`pragma journal_mode`).Scan(&journalMode))
			require.Equal(t, "wal", journalMode)
			_, err = st.Status(context.Background())
			require.NoError(t, err)
		})
	}
}

func BenchmarkStoreStartup(b *testing.B) {
	b.Run("fresh", func(b *testing.B) {
		dir := b.TempDir()
		var iteration int
		b.ReportAllocs()
		for b.Loop() {
			path := filepath.Join(dir, fmt.Sprintf("archive-%d.db", iteration))
			iteration++
			st, err := Open(path)
			if err != nil {
				b.Fatal(err)
			}
			if err := st.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("reopen", func(b *testing.B) {
		path := filepath.Join(b.TempDir(), "archive.db")
		st, err := Open(path)
		if err != nil {
			b.Fatal(err)
		}
		if err := st.Close(); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for b.Loop() {
			st, err := Open(path)
			if err != nil {
				b.Fatal(err)
			}
			if err := st.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
