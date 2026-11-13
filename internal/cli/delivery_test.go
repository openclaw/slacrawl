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
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/openclaw/crawlkit/remote"
	crawlstore "github.com/openclaw/crawlkit/store"
	"github.com/openclaw/slacrawl/internal/remotedelivery"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/stretchr/testify/require"
)

func deliveryCLIFixture(t *testing.T) (string, string, string, *App, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	db, scope, outbox := filepath.Join(dir, "native.db"), filepath.Join(dir, "scope.json"), filepath.Join(dir, "outbox.db")
	seedArchiveStore(t, db, "private-message-canary")
	st, err := store.Open(db)
	require.NoError(t, err)
	raw := `{"id":"C1","is_channel":true,"is_group":false,"is_im":false,"is_mpim":false,"is_private":false,"is_shared":false}`
	_, err = st.DB().Exec(`update channels set raw_json=?`, raw)
	require.NoError(t, err)
	require.NoError(t, st.Close())
	digest, err := store.NativeChannelAdmissionSHA256(store.NativeChannelFacts{ChannelID: "C1", WorkspaceID: "T1", Kind: "public_channel", RawJSON: raw})
	require.NoError(t, err)
	body, err := json.Marshal(deliveryScopeDocument{Version: 1, WorkspaceID: "T1", Channels: []deliveryChannelAdmission{{"C1", digest}}, SyncStateKeys: []deliverySyncStateKey{}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(scope, body, 0o600))
	badConfig := filepath.Join(dir, "bad-default.toml")
	require.NoError(t, os.WriteFile(badConfig, []byte("invalid [ config-canary"), 0o600))
	t.Setenv("SLACRAWL_CONFIG", badConfig)
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME"} {
		t.Setenv(name, dir)
	}
	t.Setenv("SLACRAWL_NO_UPDATE_CHECK", "")
	t.Setenv("PATH", dir) // Accidental share/provider child processes cannot execute.
	var output bytes.Buffer
	app := &App{Stdout: &output, Stderr: &output, configPath: "untouched-config-canary"}
	return db, scope, outbox, app, &output
}
func deliveryInitArgs(db, scope, outbox, origin string) []string {
	return []string{"--json", "delivery", "init", "--db", db, "--scope", scope, "--outbox", outbox, "--origin", origin, "--archive", "fixture", "--binding", "fixture-binding"}
}
func deliveryStageArgs(db, scope, outbox string) []string {
	return []string{"delivery", "stage-seed", "--json", "--db", db, "--scope", scope, "--outbox", outbox, "--max-rows", "100", "--max-native-bytes", "1048576", "--max-encoded-bytes", "1048576", "--timeout", "10s"}
}
func deliveryArgs(outbox string, parts int) []string {
	return []string{"delivery", "deliver", "--json", "--outbox", outbox, "--max-parts", strconv.Itoa(parts), "--timeout", "10s"}
}

