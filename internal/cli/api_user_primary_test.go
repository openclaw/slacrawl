package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
)

// Only existing parent APIs are used here. Bot-bearing Doctor cases live in a
// separate candidate-only file: the parent Doctor ignores the App HTTP seam.
func TestAPIUserPrimaryFromCLI(t *testing.T) {
	cases := []struct {
		name, source, bot, user, policy, wantError string
		fanout, tail                               bool
	}{
		{name: "user/api/omitted", source: "api", user: "user", policy: "omitted"},
		{name: "user/api/false", source: "api", user: "user", policy: "false"},
		{name: "user/api/true", source: "api", user: "user", policy: "true"},
		{name: "user/bot/false", source: "bot", user: "user", policy: "false"},
		{name: "user/all/false", source: "all", user: "user", policy: "false"},
		{name: "user/fanout", source: "api", policy: "false", fanout: true},
		{name: "bot/only", source: "api", bot: "bot", policy: "false"},
		{name: "bot/user", source: "api", bot: "bot", user: "user", policy: "false"},
		{name: "bot/invalid-user", source: "api", bot: "bot", user: "invalid-user", policy: "false"},
		{name: "invalid-bot/user", source: "api", bot: "invalid-bot", user: "user", policy: "false", wantError: "sync workspace T123: invalid_auth"},
		{name: "bot/wrong-user", source: "api", bot: "bot", user: "wrong-user", policy: "false", wantError: "sync workspace T123: user token: authenticated workspace T999 does not match requested workspace T123"},
		{name: "invalid-user/only", source: "api", user: "invalid-user", policy: "false", wantError: "sync workspace T123: invalid_auth"},
		{name: "wrong-user/only", source: "api", user: "wrong-user", policy: "false", wantError: "sync workspace T123: authenticated workspace T999 does not match requested workspace T123"},
		{name: "no-native-token", source: "api", policy: "false", wantError: "sync workspace T123: SLACK_BOT_TOKEN or SLACK_USER_TOKEN is required for api sync"},
		{name: "tail/user-app", user: "user", policy: "false", tail: true, wantError: "SLACK_BOT_TOKEN is required for tail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cfg, configPath := userPrimaryConfig(t)
			cfg.WorkspaceID = "T123"
			if tc.policy != "omitted" {
				cfg.Sync.IncludeDMs = new(tc.policy == "true")
			}
			t.Setenv(cfg.Slack.Bot.TokenEnv, tc.bot)
			t.Setenv(cfg.Slack.User.TokenEnv, tc.user)
			teams := []string{"T123"}
			if tc.fanout {
				cfg.WorkspaceID = ""
				teams = []string{"T1", "T2"}
				cfg.Workspaces = []config.Workspace{
					{ID: "T1", UserTokenEnv: "SLACRAWL_PRIMARY_T1_USER"},
					{ID: "T2", UserTokenEnv: "SLACRAWL_PRIMARY_T2_USER"},
				}
				for _, team := range teams {
					t.Setenv("SLACRAWL_PRIMARY_"+team+"_USER", "user-"+team)
					// Explicitly isolate the existing conventional fallback as well.
					for _, kind := range []string{"BOT", "APP", "USER"} {
						t.Setenv("SLACK_"+team+"_"+kind+"_TOKEN", "")
					}
				}
			}
			if tc.tail {
				t.Setenv(cfg.Slack.App.TokenEnv, "app")
			}
			require.NoError(t, cfg.Save(configPath))
			userPrimarySeed(t, cfg.DBPath, teams)
			before := shareArchiveSnapshot(t, cfg.DBPath)
			configBefore, err := os.ReadFile(configPath)
			require.NoError(t, err)
			fixture := &userPrimaryHTTP{}
			var stdout, stderr bytes.Buffer
			app := &App{Stdout: &stdout, Stderr: &stderr, apiURL: "https://fixture.invalid/", httpClient: &http.Client{Transport: fixture}}
			args := []string{"--config", configPath, "--json", "sync", "--source", tc.source, "--with-media=false"}
			if tc.tail {
				args = []string{"--config", configPath, "--json", "tail"}
			}
			runErr := app.Run(ctx, args)
			after := shareArchiveSnapshot(t, cfg.DBPath)
			configAfter, err := os.ReadFile(configPath)
			require.NoError(t, err)
			calls, problems := fixture.snapshot()
			gotError := userPrimaryError(runErr)
			preserved := reflect.DeepEqual(before, after)
			counts := userPrimaryCounts(after)
			observation, err := json.Marshal(map[string]any{
				"error": gotError, "requests": calls, "counts": counts,
				"archive_preserved": preserved, "config_preserved": bytes.Equal(configBefore, configAfter),
				"completion": stdout.Len() > 0,
			})
			require.NoError(t, err)
			t.Logf("user primary observation: %s", observation)
			require.Empty(t, problems, "synthetic transport contract")
			if gotError != tc.wantError {
				t.Fatalf("user primary boundary: error=%q expected=%q requests=%d messages=%d archive_preserved=%t", gotError, tc.wantError, len(calls), counts["messages"], preserved)
			}
			require.True(t, bytes.Equal(configBefore, configAfter))
			if tc.wantError != "" {
				require.True(t, preserved)
				require.Empty(t, stdout.String())
				wantRoles := []string{}
				switch tc.name {
				case "invalid-bot/user":
					wantRoles = []string{"invalid-bot"}
				case "bot/wrong-user":
					wantRoles = []string{"bot", "wrong-user"}
				case "invalid-user/only", "wrong-user/only":
					wantRoles = []string{tc.user}
				}
				require.Equal(t, userPrimaryAuthCalls(wantRoles), calls)
				return
			}
			primary := "user"
			if tc.bot != "" {
				primary = "bot"
			}
			hasUserReplies := tc.user == "user" || tc.fanout
			includeDMs := tc.policy != "false" && hasUserReplies
			require.NotEmpty(t, stdout.String())
			var report map[string]any
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
			roleTeams := map[string]string{}
			for _, team := range teams {
				if tc.fanout {
					roleTeams["user-"+team] = team
				} else {
					roleTeams[primary] = team
					if hasUserReplies {
						roleTeams["user"] = team
					}
				}
			}
			for _, team := range teams {
				role := primary
				if tc.fanout {
					role = "user-" + team
				}
				replyRole := ""
				if hasUserReplies {
					replyRole = "user"
					if tc.fanout {
						replyRole = role
					}
				} else if tc.user == "" {
					replyRole = "bot"
				}
				userPrimaryAssertTeam(t, cfg.DBPath, before, after, team, primary, role, replyRole, includeDMs, roleTeams, calls)
			}
			wantAuth := []string{primary}
			if tc.bot != "" && tc.user != "" {
				wantAuth = append(wantAuth, tc.user)
			}
			if tc.fanout {
				wantAuth = []string{"user-T1", "user-T2"}
			}
			var auth []userPrimaryCall
			for _, call := range calls {
				if call.Method == "auth.test" {
					auth = append(auth, call)
				}
			}
			require.Equal(t, userPrimaryAuthCalls(wantAuth), auth)
			for table, rows := range before {
				for _, row := range rows {
					if row["workspace_id"] == "TKEEP" || row["id"] == "TKEEP" || row["channel_id"] == "CKEEP" {
						require.Contains(t, after[table], row, table)
					}
				}
			}
		})
	}
	t.Run("doctor/user-app", userPrimaryDoctorBaseline)
	t.Run("doctor/channel-skips", userPrimarySkipBaseline)
}

