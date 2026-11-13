package remotedelivery_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/remotedelivery"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/stretchr/testify/require"
)

func outboxFixture(t *testing.T) (*store.Store, string, store.NativeCaptureRequest, context.Context, string, *remotedelivery.Outbox) {
	t.Helper()
	st, source, request, _ := projectionFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	path := filepath.Join(t.TempDir(), "delivery.db")
	registration := remotedelivery.Registration{BindingID: "fixture-binding", Origin: "http://127.0.0.1:8123", App: "slacrawl", Archive: "fixture", Profile: remotedelivery.PrivateArchiveProfile, Baseline: request.Baseline, ServerCursor: "seed:fixture"}
	o, err := remotedelivery.InitOutbox(ctx, path, registration)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, o.Close()) })
	return st, source, request, ctx, path, o
}

func writeCaptureMessages(t *testing.T, st *store.Store, ctx context.Context, count int) {
	t.Helper()
	for i := range count {
		message := fixtureMessage()
		message.TS = fmt.Sprintf("1710000000.%06d", i+1)
		require.NoError(t, st.UpsertMessage(ctx, message, nil))
	}
}

func readBatch(t *testing.T, part *remotedelivery.PendingPart) remote.ChangeBatch {
	t.Helper()
	require.NotNil(t, part)
	var batch remote.ChangeBatch
	require.NoError(t, json.Unmarshal(part.Body, &batch))
	require.Equal(t, fmt.Sprintf("%x", sha256.Sum256(part.Body)), part.SHA256)
	return batch
}

func partReceipt(t *testing.T, part *remotedelivery.PendingPart) remote.ChangeReceipt {
	t.Helper()
	batch := readBatch(t, part)
	return remote.ChangeReceipt{App: batch.App, Archive: batch.Archive, BindingID: batch.BindingID, BatchID: batch.BatchID(), PayloadSHA256: part.SHA256, Sequence: strconv.Itoa(101 + part.Ref.Index), Cursor: batch.BatchID()}
}

func openDelivery(t *testing.T, ctx context.Context, path string) *remotedelivery.Outbox {
	t.Helper()
	o, err := remotedelivery.OpenOutbox(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, o.Close()) })
	return o
}

func deliverySQL(t *testing.T, path string, statement string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(statement, args...)
	require.NoError(t, err)
}

func TestOutboxProcessExit(t *testing.T) {
	mode := os.Getenv("SLACRAWL_TEST_OUTBOX_CHILD")
	if mode == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	o, err := remotedelivery.OpenOutbox(ctx, os.Getenv("SLACRAWL_TEST_OUTBOX_PATH"))
	require.NoError(t, err)
	if mode == "stage" {
		var request store.NativeCaptureRequest
		require.NoError(t, json.Unmarshal([]byte(os.Getenv("SLACRAWL_TEST_CAPTURE_REQUEST")), &request))
		capture, err := store.CaptureNativeChanges(ctx, os.Getenv("SLACRAWL_TEST_SOURCE_PATH"), request)
		require.NoError(t, err)
		_, err = o.Stage(ctx, capture, remotedelivery.StageLimits{MaxEncodedBytes: 8 << 20})
		require.NoError(t, err)
	} else {
		part, err := o.NextPart(ctx)
		require.NoError(t, err)
		require.NoError(t, o.RecordReceipt(ctx, part.Ref, partReceipt(t, part)))
	}
	// Deliberately bypass Close and deferred cleanup, leaving durable WAL state.
	os.Exit(0)
}

