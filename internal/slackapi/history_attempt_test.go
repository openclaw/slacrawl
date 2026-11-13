package slackapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/slacrawl/internal/admission"
	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/require"
)

func TestHistoryAttemptRejectsSupersededPages(t *testing.T) {
	for _, sourceName := range []string{SourceBot, SourceUser} {
		for _, completed := range []bool{false, true} {
			for _, response := range []string{"message", "empty", "error", "decode", "collision"} {
				t.Run(sourceName+"/"+map[bool]string{false: "pending", true: "complete"}[completed]+"/"+response, func(t *testing.T) {
					first, second := historyAttemptStores(t)
					if response == "collision" {
						seedHistoryAttemptCollision(t, first)
					}
					ctx := context.Background()
					scope := store.APIHistoryScope{SourceName: sourceName, WorkspaceID: "T123", ChannelID: "C123"}
					require.NoError(t, seedAPIHistory(ctx, first, sourceName, "T123", "C123", "", store.APIHistoryState{Complete: true, Latest: "1709900000.000000", Pending: new("1709800000.000000")}))
					var before map[string][]string
					var newer store.APIHistoryAttempt
					calls := 0
					corrected := false
					client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, func(r *http.Request, form url.Values) (any, error) {
						require.Equal(t, "/conversations.history", r.URL.Path)
						calls++
						if corrected {
							require.Empty(t, form.Get("cursor"), "retry starts the inherited interval again")
							require.Equal(t, "1710000400.000000", form.Get("latest"))
							return map[string]any{"ok": true, "messages": []any{}}, nil
						}
						require.Equal(t, "1709800000.000000", form.Get("oldest"))
						require.Equal(t, "1710000200.000000", form.Get("latest"))
						if calls == 1 {
							return json.RawMessage(`{"ok":true,"messages":[{"ts":"1709900001.000000","text":"prior committed page"}],"response_metadata":{"next_cursor":"second"}}`), nil
						}
						require.Equal(t, "second", form.Get("cursor"))
						var err error
						newer, err = second.BeginAPIHistory(ctx, scope, store.APIHistoryOptions{}, "1710000400.000000")
						require.NoError(t, err)
						if completed {
							require.NoError(t, second.CompleteAPIHistory(ctx, newer))
						}
						before = historyAttemptSnapshot(t, first)
						return historyAttemptResponse(response), nil
					})
					var logs bytes.Buffer
					client.WithLogger(testProgressLogger(&logs))
					source := channelSyncSource{token: "fixture-bot", sourceName: sourceName, sourceRank: 2, threadSkip: newThreadSkipTracker()}
					if sourceName == SourceUser {
						source.token = "fixture-user"
						source.sourceRank = 1
					}
					err := client.syncChannelMessagesWithSource(ctx, first, "T123", historyAttemptChannel(), store.APIHistoryOptions{}, time.Unix(1710000200, 0), threadSyncSource{}, source)
					require.ErrorIs(t, err, store.ErrAPIHistorySuperseded)
					require.Equal(t, 2, calls)
					require.Equal(t, before, historyAttemptSnapshot(t, first))
					require.False(t, source.threadSkip.Omitted(), "a stale collision/error is not this attempt's outcome")
					require.NotContains(t, logs.String()+err.Error(), "TOTHER")
					require.NotContains(t, logs.String()+err.Error(), "foreign-owner-fixture")
					assertAdmissionCanariesAbsent(t, first, logs.String()+err.Error(), "stale-page-canary")
					corrected = true
					require.NoError(t, client.syncChannelMessagesWithSource(ctx, first, "T123", historyAttemptChannel(), store.APIHistoryOptions{}, time.Unix(1710000100, 0), threadSyncSource{}, source))
					require.Equal(t, 3, calls)
					state, err := first.APIHistory(ctx, scope)
					require.NoError(t, err)
					require.Equal(t, store.APIHistoryState{Complete: true, Latest: "1710000400.000000"}, state)
					rows, err := first.QueryReadOnly(ctx, "select text from messages where workspace_id='T123'")
					require.NoError(t, err)
					require.Equal(t, []map[string]any{{"text": "prior committed page"}}, rows)
				})
			}
		}
	}
}

