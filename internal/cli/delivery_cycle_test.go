package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/remotedelivery"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/stretchr/testify/require"
)

func deliveryCycleArgs(db, scope, outbox string, parts int) []string {
	args := deliveryChangesArgs(db, scope, outbox)
	args[1] = "cycle"
	for i := range args {
		if args[i] == "--max-events" {
			args[i+1] = "2000"
		}
	}
	return append(args, "--max-parts", strconv.Itoa(parts))
}

func TestDeliveryCycleResumesBeforeCapture(t *testing.T) {
	db, scope, outbox, app, output := deliveryCLIFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	readState := func() remotedelivery.OutboxState {
		t.Helper()
		o, err := remotedelivery.OpenCurrentOutbox(ctx, outbox)
		require.NoError(t, err)
		defer o.Close()
		state, err := o.State(ctx)
		require.NoError(t, err)
		return state
	}
	var mu sync.Mutex
	var calls atomic.Int32
	sequence := 0
	failReply := false
	var cancelRequest context.CancelFunc
	seen := map[string]remote.ChangeReceipt{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls.Add(1)
		o, err := remotedelivery.OpenCurrentOutbox(ctx, outbox)
		require.NoError(t, err)
		part, err := o.NextPart(ctx)
		require.NoError(t, err)
		require.NoError(t, o.Close())
		require.NotNil(t, part)
		body, err := io.ReadAll(io.LimitReader(r.Body, remote.MaxChangeBytes+1))
		require.NoError(t, err)
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "Bearer cycle-token-canary", r.Header.Get("Authorization"))
		require.Equal(t, part.Body, body)
		id := (remote.ChangeBatch{CaptureID: part.Ref.CaptureID, PartIndex: part.Ref.Index}).BatchID()
		if part.Kind == "seed" {
			suffix := strconv.Itoa(part.Ref.Index - 1)
			if part.Ref.Index == 0 {
				suffix = "begin"
			} else if part.Ref.Index == part.PartCount-1 {
				suffix = "complete"
			}
			id = "seed1:" + part.Ref.CaptureID + ":" + suffix
		} else {
			batch, err := remote.DecodeChangeBatch(body)
			require.NoError(t, err)
			require.Equal(t, id, batch.BatchID())
			require.Equal(t, "/v1/apps/slacrawl/archives/fixture/changes", r.URL.Path)
		}
		receipt, replayed := seen[id]
		if !replayed {
			sequence++
			receipt = remote.ChangeReceipt{App: "slacrawl", Archive: "fixture", BindingID: "fixture-binding", BatchID: id, Cursor: id, Sequence: strconv.Itoa(sequence), PayloadSHA256: fmt.Sprintf("%x", sha256.Sum256(body))}
			seen[id] = receipt
		}
		if cancelRequest != nil {
			cancelRequest()
			cancelRequest = nil
		}
		if failReply {
			failReply = false
			http.Error(w, "private-response-canary", 503)
			return
		}
		receipt.Replayed = replayed
		require.NoError(t, json.NewEncoder(w).Encode(receipt))
	}))
	defer server.Close()
	app.httpClient = server.Client()
	t.Setenv(remote.DefaultTokenEnv, "cycle-token-canary")
	require.NoError(t, app.Run(ctx, deliveryInitArgs(db, scope, outbox, server.URL)))
	require.NoError(t, app.Run(ctx, deliveryStageArgs(db, scope, outbox)))
	runCycle := func(callCtx context.Context, parts int) ([]deliveryOutput, error) {
		t.Helper()
		output.Reset()
		err := app.Run(callCtx, deliveryCycleArgs(db, scope, outbox, parts))
		require.NotContains(t, output.String(), "canary")
		if err != nil {
			require.NotContains(t, err.Error(), "canary")
		}
		var results []deliveryOutput
		decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
		for {
			var result deliveryOutput
			decodeErr := decoder.Decode(&result)
			if decodeErr == io.EOF {
				break
			}
			require.NoError(t, decodeErr)
			phases := []string{"resume", "capture", "deliver"}
			require.Less(t, len(results), len(phases))
			require.Equal(t, phases[len(results)], result.Phase)
			results = append(results, result)
		}
		return results, err
	}
	results, err := runCycle(ctx, 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "part_limit", results[0].Status)
	results, err = runCycle(ctx, 64)
	require.NoError(t, err)
	require.Len(t, results, 3)
	require.Equal(t, "complete", results[0].Status)
	require.Equal(t, "no_native_progress", results[1].Status)
	require.Equal(t, "idle", results[2].Status)
	qualified := readState()
	beforeCalls := calls.Load()
	results, err = runCycle(ctx, 64)
	require.NoError(t, err)
	require.Len(t, results, 3)
	require.Equal(t, qualified, readState())
	require.Equal(t, beforeCalls, calls.Load())
	scopeBytes, err := os.ReadFile(scope)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(scope, bytes.ReplaceAll(scopeBytes, []byte(`"T1"`), []byte(`"T2"`)), 0o600))
	results, err = runCycle(ctx, 64)
	require.ErrorIs(t, err, remotedelivery.ErrOutboxBaseline)
	require.Len(t, results, 2)
	require.Equal(t, "failed", results[1].Status)
	require.Equal(t, qualified, readState())
	require.Equal(t, beforeCalls, calls.Load())
	require.NoError(t, os.WriteFile(scope, scopeBytes, 0o600))
	mutate := func(count int, text string) {
		t.Helper()
		st, err := store.Open(db)
		require.NoError(t, err)
		defer st.Close()
		for i := range count {
			require.NoError(t, st.UpsertMessage(ctx, store.Message{ChannelID: "C1", TS: fmt.Sprintf("1710000000.%06d", i+100), WorkspaceID: "T1", UserID: "U1", Text: text, SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: time.Unix(1710000100, 0).UTC()}, nil))
		}
	}
	stageChanges := func() {
		t.Helper()
		args := deliveryChangesArgs(db, scope, outbox)
		for i := range args {
			if args[i] == "--max-events" {
				args[i+1] = "2000"
			}
		}
		require.NoError(t, app.Run(ctx, args))
	}
	mutate(501, "private-change-canary")
	stageChanges()
	pending := readState()
	mutate(1, "newer-private-change-canary")
	results, err = runCycle(ctx, 1)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, pending.PendingCaptureID, results[0].CaptureID)
	require.Equal(t, "part_limit", results[0].Status)
	require.Equal(t, qualified.Baseline, readState().Baseline)
	mu.Lock()
	failReply = true
	mu.Unlock()
	results, err = runCycle(ctx, 64)
	require.ErrorIs(t, err, remotedelivery.ErrDeliverySend)
	require.Len(t, results, 1)
	require.Equal(t, pending.PendingCaptureID, results[0].CaptureID)
	require.Equal(t, qualified.Baseline, readState().Baseline)
	results, err = runCycle(ctx, 64)
	require.NoError(t, err)
	require.Len(t, results, 3)
	require.Equal(t, pending.PendingCaptureID, results[0].CaptureID)
	require.Equal(t, "complete", results[0].Status)
	require.EqualValues(t, 1, results[1].Rows)
	require.Equal(t, "complete", results[2].Status)
	require.NotEqual(t, pending.PendingCaptureID, results[2].CaptureID)
	st, err := store.Open(db)
	require.NoError(t, err)
	require.NoError(t, st.UpsertUser(ctx, store.User{ID: "U1", WorkspaceID: "T1", Name: "excluded-canary", RawJSON: "{}", UpdatedAt: time.Unix(1710000200, 0).UTC()}))
	require.NoError(t, st.Close())
	results, err = runCycle(ctx, 64)
	require.NoError(t, err)
	require.Len(t, results, 3)
	require.Equal(t, "staged", results[1].Status)
	require.Zero(t, results[1].Rows)
	require.Equal(t, "complete", results[2].Status)
	mutate(1, "cancel-canary")
	stageChanges()
	pending = readState()
	mutate(1, "after-cancel-canary")
	callCtx, stop := context.WithCancel(ctx)
	defer stop()
	mu.Lock()
	cancelRequest = stop
	mu.Unlock()
	results, err = runCycle(callCtx, 64)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, results, 1)
	require.Equal(t, pending.PendingCaptureID, results[0].CaptureID)
	require.Equal(t, pending.Baseline, readState().Baseline)
	require.Equal(t, "untouched-config-canary", app.configPath)
}
