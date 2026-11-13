package slackapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/admission"
	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
)

var pageCollectionCases = []struct {
	name, value string
	success     bool
	errorText   string
}{
	{"absent", "", true, ""},
	{"null", "null", true, ""},
	{"empty", "[]", true, ""},
	{"object", "{}", true, ""},
	{"string", `"collection-canary"`, true, ""},
	{"number", "42", true, ""},
	{"boolean", "true", true, ""},
	{"empty-with-error", "[]", true, "missing_scope"},
	{"absent-before-success", "", false, " \t "},
	{"null-before-success", "null", false, " \t "},
}

func TestConversationPagesRequireExplicitSuccess(t *testing.T) {
	for _, method := range []string{"history", "replies"} {
		for _, status := range []struct{ name, field string }{{"missing", ""}, {"null", `"ok":null,`}, {"false", `"ok":false,`}} {
			for _, detail := range []struct{ name, field string }{{"missing", ""}, {"null", `"error":null,`}, {"empty", `"error":"",`}, {"blank", `"error":" \t ",`}} {
				t.Run(method+"/"+status.name+"/"+detail.name, func(t *testing.T) {
					calls := 0
					client := primaryOwnerClient(t, config.Tokens{User: "fixture"}, func(r *http.Request, form url.Values) (any, error) {
						calls++
						require.Equal(t, "/conversations."+method, r.URL.Path)
						require.Equal(t, "fixture", form.Get("token"))
						return json.RawMessage(`{` + status.field + detail.field + `"messages":[{"ts":"1710000000.000000","text":"rejected-page-canary"}]}`), nil
					})
					var err error
					if method == "history" {
						var page *conversationHistoryPage
						page, err = client.getConversationHistory(context.Background(), "fixture", &slack.GetConversationHistoryParameters{ChannelID: "C123"}, nil)
						require.Nil(t, page)
					} else {
						var page *conversationRepliesPage
						page, err = client.getConversationReplies(context.Background(), client.tokens.User, &slack.GetConversationRepliesParameters{ChannelID: "C123", Timestamp: "1710000000.000000"}, nil)
						require.Nil(t, page)
					}
					require.EqualError(t, err, "conversations."+method+" response did not report success")
					require.NotContains(t, err.Error(), "rejected-page-canary")
					require.Equal(t, 1, calls, "a malformed success response is not rate-limited")
				})
			}
		}
		t.Run(method+"/before-message-conversion", func(t *testing.T) {
			client := primaryOwnerClient(t, config.Tokens{User: "fixture"}, func(r *http.Request, _ url.Values) (any, error) {
				require.Equal(t, "/conversations."+method, r.URL.Path)
				return json.RawMessage(`{"ok":false,"messages":[42]}`), nil
			})
			if method == "history" {
				page, err := client.getConversationHistory(context.Background(), "fixture", &slack.GetConversationHistoryParameters{ChannelID: "C123"}, nil)
				require.Nil(t, page)
				require.EqualError(t, err, "conversations.history response did not report success")
			} else {
				page, err := client.getConversationReplies(context.Background(), client.tokens.User, &slack.GetConversationRepliesParameters{ChannelID: "C123", Timestamp: "1710000000.000000"}, nil)
				require.Nil(t, page)
				require.EqualError(t, err, "conversations.replies response did not report success")
			}
		})
	}

	for _, method := range []string{"conversations.history", "conversations.replies"} {
		for _, tc := range pageCollectionCases {
			t.Run("collection/"+method+"/"+tc.name, func(t *testing.T) {
				calls := 0
				client := primaryOwnerClient(t, config.Tokens{User: "fixture"}, func(r *http.Request, _ url.Values) (any, error) {
					calls++
					require.Equal(t, "/"+method, r.URL.Path)
					payload := map[string]any{"ok": tc.success, "error": tc.errorText}
					if tc.value != "" {
						payload["messages"] = json.RawMessage(tc.value)
					}
					if tc.value != "[]" {
						payload["response_metadata"] = map[string]any{"next_cursor": "untrusted-cursor"}
					}
					return payload, nil
				})
				var err error
				if method == "conversations.history" {
					page, callErr := client.getConversationHistory(context.Background(), "fixture", &slack.GetConversationHistoryParameters{ChannelID: "C123"}, nil)
					err = callErr
					if tc.value == "[]" {
						require.NotNil(t, page)
						require.Empty(t, page.Messages)
						require.Empty(t, page.NextCursor)
					} else {
						require.Nil(t, page)
					}
				} else {
					page, callErr := client.getConversationReplies(context.Background(), client.tokens.User, &slack.GetConversationRepliesParameters{ChannelID: "C123", Timestamp: "1710000000.000000"}, nil)
					err = callErr
					if tc.value == "[]" {
						require.NotNil(t, page)
						require.Empty(t, page.Messages)
						require.Empty(t, page.NextCursor)
					} else {
						require.Nil(t, page)
					}
				}
				if tc.value == "[]" {
					require.NoError(t, err, "explicit empty arrays preserve Ok=true's existing error short-circuit")
				} else if !tc.success {
					require.EqualError(t, err, method+" response did not report success")
				} else if tc.value == "" || tc.value == "null" {
					require.EqualError(t, err, method+" response did not provide a collection array; page remains uncertified")
				} else {
					var typeErr *json.UnmarshalTypeError
					require.ErrorAs(t, err, &typeErr)
				}
				if err != nil {
					require.NotContains(t, err.Error(), "collection-canary")
				}
				require.Equal(t, 1, calls, "rejected pages neither retry nor expose their cursor")
			})
		}
	}
}

func TestConversationPageSuccessPreservesNativeErrors(t *testing.T) {
	response := slack.SlackResponse{
		Error: "missing_scope", Errors: []slack.SlackResponseErrors{{Message: new("native-detail")}},
		ResponseMetadata: slack.ResponseMetadata{Messages: []string{"native-message"}, Warnings: []string{"native-warning"}, Cursor: "native-cursor"},
	}
	// Direct helper input proves metadata passthrough. Page decoding's existing
	// response_metadata cursor field does not populate the embedded metadata.
	var cause slack.SlackErrorResponse
	helperErr := nativePageSuccess("conversations.history", response, false)
	require.ErrorAs(t, helperErr, &cause)
	require.Equal(t, response.Err(), cause)
	require.EqualError(t, helperErr, "missing_scope")
	for _, method := range []string{"history", "replies"} {
		for _, collection := range []string{"records", "absent", "null"} {
			t.Run(method+"/"+collection, func(t *testing.T) {
				client := primaryOwnerClient(t, config.Tokens{User: "fixture"}, func(r *http.Request, _ url.Values) (any, error) {
					require.Equal(t, "/conversations."+method, r.URL.Path)
					payload := map[string]any{"ok": false, "error": "missing_scope", "errors": []string{"native-detail"}}
					if collection == "records" {
						payload["messages"] = []any{42}
					} else if collection == "null" {
						payload["messages"] = nil
					}
					return payload, nil
				})
				var err error
				if method == "history" {
					var page *conversationHistoryPage
					page, err = client.getConversationHistory(context.Background(), "fixture", &slack.GetConversationHistoryParameters{ChannelID: "C123"}, nil)
					require.Nil(t, page)
				} else {
					var page *conversationRepliesPage
					page, err = client.getConversationReplies(context.Background(), client.tokens.User, &slack.GetConversationRepliesParameters{ChannelID: "C123", Timestamp: "1710000000.000000"}, nil)
					require.Nil(t, page)
				}
				var native slack.SlackErrorResponse
				require.ErrorAs(t, err, &native)
				require.Equal(t, "missing_scope", native.Err)
				require.Equal(t, response.Errors, native.Errors)
			})
		}
	}
}

