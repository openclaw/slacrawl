package remotedelivery_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/remotedelivery"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/stretchr/testify/require"
)

// This HTTP peer models only the public receipt/replay contract; it has no
// crawl-remote dependency. Real Outbox and Crawlkit client code own the flow.
type deliveryPeer struct {
	mu       sync.Mutex
	requests [][]byte
	receipts map[string]remote.ChangeReceipt
	cursor   string
	before   func(http.ResponseWriter, *http.Request, remote.ChangeBatch) bool
	reply    func(http.ResponseWriter, *http.Request, *remotedelivery.PendingPart, remote.ChangeReceipt)
}

func newDeliveryPeer(t *testing.T) (*deliveryPeer, *httptest.Server) {
	t.Helper()
	p := &deliveryPeer{receipts: map[string]remote.ChangeReceipt{}, cursor: "seed:fixture"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/apps/slacrawl/archives/fixture/changes" || r.Header.Get("Authorization") != "Bearer fixture-publisher" {
			http.Error(w, "bad fixture route/auth", 400)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, remote.MaxChangeBytes))
		if err != nil {
			http.Error(w, "bad fixture bytes", 400)
			return
		}
		var batch remote.ChangeBatch
		if json.Unmarshal(body, &batch) != nil {
			http.Error(w, "bad fixture batch", 400)
			return
		}
		p.mu.Lock()
		p.requests = append(p.requests, body)
		p.mu.Unlock()
		if p.before != nil && p.before(w, r, batch) {
			return
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(body))
		p.mu.Lock()
		receipt, replay := p.receipts[batch.BatchID()]
		if replay {
			if receipt.PayloadSHA256 != digest {
				p.mu.Unlock()
				http.Error(w, "changed bytes", 409)
				return
			}
			receipt.Replayed = true
		} else {
			if batch.ExpectedCursor != p.cursor {
				p.mu.Unlock()
				http.Error(w, "cursor conflict", 409)
				return
			}
			receipt = remote.ChangeReceipt{App: batch.App, Archive: batch.Archive, BindingID: batch.BindingID, BatchID: batch.BatchID(), PayloadSHA256: digest, Sequence: strconv.Itoa(len(p.receipts) + 1), Cursor: batch.BatchID()}
			p.receipts[receipt.BatchID] = receipt
			p.cursor = receipt.Cursor
		}
		p.mu.Unlock()
		part := &remotedelivery.PendingPart{Ref: remotedelivery.PartRef{CaptureID: batch.CaptureID, Index: batch.PartIndex}, Body: body, SHA256: digest}
		if p.reply != nil {
			p.reply(w, r, part, receipt)
			return
		}
		_ = json.NewEncoder(w).Encode(receipt)
	}))
	t.Cleanup(server.Close)
	return p, server
}

func (p *deliveryPeer) snapshot() ([][]byte, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]byte(nil), p.requests...), len(p.receipts)
}

func driverFixture(t *testing.T, origin string, count int) (*remotedelivery.Outbox, string, store.NativeCapture, context.Context) {
	t.Helper()
	st, source, request, _ := projectionFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	path := filepath.Join(t.TempDir(), "delivery.db")
	o, err := remotedelivery.InitOutbox(ctx, path, remotedelivery.Registration{BindingID: "fixture-binding", Origin: origin, App: "slacrawl", Archive: "fixture", Profile: remotedelivery.PrivateArchiveProfile, Baseline: request.Baseline, ServerCursor: "seed:fixture"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, o.Close()) })
	if count == 0 {
		return o, path, store.NativeCapture{}, ctx
	}
	request.Limits.MaxEvents = count + 10
	request.Limits.MaxBytes = 8 << 20
	writeCaptureMessages(t, st, ctx, count)
	capture, err := store.CaptureNativeChanges(ctx, source, request)
	require.NoError(t, err)
	_, err = o.Stage(ctx, capture, remotedelivery.StageLimits{MaxEncodedBytes: 8 << 20})
	require.NoError(t, err)
	return o, path, capture, ctx
}

