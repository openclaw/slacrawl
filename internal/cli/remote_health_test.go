package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/config"
)

func TestCloudHealthUsesArchiveStatus(t *testing.T) {
	status := remote.Status{App: "slacrawl", Archive: "fixture", Mode: "cloud",
		ActiveSnapshotID: "pg-slack-current-v1:seed:3064", LastIngestAt: "2026-11-20T00:00:00Z",
		Capabilities: []string{"slacrawl.messages.list"}, Warnings: []string{"Coverage is not yet verified."}}
	requests := 0
	code := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/apps/slacrawl/archives/fixture/status" || r.Header.Get("Authorization") != "Bearer fixture-reader" {
			t.Errorf("unexpected archive request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(code)
		if code == http.StatusOK {
			_ = json.NewEncoder(w).Encode(status)
		}
	}))
	defer server.Close()
	cfg := config.Default()
	cfg.DBPath = filepath.Join(t.TempDir(), "absent", "forbidden.sqlite")
	cfg.Remote = remote.Config{Mode: remote.ModeCloud, Endpoint: server.URL, Archive: "fixture", TokenEnv: "SLACK_HEALTH_FIXTURE_TOKEN"}
	file := filepath.Join(t.TempDir(), "reader.toml")
	if err := cfg.Save(file); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SLACK_HEALTH_FIXTURE_TOKEN", "fixture-reader")
	for _, command := range []string{"status", "doctor"} {
		var out bytes.Buffer
		app := New()
		app.Stdout, app.Stderr = &out, io.Discard
		if err := app.Run(context.Background(), []string{"--config", file, command, "--json"}); err != nil {
			t.Fatal(err)
		}
		var got remote.Status
		if err := json.Unmarshal(out.Bytes(), &got); err != nil || !reflect.DeepEqual(got, status) {
			t.Fatalf("remote status changed: %s (%v)", out.Bytes(), err)
		}
		before := requests
		if err := app.Run(context.Background(), []string{"--config", file, command, "--help"}); err != nil || requests != before {
			t.Fatalf("help contacted archive: %v", err)
		}
		if err := app.Run(context.Background(), []string{"--config", file, command, "unexpected"}); err == nil || requests != before {
			t.Fatal("invalid arguments contacted archive")
		}
		code = http.StatusServiceUnavailable
		out.Reset()
		if err := app.Run(context.Background(), []string{"--config", file, command, "--json"}); err == nil || out.Len() != 0 {
			t.Fatal("archive failure returned local status")
		}
		code = http.StatusOK
	}
	status.Archive = "another-archive"
	app := New()
	app.Stdout, app.Stderr = io.Discard, io.Discard
	if err := app.Run(context.Background(), []string{"--config", file, "status"}); err == nil {
		t.Fatal("different archive accepted")
	}
	if _, err := os.Stat(filepath.Dir(cfg.DBPath)); !os.IsNotExist(err) {
		t.Fatalf("cloud health touched local archive: %v", err)
	}
}
