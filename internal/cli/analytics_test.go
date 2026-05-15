package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/vincentkoc/slacrawl/internal/store"
)

func TestAnalyticsHelpCommand(t *testing.T) {
	for _, args := range [][]string{
		{"analytics", "--help"},
		{"analytics", "-h"},
		{"analytics", "help"},
	} {
		t.Run(args[1], func(t *testing.T) {
			stdout := &bytes.Buffer{}
			app := &App{Stdout: stdout, Stderr: stdout}

			require.NoError(t, app.Run(context.Background(), args))
			require.Contains(t, stdout.String(), "Usage: slacrawl analytics <subcommand> [flags]")
			require.Contains(t, stdout.String(), "digest")
			require.Contains(t, stdout.String(), "quiet")
			require.Contains(t, stdout.String(), "trends")
		})
	}
}

func TestAnalyticsDigestCommand(t *testing.T) {
	ctx := context.Background()
	app, configPath, dbPath, stdout := setupAnalyticsApp(t)

	now := time.Now().UTC()
	seedCommonWorkspace(t, ctx, dbPath, now)

	st, err := store.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessage(ctx, store.Message{
		ChannelID:      "C1",
		TS:             fmt.Sprintf("%d.%06d", now.Add(-2*time.Hour).Unix(), 1),
		WorkspaceID:    "T1",
		UserID:         "U1",
		Text:           "hello",
		NormalizedText: "hello",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      now,
	}, nil))
	require.NoError(t, st.Close())

	stdout.Reset()
	require.NoError(t, app.Run(ctx, []string{"--config", configPath, "--json", "analytics", "digest", "--since", "7d", "--workspace", "T1"}))
	var analyticsDigest map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &analyticsDigest))

	stdout.Reset()
	require.NoError(t, app.Run(ctx, []string{"--config", configPath, "--json", "digest", "--since", "7d", "--workspace", "T1"}))
	var digest map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &digest))

	require.Equal(t, digest["window_label"], analyticsDigest["window_label"])
	require.Equal(t, digest["top_n"], analyticsDigest["top_n"])
	require.Equal(t, digest["totals"], analyticsDigest["totals"])
	require.Equal(t, digest["channels"], analyticsDigest["channels"])
}

func TestAnalyticsQuietCommand(t *testing.T) {
	ctx := context.Background()
	app, configPath, dbPath, stdout := setupAnalyticsApp(t)

	now := time.Now().UTC()
	seedCommonWorkspace(t, ctx, dbPath, now)

	st, err := store.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C2", WorkspaceID: "T1", Name: "stale", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C3", WorkspaceID: "T1", Name: "recent", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertMessage(ctx, store.Message{
		ChannelID:      "C2",
		TS:             fmt.Sprintf("%d.%06d", now.Add(-45*24*time.Hour).Unix(), 2),
		WorkspaceID:    "T1",
		UserID:         "U1",
		Text:           "old",
		NormalizedText: "old",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      now,
	}, nil))
	require.NoError(t, st.UpsertMessage(ctx, store.Message{
		ChannelID:      "C3",
		TS:             fmt.Sprintf("%d.%06d", now.Add(-24*time.Hour).Unix(), 3),
		WorkspaceID:    "T1",
		UserID:         "U1",
		Text:           "new",
		NormalizedText: "new",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      now,
	}, nil))
	require.NoError(t, st.Close())

	stdout.Reset()
	require.NoError(t, app.Run(ctx, []string{"--config", configPath, "analytics", "quiet", "--workspace", "T1", "--since", "30d", "--format", "json"}))
	var quiet map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &quiet))

	rows := quiet["channels"].([]any)
	require.Len(t, rows, 2)
	ids := map[string]bool{}
	for _, item := range rows {
		row := item.(map[string]any)
		ids[row["channel_id"].(string)] = true
	}
	require.True(t, ids["C1"])
	require.True(t, ids["C2"])
	require.False(t, ids["C3"])
}

