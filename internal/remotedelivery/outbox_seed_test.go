package remotedelivery_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/remotedelivery"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A protocol peer, not a replacement for receiver qualification. Actual Outbox,
// native capture/projector and Crawlkit HTTP code own the producer path.
type seedPeer struct {
	mu           sync.Mutex
	plan         *remote.SlackSeedPlan
	cursor       string
	receipts     map[string]remote.ChangeReceipt
	bodies       map[string][]byte
	observations []remote.Change
	complete     bool
	lose         string
}

func newSeedPeer(t *testing.T) (*seedPeer, *httptest.Server) {
	t.Helper()
	p := &seedPeer{receipts: map[string]remote.ChangeReceipt{}, bodies: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer fixture-publisher" {
			http.Error(w, "route/auth", 400)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, remote.MaxChangeBytes))
		if err != nil {
			http.Error(w, "body", 400)
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		var identity struct {
			App, Archive string
			BindingID    string `json:"binding_id"`
		}
		if json.Unmarshal(body, &identity) != nil || identity.App != "slacrawl" || identity.Archive != "fixture" || identity.BindingID != "fixture-binding" {
			http.Error(w, "identity", 400)
			return
		}
		var id, expected string
		var observations []remote.Change
		completes := false
		route := strings.TrimPrefix(r.URL.Path, "/v1/apps/slacrawl/archives/fixture/")
		switch {
		case route == "seeds":
			plan, e := remote.DecodeSlackSeedManifest(body)
			err = e
			if err == nil {
				p.plan = plan
				id = "seed1:" + plan.Manifest().SeedID + ":begin"
			}
		case strings.Contains(route, "/chunks/") && p.plan != nil:
			var chunk remote.SlackSeedChunk
			chunk, err = p.plan.ValidateChunk(body)
			if err == nil {
				id = "seed1:" + chunk.SeedID + ":" + strconv.Itoa(chunk.Index)
				expected = chunk.ExpectedCursor
				observations = chunk.Changes
				if route != "seeds/"+chunk.SeedID+"/chunks/"+strconv.Itoa(chunk.Index) {
					err = fmt.Errorf("route")
				}
			}
		case strings.HasSuffix(route, "/complete") && p.plan != nil:
			var complete remote.SlackSeedComplete
			complete, err = p.plan.ValidateComplete(body)
			if err == nil {
				id = "seed1:" + complete.SeedID + ":complete"
				expected = complete.ExpectedCursor
				completes = true
			}
		case route == "changes" && p.complete:
			var batch remote.ChangeBatch
			batch, err = remote.DecodeChangeBatch(body)
			if err == nil && batch.NativeFrom != p.plan.Manifest().NativeThrough {
				err = fmt.Errorf("native anchor")
			}
			if err == nil {
				id = batch.BatchID()
				expected = batch.ExpectedCursor
				observations = batch.Changes
			}
		default:
			err = fmt.Errorf("route")
		}
		if err != nil {
			http.Error(w, "contract", 400)
			return
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(body))
		receipt, replay := p.receipts[id]
		if replay {
			if receipt.PayloadSHA256 != digest {
				http.Error(w, "changed bytes", 409)
				return
			}
			receipt.Replayed = true
		} else {
			if expected != p.cursor {
				http.Error(w, "cursor", 409)
				return
			}
			receipt = remote.ChangeReceipt{App: identity.App, Archive: identity.Archive, BindingID: identity.BindingID, BatchID: id, PayloadSHA256: digest, Sequence: strconv.Itoa(len(p.receipts) + 1), Cursor: id}
			p.receipts[id] = receipt
			p.bodies[id] = append([]byte(nil), body...)
			p.cursor = id
			p.observations = append(p.observations, observations...)
			p.complete = p.complete || completes
		}
		if p.lose != "" && strings.HasSuffix(id, p.lose) {
			p.lose = ""
			connection, _, e := w.(http.Hijacker).Hijack()
			if e == nil {
				_ = connection.Close()
			}
			return
		}
		_ = json.NewEncoder(w).Encode(receipt)
	}))
	t.Cleanup(server.Close)
	return p, server
}
func seedRequest(r store.NativeCaptureRequest) store.NativeSeedRequest {
	return store.NativeSeedRequest{File: r.Baseline.File, Scope: r.Scope, ScopeSHA256: r.ScopeSHA256, Limits: store.NativeSeedLimits{MaxRows: 10000, MaxBytes: 16 << 20}}
}
func seedRegistration(origin string, r store.NativeSeedRequest) remotedelivery.SeedRegistration {
	return remotedelivery.SeedRegistration{BindingID: "fixture-binding", Origin: origin, App: "slacrawl", Archive: "fixture", Profile: remotedelivery.PrivateArchiveProfile, File: r.File, Scope: r.Scope}
}
func seedOutbox(t *testing.T, ctx context.Context, origin string, r store.NativeSeedRequest) (*remotedelivery.Outbox, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seed.db")
	o, err := remotedelivery.InitSeedOutbox(ctx, path, seedRegistration(origin, r))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, o.Close()) })
	return o, path
}

