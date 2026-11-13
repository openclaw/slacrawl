package slackapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack/slackevents"
	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/admission"
	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/share"
	"github.com/openclaw/slacrawl/internal/store"
)

func TestRetainedThreadScope(t *testing.T) {
	for _, mode := range []string{"ordinary", "full", "since", "full-since", "repair", "excluded-name", "other-channel"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			st := mustStore(t)
			defer func() { require.NoError(t, st.Close()) }()
			retainedOwnerSeed(t, st, "1710000001.000000")
			_, err := st.PrepareThreadWork(ctx, SourceUser, "T123", "C123", nil)
			require.NoError(t, err)
			before, err := st.QueryReadOnly(ctx, "select * from sync_state where entity_type='thread_pending_v1'")
			require.NoError(t, err)
			replies, histories := 0, 0
			client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, func(r *http.Request, form url.Values) (any, error) {
				switch r.URL.Path {
				case "/conversations.history":
					histories++
				case "/conversations.replies":
					replies++
					require.Equal(t, "1710000001.000000", form.Get("ts"))
					return map[string]any{"ok": true, "messages": []any{}}, nil
				}
				return primaryOwnerResponse(r.URL.Path), nil
			})
			opts := SyncOptions{WorkspaceID: "T123"}
			switch mode {
			case "full", "full-since":
				opts.Full = true
			case "excluded-name":
				opts.ExcludeChannels = []string{"fixture"}
			case "other-channel":
				opts.Channels = []string{"COTHER"}
			}
			if mode == "since" || mode == "full-since" {
				opts.Since = "1710000000.000000"
			}
			if mode == "repair" {
				err = client.repairWorkspace(ctx, st, "T123")
			} else {
				err = client.Sync(ctx, st, opts)
			}
			require.NoError(t, err)
			after, err := st.QueryReadOnly(ctx, "select * from sync_state where entity_type='thread_pending_v1'")
			require.NoError(t, err)
			if mode == "ordinary" || mode == "full" {
				require.Equal(t, 1, replies)
				require.Empty(t, after)
			} else {
				require.Zero(t, replies)
				require.Equal(t, before, after)
			}
			require.Equal(t, map[bool]int{true: 0, false: 1}[mode == "excluded-name" || mode == "other-channel"], histories)
		})
	}
}

func TestRetainedThreadDrainAndNewGeneration(t *testing.T) {
	for _, renew := range []bool{false, true} {
		t.Run(fmt.Sprintf("renew=%t", renew), func(t *testing.T) {
			ctx := context.Background()
			st := mustStore(t)
			defer func() { require.NoError(t, st.Close()) }()
			for i := 0; i < 61; i++ {
				retainedOwnerSeed(t, st, fmt.Sprintf("17100000%02d.000000", i))
			}
			calls := map[string][]string{}
			client := primaryOwnerClient(t, config.Tokens{User: "fixture-user"}, func(r *http.Request, form url.Values) (any, error) {
				if r.URL.Path == "/conversations.replies" {
					ts, cursor := form.Get("ts"), form.Get("cursor")
					calls[ts] = append(calls[ts], cursor)
					payload := map[string]any{"ok": true, "messages": []any{}}
					if cursor == "" {
						payload["response_metadata"] = map[string]any{"next_cursor": "second"}
					} else {
						require.Equal(t, "second", cursor)
						if renew {
							_, err := st.ApplyWriteBatch(ctx, store.WriteBatch{PendingThreads: []store.ThreadWork{{SourceName: SourceUser, WorkspaceID: "T123", ChannelID: "C123", TS: ts}}})
							require.NoError(t, err)
						}
					}
					return payload, nil
				}
				return primaryOwnerResponse(r.URL.Path), nil
			})
			require.NoError(t, client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123"}))
			require.Len(t, calls, 61)
			for _, cursors := range calls {
				require.Equal(t, []string{"", "second"}, cursors)
			}
			pending, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
			require.NoError(t, err)
			require.Len(t, pending, map[bool]int{true: 61, false: 0}[renew])
			status, err := st.Status(ctx)
			require.NoError(t, err)
			require.Equal(t, map[bool]string{true: "partial", false: "full"}[renew], status.ThreadState)
		})
	}
}

func TestRetainedThreadFailuresKeepWork(t *testing.T) {
	for _, mode := range []string{"history-error", "history-incomplete", "replies-error", "replies-incomplete", "canceled", "skip"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			st := mustStore(t)
			defer func() { require.NoError(t, st.Close()) }()
			retainedOwnerSeed(t, st, "1710000001.000000")
			prior := store.APIHistoryState{Complete: true, Latest: "1710000200.000000"}
			require.NoError(t, seedAPIHistory(ctx, st, SourceBot, "T123", "C123", "", prior))
			client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, func(r *http.Request, _ url.Values) (any, error) {
				if r.URL.Path == "/conversations.history" {
					if mode == "history-error" {
						return map[string]any{"ok": false, "error": "synthetic_history_failure"}, nil
					}
					if mode == "history-incomplete" {
						return map[string]any{"ok": true, "messages": []any{}, "has_more": true}, nil
					}
				}
				if r.URL.Path == "/conversations.replies" {
					if mode == "canceled" {
						cancel()
						return nil, ctx.Err()
					}
					if mode == "replies-incomplete" {
						return map[string]any{"ok": true, "messages": []any{}, "has_more": true}, nil
					}
					code := "synthetic_replies_failure"
					if mode == "skip" {
						code = "missing_scope"
					}
					return map[string]any{"ok": false, "error": code}, nil
				}
				return primaryOwnerResponse(r.URL.Path), nil
			})
			err := client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123"})
			if mode == "skip" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			if mode == "canceled" {
				require.ErrorIs(t, err, context.Canceled)
			}
			pending, err := st.PendingThreadWork(context.Background(), SourceUser, "T123", "C123")
			require.NoError(t, err)
			require.Len(t, pending, 1)
			coverage, err := readAPIHistory(context.Background(), st, SourceBot, "T123", "C123", "")
			require.NoError(t, err)
			if mode != "skip" {
				require.Equal(t, prior.Latest, coverage.Latest)
				require.True(t, coverage.Complete)
				require.Equal(t, new("1709996600.000000"), coverage.Pending)
			}
		})
	}
}

