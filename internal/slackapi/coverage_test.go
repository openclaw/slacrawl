package slackapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/share"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/require"
)

func TestHistoryCoverageRetriesIncompleteInterval(t *testing.T) {
	for _, threadFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "history page", true: "thread"}[threadFailure], func(t *testing.T) {
			ctx := context.Background()
			st := mustStore(t)
			defer st.Close()
			now := time.Unix(1710000200, 0).UTC()
			require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C123", WorkspaceID: "T123", Name: "test", UpdatedAt: now}))
			// Desktop observations do not establish API coverage.
			require.NoError(t, st.UpsertMessage(ctx, store.Message{ChannelID: "C123", WorkspaceID: "T123", TS: "1710000100.000000", Text: "desktop", NormalizedText: "desktop", SourceRank: 3, SourceName: "desktop", RawJSON: "{}", UpdatedAt: now}, nil))
			fail := true
			var oldest []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/conversations.replies" {
					if fail {
						_, _ = w.Write([]byte(`{"ok":false,"error":"synthetic_failure"}`))
					} else {
						_, _ = w.Write([]byte(`{"ok":true,"messages":[]}`))
					}
					return
				}
				require.Equal(t, "/conversations.history", r.URL.Path)
				require.Equal(t, "1710000200.000000", r.Form.Get("latest"))
				if r.Form.Get("cursor") == "" {
					oldest = append(oldest, r.Form.Get("oldest"))
					replies := "0"
					if threadFailure {
						replies = "1"
					}
					_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","ts":"1710000000.000000","text":"newer","reply_count":` + replies + `}],"response_metadata":{"next_cursor":"older"}}`))
				} else if fail {
					_, _ = w.Write([]byte(`{"ok":false,"error":"synthetic_failure"}`))
				} else {
					_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","ts":"1709800000.000000","text":"older"}],"response_metadata":{"next_cursor":""}}`))
				}
			}))
			defer server.Close()
			client := NewWithOptions(config.Tokens{Bot: "test", User: "test-user"}, server.URL+"/", server.Client())
			channels := []slack.Channel{{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C123"}, Name: "test"}}}
			_, plan, err := client.channelSyncPlan(ctx, st, "T123", channels, SyncOptions{})
			require.NoError(t, err)
			require.Empty(t, plan["C123"])
			source := channelSyncSource{historyClient: client.bot, token: "test", sourceName: SourceBot, sourceRank: 2}
			err = client.syncChannelMessagesWithSource(ctx, st, "T123", channels[0], plan["C123"], false, now, threadFailure, source)
			require.ErrorContains(t, err, "synthetic_failure")
			state, err := loadHistoryCoverage(ctx, st, SourceBot, "T123", "C123", "")
			require.NoError(t, err)
			require.NotNil(t, state.Pending)
			require.False(t, state.Complete)
			_, plan, err = client.channelSyncPlan(ctx, st, "T123", channels, SyncOptions{})
			require.NoError(t, err)
			require.Empty(t, plan["C123"])
			fail = false
			require.NoError(t, client.syncChannelMessagesWithSource(ctx, st, "T123", channels[0], plan["C123"], false, now, threadFailure, source))
			require.Equal(t, []string{"", ""}, oldest)
			state, err = loadHistoryCoverage(ctx, st, SourceBot, "T123", "C123", "")
			require.NoError(t, err)
			require.True(t, state.Complete)
			require.Nil(t, state.Pending)
			require.Equal(t, "1710000200.000000", state.Latest)
			_, plan, err = client.channelSyncPlan(ctx, st, "T123", channels, SyncOptions{})
			require.NoError(t, err)
			require.Equal(t, "1709996600.000000", plan["C123"])
			rows, err := st.SearchMessages(ctx, store.SearchOptions{Query: "older", Mode: store.SearchModeRawFTS, Limit: 10})
			require.NoError(t, err)
			require.Len(t, rows, 1)
		})
	}
}

func TestHistoryCoverageAdvancesEmptyAndSparseScans(t *testing.T) {
	for _, messages := range []string{`[]`, `[{"type":"message","ts":"1709800000.000000","text":"old"}]`} {
		t.Run(messages, func(t *testing.T) {
			ctx := context.Background()
			st := mustStore(t)
			defer st.Close()
			now := time.Unix(1710000200, 123456000).UTC()
			channel := slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C123"}}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				require.Equal(t, "1710000200.123456", r.Form.Get("latest"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true,"messages":` + messages + `}`))
			}))
			defer server.Close()
			client := NewWithOptions(config.Tokens{Bot: "test"}, server.URL+"/", server.Client())
			source := channelSyncSource{historyClient: client.bot, token: "test", sourceName: SourceBot, sourceRank: 2}
			require.NoError(t, client.syncChannelMessagesWithSource(ctx, st, "T123", channel, "", false, now, false, source))
			coverage, err := loadHistoryCoverage(ctx, st, SourceBot, "T123", "C123", "")
			require.NoError(t, err)
			require.True(t, coverage.Complete)
			require.Nil(t, coverage.Pending)
			require.Equal(t, "1710000200.123456", coverage.Latest)
			_, plan, err := client.channelSyncPlan(ctx, st, "T123", []slack.Channel{channel}, SyncOptions{})
			require.NoError(t, err)
			require.Equal(t, repairOldest(coverage.Latest, time.Hour), plan["C123"])
		})
	}
}

