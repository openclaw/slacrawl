package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openclaw/slacrawl/internal/config"
)

// The cross-repository fixture supplies the real PostgreSQL HTTP owner; only
// Slack is simulated through the normal native provider transport.
func TestAcquirePostgresHTTP(t *testing.T) {
	origin := os.Getenv("CRAWL_TEST_SLACK_ACQUISITION_ORIGIN")
	if origin == "" {
		t.Skip("requires the real PostgreSQL acquisition fixture")
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || !net.ParseIP(u.Hostname()).IsLoopback() {
		t.Fatal("fixture origin must be loopback")
	}
	t.Setenv("SLACK_BOT_TOKEN", "fixture-bot")
	t.Setenv("CRAWL_REMOTE_TOKEN", os.Getenv("CRAWL_TEST_SLACK_ACQUISITION_TOKEN"))
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		if r.Form.Get("token") != "fixture-bot" {
			t.Error("wrong provider credential")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/auth.test" {
			_, _ = io.WriteString(w, `{"ok":true,"team_id":"T1","user_id":"U1"}`)
			return
		}
		if r.URL.Path != "/conversations.history" {
			t.Error("unexpected provider route", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Form.Get("channel") != "C1" || r.Form.Get("oldest") != "1795046400.000000" || r.Form.Get("latest") != "1795132800.000000" {
			t.Error("wrong fixed window", r.Form)
		}
		switch r.Form.Get("cursor") {
		case "":
			_, _ = io.WriteString(w, `{"ok":true,"has_more":true,"messages":[{"ts":"1795046402.000000","user":"U1","text":"new\u0000text","unknown":9007199254740993,"files":[{"id":"F1","name":"a.txt","unknown":9007199254740993}]},{"ts":"1795046401.000000","user":"U1","text":"retained"}],"response_metadata":{"next_cursor":"next"}}`)
		case "next":
			_, _ = io.WriteString(w, `{"ok":true,"has_more":false,"messages":[],"response_metadata":{"next_cursor":""}}`)
		default:
			t.Error("wrong resume cursor")
		}
	}))
	defer provider.Close()
	cfg := config.Default()
	cfg.DBPath = filepath.Join(t.TempDir(), "must-not-exist.sqlite")
	configPath := filepath.Join(t.TempDir(), "credentials.toml")
	if err = cfg.Save(configPath); err != nil {
		t.Fatal(err)
	}
	args := []string{"--config", configPath, "--json", "acquire", "--origin", origin, "--archive", "collector", "--workspace", "T1", "--channel", "C1", "--source", "api-bot", "--since", "2026-11-19T00:00:00Z", "--until", "2026-11-20T00:00:00Z", "--max-pages", "3", "--timeout", "20s", "--run-id", "collector-run"}
	run := func() (*bytes.Buffer, error) {
		var out bytes.Buffer
		a := New()
		a.Stdout = &out
		a.Stderr = io.Discard
		a.apiURL = provider.URL + "/"
		a.httpClient = provider.Client()
		return &out, a.Run(context.Background(), args)
	}
	if _, err = run(); err == nil {
		t.Fatal("fixture must drop first acknowledgement")
	}
	out, err := run()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"complete": true`) {
		t.Fatal("not completed", out.String())
	}
	if _, err = os.Stat(cfg.DBPath); !os.IsNotExist(err) {
		t.Fatal("acquisition touched local SQLite", err)
	}
}