type userPrimaryCall struct {
	Method string     `json:"method"`
	Role   string     `json:"role"`
	Form   url.Values `json:"form"`
}

type userPrimaryHTTP struct {
	mu       sync.Mutex
	calls    []userPrimaryCall
	problems []string
}

func (f *userPrimaryHTTP) snapshot() ([]userPrimaryCall, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]userPrimaryCall{}, f.calls...), append([]string{}, f.problems...)
}

func (f *userPrimaryHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	role := cliSlackToken(r)
	form := r.Form
	form.Del("token")
	method := strings.TrimPrefix(r.URL.Path, "/")
	f.calls = append(f.calls, userPrimaryCall{method, role, form})
	payload := map[string]any{"ok": true}
	team := "T123"
	if strings.HasPrefix(role, "user-T") {
		team = strings.TrimPrefix(role, "user-")
	} else if role == "wrong-user" {
		team = "T999"
	}
	if role == "" || strings.HasPrefix(role, "invalid-") {
		payload = map[string]any{"ok": false, "error": "invalid_auth"}
	} else {
		switch method {
		case "auth.test":
			payload["team_id"], payload["team"] = team, "Fixture "+team
		case "conversations.list":
			switch form.Get("types") {
			case "public_channel,private_channel":
				id, private := team+"C", false
				if form.Get("cursor") == "" {
					payload["response_metadata"] = map[string]any{"next_cursor": "channels-next"}
				} else if form.Get("cursor") == "channels-next" {
					id, private = team+"G", true
				} else {
					f.problems = append(f.problems, "unexpected catalog cursor")
				}
				payload["channels"] = []any{map[string]any{"id": id, "name": "fixture-" + id, "is_channel": true, "is_private": private}}
			case "im,mpim":
				payload["channels"] = []any{map[string]any{"id": team + "D", "is_im": true, "user": "U" + team}, map[string]any{"id": team + "M", "is_mpim": true, "members": []string{"U" + team}}}
			default:
				f.problems = append(f.problems, "unexpected catalog types")
			}
		case "users.list":
			id := "U" + team
			if form.Get("cursor") == "" {
				payload["response_metadata"] = map[string]any{"next_cursor": "users-next"}
			} else if form.Get("cursor") == "users-next" {
				id += "2"
			} else {
				f.problems = append(f.problems, "unexpected users cursor")
			}
			payload["members"] = []any{map[string]any{"id": id, "name": "fixture-" + id, "profile": map[string]any{"display_name": "Fixture"}}}
		case "conversations.history":
			channel := form.Get("channel")
			if channel == team+"C" {
				payload["messages"] = []any{userPrimaryMessage(team, "1", true), userPrimaryMessage(team, "2", false)}
			} else {
				digit := map[string]string{team + "G": "4", team + "D": "5", team + "M": "6"}[channel]
				if digit == "" {
					f.problems = append(f.problems, "unexpected history owner")
				}
				payload["messages"] = []any{userPrimaryMessage(team, digit, false)}
			}
		case "conversations.replies":
			if form.Get("channel") != team+"C" || form.Get("ts") != "1710000001.000000" {
				f.problems = append(f.problems, "unexpected thread owner")
			}
			reply := userPrimaryMessage(team, "3", false)
			reply["thread_ts"] = "1710000001.000000"
			payload["messages"] = []any{userPrimaryMessage(team, "1", true), reply}
		default:
			f.problems = append(f.problems, "unexpected method "+method)
			payload = map[string]any{"ok": false, "error": "unexpected_fixture_method"}
		}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(data)), Request: r}, nil
}