func TestHistoryCoverageScopeAndExplicitBounds(t *testing.T) {
	ctx := context.Background()
	st := mustStore(t)
	defer st.Close()
	channel := slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C123"}}}
	pending := "1700000000.000000"
	require.NoError(t, saveHistoryCoverage(ctx, st, SourceBot, "T123", "C123", "", historyCoverage{Complete: true, Latest: "1710000000.000000", Pending: &pending}))
	client := &Client{}
	for _, tc := range []struct {
		source, workspace string
		opts              SyncOptions
		want              string
	}{
		{SourceBot, "T123", SyncOptions{}, pending},
		{SourceUser, "T123", SyncOptions{}, ""},
		{SourceBot, "T999", SyncOptions{}, ""},
		{SourceBot, "T123", SyncOptions{Since: "1690000000.000000"}, "1690000000.000000"},
		{SourceBot, "T123", SyncOptions{Full: true}, ""},
	} {
		_, plan, err := client.channelSyncPlan(ctx, st, tc.workspace, []slack.Channel{channel}, tc.opts, tc.source)
		require.NoError(t, err)
		require.Equal(t, tc.want, plan["C123"])
	}
	require.NoError(t, saveHistoryCoverage(ctx, st, SourceBot, "Tnew", "C123", "recent", historyCoverage{Complete: true, Latest: "1710000000.000000"}))
	_, plan, err := client.channelSyncPlan(ctx, st, "Tnew", []slack.Channel{channel}, SyncOptions{})
	require.NoError(t, err)
	require.Empty(t, plan["C123"])
}

