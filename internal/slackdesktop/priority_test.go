package slackdesktop

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw/slacrawl/internal/store"
	"github.com/stretchr/testify/require"
)

func TestReduxPreservesHigherPriorityMessage(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	defer st.Close()
	now := time.Now().UTC()
	require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{ID: "T111", Name: "test", UpdatedAt: now}))
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C111", WorkspaceID: "T111", Name: "test", UpdatedAt: now}))
	api := store.Message{
		WorkspaceID: "T111", ChannelID: "C111", TS: "1710000000.000001",
		Text: "authoritative <@U111>", NormalizedText: "authoritative", DeletedTS: "1710000001.000001",
		SourceRank: 1, SourceName: "api-user", RawJSON: `{"source":"api"}`, UpdatedAt: now,
	}
	require.NoError(t, st.UpsertMessage(ctx, api, reduxMentions(api.Text)))
	require.NoError(t, ingestReduxStates(ctx, st, []ReduxDecodedState{{
		WorkspaceID: "T111",
		Channels:    []ReduxChannel{{ID: "C111", Name: "test", ContextTeamID: "T111"}},
		Messages: []ReduxMessage{
			{Channel: "C111", TS: api.TS, Type: "message", Text: "stale <@U222>"},
			{Channel: "C111", TS: "1710000002.000001", Type: "message", Text: "desktoponly"},
		},
	}}, now.Add(time.Hour), ingestFilter{}))
	var text, normalized, deleted, source, raw string
	var rank int
	require.NoError(t, st.DB().QueryRowContext(ctx, `select text, normalized_text, deleted_ts, source_rank, source_name, raw_json from messages where channel_id = ? and ts = ?`, api.ChannelID, api.TS).Scan(&text, &normalized, &deleted, &rank, &source, &raw))
	require.Equal(t, api.Text, text)
	require.Equal(t, api.NormalizedText, normalized)
	require.Equal(t, api.DeletedTS, deleted)
	require.Equal(t, api.SourceRank, rank)
	require.Equal(t, api.SourceName, source)
	require.JSONEq(t, api.RawJSON, raw)
	mentions, err := st.Mentions(ctx, "T111", "U222", 10)
	require.NoError(t, err)
	require.Empty(t, mentions)
	rows, err := st.SearchMessages(ctx, store.SearchOptions{Query: "stale", Mode: store.SearchModeRawFTS, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, rows)
	rows, err = st.SearchMessages(ctx, store.SearchOptions{Query: "desktoponly", Mode: store.SearchModeRawFTS, Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	api.Text = "updated by API"
	api.NormalizedText = api.Text
	require.NoError(t, st.UpsertMessage(ctx, api, nil))
	require.NoError(t, st.DB().QueryRowContext(ctx, `select text from messages where channel_id = ? and ts = ?`, api.ChannelID, api.TS).Scan(&text))
	require.Equal(t, api.Text, text)
}
