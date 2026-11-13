package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/store"
)

// This fixture uses only the parent's CLI/store APIs. Observations precede
// the intended boundary so a baseline cannot confuse setup failure with loss
// of retained replies work.
func TestAPIRetainedThreadsFromCLI(t *testing.T) {
	const rootTS = "1710000001.000000"
	const oldLatest = "1710000200.000000"
	const oldOldest = "1709996600.000000"
	for _, name := range []string{"retained", "full-self", "hint-loss-restart", "unavailable-resume", "scope-skip-resume", "stored-tombstone", "since", "full-since", "current-page"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cfg, configPath := userPrimaryConfig(t)
			cfg.Sync.IncludeDMs = new(false)
			t.Setenv(cfg.Slack.Bot.TokenEnv, "fixture-bot")
			t.Setenv(cfg.Slack.User.TokenEnv, "fixture-user")
			if name == "unavailable-resume" {
				t.Setenv(cfg.Slack.User.TokenEnv, "fixture-invalid-user")
			}
			require.NoError(t, cfg.Save(configPath))
			configBefore, err := os.ReadFile(configPath)
			require.NoError(t, err)
			st, err := store.Open(cfg.DBPath)
			require.NoError(t, err)
			now := time.Unix(1710000000, 0).UTC()
			require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{ID: "T123", Name: "Fixture", RawJSON: "{}", UpdatedAt: now}))
			require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C123", WorkspaceID: "T123", Name: "fixture", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now}))
			parent := slack.Message{Msg: slack.Msg{Type: "message", Channel: "C123", Timestamp: rootTS, Text: "retained-parent", ReplyCount: 1}}
			if name == "stored-tombstone" {
				parent.SubType = "message_deleted"
			}
			if name == "full-self" {
				parent.ThreadTimestamp = rootTS
			}
			require.NoError(t, st.UpsertMessage(ctx, store.Message{ChannelID: "C123", WorkspaceID: "T123", TS: rootTS, Subtype: parent.SubType, ThreadTS: parent.ThreadTimestamp, Text: parent.Text, NormalizedText: parent.Text, ReplyCount: 1, SourceName: "api-bot", SourceRank: 2, RawJSON: store.MarshalRaw(parent), UpdatedAt: now}, nil))
			scope := ""
			if name == "since" || name == "full-since" {
				scope = oldOldest
			}
			coverageKey := retainedCLIKey("T123", "C123", scope)
			for _, source := range []string{"api-bot", "api-user"} {
				require.NoError(t, st.SetSyncState(ctx, source, "history_coverage_v1", coverageKey, fmt.Sprintf(`{"complete":true,"latest":%q}`, oldLatest)))
				require.NoError(t, st.SetSyncState(ctx, source, "workspace", "T123", "2020-01-01T00:00:00Z"))
			}
			if scope != "" || name == "stored-tombstone" {
				require.NoError(t, st.SetSyncState(ctx, "api-user", "thread_pending_v1", retainedCLIKey("T123", "C123", rootTS), "retained-generation"))
			}
			beforeWorkspace := apiKeyRows(t, st, "select * from sync_state where source_name='api-bot' and entity_type='workspace'")
			beforePending := apiKeyRows(t, st, "select * from sync_state where entity_type='thread_pending_v1'")
			require.NoError(t, st.Close())

			corrected := false
			calls := []retainedCLICall{}
			transport := cliRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				form := cliSlackForm(r)
				role := cliSlackToken(r)
				calls = append(calls, retainedCLICall{strings.TrimPrefix(r.URL.Path, "/"), role, form.Get("channel"), form.Get("ts"), form.Get("oldest"), form.Get("latest")})
				payload := map[string]any{"ok": true}
				switch r.URL.Path {
				case "/auth.test":
					if role == "fixture-invalid-user" {
						payload = map[string]any{"ok": false, "error": "invalid_auth"}
					} else {
						payload["team_id"], payload["team"] = "T123", "Fixture"
					}
				case "/conversations.list":
					payload["channels"] = []any{map[string]any{"id": "C123", "name": "fixture", "is_channel": true}}
				case "/users.list":
					payload["members"] = []any{}
				case "/conversations.history":
					payload["messages"] = []any{apiKeyMessage("1710000002.000000", "retained-history")}
					if name == "hint-loss-restart" || name == "unavailable-resume" {
						noHint := parent
						noHint.ReplyCount = 0
						payload["messages"] = []any{noHint, apiKeyMessage("1710000002.000000", "retained-history")}
					}
					if name == "scope-skip-resume" || name == "current-page" {
						payload["messages"] = []any{parent, apiKeyMessage("1710000002.000000", "retained-history")}
					}

				case "/conversations.replies":
					echo := parent
					echo.ReplyCount, echo.ThreadTimestamp = 0, rootTS
					reply := apiKeyMessage("1710000003.000000", "retained-child")
					reply["thread_ts"] = rootTS
					payload["messages"] = []any{echo, reply}
					if !corrected && (name == "hint-loss-restart" || name == "scope-skip-resume") {
						payload = map[string]any{"ok": false, "error": "synthetic_replies_failure"}
						if name == "scope-skip-resume" {
							payload["error"] = "missing_scope"
						}
					}
				default:
					return nil, fmt.Errorf("unexpected synthetic endpoint %s", r.URL.Path)
				}
				body, err := json.Marshal(payload)
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
			})
			var stdout, stderr bytes.Buffer
			app := &App{Stdout: &stdout, Stderr: &stderr, apiURL: "https://retained.invalid/", httpClient: &http.Client{Transport: transport}}
			args := []string{"--config", configPath, "--json", "sync", "--source", "api", "--workspace", "T123", "--with-media=false"}
			if scope != "" {
				args = append(args, "--since", scope)
			}
			if name == "full-self" || name == "full-since" {
				args = append(args, "--full")
			}
			runErr := app.Run(ctx, args)
			st, err = store.OpenReadOnly(cfg.DBPath)
			require.NoError(t, err)
			rows := apiKeyRows(t, st, "select ts,coalesce(deleted_ts,'') as deleted_ts,coalesce(subtype,'') as subtype,reply_count,source_name,source_rank from messages order by ts")
			pending := apiKeyRows(t, st, "select * from sync_state where entity_type='thread_pending_v1'")
			coverage := apiCompletenessCoverage(t, st, coverageKey)
			workspacePreserved := reflect.DeepEqual(beforeWorkspace, apiKeyRows(t, st, "select * from sync_state where source_name='api-bot' and entity_type='workspace'"))
			counts := apiCompletenessCounts(t, st)
			status, err := st.Status(ctx)
			require.NoError(t, err)
			require.NoError(t, st.Close())
			configAfter, err := os.ReadFile(configPath)
			require.NoError(t, err)
			replyCalls := 0
			for _, call := range calls {
				if call.Method == "conversations.replies" {
					replyCalls++
				}
			}
			wantReplies, wantPending, wantRows := 1, 0, 3
			wantError := ""
			switch name {
			case "hint-loss-restart":
				wantPending, wantRows, wantError = 1, 2, "sync workspace T123: slack conversations.replies API response failed"
				var native slack.SlackErrorResponse
				require.ErrorAs(t, runErr, &native)
				require.Equal(t, "synthetic_replies_failure", native.Err)
			case "unavailable-resume":
				wantReplies, wantPending, wantRows = 0, 1, 2
			case "scope-skip-resume":
				wantPending, wantRows = 1, 2
			case "stored-tombstone":
				wantReplies, wantRows = 0, 2
			case "since", "full-since":
				wantReplies, wantPending, wantRows = 0, 1, 2
			}
			deleted := len(rows) > 0 && (rows[0]["deleted_ts"] == rootTS || rows[0]["subtype"] == "message_deleted")
			boundary := userPrimaryError(runErr) == wantError && replyCalls == wantReplies && len(pending) == wantPending && len(rows) == wantRows && (name != "stored-tombstone" || deleted)
			observation := map[string]any{"error": userPrimaryError(runErr), "calls": calls, "rows": rows, "counts": counts, "coverage": coverage, "pending_count": len(pending), "pending_preserved": reflect.DeepEqual(beforePending, pending), "workspace_preserved": workspacePreserved, "config_preserved": bytes.Equal(configBefore, configAfter), "completion": stdout.Len() != 0, "thread_state": status.ThreadState}
			body, err := json.Marshal(observation)
			require.NoError(t, err)
			t.Logf("retained thread observation: %s", body)
			if !boundary {
				t.Fatalf("retained thread boundary: error=%q replies=%d pending=%d rows=%d deleted=%t", userPrimaryError(runErr), replyCalls, len(pending), len(rows), deleted)
			}
			require.Equal(t, wantError != "", coverage.Pending != nil)
			require.Equal(t, wantError != "", workspacePreserved)
			require.True(t, bytes.Equal(configBefore, configAfter))
			if scope != "" {
				require.Equal(t, beforePending, pending)
			}
			for _, call := range calls {
				switch call.Method {
				case "conversations.history":
					require.Equal(t, "fixture-bot", call.Role)
					require.Equal(t, "C123", call.Channel)
					require.Equal(t, map[bool]string{true: "", false: oldOldest}[name == "full-self"], call.Oldest)
					if wantError == "" {
						require.Equal(t, coverage.Latest, call.Latest)
					}
				case "conversations.replies":
					require.Equal(t, "fixture-user", call.Role)
					require.Equal(t, "C123", call.Channel)
					require.Equal(t, rootTS, call.TS)
				}
			}
			if wantPending > 0 && scope == "" {
				require.Equal(t, "partial", status.ThreadState)
				require.Equal(t, "api-user", pending[0]["source_name"])
				require.Equal(t, retainedCLIKey("T123", "C123", rootTS), pending[0]["entity_id"])
				require.NotEmpty(t, pending[0]["value"])
				// Close/reopen through a new CLI invocation, switching primary
				// source. The replies job must survive without borrowing bot coverage.
				corrected = true
				t.Setenv(cfg.Slack.Bot.TokenEnv, "")
				t.Setenv(cfg.Slack.User.TokenEnv, "fixture-user")
				calls, stdout, stderr = nil, bytes.Buffer{}, bytes.Buffer{}
				require.NoError(t, app.Run(ctx, args))
				st, err = store.OpenReadOnly(cfg.DBPath)
				require.NoError(t, err)
				require.Empty(t, apiKeyRows(t, st, "select * from sync_state where entity_type='thread_pending_v1'"))
				require.Empty(t, apiKeyRows(t, st, "select * from sync_state where entity_type='thread_skip'"))
				require.Len(t, apiKeyRows(t, st, "select * from messages"), 3)
				require.Equal(t, coverage, apiCompletenessCoverage(t, st, coverageKey))
				require.Len(t, calls, 5)
				require.NotEmpty(t, calls[2].Latest)
				require.Equal(t, []retainedCLICall{
					{Method: "auth.test", Role: "fixture-user"},
					{Method: "conversations.list", Role: "fixture-user"},
					{Method: "conversations.history", Role: "fixture-user", Channel: "C123", Oldest: oldOldest, Latest: calls[2].Latest},
					{Method: "conversations.replies", Role: "fixture-user", Channel: "C123", TS: rootTS},
					{Method: "users.list", Role: "fixture-user"},
				}, calls)
				require.Equal(t, []map[string]any{{"workspace_id": "T123", "channel_id": "C123", "ts": "1710000003.000000", "thread_ts": rootTS, "source_name": "api-user", "source_rank": int64(1)}},
					apiKeyRows(t, st, "select workspace_id,channel_id,ts,thread_ts,source_name,source_rank from messages where ts='1710000003.000000'"))
				require.NoError(t, st.Close())
			}
		})
	}
}

type retainedCLICall struct {
	Method, Role, Channel, TS, Oldest, Latest string
}

func retainedCLIKey(parts ...string) string {
	body, _ := json.Marshal(parts)
	return string(body)
}