func TestConversationPageRateLimitRetryAndCancellation(t *testing.T) {
	for _, method := range []string{"history", "replies"} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/canceled=%t", method, canceled), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls, sleeps := 0, 0
				client := NewWithOptions(config.Tokens{User: "fixture"}, "https://fixture.invalid/", &http.Client{Transport: primaryOwnerRoundTrip(func(r *http.Request) (*http.Response, error) {
					calls++
					require.NoError(t, r.ParseForm())
					require.Equal(t, "/conversations."+method, r.URL.Path)
					require.Equal(t, "second", r.Form.Get("cursor"))
					if calls == 1 {
						return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"2"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"messages":[]}`)), Request: r}, nil
				})})
				client.sleep = func(ctx context.Context, delay time.Duration) error {
					sleeps++
					require.Equal(t, 2*time.Second, delay)
					if canceled {
						cancel()
					}
					return ctx.Err()
				}
				var page any
				var err error
				if method == "history" {
					page, err = client.getConversationHistory(ctx, "fixture", &slack.GetConversationHistoryParameters{ChannelID: "C123", Cursor: "second"}, nil)
				} else {
					page, err = client.getConversationReplies(ctx, client.tokens.User, &slack.GetConversationRepliesParameters{ChannelID: "C123", Timestamp: "1710000000.000000", Cursor: "second"}, nil)
				}
				if canceled {
					require.ErrorIs(t, err, context.Canceled)
					require.Nil(t, page)
					require.Equal(t, 1, calls)
				} else {
					require.NoError(t, err)
					require.NotNil(t, page)
					require.Equal(t, 2, calls)
				}
				require.Equal(t, 1, sleeps)
			})
		}
	}
}

func TestCompletenessDecoderPreservesCapabilityProbes(t *testing.T) {
	for _, method := range []string{"history", "replies"} {
		for _, collection := range []string{"empty", "empty-cursor", "absent", "null"} {
			t.Run(method+"/"+collection, func(t *testing.T) {
				var calls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					require.NoError(t, r.ParseForm())
					require.Equal(t, "1", r.Form.Get("limit"))
					require.Equal(t, "/conversations."+method, r.URL.Path)
					payload := map[string]any{"ok": true, "has_more": true, "is_limited": true}
					switch collection {
					case "empty", "empty-cursor":
						payload["messages"] = []any{}
					case "null":
						payload["messages"] = nil
					}
					if collection == "empty-cursor" {
						payload["response_metadata"] = map[string]any{"next_cursor": "next"}
					}
					w.Header().Set("Content-Type", "application/json")
					require.NoError(t, json.NewEncoder(w).Encode(payload))
				}))
				defer server.Close()
				client := NewWithOptions(config.Tokens{User: "fixture"}, server.URL+"/", server.Client())
				missing := collection == "absent" || collection == "null"
				cursor := ""
				if collection == "empty-cursor" {
					cursor = "next"
				}
				if method == "history" {
					// Verify Limit=1 requires a collection independently of Doctor's
					// access diagnosis and without enforcing scan completion.
					page, err := client.getConversationHistory(context.Background(), "fixture", &slack.GetConversationHistoryParameters{ChannelID: "C123", Limit: 1}, nil)
					if missing {
						require.Nil(t, page)
						require.EqualError(t, err, "conversations.history response did not provide a collection array; page remains uncertified")
					} else {
						require.NoError(t, err)
						require.NotNil(t, page.Messages)
						require.Empty(t, page.Messages)
						require.True(t, page.HasMore)
						require.True(t, page.IsLimited)
						require.Equal(t, cursor, page.NextCursor)
					}
				} else {
					page, err := client.getConversationReplies(context.Background(), client.tokens.User, &slack.GetConversationRepliesParameters{ChannelID: "C123", Timestamp: "1710000001.000000", Limit: 1}, nil)
					if missing {
						require.Nil(t, page)
						require.EqualError(t, err, "conversations.replies response did not provide a collection array; page remains uncertified")
					} else {
						require.NoError(t, err)
						require.NotNil(t, page.Messages)
						require.Empty(t, page.Messages)
						require.True(t, page.HasMore)
						require.Equal(t, cursor, page.NextCursor)
					}
				}
				require.EqualValues(t, 1, calls.Load())
			})
		}
	}
	for _, failure := range []string{"absent", "null", "missing-scope"} {
		t.Run("doctor/"+failure, func(t *testing.T) {
			var calls []string
			client := primaryOwnerClient(t, config.Tokens{User: "fixture-user"}, func(r *http.Request, form url.Values) (any, error) {
				calls = append(calls, r.URL.Path)
				switch r.URL.Path {
				case "/auth.test":
					return primaryOwnerResponse(r.URL.Path), nil
				case "/conversations.list":
					return json.RawMessage(`{"ok":true,"channels":[{"id":"D123","is_im":true}]}`), nil
				case "/conversations.history":
					require.Equal(t, "D123", form.Get("channel"))
					require.Equal(t, "1", form.Get("limit"))
					if failure == "missing-scope" {
						return json.RawMessage(`{"ok":false,"error":"missing_scope"}`), nil
					}
					payload := map[string]any{"ok": true}
					if failure == "null" {
						payload["messages"] = nil
					}
					return payload, nil
				default:
					t.Fatalf("unexpected method %s", r.URL.Path)
					return nil, nil
				}
			}).WithDMPolicy(admission.Include)
			diag, err := client.Doctor(context.Background())
			require.NoError(t, err)
			require.True(t, diag.UserAuthAvailable)
			require.True(t, diag.DMsIncluded)
			require.Equal(t, "full", diag.ThreadCoverage)
			wantScope := ""
			if failure == "missing-scope" {
				wantScope = "im:history"
			}
			require.Equal(t, wantScope, diag.DMsMissingScope)
			wantFailure := "history_failed"
			if failure == "missing-scope" {
				wantFailure = ""
			}
			require.Equal(t, wantFailure, diag.DMProbeError, "access sampling does not certify a page")
			require.Equal(t, []string{"/auth.test", "/conversations.list", "/conversations.history"}, calls)
		})
	}
}

func TestHistoryCompletenessAcrossSources(t *testing.T) {
	for _, sourceName := range []string{SourceBot, SourceUser} {
		for _, mode := range []string{"more", "limited", "unknown-limited"} {
			t.Run(sourceName+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				st := mustStore(t)
				defer st.Close()
				now := time.Unix(1710000200, 0).UTC()
				channel := slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C123"}}}
				require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C123", WorkspaceID: "T123", Name: "fixture", UpdatedAt: now}))
				prior := store.APIHistoryState{Complete: true, Latest: "1709900000.000000"}
				oldest := "1709896400.000000"
				if mode == "unknown-limited" {
					prior = store.APIHistoryState{}
					oldest = ""
				} else {
					require.NoError(t, seedAPIHistory(ctx, st, sourceName, "T123", "C123", "", prior))
				}
				var cursorsMu sync.Mutex
				var cursors []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					require.NoError(t, r.ParseForm())
					require.Equal(t, sourceName, r.Form.Get("token"))
					require.Equal(t, oldest, r.Form.Get("oldest"))
					require.Equal(t, "1710000200.000000", r.Form.Get("latest"))
					cursor := r.Form.Get("cursor")
					cursorsMu.Lock()
					cursors = append(cursors, cursor)
					cursorsMu.Unlock()
					payload := map[string]any{"ok": true, "messages": []any{map[string]any{"channel": "C123", "ts": "1710000000.000000", "text": "first"}}}
					if cursor == "" {
						payload["is_limited"] = mode != "more"
						payload["response_metadata"] = map[string]any{"next_cursor": "second"}
					} else {
						require.Equal(t, "second", cursor)
						payload["messages"] = []any{map[string]any{"channel": "C123", "ts": "1710000001.000000", "text": "second"}}
						payload["has_more"] = mode == "more"
						payload["is_limited"] = false
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(payload)
				}))
				defer server.Close()
				client := NewWithOptions(config.Tokens{Bot: SourceBot, User: SourceUser}, server.URL+"/", server.Client())
				source := channelSyncSource{token: sourceName, sourceName: sourceName, sourceRank: 2}
				if sourceName == SourceUser {
					source.sourceRank = 1
				}
				err := client.syncChannelMessagesWithSource(ctx, st, "T123", channel, store.APIHistoryOptions{}, now, threadSyncSource{}, source)
				if mode == "more" {
					require.ErrorContains(t, err, "has_more without a continuation cursor")
				} else {
					require.ErrorContains(t, err, "completeness of the requested interval is uncertified")
				}
				cursorsMu.Lock()
				gotCursors := append([]string(nil), cursors...)
				cursorsMu.Unlock()
				require.Equal(t, []string{"", "second"}, gotCursors)
				coverage, err := readAPIHistory(ctx, st, sourceName, "T123", "C123", "")
				require.NoError(t, err)
				require.Equal(t, prior.Latest, coverage.Latest)
				require.Equal(t, prior.Complete, coverage.Complete)
				require.NotNil(t, coverage.Pending)
				require.Equal(t, oldest, *coverage.Pending)
				rows, err := st.QueryReadOnly(ctx, "select source_name,source_rank from messages order by ts")
				require.NoError(t, err)
				require.Len(t, rows, 2, "valid bounded pages remain committed")
				for _, row := range rows {
					require.Equal(t, sourceName, row["source_name"])
					require.EqualValues(t, source.sourceRank, row["source_rank"])
				}
			})
		}
	}
}

func TestHistoryPageSuccessRetriesPendingInterval(t *testing.T) {
	for _, failure := range []string{"unsuccessful", "absent", "null", "decode", "native-error"} {
		failureReason := "conversations.history response did not report success"
		if failure == "native-error" {
			failureReason = "slack conversations.history API response failed"
		} else if failure == "decode" {
			failureReason = "slack conversations.history message decode failed"
		} else if failure != "unsuccessful" {
			failureReason = "conversations.history response did not provide a collection array; page remains uncertified"
		}
		for _, tc := range []struct {
			name, source, token string
			tokens              config.Tokens
			repair              bool
		}{
			{"bot", SourceBot, "fixture-bot", config.Tokens{Bot: "fixture-bot"}, false},
			{"user", SourceUser, "fixture-user", config.Tokens{User: "fixture-user"}, false},
			{"repair", SourceBot, "fixture-bot", config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, true},
		} {
			t.Run(tc.name+"/"+failure, func(t *testing.T) {
				ctx := context.Background()
				st := mustStore(t)
				defer func() { require.NoError(t, st.Close()) }()
				now := time.Unix(1710000200, 0).UTC()
				require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C123", WorkspaceID: "T123", Name: "fixture", Kind: "public_channel", UpdatedAt: now}))
				require.NoError(t, seedAPIHistory(ctx, st, tc.source, "T123", "C123", "", store.APIHistoryState{Complete: true, Latest: "1709900000.000000"}))
				require.NoError(t, st.SetSyncState(ctx, tc.source, "workspace", "T123", "2020-01-01T00:00:00Z"))
				require.NoError(t, st.SetSyncState(ctx, "doctor", "threads", "coverage", "stored-status"))
				const progressQuery = "select * from sync_state where entity_type='workspace' or source_name='doctor' order by source_name,entity_id"
				beforeProgress := repairKeyRows(t, st, progressQuery)
				corrected := false
				var cursors, oldest []string
				var logs bytes.Buffer
				client := primaryOwnerClient(t, tc.tokens, func(r *http.Request, form url.Values) (any, error) {
					if r.URL.Path != "/conversations.history" {
						return primaryOwnerResponse(r.URL.Path), nil
					}
					require.Equal(t, tc.token, form.Get("token"))
					require.Equal(t, "C123", form.Get("channel"))
					require.Equal(t, "1710000200.000000", form.Get("latest"))
					cursors, oldest = append(cursors, form.Get("cursor")), append(oldest, form.Get("oldest"))
					if form.Get("cursor") == "" {
						return map[string]any{"ok": true, "messages": []any{repairKeyMessage("earlier-page", "1710000000.000000")}, "response_metadata": map[string]any{"next_cursor": "second"}}, nil
					}
					require.Equal(t, "second", form.Get("cursor"))
					if !corrected {
						if failure == "native-error" {
							return map[string]any{"ok": false, "error": "rejected-page-canary"}, nil
						}
						if failure == "decode" {
							message := repairKeyMessage("rejected-page-canary", "1710000002.000000")
							message["blocks"] = []any{map[string]any{"type": "input", "element": map[string]any{"type": "rejected-page-canary"}}}
							return map[string]any{"ok": true, "messages": []any{message}}, nil
						}
						if failure != "unsuccessful" {
							payload := map[string]any{"ok": true}
							if failure == "null" {
								payload["messages"] = nil
							}
							return payload, nil
						}
						return map[string]any{"messages": []any{repairKeyMessage("rejected-page-canary", "1710000002.000000")}}, nil
					}
					return map[string]any{"ok": true, "messages": []any{repairKeyMessage("recovered-page", "1710000002.000000")}}, nil
				}).WithLogger(testProgressLogger(&logs))
				client.now = func() time.Time { return now }
				run := func() error {
					if tc.repair {
						return client.repairWorkspace(ctx, st, "T123")
					}
					return client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123"})
				}
				runErr := run()
				require.EqualError(t, runErr, "channel C123 history: "+failureReason)
				if failure == "native-error" {
					requireNativeErrorCode(t, runErr, "rejected-page-canary")
				}
				if failure == "decode" || failure == "native-error" {
					require.Contains(t, logs.String(), "state=failed")
					require.Contains(t, logs.String(), failureReason)
					require.NotContains(t, logs.String(), "state=finished")
				}
				require.Equal(t, []string{"", "second"}, cursors)
				require.Equal(t, beforeProgress, repairKeyRows(t, st, progressQuery))
				coverage, err := readAPIHistory(ctx, st, tc.source, "T123", "C123", "")
				require.NoError(t, err)
				require.NotEmpty(t, coverage.Generation)
				require.Equal(t, store.APIHistoryState{Complete: true, Latest: "1709900000.000000", Pending: new("1709896400.000000"), Generation: coverage.Generation, PendingLatest: "1710000200.000000"}, coverage)
				require.Equal(t, []map[string]any{{"ts": "1710000000.000000", "source_name": tc.source}}, repairKeyRows(t, st, "select ts,source_name from messages order by ts"))
				require.Equal(t, []string{"1710000000.000000|FEARLIERPAGE|earlier-page.txt|UEARLIERPAGE"}, repairKeyDerived(t, st))
				assertAdmissionCanariesAbsent(t, st, logs.String()+runErr.Error(), "rejected-page-canary", "UREJECTEDPAGECANARY", "FREJECTEDPAGECANARY")
				corrected = true
				require.NoError(t, run())
				require.Equal(t, []string{"", "second", "", "second"}, cursors)
				require.Equal(t, []string{"1709896400.000000", "1709896400.000000", "1709896400.000000", "1709896400.000000"}, oldest)
				coverage, err = readAPIHistory(ctx, st, tc.source, "T123", "C123", "")
				require.NoError(t, err)
				require.Equal(t, store.APIHistoryState{Complete: true, Latest: "1710000200.000000"}, coverage)
				require.Equal(t, []map[string]any{{"ts": "1710000000.000000", "source_name": tc.source}, {"ts": "1710000002.000000", "source_name": tc.source}}, repairKeyRows(t, st, "select ts,source_name from messages order by ts"))
				require.Equal(t, []string{"1710000000.000000|FEARLIERPAGE|earlier-page.txt|UEARLIERPAGE", "1710000002.000000|FRECOVEREDPAGE|recovered-page.txt|URECOVEREDPAGE"}, repairKeyDerived(t, st))
				assertAdmissionCanariesAbsent(t, st, logs.String(), "rejected-page-canary", "UREJECTEDPAGECANARY", "FREJECTEDPAGECANARY")
				if tc.repair {
					require.Equal(t, beforeProgress, repairKeyRows(t, st, progressQuery))
				} else {
					completed, err := st.GetSyncState(ctx, tc.source, "workspace", "T123")
					require.NoError(t, err)
					require.Equal(t, now.Format(time.RFC3339), completed)
				}
			})
		}
	}
}

func TestHistoryCompletenessKeepsConcreteFailures(t *testing.T) {
	for _, mode := range []string{"request", "decode", "identity", "timestamp", "store", "thread", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			st := mustStore(t)
			defer st.Close()
			now := time.Unix(1710000200, 0).UTC()
			channel := slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C123"}}}
			require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C123", WorkspaceID: "T123", Name: "fixture", UpdatedAt: now}))
			require.NoError(t, seedAPIHistory(ctx, st, SourceBot, "T123", "C123", "", store.APIHistoryState{Complete: true, Latest: "1709900000.000000"}))
			if mode == "store" {
				_, err := st.DB().ExecContext(ctx, "create trigger reject_second before insert on messages when new.ts='1710000001.000000' begin select raise(abort,'synthetic_write_failure'); end")
				require.NoError(t, err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				payload := map[string]any{"ok": true}
				if r.URL.Path == "/conversations.replies" {
					require.Equal(t, "thread", mode)
					payload["ok"], payload["error"] = false, "synthetic_thread_failure"
				} else if r.Form.Get("cursor") == "" {
					payload["messages"] = []any{map[string]any{"channel": "C123", "ts": "1710000000.000000", "text": "first"}}
					payload["is_limited"] = true
					payload["response_metadata"] = map[string]any{"next_cursor": "second"}
				} else {
					message := map[string]any{"channel": "C123", "ts": "1710000001.000000", "text": "second"}
					payload["messages"], payload["has_more"] = []any{message}, true
					switch mode {
					case "request":
						payload["ok"], payload["error"] = false, "synthetic_request_failure"
					case "decode":
						payload["messages"] = []any{123}
					case "identity":
						message["channel"] = "COTHER"
					case "timestamp":
						message["ts"] = ""
					case "thread":
						message["reply_count"] = 1
					case "cancellation":
						cancel()
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(payload)
			}))
			defer server.Close()
			client := NewWithOptions(config.Tokens{Bot: "fixture", User: "fixture-user"}, server.URL+"/", server.Client())
			source := channelSyncSource{token: "fixture", sourceName: SourceBot, sourceRank: 2}
			err := client.syncChannelMessagesWithSource(ctx, st, "T123", channel, store.APIHistoryOptions{}, now, threadSyncSource{token: client.tokens.User, sourceName: SourceUser, sourceRank: 1}, source)
			if mode == "cancellation" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				want := map[string]string{"request": "slack conversations.history API response failed", "decode": "slack conversations.history message decode failed", "identity": "message channel does not match requested conversation", "timestamp": "message is missing a timestamp", "store": "synthetic_write_failure", "thread": "slack conversations.replies API response failed"}
				require.ErrorContains(t, err, want[mode])
				if mode == "request" || mode == "thread" {
					requireNativeErrorCode(t, err, map[string]string{"request": "synthetic_request_failure", "thread": "synthetic_thread_failure"}[mode])
				}
				if mode == "decode" {
					var typeErr *json.UnmarshalTypeError
					require.ErrorAs(t, err, &typeErr)
					require.Equal(t, "number", typeErr.Value)
				}
			}
			require.NotContains(t, err.Error(), "continuation cursor")
			require.NotContains(t, err.Error(), "uncertified")
			coverage, err := readAPIHistory(context.Background(), st, SourceBot, "T123", "C123", "")
			require.NoError(t, err)
			require.True(t, coverage.Complete)
			require.Equal(t, "1709900000.000000", coverage.Latest)
			require.NotNil(t, coverage.Pending)
			require.Equal(t, "1709896400.000000", *coverage.Pending)
			rows, err := st.QueryReadOnly(context.Background(), "select ts from messages order by ts")
			require.NoError(t, err)
			wantRows := 1
			if mode == "thread" {
				wantRows = 2
			}
			require.Len(t, rows, wantRows)
			require.Equal(t, "1710000000.000000", rows[0]["ts"])
		})
	}
}

func TestRepairCompletenessRetriesPendingInterval(t *testing.T) {
	for _, mode := range []string{"history-more", "replies-more", "limited-earlier"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			st := mustStore(t)
			defer st.Close()
			now := time.Unix(1710000200, 0).UTC()
			require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C123", WorkspaceID: "T123", Name: "fixture", Kind: "public_channel", UpdatedAt: now}))
			require.NoError(t, seedAPIHistory(ctx, st, SourceBot, "T123", "C123", "", store.APIHistoryState{Complete: true, Latest: "1709900000.000000"}))
			require.NoError(t, st.SetSyncState(ctx, SourceBot, "workspace", "T123", "2020-01-01T00:00:00Z"))
			beforeWorkspace, err := st.QueryReadOnly(ctx, "select * from sync_state where source_name='api-bot' and entity_type='workspace'")
			require.NoError(t, err)
			var corrected atomic.Bool
			var startsMu sync.Mutex
			var starts []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				payload := map[string]any{"ok": true}
				switch r.URL.Path {
				case "/auth.test":
					payload["team_id"] = "T123"
				case "/conversations.list":
					payload["channels"] = []any{map[string]any{"id": "C123", "name": "fixture", "is_channel": true}}
				case "/conversations.history":
					if r.Form.Get("cursor") == "" {
						startsMu.Lock()
						starts = append(starts, r.Form.Get("oldest"))
						startsMu.Unlock()
						payload["messages"] = []any{map[string]any{"channel": "C123", "ts": "1710000000.000000", "text": "parent", "reply_count": 1}}
						payload["response_metadata"] = map[string]any{"next_cursor": "second"}
						payload["is_limited"] = mode == "limited-earlier" && !corrected.Load()
					} else {
						payload["messages"] = []any{map[string]any{"channel": "C123", "ts": "1710000002.000000", "text": "older page"}}
						payload["has_more"] = mode == "history-more" && !corrected.Load()
					}
				case "/conversations.replies":
					payload["messages"] = []any{map[string]any{"channel": "C123", "ts": "1710000001.000000", "thread_ts": "1710000000.000000", "text": "reply"}}
					payload["has_more"] = mode == "replies-more" && !corrected.Load()
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(payload)
			}))
			defer server.Close()
			client := NewWithOptions(config.Tokens{Bot: "fixture", User: "fixture-user"}, server.URL+"/", server.Client())
			client.now = func() time.Time { return now }
			wantError := "has_more without a continuation cursor"
			if mode == "limited-earlier" {
				wantError = "completeness of the requested interval is uncertified"
			}
			require.ErrorContains(t, client.repairWorkspace(ctx, st, "T123"), wantError)
			coverage, err := readAPIHistory(ctx, st, SourceBot, "T123", "C123", "")
			require.NoError(t, err)
			require.True(t, coverage.Complete)
			require.Equal(t, "1709900000.000000", coverage.Latest)
			require.NotNil(t, coverage.Pending)
			require.Equal(t, "1709896400.000000", *coverage.Pending)
			rows, err := st.QueryReadOnly(ctx, "select ts from messages order by ts")
			require.NoError(t, err)
			wantRows := 3
			if mode == "replies-more" {
				wantRows = 2
			}
			require.Len(t, rows, wantRows)
			corrected.Store(true)
			require.NoError(t, client.repairWorkspace(ctx, st, "T123"))
			startsMu.Lock()
			gotStarts := append([]string(nil), starts...)
			startsMu.Unlock()
			require.Equal(t, []string{"1709896400.000000", "1709896400.000000"}, gotStarts)
			coverage, err = readAPIHistory(ctx, st, SourceBot, "T123", "C123", "")
			require.NoError(t, err)
			require.True(t, coverage.Complete)
			require.Nil(t, coverage.Pending)
			require.Equal(t, "1710000200.000000", coverage.Latest)
			rows, err = st.QueryReadOnly(ctx, "select ts from messages order by ts")
			require.NoError(t, err)
			require.Equal(t, []map[string]any{{"ts": "1710000000.000000"}, {"ts": "1710000001.000000"}, {"ts": "1710000002.000000"}}, rows)
			afterWorkspace, err := st.QueryReadOnly(ctx, "select * from sync_state where source_name='api-bot' and entity_type='workspace'")
			require.NoError(t, err)
			require.Equal(t, beforeWorkspace, afterWorkspace, "periodic repair does not publish the ordinary workspace success marker")
		})
	}
}

func TestCatalogPagesRequireExplicitSuccess(t *testing.T) {
	for _, method := range []string{"conversations.list", "users.list"} {
		for _, status := range []struct{ name, field string }{{"missing", ""}, {"null", `"ok":null,`}, {"false", `"ok":false,`}} {
			for _, detail := range []struct{ name, field string }{{"missing", ""}, {"null", `"error":null,`}, {"empty", `"error":"",`}, {"blank", `"error":" \t ",`}} {
				t.Run(method+"/"+status.name+"/"+detail.name, func(t *testing.T) {
					calls := 0
					client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot"}, func(r *http.Request, form url.Values) (any, error) {
						calls++
						require.Equal(t, "/"+method, r.URL.Path)
						require.Equal(t, "fixture-bot", form.Get("token"))
						return json.RawMessage(`{` + status.field + detail.field + `"channels":[{"id":"CREJECTED","name":"catalog-canary"}],"members":[{"id":"UREJECTED","name":"catalog-canary"}],"response_metadata":{"next_cursor":"untrusted-cursor"}}`), nil
					})
					var err error
					if method == "conversations.list" {
						var rows []slack.Channel
						var cursor string
						rows, cursor, err = client.getConversations(context.Background(), client.tokens.Bot, &slack.GetConversationsParameters{})
						require.Nil(t, rows)
						require.Empty(t, cursor)
					} else {
						var rows []slack.User
						rows, err = client.getUsers(context.Background(), client.tokens.Bot)
						require.Nil(t, rows)
					}
					require.EqualError(t, err, method+" response did not report success")
					require.NotContains(t, err.Error(), "catalog-canary")
					require.Equal(t, 1, calls, "unsuccessful pages neither retry nor expose their cursor")
				})
			}
		}
	}

	for _, method := range []string{"conversations.list", "users.list"} {
		for _, tc := range pageCollectionCases {
			t.Run("collection/"+method+"/"+tc.name, func(t *testing.T) {
				calls := 0
				client := primaryOwnerClient(t, config.Tokens{User: "fixture"}, func(r *http.Request, _ url.Values) (any, error) {
					calls++
					require.Equal(t, "/"+method, r.URL.Path)
					payload := map[string]any{"ok": tc.success, "error": tc.errorText}
					field := "channels"
					if method == "users.list" {
						field = "members"
					}
					if tc.value != "" {
						payload[field] = json.RawMessage(tc.value)
					}
					if tc.value != "[]" {
						payload["response_metadata"] = map[string]any{"next_cursor": "untrusted-cursor"}
					}
					return payload, nil
				})
				var err error
				if method == "conversations.list" {
					rows, cursor, callErr := client.getConversations(context.Background(), "fixture", &slack.GetConversationsParameters{})
					err = callErr
					require.Empty(t, cursor)
					if tc.value == "[]" {
						require.NotNil(t, rows)
						require.Empty(t, rows)
					} else {
						require.Nil(t, rows)
					}
				} else {
					rows, callErr := client.getUsers(context.Background(), "fixture")
					err = callErr
					require.Nil(t, rows, "empty successful users retain the nil accumulator")
				}
				if tc.value == "[]" {
					require.NoError(t, err, "explicit empty arrays preserve Ok=true's existing error short-circuit")
				} else if !tc.success {
					require.EqualError(t, err, method+" response did not report success")
				} else if tc.value == "" || tc.value == "null" {
					require.EqualError(t, err, method+" response did not provide a collection array; page remains uncertified")
				} else {
					var typeErr *json.UnmarshalTypeError
					require.ErrorAs(t, err, &typeErr)
				}
				if err != nil {
					require.NotContains(t, err.Error(), "collection-canary")
				}
				require.Equal(t, 1, calls, "rejected pages neither retry nor expose their cursor")
			})
		}
	}
}

func TestCatalogPagesPreserveNativeErrors(t *testing.T) {
	for _, method := range []string{"conversations.list", "users.list"} {
		for _, collection := range []string{"records", "absent", "null"} {
			t.Run(method+"/"+collection, func(t *testing.T) {
				calls := 0
				client := primaryOwnerClient(t, config.Tokens{User: "fixture-user"}, func(r *http.Request, _ url.Values) (any, error) {
					calls++
					require.Equal(t, "/"+method, r.URL.Path)
					payload := map[string]any{"error": "missing_scope", "errors": []string{"native-detail"}, "response_metadata": map[string]any{"next_cursor": "untrusted-cursor"}}
					if collection == "records" {
						payload["channels"] = []any{map[string]any{"id": "CREJECTED"}}
						payload["members"] = []any{map[string]any{"id": "UREJECTED"}}
					} else if collection == "null" {
						payload["channels"], payload["members"] = nil, nil
					}
					return payload, nil
				})
				var err error
				if method == "conversations.list" {
					rows, cursor, callErr := client.getConversations(context.Background(), client.tokens.User, &slack.GetConversationsParameters{})
					require.Nil(t, rows)
					require.Empty(t, cursor)
					err = callErr
				} else {
					rows, callErr := client.getUsers(context.Background(), client.tokens.User)
					require.Nil(t, rows)
					err = callErr
				}
				var native slack.SlackErrorResponse
				require.ErrorAs(t, err, &native)
				require.Equal(t, "missing_scope", native.Err)
				require.Equal(t, []slack.SlackResponseErrors{{Message: new("native-detail")}}, native.Errors)
				require.Equal(t, 1, calls)
			})
		}
	}
}

func TestCatalogPagesValidateWholeBody(t *testing.T) {
	for _, method := range []string{"conversations.list", "users.list"} {
		for _, failure := range []string{"trailing-json", "read-error"} {
			t.Run(method+"/"+failure, func(t *testing.T) {
				readErr := errors.New("synthetic catalog read failure")
				calls := 0
				client := NewWithOptions(config.Tokens{Bot: "fixture-bot"}, "https://fixture.invalid/", &http.Client{Transport: primaryOwnerRoundTrip(func(r *http.Request) (*http.Response, error) {
					calls++
					require.Equal(t, "/"+method, r.URL.Path)
					payload := `{"ok":true,"channels":[{"id":"CREJECTED","name":"catalog-canary"}],"members":[{"id":"UREJECTED","name":"catalog-canary"}],"response_metadata":{"next_cursor":"untrusted-cursor"}}`
					var body io.Reader = strings.NewReader(payload + ` {"ok":true}`)
					if failure == "read-error" {
						body = io.MultiReader(strings.NewReader(payload), iotest.ErrReader(readErr))
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body), Request: r}, nil
				})})
				var err error
				if method == "conversations.list" {
					rows, cursor, callErr := client.getConversations(context.Background(), client.tokens.Bot, &slack.GetConversationsParameters{})
					require.Nil(t, rows)
					require.Empty(t, cursor)
					err = callErr
				} else {
					rows, callErr := client.getUsers(context.Background(), client.tokens.Bot)
					require.Nil(t, rows)
					err = callErr
				}
				if failure == "read-error" {
					require.ErrorIs(t, err, readErr)
				} else {
					var syntax *json.SyntaxError
					require.ErrorAs(t, err, &syntax)
				}
				require.NotContains(t, err.Error(), "catalog-canary")
				require.Equal(t, 1, calls)
			})
		}
	}
}

func TestNativePageStatusErrors(t *testing.T) {
	for _, method := range []string{"conversations.list", "users.list", "conversations.history", "conversations.replies"} {
		for _, code := range []int{http.StatusBadGateway, http.StatusTooManyRequests} {
			t.Run(fmt.Sprintf("%s/%d", method, code), func(t *testing.T) {
				calls := 0
				status := fmt.Sprintf("%d %s", code, http.StatusText(code))
				client := NewWithOptions(config.Tokens{User: "fixture-user"}, "https://fixture.invalid/", &http.Client{Transport: primaryOwnerRoundTrip(func(r *http.Request) (*http.Response, error) {
					calls++
					require.Equal(t, "/"+method, r.URL.Path)
					return &http.Response{StatusCode: code, Status: status, Body: io.NopCloser(strings.NewReader("unparsed-status-canary")), Request: r}, nil
				})})
				client.sleep = func(context.Context, time.Duration) error { t.Fatal("status errors must not add retries"); return nil }
				var err error
				switch method {
				case "conversations.list":
					rows, cursor, callErr := client.getConversations(context.Background(), client.tokens.User, &slack.GetConversationsParameters{})
					require.Nil(t, rows)
					require.Empty(t, cursor)
					err = callErr
				case "users.list":
					rows, callErr := client.getUsers(context.Background(), client.tokens.User)
					require.Nil(t, rows)
					err = callErr
				case "conversations.history":
					page, callErr := client.getConversationHistory(context.Background(), client.tokens.User, &slack.GetConversationHistoryParameters{ChannelID: "C123"}, nil)
					require.Nil(t, page)
					err = callErr
				case "conversations.replies":
					page, callErr := client.getConversationReplies(context.Background(), client.tokens.User, &slack.GetConversationRepliesParameters{ChannelID: "C123", Timestamp: "1710000000.000000"}, nil)
					require.Nil(t, page)
					err = callErr
				}
				var native slack.StatusCodeError
				require.ErrorAs(t, err, &native)
				require.Equal(t, slack.StatusCodeError{Code: code, Status: status}, native)
				require.NotContains(t, err.Error(), "unparsed-status-canary")
				require.Equal(t, 1, calls, "429 without Retry-After remains a status error")
			})
		}
	}
}

func TestCatalogFormsAndEmptyPages(t *testing.T) {
	for _, tc := range []struct {
		name    string
		params  slack.GetConversationsParameters
		form    url.Values
		payload string
	}{
		{name: "defaults", form: url.Values{"token": {"fixture-token"}}, payload: `{"ok":true,"channels":[]}`},
		{name: "empty-types", params: slack.GetConversationsParameters{Types: []string{}}, form: url.Values{"token": {"fixture-token"}, "types": {""}}, payload: `{"ok":true,"channels":[]}`},
		{name: "explicit", params: slack.GetConversationsParameters{Cursor: "second", Limit: 17, Types: []string{"public_channel", "private_channel"}, ExcludeArchived: true, TeamID: "T123"}, form: url.Values{"token": {"fixture-token"}, "cursor": {"second"}, "limit": {"17"}, "types": {"public_channel,private_channel"}, "exclude_archived": {"true"}, "team_id": {"T123"}}, payload: `{"ok":true,"channels":[],"response_metadata":{"next_cursor":"third"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot"}, func(r *http.Request, form url.Values) (any, error) {
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "/conversations.list", r.URL.Path)
				require.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
				require.Empty(t, r.Header.Get("Authorization"))
				require.Equal(t, tc.form, form)
				return json.RawMessage(tc.payload), nil
			})
			rows, cursor, err := client.getConversations(context.Background(), "fixture-token", &tc.params)
			require.NoError(t, err)
			require.Empty(t, rows)
			require.NotNil(t, rows)
			wantCursor := ""
			if tc.name == "explicit" {
				wantCursor = "third"
			}
			require.Equal(t, wantCursor, cursor)
		})
	}
	t.Run("users-empty-pagination", func(t *testing.T) {
		var cursors []string
		client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot"}, func(r *http.Request, form url.Values) (any, error) {
			require.Equal(t, http.MethodPost, r.Method)
			require.Equal(t, "/users.list", r.URL.Path)
			require.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
			require.Empty(t, r.Header.Get("Authorization"))
			require.Equal(t, url.Values{"token": {"fixture-token"}, "limit": {"200"}, "presence": {"false"}, "cursor": {form.Get("cursor")}, "team_id": {""}, "include_locale": {"true"}}, form)
			cursors = append(cursors, form.Get("cursor"))
			if len(cursors) == 1 {
				return json.RawMessage(`{"ok":true,"members":[],"response_metadata":{"next_cursor":"second"}}`), nil
			}
			return json.RawMessage(`{"ok":true,"members":[]}`), nil
		})
		rows, err := client.getUsers(context.Background(), "fixture-token")
		require.NoError(t, err)
		require.Nil(t, rows, "preserve the accumulator's nil result for empty successful catalogs")
		require.Equal(t, []string{"", "second"}, cursors)
	})
}