func userPrimaryMessage(team, digit string, root bool) map[string]any {
	msg := map[string]any{
		"type": "message", "ts": "171000000" + digit + ".000000", "user": "U" + team,
		"text":  "user-primary-" + team + "-" + digit + " <@U" + team + ">",
		"files": []any{map[string]any{"id": "F" + team + digit, "name": "user-primary-file-" + digit, "mimetype": "text/plain"}},
	}
	if root {
		msg["thread_ts"], msg["reply_count"] = msg["ts"], 1
	}
	return msg
}

func userPrimaryConfig(t *testing.T) (config.Config, string) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	cfg.DBPath, cfg.CacheDir, cfg.LogDir = filepath.Join(root, "archive.db"), filepath.Join(root, "cache"), filepath.Join(root, "logs")
	cfg.Share.Remote, cfg.Share.RepoPath, cfg.Share.AutoUpdate = "", filepath.Join(root, "share"), false
	cfg.Slack.Desktop.Enabled, cfg.Slack.Desktop.Path = false, filepath.Join(root, "desktop")
	cfg.Slack.MCP.Enabled, cfg.Slack.MCP.AuthPath = false, filepath.Join(root, "mcp-auth.json")
	cfg.Sync.Concurrency, cfg.Sync.FileMedia = 1, new(false)
	cfg.Slack.Bot.TokenEnv, cfg.Slack.User.TokenEnv, cfg.Slack.App.TokenEnv = "SLACRAWL_PRIMARY_BOT", "SLACRAWL_PRIMARY_USER", "SLACRAWL_PRIMARY_APP"
	for _, name := range []string{cfg.Slack.Bot.TokenEnv, cfg.Slack.User.TokenEnv, cfg.Slack.App.TokenEnv} {
		t.Setenv(name, "")
	}
	t.Setenv("SLACRAWL_NO_UPDATE_CHECK", "1")
	return cfg, filepath.Join(root, "config.toml")
}