func TestRetainedThreadPageSuccessKeepsCurrentWork(t *testing.T) {
	for _, failure := range []string{"unsuccessful", "absent", "null", "native-error"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			st := mustStore(t)
			defer func() { require.NoError(t, st.Close()) }()
			const rootTS = "1710000001.000000"
			retainedOwnerSeed(t, st, rootTS)
			initial, err := st.PrepareThreadWork(ctx, SourceUser, "T123", "C123", nil)
			require.NoError(t, err)
			require.Len(t, initial, 1)
			require.NoError(t, st.SetSyncState(ctx, SourceUser, "thread_skip", "T123|C123|"+rootTS, "not_in_channel"))
			require.NoError(t, seedAPIHistory(ctx, st, SourceBot, "T123", "C123", "", store.APIHistoryState{Complete: true, Latest: "1709900000.000000"}))
			require.NoError(t, st.SetSyncState(ctx, SourceBot, "workspace", "T123", "2020-01-01T00:00:00Z"))
			require.NoError(t, st.SetSyncState(ctx, "doctor", "threads", "coverage", "stored-status"))
			const skipQuery = "select * from sync_state where source_name='api-user' and entity_type='thread_skip'"
			const progressQuery = "select * from sync_state where entity_type='workspace' or source_name='doctor' order by source_name,entity_id"
			beforeSkip, beforeProgress, beforeParent := repairKeyRows(t, st, skipQuery), repairKeyRows(t, st, progressQuery), repairKeyParent(t, st)
			corrected := false
			var attempts []store.ThreadWork
			var cursors []string
			client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, func(r *http.Request, form url.Values) (any, error) {
				if r.URL.Path != "/conversations.replies" {
					return primaryOwnerResponse(r.URL.Path), nil
				}
				require.Equal(t, "fixture-user", form.Get("token"))
				require.Equal(t, "C123", form.Get("channel"))
				require.Equal(t, rootTS, form.Get("ts"))
				cursors = append(cursors, form.Get("cursor"))
				work, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
				require.NoError(t, err)
				require.Len(t, work, 1)
				current, err := st.ThreadWorkCurrent(ctx, work[0])
				require.NoError(t, err)
				require.True(t, current)
				if form.Get("cursor") == "" {
					attempts = append(attempts, work[0])
					reply := repairKeyMessage("earlier-reply", "1710000002.000000")
					reply["thread_ts"] = rootTS
					return map[string]any{"ok": true, "messages": []any{reply}, "response_metadata": map[string]any{"next_cursor": "second"}}, nil
				}
				require.Equal(t, "second", form.Get("cursor"))
				require.Equal(t, attempts[len(attempts)-1], work[0])
				if !corrected && failure == "native-error" {
					return map[string]any{"ok": false, "error": "rejected-reply-canary"}, nil
				}
				if !corrected && failure != "unsuccessful" {
					payload := map[string]any{"ok": true}
					if failure == "null" {
						payload["messages"] = nil
					}
					return payload, nil
				}
				text := "rejected-reply-canary"
				if corrected {
					text = "recovered-reply"
				}
				reply := repairKeyMessage(text, "1710000003.000000")
				reply["thread_ts"] = rootTS
				return map[string]any{"ok": corrected, "error": " \t ", "messages": []any{reply}}, nil
			})
			client.now = func() time.Time { return time.Unix(1710000200, 0).UTC() }
			runErr := client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123"})
			wantError := "conversations.replies response did not report success"
			if failure == "native-error" {
				wantError = "slack conversations.replies API response failed"
				requireNativeErrorCode(t, runErr, "rejected-reply-canary")
			} else if failure != "unsuccessful" {
				wantError = "conversations.replies response did not provide a collection array; page remains uncertified"
			}
			require.EqualError(t, runErr, wantError)
			require.Equal(t, []string{"", "second"}, cursors)
			require.Len(t, attempts, 1)
			require.NotEmpty(t, attempts[0].Generation)
			require.NotEqual(t, initial[0].Generation, attempts[0].Generation, "ordinary preparation renews the generation before HTTP")
			wantWork := initial[0]
			wantWork.Generation = attempts[0].Generation
			require.Equal(t, wantWork, attempts[0])
			pending, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
			require.NoError(t, err)
			require.Equal(t, attempts, pending)
			require.Equal(t, beforeSkip, repairKeyRows(t, st, skipQuery))
			require.Equal(t, beforeProgress, repairKeyRows(t, st, progressQuery))
			require.Equal(t, beforeParent, repairKeyParent(t, st))
			coverage, err := readAPIHistory(ctx, st, SourceBot, "T123", "C123", "")
			require.NoError(t, err)
			require.NotEmpty(t, coverage.Generation)
			require.Equal(t, store.APIHistoryState{Complete: true, Latest: "1709900000.000000", Pending: new("1709896400.000000"), Generation: coverage.Generation, PendingLatest: "1710000200.000000"}, coverage)
			require.Equal(t, []string{"1710000002.000000|FEARLIERREPLY|earlier-reply.txt|UEARLIERREPLY"}, repairKeyDerived(t, st))
			assertAdmissionCanariesAbsent(t, st, runErr.Error(), "rejected-reply-canary", "UREJECTEDREPLYCANARY", "FREJECTEDREPLYCANARY")
			corrected = true
			require.NoError(t, client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123"}))
			require.Equal(t, []string{"", "second", "", "second"}, cursors)
			require.Len(t, attempts, 2)
			require.NotEqual(t, attempts[0].Generation, attempts[1].Generation)
			pending, err = st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
			require.NoError(t, err)
			require.Empty(t, pending)
			require.Empty(t, repairKeyRows(t, st, skipQuery))
			require.Equal(t, beforeParent, repairKeyParent(t, st))
			coverage, err = readAPIHistory(ctx, st, SourceBot, "T123", "C123", "")
			require.NoError(t, err)
			require.Equal(t, store.APIHistoryState{Complete: true, Latest: "1710000200.000000"}, coverage)
			require.Equal(t, []map[string]any{{"ts": rootTS, "thread_ts": rootTS}, {"ts": "1710000002.000000", "thread_ts": rootTS}, {"ts": "1710000003.000000", "thread_ts": rootTS}}, repairKeyRows(t, st, "select ts,thread_ts from messages order by ts"))
			require.Equal(t, []string{"1710000002.000000|FEARLIERREPLY|earlier-reply.txt|UEARLIERREPLY", "1710000003.000000|FRECOVEREDREPLY|recovered-reply.txt|URECOVEREDREPLY"}, repairKeyDerived(t, st))
			assertAdmissionCanariesAbsent(t, st, "", "rejected-reply-canary", "UREJECTEDREPLYCANARY", "FREJECTEDREPLYCANARY")
		})
	}
}