func TestCatalogRetryBudgetAndCancellation(t *testing.T) {
	for _, method := range []string{"conversations.list", "users.list"} {
		for _, mode := range []string{"retry", "cancel", "exhausted"} {
			t.Run(method+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var cursors []string
				sleeps := 0
				client := NewWithOptions(config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, "https://fixture.invalid/", &http.Client{Transport: primaryOwnerRoundTrip(func(r *http.Request) (*http.Response, error) {
					require.NoError(t, r.ParseForm())
					require.Equal(t, "/"+method, r.URL.Path)
					require.Equal(t, "fixture-user", r.Form.Get("token"))
					cursors = append(cursors, r.Form.Get("cursor"))
					payload := `{"ok":true,"channels":[],"members":[]}`
					if len(cursors) == 1 {
						payload = `{"ok":true,"channels":[],"members":[],"response_metadata":{"next_cursor":"second"}}`
					} else if len(cursors) == 2 || mode == "exhausted" {
						return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"2"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(payload)), Request: r}, nil
				})})
				client.sleep = func(ctx context.Context, delay time.Duration) error {
					sleeps++
					require.Equal(t, 2*time.Second, delay)
					if mode == "cancel" {
						cancel()
					}
					return ctx.Err()
				}
				var err error
				if method == "conversations.list" {
					rows, callErr := client.fetchDMs(ctx, "T123", nil)
					require.Nil(t, rows)
					err = callErr
				} else {
					rows, callErr := client.getUsers(ctx, client.tokens.User)
					require.Nil(t, rows)
					err = callErr
				}
				switch mode {
				case "retry":
					require.NoError(t, err)
					require.Equal(t, []string{"", "second", "second"}, cursors)
					require.Equal(t, 1, sleeps)
				case "cancel":
					require.ErrorIs(t, err, context.Canceled)
					require.Equal(t, []string{"", "second"}, cursors)
					require.Equal(t, 1, sleeps)
				case "exhausted":
					var limited *slack.RateLimitedError
					require.ErrorAs(t, err, &limited)
					require.Equal(t, 2*time.Second, limited.RetryAfter)
					require.Equal(t, []string{"", "second", "second", "second"}, cursors)
					require.Equal(t, 2, sleeps, "DMs share one three-attempt page retry owner")
				}
			})
		}
	}
}