func userPrimarySeed(t *testing.T, path string, teams []string) {
	t.Helper()
	st, err := store.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()
	require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{ID: "TKEEP", Name: "retained", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "CKEEP", WorkspaceID: "TKEEP", Name: "retained", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertMessage(ctx, store.Message{ChannelID: "CKEEP", WorkspaceID: "TKEEP", TS: "1700000000.000000", Text: "retained", NormalizedText: "retained", RawJSON: "{}", SourceName: "api-bot", SourceRank: 2, UpdatedAt: now}, nil))
	for _, team := range teams {
		for _, source := range []string{"api-bot", "api-user"} {
			_, err := st.DB().ExecContext(ctx, "insert into sync_state(source_name,entity_type,entity_id,value,updated_at) values(?,?,?,?,?)", source, "workspace", team, "2020-01-01T00:00:00Z", "2020-01-01T00:00:00Z")
			require.NoError(t, err)
		}
	}
}

func userPrimaryAuthCalls(roles []string) []userPrimaryCall {
	calls := []userPrimaryCall{}
	for _, role := range roles {
		calls = append(calls, userPrimaryCall{"auth.test", role, url.Values{}})
	}
	return calls
}

func userPrimaryError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func userPrimaryCounts(snapshot map[string][]map[string]any) map[string]int {
	counts := map[string]int{}
	for table, rows := range snapshot {
		counts[table] = len(rows)
	}
	return counts
}

