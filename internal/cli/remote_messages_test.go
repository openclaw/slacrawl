package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
)

func TestMessagesPostgresHTTP(t *testing.T) {
	origin := os.Getenv("CRAWL_TEST_SLACK_READER_ORIGIN")
	if origin == "" {
		t.Skip("requires actual PostgreSQL reader fixture")
	}
	u, e := url.Parse(origin)
	if e != nil || u.Scheme != "http" || !net.ParseIP(u.Hostname()).IsLoopback() {
		t.Fatal("fixture must use loopback")
	}
	ctx := context.Background()
	local, e := store.Open(filepath.Join(t.TempDir(), "expected.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer local.Close()
	now := time.Now().UTC()
	for _, err := range []error{local.UpsertWorkspace(ctx, store.Workspace{ID: "T1", Name: "workspace\x00雪", RawJSON: "{}", UpdatedAt: now}), local.UpsertChannel(ctx, store.Channel{ID: "C1", WorkspaceID: "T1", Name: "channel", Kind: "private", RawJSON: "{}", UpdatedAt: now}), local.UpsertUser(ctx, store.User{ID: "U1", WorkspaceID: "T1", Name: "user\x00name", RawJSON: "{}", UpdatedAt: now})} {
		if err != nil {
			t.Fatal(err)
		}
	}
	root := store.Message{ChannelID: "C1", TS: "1700000000.123456789", WorkspaceID: "T1", UserID: "U1", Text: "message\x00body", NormalizedText: "hello <@U1>\x00", ReplyCount: 1, SourceName: "fixture", RawJSON: "{}", UpdatedAt: now}
	reply := store.Message{ChannelID: "C1", TS: "1700000001.000001", WorkspaceID: "T1", Text: "reply\x00雪", NormalizedText: "reply\x00雪", ThreadTS: root.TS, SourceName: "fixture", RawJSON: "{}", UpdatedAt: now}
	if e = local.UpsertMessage(ctx, root, []store.Mention{{Type: "user", TargetID: "U1", DisplayText: "mention\x00"}}); e != nil {
		t.Fatal(e)
	}
	if e = local.UpsertMessage(ctx, reply, nil); e != nil {
		t.Fatal(e)
	}
	expected, e := local.Messages(ctx, "T1", "C1", "", 10)
	if e != nil {
		t.Fatal(e)
	}
	cfg := config.Default()
	cfg.DBPath = filepath.Join(t.TempDir(), "absent", "forbidden.sqlite")
	cfg.Remote = remote.Config{Mode: remote.ModeCloud, Endpoint: origin, Archive: "message-reader", TokenEnv: "SLACK_READ_FIXTURE_TOKEN"}
	file := filepath.Join(t.TempDir(), "reader.toml")
	if e = cfg.Save(file); e != nil {
		t.Fatal(e)
	}
	t.Setenv("SLACK_READ_FIXTURE_TOKEN", "fixture-reader")
	for _, extra := range [][]string{nil, {"--thread", root.TS}} {
		var out bytes.Buffer
		app := New()
		app.Stdout = &out
		app.Stderr = io.Discard
		args := append([]string{"--config", file, "--json", "messages", "--workspace", "T1", "--channel", "C1", "--limit", "10"}, extra...)
		if e = app.Run(ctx, args); e != nil {
			t.Fatal(e)
		}
		var actual []store.MessageRow
		if e = json.Unmarshal(out.Bytes(), &actual); e != nil {
			t.Fatal(e)
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("PostgreSQL/native rows differ\nactual %#v\nexpected %#v", actual, expected)
		}
	}
	for _, command := range []string{"search", "tui", "status", "files", "sync", "sql", "users", "channels"} {
		app := New()
		app.Stdout = io.Discard
		app.Stderr = io.Discard
		if e = app.Run(ctx, []string{"--config", file, command}); e == nil {
			t.Fatalf("cloud %s fell back", command)
		}
	}
	t.Setenv("SLACK_READ_FIXTURE_TOKEN", "wrong")
	app := New()
	app.Stdout = io.Discard
	app.Stderr = io.Discard
	if e = app.Run(ctx, []string{"--config", file, "messages"}); e == nil {
		t.Fatal("wrong reader accepted")
	}
	if _, e = os.Stat(filepath.Dir(cfg.DBPath)); !os.IsNotExist(e) {
		t.Fatal("cloud opened SQLite", e)
	}
}