func TestChannelCatalogFailureRestartsBeforeWrites(t *testing.T) {
	for _, failure := range []string{"unsuccessful", "absent", "null"} {
		failureReason := "response did not report success"
		if failure != "unsuccessful" {
			failureReason = "response did not provide a collection array; page remains uncertified"
		}
		for _, owner := range []string{"bot", "user", "repair"} {
			t.Run(owner+"/"+failure, func(t *testing.T) {
				ctx := context.Background()
				st := mustStore(t)
				defer func() { require.NoError(t, st.Close()) }()
				tokens := config.Tokens{Bot: "fixture-bot", User: "fixture-user"}
				source, token := SourceBot, tokens.Bot
				if owner == "user" {
					tokens.Bot, source, token = "", SourceUser, tokens.User
				}
				require.NoError(t, st.SetSyncState(ctx, source, "workspace", "T123", "old-success"))
				require.NoError(t, st.SetSyncState(ctx, "doctor", "threads", "coverage", "old-coverage"))
				require.NoError(t, st.SetSyncState(ctx, SourceUser, "thread_skip", "T123|legacy", "old-skip"))
				before := map[string][]map[string]any{}
				for _, table := range admissionTables {
					if table != "workspaces" || owner == "repair" {
						before[table] = repairKeyRows(t, st, "select * from "+table)
					}
				}
				corrected := false
				var cursors []string
				histories := 0
				client := primaryOwnerClient(t, tokens, func(r *http.Request, form url.Values) (any, error) {
					switch r.URL.Path {
					case "/conversations.list":
						want := url.Values{"token": {token}, "limit": {"200"}, "types": {"public_channel,private_channel"}, "team_id": {"T123"}}
						if form.Get("cursor") != "" {
							want.Set("cursor", "second")
						}
						require.Equal(t, want, form)
						cursors = append(cursors, form.Get("cursor"))
						if form.Get("cursor") == "" {
							return json.RawMessage(`{"ok":true,"channels":[{"id":"C123","name":"uncommitted-first-page","is_channel":true}],"response_metadata":{"next_cursor":"second"}}`), nil
						}
						if !corrected {
							if failure != "unsuccessful" {
								payload := map[string]any{"ok": true}
								if failure == "null" {
									payload["channels"] = nil
								}
								return payload, nil
							}
							return json.RawMessage(`{"ok":false,"channels":[{"id":"CREJECTED","name":"rejected-catalog-canary","is_channel":true}],"response_metadata":{"next_cursor":"untrusted-cursor"}}`), nil
						}
						return json.RawMessage(`{"ok":true,"channels":[{"id":"C456","name":"recovered","is_channel":true}]}`), nil
					case "/conversations.history":
						histories++
						require.Equal(t, token, form.Get("token"))
					case "/users.list":
						require.Equal(t, token, form.Get("token"))
					}
					return primaryOwnerResponse(r.URL.Path), nil
				})
				now := time.Unix(1710000200, 0).UTC()
				client.now = func() time.Time { return now }
				run := func() error {
					if owner == "repair" {
						return client.repairWorkspace(ctx, st, "T123")
					}
					return client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123", Full: true})
				}
				require.EqualError(t, run(), "conversations.list "+failureReason)
				require.Equal(t, []string{"", "second"}, cursors)
				require.Zero(t, histories, "the whole channel catalog precedes history")
				for table, rows := range before {
					require.Equal(t, rows, repairKeyRows(t, st, "select * from "+table), table)
				}
				assertAdmissionCanariesAbsent(t, st, "", "uncommitted-first-page", "rejected-catalog-canary", "CREJECTED")
				corrected = true
				require.NoError(t, run())
				require.Equal(t, []string{"", "second", "", "second"}, cursors)
				require.Equal(t, 2, histories)
				require.Equal(t, []map[string]any{{"id": "C123"}, {"id": "C456"}}, repairKeyRows(t, st, "select id from channels order by id"))
				assertAdmissionCanariesAbsent(t, st, "", "rejected-catalog-canary", "CREJECTED")
				marker, err := st.GetSyncState(ctx, source, "workspace", "T123")
				require.NoError(t, err)
				wantMarker := now.Format(time.RFC3339)
				if owner == "repair" {
					wantMarker = "old-success"
				}
				require.Equal(t, wantMarker, marker)
			})
		}
	}
}