func TestHistoryAttemptRejectsSupersededReplies(t *testing.T) {
	for _, retained := range []bool{false, true} {
		for _, completed := range []bool{false, true} {
			for _, response := range []string{"message", "empty", "error", "collision"} {
				t.Run(map[bool]string{false: "sliced", true: "retained"}[retained]+"/"+map[bool]string{false: "pending", true: "complete"}[completed]+"/"+response, func(t *testing.T) {
					first, second := historyAttemptStores(t)
					if response == "collision" {
						seedHistoryAttemptCollision(t, first)
					}
					ctx := context.Background()
					scope := store.APIHistoryScope{SourceName: SourceBot, WorkspaceID: "T123", ChannelID: "C123"}
					if !retained {
						scope.Since = "1709900000.000000"
					}
					skipKey := "T123|C123|1710000000.000000"
					require.NoError(t, first.SetSyncState(ctx, SourceUser, "thread_skip", skipKey, "prior-skip"))
					var before map[string][]string
					histories, replies := 0, 0
					client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, func(r *http.Request, _ url.Values) (any, error) {
						if r.URL.Path == "/conversations.history" {
							histories++
							return json.RawMessage(`{"ok":true,"messages":[{"ts":"1710000000.000000","text":"parent","reply_count":1}]}`), nil
						}
						require.Equal(t, "/conversations.replies", r.URL.Path)
						replies++
						if replies == 1 {
							return json.RawMessage(`{"ok":true,"messages":[{"ts":"1710000001.000000","thread_ts":"1710000000.000000","text":"prior reply"}],"response_metadata":{"next_cursor":"second"}}`), nil
						}
						newer, err := second.BeginAPIHistory(ctx, scope, store.APIHistoryOptions{}, "1710000400.000000")
						require.NoError(t, err)
						if completed {
							require.NoError(t, second.CompleteAPIHistory(ctx, newer))
						}
						before = historyAttemptSnapshot(t, first)
						return historyAttemptResponse(response), nil
					})
					source := channelSyncSource{token: "fixture-bot", sourceName: SourceBot, sourceRank: 2, coverageScope: scope.Since, retainedThreads: retained, threadSkip: newThreadSkipTracker()}
					err := client.syncChannelMessagesWithSource(ctx, first, "T123", historyAttemptChannel(), store.APIHistoryOptions{RestoreRequested: !retained}, time.Unix(1710000200, 0), threadSyncSource{token: client.tokens.User, sourceName: SourceUser, sourceRank: 1}, source)
					require.ErrorIs(t, err, store.ErrAPIHistorySuperseded)
					require.Equal(t, 1, histories)
					require.Equal(t, 2, replies)
					require.Equal(t, before, historyAttemptSnapshot(t, first), "preserve current generation, skip and previously committed reply")
					require.False(t, source.threadSkip.Skipped())
					require.NotContains(t, err.Error(), "TOTHER")
					require.NotContains(t, err.Error(), "foreign-owner-fixture")
					assertAdmissionCanariesAbsent(t, first, err.Error(), "stale-page-canary")
					work, err := first.PendingThreadWork(ctx, SourceUser, "T123", "C123")
					require.NoError(t, err)
					if retained {
						require.Len(t, work, 1)
						require.NotEmpty(t, work[0].Generation)
					} else {
						require.Empty(t, work)
					}
				})
			}
		}
	}
}