func TestSeedOutboxH0EmptyTablesAndNextDelta(t *testing.T) {
	peer, server := newSeedPeer(t)
	st, source, request, ctx := projectionFixture(t)
	request.Scope.IncludeWorkspaceUsers = false
	request.ScopeSHA256, _ = store.NativeCaptureScopeSHA256(request.Scope)
	// Construct a pre-journal archive by removing only v9 journal objects from
	// this synthetic catalog. Native Open performs the real v8 -> v9 upgrade.
	require.NoError(t, st.Close())
	db, err := sql.Open("sqlite", source)
	require.NoError(t, err)
	triggers, err := db.Query(`select name from sqlite_schema where type='trigger' and name glob 'native_changes_*'`)
	require.NoError(t, err)
	var names []string
	for triggers.Next() {
		var name string
		require.NoError(t, triggers.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, triggers.Err())
	require.NoError(t, triggers.Close())
	for _, name := range names {
		_, err = db.Exec(`drop trigger "` + strings.ReplaceAll(name, `"`, `""`) + `"`)
		require.NoError(t, err)
	}
	_, err = db.Exec(`drop table native_changes; drop table native_change_state; pragma user_version=8`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	st, err = store.Open(source)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	sr := seedRequest(request)
	seed, err := store.CaptureNativeSeed(ctx, source, sr)
	require.NoError(t, err)
	require.Zero(t, seed.Through.Sequence)
	before, err := st.Status(ctx)
	require.NoError(t, err)
	o, path := seedOutbox(t, ctx, server.URL, sr)
	state, err := o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, remotedelivery.BaselineUnqualified, state.BaselineState)
	_, err = o.Stage(ctx, store.NativeCapture{}, remotedelivery.StageLimits{MaxEncodedBytes: 1 << 20})
	require.ErrorIs(t, err, remotedelivery.ErrOutboxBaseline)
	_, err = o.StageSeed(ctx, seed, remotedelivery.StageLimits{MaxEncodedBytes: 1 << 20})
	require.NoError(t, err)
	part, err := o.NextPart(ctx)
	require.NoError(t, err)
	plan, err := remote.DecodeSlackSeedManifest(part.Body)
	require.NoError(t, err)
	for _, table := range plan.Manifest().Tables[2:] {
		require.Equal(t, "0", table.Rows)
		require.Empty(t, table.Chunks)
	}
	result, err := o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(1))
	require.NoError(t, err)
	require.Equal(t, remotedelivery.DeliveryPartLimit, result.Status)
	state, err = o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, remotedelivery.BaselineUnqualified, state.BaselineState)
	require.NoError(t, o.Close())
	o = openDelivery(t, ctx, path)
	result, err = o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(64))
	require.NoError(t, err)
	require.Equal(t, remotedelivery.DeliveryComplete, result.Status)
	state, err = o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, remotedelivery.BaselineQualified, state.BaselineState)
	require.Equal(t, seed.Through, state.Baseline.Cursor)
	require.Equal(t, state.Baseline, state.Registration.Baseline)
	require.Equal(t, state.ServerCursor, state.Registration.ServerCursor)
	after, err := st.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, before.NativeChanges, after.NativeChanges, "seed does not advance native journal")
	request.Baseline = state.Baseline
	require.NoError(t, st.UpsertMessage(ctx, fixtureMessage(), nil))
	capture, err := store.CaptureNativeChanges(ctx, source, request)
	require.NoError(t, err)
	require.Equal(t, seed.Through, capture.From)
	_, err = o.Stage(ctx, capture, remotedelivery.StageLimits{MaxEncodedBytes: 1 << 20})
	require.NoError(t, err)
	_, err = o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(64))
	require.NoError(t, err)
	peer.mu.Lock()
	receipt := peer.receipts["seed1:"+part.Ref.CaptureID+":begin"]
	observations := append([]remote.Change(nil), peer.observations...)
	peer.mu.Unlock()
	receipt.Replayed = true
	require.NoError(t, o.RecordReceipt(ctx, part.Ref, receipt), "seed receipt replay after later delta")
	require.Equal(t, "snowman ☃\x00tail", *observations[len(observations)-1].Row[8].Value)
}