func TestCatalogFailurePreservesCompletedPublicWork(t *testing.T) {
	for _, failure := range []string{"unsuccessful", "absent", "null"} {
		failureReason := "response did not report success"
		if failure != "unsuccessful" {
			failureReason = "response did not provide a collection array; page remains uncertified"
		}
		for _, owner := range []string{"bot", "user"} {
			for _, failedCatalog := range []string{"users", "dms"} {
				t.Run(owner+"/"+failedCatalog+"/"+failure, func(t *testing.T) {
					ctx := context.Background()
					st := mustStore(t)
					defer func() { require.NoError(t, st.Close()) }()
					tokens := config.Tokens{Bot: "fixture-bot", User: "fixture-user"}
					source, primary := SourceBot, tokens.Bot
					if owner == "user" {
						tokens.Bot, source, primary = "", SourceUser, tokens.User
					}
					require.NoError(t, st.SetSyncState(ctx, source, "workspace", "T123", "old-success"))
					require.NoError(t, st.SetSyncState(ctx, "doctor", "threads", "coverage", "old-coverage"))
					require.NoError(t, st.SetSyncState(ctx, SourceUser, "thread_skip", "T123|legacy", "old-skip"))
					const finalQuery = "select * from sync_state where entity_type='workspace' or source_name='doctor' or entity_type='thread_skip' order by source_name,entity_type,entity_id"
					before := repairKeyRows(t, st, finalQuery)
					corrected := false
					var usersCursors, dmCursors, historyChannels []string
					client := primaryOwnerClient(t, tokens, func(r *http.Request, form url.Values) (any, error) {
						switch r.URL.Path {
						case "/conversations.list":
							if form.Get("types") != "im,mpim" {
								require.Equal(t, primary, form.Get("token"))
								return primaryOwnerResponse(r.URL.Path), nil
							}
							want := url.Values{"token": {tokens.User}, "limit": {"200"}, "types": {"im,mpim"}, "team_id": {"T123"}}
							if form.Get("cursor") != "" {
								want.Set("cursor", "second")
							}
							require.Equal(t, want, form)
							dmCursors = append(dmCursors, form.Get("cursor"))
							if failedCatalog == "users" {
								return json.RawMessage(`{"ok":true,"channels":[]}`), nil
							}
							if form.Get("cursor") == "" {
								return json.RawMessage(`{"ok":true,"channels":[{"id":"DGOOD","is_im":true,"is_private":true,"user":"U123"}],"response_metadata":{"next_cursor":"second"}}`), nil
							}
							if !corrected {
								if failure != "unsuccessful" {
									payload := map[string]any{"ok": true}
									if failure == "null" {
										payload["channels"] = nil
									}
									return payload, nil
								}
								return json.RawMessage(`{"ok":null,"channels":[]}`), nil
							}
							return json.RawMessage(`{"ok":true,"channels":[{"id":"DRECOVERED","is_im":true,"is_private":true,"user":"U123"}]}`), nil
						case "/users.list":
							require.Equal(t, url.Values{"token": {primary}, "limit": {"200"}, "presence": {"false"}, "cursor": {form.Get("cursor")}, "team_id": {""}, "include_locale": {"true"}}, form)
							usersCursors = append(usersCursors, form.Get("cursor"))
							if failedCatalog == "dms" {
								return json.RawMessage(`{"ok":true,"members":[{"id":"U123","name":"fixture-user"}]}`), nil
							}
							if form.Get("cursor") == "" {
								return json.RawMessage(`{"ok":true,"members":[{"id":"U123","name":"fixture-user"}],"response_metadata":{"next_cursor":"second"}}`), nil
							}
							require.Equal(t, "second", form.Get("cursor"))
							if !corrected {
								if failure != "unsuccessful" {
									payload := map[string]any{"ok": true}
									if failure == "null" {
										payload["members"] = nil
									}
									return payload, nil
								}
								return json.RawMessage(`{"members":[{"id":"UREJECTED","name":"rejected-catalog-canary"}],"response_metadata":{"next_cursor":"untrusted-cursor"}}`), nil
							}
							return json.RawMessage(`{"ok":true,"members":[{"id":"URECOVERED","name":"recovered-user"}]}`), nil
						case "/conversations.history":
							channel := form.Get("channel")
							historyChannels = append(historyChannels, channel)
							if channel == "C123" {
								require.Equal(t, primary, form.Get("token"))
								return map[string]any{"ok": true, "messages": []any{repairKeyMessage("completed-public", "1710000000.000000")}}, nil
							}
							require.Contains(t, []string{"DGOOD", "DRECOVERED"}, channel)
							require.Equal(t, tokens.User, form.Get("token"))
						}
						return primaryOwnerResponse(r.URL.Path), nil
					}).WithDMPolicy(admission.Include)
					now := time.Unix(1710000200, 0).UTC()
					client.now = func() time.Time { return now }
					wantError := "users.list " + failureReason
					if failedCatalog == "dms" {
						wantError = "conversations.list " + failureReason
					}
					require.EqualError(t, client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123", Full: true}), wantError)
					require.Equal(t, before, repairKeyRows(t, st, finalQuery))
					require.Equal(t, []string{"C123"}, historyChannels)
					require.Equal(t, []map[string]any{{"id": "C123"}}, repairKeyRows(t, st, "select id from channels"))
					require.Empty(t, repairKeyRows(t, st, "select * from users"), "neither a partial nor a complete user catalog is persisted before the failed DM phase")
					require.Equal(t, []map[string]any{{"channel_id": "C123", "ts": "1710000000.000000", "source_name": source}}, repairKeyRows(t, st, "select channel_id,ts,source_name from messages"))
					require.Equal(t, []string{"1710000000.000000|FCOMPLETEDPUBLIC|completed-public.txt|UCOMPLETEDPUBLIC"}, repairKeyDerived(t, st))
					coverage, err := readAPIHistory(ctx, st, source, "T123", "C123", "")
					require.NoError(t, err)
					require.Equal(t, store.APIHistoryState{Complete: true, Latest: "1710000200.000000"}, coverage, "the completed public interval survives later catalog failure")
					assertAdmissionCanariesAbsent(t, st, "", "rejected-catalog-canary", "UREJECTED")
					corrected = true
					require.NoError(t, client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123", Full: true}))
					if failedCatalog == "users" {
						require.Equal(t, []string{"", "second", "", "second"}, usersCursors)
						require.Equal(t, []string{""}, dmCursors)
						require.Equal(t, []string{"C123", "C123"}, historyChannels)
						require.Equal(t, []map[string]any{{"id": "U123"}, {"id": "URECOVERED"}}, repairKeyRows(t, st, "select id from users order by id"))
					} else {
						require.Equal(t, []string{"", ""}, usersCursors)
						require.Equal(t, []string{"", "second", "", "second"}, dmCursors)
						require.Equal(t, []string{"C123", "C123", "DGOOD", "DRECOVERED"}, historyChannels)
						require.Equal(t, []map[string]any{{"id": "U123"}}, repairKeyRows(t, st, "select id from users"))
					}
					marker, err := st.GetSyncState(ctx, source, "workspace", "T123")
					require.NoError(t, err)
					require.Equal(t, now.Format(time.RFC3339), marker)
					coverageValue, err := st.GetSyncState(ctx, "doctor", "threads", "coverage")
					require.NoError(t, err)
					require.Equal(t, "full", coverageValue)
					require.Empty(t, repairKeyRows(t, st, "select * from sync_state where entity_type='thread_skip'"))
					assertAdmissionCanariesAbsent(t, st, "", "rejected-catalog-canary", "UREJECTED")
				})
			}
		}
	}
}

func TestEmptyCatalogSyncKeepsUserRefetch(t *testing.T) {
	for _, owner := range []string{"bot", "user"} {
		t.Run(owner, func(t *testing.T) {
			tokens := config.Tokens{Bot: "fixture-bot", User: "fixture-user"}
			primary := tokens.Bot
			if owner == "user" {
				tokens.Bot, primary = "", tokens.User
			}
			var requests []string
			client := primaryOwnerClient(t, tokens, func(r *http.Request, form url.Values) (any, error) {
				if r.URL.Path == "/auth.test" {
					return primaryOwnerResponse(r.URL.Path), nil
				}
				requests = append(requests, r.URL.Path+":"+form.Get("types")+":"+form.Get("token"))
				switch r.URL.Path {
				case "/conversations.list":
					return json.RawMessage(`{"ok":true,"channels":[]}`), nil
				case "/users.list":
					return json.RawMessage(`{"ok":true,"members":[]}`), nil
				default:
					t.Fatalf("unexpected endpoint %s", r.URL.Path)
					return nil, nil
				}
			}).WithDMPolicy(admission.Include)
			st := mustStore(t)
			defer func() { require.NoError(t, st.Close()) }()
			ctx := context.Background()
			require.NoError(t, st.SetSyncState(ctx, SourceUser, "thread_skip", "T123|legacy", "old-skip"))
			require.NoError(t, client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123", Full: true}))
			require.Equal(t, []string{"/conversations.list:public_channel,private_channel:" + primary, "/users.list::" + primary, "/conversations.list:im,mpim:" + tokens.User, "/users.list::" + primary}, requests)
			for _, table := range []string{"channels", "users", "messages"} {
				require.Empty(t, repairKeyRows(t, st, "select * from "+table))
			}
			coverage, err := st.GetSyncState(ctx, "doctor", "threads", "coverage")
			require.NoError(t, err)
			require.Equal(t, "full", coverage)
			require.Empty(t, repairKeyRows(t, st, "select * from sync_state where entity_type='thread_skip'"))
		})
	}
}

func TestNativeResponsesRequireExplicitSuccess(t *testing.T) {
	for _, method := range []string{"auth.test", "conversations.info", "conversations.join"} {
		for _, status := range []struct{ name, field string }{{"missing", ""}, {"null", `"ok":null,`}, {"false", `"ok":false,`}} {
			for _, detail := range []struct{ name, field string }{{"missing", ""}, {"null", `"error":null,`}, {"empty", `"error":"",`}, {"blank", `"error":" \t ",`}} {
				t.Run(method+"/"+status.name+"/"+detail.name, func(t *testing.T) {
					calls := 0
					client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, func(r *http.Request, form url.Values) (any, error) {
						calls++
						require.Equal(t, "/"+method, r.URL.Path)
						require.Equal(t, http.MethodPost, r.Method)
						require.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
						require.Empty(t, r.Header.Get("Authorization"))
						wantForm := url.Values{"token": {"fixture-bot"}, "channel": {"C123"}}
						switch method {
						case "auth.test":
							wantForm = url.Values{"token": {"fixture-user"}}
						case "conversations.info":
							wantForm.Set("include_locale", "false")
							wantForm.Set("include_num_members", "false")
						}
						require.Equal(t, wantForm, form)
						return json.RawMessage(`{` + status.field + detail.field + `"team_id":"T123","team":"native-response-canary","user_id":"U123","channel":{"id":"C123","is_channel":true,"name":"native-response-canary","latest":{"text":"native-response-canary"}}}`), nil
					})
					err := nativeResponseCall(t, context.Background(), client, method)
					require.EqualError(t, err, method+" response did not report success")
					require.NotContains(t, err.Error(), "native-response-canary")
					require.Equal(t, 1, calls)
				})
			}
		}
	}
}

func TestNativeResponsesPreserveSuccessAndErrors(t *testing.T) {
	for _, method := range []string{"auth.test", "conversations.info", "conversations.join"} {
		for _, success := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/success=%t", method, success), func(t *testing.T) {
				client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, func(r *http.Request, _ url.Values) (any, error) {
					require.Equal(t, "/"+method, r.URL.Path)
					if success {
						// Info retains its zero Channel and join accepts a missing channel,
						// as the SDK did. This boundary does not certify payload shape.
						return json.RawMessage(`{"ok":true}`), nil
					}
					return json.RawMessage(`{"ok":false,"error":"missing_scope","errors":["native-detail"],"response_metadata":{"warnings":["native-warning"]}}`), nil
				})
				err := nativeResponseCall(t, context.Background(), client, method)
				if success {
					require.NoError(t, err)
				} else {
					var native slack.SlackErrorResponse
					require.ErrorAs(t, err, &native)
					require.Equal(t, "missing_scope", native.Err)
					require.Equal(t, []slack.SlackResponseErrors{{Message: new("native-detail")}}, native.Errors)
					if method == "auth.test" {
						require.Equal(t, []string{"native-warning"}, native.ResponseMetadata.Warnings)
					} else {
						// These SDK-compatible DTOs own an outer response_metadata field.
						require.Empty(t, native.ResponseMetadata)
					}
				}
			})
		}
	}
}

