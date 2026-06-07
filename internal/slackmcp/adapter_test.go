package slackmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/mcpclient"
	"github.com/openclaw/slacrawl/internal/store"
)

const testConnectorID = "asdk_app_test"

func TestSyncStoresMCPConnectorData(t *testing.T) {
	server := newTestGatewayServer(t)
	defer server.Close()
	t.Setenv("CODEX_APPS_ACCESS_TOKEN", "test-token")
	t.Setenv("CODEX_APPS_ACCOUNT_ID", "acct-123")

	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	summary, err := Sync(context.Background(), st, Options{
		WorkspaceID: "T123",
		Config: config.MCPConfig{
			Enabled:         true,
			BaseURL:         server.URL,
			AuthPath:        "/unused",
			TokenEnv:        "CODEX_APPS_ACCESS_TOKEN",
			AccountIDEnv:    "CODEX_APPS_ACCOUNT_ID",
			ConnectorID:     testConnectorID,
			ChannelTypes:    "public_channel,private_channel",
			PageSize:        100,
			SearchLimit:     100,
			MaxPages:        10,
			ProtocolVersion: "2025-03-26",
		},
	})
	require.NoError(t, err)
	require.Equal(t, 1, summary.Users)
	require.Equal(t, 1, summary.Channels)
	require.Equal(t, 1, summary.Messages)
	require.Equal(t, 1, summary.Replies)

	rows, err := st.QueryReadOnly(context.Background(), `
select channel_id, ts, workspace_id, thread_ts, text, source_name, source_rank
from messages order by ts
`)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "T123", rows[0]["workspace_id"])
	require.Equal(t, SourceName, rows[0]["source_name"])
	require.Equal(t, int64(SourceRank), rows[0]["source_rank"])
	require.Equal(t, "1772574099.659199", rows[1]["thread_ts"])

	mentions, err := st.QueryReadOnly(context.Background(), `select target_id from message_mentions`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"target_id": "U456"}}, mentions)

	state, err := st.GetSyncState(context.Background(), SourceName, "workspace", "T123")
	require.NoError(t, err)
	require.NotEmpty(t, state)
}

func TestSyncStoresReferenceSlackMCPData(t *testing.T) {
	server := newReferenceGatewayServer(t)
	defer server.Close()
	t.Setenv("CODEX_APPS_ACCESS_TOKEN", "test-token")

	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	summary, err := Sync(context.Background(), st, Options{
		WorkspaceID: "TREF",
		Config: func() config.MCPConfig {
			cfg := testMCPConfig(server.URL)
			cfg.ConnectorID = ""
			return cfg
		}(),
	})
	require.NoError(t, err)
	require.Equal(t, 2, summary.Users)
	require.Equal(t, 1, summary.Channels)
	require.Equal(t, 1, summary.Messages)
	require.Equal(t, 1, summary.Replies)

	rows, err := st.QueryReadOnly(context.Background(), `
select channel_id, ts, thread_ts, user_id, text, source_name
from messages order by ts
`)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "CREF", rows[0]["channel_id"])
	require.Equal(t, "UADA", rows[0]["user_id"])
	require.Equal(t, "", rows[0]["thread_ts"])
	require.Equal(t, "1710000000.000001", rows[1]["thread_ts"])
	require.Equal(t, SourceName, rows[1]["source_name"])
}

func TestIncrementalSyncReconcilesStoredThreadRoots(t *testing.T) {
	server, threadCalls := newIncrementalThreadGatewayServer(t)
	defer server.Close()
	t.Setenv("CODEX_APPS_ACCESS_TOKEN", "test-token")

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	opts := Options{
		WorkspaceID: "T123",
		Channels:    []string{"C123"},
		Config:      testMCPConfig(server.URL),
	}
	require.NoError(t, syncTwice(ctx, st, opts))
	require.Equal(t, 2, *threadCalls)

	rows, err := st.QueryReadOnly(ctx, `select ts from messages where channel_id = 'C123' and thread_ts = '1710000000.000001' order by ts`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"ts": "1710000001.000002"}, {"ts": "1710000002.000003"}}, rows)
}