func TestSeedOutboxProcessExit(t *testing.T) {
	mode := os.Getenv("SLACRAWL_TEST_SEED_CHILD")
	if mode == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	o, err := remotedelivery.OpenOutbox(ctx, os.Getenv("SLACRAWL_TEST_OUTBOX_PATH"))
	require.NoError(t, err)
	if mode == "stage" {
		var request store.NativeSeedRequest
		require.NoError(t, json.Unmarshal([]byte(os.Getenv("SLACRAWL_TEST_SEED_REQUEST")), &request))
		seed, err := store.CaptureNativeSeed(ctx, os.Getenv("SLACRAWL_TEST_SOURCE_PATH"), request)
		require.NoError(t, err)
		_, err = o.StageSeed(ctx, seed, remotedelivery.StageLimits{MaxEncodedBytes: 8 << 20})
		require.NoError(t, err)
	} else {
		_, err = o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(2))
		require.NoError(t, err)
	}
	os.Exit(0) // Actual durable WAL state, without Close.
}

func TestSeedOutboxCrashReplayAndFinalRollback(t *testing.T) {
	peer, server := newSeedPeer(t)
	st, source, request, _ := projectionFixture(t)
	// Durable inserts, subprocess exits, and replay need more than the small fixture budget under -race.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	writeCaptureMessages(t, st, ctx, 501)
	sr := seedRequest(request)
	o, path := seedOutbox(t, ctx, server.URL, sr)
	require.NoError(t, o.Close())
	encoded, _ := json.Marshal(sr)
	child := func(mode string) {
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSeedOutboxProcessExit$")
		command.Env = append(os.Environ(), "SLACRAWL_TEST_SEED_CHILD="+mode, "SLACRAWL_TEST_OUTBOX_PATH="+path, "SLACRAWL_TEST_SOURCE_PATH="+source, "SLACRAWL_TEST_SEED_REQUEST="+string(encoded))
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	child("stage")
	child("deliver")
	o = openDelivery(t, ctx, path)
	state, err := o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, remotedelivery.BaselineUnqualified, state.BaselineState)
	part, err := o.NextPart(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, part.Ref.Index)
	message := fixtureMessage()
	message.Text = "changed after staging"
	require.NoError(t, st.UpsertMessage(ctx, message, nil))
	peer.mu.Lock()
	peer.lose = ":1"
	peer.mu.Unlock()
	_, err = o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(64))
	require.ErrorIs(t, err, remotedelivery.ErrDeliverySend)
	replayed, err := o.NextPart(ctx)
	require.NoError(t, err)
	require.Equal(t, part, replayed)
	deliverySQL(t, path, `create trigger fail_seed_qualification before update of baseline_state on binding begin select raise(abort,'fixture qualification failure'); end`)
	_, err = o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(64))
	require.ErrorIs(t, err, remotedelivery.ErrDeliveryState)
	state, err = o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, remotedelivery.BaselineUnqualified, state.BaselineState)
	final, err := o.NextPart(ctx)
	require.NoError(t, err)
	require.Equal(t, final.PartCount-1, final.Ref.Index)
	peer.mu.Lock()
	committed := peer.receipts["seed1:"+final.Ref.CaptureID+":complete"]
	peer.mu.Unlock()
	require.NotEmpty(t, committed.BatchID, "remote committed; local qualification rolled back")
	require.NoError(t, o.Close())
	o = openDelivery(t, ctx, path)
	deliverySQL(t, path, `drop trigger fail_seed_qualification`)
	_, err = o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(64))
	require.NoError(t, err)
	state, err = o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, remotedelivery.BaselineQualified, state.BaselineState)
	committed.Replayed = true
	require.NoError(t, o.RecordReceipt(ctx, final.Ref, committed))
	peer.mu.Lock()
	defer peer.mu.Unlock()
	messages := 0
	for _, change := range peer.observations {
		if change.Table == "messages" {
			messages++
			require.NotEqual(t, "changed after staging", *change.Row[8].Value)
		}
	}
	require.Equal(t, 501, messages)
	require.Len(t, peer.plan.Manifest().Tables[3].Chunks, 2)
}