func TestHistoryAttemptGuardsJoinResults(t *testing.T) {
	for _, response := range []string{"success", "failure"} {
		t.Run(response, func(t *testing.T) {
			first, second := historyAttemptStores(t)
			ctx := context.Background()
			calls := []string{}
			var before map[string][]string
			client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot"}, func(r *http.Request, _ url.Values) (any, error) {
				calls = append(calls, r.URL.Path)
				if r.URL.Path == "/conversations.history" {
					return map[string]any{"ok": false, "error": "not_in_channel"}, nil
				}
				require.Equal(t, "/conversations.join", r.URL.Path)
				_, err := second.BeginAPIHistory(ctx, store.APIHistoryScope{SourceName: SourceBot, WorkspaceID: "T123", ChannelID: "C123"}, store.APIHistoryOptions{}, "1710000400.000000")
				require.NoError(t, err)
				before = historyAttemptSnapshot(t, first)
				if response == "failure" {
					return map[string]any{"ok": false, "error": "missing_scope"}, nil
				}
				return map[string]any{"ok": true, "channel": map[string]any{"id": "C123"}}, nil
			})
			source := channelSyncSource{token: "fixture-bot", sourceName: SourceBot, sourceRank: 2, allowJoin: true, threadSkip: newThreadSkipTracker()}
			err := client.syncChannelMessagesWithSource(ctx, first, "T123", historyAttemptChannel(), store.APIHistoryOptions{}, time.Unix(1710000200, 0), threadSyncSource{}, source)
			require.ErrorIs(t, err, store.ErrAPIHistorySuperseded)
			require.Equal(t, []string{"/conversations.history", "/conversations.join"}, calls, "already dispatched join is not undone, but stale state and history retry are blocked")
			require.Equal(t, before, historyAttemptSnapshot(t, first))
			require.False(t, source.threadSkip.Omitted())
		})
	}
}

func TestHistoryAttemptChecksEachPhysicalRetry(t *testing.T) {
	for _, method := range []string{"history", "replies"} {
		t.Run(method, func(t *testing.T) {
			first, second := historyAttemptStores(t)
			ctx := context.Background()
			scope := store.APIHistoryScope{SourceName: SourceBot, WorkspaceID: "T123", ChannelID: "C123"}
			attempt, err := first.BeginAPIHistory(ctx, scope, store.APIHistoryOptions{}, "1710000200.000000")
			require.NoError(t, err)
			requests, sleeps := 0, 0
			client := NewWithOptions(config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, "https://fixture.invalid/", &http.Client{Transport: primaryOwnerRoundTrip(func(r *http.Request) (*http.Response, error) {
				requests++
				require.Equal(t, "/conversations."+method, r.URL.Path)
				return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"1"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})})
			var before map[string][]string
			client.sleep = func(context.Context, time.Duration) error {
				sleeps++
				_, err := second.BeginAPIHistory(ctx, scope, store.APIHistoryOptions{}, "1710000300.000000")
				require.NoError(t, err)
				before = historyAttemptSnapshot(t, first)
				return nil
			}
			check := func() error { return first.CheckAPIHistory(ctx, attempt) }
			if method == "history" {
				page, e := client.getConversationHistory(ctx, "fixture-bot", &slack.GetConversationHistoryParameters{ChannelID: "C123"}, check)
				err = e
				require.Nil(t, page)
			} else {
				page, e := client.getConversationReplies(ctx, client.tokens.User, &slack.GetConversationRepliesParameters{ChannelID: "C123", Timestamp: "1710000000.000000"}, check)
				err = e
				require.Nil(t, page)
			}
			require.ErrorIs(t, err, store.ErrAPIHistorySuperseded)
			require.Equal(t, 1, requests)
			require.Equal(t, 1, sleeps)
			require.Equal(t, before, historyAttemptSnapshot(t, first))
		})
	}
}

