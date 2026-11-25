package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/slacrawl/internal/acquisition"
	"github.com/openclaw/slacrawl/internal/config"
)

// Run the shipped ExecStart arguments through the real CLI and native transports.
// The HTTP fixture supplies durable checkpoints; no SQLite archive is available.
func TestAcquireServiceUsesAPICheckpoints(t *testing.T) {
	unitBytes, err := os.ReadFile("../../operations/slacrawl-acquire@.service")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(unitBytes)
	var command []string
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			command = strings.Fields(strings.TrimPrefix(line, "ExecStart="))
		}
	}
	if len(command) < 7 || strings.Join(command[:6], " ") != "/usr/bin/flock --nonblock --conflict-exit-code=75 /var/lib/slacrawl/%i/acquire.lock /usr/local/bin/slacrawl --config" {
		t.Fatal("service must retain its source lock and explicit credential config")
	}
	for _, want := range []string{"EnvironmentFile=/etc/slacrawl/provider-%i.env", "EnvironmentFile=/etc/slacrawl/acquire-%i.env", "TimeoutStartSec=120s", "TimeoutStopSec=30s", "KillMode=control-group", "NoNewPrivileges=yes", "ProtectSystem=strict", "MemoryMax=512M"} {
		if !strings.Contains(unit, want+"\n") {
			t.Fatalf("missing unit boundary %s", want)
		}
	}
	for _, arg := range command {
		if arg == "sync" || arg == "delivery" || arg == "--since" || arg == "--until" || arg == "--run-id" || arg == "--db" {
			t.Fatalf("scheduled command selects local state or overrides checkpoint: %s", arg)
		}
	}

	t.Setenv("SLACK_BOT_TOKEN", "fixture-provider")
	t.Setenv("SLACRAWL_PUBLISHER_TOKEN", "fixture-publisher")
	cfg := config.Default()
	cfg.DBPath = filepath.Join(t.TempDir(), "absent", "must-not-exist.sqlite")
	configPath := filepath.Join(t.TempDir(), "credentials.toml")
	if err = cfg.Save(configPath); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	horizon := since.Add(24 * time.Hour)
	run := acquisition.Run{ID: "retained-run", ExpectedRevision: 0, WorkspaceID: "T1", ChannelID: "C1", SourceName: "api-bot", Since: since, Horizon: horizon}
	state := acquisition.State{Profile: "slack-acquisition/v1", Sequence: 1, CompletedSequence: 1, ActiveRun: &run}
	pages, histories, identities := 0, 0, 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("token") != "fixture-provider" || r.Header.Get("Authorization") == "Bearer fixture-publisher" {
			t.Error("provider credential separation failed")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			identities++
			_, _ = io.WriteString(w, `{"ok":true,"team_id":"T1","user_id":"U1"}`)
		case "/conversations.history":
			histories++
			if r.Form.Get("channel") != "C1" || r.Form.Get("cursor") != state.Cursor || r.Form.Get("oldest") != "1795046400.000000" || r.Form.Get("latest") != "1795132800.000000" {
				t.Error("service lost the API-owned window/cursor")
			}
			next := strconv.Itoa(pages + 1)
			if pages == 20 {
				next = ""
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "has_more": next != "", "messages": []any{}, "response_metadata": map[string]string{"next_cursor": next}})
		default:
			t.Errorf("unexpected provider route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-publisher" {
			t.Error("publisher credential separation failed")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/apps/slacrawl/archives/fixture/acquisition/state":
			if r.Method != http.MethodGet || r.URL.Query().Get("channel") != "C1" || r.URL.Query().Get("sourceName") != "api-bot" {
				t.Error("wrong checkpoint request")
			}
			_ = json.NewEncoder(w).Encode(state)
		case "/v1/apps/slacrawl/archives/fixture/acquisition/pages":
			body, _ := io.ReadAll(r.Body)
			var page acquisition.Page
			if r.Method != http.MethodPost || json.Unmarshal(body, &page) != nil || page.RunID != run.ID || page.ExpectedRevision != state.Revision || page.Cursor != state.Cursor || len(page.Changes) != 0 {
				t.Error("wrong atomic page")
			}
			pages++
			state.Revision++
			state.Sequence++
			state.Cursor = page.NextCursor
			if page.Complete {
				state.ActiveRun = nil
				state.CompletedThrough = &horizon
			}
			hash := sha256.Sum256(body)
			_ = json.NewEncoder(w).Encode(acquisition.Receipt{RunID: run.ID, PageID: page.ID, SHA256: hex.EncodeToString(hash[:]), Revision: state.Revision, Sequence: state.Sequence, Final: page.Complete})
		default:
			t.Errorf("unexpected API route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	vars := map[string]string{"SLACRAWL_ORIGIN": api.URL, "SLACRAWL_ARCHIVE": "fixture", "SLACRAWL_WORKSPACE": "T1", "SLACRAWL_CHANNEL": "C1", "SLACRAWL_SOURCE": "api-bot"}
	args := append([]string(nil), command[5:]...)
	for i, arg := range args {
		args[i] = os.Expand(arg, func(key string) string { return vars[key] })
		if args[i] == "/etc/slacrawl/acquire-%i.toml" {
			args[i] = configPath
		}
	}
	runCLI := func() string {
		var out bytes.Buffer
		app := New()
		app.Stdout, app.Stderr = &out, io.Discard
		app.apiURL, app.httpClient = provider.URL+"/", provider.Client()
		if err := app.Run(context.Background(), args); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if out := runCLI(); !strings.Contains(out, `"status": "bounded"`) || pages != 20 || state.ActiveRun == nil {
		t.Fatalf("first cycle did not retain bounded progress: %s", out)
	}
	if out := runCLI(); !strings.Contains(out, `"complete": true`) || pages != 21 || histories != 21 || identities != 2 {
		t.Fatalf("second cycle did not resume and complete: %s", out)
	}
	// The recurring unit cannot silently invent a first-run history boundary.
	state.ActiveRun, state.CompletedThrough = nil, nil
	app := New()
	app.Stdout, app.Stderr = io.Discard, io.Discard
	app.apiURL, app.httpClient = provider.URL+"/", provider.Client()
	if err := app.Run(context.Background(), args); err == nil || !strings.Contains(err.Error(), "first acquisition requires explicit --since") {
		t.Fatalf("missing checkpoint was not refused: %v", err)
	}
	if histories != 21 || pages != 21 {
		t.Fatal("missing checkpoint performed acquisition")
	}
	if _, err := os.Stat(filepath.Dir(cfg.DBPath)); !os.IsNotExist(err) {
		t.Fatal("service entered local SQLite setup", err)
	}
}
