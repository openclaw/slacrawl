package remotedelivery

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/slacrawl/internal/store"
	"github.com/stretchr/testify/require"
)

func TestOutboxCancellationReleasesWriteTransaction(t *testing.T) {
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	registration := Registration{BindingID: "binding", Origin: "http://127.0.0.1:8123", App: "slacrawl", Archive: "fixture", Profile: PrivateArchiveProfile,
		Baseline: store.NativeCaptureBaseline{File: store.NativeFileIdentity{Platform: "fixture", VolumeID: "1", FileID: "2"}, ScopeSHA256: strings.Repeat("a", 64), Cursor: store.NativeCursor{GenerationID: strings.Repeat("b", 32), EntryID: strings.Repeat("c", 32)}}}
	path := filepath.Join(t.TempDir(), "not-created", "delivery.db")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := InitOutbox(canceled, path, registration)
	require.ErrorIs(t, err, context.Canceled)
	_, err = os.Stat(filepath.Dir(path))
	require.ErrorIs(t, err, os.ErrNotExist)
	o, err := InitOutbox(ctx, path, registration)
	require.NoError(t, err)
	defer o.Close()
	initial, err := o.State(ctx)
	require.NoError(t, err)
	writeCtx, cancelWrite := context.WithCancel(ctx)
	err = o.write(writeCtx, func(conn *sql.Conn) error {
		var synchronous int
		require.NoError(t, conn.QueryRowContext(writeCtx, `pragma synchronous`).Scan(&synchronous))
		require.Equal(t, 2, synchronous, "the actual write connection uses FULL")
		if _, err := conn.ExecContext(writeCtx, `update binding set server_cursor='uncommitted'`); err != nil {
			return err
		}
		cancelWrite()
		// Exercise the real driver cancellation path after BEGIN and a write.
		_, err := conn.ExecContext(writeCtx, `update binding set server_cursor='cancelled'`)
		return err
	})
	require.ErrorIs(t, err, context.Canceled)
	state, err := o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, initial, state)
	require.NoError(t, o.write(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `update binding set server_cursor='committed'`)
		return err
	}))
	state, err = o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, "committed", state.ServerCursor)
}
