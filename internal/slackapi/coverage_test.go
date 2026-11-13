package slackapi

import (
	"context"
	"encoding/json"
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
			require.Equal(t, store.APIHistoryOptions{}, plan)
			replies := threadSyncSource{}
			if threadFailure {
				replies = threadSyncSource{token: "test-user", sourceName: SourceUser, sourceRank: 1}
			}
			source := channelSyncSource{token: "test", sourceName: SourceBot, sourceRank: 2}
			err = client.syncChannelMessagesWithSource(ctx, st, "T123", channels[0], plan, now, replies, source)
			require.ErrorContains(t, err, map[bool]string{false: "slack conversations.history API response failed", true: "slack conversations.replies API response failed"}[threadFailure])
			requireNativeErrorCode(t, err, "synthetic_failure")
			state, err := readAPIHistory(ctx, st, SourceBot, "T123", "C123", "")
			require.NoError(t, err)
			require.NotNil(t, state.Pending)
			require.False(t, state.Complete)
			_, plan, err = client.channelSyncPlan(ctx, st, "T123", channels, SyncOptions{})
			require.NoError(t, err)
			require.Equal(t, store.APIHistoryOptions{}, plan)
			fail = false
			require.NoError(t, client.syncChannelMessagesWithSource(ctx, st, "T123", channels[0], plan, now, replies, source))
			require.Equal(t, []string{"", ""}, oldest)
			state, err = readAPIHistory(ctx, st, SourceBot, "T123", "C123", "")
			require.NoError(t, err)
			require.True(t, state.Complete)
			require.Nil(t, state.Pending)
			require.Equal(t, "1710000200.000000", state.Latest)
			_, plan, err = client.channelSyncPlan(ctx, st, "T123", channels, SyncOptions{})
			require.NoError(t, err)
			attempt, err := st.BeginAPIHistory(ctx, store.APIHistoryScope{SourceName: SourceBot, WorkspaceID: "T123", ChannelID: "C123"}, plan, "1710000200.000000")
			require.NoError(t, err)
			require.Equal(t, "1709996600.000000", attempt.Oldest)
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
			require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C123", WorkspaceID: "T123", UpdatedAt: now}))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				require.Equal(t, "1710000200.123456", r.Form.Get("latest"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true,"messages":` + messages + `}`))
			}))
			defer server.Close()
			client := NewWithOptions(config.Tokens{Bot: "test"}, server.URL+"/", server.Client())
			source := channelSyncSource{token: "test", sourceName: SourceBot, sourceRank: 2}
			require.NoError(t, client.syncChannelMessagesWithSource(ctx, st, "T123", channel, store.APIHistoryOptions{}, now, threadSyncSource{}, source))
			coverage, err := readAPIHistory(ctx, st, SourceBot, "T123", "C123", "")
			require.NoError(t, err)
			require.True(t, coverage.Complete)
			require.Nil(t, coverage.Pending)
			require.Equal(t, "1710000200.123456", coverage.Latest)
			_, plan, err := client.channelSyncPlan(ctx, st, "T123", []slack.Channel{channel}, SyncOptions{})
			require.NoError(t, err)
			attempt, err := st.BeginAPIHistory(ctx, store.APIHistoryScope{SourceName: SourceBot, WorkspaceID: "T123", ChannelID: "C123"}, plan, "1710000200.123456")
			require.NoError(t, err)
			require.Equal(t, "1709996600.123456", attempt.Oldest)
		})
	}
}

