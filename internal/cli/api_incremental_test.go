package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/stretchr/testify/require"
)

func TestAPIIncrementalCLIRejectsAmbiguousAdmissionBeforeOpeningStore(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		omit        string
		share       bool
	}{
		{name: "full", extra: "--full"},
		{name: "since", extra: "--since="},
		{name: "latest-only", extra: "--latest-only"},
		{name: "limit", extra: "--limit=0"},
		{name: "join", extra: "--auto-join=true"},
		{name: "media", extra: "--with-media=true"},
		{name: "desktop", extra: "--source=desktop"},
		{name: "missing-workspace", omit: "--workspace=T123"},
		{name: "missing-channels", omit: "--channels=C123"},
		{name: "share-import", share: true},
		{name: "missing-archive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := config.Default()
			cfg.DBPath, cfg.CacheDir, cfg.LogDir = filepath.Join(dir, "missing.db"), filepath.Join(dir, "cache"), filepath.Join(dir, "logs")
			if tc.share {
				cfg.Share.Remote, cfg.Share.AutoUpdate = "https://example.invalid/archive.git", true
			}
			configPath := filepath.Join(dir, "config.toml")
			require.NoError(t, cfg.Save(configPath))
			args := []string{"--config", configPath, "sync", "--incremental", "--auto-join=false", "--with-media=false"}
			for _, arg := range []string{"--workspace=T123", "--channels=C123"} {
				if arg != tc.omit {
					args = append(args, arg)
				}
			}
			if tc.extra != "" {
				args = append(args, tc.extra)
			}
			var output bytes.Buffer
			app := &App{Stdout: &output, Stderr: &output, httpClient: &http.Client{Transport: rejectingTransport{t: t}}}
			require.ErrorContains(t, app.Run(context.Background(), args), "--incremental requires")
			_, err := os.Stat(cfg.DBPath)
			require.True(t, os.IsNotExist(err), "rejected admission must not create an archive")
		})
	}
}

type rejectingTransport struct{ t *testing.T }

func (r rejectingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.t.Fatal("rejected admission must not make an HTTP request")
	return nil, nil
}

func TestAPIIncrementalCLIRequiresCoverageAndResumesPendingRange(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DBPath, cfg.CacheDir, cfg.LogDir = filepath.Join(dir, "archive.db"), filepath.Join(dir, "cache"), filepath.Join(dir, "logs")
	cfg.Share.Remote, cfg.Share.AutoUpdate = "", false
	cfg.Slack.Desktop.Enabled, cfg.Slack.User.Enabled, cfg.Slack.App.Enabled = false, false, false
	cfg.Slack.Bot.TokenEnv = "SLACRAWL_INCREMENTAL_BOT"
	cfg.Sync.IncludeDMs, cfg.Sync.FileMedia = new(false), new(false)
	t.Setenv(cfg.Slack.Bot.TokenEnv, "fixture-bot")
	configPath := filepath.Join(dir, "config.toml")
	require.NoError(t, cfg.Save(configPath))
	st, err := store.Open(cfg.DBPath)
	require.NoError(t, err)
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C123", WorkspaceID: "T123", Name: "fixture", UpdatedAt: time.Now()}))
	require.NoError(t, st.Close())

	var oldest []string
	failPage, empty := true, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"team_id":"T123","team":"Fixture"}`))
		case "/conversations.list":
			_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"C123","name":"fixture","is_channel":true,"is_member":true}]}`))
		case "/users.list":
			_, _ = w.Write([]byte(`{"ok":true,"members":[]}`))
		case "/conversations.history":
			require.Equal(t, "C123", r.Form.Get("channel"))
			if r.Form.Get("cursor") == "" {
				oldest = append(oldest, r.Form.Get("oldest"))
				if empty {
					_, _ = w.Write([]byte(`{"ok":true,"messages":[]}`))
				} else {
					_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","ts":"1710000001.000000","text":"retained page"}],"has_more":true,"response_metadata":{"next_cursor":"second"}}`))
				}
			} else if failPage {
				_, _ = w.Write([]byte(`{"ok":false,"error":"synthetic_failure"}`))
			} else {
				_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","ts":"1710000002.000000","text":"second page"}]}`))
			}
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	run := func() error {
		var output bytes.Buffer
		app := &App{Stdout: &output, Stderr: &output, apiURL: server.URL + "/", httpClient: server.Client()}
		return app.Run(ctx, []string{"--config", configPath, "--json", "sync", "--source", "bot", "--workspace", "T123", "--channels", "C123", "--auto-join=false", "--with-media=false", "--concurrency", "1", "--incremental"})
	}
	scope := store.APIHistoryScope{SourceName: "api-bot", WorkspaceID: "T123", ChannelID: "C123"}
	read := func() store.APIHistoryState {
		st, err := store.OpenReadOnly(cfg.DBPath)
		require.NoError(t, err)
		defer st.Close()
		state, err := st.APIHistory(ctx, scope)
		require.NoError(t, err)
		return state
	}
	require.ErrorContains(t, run(), "local completed history")
	require.Empty(t, oldest, "unknown coverage must not dispatch history")
	require.Equal(t, store.APIHistoryState{}, read())

	st, err = store.Open(cfg.DBPath)
	require.NoError(t, err)
	attempt, err := st.BeginAPIHistory(ctx, scope, store.APIHistoryOptions{}, "1710000000.000000")
	require.NoError(t, err)
	require.NoError(t, st.CompleteAPIHistory(ctx, attempt))
	require.NoError(t, st.Close())
	require.ErrorContains(t, run(), "conversations.history API response failed")
	pending := read()
	require.True(t, pending.Complete)
	require.Equal(t, "1710000000.000000", pending.Latest)
	require.NotNil(t, pending.Pending)
	require.Equal(t, "1709996400.000000", *pending.Pending)
	failPage = false
	require.NoError(t, run())
	require.Equal(t, []string{"1709996400.000000", "1709996400.000000"}, oldest)
	completed := read()
	require.Nil(t, completed.Pending)
	require.NotEqual(t, pending.Latest, completed.Latest)
	st, err = store.OpenReadOnly(cfg.DBPath)
	require.NoError(t, err)
	rows, err := st.QueryReadOnly(ctx, "select count(*) as n from messages")
	require.NoError(t, err)
	require.EqualValues(t, 2, rows[0]["n"])
	require.NoError(t, st.Close())

	empty = true
	require.NoError(t, run())
	require.Nil(t, read().Pending)
	require.NotEqual(t, completed.Latest, read().Latest, "empty history must advance its requested horizon")

	st, err = store.Open(cfg.DBPath)
	require.NoError(t, err)
	unbounded := read()
	unbounded.Pending = new("")
	unbounded.Generation = "retained-full-attempt"
	unbounded.PendingLatest = unbounded.Latest
	raw, err := json.Marshal(unbounded)
	require.NoError(t, err)
	require.NoError(t, st.SetSyncState(ctx, scope.SourceName, store.APIHistoryEntityType, `["T123","C123",""]`, string(raw)))
	require.NoError(t, st.Close())
	beforeCalls := len(oldest)
	require.ErrorContains(t, run(), "local completed history")
	require.Len(t, oldest, beforeCalls, "incremental mode must not resume an unbounded Full attempt")
	require.Equal(t, unbounded, read())
}
