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
	"testing"

	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/stretchr/testify/require"
)

const fixtureSeedManifest = `{"schema":"slack-approved-channel-inventory/v1","policy":"slack-report-channels/v1","workspace":"T1","readerApp":"A1","readerUser":"U1","channels":[{"id":"C1","name":"one","kind":"public_channel","member":true,"archived":false,"shared":false,"external":false,"orgShared":false},{"id":"C2","name":"two","kind":"private_channel","member":true,"archived":false,"shared":true,"external":true,"orgShared":false}]}`

const firstMetadataChannel = `{"id":"C1","name":"one","context_team_id":"T1","created":1700000000,"is_member":true,"is_archived":false,"is_private":false,"is_im":false,"is_mpim":false,"is_channel":true,"is_group":false,"is_shared":false,"is_ext_shared":false,"is_org_shared":false}`
const secondMetadataChannel = `{"id":"C2","name":"two","context_team_id":"T1","created":1700000001,"is_member":true,"is_archived":false,"is_private":true,"is_im":false,"is_mpim":false,"is_channel":true,"is_group":false,"is_shared":true,"is_ext_shared":true,"is_org_shared":false,"topic":{"value":"topic-canary"},"latest":{"text":"message-canary"},"members":["member-canary"]}`

func metadataFixture(t *testing.T, second string, appID string) (*App, string, []string, *[]string) {
	t.Helper()
	dir := t.TempDir()
	target, manifest := filepath.Join(dir, "seed"), filepath.Join(dir, "manifest.json")
	require.NoError(t, os.WriteFile(manifest, []byte(fixtureSeedManifest), 0600))
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		require.Equal(t, "fixture-reader", r.Form.Get("token"))
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case "/auth.test":
			fmt.Fprint(w, `{"ok":true,"team":"fixture","team_id":"T1","user_id":"U1","bot_id":"B1"}`)
		case "/bots.info":
			require.Equal(t, "B1", r.Form.Get("bot"))
			fmt.Fprintf(w, `{"ok":true,"bot":{"id":"B1","user_id":"U1","app_id":%q}}`, appID)
		case "/conversations.list":
			require.Equal(t, "T1", r.Form.Get("team_id"))
			require.Equal(t, "public_channel,private_channel", r.Form.Get("types"))
			switch r.Form.Get("cursor") {
			case "":
				fmt.Fprintf(w, `{"ok":true,"channels":[%s,{"id":"CBYSTANDER","name":"excluded-channel-canary"}],"response_metadata":{"next_cursor":"two"}}`, firstMetadataChannel)
			case "two":
				fmt.Fprintf(w, `{"ok":true,"channels":[%s]}`, second)
			default:
				t.Errorf("unexpected cursor")
			}
		default:
			t.Errorf("unexpected provider read: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("SLACK_BOT_TOKEN", "fixture-reader")
	app := New()
	app.Stdout, app.Stderr = io.Discard, io.Discard
	app.apiURL, app.httpClient = srv.URL+"/", srv.Client()
	return app, target, []string{"--json", "acquire", "metadata-seed", "--manifest", manifest, "--manifest-sha256", fmt.Sprintf("%x", sha256.Sum256([]byte(fixtureSeedManifest))), "--output-dir", target, "--timeout", "15s"}, &calls
}

func TestMetadataSeedStagesAndDeliversOnlyApprovedChannelFacts(t *testing.T) {
	app, target, args, calls := metadataFixture(t, secondMetadataChannel, "A1")
	ctx := context.Background()
	require.NoError(t, app.Run(ctx, args))
	require.Equal(t, []string{"/auth.test", "/bots.info", "/conversations.list", "/conversations.list"}, *calls)
	db, scope, outbox := filepath.Join(target, "source.sqlite"), filepath.Join(target, "scope.json"), filepath.Join(target, "outbox.sqlite")
	st, err := store.OpenReadOnly(db)
	require.NoError(t, err)
	defer st.Close()
	var channels, users, messages int
	require.NoError(t, st.DB().QueryRow(`select (select count(*) from channels),(select count(*) from users),(select count(*) from messages)`).Scan(&channels, &users, &messages))
	require.Equal(t, []int{2, 0, 0}, []int{channels, users, messages})
	var raw string
	var priv, shared bool
	require.NoError(t, st.DB().QueryRow(`select raw_json,is_private,is_shared from channels where id='C2'`).Scan(&raw, &priv, &shared))
	require.True(t, priv && shared)
	require.Contains(t, raw, `"is_ext_shared":true`)
	require.Contains(t, raw, `"created":1700000001`)
	require.NotContains(t, raw, "canary")
	var plan *remote.SlackSeedPlan
	var received []string
	var seed string
	part := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NotContains(t, string(body), "canary")
		suffix := strconv.Itoa(part - 1)
		if part == 0 {
			plan, err = remote.DecodeSlackSeedManifest(body)
			require.NoError(t, err)
			manifest := plan.Manifest()
			require.Equal(t, []string{"C1", "C2"}, manifest.Scope.ChannelIDs)
			require.False(t, manifest.Scope.IncludeWorkspaceUsers)
			seed = manifest.SeedID
			suffix = "begin"
		}
		if strings.Contains(r.URL.Path, "/chunks/") {
			chunk, err := plan.ValidateChunk(body)
			require.NoError(t, err)
			for _, change := range chunk.Changes {
				received = append(received, change.Table)
			}
		}
		if strings.HasSuffix(r.URL.Path, "/complete") {
			_, err := plan.ValidateComplete(body)
			require.NoError(t, err)
			suffix = "complete"
		}
		id := "seed1:" + seed + ":" + suffix
		part++
		require.NoError(t, json.NewEncoder(w).Encode(remote.ChangeReceipt{App: "slacrawl", Archive: "fixture", BindingID: "fixture-binding", BatchID: id, Cursor: id, Sequence: strconv.Itoa(part), PayloadSHA256: fmt.Sprintf("%x", sha256.Sum256(body))}))
	}))
	defer server.Close()
	app.httpClient = server.Client()
	t.Setenv(remote.DefaultTokenEnv, "synthetic-publisher")
	require.NoError(t, app.Run(ctx, deliveryInitArgs(db, scope, outbox, server.URL)))
	require.NoError(t, app.Run(ctx, deliveryStageArgs(db, scope, outbox)))
	var output bytes.Buffer
	app.Stdout = &output
	require.NoError(t, app.Run(ctx, deliveryArgs(outbox, 64)))
	require.Contains(t, output.String(), `"baseline_state": "qualified"`)
	require.GreaterOrEqual(t, part, 3)
	require.ElementsMatch(t, []string{"workspaces", "channels", "channels"}, received)
	before, err := os.ReadFile(scope)
	require.NoError(t, err)
	require.ErrorContains(t, app.Run(ctx, args), "new directory")
	after, err := os.ReadFile(scope)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Len(t, *calls, 4)
}