func TestSyncPreservesThreadMetadataWhenThreadPayloadHasNoReplies(t *testing.T) {
	server := newParentOnlyThreadGatewayServer(t)
	defer server.Close()
	t.Setenv("CODEX_APPS_ACCESS_TOKEN", "test-token")

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	summary, err := Sync(ctx, st, Options{
		WorkspaceID: "T123",
		Channels:    []string{"C123"},
		Config:      testMCPConfig(server.URL),
	})
	require.NoError(t, err)
	require.Equal(t, 1, summary.Messages)
	require.Equal(t, 0, summary.Replies)

	rows, err := st.QueryReadOnly(ctx, `select reply_count, latest_reply from messages where channel_id = 'C123' and ts = '1778016984.861209'`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"reply_count": int64(3), "latest_reply": "1778015241.379869"}}, rows)
}

func syncTwice(ctx context.Context, st *store.Store, opts Options) error {
	if _, err := Sync(ctx, st, opts); err != nil {
		return err
	}
	_, err := Sync(ctx, st, opts)
	return err
}

func TestSyncPreservesRicherExistingRecords(t *testing.T) {
	server := newTestGatewayServer(t)
	defer server.Close()
	t.Setenv("CODEX_APPS_ACCESS_TOKEN", "test-token")
	t.Setenv("CODEX_APPS_ACCOUNT_ID", "acct-123")

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	now := time.Now().UTC()
	require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{ID: "T123", Name: "richer", RawJSON: `{"source":"api"}`, UpdatedAt: now}))
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C123", WorkspaceID: "T123", Name: "richer-channel", Kind: "private_channel", Topic: "keep-topic", IsPrivate: true, RawJSON: `{"source":"api"}`, UpdatedAt: now}))
	require.NoError(t, st.UpsertUser(ctx, store.User{ID: "U123", WorkspaceID: "T123", Name: "richer-user", RealName: "Keep Name", IsBot: true, RawJSON: `{"source":"api"}`, UpdatedAt: now}))
	require.NoError(t, st.UpsertMessage(ctx, store.Message{
		ChannelID: "C123", TS: "1772574099.659199", WorkspaceID: "T123", UserID: "U123",
		Text: "richer root", NormalizedText: "richer root", SourceRank: 1, SourceName: "api-user",
		RawJSON: `{"source":"api"}`, UpdatedAt: now,
	}, []store.Mention{{Type: "user", TargetID: "UKEEP"}}))

	_, err = Sync(ctx, st, Options{WorkspaceID: "T123", Config: testMCPConfig(server.URL)})
	require.NoError(t, err)

	rows, err := st.QueryReadOnly(ctx, `select name, kind, topic, is_private from channels where id = 'C123'`)
	require.NoError(t, err)
	require.Equal(t, "richer-channel", rows[0]["name"])
	require.Equal(t, "private_channel", rows[0]["kind"])
	require.Equal(t, "keep-topic", rows[0]["topic"])
	require.Equal(t, int64(1), rows[0]["is_private"])

	rows, err = st.QueryReadOnly(ctx, `select name, real_name, is_bot from users where id = 'U123'`)
	require.NoError(t, err)
	require.Equal(t, "richer-user", rows[0]["name"])
	require.Equal(t, "Keep Name", rows[0]["real_name"])
	require.Equal(t, int64(1), rows[0]["is_bot"])

	rows, err = st.QueryReadOnly(ctx, `select text, source_name, source_rank from messages where channel_id = 'C123' and ts = '1772574099.659199'`)
	require.NoError(t, err)
	require.Equal(t, "richer root", rows[0]["text"])
	require.Equal(t, "api-user", rows[0]["source_name"])
	require.Equal(t, int64(1), rows[0]["source_rank"])
	mentions, err := st.QueryReadOnly(ctx, `select target_id from message_mentions where channel_id = 'C123' and ts = '1772574099.659199'`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"target_id": "UKEEP"}}, mentions)
}

func TestSyncRequiresWorkspaceID(t *testing.T) {
	_, err := Sync(context.Background(), nil, Options{})
	require.ErrorContains(t, err, "workspace ID is required")
}

func TestFilterChannelsByIDAndName(t *testing.T) {
	channels := []ChannelRecord{{ID: "C1", Name: "alpha"}, {ID: "C2", Name: "beta"}}
	require.Equal(t, []ChannelRecord{channels[1]}, filterChannels(channels, []string{"#alpha"}))
	require.Equal(t, []ChannelRecord{channels[0]}, filterChannels(channels, []string{"C2"}))
}