func TestOutboxResumesExactPartsAcrossProcessExit(t *testing.T) {
	st, source, request, ctx, path, o := outboxFixture(t)
	writeCaptureMessages(t, st, ctx, 501)
	capture, err := store.CaptureNativeChanges(ctx, source, request)
	require.NoError(t, err)
	require.NoError(t, o.Close())
	encodedRequest, err := json.Marshal(request)
	require.NoError(t, err)
	runChild := func(mode string) {
		t.Helper()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOutboxProcessExit$")
		command.Env = append(os.Environ(), "SLACRAWL_TEST_OUTBOX_CHILD="+mode, "SLACRAWL_TEST_OUTBOX_PATH="+path, "SLACRAWL_TEST_SOURCE_PATH="+source, "SLACRAWL_TEST_CAPTURE_REQUEST="+string(encodedRequest))
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	runChild("stage")
	o = openDelivery(t, ctx, path)
	part0, err := o.NextPart(ctx)
	require.NoError(t, err)
	batch0 := readBatch(t, part0)
	require.Equal(t, 2, batch0.PartCount)
	require.Len(t, batch0.Changes, 500)
	require.Equal(t, "seed:fixture", batch0.ExpectedCursor)
	require.Equal(t, fmt.Sprintf("slacrawl-native-v1:%s:%d:%s", capture.From.GenerationID, capture.From.Sequence, capture.From.EntryID), batch0.NativeFrom)
	require.Equal(t, fmt.Sprintf("slacrawl-native-v1:%s:%d:%s", capture.Through.GenerationID, capture.Through.Sequence, capture.Through.EntryID), batch0.NativeThrough)
	message := fixtureMessage()
	message.Text = "source changed after staging"
	require.NoError(t, st.UpsertMessage(ctx, message, nil))
	_, err = o.Stage(ctx, capture, remotedelivery.StageLimits{MaxEncodedBytes: 8 << 20})
	require.ErrorIs(t, err, remotedelivery.ErrPendingCapture)
	require.NoError(t, o.Close())
	o = openDelivery(t, ctx, path)
	replayed, err := o.NextPart(ctx)
	require.NoError(t, err)
	require.Equal(t, part0, replayed)
	require.NoError(t, o.Close())
	runChild("receipt")
	o = openDelivery(t, ctx, path)
	state, err := o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, request.Baseline, state.Baseline)
	require.Equal(t, "seed:fixture", state.ServerCursor)
	part1, err := o.NextPart(ctx)
	require.NoError(t, err)
	batch1 := readBatch(t, part1)
	require.Equal(t, 1, part1.Ref.Index)
	require.Len(t, batch1.Changes, 1)
	require.Equal(t, batch0.BatchID(), batch1.ExpectedCursor)
	require.Equal(t, batch0.NativeFrom, batch1.NativeFrom)
	require.Equal(t, batch0.NativeThrough, batch1.NativeThrough)
	require.NoError(t, o.Close())
	runChild("receipt")
	o = openDelivery(t, ctx, path)
	state, err = o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, capture.Through, state.Baseline.Cursor)
	require.Equal(t, batch1.BatchID(), state.ServerCursor)
	require.Empty(t, state.PendingCaptureID)
	part, err := o.NextPart(ctx)
	require.NoError(t, err)
	require.Nil(t, part)
	request.Baseline = state.Baseline
	next, err := store.CaptureNativeChanges(ctx, source, request)
	require.NoError(t, err)
	newID, err := o.Stage(ctx, next, remotedelivery.StageLimits{MaxEncodedBytes: 8 << 20})
	require.NoError(t, err)
	before, err := o.State(ctx)
	require.NoError(t, err)
	receipt := partReceipt(t, part1)
	receipt.Replayed = true
	require.NoError(t, o.RecordReceipt(ctx, part1.Ref, receipt))
	after, err := o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, newID, after.PendingCaptureID)
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	var completedReceipts int
	require.NoError(t, db.QueryRow(`select count(*) from parts where receipt_json is not null`).Scan(&completedReceipts))
	require.Equal(t, 2, completedReceipts)
	var savedReceipt []byte
	require.NoError(t, db.QueryRow(`select receipt_json from parts where capture_id=? and part_index=?`, part1.Ref.CaptureID, part1.Ref.Index).Scan(&savedReceipt))
	var firstReceipt remote.ChangeReceipt
	require.NoError(t, json.Unmarshal(savedReceipt, &firstReceipt))
	require.False(t, firstReceipt.Replayed, "retain the first receipt after later replay responses")
}

