package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/stretchr/testify/require"
)

func TestMCPDMPolicyAcrossCLIPaths(t *testing.T) {
	const canary = "SYNTHETIC_MCP_DM_CONTENT"
	for _, source := range []string{"mcp", "connector"} {
		for _, native := range []bool{false, true} {
			for _, policy := range []struct {
				name  string
				value *bool
			}{{"omitted", nil}, {"false", new(false)}, {"true", new(true)}} {
				for _, selection := range []string{"all", "id", "name"} {
					t.Run(fmt.Sprintf("%s/native=%v/%s/%s", source, native, policy.name, selection), func(t *testing.T) {
						var dataCalls, catalogCalls, dmCalls atomic.Int32
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							var req struct {
								Method string `json:"method"`
								Params struct {
									Name      string         `json:"name"`
									Arguments map[string]any `json:"arguments"`
								} `json:"params"`
							}
							require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
							var result any
							switch req.Method {
							case "initialize":
								result = map[string]any{"protocolVersion": "2025-03-26"}
							case "notifications/initialized":
								w.WriteHeader(http.StatusAccepted)
								return
							case "tools/list":
								names := []string{"slack_search_channels", "slack_search_users", "slack_read_channel", "slack_read_thread"}
								if native {
									names = []string{"slack_list_channels", "slack_get_users", "slack_get_channel_history", "slack_get_thread_replies"}
								}
								tools := []map[string]any{}
								for _, name := range names {
									tools = append(tools, map[string]any{"name": name})
								}
								result = map[string]any{"tools": tools}
							case "tools/call":
								dataCalls.Add(1)
								payload := map[string]any{"ok": true}
								channel, _ := req.Params.Arguments["channel_id"].(string)
								text := "safe"
								if channel == "CDM" {
									text = canary
									dmCalls.Add(1)
								}
								switch req.Params.Name {
								case "slack_list_channels":
									catalogCalls.Add(1)
									payload["channels"] = []map[string]any{{"id": "CKEEP", "name": "safe", "is_channel": true}, {"id": "CDM", "name": "direct", "is_im": true, "is_private": true, "topic": map[string]any{"value": canary}}}
								case "slack_search_channels":
									catalogCalls.Add(1)
									// Search filtering is the connector's responsibility; return the
									// requested name alone so this fixture also runs on the parent.
									payload["results"] = "### Result 1\nChannel ID: CDM\nName: direct\nIs Private: true\nTopic: " + canary
									if req.Params.Arguments["query"] != "direct" {
										payload["results"] = "### Result 1\nChannel ID: CKEEP\nName: safe\nIs Private: false\n### Result 2\nChannel ID: CDM\nName: direct\nIs Private: true\nTopic: " + canary
									}
								case "slack_get_users":
									payload["members"] = []any{}
								case "slack_search_users":
									payload["results"] = ""
								case "slack_get_channel_history":
									payload["messages"] = []map[string]any{{"channel": channel, "context_team_id": "TLOCAL", "ts": "1710000000.000001", "text": text, "user": "UEXTERNAL", "team": "TEXTERNAL", "reply_count": 1}}
								case "slack_get_thread_replies":
									payload["messages"] = []map[string]any{{"channel": channel, "ts": "1710000000.000001", "text": text, "reply_count": 1}, {"channel": channel, "ts": "1710000001.000002", "thread_ts": "1710000000.000001", "text": text, "files": []map[string]any{{"title": text}}}}
								case "slack_read_channel":
									payload["messages"] = "Channel: fixture (" + channel + ")\n\n=== Message from External (UEXTERNAL) at 2024-03-09T16:00:00Z === \nMessage TS: 1710000000.000001\n" + text + "\nThread: 1 replies (latest: 1710000001.000002)"
								case "slack_read_thread":
									payload["messages"] = "From: External (UEXTERNAL)\nTime: 2024-03-09T16:00:00Z\nMessage TS: 1710000000.000001\n" + text + "\n\n=== THREAD REPLIES\n\n--- Reply 1 ---\nFrom: External (UEXTERNAL)\nTime: 2024-03-09T16:00:01Z\nMessage TS: 1710000001.000002\n" + text
								default:
									t.Errorf("unexpected tool %q", req.Params.Name)
								}
								raw, err := json.Marshal(payload)
								require.NoError(t, err)
								result = map[string]any{"content": []map[string]any{{"type": "text", "text": string(raw)}}}
							default:
								t.Errorf("unexpected method %q", req.Method)
							}
							require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}))
						}))
						defer server.Close()
						dir := t.TempDir()
						cfg := config.Default()
						cfg.DBPath = filepath.Join(dir, "archive.db")
						cfg.CacheDir = filepath.Join(dir, "cache")
						cfg.LogDir = filepath.Join(dir, "logs")
						cfg.Share.RepoPath = filepath.Join(dir, "share")
						cfg.Share.AutoUpdate = false
						cfg.Sync.IncludeDMs = policy.value
						cfg.Slack.MCP.Enabled = true
						cfg.Slack.MCP.BaseURL = server.URL
						cfg.Slack.MCP.TokenEnv = "SLACRAWL_MCP_ADMISSION_TOKEN"
						cfg.Slack.MCP.ConnectorID = ""
						t.Setenv("SLACRAWL_MCP_ADMISSION_TOKEN", "synthetic-token")
						path := filepath.Join(dir, "config.toml")
						require.NoError(t, cfg.Save(path))
						args := []string{"--config", path, "sync", "--source", source, "--workspace", "TLOCAL", "--full"}
						if selection != "all" {
							channel := "CDM"
							if selection == "name" {
								channel = "direct"
							}
							args = append(args, "--channels", channel)
						}
						var stdout, stderr bytes.Buffer
						app := &App{Stdout: &stdout, Stderr: &stderr}
						err := app.Run(context.Background(), args)
						st, openErr := store.OpenReadOnly(cfg.DBPath)
						require.NoError(t, openErr)
						defer func() { require.NoError(t, st.Close()) }()
						rows, queryErr := st.QueryReadOnly(context.Background(), "select * from messages where channel_id = 'CDM'")
						require.NoError(t, queryErr)
						wantDM := 2
						if policy.name == "false" {
							wantDM = 0
						}
						require.Len(t, rows, wantDM, "persisted DM message count")
						if policy.name == "false" && !native {
							require.ErrorContains(t, err, "requires native conversation type evidence")
							require.Zero(t, dataCalls.Load())
						} else {
							require.NoError(t, err)
						}
						if policy.name == "false" {
							require.Zero(t, dmCalls.Load())
							for _, table := range []string{"workspaces", "channels", "users", "messages", "message_files", "message_events", "message_event_heads", "message_mentions", "message_fts", "embedding_jobs", "sync_state"} {
								rows, queryErr := st.QueryReadOnly(context.Background(), "select * from "+table)
								require.NoError(t, queryErr)
								require.NotContains(t, fmt.Sprint(rows), canary, table)
								// Revisions are random: match the channel token, not a substring of its ID.
								require.NotRegexp(t, `\bCDM\b`, fmt.Sprint(rows), table)
								if !native || selection != "all" {
									require.Empty(t, rows, table)
								}
							}
							output := stdout.String() + stderr.String() + fmt.Sprint(err)
							require.NotContains(t, output, canary)
							require.NotContains(t, output, "CDM")
							if native {
								require.Contains(t, stdout.String(), "Completed with omissions")
								require.Equal(t, int32(1), catalogCalls.Load())
							}
						} else if selection == "id" {
							require.Zero(t, catalogCalls.Load(), "compatible explicit IDs avoid enumeration")
						}
					})
				}
			}
		}
	}
}

func TestMCPNoEligibleRendering(t *testing.T) {
	for _, noEligible := range []bool{false, true} {
		var output strings.Builder
		require.True(t, renderSyncBlock(&output, "sync", map[string]any{"summary": map[string]any{"mcp": map[string]any{"channels": 0, "no_eligible_conversations": noEligible}}}))
		if noEligible {
			require.Contains(t, output.String(), "no eligible MCP conversations")
			require.NotContains(t, output.String(), "local state refreshed")
		} else {
			require.Contains(t, output.String(), "local state refreshed")
		}
	}
}