func deliveryOptions() remotedelivery.DeliveryOptions {
	return remotedelivery.DeliveryOptions{TokenProvider: remote.StaticToken("fixture-publisher")}
}
func deliveryLimits(parts int) remotedelivery.DeliveryLimits {
	return remotedelivery.DeliveryLimits{MaxParts: parts, MaxDuration: 5 * time.Second}
}

func TestDeliverPendingPartBudgetAndReopen(t *testing.T) {
	p, server := newDeliveryPeer(t)
	o, path, capture, ctx := driverFixture(t, server.URL, 501)
	initial, err := o.State(ctx)
	require.NoError(t, err)
	first, err := o.NextPart(ctx)
	require.NoError(t, err)
	result, err := o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(1))
	require.NoError(t, err)
	require.Equal(t, remotedelivery.DeliveryPartLimit, result.Status)
	require.Equal(t, 1, result.ReceiptsRecorded)
	require.Equal(t, first.Ref.CaptureID, result.CaptureID)
	state, err := o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, initial.Baseline, state.Baseline)
	require.Equal(t, initial.ServerCursor, state.ServerCursor)
	second, err := o.NextPart(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, second.Ref.Index)
	require.NoError(t, o.Close())
	o = openDelivery(t, ctx, path)
	result, err = o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(1))
	require.NoError(t, err)
	require.Equal(t, remotedelivery.DeliveryComplete, result.Status)
	require.Equal(t, 1, result.ReceiptsRecorded)
	state, err = o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, capture.Through, state.Baseline.Cursor)
	require.Empty(t, state.PendingCaptureID)
	requests, commits := p.snapshot()
	require.Equal(t, 2, commits)
	require.Equal(t, [][]byte{first.Body, second.Body}, requests)
	result, err = o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(1))
	require.NoError(t, err)
	require.Equal(t, remotedelivery.DeliveryIdle, result.Status)
	require.Empty(t, result.CaptureID)
	require.Zero(t, result.ReceiptsRecorded)
	requests, _ = p.snapshot()
	require.Len(t, requests, 2)
}

func TestDeliverPendingLostReplyResendsExactBytes(t *testing.T) {
	p, server := newDeliveryPeer(t)
	var once sync.Once
	p.reply = func(w http.ResponseWriter, r *http.Request, part *remotedelivery.PendingPart, receipt remote.ChangeReceipt) {
		drop := false
		once.Do(func() { drop = true })
		if drop {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		_ = json.NewEncoder(w).Encode(receipt)
	}
	o, path, capture, ctx := driverFixture(t, server.URL, 1)
	part, err := o.NextPart(ctx)
	require.NoError(t, err)
	result, err := o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(64))
	require.ErrorIs(t, err, remotedelivery.ErrDeliverySend)
	require.Equal(t, remotedelivery.DeliveryPending, result.Status)
	require.Zero(t, result.ReceiptsRecorded)
	requests, commits := p.snapshot()
	require.Len(t, requests, 1)
	require.Equal(t, 1, commits, "remote commit survived lost reply")
	still, err := o.NextPart(ctx)
	require.NoError(t, err)
	require.Equal(t, part, still)
	require.NoError(t, o.Close())
	o = openDelivery(t, ctx, path)
	result, err = o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(64))
	require.NoError(t, err)
	require.Equal(t, remotedelivery.DeliveryComplete, result.Status)
	require.Equal(t, 1, result.ReceiptsRecorded)
	requests, commits = p.snapshot()
	require.Equal(t, [][]byte{part.Body, part.Body}, requests)
	require.Equal(t, 1, commits)
	state, err := o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, capture.Through, state.Baseline.Cursor)
}