func TestMetadataSeedRejectsIncompleteApprovedFlags(t *testing.T) {
	app, target, args, calls := metadataFixture(t, secondMetadataChannel, "A1")
	body := []byte(strings.Replace(fixtureSeedManifest, `"orgShared":false`, `"unknown":false`, 1))
	require.NoError(t, os.WriteFile(args[4], body, 0600))
	args[6] = fmt.Sprintf("%x", sha256.Sum256(body))
	require.ErrorContains(t, app.Run(context.Background(), args), "invalid approved channel inventory")
	require.Empty(t, *calls)
	_, err := os.Stat(target)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestMetadataSeedStopsBeforeCreatingAnArchive(t *testing.T) {
	for _, tc := range []struct {
		name, channel, app string
		badDigest          bool
	}{
		{"hash", secondMetadataChannel, "A1", true},
		{"reader", secondMetadataChannel, "AOTHER", false},
		{"not-a-member", strings.Replace(secondMetadataChannel, `"is_member":true`, `"is_member":false`, 1), "A1", false},
		{"unshared", strings.Replace(secondMetadataChannel, `"is_ext_shared":true`, `"is_ext_shared":false`, 1), "A1", false},
		{"foreign", strings.Replace(secondMetadataChannel, "T1", "TOTHER", 1), "A1", false},
		{"direct", strings.Replace(secondMetadataChannel, `"is_mpim":false`, `"is_mpim":true`, 1), "A1", false},
		{"parent", strings.Replace(secondMetadataChannel, `"id":"C2"`, `"id":"C2","parent_conversation":"D1"`, 1), "A1", false},
		{"missing", `{"id":"COTHER"}`, "A1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, target, args, calls := metadataFixture(t, tc.channel, tc.app)
			if tc.badDigest {
				args[6] = strings.Repeat("0", 64)
			}
			require.Error(t, app.Run(context.Background(), args))
			_, err := os.Stat(target)
			require.ErrorIs(t, err, os.ErrNotExist)
			if tc.badDigest {
				require.Empty(t, *calls)
			}
			if tc.name == "reader" {
				require.Equal(t, []string{"/auth.test", "/bots.info"}, *calls)
			}
		})
	}
}