func TestHistoryAttemptCancellationPrecedesStaleResponse(t *testing.T) {
	for _, method := range []string{"history", "replies"} {
		t.Run(method, func(t *testing.T) {
			first, second := historyAttemptStores(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			scope := store.APIHistoryScope{SourceName: SourceBot, WorkspaceID: "T123", ChannelID: "C123"}
			attempt, err := first.BeginAPIHistory(ctx, scope, store.APIHistoryOptions{}, "1710000200.000000")
			require.NoError(t, err)
			var before map[string][]string
			calls := 0
			client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, func(r *http.Request, _ url.Values) (any, error) {
				calls++
				require.Equal(t, "/conversations."+method, r.URL.Path)
				_, err := second.BeginAPIHistory(context.Background(), scope, store.APIHistoryOptions{}, "1710000300.000000")
				require.NoError(t, err)
				before = historyAttemptSnapshot(t, first)
				cancel()
				return map[string]any{"ok": false, "error": "missing_scope"}, nil
			})
			check := func() error { return first.CheckAPIHistory(ctx, attempt) }
			if method == "history" {
				page, e := client.getConversationHistory(ctx, "fixture-bot", &slack.GetConversationHistoryParameters{ChannelID: "C123"}, check)
				err = e
				require.Nil(t, page)
			} else {
				page, e := client.getConversationReplies(ctx, client.tokens.User, &slack.GetConversationRepliesParameters{ChannelID: "C123", Timestamp: "1710000000.000000"}, check)
				err = e
				require.Nil(t, page)
			}
			require.ErrorIs(t, err, context.Canceled)
			require.NotErrorIs(t, err, store.ErrAPIHistorySuperseded)
			require.Equal(t, 1, calls)
			require.Equal(t, before, historyAttemptSnapshot(t, first))
		})
	}
}

func TestHistoryAttemptFailureStopsSyncCompletion(t *testing.T) {
	for _, mode := range []string{"bot", "user", "repair"} {
		for _, workers := range []int{1, 2} {
			if mode == "repair" && workers == 2 {
				continue
			} // repair owns its existing serial options
			t.Run(mode+"/"+map[int]string{1: "serial", 2: "parallel"}[workers], func(t *testing.T) {
				first, second := historyAttemptStores(t)
				ctx := context.Background()
				source := SourceBot
				tokens := config.Tokens{Bot: "fixture-bot", User: "fixture-user"}
				if mode == "user" {
					source = SourceUser
					tokens.Bot = ""
				}
				scope := store.APIHistoryScope{SourceName: source, WorkspaceID: "T123", ChannelID: "C123"}
				require.NoError(t, first.SetSyncState(ctx, source, "workspace", "T123", "prior completion"))
				require.NoError(t, first.SetSyncState(ctx, "doctor", "threads", "coverage", "partial"))
				require.NoError(t, first.SetSyncState(ctx, SourceUser, "thread_skip", "T123|old|1", "legacy skip"))
				var mu sync.Mutex
				histories := 0
				var expected store.APIHistoryState
				client := primaryOwnerClient(t, tokens, func(r *http.Request, form url.Values) (any, error) {
					if r.URL.Path == "/conversations.list" {
						require.NotEqual(t, "im,mpim", form.Get("types"), "failed channel work stops later catalogs")
						return map[string]any{"ok": true, "channels": []any{map[string]any{"id": "C123", "is_channel": true}, map[string]any{"id": "C456", "is_channel": true}}}, nil
					}
					if r.URL.Path == "/conversations.history" {
						mu.Lock()
						defer mu.Unlock()
						histories++
						if form.Get("channel") == "C123" {
							_, err := second.BeginAPIHistory(ctx, scope, store.APIHistoryOptions{}, "1710000400.000000")
							require.NoError(t, err)
							expected, err = second.APIHistory(ctx, scope)
							require.NoError(t, err)
						}
						return map[string]any{"ok": true, "messages": []any{}}, nil
					}
					require.Equal(t, "/auth.test", r.URL.Path, "no users catalog after failed history")
					return primaryOwnerResponse(r.URL.Path), nil
				}).WithDMPolicy(admission.Include)
				client.now = func() time.Time { return time.Unix(1710000200, 0) }
				var logs bytes.Buffer
				client.WithLogger(testProgressLogger(&logs))
				var err error
				if mode == "repair" {
					err = client.repairWorkspace(ctx, first, "T123")
				} else {
					err = client.Sync(ctx, first, SyncOptions{WorkspaceID: "T123", Full: true, Concurrency: workers})
				}
				require.ErrorIs(t, err, store.ErrAPIHistorySuperseded)
				require.Contains(t, logs.String(), "state=failed")
				require.NotContains(t, logs.String(), "state=finished")
				require.GreaterOrEqual(t, histories, 1)
				state, err := first.APIHistory(ctx, scope)
				require.NoError(t, err)
				require.Equal(t, expected, state)
				for _, row := range []struct{ source, kind, key, want string }{
					{source, "workspace", "T123", "prior completion"},
					{"doctor", "threads", "coverage", "partial"},
					{SourceUser, "thread_skip", "T123|old|1", "legacy skip"},
				} {
					value, err := first.GetSyncState(ctx, row.source, row.kind, row.key)
					require.NoError(t, err)
					require.Equal(t, row.want, value)
				}
			})
		}
	}
}