func TestOutboxRollsBackIncompleteStageAndFinalReceipt(t *testing.T) {
	st, source, request, ctx, path, o := outboxFixture(t)
	writeCaptureMessages(t, st, ctx, 501)
	capture, err := store.CaptureNativeChanges(ctx, source, request)
	require.NoError(t, err)
	initial, err := o.State(ctx)
	require.NoError(t, err)
	deliverySQL(t, path, `create trigger fail_part before insert on parts when new.part_index=1 begin select raise(abort,'fixture disk failure'); end`)
	_, err = o.Stage(ctx, capture, remotedelivery.StageLimits{MaxEncodedBytes: 8 << 20})
	require.Error(t, err)
	state, err := o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, initial, state)
	deliverySQL(t, path, `drop trigger fail_part`)
	_, err = o.Stage(ctx, capture, remotedelivery.StageLimits{MaxEncodedBytes: 8 << 20})
	require.NoError(t, err)
	first, err := o.NextPart(ctx)
	require.NoError(t, err)
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	future := &remotedelivery.PendingPart{Ref: remotedelivery.PartRef{CaptureID: first.Ref.CaptureID, Index: 1}}
	require.NoError(t, db.QueryRow(`select body,payload_sha256 from parts where capture_id=? and part_index=1`, first.Ref.CaptureID).Scan(&future.Body, &future.SHA256))
	require.Error(t, o.RecordReceipt(ctx, future.Ref, partReceipt(t, future)), "a matching later receipt cannot skip its predecessor")
	require.NoError(t, o.RecordReceipt(ctx, first.Ref, partReceipt(t, first)))
	last, err := o.NextPart(ctx)
	require.NoError(t, err)
	deliverySQL(t, path, `create trigger fail_completion before update of saved_native on binding begin select raise(abort,'fixture completion failure'); end`)
	require.Error(t, o.RecordReceipt(ctx, last.Ref, partReceipt(t, last)))
	state, err = o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, initial.Baseline, state.Baseline)
	require.Equal(t, initial.ServerCursor, state.ServerCursor)
	stillPending, err := o.NextPart(ctx)
	require.NoError(t, err)
	require.Equal(t, last, stillPending)
	deliverySQL(t, path, `drop trigger fail_completion`)
	require.NoError(t, o.RecordReceipt(ctx, last.Ref, partReceipt(t, last)))
	state, err = o.State(ctx)
	require.NoError(t, err)
	require.Empty(t, state.PendingCaptureID)
	require.Equal(t, capture.Through, state.Baseline.Cursor)
}

func TestOutboxConcurrentStageAndReceiptReplay(t *testing.T) {
	st, source, request, ctx, path, first := outboxFixture(t)
	writeCaptureMessages(t, st, ctx, 1)
	capture, err := store.CaptureNativeChanges(ctx, source, request)
	require.NoError(t, err)
	second := openDelivery(t, ctx, path)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, o := range []*remotedelivery.Outbox{first, second} {
		go func() {
			<-start
			_, err := o.Stage(ctx, capture, remotedelivery.StageLimits{MaxEncodedBytes: 1 << 20})
			results <- err
		}()
	}
	close(start)
	a, b := <-results, <-results
	if a == nil {
		require.ErrorIs(t, b, remotedelivery.ErrPendingCapture)
	} else {
		require.ErrorIs(t, a, remotedelivery.ErrPendingCapture)
		require.NoError(t, b)
	}
	one, err := first.NextPart(ctx)
	require.NoError(t, err)
	two, err := second.NextPart(ctx)
	require.NoError(t, err)
	require.Equal(t, one, two)
	receipt := partReceipt(t, one)
	for _, o := range []*remotedelivery.Outbox{first, second} {
		go func() { results <- o.RecordReceipt(ctx, one.Ref, receipt) }()
	}
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	state, err := first.State(ctx)
	require.NoError(t, err)
	require.Empty(t, state.PendingCaptureID)
	require.Equal(t, capture.Through, state.Baseline.Cursor)
}

