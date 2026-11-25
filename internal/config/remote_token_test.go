package config

import (
	"errors"
	"testing"

	"github.com/openclaw/crawlkit/remote"
	"github.com/zalando/go-keyring"
)

func TestRemoteTokenSources(t *testing.T) {
	keyring.MockInit()
	cfg := Default()
	cfg.Remote = remote.Config{Mode: "cloud", Endpoint: "https://archive.example.test", Archive: "fixture", TokenEnv: "SLACRAWL_TEST_REMOTE_TOKEN"}
	t.Setenv(cfg.Remote.TokenEnv, "environment-session")
	auth, err := StoreRemoteToken(cfg, "keyring-session")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Remote.Auth = auth
	got, err := ResolveRemoteToken(cfg)
	if err != nil || got.Token != "keyring-session" || got.Source != "keyring" {
		t.Fatal("explicit keyring did not win")
	}
	cfg.Remote.Auth.TokenSource = "env"
	got, err = ResolveRemoteToken(cfg)
	if err != nil || got.Token != "environment-session" {
		t.Fatal("env token missing")
	}
	t.Setenv(cfg.Remote.TokenEnv, "")
	got, err = ResolveRemoteToken(cfg)
	if err != nil || got.Token != "keyring-session" {
		t.Fatal("configured keyring fallback missing")
	}
	cfg.Remote.Auth.TokenSource = "none"
	if _, err = ResolveRemoteToken(cfg); !errors.Is(err, remote.ErrMissingToken) {
		t.Fatal("none source accepted")
	}
	cfg.Remote.Auth.TokenSource = "unknown"
	if err = cfg.Normalize(); err == nil {
		t.Fatal("unknown token source accepted")
	}
}