func TestHistoryPageChildrenPersistThreadWork(t *testing.T) {
	for _, mode := range []string{"root-child-error", "child-root-incomplete", "root-page-limited", "child-page-error", "known", "synced", "since", "repair"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			st := mustStore(t)
			defer func() { require.NoError(t, st.Close()) }()
			const rootTS, childTS = "1710000001.000000", "1710000002.000000"
			now := time.Unix(1710000400, 0).UTC()
			require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C123", WorkspaceID: "T123", Name: "fixture", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now}))
			prior := store.APIHistoryState{Complete: true, Latest: "1710000200.000000"}
			require.NoError(t, seedAPIHistory(ctx, st, SourceBot, "T123", "C123", "", prior))
			require.NoError(t, st.SetSyncState(ctx, SourceBot, "workspace", "T123", "2020-01-01T00:00:00Z"))
			workspaceBefore, err := st.QueryReadOnly(ctx, "select * from sync_state where entity_type='workspace'")
			require.NoError(t, err)
			root := map[string]any{"ts": rootTS, "channel": "C123", "text": "root", "reply_count": 0}
			child := map[string]any{"ts": childTS, "channel": "C123", "thread_ts": rootTS, "text": "child <@U1>"}
			if mode == "known" {
				for _, msg := range []store.Message{
					{WorkspaceID: "T123", ChannelID: "C123", TS: rootTS, SourceName: SourceBot, SourceRank: 2, RawJSON: "{}", UpdatedAt: now},
					{WorkspaceID: "T123", ChannelID: "C123", TS: childTS, ThreadTS: rootTS, SourceName: SourceBot, SourceRank: 2, RawJSON: "{}", UpdatedAt: now},
				} {
					require.NoError(t, st.UpsertMessage(ctx, msg, nil))
				}
			}
			histories, replies := 0, 0
			var prepared []map[string]any
			client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, func(r *http.Request, form url.Values) (any, error) {
				switch r.URL.Path {
				case "/conversations.history":
					histories++
					require.Equal(t, "C123", form.Get("channel"))
					if histories == 1 && mode == "known" {
						prepared, err = st.QueryReadOnly(ctx, "select * from sync_state where entity_type='thread_pending_v1'")
						require.NoError(t, err)
						require.Len(t, prepared, 1)
					}
					messages := []any{root, child}
					next := "next"
					payload := map[string]any{"ok": true}
					switch mode {
					case "child-root-incomplete":
						messages, next = []any{child, root}, ""
						payload["has_more"] = true
					case "root-page-limited", "child-page-error":
						first, second := any(root), any(child)
						if mode == "child-page-error" {
							first, second = second, first
						}
						if histories == 1 {
							messages = []any{first}
						} else if histories == 2 {
							messages, next = []any{second}, "last"
							if mode == "root-page-limited" {
								next = ""
								payload["is_limited"] = true
							}
						} else {
							return map[string]any{"ok": false, "error": "synthetic_later_history_failure"}, nil
						}
					case "synced":
						if histories == 1 {
							messages = []any{map[string]any{"ts": rootTS, "channel": "C123", "text": "root", "reply_count": 1}}
						} else if histories == 2 {
							next = "last"
						} else {
							return map[string]any{"ok": false, "error": "synthetic_later_history_failure"}, nil
						}
					default:
						if histories > 1 {
							return map[string]any{"ok": false, "error": "synthetic_later_history_failure"}, nil
						}
					}
					payload["messages"] = messages
					if next != "" {
						payload["response_metadata"] = map[string]any{"next_cursor": next}
					}
					return payload, nil
				case "/conversations.replies":
					replies++
					require.Equal(t, "synced", mode)
					require.Equal(t, rootTS, form.Get("ts"))
					return map[string]any{"ok": true, "messages": []any{}}, nil
				}
				return primaryOwnerResponse(r.URL.Path), nil
			})
			client.now = func() time.Time { return now }
			opts := SyncOptions{WorkspaceID: "T123"}
			if mode == "since" {
				opts.Since = "1710000000.000000"
			}
			if mode == "repair" {
				err = client.repairWorkspace(ctx, st, "T123")
			} else {
				err = client.Sync(ctx, st, opts)
			}
			require.Error(t, err)
			switch mode {
			case "child-root-incomplete":
				require.ErrorContains(t, err, "has_more without a continuation cursor")
			case "root-page-limited":
				require.ErrorContains(t, err, "completeness of the requested interval is uncertified")
			default:
				require.ErrorContains(t, err, "slack conversations.history API response failed")
				requireNativeErrorCode(t, err, "synthetic_later_history_failure")
			}
			require.Equal(t, map[string]int{"root-child-error": 2, "child-root-incomplete": 1, "root-page-limited": 2, "child-page-error": 3, "known": 2, "synced": 3, "since": 2, "repair": 2}[mode], histories)
			require.Equal(t, map[bool]int{true: 1, false: 0}[mode == "synced"], replies)
			pending, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
			require.NoError(t, err)
			if mode == "synced" || mode == "since" || mode == "repair" {
				require.Empty(t, pending)
			} else {
				require.Len(t, pending, 1)
				require.Equal(t, rootTS, pending[0].TS)
				require.NotEmpty(t, pending[0].Generation)
			}
			if mode == "known" {
				after, err := st.QueryReadOnly(ctx, "select * from sync_state where entity_type='thread_pending_v1'")
				require.NoError(t, err)
				require.Equal(t, prepared, after, "page discovery must not renew the prepared generation")
			}
			rows, err := st.QueryReadOnly(ctx, "select ts,coalesce(thread_ts,'') as thread_ts,source_name,source_rank from messages order by ts")
			require.NoError(t, err)
			require.Equal(t, []map[string]any{
				{"ts": rootTS, "thread_ts": "", "source_name": SourceBot, "source_rank": int64(2)},
				{"ts": childTS, "thread_ts": rootTS, "source_name": SourceBot, "source_rank": int64(2)},
			}, rows)
			workspaceAfter, err := st.QueryReadOnly(ctx, "select * from sync_state where entity_type='workspace'")
			require.NoError(t, err)
			require.Equal(t, workspaceBefore, workspaceAfter)
			coverage, err := readAPIHistory(ctx, st, SourceBot, "T123", "C123", "")
			require.NoError(t, err)
			require.Equal(t, prior.Latest, coverage.Latest)
			require.True(t, coverage.Complete)
			if mode != "since" {
				require.NotNil(t, coverage.Pending)
			}
		})
	}
}