func TestOutboxRejectsBaselineAndReceiptMismatch(t *testing.T) {
	st, source, request, ctx, _, o := outboxFixture(t)
	writeCaptureMessages(t, st, ctx, 1)
	capture, err := store.CaptureNativeChanges(ctx, source, request)
	require.NoError(t, err)
	initial, err := o.State(ctx)
	require.NoError(t, err)
	for _, mode := range []string{"file", "scope", "from", "generation", "backwards", "anchor"} {
		t.Run(mode, func(t *testing.T) {
			bad := capture
			switch mode {
			case "file":
				bad.File.FileID += "changed"
			case "scope":
				bad.ScopeSHA256 = strings.Repeat("a", 64)
			case "from":
				bad.From.Sequence--
			case "generation":
				bad.Through.GenerationID = strings.Repeat("a", 32)
			case "backwards":
				bad.Through.Sequence = bad.From.Sequence - 1
			case "anchor":
				bad.Through.EntryID = "invalid"
			}
			_, err := o.Stage(ctx, bad, remotedelivery.StageLimits{MaxEncodedBytes: 1 << 20})
			require.ErrorIs(t, err, remotedelivery.ErrOutboxBaseline)
		})
	}
	for _, limit := range []int64{0, 1, (256 << 20) + 1} {
		_, err := o.Stage(ctx, capture, remotedelivery.StageLimits{MaxEncodedBytes: limit})
		require.ErrorIs(t, err, remotedelivery.ErrOutboxLimit)
	}
	state, err := o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, initial, state)
	_, err = o.Stage(ctx, capture, remotedelivery.StageLimits{MaxEncodedBytes: 1 << 20})
	require.NoError(t, err)
	part, err := o.NextPart(ctx)
	require.NoError(t, err)
	good := partReceipt(t, part)
	for _, mode := range []string{"app", "archive", "binding", "batch", "hash", "cursor", "zero", "negative", "leading-zero", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			bad := good
			switch mode {
			case "app":
				bad.App = "other"
			case "archive":
				bad.Archive = "other"
			case "binding":
				bad.BindingID = "other"
			case "batch":
				bad.BatchID = "other"
			case "hash":
				bad.PayloadSHA256 = strings.Repeat("0", 64)
			case "cursor":
				bad.Cursor = "other"
			case "zero":
				bad.Sequence = "0"
			case "negative":
				bad.Sequence = "-1"
			case "leading-zero":
				bad.Sequence = "0101"
			case "overflow":
				bad.Sequence = "9223372036854775808"
			}
			require.Error(t, o.RecordReceipt(ctx, part.Ref, bad))
		})
	}
	unchanged, err := o.NextPart(ctx)
	require.NoError(t, err)
	require.Equal(t, part, unchanged)
	require.NoError(t, o.RecordReceipt(ctx, part.Ref, good))
	good.Sequence = "102"
	require.Error(t, o.RecordReceipt(ctx, part.Ref, good))
}

func TestOutboxEmptyProgressAndSafeOpen(t *testing.T) {
	st, source, request, ctx, path, o := outboxFixture(t)
	capture, err := store.CaptureNativeChanges(ctx, source, request)
	require.NoError(t, err)
	_, err = o.Stage(ctx, capture, remotedelivery.StageLimits{MaxEncodedBytes: 1 << 20})
	require.ErrorIs(t, err, remotedelivery.ErrNoNativeProgress)
	require.NoError(t, st.SetSyncState(ctx, "provider:fixture", "workspace", "T1", "private acquisition checkpoint"))
	capture, err = store.CaptureNativeChanges(ctx, source, request)
	require.NoError(t, err)
	_, err = o.Stage(ctx, capture, remotedelivery.StageLimits{MaxEncodedBytes: 1 << 20})
	require.NoError(t, err)
	part, err := o.NextPart(ctx)
	require.NoError(t, err)
	batch := readBatch(t, part)
	require.NotNil(t, batch.Changes)
	require.Empty(t, batch.Changes)
	require.Equal(t, 1, batch.PartCount)
	require.NotContains(t, string(part.Body), "private acquisition checkpoint")
	require.NoError(t, o.RecordReceipt(ctx, part.Ref, partReceipt(t, part)))
	state, err := o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, capture.Through, state.Baseline.Cursor)
	_, err = remotedelivery.InitOutbox(ctx, path, state.Registration)
	require.Error(t, err, "registration never replaces an existing outbox")
	require.NoError(t, st.Close())
	require.NoError(t, os.Chmod(source, 0o640))
	before, err := os.ReadFile(source)
	require.NoError(t, err)
	_, err = remotedelivery.OpenOutbox(ctx, source)
	require.Error(t, err)
	after, err := os.ReadFile(source)
	require.NoError(t, err)
	require.Equal(t, before, after)
	info, err := os.Stat(source)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	for _, invalid := range []string{":memory:", "file:" + path, "relative.db", filepath.Join(t.TempDir(), "absent.db")} {
		_, err := remotedelivery.OpenOutbox(ctx, invalid)
		require.Error(t, err)
	}
	link := filepath.Join(t.TempDir(), "linked.db")
	require.NoError(t, os.Symlink(path, link))
	_, err = remotedelivery.OpenOutbox(ctx, link)
	require.Error(t, err)
}