func TestNativeResponseRetryAndStatus(t *testing.T) {
	for _, method := range []string{"auth.test", "conversations.info", "conversations.join"} {
		for _, outcome := range []string{"retry", "cancel", "status"} {
			t.Run(method+"/"+outcome, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls, sleeps := 0, 0
				client := NewWithOptions(config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, "https://fixture.invalid/", &http.Client{Transport: primaryOwnerRoundTrip(func(r *http.Request) (*http.Response, error) {
					calls++
					require.Equal(t, "/"+method, r.URL.Path)
					response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: r}
					if calls == 1 {
						response.StatusCode = http.StatusTooManyRequests
						response.Header.Set("Retry-After", "2")
					}
					if outcome == "status" {
						response.StatusCode, response.Status = http.StatusBadGateway, "502 Bad Gateway"
					}
					return response, nil
				})})
				client.sleep = func(ctx context.Context, delay time.Duration) error {
					sleeps++
					require.Equal(t, 2*time.Second, delay)
					if outcome == "cancel" {
						cancel()
						return ctx.Err()
					}
					return nil
				}
				err := nativeResponseCall(t, ctx, client, method)
				switch outcome {
				case "retry":
					require.NoError(t, err)
					require.Equal(t, 2, calls)
					require.Equal(t, 1, sleeps)
				case "cancel":
					require.ErrorIs(t, err, context.Canceled)
					require.Equal(t, 1, calls)
					require.Equal(t, 1, sleeps)
				case "status":
					var status slack.StatusCodeError
					require.ErrorAs(t, err, &status)
					require.Equal(t, slack.StatusCodeError{Code: 502, Status: "502 Bad Gateway"}, status)
					require.Equal(t, 1, calls)
					require.Zero(t, sleeps)
				}
			})
		}
	}
}