func TestHistoryCoverageScopeAndExplicitBounds(t *testing.T) {
	ctx := context.Background()
	st := mustStore(t)
	defer st.Close()
	pending := "1700000000.000000"
	for _, owner := range []struct{ workspace, channel string }{{"T123", "C123"}, {"T999", "C999"}, {"Tnew", "CNEW"}} {
		require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: owner.channel, WorkspaceID: owner.workspace}))
	}
	require.NoError(t, seedAPIHistory(ctx, st, SourceBot, "T123", "C123", "", store.APIHistoryState{Complete: true, Latest: "1710000000.000000", Pending: &pending}))
	for _, tc := range []struct {
		source, workspace, channel, since string
		opts                              store.APIHistoryOptions
		want                              string
	}{
		{SourceBot, "T123", "C123", "", store.APIHistoryOptions{}, pending},
		{SourceUser, "T123", "C123", "", store.APIHistoryOptions{}, ""},
		{SourceBot, "T999", "C999", "", store.APIHistoryOptions{}, ""},
		{SourceBot, "T123", "C123", "1690000000.000000", store.APIHistoryOptions{RestoreRequested: true}, "1690000000.000000"},
		{SourceBot, "T123", "C123", "", store.APIHistoryOptions{Full: true, RestoreRequested: true}, ""},
	} {
		attempt, err := st.BeginAPIHistory(ctx, store.APIHistoryScope{SourceName: tc.source, WorkspaceID: tc.workspace, ChannelID: tc.channel, Since: tc.since}, tc.opts, "1710000200.000000")
		require.NoError(t, err)
		require.Equal(t, tc.want, attempt.Oldest)
	}
	require.NoError(t, seedAPIHistory(ctx, st, SourceBot, "Tnew", "CNEW", "1690000000.000000", store.APIHistoryState{Complete: true, Latest: "1710000000.000000"}))
	attempt, err := st.BeginAPIHistory(ctx, store.APIHistoryScope{SourceName: SourceBot, WorkspaceID: "Tnew", ChannelID: "CNEW"}, store.APIHistoryOptions{}, "1710000200.000000")
	require.NoError(t, err)
	require.Empty(t, attempt.Oldest)
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
	require.NoError(t, seedAPIHistory(ctx, st, SourceBot, "T123", "C123", "", store.APIHistoryState{Complete: true, Latest: "1709900000.000000"}))
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
	repairErr := client.repairWorkspace(ctx, st, "T123")
	require.ErrorContains(t, repairErr, "slack conversations.history API response failed")
	requireNativeErrorCode(t, repairErr, "synthetic_failure")
	coverage, err := readAPIHistory(ctx, st, SourceBot, "T123", "C123", "")
	require.NoError(t, err)
	require.NotNil(t, coverage.Pending)
	require.Equal(t, "1709896400.000000", *coverage.Pending)
	fail = false
	require.NoError(t, client.repairWorkspace(ctx, st, "T123"))
	require.Equal(t, []string{"1709896400.000000", "1709896400.000000"}, oldest)
	coverage, err = readAPIHistory(ctx, st, SourceBot, "T123", "C123", "")
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
					require.NoError(t, seedAPIHistory(ctx, st, sourceName, "T123", "C123", "", store.APIHistoryState{Complete: true, Latest: "1710000100.000000"}))
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
					coverage, err := readAPIHistory(ctx, st, sourceName, "T123", "C123", "")
					require.NoError(t, err)
					require.Equal(t, store.APIHistoryState{}, coverage)
					client := NewWithOptions(config.Tokens{Bot: "fixture", User: "fixture"}, server.URL+"/", server.Client())
					_, plan, err := client.channelSyncPlan(ctx, st, "T123", []slack.Channel{channel}, SyncOptions{})
					require.NoError(t, err)
					require.Equal(t, store.APIHistoryOptions{}, plan, "selection does not derive coverage from message maxima")
					source := channelSyncSource{token: "fixture", sourceName: sourceName, sourceRank: 2}
					err = client.syncChannelMessagesWithSource(ctx, st, "T123", channel, plan, now, threadSyncSource{}, source)
					require.ErrorContains(t, err, "slack conversations.history API response failed")
					requireNativeErrorCode(t, err, "synthetic_failure")
					require.NoError(t, st.Close())
					st, err = store.Open(dbPath)
					require.NoError(t, err)
					coverage, err = readAPIHistory(ctx, st, sourceName, "T123", "C123", "")
					require.NoError(t, err)
					require.False(t, coverage.Complete)
					require.NotNil(t, coverage.Pending)
					require.Equal(t, floor, *coverage.Pending)
					fail = false
					require.NoError(t, client.syncChannelMessagesWithSource(ctx, st, "T123", channel, store.APIHistoryOptions{}, now, threadSyncSource{}, source))
					require.NoError(t, st.Close())
					st, err = store.Open(dbPath)
					require.NoError(t, err)
					coverage, err = readAPIHistory(ctx, st, sourceName, "T123", "C123", "")
					require.NoError(t, err)
					require.True(t, coverage.Complete)
					require.Nil(t, coverage.Pending)
					require.Equal(t, "1710000200.000000", coverage.Latest)
					require.Equal(t, 2, requests)
					_, plan, err = client.channelSyncPlan(ctx, st, "T123", []slack.Channel{channel}, SyncOptions{})
					require.NoError(t, err)
					attempt, err := st.BeginAPIHistory(ctx, store.APIHistoryScope{SourceName: sourceName, WorkspaceID: "T123", ChannelID: "C123"}, plan, "1710000200.000000")
					require.NoError(t, err)
					require.Equal(t, (store.ChannelSyncCursor{RetentionFloor: floor}).ApplyRetentionFloor("1709996600.000000"), attempt.Oldest)
				})
			}
		}
	}
}

// Fixtures seed the existing wire format directly; production mutation is owned
// exclusively by BeginAPIHistory and guarded completion.
func seedAPIHistory(ctx context.Context, st *store.Store, source, workspace, channel, since string, state store.APIHistoryState) error {
	key, err := json.Marshal([3]string{workspace, channel, since})
	if err != nil {
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return st.SetSyncState(ctx, source, store.APIHistoryEntityType, string(key), string(raw))
}

func readAPIHistory(ctx context.Context, st *store.Store, source, workspace, channel, since string) (store.APIHistoryState, error) {
	return st.APIHistory(ctx, store.APIHistoryScope{SourceName: source, WorkspaceID: workspace, ChannelID: channel, Since: since})
}