func TestRetainedThreadRevivalRequeuesCanceledWork(t *testing.T) {
	for _, mode := range []string{"hint", "duplicate-hint", "child", "history-error", "history-incomplete", "history-limited", "unavailable", "renewed", "completed", "revoked-later-page"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			st := mustStore(t)
			defer func() { require.NoError(t, st.Close()) }()
			const rootTS, childTS = "1710000001.000000", "1710000002.000000"
			now := time.Unix(1710000400, 0).UTC()
			retainedOwnerSeed(t, st, rootTS)
			prior := store.APIHistoryState{Complete: true, Latest: "1710000200.000000"}
			require.NoError(t, seedAPIHistory(ctx, st, SourceBot, "T123", "C123", "", prior))
			require.NoError(t, st.SetSyncState(ctx, SourceBot, "workspace", "T123", "2020-01-01T00:00:00Z"))
			workspaceBefore, err := st.QueryReadOnly(ctx, "select * from sync_state where entity_type='workspace'")
			require.NoError(t, err)
			deleteRoot := func() {
				require.NoError(t, st.MarkMessageDeleted(ctx, store.Message{WorkspaceID: "T123", ChannelID: "C123", TS: rootTS,
					DeletedTS: rootTS, SourceName: SourceBot, SourceRank: 2, RawJSON: `{"subtype":"message_deleted"}`, UpdatedAt: now}, nil))
				pending, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
				require.NoError(t, err)
				require.Empty(t, pending, "committed deletion cancels the prepared generation")
			}
			tokens := config.Tokens{Bot: "fixture-bot", User: "fixture-user"}
			histories, replies := 0, 0
			var prepared store.ThreadWork
			var newer []map[string]any
			const workQuery = "select * from sync_state where source_name='api-user' and entity_type in ('thread_pending_v1','thread_skip') order by entity_type,entity_id"
			client := primaryOwnerClient(t, tokens, func(r *http.Request, form url.Values) (any, error) {
				switch r.URL.Path {
				case "/auth.test":
					if mode == "unavailable" && form.Get("token") == tokens.User {
						return map[string]any{"ok": false, "error": "invalid_auth"}, nil
					}
				case "/conversations.history":
					histories++
					require.Equal(t, "C123", form.Get("channel"))
					if histories == 1 {
						work, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
						require.NoError(t, err)
						require.Len(t, work, 1)
						prepared = work[0]
						if mode != "completed" && mode != "revoked-later-page" {
							deleteRoot()
						}
						if mode == "renewed" {
							retainedOwnerSeed(t, st, rootTS)
							_, err := st.PrepareThreadWork(ctx, SourceUser, "T123", "C123", nil)
							require.NoError(t, err)
							require.NoError(t, st.SetSyncState(ctx, SourceUser, "thread_skip", "T123|C123|"+rootTS, "new attempt skip"))
							newer, err = st.QueryReadOnly(ctx, workQuery)
							require.NoError(t, err)
						}
					} else if mode == "history-error" {
						return map[string]any{"ok": false, "error": "synthetic_after_revival_failure"}, nil
					}
					root := map[string]any{"ts": rootTS, "text": "revived root", "reply_count": 1}
					messages := []any{root}
					if mode == "duplicate-hint" {
						messages = append(messages, map[string]any{"ts": rootTS, "text": "revived root", "reply_count": 0})
					}
					if mode == "child" || mode == "history-error" || mode == "history-incomplete" || mode == "history-limited" {
						root["reply_count"] = 0
						messages = append(messages, map[string]any{"ts": childTS, "thread_ts": rootTS, "text": "history child <@U1>"})
					}
					payload := map[string]any{"ok": true, "messages": messages}
					if histories == 1 && (mode == "history-error" || mode == "completed" || mode == "revoked-later-page") {
						payload["response_metadata"] = map[string]any{"next_cursor": "next-history"}
					}
					if mode == "history-incomplete" {
						payload["has_more"] = true
					}
					if mode == "history-limited" {
						payload["is_limited"] = true
					}
					return payload, nil
				case "/conversations.replies":
					replies++
					require.Equal(t, 1, replies)
					require.Equal(t, "C123", form.Get("channel"))
					require.Equal(t, rootTS, form.Get("ts"))
					work, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
					require.NoError(t, err)
					require.Len(t, work, 1)
					if mode != "completed" && mode != "revoked-later-page" {
						require.NotEqual(t, prepared.Generation, work[0].Generation, "revival must use the newly committed generation")
					}
					payload := map[string]any{"ok": true, "messages": []any{map[string]any{"ts": "1710000003.000000", "thread_ts": rootTS, "text": "reply child"}}}
					if mode == "revoked-later-page" {
						deleteRoot()
						payload["has_more"] = true
						payload["response_metadata"] = map[string]any{"next_cursor": "stale-next"}
					}
					return payload, nil
				}
				return primaryOwnerResponse(r.URL.Path), nil
			})
			client.now = func() time.Time { return now }
			err = client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123"})
			failed := mode == "history-error" || mode == "history-incomplete" || mode == "history-limited"
			if failed {
				require.ErrorContains(t, err, map[string]string{"history-error": "slack conversations.history API response failed", "history-incomplete": "has_more without a continuation cursor", "history-limited": "completeness of the requested interval is uncertified"}[mode])
				if mode == "history-error" {
					requireNativeErrorCode(t, err, "synthetic_after_revival_failure")
				}
				workspaceAfter, err := st.QueryReadOnly(ctx, "select * from sync_state where entity_type='workspace'")
				require.NoError(t, err)
				require.Equal(t, workspaceBefore, workspaceAfter)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, map[bool]int{true: 2, false: 1}[mode == "history-error" || mode == "completed" || mode == "revoked-later-page"], histories)
			wantReply := mode == "hint" || mode == "duplicate-hint" || mode == "child" || mode == "completed" || mode == "revoked-later-page"
			require.Equal(t, map[bool]int{true: 1, false: 0}[wantReply], replies)
			pending, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
			require.NoError(t, err)
			wantPending := failed || mode == "unavailable" || mode == "renewed" || mode == "revoked-later-page"
			require.Len(t, pending, map[bool]int{true: 1, false: 0}[wantPending])
			if wantPending {
				require.Equal(t, rootTS, pending[0].TS)
				require.NotEqual(t, prepared.Generation, pending[0].Generation)
				current, err := st.ThreadWorkCurrent(ctx, pending[0])
				require.NoError(t, err)
				require.True(t, current)
			}
			if mode == "renewed" {
				after, err := st.QueryReadOnly(ctx, workQuery)
				require.NoError(t, err)
				require.Equal(t, newer, after, "another attempt's generation, skip and timestamps must survive")
			}
			if mode == "revoked-later-page" {
				rows, err := st.QueryReadOnly(ctx, "select ts from messages where ts='1710000003.000000'")
				require.NoError(t, err)
				require.Empty(t, rows, "the canceled response must remain discarded")
			}
			coverage, err := readAPIHistory(ctx, st, SourceBot, "T123", "C123", "")
			require.NoError(t, err)
			if failed {
				require.Equal(t, prior.Latest, coverage.Latest)
				require.NotNil(t, coverage.Pending)
			} else {
				require.Equal(t, "1710000400.000000", coverage.Latest)
				require.Nil(t, coverage.Pending)
			}
			status, err := st.Status(ctx)
			require.NoError(t, err)
			require.Equal(t, map[bool]string{true: "partial", false: "full"}[wantPending], status.ThreadState)
		})
	}
}

func TestRetainedThreadConcurrentSyncKeepsOwner(t *testing.T) {
	for _, mode := range []string{"bot-only", "user-capable"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			var workers sync.WaitGroup
			defer func() {
				cancel()
				workers.Wait()
			}()
			path := filepath.Join(t.TempDir(), "concurrent.db")
			contenderStore, err := store.Open(path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, contenderStore.Close()) })
			ownerStore, err := store.Open(path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, ownerStore.Close()) })
			const rootTS, childTS = "1710000001.000000", "1710000002.000000"
			const workQuery = "select * from sync_state where source_name='api-user' and entity_type in ('thread_pending_v1','thread_skip') order by entity_type,entity_id"
			historyEntered, historyRelease := make(chan struct{}), make(chan struct{})
			repliesEntered, repliesRelease := make(chan struct{}), make(chan struct{})
			await := func(signal <-chan struct{}) {
				t.Helper()
				select {
				case <-signal:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			tokens := config.Tokens{Bot: "fixture-contender-bot"}
			if mode == "user-capable" {
				tokens.User = "fixture-contender-user"
			}
			contenderReplies := 0
			contender := primaryOwnerClient(t, tokens, func(r *http.Request, form url.Values) (any, error) {
				switch r.URL.Path {
				case "/conversations.history":
					close(historyEntered)
					select {
					case <-historyRelease:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
					return map[string]any{"ok": true, "messages": []any{map[string]any{"ts": rootTS, "text": "root", "reply_count": 1}}}, nil
				case "/conversations.replies":
					contenderReplies++
					return nil, fmt.Errorf("contender requested unowned replies for %s", form.Get("ts"))
				}
				return primaryOwnerResponse(r.URL.Path), nil
			})
			ownerReplies := 0
			owner := primaryOwnerClient(t, config.Tokens{Bot: "fixture-owner-bot", User: "fixture-owner-user"}, func(r *http.Request, form url.Values) (any, error) {
				switch r.URL.Path {
				case "/conversations.history":
					return map[string]any{"ok": true, "messages": []any{map[string]any{"ts": rootTS, "text": "root", "reply_count": 1}}}, nil
				case "/conversations.replies":
					ownerReplies++
					if ownerReplies != 1 || form.Get("channel") != "C123" || form.Get("ts") != rootTS {
						return nil, fmt.Errorf("unexpected owner replies request: %v", form)
					}
					close(repliesEntered)
					select {
					case <-repliesRelease:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
					return map[string]any{"ok": true, "messages": []any{map[string]any{"ts": childTS, "thread_ts": rootTS, "text": "owner reply <@U1>"}}}, nil
				}
				return primaryOwnerResponse(r.URL.Path), nil
			})
			contenderDone, ownerDone := make(chan error, 1), make(chan error, 1)
			workers.Go(func() { contenderDone <- contender.Sync(ctx, contenderStore, SyncOptions{WorkspaceID: "T123"}) })
			await(historyEntered)
			pending, err := contenderStore.PendingThreadWork(ctx, SourceUser, "T123", "C123")
			require.NoError(t, err)
			require.Empty(t, pending, "the contender prepared before the root existed")
			workers.Go(func() { ownerDone <- owner.Sync(ctx, ownerStore, SyncOptions{WorkspaceID: "T123"}) })
			await(repliesEntered)
			pending, err = ownerStore.PendingThreadWork(ctx, SourceUser, "T123", "C123")
			require.NoError(t, err)
			require.Len(t, pending, 1)
			require.NoError(t, ownerStore.SetSyncState(ctx, SourceUser, "thread_skip", "T123|C123|"+rootTS, "owner skip"))
			before, err := ownerStore.QueryReadOnly(ctx, workQuery)
			require.NoError(t, err)
			close(historyRelease)
			require.ErrorIs(t, <-contenderDone, store.ErrAPIHistorySuperseded)
			require.Zero(t, contenderReplies)
			after, err := contenderStore.QueryReadOnly(ctx, workQuery)
			require.NoError(t, err)
			require.Equal(t, before, after, "the contender must preserve the owner's generation, skip and timestamps")
			current, err := ownerStore.ThreadWorkCurrent(ctx, pending[0])
			require.NoError(t, err)
			require.True(t, current)
			status, err := contenderStore.Status(ctx)
			require.NoError(t, err)
			require.Equal(t, "partial", status.ThreadState)
			close(repliesRelease)
			require.NoError(t, <-ownerDone)
			require.Equal(t, 1, ownerReplies)
			after, err = ownerStore.QueryReadOnly(ctx, workQuery)
			require.NoError(t, err)
			require.Empty(t, after)
			rows, err := ownerStore.QueryReadOnly(ctx, "select ts,thread_ts,source_name,source_rank from messages where ts='"+childTS+"'")
			require.NoError(t, err)
			require.Equal(t, []map[string]any{{"ts": childTS, "thread_ts": rootTS, "source_name": SourceUser, "source_rank": int64(1)}}, rows)
			status, err = ownerStore.Status(ctx)
			require.NoError(t, err)
			require.Equal(t, "full", status.ThreadState)
		})
	}
}