func finalSeedPart(t *testing.T, path string) *remotedelivery.PendingPart {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	p := &remotedelivery.PendingPart{Kind: "seed"}
	require.NoError(t, db.QueryRow(`select p.capture_id,p.part_index,p.body,p.payload_sha256,c.part_count from parts p join captures c using(capture_id) where c.completed=0 and p.part_index=c.part_count-1`).Scan(&p.Ref.CaptureID, &p.Ref.Index, &p.Body, &p.SHA256, &p.PartCount))
	return p
}
func finalSeedReceipt(p *remotedelivery.PendingPart) remote.ChangeReceipt {
	id := "seed1:" + p.Ref.CaptureID + ":complete"
	return remote.ChangeReceipt{App: "slacrawl", Archive: "fixture", BindingID: "fixture-binding", BatchID: id, PayloadSHA256: p.SHA256, Sequence: "9007199254740993", Cursor: id}
}

func TestSeedOutboxAmbiguousKeysRemainEvidenceOnly(t *testing.T) {
	for _, kind := range []string{"null", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			peer, server := newSeedPeer(t)
			st, source, request, ctx := projectionFixture(t)
			// Native SQLite permits multiple NULL text-primary-key components.
			_, err := st.DB().Exec(`insert into users(id,workspace_id,name,raw_json,updated_at) values(null,'T1','first','{}','now'),(null,'T1','second','{}','now')`)
			require.NoError(t, err)
			sr := seedRequest(request)
			seed, err := store.CaptureNativeSeed(ctx, source, sr)
			require.NoError(t, err)
			if kind == "duplicate" {
				// A materialized duplicate non-NULL key must also remain unqualified.
				for i, column := range seed.Tables[2].Columns {
					if column == "id" {
						for j := range seed.Tables[2].Rows {
							if seed.Tables[2].Rows[j][i].Kind == store.NativeNull {
								seed.Tables[2].Rows[j][i] = store.NativeValue{Kind: store.NativeText, Text: "U2"}
							}
						}
					}
				}
			}
			o, path := seedOutbox(t, ctx, server.URL, sr)
			_, err = o.StageSeed(ctx, seed, remotedelivery.StageLimits{MaxEncodedBytes: 1 << 20})
			require.NoError(t, err)
			state, err := o.State(ctx)
			require.NoError(t, err)
			require.Equal(t, "ambiguous_keys", state.BaselineHold)
			final := finalSeedPart(t, path)
			require.ErrorIs(t, o.RecordReceipt(ctx, final.Ref, finalSeedReceipt(final)), remotedelivery.ErrSeedBaselineHeld)
			_, err = o.DeliverPending(ctx, deliveryOptions(), deliveryLimits(64))
			require.ErrorIs(t, err, remotedelivery.ErrSeedBaselineHeld)
			require.NoError(t, o.Close())
			o = openDelivery(t, ctx, path)
			_, err = o.NextPart(ctx)
			require.ErrorIs(t, err, remotedelivery.ErrSeedBaselineHeld)
			require.ErrorIs(t, o.RecordReceipt(ctx, final.Ref, finalSeedReceipt(final)), remotedelivery.ErrSeedBaselineHeld)
			state, err = o.State(ctx)
			require.NoError(t, err)
			require.Equal(t, remotedelivery.BaselineUnqualified, state.BaselineState)
			peer.mu.Lock()
			defer peer.mu.Unlock()
			require.False(t, peer.complete)
			mentions := 0
			for _, c := range peer.observations {
				if c.Table == "users" {
					mentions++
				}
			}
			require.Equal(t, 3, mentions, "hold must not collapse or discard evidence")
		})
	}
}