func userPrimaryAssertTeam(t *testing.T, path string, before, after map[string][]map[string]any, team, primary, role, replyRole string, dms bool, roleTeams map[string]string, calls []userPrimaryCall) {
	t.Helper()
	replies := replyRole != ""
	userReplies := replies && replyRole != "bot"
	wantMessages := 3
	if replies {
		wantMessages++
	}
	if dms {
		wantMessages += 2
	}
	st, err := store.OpenReadOnly(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	rows, err := st.QueryReadOnly(context.Background(), fmt.Sprintf("select * from messages where workspace_id='%s' order by ts", team))
	require.NoError(t, err)
	require.Len(t, rows, wantMessages)
	wantKinds := map[string]string{team + "C": "public_channel", team + "G": "private_channel"}
	if dms {
		wantKinds[team+"D"], wantKinds[team+"M"] = "im", "mpim"
	}
	gotKinds := map[string]string{}
	for _, row := range after["channels"] {
		if row["workspace_id"] == team {
			gotKinds[row["id"].(string)] = row["kind"].(string)
		}
	}
	require.Equal(t, wantKinds, gotKinds)
	for _, row := range rows {
		source, rank := "api-"+primary, int64(2)
		if primary == "user" || (userReplies && (row["ts"] == "1710000001.000000" || row["ts"] == "1710000003.000000")) || row["channel_id"] == team+"D" || row["channel_id"] == team+"M" {
			source, rank = "api-user", 1
		}
		require.Equal(t, source, row["source_name"])
		require.Equal(t, rank, row["source_rank"])
		require.Contains(t, row["text"], "user-primary-"+team)
		require.Contains(t, row["raw_json"], "user-primary-file-")
	}
	for _, table := range []string{"message_files", "message_mentions", "message_fts"} {
		count := 0
		for _, row := range after[table] {
			if strings.HasPrefix(fmt.Sprint(row["channel_id"]), team) || (table == "message_fts" && strings.HasPrefix(fmt.Sprint(row["message_key"]), team)) {
				count++
			}
		}
		require.Equal(t, wantMessages, count, table)
	}
	for _, table := range []string{"message_events", "message_event_heads"} {
		count := 0
		bySource := map[string]int{}
		for _, row := range after[table] {
			if strings.HasPrefix(fmt.Sprint(row["channel_id"]), team) {
				count++
				bySource[row["source_name"].(string)]++
			}
		}
		want := wantMessages
		if primary == "bot" && userReplies {
			want++ // The same native parent is also observed by the user replies source.
		}
		require.Equal(t, want, count, table)
		wantSources := map[string]int{"api-" + primary: wantMessages}
		if primary == "bot" && userReplies {
			wantSources = map[string]int{"api-bot": 3, "api-user": 2}
		}
		require.Equal(t, wantSources, bySource, table)
	}
	users, err := st.QueryReadOnly(context.Background(), fmt.Sprintf("select id from users where workspace_id='%s' order by id", team))
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"id": "U" + team}, {"id": "U" + team + "2"}}, users)
	var horizon string
	var catalog, profiles, history, thread, dmCatalog, dmHistory int
	userRole := "user"
	if primary == "user" {
		userRole = role
	}
	for _, call := range calls {
		if call.Method == "auth.test" {
			continue
		}
		callTeam, knownRole := roleTeams[call.Role]
		require.True(t, knownRole, "unexpected data role %q", call.Role)
		if callTeam != team {
			// Another configured fanout workspace is checked by its own iteration.
			continue
		}
		switch call.Method {
		case "conversations.list":
			switch call.Form.Get("types") {
			case "public_channel,private_channel":
				require.Equal(t, role, call.Role)
				catalog++
			case "im,mpim":
				require.Equal(t, userRole, call.Role)
				dmCatalog++
			default:
				t.Fatalf("unexpected catalog types %q", call.Form.Get("types"))
			}
			require.Equal(t, team, call.Form.Get("team_id"))
		case "users.list":
			require.Equal(t, role, call.Role)
			profiles++
		case "conversations.history":
			switch call.Form.Get("channel") {
			case team + "C", team + "G":
				require.Equal(t, role, call.Role)
				history++
			case team + "D", team + "M":
				require.True(t, dms, "DM history must not be acquired when excluded")
				require.Equal(t, userRole, call.Role)
				dmHistory++
			default:
				t.Fatalf("unexpected history channel %q", call.Form.Get("channel"))
			}
			if horizon == "" {
				horizon = call.Form.Get("latest")
			}
			require.Equal(t, horizon, call.Form.Get("latest"))
		case "conversations.replies":
			require.Equal(t, replyRole, call.Role)
			require.Equal(t, team+"C", call.Form.Get("channel"))
			require.Equal(t, "1710000001.000000", call.Form.Get("ts"))
			thread++
		default:
			t.Fatalf("unexpected data request %s", call.Method)
		}
	}
	require.Equal(t, 2, catalog)
	require.Equal(t, 2, profiles)
	require.Equal(t, 2, history)
	require.Equal(t, map[bool]int{false: 0, true: 1}[dms], dmCatalog)
	require.Equal(t, map[bool]int{false: 0, true: 2}[dms], dmHistory)
	require.Equal(t, map[bool]int{false: 0, true: 1}[replies], thread)
	require.NotEmpty(t, horizon)
	for _, channel := range []string{team + "C", team + "G"} {
		key := fmt.Sprintf(`["%s","%s",""]`, team, channel)
		value, err := st.GetSyncState(context.Background(), "api-"+primary, "history_coverage_v1", key)
		require.NoError(t, err)
		var coverage map[string]any
		require.NoError(t, json.Unmarshal([]byte(value), &coverage))
		require.Equal(t, true, coverage["complete"])
		require.Equal(t, horizon, coverage["latest"])
		require.Nil(t, coverage["pending"])
	}
	for _, old := range before["sync_state"] {
		if old["entity_id"] != team {
			continue
		}
		if old["source_name"] != "api-"+primary {
			require.Contains(t, after["sync_state"], old)
		} else {
			value, err := st.GetSyncState(context.Background(), "api-"+primary, "workspace", team)
			require.NoError(t, err)
			require.NotEqual(t, old["value"], value)
			_, err = time.Parse(time.RFC3339, value)
			require.NoError(t, err)
		}
	}
}

