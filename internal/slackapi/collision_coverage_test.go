package slackapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/openclaw/slacrawl/internal/admission"
	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/stretchr/testify/require"
)

func TestHistoryCollisionRetainsIntervalAndChannelIsolation(t *testing.T) {
	for _, sourceName := range []string{SourceBot, SourceUser} {
		for _, collisionPage := range []string{"first", "last"} {
			t.Run(sourceName+"/"+collisionPage, func(t *testing.T) {
				st, _ := historyAttemptStores(t)
				ctx := context.Background()
				seedHistoryAttemptCollision(t, st)
				foreign, err := st.QueryReadOnly(ctx, "select * from messages where workspace_id='TOTHER'")
				require.NoError(t, err)
				foreignProjection := func() map[string][]map[string]any {
					result := map[string][]map[string]any{}
					for _, table := range []string{"message_events", "message_event_heads", "message_fts", "message_mentions"} {
						predicate := "channel_id='C123' and ts='1710000002.000000'"
						if table == "message_fts" {
							predicate = "message_key='C123|1710000002.000000'"
						}
						rows, err := st.QueryReadOnly(ctx, "select * from "+table+" where "+predicate)
						require.NoError(t, err)
						result[table] = rows
					}
					return result
				}
				foreignDerived := foreignProjection()
				scope := store.APIHistoryScope{SourceName: sourceName, WorkspaceID: "T123", ChannelID: "C123"}
				require.NoError(t, seedAPIHistory(ctx, st, sourceName, "T123", "C123", "", store.APIHistoryState{Complete: true, Latest: "1709900000.000000", Pending: new("1709800000.000000")}))
				corrected := false
				histories := 0
				tokens := config.Tokens{User: "fixture-user"}
				if sourceName == SourceBot {
					tokens.Bot = "fixture-bot"
				}
				client := primaryOwnerClient(t, tokens, func(r *http.Request, form url.Values) (any, error) {
					switch r.URL.Path {
					case "/conversations.list":
						if form.Get("types") == "im,mpim" {
							return map[string]any{"ok": true, "channels": []any{}}, nil
						}
						return map[string]any{"ok": true, "channels": []any{
							map[string]any{"id": "C123", "name": "fixture", "is_channel": true},
							map[string]any{"id": "COK", "name": "other", "is_channel": true},
						}}, nil
					case "/conversations.history":
						histories++
						if form.Get("channel") == "COK" {
							return primaryOwnerResponse(r.URL.Path), nil
						}
						require.Equal(t, "1709800000.000000", form.Get("oldest"))
						if corrected {
							require.Empty(t, form.Get("cursor"))
							return primaryOwnerResponse(r.URL.Path), nil
						}
						first := form.Get("cursor") == ""
						rows := []any{map[string]any{"ts": "1710000003.000000", "text": "admitted first <@U123>"}}
						if !first {
							rows = []any{map[string]any{"ts": "1710000004.000000", "text": "admitted last"}}
						}
						if first == (collisionPage == "first") {
							rows = append(rows, map[string]any{"ts": "1710000002.000000", "text": "collision replacement"})
						}
						response := map[string]any{"ok": true, "messages": rows}
						if first {
							response["response_metadata"] = map[string]any{"next_cursor": "next"}
						}
						return response, nil
					default:
						return primaryOwnerResponse(r.URL.Path), nil
					}
				}).WithDMPolicy(admission.Include)
				client.now = func() time.Time { return time.Unix(1710000200, 0) }
				require.NoError(t, client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123"}))
				require.Equal(t, 3, histories)
				state, err := st.APIHistory(ctx, scope)
				require.NoError(t, err)
				require.True(t, state.Complete)
				require.Equal(t, "1709900000.000000", state.Latest)
				require.Equal(t, new("1709800000.000000"), state.Pending)
				require.NotEmpty(t, state.Generation)
				require.Equal(t, "1710000200.000000", state.PendingLatest)
				other, err := st.APIHistory(ctx, store.APIHistoryScope{SourceName: sourceName, WorkspaceID: "T123", ChannelID: "COK"})
				require.NoError(t, err)
				require.Equal(t, store.APIHistoryState{Complete: true, Latest: "1710000200.000000"}, other, "collision state is channel-local")
				rows, err := st.QueryReadOnly(ctx, "select ts from messages where workspace_id='T123' order by ts")
				require.NoError(t, err)
				require.Equal(t, []map[string]any{{"ts": "1710000003.000000"}, {"ts": "1710000004.000000"}}, rows)
				for _, table := range []string{"message_events", "message_event_heads"} {
					rows, err := st.QueryReadOnly(ctx, "select ts,source_name from "+table+" where channel_id='C123' and ts in ('1710000003.000000','1710000004.000000') order by ts")
					require.NoError(t, err)
					require.Equal(t, []map[string]any{{"ts": "1710000003.000000", "source_name": sourceName}, {"ts": "1710000004.000000", "source_name": sourceName}}, rows, table)
				}
				fts, err := st.QueryReadOnly(ctx, "select message_key from message_fts where message_key in ('C123|1710000003.000000','C123|1710000004.000000') order by message_key")
				require.NoError(t, err)
				require.Equal(t, []map[string]any{{"message_key": "C123|1710000003.000000"}, {"message_key": "C123|1710000004.000000"}}, fts)
				mentions, err := st.QueryReadOnly(ctx, "select ts,mention_type,target_id from message_mentions where channel_id='C123' and ts='1710000003.000000'")
				require.NoError(t, err)
				require.Equal(t, []map[string]any{{"ts": "1710000003.000000", "mention_type": "user", "target_id": "U123"}}, mentions)
				require.Equal(t, foreignDerived, foreignProjection())
				status, err := st.Status(ctx)
				require.NoError(t, err)
				require.Equal(t, "partial", status.ThreadState)
				corrected = true
				require.NoError(t, client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123"}))
				require.Equal(t, 5, histories)
				state, err = st.APIHistory(ctx, scope)
				require.NoError(t, err)
				require.Equal(t, store.APIHistoryState{Complete: true, Latest: "1710000200.000000"}, state)
				after, err := st.QueryReadOnly(ctx, "select * from messages where workspace_id='TOTHER'")
				require.NoError(t, err)
				require.Equal(t, foreign, after)
				require.Equal(t, foreignDerived, foreignProjection())
				status, err = st.Status(ctx)
				require.NoError(t, err)
				require.Equal(t, "full", status.ThreadState)
			})
		}
	}
}

func TestReplyCollisionRetainsWorkAndRetry(t *testing.T) {
	const root = "1710000000.000000"
	const skipKey = "T123|C123|" + root
	for _, mode := range []string{"ordinary", "since", "repair"} {
		for _, later := range []string{"empty", "transport", "admission", "store"} {
			t.Run(mode+"/"+later, func(t *testing.T) {
				st, _ := historyAttemptStores(t)
				ctx := context.Background()
				retainedOwnerSeed(t, st, root)
				seedHistoryAttemptCollision(t, st)
				require.NoError(t, st.SetSyncState(ctx, SourceUser, "thread_skip", skipKey, "old skip"))
				require.NoError(t, st.SetSyncState(ctx, "doctor", "threads", "coverage", "old status"))
				require.NoError(t, st.SetSyncState(ctx, SourceBot, "workspace", "T123", "prior-marker"))
				scope := store.APIHistoryScope{SourceName: SourceBot, WorkspaceID: "T123", ChannelID: "C123"}
				if mode == "since" {
					scope.Since = "1709900000.000000"
				}
				require.NoError(t, seedAPIHistory(ctx, st, SourceBot, "T123", "C123", scope.Since, store.APIHistoryState{Complete: true, Latest: "1709900000.000000"}))
				if later == "store" {
					_, err := st.DB().ExecContext(ctx, "create trigger reject_later_reply before insert on messages when new.ts='1710000004.000000' begin select raise(abort,'later reply store failure'); end")
					require.NoError(t, err)
				}
				transportErr := errors.New("later request fixture")
				phase, replies := 0, 0
				var active []store.ThreadWork
				var attempted store.APIHistoryState
				client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, func(r *http.Request, form url.Values) (any, error) {
					switch r.URL.Path {
					case "/conversations.list":
						if form.Get("types") == "im,mpim" {
							return map[string]any{"ok": true, "channels": []any{}}, nil
						}
						return primaryOwnerResponse(r.URL.Path), nil
					case "/conversations.history":
						if phase == 0 {
							var err error
							attempted, err = st.APIHistory(ctx, scope)
							require.NoError(t, err)
						}
						if phase == 2 || (phase == 1 && mode == "ordinary") {
							return primaryOwnerResponse(r.URL.Path), nil
						}
						return map[string]any{"ok": true, "messages": []any{map[string]any{"ts": root, "text": "root", "reply_count": 1}}}, nil
					case "/conversations.replies":
						replies++
						if phase > 0 {
							require.Empty(t, form.Get("cursor"))
							return map[string]any{"ok": true, "messages": []any{map[string]any{"ts": "1710000003.000000", "thread_ts": root, "text": "recovered reply"}}}, nil
						}
						if form.Get("cursor") == "" {
							return map[string]any{"ok": true, "messages": []any{
								map[string]any{"ts": "1710000002.000000", "thread_ts": root, "text": "collided reply"},
								map[string]any{"ts": "1710000003.000000", "thread_ts": root, "text": "admitted reply"},
							}, "response_metadata": map[string]any{"next_cursor": "next"}}, nil
						}
						var err error
						active, err = st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
						require.NoError(t, err)
						require.Len(t, active, 1, "the committed collision must retain or create the requested root job")
						switch later {
						case "transport":
							return nil, transportErr
						case "admission":
							return map[string]any{"ok": true, "messages": []any{map[string]any{"ts": "1710000004.000000", "channel": "CWRONG"}}}, nil
						case "store":
							return map[string]any{"ok": true, "messages": []any{map[string]any{"ts": "1710000004.000000", "thread_ts": root, "text": "must roll back"}}}, nil
						default:
							return map[string]any{"ok": true, "messages": []any{}}, nil
						}
					default:
						return primaryOwnerResponse(r.URL.Path), nil
					}
				}).WithDMPolicy(admission.Include)
				client.now = func() time.Time { return time.Unix(1710000200, 0) }
				run := func() error {
					if mode == "repair" && phase != 2 {
						return client.repairWorkspace(ctx, st, "T123")
					}
					since := scope.Since
					if phase == 2 {
						since = ""
					}
					return client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123", Since: since})
				}
				err := run()
				if later == "empty" {
					require.NoError(t, err)
				} else {
					require.Error(t, err, "a later concrete failure wins over the earlier collision")
					if later == "transport" {
						require.ErrorIs(t, err, transportErr)
					}
					if later == "store" {
						require.ErrorContains(t, err, "later reply store failure")
					}
				}
				require.Equal(t, 2, replies)
				status, err := st.Status(ctx)
				require.NoError(t, err)
				require.Equal(t, map[bool]string{true: "partial", false: "old status"}[later == "empty"], status.ThreadState)
				if later != "empty" {
					marker, err := st.GetSyncState(ctx, SourceBot, "workspace", "T123")
					require.NoError(t, err)
					require.Equal(t, "prior-marker", marker)
				}
				pending, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
				require.NoError(t, err)
				require.Equal(t, active, pending, "neither terminal success nor later failure retires the collision generation")
				skip, err := st.GetSyncState(ctx, SourceUser, "thread_skip", skipKey)
				require.NoError(t, err)
				require.Equal(t, "old skip", skip)
				state, err := st.APIHistory(ctx, scope)
				require.NoError(t, err)
				require.Equal(t, attempted, state, "collision preserves the exact pending generation and interval")
				require.True(t, state.Complete)
				require.Equal(t, "1709900000.000000", state.Latest)
				require.NotNil(t, state.Pending)
				require.NotEmpty(t, state.Generation)
				require.Equal(t, "1710000200.000000", state.PendingLatest)
				rows, err := st.QueryReadOnly(ctx, "select text from messages where ts='1710000003.000000'")
				require.NoError(t, err)
				require.Equal(t, []map[string]any{{"text": "admitted reply"}}, rows)
				phase = 1
				require.NoError(t, run())
				require.Equal(t, 3, replies)
				state, err = st.APIHistory(ctx, scope)
				require.NoError(t, err)
				require.Equal(t, store.APIHistoryState{Complete: true, Latest: "1710000200.000000"}, state)
				pending, err = st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
				require.NoError(t, err)
				if mode != "ordinary" {
					require.Equal(t, active, pending, "successful scoped replies do not drain ordinary backlog")
					phase = 2
					require.NoError(t, run())
					require.Equal(t, 4, replies)
				}
				pending, err = st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
				require.NoError(t, err)
				require.Empty(t, pending, "a hint-free ordinary retry drains retained work")
				skips, err := st.ListSyncState(ctx, SourceUser, "thread_skip", 10)
				require.NoError(t, err)
				require.Empty(t, skips)
				rows, err = st.QueryReadOnly(ctx, "select text from messages where workspace_id='TOTHER'")
				require.NoError(t, err)
				require.Equal(t, []map[string]any{{"text": "foreign-owner-fixture"}}, rows)
			})
		}
	}
}

func TestReplyCollisionSupersedesOrdinaryGeneration(t *testing.T) {
	for _, collisionFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary-first", true: "collision-first"}[collisionFirst], func(t *testing.T) {
			first, second := historyAttemptStores(t)
			ctx := context.Background()
			const root = "1710000000.000000"
			retainedOwnerSeed(t, first, root)
			seedHistoryAttemptCollision(t, first)
			a, err := first.BeginAPIHistory(ctx, store.APIHistoryScope{SourceName: SourceBot, WorkspaceID: "T123", ChannelID: "C123"}, store.APIHistoryOptions{}, "1710000200.000000")
			require.NoError(t, err)
			work, err := first.PrepareThreadWork(ctx, SourceUser, "T123", "C123", &a)
			require.NoError(t, err)
			require.Len(t, work, 1)
			skipKey := "T123|C123|" + root
			require.NoError(t, first.SetSyncState(ctx, SourceUser, "thread_skip", skipKey, "old skip"))
			b, err := second.BeginAPIHistory(ctx, store.APIHistoryScope{SourceName: SourceBot, WorkspaceID: "T123", ChannelID: "C123", Since: root}, store.APIHistoryOptions{}, "1710000300.000000")
			require.NoError(t, err)
			collisionCalls, ordinaryCalls := 0, 0
			collider := primaryOwnerClient(t, config.Tokens{User: "fixture-user"}, func(r *http.Request, _ url.Values) (any, error) {
				require.Equal(t, "/conversations.replies", r.URL.Path)
				collisionCalls++
				return map[string]any{"ok": true, "messages": []any{map[string]any{"ts": "1710000002.000000", "thread_ts": root, "text": "collided"}}}, nil
			})
			var next []store.ThreadWork
			collide := func() {
				outcome, err := collider.syncThread(ctx, second, "T123", "C123", root, true, time.Unix(1710000300, 0), threadSyncSource{token: collider.tokens.User, sourceName: SourceUser, sourceRank: 1}, nil, newThreadSkipTracker(), &b)
				require.NoError(t, err)
				require.Equal(t, threadSyncIncomplete, outcome)
				next, err = second.PendingThreadWork(ctx, SourceUser, "T123", "C123")
				require.NoError(t, err)
				require.Len(t, next, 1)
				require.NotEqual(t, work[0].Generation, next[0].Generation)
			}
			ordinary := primaryOwnerClient(t, config.Tokens{User: "fixture-user"}, func(r *http.Request, _ url.Values) (any, error) {
				require.Equal(t, "/conversations.replies", r.URL.Path)
				ordinaryCalls++
				if collisionFirst {
					// The second handle commits the scoped collision while G1's
					// request is dispatched but before its response is returned.
					collide()
				}
				return map[string]any{"ok": true, "messages": []any{map[string]any{"ts": "1710000003.000000", "thread_ts": root, "text": "ordinary response"}}}, nil
			})
			outcome, err := ordinary.syncThread(ctx, first, "T123", "C123", root, true, time.Unix(1710000200, 0), threadSyncSource{token: ordinary.tokens.User, sourceName: SourceUser, sourceRank: 1}, &work[0], newThreadSkipTracker(), &a)
			require.NoError(t, err)
			require.Equal(t, map[bool]threadSyncResult{true: threadSyncRevoked, false: threadSyncComplete}[collisionFirst], outcome)
			completed, err := first.CompleteThreadWork(ctx, work[0], skipKey, &a)
			require.NoError(t, err)
			require.Equal(t, !collisionFirst, completed)
			if !collisionFirst {
				collide()
			}
			current, err := first.PendingThreadWork(ctx, SourceUser, "T123", "C123")
			require.NoError(t, err)
			require.Equal(t, next, current)
			rows, err := first.QueryReadOnly(ctx, "select * from messages where ts='1710000003.000000'")
			require.NoError(t, err)
			require.Len(t, rows, map[bool]int{true: 0, false: 1}[collisionFirst])
			skips, err := first.ListSyncState(ctx, SourceUser, "thread_skip", 10)
			require.NoError(t, err)
			require.Len(t, skips, map[bool]int{true: 1, false: 0}[collisionFirst])
			require.Equal(t, 1, ordinaryCalls)
			require.Equal(t, 1, collisionCalls)
		})
	}
}