func TestSeedOutboxAdmissionBoundsAndAtomicStage(t *testing.T) {
	_, source, request, ctx := projectionFixture(t)
	sr := seedRequest(request)
	seed, err := store.CaptureNativeSeed(ctx, source, sr)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "absent", "seed.db")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = remotedelivery.InitSeedOutbox(canceled, path, seedRegistration("http://127.0.0.1:8123", sr))
	require.ErrorIs(t, err, context.Canceled)
	invalid := seedRegistration("http://127.0.0.1:8123", sr)
	invalid.BindingID = strings.Repeat("x", 129)
	_, err = remotedelivery.InitSeedOutbox(ctx, path, invalid)
	require.Error(t, err)
	_, err = os.Stat(filepath.Dir(path))
	require.ErrorIs(t, err, os.ErrNotExist)
	o, path := seedOutbox(t, ctx, "http://127.0.0.1:8123", sr)
	for _, kind := range []string{"bytes", "file", "scope", "anchor", "schema", "single-row"} {
		t.Run(kind, func(t *testing.T) {
			bad := seed
			limits := remotedelivery.StageLimits{MaxEncodedBytes: 8 << 20}
			switch kind {
			case "bytes":
				limits.MaxEncodedBytes = 1
			case "file":
				bad.File.FileID = "different"
			case "scope":
				bad.ScopeSHA256 = strings.Repeat("0", 64)
			case "anchor":
				bad.Through.EntryID = "invalid"
			case "schema":
				bad.Tables = bad.Tables[:5]
			case "single-row":
				bad.Tables = append([]store.NativeSeedTable(nil), seed.Tables...)
				row := append([]store.NativeValue(nil), seed.Tables[0].Rows[0]...)
				for i, c := range seed.Tables[0].Columns {
					if c == "name" {
						row[i] = store.NativeValue{Kind: store.NativeText, Text: strings.Repeat("\x00", remote.MaxChangeBytes/5)}
					}
				}
				bad.Tables[0].Rows = [][]store.NativeValue{row}
			}
			_, err := o.StageSeed(ctx, bad, limits)
			require.Error(t, err)
			state, err := o.State(ctx)
			require.NoError(t, err)
			require.Empty(t, state.PendingCaptureID)
		})
	}
	deliverySQL(t, path, `create trigger fail_seed_part before insert on parts when new.part_index=1 begin select raise(abort,'fixture stage failure'); end`)
	_, err = o.StageSeed(ctx, seed, remotedelivery.StageLimits{MaxEncodedBytes: 1 << 20})
	require.Error(t, err)
	state, err := o.State(ctx)
	require.NoError(t, err)
	require.Empty(t, state.PendingCaptureID)
	deliverySQL(t, path, `drop trigger fail_seed_part`)
	second := openDelivery(t, ctx, path)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, handle := range []*remotedelivery.Outbox{o, second} {
		go func(handle *remotedelivery.Outbox) {
			<-start
			_, err := handle.StageSeed(ctx, seed, remotedelivery.StageLimits{MaxEncodedBytes: 1 << 20})
			results <- err
		}(handle)
	}
	close(start)
	success, pending := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else {
			require.ErrorIs(t, err, remotedelivery.ErrPendingCapture)
			pending++
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, pending)
	final := finalSeedPart(t, path)
	require.Error(t, o.RecordReceipt(ctx, final.Ref, finalSeedReceipt(final)), "completion cannot skip ordered receipts")
	state, err = o.State(ctx)
	require.NoError(t, err)
	require.Equal(t, remotedelivery.BaselineUnqualified, state.BaselineState)
}