func TestRetainedThreadUnownedHintCanAcquireLaterPage(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "later-page.db")
	st, err := store.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	other, err := store.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, other.Close()) }()
	const rootTS, childTS = "1710000001.000000", "1710000002.000000"
	const workQuery = "select * from sync_state where source_name='api-user' and entity_type in ('thread_pending_v1','thread_skip') order by entity_type,entity_id"
	now := time.Unix(1710000400, 0).UTC()
	var previous []map[string]any
	var priorGeneration string
	histories, replies := 0, 0
	client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, func(r *http.Request, form url.Values) (any, error) {
		switch r.URL.Path {
		case "/conversations.history":
			histories++
			if histories == 1 {
				pending, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
				require.NoError(t, err)
				require.Empty(t, pending)
				retainedOwnerSeed(t, other, rootTS)
				pending, err = other.PrepareThreadWork(ctx, SourceUser, "T123", "C123", nil)
				require.NoError(t, err)
				require.Len(t, pending, 1)
				priorGeneration = pending[0].Generation
				require.NoError(t, other.SetSyncState(ctx, SourceUser, "thread_skip", "T123|C123|"+rootTS, "other owner skip"))
				previous, err = other.QueryReadOnly(ctx, workQuery)
				require.NoError(t, err)
			} else {
				require.Equal(t, 2, histories)
				require.Equal(t, "next-history", form.Get("cursor"))
				require.Zero(t, replies, "unowned hints must not consume the attempted-thread slot")
				after, err := other.QueryReadOnly(ctx, workQuery)
				require.NoError(t, err)
				require.Equal(t, previous, after)
				require.NoError(t, other.MarkMessageDeleted(ctx, store.Message{WorkspaceID: "T123", ChannelID: "C123", TS: rootTS,
					DeletedTS: rootTS, SourceName: SourceBot, SourceRank: 2, RawJSON: `{}`, UpdatedAt: now}, nil))
				deleted, err := other.QueryReadOnly(ctx, "select deleted_ts from messages where workspace_id='T123' and channel_id='C123' and ts='"+rootTS+"'")
				require.NoError(t, err)
				require.Equal(t, []map[string]any{{"deleted_ts": rootTS}}, deleted)
				after, err = other.QueryReadOnly(ctx, workQuery)
				require.NoError(t, err)
				require.Empty(t, after, "committed deletion must retire the prior generation and skip")
			}
			payload := map[string]any{"ok": true, "messages": []any{map[string]any{"ts": rootTS, "text": "revived root", "reply_count": 1}}}
			if histories == 1 {
				payload["response_metadata"] = map[string]any{"next_cursor": "next-history"}
			}
			return payload, nil
		case "/conversations.replies":
			replies++
			require.Equal(t, 2, histories)
			require.Equal(t, rootTS, form.Get("ts"))
			pending, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
			require.NoError(t, err)
			require.Len(t, pending, 1)
			require.NotEqual(t, priorGeneration, pending[0].Generation)
			return map[string]any{"ok": true, "messages": []any{map[string]any{"ts": childTS, "thread_ts": rootTS, "text": "later reply"}}}, nil
		}
		return primaryOwnerResponse(r.URL.Path), nil
	})
	client.now = func() time.Time { return now }
	require.NoError(t, client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123"}))
	require.Equal(t, 2, histories)
	require.Equal(t, 1, replies)
	after, err := st.QueryReadOnly(ctx, workQuery)
	require.NoError(t, err)
	require.Empty(t, after)
	rows, err := st.QueryReadOnly(ctx, "select thread_ts,source_name from messages where ts='"+childTS+"'")
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"thread_ts": rootTS, "source_name": SourceUser}}, rows)
}

func TestRetainedThreadUnattemptedRevocationCanAcquireLaterPage(t *testing.T) {
	for _, mode := range []string{"replies", "cached-skip"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			st := mustStore(t)
			defer func() { require.NoError(t, st.Close()) }()
			const firstTS, secondTS, childTS = "1710000001.000000", "1710000002.000000", "1710000003.000000"
			retainedOwnerSeed(t, st, firstTS)
			retainedOwnerSeed(t, st, secondTS)
			now := time.Unix(1710000400, 0).UTC()
			histories := 0
			var calls []string
			var preparedSecond store.ThreadWork
			client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, func(r *http.Request, form url.Values) (any, error) {
				switch r.URL.Path {
				case "/conversations.history":
					histories++
					require.Equal(t, "C123", form.Get("channel"))
					calls = append(calls, "history:"+form.Get("cursor"))
					second := map[string]any{"ts": secondTS, "text": "revived second root", "reply_count": 1}
					if histories == 1 {
						require.Empty(t, form.Get("cursor"))
						pending, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
						require.NoError(t, err)
						require.Len(t, pending, 2)
						require.Equal(t, secondTS, pending[1].TS)
						preparedSecond = pending[1]
						return map[string]any{"ok": true, "messages": []any{
							map[string]any{"ts": firstTS, "text": "first root", "reply_count": 1}, second,
						}, "response_metadata": map[string]any{"next_cursor": "next-history"}}, nil
					}
					require.Equal(t, 2, histories)
					require.Equal(t, "next-history", form.Get("cursor"))
					require.Equal(t, []string{"history:", "replies:" + firstTS, "history:next-history"}, calls)
					current, err := st.ThreadWorkCurrent(ctx, preparedSecond)
					require.NoError(t, err)
					require.False(t, current)
					rows, err := st.QueryReadOnly(ctx, "select deleted_ts from messages where ts='"+secondTS+"'")
					require.NoError(t, err)
					require.Equal(t, []map[string]any{{"deleted_ts": secondTS}}, rows)
					rows, err = st.QueryReadOnly(ctx, "select * from sync_state where entity_type='thread_skip' and entity_id='T123|C123|"+secondTS+"'")
					require.NoError(t, err)
					require.Empty(t, rows, "the canceled generation cannot commit a cached skip")
					return map[string]any{"ok": true, "messages": []any{second}}, nil
				case "/conversations.replies":
					require.Equal(t, "C123", form.Get("channel"))
					require.Empty(t, form.Get("cursor"))
					calls = append(calls, "replies:"+form.Get("ts"))
					if form.Get("ts") == firstTS {
						require.Equal(t, 1, histories)
						// Both page jobs are committed before A's request cancels B.
						require.NoError(t, st.MarkMessageDeleted(ctx, store.Message{
							WorkspaceID: "T123", ChannelID: "C123", TS: secondTS, DeletedTS: secondTS,
							SourceName: SourceBot, SourceRank: 2, RawJSON: `{}`, UpdatedAt: now,
						}, nil))
						if mode == "cached-skip" {
							return map[string]any{"ok": false, "error": "missing_scope"}, nil
						}
						return map[string]any{"ok": true, "messages": []any{}}, nil
					}
					require.Equal(t, "replies", mode, "cached missing_scope must avoid B's RPC")
					require.Equal(t, secondTS, form.Get("ts"))
					require.Equal(t, 2, histories)
					pending, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
					require.NoError(t, err)
					require.Len(t, pending, 1)
					require.Equal(t, secondTS, pending[0].TS)
					require.NotEqual(t, preparedSecond.Generation, pending[0].Generation)
					return map[string]any{"ok": true, "messages": []any{
						map[string]any{"ts": childTS, "thread_ts": secondTS, "text": "later B reply <@U1>"},
					}}, nil
				}
				return primaryOwnerResponse(r.URL.Path), nil
			})
			client.now = func() time.Time { return now }
			require.NoError(t, client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123"}))
			wantCalls := []string{"history:", "replies:" + firstTS, "history:next-history"}
			if mode == "replies" {
				wantCalls = append(wantCalls, "replies:"+secondTS)
			}
			require.Equal(t, wantCalls, calls)
			pending, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
			require.NoError(t, err)
			skips, err := st.ListSyncState(ctx, SourceUser, "thread_skip", 20)
			require.NoError(t, err)
			rows, err := st.QueryReadOnly(ctx, "select ts,thread_ts,source_name,source_rank from messages where ts='"+childTS+"'")
			require.NoError(t, err)
			if mode == "cached-skip" {
				require.Len(t, pending, 2)
				require.Equal(t, secondTS, pending[1].TS)
				require.NotEqual(t, preparedSecond.Generation, pending[1].Generation)
				current, err := st.ThreadWorkCurrent(ctx, pending[1])
				require.NoError(t, err)
				require.True(t, current)
				require.ElementsMatch(t, []store.SyncStateRow{
					{SourceName: SourceUser, EntityType: "thread_skip", EntityID: "T123|C123|" + firstTS, Value: "missing_scope"},
					{SourceName: SourceUser, EntityType: "thread_skip", EntityID: "T123|C123|" + secondTS, Value: "missing_scope"},
				}, skips)
				require.Empty(t, rows)
			} else {
				require.Empty(t, pending)
				require.Empty(t, skips)
				require.Equal(t, []map[string]any{{"ts": childTS, "thread_ts": secondTS, "source_name": SourceUser, "source_rank": int64(1)}}, rows)
			}
			coverage, err := readAPIHistory(ctx, st, SourceBot, "T123", "C123", "")
			require.NoError(t, err)
			require.Equal(t, store.APIHistoryState{Complete: true, Latest: "1710000400.000000"}, coverage)
			status, err := st.Status(ctx)
			require.NoError(t, err)
			require.Equal(t, map[bool]string{true: "partial", false: "full"}[mode == "cached-skip"], status.ThreadState)
		})
	}
}