func TestNativeResponsesValidateWholeBody(t *testing.T) {
	for _, method := range []string{"auth.test", "conversations.info", "conversations.join"} {
		for _, failure := range []string{"trailing-json", "read-error"} {
			t.Run(method+"/"+failure, func(t *testing.T) {
				readErr := errors.New("synthetic native response read failure")
				calls := 0
				client := NewWithOptions(config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, "https://fixture.invalid/", &http.Client{Transport: primaryOwnerRoundTrip(func(r *http.Request) (*http.Response, error) {
					calls++
					require.Equal(t, "/"+method, r.URL.Path)
					payload := `{"ok":true,"team_id":"T123","team":"native-body-canary","channel":{"id":"C123","is_channel":true,"name":"native-body-canary"}}`
					var body io.Reader = strings.NewReader(payload + ` {"ok":true}`)
					if failure == "read-error" {
						body = io.MultiReader(strings.NewReader(payload), iotest.ErrReader(readErr))
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body), Request: r}, nil
				})})
				err := nativeResponseCall(t, context.Background(), client, method)
				if failure == "read-error" {
					require.ErrorIs(t, err, readErr)
				} else {
					var syntax *json.SyntaxError
					require.ErrorAs(t, err, &syntax)
				}
				require.NotContains(t, err.Error(), "native-body-canary")
				require.Equal(t, 1, calls)
			})
		}
	}
}