func userPrimaryDoctorBaseline(t *testing.T) {
	cfg, configPath := userPrimaryConfig(t)
	cfg.Sync.IncludeDMs = new(false)
	t.Setenv(cfg.Slack.User.TokenEnv, "user")
	t.Setenv(cfg.Slack.App.TokenEnv, "app")
	require.NoError(t, cfg.Save(configPath))
	fixture := &userPrimaryHTTP{}
	var output bytes.Buffer
	app := &App{Stdout: &output, Stderr: &output, apiURL: "https://fixture.invalid/", httpClient: &http.Client{Transport: fixture}}
	runErr := app.Run(context.Background(), []string{"--config", configPath, "--json", "doctor"})
	var report map[string]any
	decodeErr := json.Unmarshal(output.Bytes(), &report)
	output.Reset()
	textErr := app.Run(context.Background(), []string{"--config", configPath, "doctor"})
	calls, problems := fixture.snapshot()
	diag, _ := report["slack_api"].(map[string]any)
	missingBot := strings.Contains(output.String(), "bot token missing")
	t.Logf("user primary doctor observation: error=%t text_error=%t requests=%d user_available=%t global=%v top=%v tail=%v missing_bot_reason=%t db_exists=%t", runErr != nil, textErr != nil, len(calls), diag["user_auth_available"] == true, diag["thread_coverage"], report["thread_coverage"], diag["app_tail_available"], missingBot, sharePathExists(t, cfg.DBPath))
	require.Empty(t, problems)
	if runErr != nil || textErr != nil || diag["user_auth_available"] != true || !missingBot {
		t.Fatalf("user primary doctor boundary: error=%t requests=%d user_available=%t missing_bot_reason=%t", runErr != nil, len(calls), diag["user_auth_available"] == true, missingBot)
	}
	require.NoError(t, decodeErr)
	require.Equal(t, userPrimaryAuthCalls([]string{"user", "user"}), calls)
	require.Equal(t, false, diag["bot_configured"])
	require.Nil(t, diag["bot_auth_team_id"])
	require.Equal(t, true, diag["app_configured"])
	require.Equal(t, false, diag["app_tail_available"])
	require.Equal(t, "full", diag["thread_coverage"])
	require.Equal(t, "full", report["thread_coverage"])
	require.False(t, sharePathExists(t, cfg.DBPath))
}