func TestDeliveryCLIStagesExactBytesAndResumesWithoutSource(t *testing.T) {
	db, scope, outbox, app, output := deliveryCLIFixture(t)
	before, err := os.ReadFile(db)
	require.NoError(t, err)
	var calls atomic.Int32
	var bodies [][]byte
	var seedID string
	fail := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(calls.Add(1)) - 1
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "Bearer service-token-canary", r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		// The first response fails. The next invocation must send identical part 0.
		if fail {
			fail = false
			require.Equal(t, bodies[0], body)
			http.Error(w, "private-response-canary", 503)
			return
		}
		index--
		require.Less(t, index, len(bodies))
		require.Equal(t, bodies[index], body)
		suffix := strconv.Itoa(index - 1)
		path := "/v1/apps/slacrawl/archives/fixture/seeds/" + seedID + "/chunks/" + suffix
		if index == 0 {
			suffix = "begin"
			path = "/v1/apps/slacrawl/archives/fixture/seeds"
		}
		if index == len(bodies)-1 {
			suffix = "complete"
			path = "/v1/apps/slacrawl/archives/fixture/seeds/" + seedID + "/complete"
		}
		require.Equal(t, path, r.URL.Path)
		id := "seed1:" + seedID + ":" + suffix
		require.NoError(t, json.NewEncoder(w).Encode(remote.ChangeReceipt{App: "slacrawl", Archive: "fixture", BindingID: "fixture-binding", BatchID: id, Cursor: id, Sequence: strconv.Itoa(index + 1), PayloadSHA256: fmt.Sprintf("%x", sha256.Sum256(body))}))
	}))
	defer server.Close()
	app.httpClient = server.Client()
	require.NoError(t, app.Run(context.Background(), deliveryInitArgs(db, scope, outbox, server.URL)))
	require.Contains(t, output.String(), `"baseline_state": "unqualified"`)
	output.Reset()
	require.NoError(t, app.Run(context.Background(), deliveryStageArgs(db, scope, outbox)))
	var staged deliveryOutput
	require.NoError(t, json.Unmarshal(output.Bytes(), &staged))
	require.Equal(t, "staged", staged.Status)
	require.Equal(t, "unqualified", staged.BaselineState)
	require.NotNil(t, staged.NativeAnchor)
	require.EqualValues(t, 0, calls.Load())
	require.Equal(t, "untouched-config-canary", app.configPath)
	after, err := os.ReadFile(db)
	require.NoError(t, err)
	require.Equal(t, before, after)
	// Inspect the durable fixture through the read-only owner, independently of
	// the sender. HTTP must receive these exact bytes, not a newly captured seed.
	ro, err := crawlstore.OpenReadOnly(context.Background(), outbox)
	require.NoError(t, err)
	rows, err := ro.DB().Query(`select body from parts order by part_index`)
	require.NoError(t, err)
	for rows.Next() {
		var body []byte
		require.NoError(t, rows.Scan(&body))
		bodies = append(bodies, body)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.NoError(t, ro.Close())
	plan, err := remote.DecodeSlackSeedManifest(bodies[0])
	require.NoError(t, err)
	seedID = plan.Manifest().SeedID
	require.Equal(t, staged.CaptureID, seedID)
	require.Equal(t, "slacrawl-native-v1:"+staged.NativeAnchor.GenerationID+":"+staged.NativeAnchor.Sequence+":"+staged.NativeAnchor.EntryID, plan.Manifest().NativeThrough)
	require.NoError(t, os.Remove(db))
	require.NoError(t, os.Remove(scope)) // Only isolated fixture inputs.
	t.Setenv(remote.DefaultTokenEnv, "")
	t.Setenv("SLACK_BOT_TOKEN", "provider-token-canary")
	t.Setenv("SLACK_USER_TOKEN", "provider-token-canary")
	t.Setenv("CODEX_APPS_ACCESS_TOKEN", "provider-token-canary")
	output.Reset()
	err = app.Run(context.Background(), deliveryArgs(outbox, 1))
	require.ErrorIs(t, err, remotedelivery.ErrDeliverySend)
	require.Zero(t, calls.Load())
	t.Setenv(remote.DefaultTokenEnv, "service-token-canary")
	err = app.Run(context.Background(), deliveryArgs(outbox, 1))
	require.ErrorIs(t, err, remotedelivery.ErrDeliverySend)
	require.NotContains(t, err.Error(), "canary")
	require.NoError(t, app.Run(context.Background(), deliveryArgs(outbox, 1)))
	require.Contains(t, output.String(), `"status": "part_limit"`)
	require.NotContains(t, output.String(), "canary")
	output.Reset()
	require.NoError(t, app.Run(context.Background(), deliveryArgs(outbox, 64)))
	require.Contains(t, output.String(), `"status": "complete"`)
	require.Contains(t, output.String(), `"baseline_state": "qualified"`)
	require.NotContains(t, output.String(), "canary")
	require.EqualValues(t, len(bodies)+1, calls.Load())
	output.Reset()
	require.NoError(t, app.Run(context.Background(), deliveryArgs(outbox, 1)))
	require.Contains(t, output.String(), `"status": "idle"`)
}

func TestDeliveryCLIAdmissionFailuresDoNotCreateOrStage(t *testing.T) {
	db, scope, outbox, app, output := deliveryCLIFixture(t)
	ctx := context.Background()
	original, err := os.ReadFile(db)
	require.NoError(t, err)
	for _, config := range []string{"bad-config-canary", ""} {
		err = app.Run(ctx, append([]string{"--config", config}, deliveryInitArgs(db, scope, outbox, "http://127.0.0.1:1")...))
		require.ErrorContains(t, err, "does not accept --config")
	}
	require.NoFileExists(t, outbox)
	require.Error(t, app.Run(ctx, deliveryInitArgs(filepath.Join(t.TempDir(), "absent.db"), scope, outbox, "http://127.0.0.1:1")))
	require.NoFileExists(t, outbox)
	require.NoError(t, app.Run(ctx, deliveryInitArgs(db, scope, outbox, "http://127.0.0.1:1")))
	before, err := os.ReadFile(outbox)
	require.NoError(t, err)
	require.Error(t, app.Run(ctx, deliveryInitArgs(db, scope, outbox, "http://127.0.0.1:1")))
	unchanged, err := os.ReadFile(outbox)
	require.NoError(t, err)
	require.Equal(t, before, unchanged)
	for _, change := range []struct{ flag, value string }{{"--max-rows", "0"}, {"--max-native-bytes", "0"}, {"--max-encoded-bytes", "0"}, {"--timeout", "0s"}} {
		args := deliveryStageArgs(db, scope, outbox)
		for i := range args {
			if args[i] == change.flag {
				args[i+1] = change.value
			}
		}
		require.Error(t, app.Run(ctx, args))
	}
	// A bounded capture refusal and a different physical file cannot stage.
	args := deliveryStageArgs(db, scope, outbox)
	for i := range args {
		if args[i] == "--max-rows" {
			args[i+1] = "1"
		}
	}
	require.ErrorIs(t, app.Run(ctx, args), store.ErrNativeLimit)
	copied := filepath.Join(t.TempDir(), "copy.db")
	require.NoError(t, os.WriteFile(copied, original, 0o600))
	require.ErrorIs(t, app.Run(ctx, deliveryStageArgs(copied, scope, outbox)), store.ErrNativeBaseline)
	scopeBytes, err := os.ReadFile(scope)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(scope, bytes.ReplaceAll(scopeBytes, []byte(`"T1"`), []byte(`"T2"`)), 0o600))
	require.ErrorIs(t, app.Run(ctx, deliveryStageArgs(db, scope, outbox)), remotedelivery.ErrOutboxBaseline)
	require.NoError(t, os.WriteFile(scope, scopeBytes, 0o600))
	o, err := remotedelivery.OpenCurrentOutbox(ctx, outbox)
	require.NoError(t, err)
	state, err := o.State(ctx)
	require.NoError(t, err)
	require.Empty(t, state.PendingCaptureID)
	require.Equal(t, "unqualified", state.BaselineState)
	require.NoError(t, o.Close())
	after, err := os.ReadFile(db)
	require.NoError(t, err)
	require.Equal(t, original, after)
	// Legacy native schema is rejected, not upgraded by the CLI.
	st, err := store.Open(db)
	require.NoError(t, err)
	_, err = st.DB().Exec(`pragma user_version=8`)
	require.NoError(t, err)
	require.NoError(t, st.Close())
	legacy, err := os.ReadFile(db)
	require.NoError(t, err)
	require.ErrorIs(t, app.Run(ctx, deliveryStageArgs(db, scope, outbox)), store.ErrNativeBaseline)
	after, err = os.ReadFile(db)
	require.NoError(t, err)
	require.Equal(t, legacy, after)
	require.NotContains(t, output.String(), "canary")
}