func TestSyncExplicitChannelAvoidsGlobalEnumeration(t *testing.T) {
	server := newTargetedGatewayServer(t)
	defer server.Close()
	t.Setenv("CODEX_APPS_ACCESS_TOKEN", "test-token")

	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	summary, err := Sync(context.Background(), st, Options{
		WorkspaceID: "T123",
		Channels:    []string{"C123"},
		Config: config.MCPConfig{
			BaseURL:         server.URL,
			TokenEnv:        "CODEX_APPS_ACCESS_TOKEN",
			ConnectorID:     testConnectorID,
			ChannelTypes:    "public_channel,private_channel",
			PageSize:        100,
			SearchLimit:     100,
			MaxPages:        10,
			ProtocolVersion: "2025-03-26",
		},
	})
	require.NoError(t, err)
	require.Equal(t, 1, summary.Channels)
	require.Equal(t, 0, summary.Users)
	require.Equal(t, 1, summary.Messages)

	rows, err := st.QueryReadOnly(context.Background(), `select kind from channels where id = 'C123'`)
	require.NoError(t, err)
	require.Equal(t, "mcp_channel", rows[0]["kind"])
}

func TestSyncPlanOverlapsAndHonorsLatestOnly(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	require.NoError(t, st.EnsureWorkspace(ctx, store.Workspace{ID: "T1", Name: "T1", RawJSON: "{}", UpdatedAt: time.Now().UTC()}))
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C1", WorkspaceID: "T1", Name: "one", Kind: "mcp_channel", RawJSON: "{}", UpdatedAt: time.Now().UTC()}))
	require.NoError(t, st.UpsertMessage(ctx, store.Message{
		ChannelID: "C1", TS: "1772574099.659199", WorkspaceID: "T1", Text: "existing", NormalizedText: "existing",
		SourceRank: SourceRank, SourceName: SourceName, RawJSON: "{}", UpdatedAt: time.Now().UTC(),
	}, nil))

	channels := []ChannelRecord{{ID: "C1"}, {ID: "C2"}}
	oldest, selected, err := syncPlan(ctx, st, "T1", channels, Options{LatestOnly: true})
	require.NoError(t, err)
	require.Equal(t, []ChannelRecord{{ID: "C1"}}, selected)
	require.Equal(t, "1772570499.659199", oldest["C1"])

	oldest, selected, err = syncPlan(ctx, st, "T1", channels, Options{Full: true})
	require.NoError(t, err)
	require.Len(t, selected, 2)
	require.Empty(t, oldest["C1"])

	oldest, selected, err = syncPlan(ctx, st, "T1", channels, Options{Since: "2026-03-08T12:00:00Z"})
	require.NoError(t, err)
	require.Len(t, selected, 2)
	require.Equal(t, "1772971200.000000", oldest["C1"])
}

func TestWalkPagesRejectsTruncation(t *testing.T) {
	calls := 0
	err := walkPages(2, func(string) (string, error) {
		calls++
		return fmt.Sprintf("more-%d", calls), nil
	})
	require.ErrorContains(t, err, "max_pages=2")
	require.Equal(t, 2, calls)
}

func TestResolveAuthPrefersEnvironment(t *testing.T) {
	t.Setenv("CODEX_APPS_ACCESS_TOKEN", "env-token")
	t.Setenv("CODEX_APPS_ACCOUNT_ID", "acct-123")
	auth, err := resolveAuth(config.MCPConfig{TokenEnv: "CODEX_APPS_ACCESS_TOKEN", AccountIDEnv: "CODEX_APPS_ACCOUNT_ID", AuthPath: "/unused"})
	require.NoError(t, err)
	require.Equal(t, "env-token", auth.AccessToken)
	require.Equal(t, "acct-123", auth.AccountID)
}