func userPrimarySkipBaseline(t *testing.T) {
	cfg, configPath := userPrimaryConfig(t)
	require.NoError(t, cfg.Save(configPath))
	st, err := store.Open(cfg.DBPath)
	require.NoError(t, err)
	var want []map[string]any
	for i := 0; i < 24; i++ {
		source := []string{"api-bot", "api-user"}[i%2]
		id := fmt.Sprintf("C%02d", i)
		updated := fmt.Sprintf("2026-01-%02dT00:00:00Z", 1+i/2)
		_, err = st.DB().ExecContext(context.Background(), "insert into sync_state values(?,?,?,?,?)", source, "channel_skip", id, "fixture skip", updated)
		require.NoError(t, err)
		want = append(want, map[string]any{"source_name": source, "entity_type": "channel_skip", "entity_id": id, "value": "fixture skip"})
	}
	for _, pair := range [][2]string{{"desktop", "channel_skip"}, {"mcp", "channel_skip"}, {"api-user", "thread_skip"}, {"api-bot", "workspace"}} {
		require.NoError(t, st.SetSyncState(context.Background(), pair[0], pair[1], "other", "other"))
	}
	require.NoError(t, st.Close())
	before := shareArchiveSnapshot(t, cfg.DBPath)
	// Descending timestamp pairs, ascending IDs within each equal-time pair.
	ordered := make([]map[string]any, 0, len(want))
	for i := len(want) - 2; i >= 0; i -= 2 {
		ordered = append(ordered, want[i], want[i+1])
	}
	want = ordered
	want = want[:20]
	fixture := &userPrimaryHTTP{}
	var output bytes.Buffer
	app := &App{Stdout: &output, Stderr: &output, apiURL: "https://fixture.invalid/", httpClient: &http.Client{Transport: fixture}}
	runErr := app.Run(context.Background(), []string{"--config", configPath, "--json", "doctor"})
	var report map[string]any
	decodeErr := json.Unmarshal(output.Bytes(), &report)
	got, _ := json.Marshal(report["api_channel_skips"])
	wantJSON, err := json.Marshal(want)
	require.NoError(t, err)
	calls, problems := fixture.snapshot()
	preserved := reflect.DeepEqual(before, shareArchiveSnapshot(t, cfg.DBPath))
	t.Logf("user primary skip observation: error=%t requests=%d skips=%s archive_preserved=%t", runErr != nil, len(calls), got, preserved)
	require.Empty(t, problems)
	if runErr != nil || !bytes.Equal(wantJSON, got) {
		t.Fatalf("user primary skip boundary: error=%t combined_order=%t archive_preserved=%t", runErr != nil, bytes.Equal(wantJSON, got), preserved)
	}
	require.NoError(t, decodeErr)
	require.Empty(t, calls)
	require.True(t, preserved)
	// Keep the opened-but-empty array contract separate from the bounded rows.
	empty := filepath.Join(filepath.Dir(cfg.DBPath), "empty.db")
	st, err = store.Open(empty)
	require.NoError(t, err)
	require.NoError(t, st.Close())
	cfg.DBPath = empty
	require.NoError(t, cfg.Save(configPath))
	output.Reset()
	require.NoError(t, app.Run(context.Background(), []string{"--config", configPath, "--json", "doctor"}))
	require.NoError(t, json.Unmarshal(output.Bytes(), &report))
	require.Equal(t, []any{}, report["api_channel_skips"])
}

func TestNativeAuthFailureFromCLILeavesArchiveUnchanged(t *testing.T) {
	for _, source := range []string{"api", "bot"} {
		for _, primary := range []string{"bot", "user"} {
			t.Run(source+"/"+primary, func(t *testing.T) {
				cfg, configPath := userPrimaryConfig(t)
				cfg.WorkspaceID = "T123"
				cfg.Sync.IncludeDMs = new(false)
				t.Setenv(cfg.Slack.User.TokenEnv, "fixture-user")
				if primary == "bot" {
					t.Setenv(cfg.Slack.Bot.TokenEnv, "fixture-bot")
				}
				require.NoError(t, cfg.Save(configPath))
				userPrimarySeed(t, cfg.DBPath, []string{"T123"})
				before := shareArchiveSnapshot(t, cfg.DBPath)
				configBefore, err := os.ReadFile(configPath)
				require.NoError(t, err)
				var calls []string
				var stdout, stderr bytes.Buffer
				app := &App{Stdout: &stdout, Stderr: &stderr, apiURL: "https://fixture.invalid/", httpClient: &http.Client{Transport: cliRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					require.NoError(t, r.ParseForm())
					calls = append(calls, r.URL.Path+":"+r.Form.Get("token"))
					require.Equal(t, "/auth.test", r.URL.Path)
					require.Equal(t, url.Values{"token": {"fixture-" + primary}}, r.Form)
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":false,"team_id":"T123","team":"cli-auth-canary","user_id":"U123"}`)), Request: r}, nil
				})}}
				err = app.Run(context.Background(), []string{"--config", configPath, "--json", "sync", "--source", source, "--with-media=false"})
				require.EqualError(t, err, "sync workspace T123: auth.test response did not report success")
				require.Equal(t, []string{"/auth.test:fixture-" + primary}, calls, "failed bot auth must not fall back to the configured user")
				require.Empty(t, stdout.String())
				require.NotContains(t, stderr.String(), "cli-auth-canary")
				require.Equal(t, before, shareArchiveSnapshot(t, cfg.DBPath))
				configAfter, readErr := os.ReadFile(configPath)
				require.NoError(t, readErr)
				require.Equal(t, configBefore, configAfter)
			})
		}
	}
}
