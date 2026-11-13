package slackapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/admission"
	"github.com/openclaw/slacrawl/internal/config"
)

func TestSyncBotChannelThreads(t *testing.T) {
	for _, failure := range []string{"", "missing_scope", "not_in_channel"} {
		name := "complete"
		if failure != "" {
			name = "second-page-" + failure
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var replies, histories atomic.Int32
			var fail atomic.Bool
			fail.Store(failure != "")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				require.Equal(t, "xoxb-fixture", r.FormValue("token"))
				switch r.URL.Path {
				case "/auth.test":
					fmt.Fprint(w, `{"ok":true,"team_id":"T123","team":"Fixture"}`)
				case "/conversations.list":
					fmt.Fprint(w, `{"ok":true,"channels":[{"id":"C123","name":"fixture","is_channel":true,"is_member":true}],"response_metadata":{"next_cursor":""}}`)
				case "/users.list":
					fmt.Fprint(w, `{"ok":true,"members":[]}`)
				case "/conversations.history":
					histories.Add(1)
					if histories.Load() == 1 {
						fmt.Fprint(w, `{"ok":true,"messages":[{"ts":"1710000000.000100","text":"root","reply_count":2}]}`)
					} else {
						// Retained work must finish even when the new history page has no hint.
						fmt.Fprint(w, `{"ok":true,"messages":[]}`)
					}
				case "/conversations.replies":
					replies.Add(1)
					require.Equal(t, "C123", r.FormValue("channel"))
					require.Equal(t, "1710000000.000100", r.FormValue("ts"))
					if r.FormValue("cursor") == "" {
						fmt.Fprint(w, `{"ok":true,"messages":[{"ts":"1710000000.000100","text":"root","reply_count":2},{"ts":"1710000001.000100","thread_ts":"1710000000.000100","text":"reply one"}],"has_more":true,"response_metadata":{"next_cursor":"next"}}`)
					} else {
						require.Equal(t, "next", r.FormValue("cursor"))
						if fail.Load() {
							fmt.Fprintf(w, `{"ok":false,"error":%q}`, failure)
						} else {
							fmt.Fprint(w, `{"ok":true,"messages":[{"ts":"1710000002.000100","thread_ts":"1710000000.000100","text":"reply two"}],"has_more":false}`)
						}
					}
				default:
					t.Errorf("unexpected method %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			st := mustStore(t)
			defer func() { require.NoError(t, st.Close()) }()
			client := NewWithOptions(config.Tokens{Bot: "xoxb-fixture"}, server.URL+"/", server.Client())
			client.now = func() time.Time { return time.Unix(1710000100, 0) }
			noJoin := false
			require.NoError(t, client.Sync(ctx, st, SyncOptions{AutoJoin: &noJoin}))
			require.Equal(t, int32(2), replies.Load(), "bot replies must traverse the cursor")
			pending, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
			require.NoError(t, err)
			if failure != "" {
				require.Len(t, pending, 1)
				skip, err := st.GetSyncState(ctx, SourceUser, "thread_skip", "T123|C123|1710000000.000100")
				require.NoError(t, err)
				require.Equal(t, failure, skip)
				rows, err := st.Messages(ctx, "T123", "C123", "", 10)
				require.NoError(t, err)
				require.Len(t, rows, 2, "the first valid reply page remains archived")
				status, err := st.Status(ctx)
				require.NoError(t, err)
				require.Equal(t, "partial", status.ThreadState)
				fail.Store(false)
				require.NoError(t, client.Sync(ctx, st, SyncOptions{AutoJoin: &noJoin}))
				require.Equal(t, int32(4), replies.Load())
			}
			pending, err = st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
			require.NoError(t, err)
			require.Empty(t, pending)
			rows, err := st.QueryReadOnly(ctx, "SELECT source_name,source_rank FROM messages ORDER BY ts")
			require.NoError(t, err)
			require.Len(t, rows, 3)
			for _, row := range rows {
				require.Equal(t, SourceBot, row["source_name"])
				require.Equal(t, int64(2), row["source_rank"])
			}
			status, err := st.Status(ctx)
			require.NoError(t, err)
			require.Equal(t, "partial", status.ThreadState, "one bot-accessible channel is not full workspace coverage")
		})
	}
}

func TestSyncRejectedUserDoesNotFallBackToBotReplies(t *testing.T) {
	for _, authRejected := range []bool{true, false} {
		t.Run(fmt.Sprint("auth-rejected-", authRejected), func(t *testing.T) {
			var replies atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/auth.test":
					if r.FormValue("token") == "xoxp-fixture" && authRejected {
						fmt.Fprint(w, `{"ok":false,"error":"invalid_auth"}`)
					} else {
						fmt.Fprint(w, `{"ok":true,"team_id":"T123","team":"Fixture"}`)
					}
				case "/conversations.list":
					fmt.Fprint(w, `{"ok":true,"channels":[{"id":"C123","name":"fixture","is_channel":true,"is_member":true}]}`)
				case "/conversations.history":
					fmt.Fprint(w, `{"ok":true,"messages":[{"ts":"1710000000.000100","text":"root","reply_count":1}]}`)
				case "/conversations.replies":
					replies.Add(1)
					require.Equal(t, "xoxp-fixture", r.FormValue("token"))
					fmt.Fprint(w, `{"ok":false,"error":"missing_scope"}`)
				case "/users.list":
					fmt.Fprint(w, `{"ok":true,"members":[]}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			st := mustStore(t)
			defer func() { require.NoError(t, st.Close()) }()
			client := NewWithOptions(config.Tokens{Bot: "xoxb-fixture", User: "xoxp-fixture"}, server.URL+"/", server.Client()).WithDMPolicy(admission.Exclude)
			require.NoError(t, client.Sync(context.Background(), st, SyncOptions{}))
			want := int32(1)
			if authRejected {
				want = 0
			}
			require.Equal(t, want, replies.Load())
			pending, err := st.PendingThreadWork(context.Background(), SourceUser, "T123", "C123")
			require.NoError(t, err)
			require.Len(t, pending, 1)
			status, err := st.Status(context.Background())
			require.NoError(t, err)
			require.Equal(t, "partial", status.ThreadState)
		})
	}
}