func TestTailDeletionRetiresPendingThread(t *testing.T) {
	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()
	ctx := context.Background()
	retainedOwnerSeed(t, st, "1710000001.000000")
	for _, source := range []string{SourceUser, "mcp"} {
		_, err := st.PrepareThreadWork(ctx, source, "T123", "C123", nil)
		require.NoError(t, err)
	}
	require.NoError(t, st.SetSyncState(ctx, SourceUser, "thread_skip", "T123|C123|1710000001.000000", "missing_scope"))
	client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot"}, func(r *http.Request, _ url.Values) (any, error) {
		t.Fatalf("typed event should not make an API request: %s", r.URL.Path)
		return nil, nil
	})
	client.now = func() time.Time { return time.Unix(1710000100, 0).UTC() }
	event, err := slackevents.ParseEvent([]byte(`{"type":"event_callback","team_id":"T123","event":{"type":"message","subtype":"message_deleted","channel":"C123","channel_type":"channel","ts":"1710000005.000000","deleted_ts":"1710000001.000000"}}`), slackevents.OptionNoVerifyToken())
	require.NoError(t, err)
	require.NoError(t, client.HandleEventsAPIEvent(ctx, st, "T123", event))
	for _, source := range []string{SourceUser, "mcp"} {
		pending, err := st.PendingThreadWork(ctx, source, "T123", "C123")
		require.NoError(t, err)
		require.Empty(t, pending)
	}
	rows, err := st.QueryReadOnly(ctx, "select ts,deleted_ts from messages")
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"ts": "1710000001.000000", "deleted_ts": "1710000001.000000"}}, rows)
	skips, err := st.ListSyncState(ctx, SourceUser, "thread_skip", 20)
	require.NoError(t, err)
	require.Empty(t, skips)
	ordinary := primaryOwnerClient(t, config.Tokens{User: "fixture-user"}, func(r *http.Request, _ url.Values) (any, error) {
		require.NotEqual(t, "/conversations.replies", r.URL.Path, "a tombstoned root must not be fetched to clear a stale skip")
		return primaryOwnerResponse(r.URL.Path), nil
	})
	require.NoError(t, ordinary.Sync(ctx, st, SyncOptions{WorkspaceID: "T123"}))
	status, err := st.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, "full", status.ThreadState)
}