func TestResolveSlackToolset(t *testing.T) {
	tools := []mcpclient.Tool{
		{Name: "slack_search_channels", Meta: &mcpclient.ToolMeta{ConnectorID: testConnectorID}},
		{Name: "slack_search_users", Meta: &mcpclient.ToolMeta{ConnectorID: testConnectorID}},
		{Name: "slack_read_channel", Meta: &mcpclient.ToolMeta{ConnectorID: testConnectorID}},
		{Name: "slack_read_thread", Meta: &mcpclient.ToolMeta{ConnectorID: testConnectorID}},
	}
	client := Client{connectorID: testConnectorID}
	filtered, err := filterSlackTools(tools, client.connectorID)
	require.NoError(t, err)
	searchChannels, err := matchTool(filtered, []string{"search", "channel"}, nil)
	require.NoError(t, err)
	require.Equal(t, "slack_search_channels", searchChannels)
	require.Equal(t, "slack_read_thread", matchToolOptional(filtered, []string{"read", "thread"}, nil))
}

func testMCPConfig(baseURL string) config.MCPConfig {
	return config.MCPConfig{
		Enabled: true, BaseURL: baseURL, AuthPath: "/unused",
		TokenEnv: "CODEX_APPS_ACCESS_TOKEN", AccountIDEnv: "CODEX_APPS_ACCOUNT_ID",
		ConnectorID: testConnectorID, ChannelTypes: "public_channel,private_channel",
		PageSize: 100, SearchLimit: 100, MaxPages: 10, ProtocolVersion: "2025-03-26",
	}
}

func TestFilterSlackToolsRejectsUnknownConfiguredConnector(t *testing.T) {
	tools := []mcpclient.Tool{{Name: "slack_search_channels", Meta: &mcpclient.ToolMeta{ConnectorID: "new-id"}}}
	_, err := filterSlackTools(tools, "stale-id")
	require.ErrorContains(t, err, "was not found")
}

func TestFilterSlackToolsRejectsMissingSlackConnector(t *testing.T) {
	tools := []mcpclient.Tool{{Name: "gmail_search_emails", Meta: &mcpclient.ToolMeta{ConnectorName: "Gmail"}}}
	_, err := filterSlackTools(tools, "")
	require.ErrorContains(t, err, "none matched Slack")
}

func TestFilterSlackToolsUsesExactConnectorWhenAvailable(t *testing.T) {
	wanted := mcpclient.Tool{Name: "search_channels", Meta: &mcpclient.ToolMeta{ConnectorID: "wanted", ConnectorName: "Slack"}}
	other := mcpclient.Tool{Name: "slack_search_channels", Meta: &mcpclient.ToolMeta{ConnectorID: "other", ConnectorName: "Slack"}}
	filtered, err := filterSlackTools([]mcpclient.Tool{other, wanted}, "wanted")
	require.NoError(t, err)
	require.Equal(t, []mcpclient.Tool{wanted}, filtered)
}

func newTestGatewayServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		require.Equal(t, "acct-123", r.Header.Get("ChatGPT-Account-ID"))
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			writeRPCResult(t, w, map[string]any{"protocolVersion": "2025-03-26"})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPCResult(t, w, map[string]any{"tools": []map[string]any{
				{"name": "slack_search_channels", "_meta": map[string]any{"connector_id": testConnectorID}},
				{"name": "slack_search_users", "_meta": map[string]any{"connector_id": testConnectorID}},
				{"name": "slack_read_channel", "_meta": map[string]any{"connector_id": testConnectorID}},
				{"name": "slack_read_thread", "_meta": map[string]any{"connector_id": testConnectorID}},
			}, "nextCursor": ""})
		case "tools/call":
			name, _ := req.Params["name"].(string)
			switch name {
			case "slack_search_channels":
				writeToolText(t, w, map[string]any{"results": "### Result 1\nName: #codex-feedback\nCreated: 2026-03-08T12:00:00Z\nPurpose: feedback\nTopic: debugging\nPermalink: [open](https://openai.slack.com/archives/C123)\nIs Archived: false", "pagination_info": ""})
			case "slack_search_users":
				writeToolText(t, w, map[string]any{"results": "### Result 1\nUser ID: U123\nName: Ada\nTitle: Engineer\nEmail: ada@example.com\nTimezone: UTC\nPermalink: [open](https://openai.slack.com/team/U123)", "pagination_info": ""})
			case "slack_read_channel":
				writeToolText(t, w, map[string]any{"messages": "Channel: codex-feedback (C123)\n\n=== Message from Ada (U123) at 2026-03-08T12:00:00Z === \nMessage TS: 1772574099.659199\nroot message <@U456>\nThread: 1 replies (latest: 2026-03-08T12:03:19Z)", "pagination_info": ""})
			case "slack_read_thread":
				writeToolText(t, w, map[string]any{"messages": "From: Ada (U123)\nTime: 2026-03-08T12:00:00Z\nMessage TS: 1772574099.659199\nroot message <@U456>\n\n=== THREAD REPLIES\n\n--- Reply 1 ---\nFrom: Grace (U456)\nTime: 2026-03-08T12:03:19Z\nMessage TS: 1772574199.000000\nreply message", "pagination_info": ""})
			default:
				t.Fatalf("unexpected tool call %q", name)
			}
		default:
			t.Fatalf("unexpected RPC method %q", req.Method)
		}
	}))
}

func newTargetedGatewayServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		switch req.Method {
		case "initialize":
			writeRPCResult(t, w, map[string]any{"protocolVersion": "2025-03-26"})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPCResult(t, w, map[string]any{"tools": []map[string]any{
				{"name": "slack_search_channels", "_meta": map[string]any{"connector_id": testConnectorID}},
				{"name": "slack_search_users", "_meta": map[string]any{"connector_id": testConnectorID}},
				{"name": "slack_read_channel", "_meta": map[string]any{"connector_id": testConnectorID}},
			}, "nextCursor": ""})
		case "tools/call":
			name, _ := req.Params["name"].(string)
			require.Equal(t, "slack_read_channel", name, "targeted sync must not enumerate channels or users")
			writeToolText(t, w, map[string]any{
				"messages":        "Channel: codex-feedback (C123)\n\n=== Message from Ada (U123) at 2026-03-08T12:00:00Z === \nMessage TS: 1772574099.659199\ntargeted message",
				"pagination_info": "",
			})
		default:
			t.Fatalf("unexpected RPC method %q", req.Method)
		}
	}))
}

func newParentOnlyThreadGatewayServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			writeRPCResult(t, w, map[string]any{"protocolVersion": "2025-03-26"})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPCResult(t, w, map[string]any{"tools": []map[string]any{
				{"name": "slack_search_channels", "_meta": map[string]any{"connector_id": testConnectorID}},
				{"name": "slack_search_users", "_meta": map[string]any{"connector_id": testConnectorID}},
				{"name": "slack_read_channel", "_meta": map[string]any{"connector_id": testConnectorID}},
				{"name": "slack_read_thread", "_meta": map[string]any{"connector_id": testConnectorID}},
			}, "nextCursor": ""})
		case "tools/call":
			name, _ := req.Params["name"].(string)
			switch name {
			case "slack_read_channel":
				writeToolText(t, w, map[string]any{
					"messages":        "Channel: parent-only (C123)\n\n=== Message from Ada (U123) at 2026-05-05T21:36:24Z === \nMessage TS: 1778016984.861209\nroot\nThread: 3 replies (latest: 1778015241.379869)",
					"pagination_info": "",
				})
			case "slack_read_thread":
				writeToolText(t, w, map[string]any{
					"messages":        "=== THREAD PARENT MESSAGE ===\nFrom: Ada (U123)\nTime: 2026-05-05 14:36:24 PDT\nMessage TS: 1778016984.861209\nroot",
					"pagination_info": "There are no more messages in this thread.\n",
				})
			default:
				t.Fatalf("unexpected tool call %q", name)
			}
		default:
			t.Fatalf("unexpected RPC method %q", req.Method)
		}
	}))
}

func newReferenceGatewayServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		switch req.Method {
		case "initialize":
			writeRPCResult(t, w, map[string]any{"protocolVersion": "2025-03-26"})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPCResult(t, w, map[string]any{"tools": []map[string]any{
				{"name": "slack_list_channels"},
				{"name": "slack_get_channel_history"},
				{"name": "slack_get_thread_replies"},
				{"name": "slack_get_users"},
			}})
		case "tools/call":
			name, _ := req.Params["name"].(string)
			switch name {
			case "slack_list_channels":
				writeToolText(t, w, map[string]any{
					"ok": true,
					"channels": []map[string]any{{
						"id": "CREF", "name": "reference", "is_private": false,
						"topic": map[string]any{"value": "MCP"}, "purpose": map[string]any{"value": "test"},
					}},
					"response_metadata": map[string]any{"next_cursor": ""},
				})
			case "slack_get_users":
				writeToolText(t, w, map[string]any{
					"ok": true,
					"members": []map[string]any{
						{"id": "UADA", "name": "ada", "profile": map[string]any{"real_name": "Ada", "display_name": "ada"}},
						{"id": "UGRACE", "name": "grace", "profile": map[string]any{"real_name": "Grace", "display_name": "grace"}},
					},
					"response_metadata": map[string]any{"next_cursor": ""},
				})
			case "slack_get_channel_history":
				writeToolText(t, w, map[string]any{
					"ok": true,
					"messages": []map[string]any{{
						"ts": "1710000000.000001", "user": "UADA", "text": "reference root",
						"reply_count": 1, "latest_reply": "1710000001.000002",
					}},
				})
			case "slack_get_thread_replies":
				writeToolText(t, w, map[string]any{
					"ok": true,
					"messages": []map[string]any{
						{"ts": "1710000000.000001", "user": "UADA", "text": "reference root", "reply_count": 1},
						{"ts": "1710000001.000002", "thread_ts": "1710000000.000001", "user": "UGRACE", "text": "reference reply"},
					},
				})
			default:
				t.Fatalf("unexpected reference tool call %q", name)
			}
		default:
			t.Fatalf("unexpected RPC method %q", req.Method)
		}
	}))
}

func newIncrementalThreadGatewayServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	historyCalls := 0
	threadCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		switch req.Method {
		case "initialize":
			writeRPCResult(t, w, map[string]any{"protocolVersion": "2025-03-26"})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPCResult(t, w, map[string]any{"tools": []map[string]any{
				{"name": "slack_search_channels", "_meta": map[string]any{"connector_id": testConnectorID}},
				{"name": "slack_search_users", "_meta": map[string]any{"connector_id": testConnectorID}},
				{"name": "slack_read_channel", "_meta": map[string]any{"connector_id": testConnectorID}},
				{"name": "slack_read_thread", "_meta": map[string]any{"connector_id": testConnectorID}},
			}})
		case "tools/call":
			name, _ := req.Params["name"].(string)
			switch name {
			case "slack_read_channel":
				historyCalls++
				messages := "Channel: test (C123)\n\n=== Message from Ada (U1) at 2024-03-09T16:00:00Z === \nMessage TS: 1710003600.000010\nnewer root"
				if historyCalls == 1 {
					messages = "Channel: test (C123)\n\n=== Message from Ada (U1) at 2024-03-09T15:00:00Z === \nMessage TS: 1710000000.000001\nold root\nThread: 1 replies (latest: 2024-03-09T15:00:01Z)\n\n=== Message from Ada (U1) at 2024-03-09T16:00:00Z === \nMessage TS: 1710003600.000010\nnewer root"
				}
				writeToolText(t, w, map[string]any{"messages": messages, "pagination_info": ""})
			case "slack_read_thread":
				threadCalls++
				messages := "From: Ada (U1)\nTime: 2024-03-09T15:00:00Z\nMessage TS: 1710000000.000001\nold root\n\n=== THREAD REPLIES\n\n--- Reply 1 ---\nFrom: Grace (U2)\nTime: 2024-03-09T15:00:01Z\nMessage TS: 1710000001.000002\nfirst reply"
				if threadCalls > 1 {
					messages += "\n\n--- Reply 2 ---\nFrom: Linus (U3)\nTime: 2024-03-09T15:00:02Z\nMessage TS: 1710000002.000003\nlate reply"
				}
				writeToolText(t, w, map[string]any{"messages": messages, "pagination_info": ""})
			default:
				t.Fatalf("unexpected incremental tool call %q", name)
			}
		default:
			t.Fatalf("unexpected RPC method %q", req.Method)
		}
	}))
	return server, &threadCalls
}

func writeToolText(t *testing.T, w http.ResponseWriter, payload map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	writeRPCResult(t, w, map[string]any{"isError": false, "content": []map[string]any{{"text": string(raw)}}})
}

func writeRPCResult(t *testing.T, w http.ResponseWriter, result any) {
	t.Helper()
	require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}))
}