func TestDeliveryCLIScopeIsExplicitBoundedAndStrict(t *testing.T) {
	db, scope, outbox, app, output := deliveryCLIFixture(t)
	original, err := os.ReadFile(scope)
	require.NoError(t, err)
	for name, body := range map[string][]byte{
		"missing opt-in":   bytes.Replace(original, []byte(`,"include_workspace_users":false`), nil, 1),
		"null keys":        bytes.Replace(original, []byte(`"sync_state_keys":[]`), []byte(`"sync_state_keys":null`), 1),
		"unknown":          bytes.Replace(original, []byte(`"version":1`), []byte(`"version":1,"unknown":"private-canary"`), 1),
		"duplicate":        bytes.Replace(original, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		"duplicate nested": bytes.Replace(original, []byte(`"channel_id":"C1"`), []byte(`"channel_id":"C1","channel_id":"C1"`), 1),
		"invalid UTF8":     append([]byte{0xff}, original...),
		"oversize":         bytes.Repeat([]byte(" "), deliveryScopeMaxBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(scope, body, 0o600))
			err := app.Run(context.Background(), deliveryInitArgs(db, scope, outbox, "http://127.0.0.1:1"))
			require.Error(t, err)
			require.NotContains(t, err.Error(), "canary")
			require.NoFileExists(t, outbox)
		})
	}
	require.NotContains(t, output.String(), "canary")
}

func TestDeliveryCLIHelpAndFormats(t *testing.T) {
	for _, command := range []string{"init", "stage-seed", "deliver"} {
		var output bytes.Buffer
		app := &App{Stdout: &output, Stderr: &output}
		require.NoError(t, app.Run(context.Background(), []string{"delivery", command, "--help"}))
		require.Contains(t, output.String(), "-outbox")
	}
	for _, format := range []string{"text", "log"} {
		t.Run(format, func(t *testing.T) {
			db, scope, outbox, app, output := deliveryCLIFixture(t)
			args := deliveryInitArgs(db, scope, outbox, "http://127.0.0.1:1")[1:]
			require.NoError(t, app.Run(context.Background(), append([]string{"--format", format}, args...)))
			require.Contains(t, strings.ToLower(output.String()), "unqualified")
			require.NotContains(t, output.String(), "canary")
		})
	}
}

func TestDeliveryCLIStagedHoldIsVisibleAndNotQualified(t *testing.T) {
	db, scope, outbox, app, output := deliveryCLIFixture(t)
	st, err := store.Open(db)
	require.NoError(t, err)
	_, err = st.DB().Exec(`update users set id=null`)
	require.NoError(t, err)
	require.NoError(t, st.Close())
	body, err := os.ReadFile(scope)
	require.NoError(t, err)
	body = bytes.Replace(body, []byte(`"include_workspace_users":false`), []byte(`"include_workspace_users":true`), 1)
	require.NoError(t, os.WriteFile(scope, body, 0o600))
	require.NoError(t, app.Run(context.Background(), deliveryInitArgs(db, scope, outbox, "http://127.0.0.1:1")))
	output.Reset()
	require.ErrorIs(t, app.Run(context.Background(), deliveryStageArgs(db, scope, outbox)), remotedelivery.ErrSeedBaselineHeld)
	require.Contains(t, output.String(), `"baseline_state": "unqualified"`)
	require.Contains(t, output.String(), `"baseline_hold": "ambiguous_keys"`)
	require.Contains(t, output.String(), `"native_anchor"`)
	require.ErrorIs(t, app.Run(context.Background(), deliveryStageArgs(db, scope, outbox)), remotedelivery.ErrPendingCapture)
}