func TestOrdinarySyncReconcilesMergedTombstoneSkips(t *testing.T) {
	for _, marker := range []string{"deleted_ts", "subtype"} {
		t.Run(marker, func(t *testing.T) {
			ctx := context.Background()
			st := mustStore(t)
			defer func() { require.NoError(t, st.Close()) }()
			const ts = "1710000001.000000"
			const skipQuery = "select * from sync_state where source_name='api-user' and entity_type='thread_skip'"
			first, replies := true, 0
			now := time.Unix(1710000200, 0).UTC()
			client := primaryOwnerClient(t, config.Tokens{User: "fixture-user"}, func(r *http.Request, form url.Values) (any, error) {
				if first && r.URL.Path == "/conversations.history" {
					return map[string]any{"ok": true, "messages": []any{map[string]any{"ts": ts, "channel": "C123", "thread_ts": ts, "reply_count": 1, "text": "parent"}}}, nil
				}
				if r.URL.Path == "/conversations.replies" {
					replies++
					require.True(t, first, "ordinary reconciliation must not fetch a stored tombstone")
					require.Equal(t, ts, form.Get("ts"))
					return map[string]any{"ok": false, "error": "missing_scope"}, nil
				}
				return primaryOwnerResponse(r.URL.Path), nil
			})
			client.now = func() time.Time { return now }
			// Current-history replies can record a skip during a sliced sync,
			// which deliberately creates no ordinary pending job.
			require.NoError(t, client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123", Since: "1710000000.000000"}))
			require.Equal(t, 1, replies)
			first = false
			before, err := st.QueryReadOnly(ctx, skipQuery)
			require.NoError(t, err)
			require.Len(t, before, 1)
			pending, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
			require.NoError(t, err)
			require.Empty(t, pending)
			status, err := st.Status(ctx)
			require.NoError(t, err)
			require.Equal(t, "partial", status.ThreadState)

			donor := mustStore(t)
			defer func() { require.NoError(t, donor.Close()) }()
			require.NoError(t, donor.UpsertWorkspace(ctx, store.Workspace{ID: "T123", Name: "Fixture", RawJSON: "{}", UpdatedAt: now}))
			require.NoError(t, donor.UpsertChannel(ctx, store.Channel{ID: "C123", WorkspaceID: "T123", Name: "fixture", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now}))
			deleted := store.Message{WorkspaceID: "T123", ChannelID: "C123", TS: ts, ThreadTS: ts, SourceName: SourceUser, SourceRank: 1, RawJSON: "{}", UpdatedAt: now.Add(time.Second)}
			if marker == "deleted_ts" {
				deleted.DeletedTS = "1710000201.000000"
			} else {
				deleted.Subtype = "message_deleted"
			}
			require.NoError(t, donor.UpsertMessage(ctx, deleted, nil))
			opts := share.Options{RepoPath: filepath.Join(t.TempDir(), "share")}
			_, err = share.Export(ctx, donor, opts)
			require.NoError(t, err)
			_, err = share.Import(ctx, st, opts)
			require.NoError(t, err)
			rows, err := st.QueryReadOnly(ctx, "select coalesce(deleted_ts,'') as deleted_ts,coalesce(subtype,'') as subtype,reply_count from messages")
			require.NoError(t, err)
			require.Equal(t, []map[string]any{{"deleted_ts": deleted.DeletedTS, "subtype": deleted.Subtype, "reply_count": int64(0)}}, rows)
			for _, scoped := range []SyncOptions{
				{WorkspaceID: "T123", Since: "1710000000.000000"},
				{WorkspaceID: "T123", Channels: []string{"COTHER"}},
			} {
				require.NoError(t, client.Sync(ctx, st, scoped))
				after, err := st.QueryReadOnly(ctx, skipQuery)
				require.NoError(t, err)
				require.Equal(t, before, after, "sliced or unselected sync cannot reconcile ordinary work")
			}
			require.NoError(t, client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123"}))
			after, err := st.QueryReadOnly(ctx, skipQuery)
			require.NoError(t, err)
			require.Empty(t, after)
			pending, err = st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
			require.NoError(t, err)
			require.Empty(t, pending)
			require.Equal(t, 1, replies)
			status, err = st.Status(ctx)
			require.NoError(t, err)
			require.Equal(t, "full", status.ThreadState)
		})
	}
}

func TestRetainedThreadRespectsDMExclusion(t *testing.T) {
	ctx := context.Background()
	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()
	now := time.Unix(1710000000, 0).UTC()
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "D123", WorkspaceID: "T123", Name: "dm", Kind: "im", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertMessage(ctx, store.Message{ChannelID: "D123", WorkspaceID: "T123", TS: "1710000001.000000", ReplyCount: 1, SourceName: SourceUser, SourceRank: 1, RawJSON: "{}", UpdatedAt: now}, nil))
	_, err := st.PrepareThreadWork(ctx, SourceUser, "T123", "D123", nil)
	require.NoError(t, err)
	before, err := st.QueryReadOnly(ctx, "select * from sync_state where entity_type='thread_pending_v1'")
	require.NoError(t, err)
	var methods []string
	client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, func(r *http.Request, _ url.Values) (any, error) {
		methods = append(methods, r.URL.Path)
		if r.URL.Path == "/conversations.list" {
			return map[string]any{"ok": true, "channels": []any{map[string]any{"id": "D123", "is_im": true}}}, nil
		}
		return primaryOwnerResponse(r.URL.Path), nil
	})
	require.NoError(t, client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123"}))
	require.Equal(t, []string{"/auth.test", "/auth.test", "/conversations.list", "/users.list"}, methods)
	after, err := st.QueryReadOnly(ctx, "select * from sync_state where entity_type='thread_pending_v1'")
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestRetainedThreadPreservesRetentionAndFullRestore(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%t", full), func(t *testing.T) {
			ctx := context.Background()
			st := mustStore(t)
			defer func() { require.NoError(t, st.Close()) }()
			retainedOwnerSeed(t, st, "1710000001.000000")
			require.NoError(t, st.SetSyncState(ctx, "retention", "channel_floor", "T123|C123", "1710000010.000000"))
			require.NoError(t, st.SetSyncState(ctx, "retention", "channel_seed", "T123|C123", "1"))
			replies := 0
			client := primaryOwnerClient(t, config.Tokens{User: "fixture-user"}, func(r *http.Request, form url.Values) (any, error) {
				if r.URL.Path == "/conversations.history" {
					require.Equal(t, map[bool]string{true: "", false: "1710000010.000000"}[full], form.Get("oldest"))
				}
				if r.URL.Path == "/conversations.replies" {
					replies++
					return map[string]any{"ok": true, "messages": []any{map[string]any{"ts": "1710000020.000000", "thread_ts": "1710000001.000000", "text": "late reply to old parent"}}}, nil
				}
				return primaryOwnerResponse(r.URL.Path), nil
			})
			require.NoError(t, client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123", Full: full}))
			require.Equal(t, 1, replies)
			rows, err := st.QueryReadOnly(ctx, "select ts from messages order by ts")
			require.NoError(t, err)
			require.Len(t, rows, map[bool]int{true: 2, false: 1}[full])
			pending, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
			require.NoError(t, err)
			require.Empty(t, pending)
		})
	}
}

func TestRetainedThreadRevocationDuringHTTP(t *testing.T) {
	for _, mode := range []string{"history-delete", "replies-delete", "empty-delete", "error-delete", "invalid-delete", "renewed-full", "complete-full"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			st := mustStore(t)
			defer func() { require.NoError(t, st.Close()) }()
			const rootTS = "1710000001.000000"
			retainedOwnerSeed(t, st, rootTS)
			key := "T123|C123|" + rootTS
			if mode == "complete-full" {
				require.NoError(t, st.SetSyncState(ctx, SourceUser, "thread_skip", "T123|legacy", "old skip"))
			}
			deleteRoot := func() {
				require.NoError(t, st.MarkMessageDeleted(ctx, store.Message{
					WorkspaceID: "T123", ChannelID: "C123", TS: rootTS, DeletedTS: rootTS,
					SourceName: SourceBot, SourceRank: 2, RawJSON: `{"subtype":"message_deleted"}`, UpdatedAt: time.Unix(1710000010, 0).UTC(),
				}, nil))
			}
			calls := 0
			client := primaryOwnerClient(t, config.Tokens{User: "fixture-user"}, func(r *http.Request, form url.Values) (any, error) {
				if r.URL.Path == "/conversations.history" && mode == "history-delete" {
					deleteRoot()
				}
				if r.URL.Path == "/conversations.list" && form.Get("types") == "im,mpim" {
					return map[string]any{"ok": true, "channels": []any{}}, nil
				}
				if r.URL.Path != "/conversations.replies" {
					return primaryOwnerResponse(r.URL.Path), nil
				}
				calls++
				require.Equal(t, 1, calls, "revocation must stop subsequent cursor requests")
				require.Equal(t, rootTS, form.Get("ts"))
				payload := map[string]any{"ok": true, "messages": []any{
					map[string]any{"ts": rootTS, "thread_ts": rootTS, "text": "reply echo"},
					map[string]any{"ts": "1710000003.000000", "thread_ts": rootTS, "text": "reply child"},
				}}
				if mode == "complete-full" {
					return payload, nil
				}
				if mode == "renewed-full" {
					_, err := st.ApplyWriteBatch(ctx, store.WriteBatch{PendingThreads: []store.ThreadWork{{SourceName: SourceUser, WorkspaceID: "T123", ChannelID: "C123", TS: rootTS}}})
					require.NoError(t, err)
					require.NoError(t, st.SetSyncState(ctx, SourceUser, "thread_skip", key, "new attempt skip"))
				} else {
					deleteRoot()
				}
				payload["response_metadata"] = map[string]any{"next_cursor": "second"}
				payload["has_more"] = true
				switch mode {
				case "empty-delete":
					payload["messages"] = []any{}
				case "error-delete":
					payload = map[string]any{"ok": false, "error": "thread_not_found"}
				case "invalid-delete":
					payload["messages"] = []any{map[string]any{"ts": "", "text": "invalid stale reply"}}
				}
				return payload, nil
			})
			full := mode == "renewed-full" || mode == "complete-full"
			if full {
				client.WithDMPolicy(admission.Include)
			}
			require.NoError(t, client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123", Full: full}))
			require.Equal(t, map[bool]int{true: 0, false: 1}[mode == "history-delete"], calls)
			pending, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
			require.NoError(t, err)
			require.Len(t, pending, map[bool]int{true: 1, false: 0}[mode == "renewed-full"])
			rows, err := st.QueryReadOnly(ctx, "select ts,coalesce(deleted_ts,'') as deleted_ts,text from messages order by ts")
			require.NoError(t, err)
			if mode == "complete-full" {
				require.Len(t, rows, 2)
			} else {
				require.Len(t, rows, 1, "stale responses must not add the child or overwrite the parent")
				if mode == "renewed-full" {
					require.Equal(t, "retained root", rows[0]["text"])
				} else {
					require.Equal(t, rootTS, rows[0]["deleted_ts"])
				}
			}
			skips, err := st.ListSyncState(ctx, SourceUser, "thread_skip", 20)
			require.NoError(t, err)
			if mode == "renewed-full" {
				require.Equal(t, []store.SyncStateRow{{SourceName: SourceUser, EntityType: "thread_skip", EntityID: key, Value: "new attempt skip"}}, skips)
			} else {
				require.Empty(t, skips)
			}
		})
	}
}