func TestDeliverPendingDoesNotFollowConcurrentSuccessor(t *testing.T) {
	p, server := newDeliveryPeer(t)
	st, source, request, ctx := projectionFixture(t)
	path := filepath.Join(t.TempDir(), "delivery.db")
	o, err := remotedelivery.InitOutbox(ctx, path, remotedelivery.Registration{
		BindingID: "fixture-binding", Origin: server.URL, App: "slacrawl", Archive: "fixture",
		Profile: remotedelivery.PrivateArchiveProfile, Baseline: request.Baseline, ServerCursor: "seed:fixture",
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, o.Close()) })
	writeCaptureMessages(t, st, ctx, 1)
	first, err := store.CaptureNativeChanges(ctx, source, request)
	require.NoError(t, err)
	id, err := o.Stage(ctx, first, remotedelivery.StageLimits{MaxEncodedBytes: 1 << 20})
	require.NoError(t, err)
	// Capture a real subsequent native write while the first delivery is pending.
	request.Baseline.Cursor = first.Through
	message := fixtureMessage()
	message.Text = "later native observation"
	require.NoError(t, st.UpsertMessage(ctx, message, nil))
	later, err := store.CaptureNativeChanges(ctx, source, request)
	require.NoError(t, err)
	other := openDelivery(t, ctx, path)
	type staged struct {
		id  string
		err error
	}
	completed := make(chan staged, 1)
	p.reply = func(w http.ResponseWriter, r *http.Request, part *remotedelivery.PendingPart, receipt remote.ChangeReceipt) {
		// A second dispatcher settles the same remote receipt and a producer
		// stages the next capture before our HTTP request has returned.
		err := other.RecordReceipt(ctx, part.Ref, receipt)
		var next string
		if err == nil {
			next, err = other.Stage(ctx, later, remotedelivery.StageLimits{MaxEncodedBytes: 1 << 20})
		}
		completed <- staged{next, err}
		_ = json.NewEncoder(w).Encode(receipt)
	}
	result, err := o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(64))
	require.NoError(t, err)
	settled := <-completed
	require.NoError(t, settled.err)
	require.Equal(t, id, result.CaptureID)
	require.Equal(t, remotedelivery.DeliveryComplete, result.Status)
	require.Equal(t, 1, result.ReceiptsRecorded, "accepted receipt is not a unique remote/local effect count")
	state, err := o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, settled.id, state.PendingCaptureID)
	require.Equal(t, first.Through, state.Baseline.Cursor)
	requests, commits := p.snapshot()
	require.Len(t, requests, 1)
	require.Equal(t, 1, commits)
}

func TestDeliverPendingPartialFailureAndReceiptFailure(t *testing.T) {
	for _, mode := range []string{"http", "receipt", "local-commit"} {
		t.Run(mode, func(t *testing.T) {
			p, server := newDeliveryPeer(t)
			o, path, _, ctx := driverFixture(t, server.URL, 501)
			initial, err := o.State(ctx)
			require.NoError(t, err)
			switch mode {
			case "http":
				p.before = func(w http.ResponseWriter, r *http.Request, b remote.ChangeBatch) bool {
					if b.PartIndex == 1 {
						http.Error(w, `{"message":"SECRET private body or credential"}`, 503)
						return true
					}
					return false
				}
			case "receipt":
				p.reply = func(w http.ResponseWriter, r *http.Request, part *remotedelivery.PendingPart, receipt remote.ChangeReceipt) {
					if part.Ref.Index == 1 {
						receipt.BindingID = "SECRET"
					}
					_ = json.NewEncoder(w).Encode(receipt)
				}
			case "local-commit":
				deliverySQL(t, path, `create trigger fail_completion before update of saved_native on binding begin select raise(abort,'SECRET local payload'); end`)
			}
			result, err := o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(64))
			if mode == "local-commit" {
				require.ErrorIs(t, err, remotedelivery.ErrDeliveryState)
			} else {
				require.ErrorIs(t, err, remotedelivery.ErrDeliverySend)
			}
			require.NotContains(t, err.Error(), "SECRET")
			require.NotContains(t, err.Error(), server.URL)
			require.Equal(t, 1, result.ReceiptsRecorded)
			require.Equal(t, remotedelivery.DeliveryPending, result.Status)
			requests, _ := p.snapshot()
			require.Len(t, requests, 2, "failure must not retry")
			state, err := o.State(ctx)
			require.NoError(t, err)
			require.Equal(t, initial.Baseline, state.Baseline)
			require.Equal(t, initial.ServerCursor, state.ServerCursor)
			part, err := o.NextPart(ctx)
			require.NoError(t, err)
			require.Equal(t, 1, part.Ref.Index)
			// Reopen first; only an explicit second driver call may recover the part.
			require.NoError(t, o.Close())
			o = openDelivery(t, ctx, path)
			p.before = nil
			p.reply = nil
			if mode == "local-commit" {
				deliverySQL(t, path, `drop trigger fail_completion`)
			}
			result, err = o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(64))
			require.NoError(t, err)
			require.Equal(t, remotedelivery.DeliveryComplete, result.Status)
			require.Equal(t, 1, result.ReceiptsRecorded)
			requests, commits := p.snapshot()
			require.Len(t, requests, 3)
			require.Equal(t, 2, commits)
			require.Equal(t, requests[1], requests[2])
		})
	}
}