func TestAnalyticsTrendsCommand(t *testing.T) {
	ctx := context.Background()
	app, configPath, dbPath, stdout := setupAnalyticsApp(t)

	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	app.now = func() time.Time { return now }
	currentWeekStart := startOfWeekForTest(now)
	seedCommonWorkspace(t, ctx, dbPath, now)

	st, err := store.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C2", WorkspaceID: "T1", Name: "beta", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now}))

	seed := func(channelID string, weekStart time.Time, count int) {
		for i := 0; i < count; i++ {
			ts := fmt.Sprintf("%d.%06d", weekStart.Add(time.Duration(i+1)*time.Hour).Unix(), i+1)
			require.NoError(t, st.UpsertMessage(ctx, store.Message{
				ChannelID:      channelID,
				TS:             ts,
				WorkspaceID:    "T1",
				UserID:         "U1",
				Text:           "msg",
				NormalizedText: "msg",
				SourceRank:     2,
				SourceName:     "api-bot",
				RawJSON:        "{}",
				UpdatedAt:      now,
			}, nil))
		}
	}

	seed("C1", currentWeekStart.AddDate(0, 0, -14), 2)
	seed("C1", currentWeekStart.AddDate(0, 0, -7), 1)
	seed("C1", currentWeekStart, 1)
	seed("C2", currentWeekStart.AddDate(0, 0, -7), 2)
	seed("C2", currentWeekStart, 2)
	require.NoError(t, st.Close())

	stdout.Reset()
	require.NoError(t, app.Run(ctx, []string{"--config", configPath, "analytics", "trends", "--workspace", "T1", "--weeks", "3", "--format", "json"}))
	var trends map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &trends))

	rows := trends["rows"].([]any)
	require.Len(t, rows, 2)

	alpha := rows[0].(map[string]any)
	require.Equal(t, "alpha", alpha["channel_name"])
	alphaWeekly := alpha["weekly"].([]any)
	require.Equal(t, float64(2), alphaWeekly[0].(map[string]any)["messages"])

	beta := rows[1].(map[string]any)
	require.Equal(t, "beta", beta["channel_name"])
	betaWeekly := beta["weekly"].([]any)
	require.Equal(t, float64(0), betaWeekly[0].(map[string]any)["messages"])
	require.Equal(t, float64(2), betaWeekly[1].(map[string]any)["messages"])
	require.Equal(t, float64(2), betaWeekly[2].(map[string]any)["messages"])
}

func TestAnalyticsTrendsCommandIncludesRequestedQuietChannel(t *testing.T) {
	ctx := context.Background()
	app, configPath, dbPath, stdout := setupAnalyticsApp(t)

	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	app.now = func() time.Time { return now }
	seedCommonWorkspace(t, ctx, dbPath, now)

	st, err := store.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C2", WorkspaceID: "T1", Name: "quiet", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.Close())

	stdout.Reset()
	require.NoError(t, app.Run(ctx, []string{"--config", configPath, "analytics", "trends", "--workspace", "T1", "--channel", "quiet", "--weeks", "2", "--format", "json"}))
	var trends map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &trends))

	rows := trends["rows"].([]any)
	require.Len(t, rows, 1)
	row := rows[0].(map[string]any)
	require.Equal(t, "quiet", row["channel_name"])
	weekly := row["weekly"].([]any)
	require.Len(t, weekly, 2)
	require.Equal(t, float64(0), weekly[0].(map[string]any)["messages"])
	require.Equal(t, float64(0), weekly[1].(map[string]any)["messages"])
}

func TestAnalyticsCommandsRejectPositionalArgs(t *testing.T) {
	ctx := context.Background()
	app, configPath, _, _ := setupAnalyticsApp(t)

	err := app.Run(ctx, []string{"--config", configPath, "analytics", "quiet", "typo", "--workspace", "T1"})
	require.ErrorContains(t, err, "analytics quiet does not accept positional arguments")
	require.ErrorContains(t, err, "typo")

	err = app.Run(ctx, []string{"--config", configPath, "analytics", "trends", "typo", "--weeks", "3"})
	require.ErrorContains(t, err, "analytics trends does not accept positional arguments")
	require.ErrorContains(t, err, "typo")
}

func setupAnalyticsApp(t *testing.T) (*App, string, string, *bytes.Buffer) {
	t.Helper()
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "config.toml")
	dbPath := filepath.Join(tmp, "slacrawl.db")
	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Stderr: stdout}
	require.NoError(t, app.Run(context.Background(), []string{"--config", configPath, "init", "--db", dbPath}))
	return app, configPath, dbPath, stdout
}

func seedCommonWorkspace(t *testing.T, ctx context.Context, dbPath string, now time.Time) {
	t.Helper()
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{ID: "T1", Name: "team", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C1", WorkspaceID: "T1", Name: "alpha", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertUser(ctx, store.User{ID: "U1", WorkspaceID: "T1", Name: "alice", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.Close())
}

func startOfWeekForTest(t time.Time) time.Time {
	t = t.UTC()
	dayStart := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	offset := (int(dayStart.Weekday()) + 6) % 7
	return dayStart.AddDate(0, 0, -offset)
}