func TestNativeAuthHeadersArePrivateCopies(t *testing.T) {
	headers := http.Header{"X-Fixture-Header": {"private-header-canary"}}
	client := NewWithOptions(config.Tokens{Bot: "fixture-bot"}, "https://fixture.invalid/", &http.Client{Transport: primaryOwnerRoundTrip(func(r *http.Request) (*http.Response, error) {
		require.NoError(t, r.ParseForm())
		require.Equal(t, url.Values{"token": {"selected-auth-token"}}, r.Form)
		return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(`{"ok":true,"team_id":"T123","user_id":"U123","team":"fixture","bot_id":"B123","url":"https://fixture.invalid/","enterprise_id":"E123"}`)), Request: r}, nil
	})})
	auth, err := client.authTest(context.Background(), "selected-auth-token")
	require.NoError(t, err)
	require.Equal(t, &slack.AuthTestResponse{TeamID: "T123", UserID: "U123", Team: "fixture", BotID: "B123", URL: "https://fixture.invalid/", EnterpriseID: "E123", Header: headers}, auth)
	headers["X-Fixture-Header"][0] = "changed"
	require.Equal(t, "private-header-canary", auth.Header.Get("X-Fixture-Header"))
	auth.Header.Set("X-Other", "auth-owned")
	require.Empty(t, headers.Get("X-Other"))
	encoded, err := json.Marshal(auth)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private-header-canary")
	require.NotContains(t, string(encoded), "auth-owned")
	require.NotContains(t, string(encoded), "Header")
}

func TestNativeConversationInfoRequiresChannel(t *testing.T) {
	client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot"}, func(*http.Request, url.Values) (any, error) {
		t.Fatal("empty channel must not reach HTTP")
		return nil, nil
	})
	channel, err := client.getConversationInfo(context.Background(), "")
	require.Nil(t, channel)
	require.EqualError(t, err, "ChannelID must be defined")
}

func nativeResponseCall(t *testing.T, ctx context.Context, client *Client, method string) error {
	t.Helper()
	switch method {
	case "auth.test":
		response, err := client.authTest(ctx, client.tokens.User)
		if err != nil {
			require.Nil(t, response)
		} else {
			require.NotNil(t, response)
		}
		return err
	case "conversations.info":
		response, err := client.getConversationInfo(ctx, "C123")
		if err != nil {
			require.Nil(t, response)
		} else {
			require.NotNil(t, response)
		}
		return err
	case "conversations.join":
		return client.joinConversation(ctx, "C123")
	case "conversations.list":
		rows, cursor, err := client.getConversations(ctx, client.tokens.User, &slack.GetConversationsParameters{})
		if err != nil {
			require.Nil(t, rows)
			require.Empty(t, cursor)
		}
		return err
	case "users.list":
		rows, err := client.getUsers(ctx, client.tokens.User)
		if err != nil {
			require.Nil(t, rows)
		}
		return err
	case "conversations.history":
		page, err := client.getConversationHistory(ctx, client.tokens.User, &slack.GetConversationHistoryParameters{ChannelID: "C123"}, nil)
		if err != nil {
			require.Nil(t, page)
		} else {
			require.NotNil(t, page)
		}
		return err
	case "conversations.replies":
		page, err := client.getConversationReplies(ctx, client.tokens.User, &slack.GetConversationRepliesParameters{ChannelID: "C123", Timestamp: "1710000000.000000"}, nil)
		if err != nil {
			require.Nil(t, page)
		} else {
			require.NotNil(t, page)
		}
		return err
	default:
		t.Fatalf("unexpected native method %q", method)
		return nil
	}
}