func TestDeliverPendingBoundsCancellationAndOrigin(t *testing.T) {
	t.Run("invalid budgets do not send", func(t *testing.T) {
		p, server := newDeliveryPeer(t)
		o, _, _, ctx := driverFixture(t, server.URL, 1)
		for _, limits := range []remotedelivery.DeliveryLimits{{}, {MaxParts: -1, MaxDuration: time.Second}, {MaxParts: 65, MaxDuration: time.Second}, {MaxParts: 1}} {
			_, err := o.DeliverPending(ctx, deliveryOptions(), limits)
			require.ErrorIs(t, err, remotedelivery.ErrDeliveryOptions)
		}
		_, err := o.DeliverPending(ctx, remotedelivery.DeliveryOptions{}, deliveryLimits(1))
		require.ErrorIs(t, err, remotedelivery.ErrDeliveryOptions)
		requests, _ := p.snapshot()
		require.Empty(t, requests)
	})
	for _, parent := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline parent=%v", parent), func(t *testing.T) {
			p, server := newDeliveryPeer(t)
			seen := make(chan struct{})
			p.before = func(w http.ResponseWriter, r *http.Request, b remote.ChangeBatch) bool {
				close(seen)
				<-r.Context().Done()
				return true
			}
			o, _, _, ctx := driverFixture(t, server.URL, 1)
			limits := deliveryLimits(1)
			if parent {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
				defer cancel()
			} else {
				limits.MaxDuration = 200 * time.Millisecond
			}
			result, err := o.DeliverPending(ctx, deliveryOptions(), limits)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.ErrorIs(t, err, remotedelivery.ErrDeliverySend)
			require.Zero(t, result.ReceiptsRecorded)
			select {
			case <-seen:
			default:
				t.Fatal("request did not exercise HTTP deadline")
			}
			requests, commits := p.snapshot()
			require.Len(t, requests, 1)
			require.Zero(t, commits)
		})
	}
	t.Run("cross origin redirect refused", func(t *testing.T) {
		var mu sync.Mutex
		calls := 0
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mu.Lock(); calls++; mu.Unlock() }))
		defer other.Close()
		p, server := newDeliveryPeer(t)
		p.before = func(w http.ResponseWriter, r *http.Request, b remote.ChangeBatch) bool {
			http.Redirect(w, r, other.URL+"/SECRET", http.StatusTemporaryRedirect)
			return true
		}
		o, _, _, ctx := driverFixture(t, server.URL, 1)
		options := deliveryOptions()
		options.HTTPClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
		_, err := o.DeliverPending(ctx, options, deliveryLimits(1))
		require.ErrorIs(t, err, remotedelivery.ErrDeliverySend)
		require.NotContains(t, err.Error(), "SECRET")
		mu.Lock()
		defer mu.Unlock()
		require.Zero(t, calls)
	})
	t.Run("credential error is sanitized but context identity survives", func(t *testing.T) {
		p, server := newDeliveryPeer(t)
		o, _, _, ctx := driverFixture(t, server.URL, 1)
		options := deliveryOptions()
		options.TokenProvider = deliveryTokenError{}
		_, err := o.DeliverPending(ctx, options, deliveryLimits(1))
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorIs(t, err, remotedelivery.ErrDeliverySend)
		require.NotContains(t, err.Error(), "SECRET")
		requests, _ := p.snapshot()
		require.Empty(t, requests)
	})
}

type deliveryTokenError struct{}

func (deliveryTokenError) Token(context.Context) (string, error) {
	return "", errors.Join(errors.New("SECRET token provider"), context.Canceled)
}