func TestRepairWorkspaceRetriesPendingHistory(t *testing.T) {
	ctx := context.Background()
	st := mustStore(t)
	defer st.Close()
	now := time.Unix(1710000200, 0).UTC()
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C123", WorkspaceID: "T123", Name: "test", UpdatedAt: now}))
	require.NoError(t, st.UpsertMessage(ctx, store.Message{
		ChannelID: "C123", WorkspaceID: "T123", TS: "1709900000.000000",
		Text: "seed", NormalizedText: "seed", SourceRank: 2, SourceName: SourceBot,
		RawJSON: "{}", UpdatedAt: now,
	}, nil))
	require.NoError(t, saveHistoryCoverage(ctx, st, SourceBot, "T123", "C123", "", historyCoverage{Complete: true, Latest: "1709900000.000000"}))
	fail := true
	var oldest []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.list":
			_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"C123","name":"test"}]}`))
		case "/conversations.history":
			if r.Form.Get("cursor") == "" {
				oldest = append(oldest, r.Form.Get("oldest"))
				_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","ts":"1710000000.000000","text":"newer"}],"response_metadata":{"next_cursor":"older"}}`))
			} else if fail {
				_, _ = w.Write([]byte(`{"ok":false,"error":"synthetic_failure"}`))
			} else {
				_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","ts":"1709896500.000000","text":"recovered"}]}`))
			}
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewWithOptions(config.Tokens{Bot: "test"}, server.URL+"/", server.Client())
	client.now = func() time.Time { return now }
	require.ErrorContains(t, client.repairWorkspace(ctx, st, "T123"), "synthetic_failure")
	coverage, err := loadHistoryCoverage(ctx, st, SourceBot, "T123", "C123", "")
	require.NoError(t, err)
	require.NotNil(t, coverage.Pending)
	require.Equal(t, "1709896400.000000", *coverage.Pending)
	fail = false
	require.NoError(t, client.repairWorkspace(ctx, st, "T123"))
	require.Equal(t, []string{"1709896400.000000", "1709896400.000000"}, oldest)
	coverage, err = loadHistoryCoverage(ctx, st, SourceBot, "T123", "C123", "")
	require.NoError(t, err)
	require.Nil(t, coverage.Pending)
	require.True(t, coverage.Complete)
	rows, err := st.SearchMessages(ctx, store.SearchOptions{Query: "recovered", Mode: store.SearchModeRawFTS, Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

func TestHistoryMigrationAndRestoreRequireLocalCoverage(t *testing.T) {
	for _, mode := range []string{"migration", "restore"} {
		for _, sourceName := range []string{SourceBot, SourceUser} {
			for _, floor := range []string{"", "1700000000.000000"} {
				t.Run(mode+"/"+sourceName+"/floor="+floor, func(t *testing.T) {
					ctx := context.Background()
					dbPath := filepath.Join(t.TempDir(), "archive.db")
					st, err := store.Open(dbPath)
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, st.Close()) })
					now := time.Unix(1710000200, 0).UTC()
					channel := slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C123"}}}
					require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C123", WorkspaceID: "T123", Name: "fixture", UpdatedAt: now}))
					require.NoError(t, st.UpsertMessage(ctx, store.Message{
						ChannelID: "C123", WorkspaceID: "T123", TS: "1710000100.000000",
						Text: "newer observation", NormalizedText: "newer observation", SourceRank: 2,
						SourceName: sourceName, RawJSON: "{}", UpdatedAt: now,
					}, nil))
					if floor != "" {
						require.NoError(t, st.SetSyncState(ctx, "retention", "channel_floor", "T123|C123", floor))
						require.NoError(t, st.SetSyncState(ctx, "retention", "channel_seed", "T123|C123", "1"))
					}
					require.NoError(t, saveHistoryCoverage(ctx, st, sourceName, "T123", "C123", "", historyCoverage{Complete: true, Latest: "1710000100.000000"}))
					requests, fail := 0, true
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests++
						require.Equal(t, "/conversations.history", r.URL.Path)
						require.NoError(t, r.ParseForm())
						require.Equal(t, floor, r.Form.Get("oldest"))
						w.Header().Set("Content-Type", "application/json")
						if fail {
							_, _ = w.Write([]byte(`{"ok":false,"error":"synthetic_failure"}`))
						} else {
							_, _ = w.Write([]byte(`{"ok":true,"messages":[]}`))
						}
					}))
					defer server.Close()
					if mode == "migration" {
						// v7 and v8 have identical DDL; this is a synthetic pre-upgrade checkpoint.
						_, err = st.DB().Exec(`pragma user_version = 7`)
						require.NoError(t, err)
						require.NoError(t, st.Close())
						st, err = store.Open(dbPath)
						require.NoError(t, err)
					} else {
						opts := share.Options{RepoPath: filepath.Join(t.TempDir(), "snapshot")}
						_, err := share.Export(ctx, st, opts)
						require.NoError(t, err)
						_, err = share.Restore(ctx, st, opts)
						require.NoError(t, err)
					}
					require.Zero(t, requests, "opening or restoring must not contact the provider")
					coverage, err := loadHistoryCoverage(ctx, st, sourceName, "T123", "C123", "")
					require.NoError(t, err)
					require.Equal(t, historyCoverage{}, coverage)
					client := NewWithOptions(config.Tokens{Bot: "fixture", User: "fixture"}, server.URL+"/", server.Client())
					_, plan, err := client.channelSyncPlan(ctx, st, "T123", []slack.Channel{channel}, SyncOptions{}, sourceName)
					require.NoError(t, err)
					require.Equal(t, floor, plan["C123"], "newest saved message is not coverage")
					historyClient := client.bot
					if sourceName == SourceUser {
						historyClient = client.user
					}
					source := channelSyncSource{historyClient: historyClient, token: "fixture", sourceName: sourceName, sourceRank: 2}
					err = client.syncChannelMessagesWithSource(ctx, st, "T123", channel, plan["C123"], false, now, false, source)
					require.ErrorContains(t, err, "synthetic_failure")
					require.NoError(t, st.Close())
					st, err = store.Open(dbPath)
					require.NoError(t, err)
					coverage, err = loadHistoryCoverage(ctx, st, sourceName, "T123", "C123", "")
					require.NoError(t, err)
					require.False(t, coverage.Complete)
					require.NotNil(t, coverage.Pending)
					require.Equal(t, floor, *coverage.Pending)
					fail = false
					require.NoError(t, client.syncChannelMessagesWithSource(ctx, st, "T123", channel, floor, false, now, false, source))
					require.NoError(t, st.Close())
					st, err = store.Open(dbPath)
					require.NoError(t, err)
					coverage, err = loadHistoryCoverage(ctx, st, sourceName, "T123", "C123", "")
					require.NoError(t, err)
					require.True(t, coverage.Complete)
					require.Nil(t, coverage.Pending)
					require.Equal(t, "1710000200.000000", coverage.Latest)
					require.Equal(t, 2, requests)
					_, plan, err = client.channelSyncPlan(ctx, st, "T123", []slack.Channel{channel}, SyncOptions{}, sourceName)
					require.NoError(t, err)
					require.Equal(t, (store.ChannelSyncCursor{RetentionFloor: floor}).ApplyRetentionFloor(repairOldest(coverage.Latest, time.Hour)), plan["C123"])
				})
			}
		}
	}
}