func TestHistoryAttemptNilGuardDoctorProbe(t *testing.T) {
	calls := []string{}
	client := primaryOwnerClient(t, config.Tokens{User: "fixture-user"}, func(r *http.Request, form url.Values) (any, error) {
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case "/conversations.list":
			return json.RawMessage(`{"ok":true,"channels":[{"id":"D123","is_im":true}]}`), nil
		case "/conversations.history":
			require.Equal(t, "1", form.Get("limit"))
			require.Equal(t, "D123", form.Get("channel"))
			return map[string]any{"ok": true, "messages": []any{}}, nil
		default:
			return primaryOwnerResponse(r.URL.Path), nil
		}
	}).WithDMPolicy(admission.Include)
	diag, err := client.Doctor(context.Background())
	require.NoError(t, err)
	require.Empty(t, diag.DMProbeError)
	require.True(t, diag.UserAuthAvailable)
	require.Equal(t, []string{"/auth.test", "/conversations.list", "/conversations.history"}, calls)
}

func historyAttemptResponse(kind string) json.RawMessage {
	switch kind {
	case "message":
		return json.RawMessage(`{"ok":true,"messages":[{"ts":"1710000002.000000","text":"stale-page-canary","reply_count":1}],"response_metadata":{"next_cursor":"stale-page-canary"}}`)
	case "empty":
		return json.RawMessage(`{"ok":true,"messages":[]}`)
	case "error":
		return json.RawMessage(`{"ok":false,"error":"missing_scope"}`)
	case "decode":
		return json.RawMessage(`{"ok":true,"messages":[{"ts":42,"text":"stale-page-canary"}]}`)
	case "collision":
		return json.RawMessage(`{"ok":true,"messages":[{"channel":"C123","ts":"1710000002.000000","text":"stale-page-canary"}]}`)
	default:
		panic("unknown fixture response")
	}
}

func historyAttemptChannel() slack.Channel {
	return slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C123"}}, IsChannel: true}
}

func historyAttemptStores(t *testing.T) (*store.Store, *store.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.db")
	first, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	require.NoError(t, first.UpsertChannel(context.Background(), store.Channel{ID: "C123", WorkspaceID: "T123", Name: "fixture", UpdatedAt: time.Unix(1710000000, 0)}))
	return first, second
}

func historyAttemptSnapshot(t *testing.T, st *store.Store) map[string][]string {
	t.Helper()
	snapshot := map[string][]string{}
	for _, table := range admissionTables {
		rows, err := st.QueryReadOnly(context.Background(), "select * from "+table)
		require.NoError(t, err)
		for _, row := range rows {
			raw, err := json.Marshal(row)
			require.NoError(t, err)
			snapshot[table] = append(snapshot[table], string(raw))
		}
		sort.Strings(snapshot[table])
	}
	return snapshot
}

// This intentionally retained foreign-workspace row matches the existing
// collision fixtures: the response key passes page identity but cannot upsert.
func seedHistoryAttemptCollision(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	message := store.Message{WorkspaceID: "TOTHER", ChannelID: "C123", TS: "1710000002.000000", Text: "foreign-owner-fixture", NormalizedText: "foreign-owner-fixture", SourceName: SourceBot, SourceRank: 2, RawJSON: "{}", UpdatedAt: time.Unix(1710000000, 0)}
	require.NoError(t, st.UpsertMessage(ctx, message, nil))
	before := historyAttemptSnapshot(t, st)
	message.WorkspaceID = "T123"
	err := st.UpsertMessage(ctx, message, nil)
	require.True(t, store.IsWorkspaceCollision(err, "message"), "calibrate a real message ownership collision")
	require.Equal(t, before, historyAttemptSnapshot(t, st))
}