func TestRetainedThreadCachedSkipCannotReplaceNewerWork(t *testing.T) {
	ctx := context.Background()
	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()
	retainedOwnerSeed(t, st, "1710000001.000000")
	retainedOwnerSeed(t, st, "1710000002.000000")
	const secondKey = "T123|C123|1710000002.000000"
	calls := 0
	client := primaryOwnerClient(t, config.Tokens{User: "fixture-user"}, func(r *http.Request, form url.Values) (any, error) {
		if r.URL.Path != "/conversations.replies" {
			return primaryOwnerResponse(r.URL.Path), nil
		}
		calls++
		require.Equal(t, "1710000001.000000", form.Get("ts"))
		_, err := st.ApplyWriteBatch(ctx, store.WriteBatch{PendingThreads: []store.ThreadWork{{SourceName: SourceUser, WorkspaceID: "T123", ChannelID: "C123", TS: "1710000002.000000"}}})
		require.NoError(t, err)
		require.NoError(t, st.SetSyncState(ctx, SourceUser, "thread_skip", secondKey, "new attempt skip"))
		return map[string]any{"ok": false, "error": "missing_scope"}, nil
	})
	require.NoError(t, client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123"}))
	require.Equal(t, 1, calls)
	value, err := st.GetSyncState(ctx, SourceUser, "thread_skip", secondKey)
	require.NoError(t, err)
	require.Equal(t, "new attempt skip", value)
	pending, err := st.PendingThreadWork(ctx, SourceUser, "T123", "C123")
	require.NoError(t, err)
	require.Len(t, pending, 2)
}

func TestFullThreadSkipCleanupStaysWithinWorkspace(t *testing.T) {
	ctx := context.Background()
	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()
	const rootTS = "1710000001.000000"
	now := time.Unix(1710000000, 0).UTC()
	for _, id := range []string{"T1", "T2"} {
		require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{ID: id, Name: "Fixture", RawJSON: "{}", UpdatedAt: now}))
		require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: id + "C", WorkspaceID: id, Name: "fixture", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now}))
	}
	require.NoError(t, st.UpsertMessage(ctx, store.Message{ChannelID: "T2C", WorkspaceID: "T2", TS: rootTS, ThreadTS: rootTS, ReplyCount: 1, Text: "retained root", SourceName: SourceBot, SourceRank: 2, RawJSON: "{}", UpdatedAt: now}, nil))
	_, err := st.ApplyWriteBatch(ctx, store.WriteBatch{PendingThreads: []store.ThreadWork{{SourceName: SourceUser, WorkspaceID: "T2", ChannelID: "T2C", TS: rootTS}}})
	require.NoError(t, err)
	require.NoError(t, st.SetSyncState(ctx, SourceUser, "thread_skip", "T1|legacy", "obsolete skip"))
	require.NoError(t, st.SetSyncState(ctx, SourceUser, "thread_skip", "T2|T2C|"+rootTS, "pending skip"))
	const t2StateQuery = `select * from sync_state where source_name = 'api-user'
and entity_id in ('["T2","T2C","1710000001.000000"]', 'T2|T2C|1710000001.000000')
order by entity_type, entity_id`
	before, err := st.QueryReadOnly(ctx, t2StateQuery)
	require.NoError(t, err)
	require.Len(t, before, 2)
	replyCalls := map[string]int{}
	clientFor := func(workspaceID string) *Client {
		return primaryOwnerClient(t, config.Tokens{User: "fixture-user-" + workspaceID}, func(r *http.Request, form url.Values) (any, error) {
			switch r.URL.Path {
			case "/auth.test":
				return map[string]any{"ok": true, "team_id": workspaceID, "team": "Fixture"}, nil
			case "/conversations.list":
				if form.Get("types") == "im,mpim" {
					return map[string]any{"ok": true, "channels": []any{}}, nil
				}
				return map[string]any{"ok": true, "channels": []any{map[string]any{"id": workspaceID + "C", "name": "fixture", "is_channel": true}}}, nil
			case "/conversations.history":
				require.Equal(t, workspaceID+"C", form.Get("channel"))
				return primaryOwnerResponse(r.URL.Path), nil
			case "/conversations.replies":
				replyCalls[workspaceID]++
				require.Equal(t, "T2", workspaceID)
				require.Equal(t, "T2C", form.Get("channel"))
				require.Equal(t, rootTS, form.Get("ts"))
				return map[string]any{"ok": true, "messages": []any{
					map[string]any{"ts": rootTS, "thread_ts": rootTS, "text": "reply echo"},
					map[string]any{"ts": "1710000003.000000", "thread_ts": rootTS, "text": "reply child"},
				}}, nil
			default:
				return primaryOwnerResponse(r.URL.Path), nil
			}
		}).WithDMPolicy(admission.Include)
	}
	require.NoError(t, clientFor("T1").Sync(ctx, st, SyncOptions{WorkspaceID: "T1", Full: true}))
	t1Skips, err := st.QueryReadOnly(ctx, "select * from sync_state where source_name='api-user' and entity_type='thread_skip' and entity_id like 'T1|%'")
	require.NoError(t, err)
	require.Empty(t, t1Skips)
	after, err := st.QueryReadOnly(ctx, t2StateQuery)
	require.NoError(t, err)
	require.Equal(t, before, after, "other workspace generations, skips and updated_at must remain unchanged")
	status, err := st.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, "partial", status.ThreadState)
	require.Empty(t, replyCalls)

	require.NoError(t, clientFor("T2").Sync(ctx, st, SyncOptions{WorkspaceID: "T2"}))
	require.Equal(t, map[string]int{"T2": 1}, replyCalls)
	after, err = st.QueryReadOnly(ctx, t2StateQuery)
	require.NoError(t, err)
	require.Empty(t, after)
	status, err = st.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, "full", status.ThreadState)
}

func retainedOwnerSeed(t *testing.T, st *store.Store, ts string) {
	t.Helper()
	now := time.Unix(1710000000, 0).UTC()
	require.NoError(t, st.UpsertChannel(context.Background(), store.Channel{ID: "C123", WorkspaceID: "T123", Name: "fixture", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertMessage(context.Background(), store.Message{ChannelID: "C123", WorkspaceID: "T123", TS: ts, ThreadTS: ts, Text: "retained root", NormalizedText: "retained root", ReplyCount: 1, SourceName: SourceBot, SourceRank: 2, RawJSON: "{}", UpdatedAt: now}, nil))
}
