package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/config"
	"github.com/zalando/go-keyring"
)

func TestRemoteSessionLoginAndCloudReads(t *testing.T) {
	keyring.MockInit()
	t.Setenv("SLACRAWL_TEST_GITHUB_TOKEN", "local-github-token")
	// A configured keyring source must win over an unrelated inherited env token.
	t.Setenv(remote.DefaultTokenEnv, "wrong-env-token")
	queries := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/github/token" {
			var req remote.GitHubTokenLoginRequest
			if r.Method != "POST" || r.Header.Get("Authorization") != "" || json.NewDecoder(r.Body).Decode(&req) != nil || req.Token != "local-github-token" {
				t.Error("login protocol differs")
				w.WriteHeader(400)
				return
			}
			_ = json.NewEncoder(w).Encode(remote.LoginPollResult{Status: "complete", Token: "private-session", Login: "fixture", Org: "example"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer private-session" {
			t.Error("session not read from keyring")
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/v1/whoami":
			_ = json.NewEncoder(w).Encode(remote.Identity{Login: "fixture", Org: "example", Roles: []string{"reader", "publisher"}})
		case "/v1/apps/slacrawl/archives/fixture/status":
			_ = json.NewEncoder(w).Encode(remote.Status{App: "slacrawl", Archive: "fixture", Mode: "cloud"})
		case "/v1/apps/slacrawl/archives/fixture/query":
			var q remote.QueryRequest
			if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
				t.Fatal(err)
			}
			queries = append(queries, q.Name)
			_ = json.NewEncoder(w).Encode(remote.QueryResult{Columns: []string{"workspace_id", "workspace_name", "channel_id", "channel_name", "ts", "user_id", "user_name", "text", "normalized_text", "thread_ts", "reply_count", "latest_reply", "subtype", "source_name", "mention_displays"}, Rows: [][]any{}, Stats: remote.QueryStats{SnapshotID: "pinned"}})
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	cfg := config.Default()
	cfg.DBPath = filepath.Join(t.TempDir(), "absent", "forbidden.sqlite")
	cfg.WorkspaceID = "T1"
	file := filepath.Join(t.TempDir(), "reader.toml")
	if err := cfg.Save(file); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	app := New()
	app.Stdout = &out
	app.Stderr = io.Discard
	run := func(args ...string) error {
		out.Reset()
		return app.Run(context.Background(), append([]string{"--config", file, "--json"}, args...))
	}
	if err := run("remote", "login", "--endpoint", server.URL, "--archive", "fixture", "--github-token-env", "SLACRAWL_TEST_GITHUB_TOKEN"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-session", "local-github-token"} {
		if bytes.Contains(raw, []byte(secret)) || strings.Contains(out.String(), secret) {
			t.Fatal("secret escaped keyring")
		}
	}
	saved, err := config.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if saved.DBPath != cfg.DBPath || saved.WorkspaceID != "T1" || saved.Remote.Auth.TokenSource != "keyring" {
		t.Fatal("config not preserved")
	}
	if token, err := keyring.Get(saved.Remote.Auth.KeyringService, saved.Remote.Auth.KeyringAccount); err != nil || token != "private-session" {
		t.Fatal("keyring session missing")
	}
	for _, args := range [][]string{{"remote", "whoami"}, {"remote", "status"}, {"status"}, {"doctor"}, {"messages", "--channel", "C1"}, {"messages", "--channel", "C1", "--thread", "1.000000"}} {
		if err := run(args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if strings.Join(queries, ",") != "slacrawl.messages.list,slacrawl.messages.thread" {
		t.Fatalf("queries %v", queries)
	}
	for _, cmd := range []string{"init", "sync", "search", "diagnostics"} {
		if err := run(cmd); err == nil || !strings.Contains(err.Error(), "not supported in cloud mode") {
			t.Fatalf("%s did not fail closed: %v", cmd, err)
		}
	}
	if _, err := os.Stat(filepath.Dir(cfg.DBPath)); !os.IsNotExist(err) {
		t.Fatalf("local database touched: %v", err)
	}
	// Keyring errors fail closed; the env credential must not become a fallback.
	keyring.MockInitWithError(errors.New("keyring unavailable"))
	if err := run("remote", "whoami"); err == nil {
		t.Fatal("keyring failure ignored")
	}
}

func TestRemoteLoginFailurePreservesConfig(t *testing.T) {
	keyring.MockInit()
	t.Setenv("SLACRAWL_TEST_GITHUB_TOKEN", "local-github-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(remote.LoginPollResult{Status: "pending", Token: "must-not-save"})
	}))
	defer server.Close()
	cfg := config.Default()
	file := filepath.Join(t.TempDir(), "config.toml")
	if err := cfg.Save(file); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(file)
	app := New()
	app.Stdout = io.Discard
	app.Stderr = io.Discard
	for _, extra := range [][]string{{}, {"--timeout", "0"}, {"--timeout", "3m"}, {"unexpected"}} {
		args := []string{"--config", file, "remote", "login", "--endpoint", server.URL, "--archive", "fixture", "--github-token-env", "SLACRAWL_TEST_GITHUB_TOKEN"}
		args = append(args, extra...)
		if err := app.Run(context.Background(), args); err == nil {
			t.Fatal("invalid/incomplete login accepted")
		}
		after, _ := os.ReadFile(file)
		if !bytes.Equal(before, after) {
			t.Fatal("failed login rewrote config")
		}
	}
	cfg.Remote.Endpoint, cfg.Remote.Archive = server.URL, "fixture"
	account, err := remoteLoginKeyringAccount(cfg, file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.Get("crawl-remote", account); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatal("failed login stored session")
	}
}

func TestRemoteLoginSeparatesEndpointAndConfigSessions(t *testing.T) {
	keyring.MockInit()
	t.Setenv("SLACRAWL_TEST_GITHUB_TOKEN", "local-github-token")
	makeServer := func(session string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/auth/github/token" {
				_ = json.NewEncoder(w).Encode(remote.LoginPollResult{Status: "complete", Token: session})
				return
			}
			if r.URL.Path != "/v1/whoami" || r.Header.Get("Authorization") != "Bearer "+session {
				t.Error("wrong endpoint session forwarded")
				w.WriteHeader(401)
				return
			}
			_ = json.NewEncoder(w).Encode(remote.Identity{Login: session})
		}))
	}
	first, second := makeServer("first-session"), makeServer("second-session")
	defer first.Close()
	defer second.Close()
	file := filepath.Join(t.TempDir(), "reader.toml")
	login := func(file, endpoint string) config.Config {
		app := New()
		app.Stdout = io.Discard
		app.Stderr = io.Discard
		if err := app.Run(context.Background(), []string{"--config", file, "remote", "login", "--endpoint", endpoint, "--archive", "fixture", "--github-token-env", "SLACRAWL_TEST_GITHUB_TOKEN"}); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(file)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	old := login(file, first.URL)
	next := login(file, second.URL)
	if old.Remote.Auth.KeyringAccount == next.Remote.Auth.KeyringAccount {
		t.Fatal("different endpoints shared a keyring account")
	}
	for _, cfg := range []config.Config{old, next} {
		client, err := newRemoteClient(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Whoami(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	other := login(filepath.Join(t.TempDir(), "reader.toml"), second.URL)
	if other.Remote.Auth.KeyringAccount == next.Remote.Auth.KeyringAccount {
		t.Fatal("different config files shared a keyring account")
	}
}

func TestRemoteLoginPromotionFailurePreservesConfigAndSession(t *testing.T) {
	for _, existingBound := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-bound-slot", true: "existing-bound-slot"}[existingBound], func(t *testing.T) {
			keyring.MockInit()
			t.Setenv("SLACRAWL_TEST_GITHUB_TOKEN", "local-github-token")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth/github/token" {
					_ = json.NewEncoder(w).Encode(remote.LoginPollResult{Status: "complete", Token: "replacement-session"})
					return
				}
				if r.URL.Path != "/v1/whoami" || r.Header.Get("Authorization") != "Bearer prior-session" {
					t.Error("prior session changed")
					w.WriteHeader(401)
					return
				}
				_ = json.NewEncoder(w).Encode(remote.Identity{Login: "prior"})
			}))
			defer server.Close()
			file := filepath.Join(t.TempDir(), "reader.toml")
			cfg := config.Default()
			cfg.Remote = remote.Config{Mode: "cloud", Endpoint: server.URL, Archive: "fixture"}
			bound, err := remoteLoginKeyringAccount(cfg, file)
			if err != nil {
				t.Fatal(err)
			}
			priorAccount := "old-archive-only-slot"
			if existingBound {
				priorAccount = bound
			}
			cfg.Remote.Auth = remote.AuthConfig{TokenSource: "keyring", KeyringService: "crawl-remote", KeyringAccount: priorAccount}
			if err := keyring.Set("crawl-remote", priorAccount, "prior-session"); err != nil {
				t.Fatal(err)
			}
			if err := cfg.Save(file); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			original := promoteRemoteLoginConfig
			promoteRemoteLoginConfig = func(staged, target string) error {
				info, err := os.Stat(staged)
				if err != nil || info.Mode().Perm() != 0o600 || filepath.Dir(staged) != filepath.Dir(target) {
					t.Error("staging was not private and on the target filesystem")
				}
				return errors.New("fixture promotion failure")
			}
			defer func() { promoteRemoteLoginConfig = original }()
			app := New()
			app.Stdout = io.Discard
			app.Stderr = io.Discard
			if err := app.Run(context.Background(), []string{"--config", file, "remote", "login", "--github-token-env", "SLACRAWL_TEST_GITHUB_TOKEN"}); err == nil {
				t.Fatal("promotion failure accepted")
			}
			after, err := os.ReadFile(file)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failed login changed prior config")
			}
			if token, err := keyring.Get("crawl-remote", priorAccount); err != nil || token != "prior-session" {
				t.Fatal("failed login changed prior session")
			}
			if !existingBound {
				if _, err := keyring.Get("crawl-remote", bound); !errors.Is(err, keyring.ErrNotFound) {
					t.Fatal("new owned keyring item retained after failure")
				}
			}
			entries, err := os.ReadDir(filepath.Dir(file))
			if err != nil || len(entries) != 1 {
				t.Fatal("staged config not cleaned up")
			}
			client, err := newRemoteClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Whoami(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRemoteLoginStageFailureDoesNotTouchSession(t *testing.T) {
	keyring.MockInit()
	file := filepath.Join(t.TempDir(), "reader.toml")
	cfg := config.Default()
	cfg.Remote = remote.Config{Mode: "cloud", Endpoint: "https://archive.example.test", Archive: "fixture"}
	account, err := remoteLoginKeyringAccount(cfg, file)
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.Set("crawl-remote", account, "prior-session"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(file); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(file)
	cfg.Remote.Mode = "invalid"
	if _, err := saveRemoteLogin(cfg, file, "replacement-session"); err == nil {
		t.Fatal("staging failure accepted")
	}
	after, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("staging changed prior config")
	}
	if token, err := keyring.Get("crawl-remote", account); err != nil || token != "prior-session" {
		t.Fatal("staging changed prior session")
	}
	entries, err := os.ReadDir(filepath.Dir(file))
	if err != nil || len(entries) != 1 {
		t.Fatal("failed stage was retained")
	}
}

func TestRemoteLoginAccountCanonicalBinding(t *testing.T) {
	cfg := config.Default()
	cfg.Remote = remote.Config{Endpoint: "https://ARCHIVE.example.test/api/", Archive: "fixture"}
	file := filepath.Join(t.TempDir(), "reader.toml")
	first, err := remoteLoginKeyringAccount(cfg, file)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Remote.Endpoint = "https://archive.example.test:443/api"
	same, err := remoteLoginKeyringAccount(cfg, file)
	if err != nil || first != same {
		t.Fatal("equivalent endpoint changed scope")
	}
	cfg.Remote.Archive = "other"
	other, err := remoteLoginKeyringAccount(cfg, file)
	if err != nil || first == other {
		t.Fatal("archive not bound")
	}
	cfg.Remote.Endpoint = "https://archive.example.test/api?secret=not-permitted"
	if _, err := remoteLoginKeyringAccount(cfg, file); err == nil {
		t.Fatal("ambiguous credential endpoint accepted")
	}
}