func TestRetainedHistoryCoverageScope(t *testing.T) {
	for _, operation := range []string{"sync", "repair"} {
		for _, mode := range []string{"local-pending", "foreign-pending", "foreign-invalid", "foreign-complete", "unrelated", "alias"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				st, _ := historyAttemptStores(t)
				ctx := context.Background()
				source, kind, key := SourceUser, store.APIHistoryEntityType, "[\"TOTHER\",\"COLD\",\"\"]"
				raw := "{\"complete\":false,\"latest\":\"\",\"pending\":\"\"}"
				switch mode {
				case "local-pending":
					key = "[\"T123\",\"COLD\",\"\"]"
				case "foreign-invalid":
					raw = "private-history-canary"
				case "foreign-complete":
					raw = "{\"complete\":true,\"latest\":\"1710000000.000000\"}"
				case "unrelated":
					source, key, raw = "mcp", "opaque", "private-history-canary"
				case "alias":
					key, raw = "[ \"TOTHER\",\"COLD\",\"\"]", "private-history-canary"
				}
				require.NoError(t, st.SetSyncState(ctx, source, kind, key, raw))
				require.NoError(t, st.SetSyncState(ctx, "doctor", "threads", "coverage", "full"))
				_, err := st.DB().ExecContext(ctx, "update sync_state set updated_at='2000-01-01T00:00:00Z' where source_name='doctor' and entity_type='threads'")
				require.NoError(t, err)
				const markerQuery = "select value,updated_at from sync_state where source_name='doctor' and entity_type='threads' and entity_id='coverage'"
				markerBefore, err := st.QueryReadOnly(ctx, markerQuery)
				require.NoError(t, err)
				client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, func(r *http.Request, form url.Values) (any, error) {
					if r.URL.Path == "/conversations.list" && form.Get("types") == "im,mpim" {
						return map[string]any{"ok": true, "channels": []any{}}, nil
					}
					return primaryOwnerResponse(r.URL.Path), nil
				}).WithDMPolicy(admission.Include)
				if operation == "repair" {
					err = client.repairWorkspace(ctx, st, "T123")
				} else {
					err = client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123"})
				}
				if mode == "alias" || (operation == "sync" && mode == "foreign-invalid") {
					require.Error(t, err)
					require.NotContains(t, err.Error(), "private-history-canary")
					require.NotContains(t, err.Error(), "TOTHER")
				} else {
					require.NoError(t, err)
				}
				markerAfter, markerErr := st.QueryReadOnly(ctx, markerQuery)
				require.NoError(t, markerErr)
				writesPartial := operation == "repair" && mode == "local-pending"
				writesFull := operation == "sync" && (mode == "foreign-complete" || mode == "unrelated")
				if writesPartial || writesFull {
					require.Len(t, markerAfter, 1)
					require.Equal(t, map[bool]string{true: "partial", false: "full"}[writesPartial], markerAfter[0]["value"])
					require.NotEqual(t, markerBefore[0]["updated_at"], markerAfter[0]["updated_at"])
				} else {
					require.Equal(t, markerBefore, markerAfter, "repair scope and blocked publication preserve the exact raw marker")
				}
				// Status is archive-wide even when repair correctly ignores a
				// canonical foreign value. It does not rewrite repair's result.
				status, statusErr := st.Status(ctx)
				if mode == "alias" || mode == "foreign-invalid" {
					require.ErrorContains(t, statusErr, "invalid API history checkpoint")
					require.NotContains(t, statusErr.Error(), "private-history-canary")
					require.Equal(t, store.Status{}, status)
				} else {
					require.NoError(t, statusErr)
					want := "full"
					if mode == "local-pending" || mode == "foreign-pending" {
						want = "partial"
					}
					require.Equal(t, want, status.ThreadState)
				}
				afterStatus, err := st.QueryReadOnly(ctx, markerQuery)
				require.NoError(t, err)
				require.Equal(t, markerAfter, afterStatus)
				after, err := st.GetSyncState(ctx, source, kind, key)
				require.NoError(t, err)
				require.Equal(t, raw, after)
			})
		}
	}
}
