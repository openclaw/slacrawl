package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPurgeMessagesPreviewsAndDeletesMessageOwnedRows(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	oldTime := time.Date(2025, 12, 1, 12, 0, 0, 0, time.UTC)
	newTime := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	uniqueMedia := "files/aa/unique.txt"
	sharedMedia := "files/bb/shared.txt"

	upsertPurgeTestMessage(t, st, "T1", "C1", oldTime, "old unique", "F1", uniqueMedia, 10)
	upsertPurgeTestMessage(t, st, "T1", "C1", oldTime.Add(time.Second), "old shared", "F2", sharedMedia, 20)
	upsertPurgeTestMessage(t, st, "T1", "C1", newTime, "new shared", "F3", sharedMedia, 20)
	upsertPurgeTestMessage(t, st, "T2", "C2", oldTime, "other workspace", "F4", "files/cc/other.txt", 30)

	_, err = st.DB().ExecContext(ctx, `
insert into embedding_jobs (channel_id, ts, state, created_at)
values (?, ?, 'pending', ?), (?, ?, 'pending', ?)
`, "C1", slackTSFromTime(oldTime), formatDBTime(oldTime), "C1", slackTSFromTime(newTime), formatDBTime(newTime))
	require.NoError(t, err)

	opts := PurgeOptions{Before: cutoff, WorkspaceID: "T1"}
	preview, err := st.PurgeMessages(ctx, opts)
	require.NoError(t, err)
	require.Equal(t, int64(2), preview.Messages)
	require.Equal(t, int64(2), preview.MessageEvents)
	require.Equal(t, int64(2), preview.MessageFiles)
	require.Equal(t, int64(2), preview.Mentions)
	require.Equal(t, int64(1), preview.EmbeddingJobs)
	require.Equal(t, int64(2), preview.FTSEntries)
	require.Equal(t, []PurgeMedia{{Path: uniqueMedia, Size: 10}}, preview.Media)
	requireTableCount(t, st, "messages", 4)

	opts.Delete = true
	executed, err := st.PurgeMessages(ctx, opts)
	require.NoError(t, err)
	require.Equal(t, preview, executed)
	requireTableCount(t, st, "messages", 2)
	requireTableCount(t, st, "message_events", 2)
	requireTableCount(t, st, "message_files", 2)
	requireTableCount(t, st, "message_mentions", 2)
	requireTableCount(t, st, "embedding_jobs", 1)
	requireTableCount(t, st, "message_fts", 2)

	rows, err := st.QueryReadOnly(ctx, `select text from messages order by text`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{
		{"text": "new shared"},
		{"text": "other workspace"},
	}, rows)

	empty, err := st.PurgeMessages(ctx, opts)
	require.NoError(t, err)
	require.Zero(t, empty.Messages)
	require.Empty(t, empty.Media)
}

func TestPurgeMessagesIncludesDesktopDraftTimestamps(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	oldTime := time.Date(2025, 12, 1, 12, 0, 0, 0, time.UTC)
	newTime := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	messages := []Message{
		{ChannelID: "C1", TS: "draft:" + slackTSFromTime(oldTime) + ":D1", WorkspaceID: "T1", Text: "old timestamped draft", NormalizedText: "old timestamped draft", SourceRank: 3, SourceName: "desktop-draft", RawJSON: "{}", UpdatedAt: newTime},
		{ChannelID: "C1", TS: "draft:D2", WorkspaceID: "T1", Text: "old untimed draft", NormalizedText: "old untimed draft", SourceRank: 3, SourceName: "desktop-draft", RawJSON: "{}", UpdatedAt: oldTime},
		{ChannelID: "C1", TS: "draft:" + slackTSFromTime(newTime) + ":D3", WorkspaceID: "T1", Text: "new draft", NormalizedText: "new draft", SourceRank: 3, SourceName: "desktop-draft", RawJSON: "{}", UpdatedAt: newTime},
		{ChannelID: "C1", TS: "draft:" + slackTSFromTime(newTime) + ":D4", ThreadTS: slackTSFromTime(oldTime), WorkspaceID: "T1", Text: "new thread draft", NormalizedText: "new thread draft", SourceRank: 3, SourceName: "desktop-draft", RawJSON: "{}", UpdatedAt: newTime},
	}
	for _, message := range messages {
		require.NoError(t, st.UpsertMessage(ctx, message, nil))
	}

	report, err := st.PurgeMessages(ctx, PurgeOptions{Before: cutoff, Delete: true})
	require.NoError(t, err)
	require.Equal(t, int64(2), report.Messages)
	rows, err := st.QueryReadOnly(ctx, "select text from messages order by text")
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"text": "new draft"}, {"text": "new thread draft"}}, rows)
	seeded, err := st.ChannelRetentionSeeded(ctx, "T1", "C1")
	require.NoError(t, err)
	require.False(t, seeded)
}

func TestPurgeMessagesPreservesNewerUntimedDraftWithinCutoffSecond(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	second := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, message := range []Message{
		{ChannelID: "C1", TS: "draft:old", WorkspaceID: "T1", Text: "old", NormalizedText: "old", SourceRank: 3, SourceName: "desktop-draft", RawJSON: "{}", UpdatedAt: second.Add(250 * time.Microsecond)},
		{ChannelID: "C1", TS: "draft:new", WorkspaceID: "T1", Text: "new", NormalizedText: "new", SourceRank: 3, SourceName: "desktop-draft", RawJSON: "{}", UpdatedAt: second.Add(750 * time.Microsecond)},
	} {
		require.NoError(t, st.UpsertMessage(ctx, message, nil))
	}
	_, err = st.DB().ExecContext(ctx, `
update messages
set updated_at = case ts
  when 'draft:old' then ?
  when 'draft:new' then ?
end
where ts in ('draft:old', 'draft:new')
`, second.Add(250*time.Microsecond).Format(time.RFC3339Nano), second.Add(750*time.Microsecond).Format(time.RFC3339Nano))
	require.NoError(t, err)

	report, err := st.PurgeMessages(ctx, PurgeOptions{Before: second.Add(500 * time.Microsecond), Delete: true})
	require.NoError(t, err)
	require.Equal(t, int64(1), report.Messages)
	rows, err := st.QueryReadOnly(ctx, "select text from messages")
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"text": "new"}}, rows)
}

func TestPurgeMessagesConservativelyHandlesSecondPrecisionDraft(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	second := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, st.UpsertMessage(ctx, Message{
		ChannelID: "C1", TS: "draft:untimed", WorkspaceID: "T1",
		Text: "same-second draft", NormalizedText: "same-second draft",
		SourceRank: 3, SourceName: "desktop-draft", RawJSON: "{}", UpdatedAt: second.Add(750 * time.Microsecond),
	}, nil))

	report, err := st.PurgeMessages(ctx, PurgeOptions{Before: second.Add(500 * time.Microsecond), Delete: true})
	require.NoError(t, err)
	require.Zero(t, report.Messages)
	requireTableCount(t, st, "messages", 1)
}

func TestPurgeMessagesFallsBackForLegacyTruncatedDraftTimestamp(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	recent := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, st.UpsertMessage(ctx, Message{
		ChannelID: "C1", TS: "draft:17672256:D1", WorkspaceID: "T1",
		Text: "legacy exact-second draft", NormalizedText: "legacy exact-second draft",
		SourceRank: 3, SourceName: "desktop-draft", RawJSON: "{}", UpdatedAt: recent,
	}, nil))

	report, err := st.PurgeMessages(ctx, PurgeOptions{
		Before: recent.Add(-24 * time.Hour),
		Delete: true,
	})
	require.NoError(t, err)
	require.Zero(t, report.Messages)
	requireTableCount(t, st, "messages", 1)
}

func TestPurgeMessagesHandlesPreUnixNanoCutoff(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, st.UpsertMessage(ctx, Message{
		ChannelID: "C1", TS: slackTSFromTime(now), WorkspaceID: "T1",
		Text: "keep", NormalizedText: "keep", SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: now,
	}, nil))

	report, err := st.PurgeMessages(ctx, PurgeOptions{Before: time.Date(1500, 1, 1, 0, 0, 0, 0, time.UTC), Delete: true})
	require.NoError(t, err)
	require.Zero(t, report.Messages)
	requireTableCount(t, st, "messages", 1)
}

func TestPurgeMessagesPreservesNanosecondCutoffSemantics(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	second := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{second, second.Add(time.Microsecond)} {
		require.NoError(t, st.UpsertMessage(ctx, Message{
			ChannelID: "C1", TS: slackTSFromTime(at), WorkspaceID: "T1",
			Text: at.Format(time.RFC3339Nano), NormalizedText: at.Format(time.RFC3339Nano),
			SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: at,
		}, nil))
	}

	report, err := st.PurgeMessages(ctx, PurgeOptions{Before: second.Add(time.Nanosecond), Delete: true})
	require.NoError(t, err)
	require.Equal(t, int64(1), report.Messages)
	rows, err := st.QueryReadOnly(ctx, "select ts from messages")
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"ts": slackTSFromTime(second.Add(time.Microsecond))}}, rows)
	floor, err := st.ChannelRetentionFloor(ctx, "T1", "C1")
	require.NoError(t, err)
	require.Equal(t, "1767225600.000001", floor)
}

func TestPurgeMessagesSetsScopedMonotonicRetentionFloors(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	for _, channel := range []Channel{
		{ID: "C1", WorkspaceID: "T1", Name: "general", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now},
		{ID: "C2", WorkspaceID: "T1", Name: "empty", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now},
		{ID: "C3", WorkspaceID: "T2", Name: "other", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now},
	} {
		require.NoError(t, st.UpsertChannel(ctx, channel))
	}
	upsertPurgeTestMessage(t, st, "T1", "C1", now.Add(-180*24*time.Hour), "old", "F1", "", 0)

	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 1, time.UTC)
	_, err = st.PurgeMessages(ctx, PurgeOptions{Before: cutoff, WorkspaceID: "T1"})
	require.NoError(t, err)
	require.Empty(t, purgeTestCursor(t, st, "T1", "C1").RetentionFloor)

	_, err = st.PurgeMessages(ctx, PurgeOptions{Before: cutoff, WorkspaceID: "T1", Delete: true})
	require.NoError(t, err)
	wantFloor := "1767225600.000001"
	require.Equal(t, wantFloor, purgeTestCursor(t, st, "T1", "C1").RetentionFloor)
	require.Equal(t, wantFloor, purgeTestCursor(t, st, "T1", "C2").RetentionFloor)
	require.Empty(t, purgeTestCursor(t, st, "T2", "C3").RetentionFloor)
	require.Equal(t, wantFloor, purgeTestCursor(t, st, "T1", "C1").ApplyRetentionFloor(""))
	require.Equal(t, wantFloor, purgeTestCursor(t, st, "T1", "C1").ApplyRetentionFloor("1767222000.000000"))
	require.NoError(t, st.UpsertChannel(ctx, Channel{
		ID: "C4", WorkspaceID: "T1", Name: "discovered-later", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now,
	}))
	require.Equal(t, wantFloor, purgeTestCursor(t, st, "T1", "C4").RetentionFloor)

	_, err = st.PurgeMessages(ctx, PurgeOptions{
		Before:      cutoff.Add(-24 * time.Hour),
		WorkspaceID: "T1",
		Delete:      true,
	})
	require.NoError(t, err)
	require.Equal(t, wantFloor, purgeTestCursor(t, st, "T1", "C1").RetentionFloor)

	later := cutoff.Add(24 * time.Hour)
	_, err = st.PurgeMessages(ctx, PurgeOptions{Before: later, WorkspaceID: "T1", Delete: true})
	require.NoError(t, err)
	require.Equal(t, formatRetentionFloor(later), purgeTestCursor(t, st, "T1", "C1").RetentionFloor)
}

func TestPurgeMessagesDoesNotChangeLastSyncAt(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	lastSync := time.Date(2025, 12, 1, 12, 0, 0, 0, time.UTC)
	_, err = st.DB().ExecContext(ctx, `
insert into sync_state (source_name, entity_type, entity_id, value, updated_at)
values ('api-bot', 'messages', 'C1', 'done', ?)
`, formatDBTime(lastSync))
	require.NoError(t, err)

	before, err := st.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, lastSync, before.LastSyncAt)

	_, err = st.PurgeMessages(ctx, PurgeOptions{
		Before:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		WorkspaceID: "T1",
		Delete:      true,
	})
	require.NoError(t, err)

	after, err := st.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, lastSync, after.LastSyncAt)
}

func TestChannelSyncCursorApplyRetentionFloor(t *testing.T) {
	cursor := ChannelSyncCursor{RetentionFloor: "1767225600.000000"}
	require.Equal(t, "1767225600.000000", cursor.ApplyRetentionFloor(""))
	require.Equal(t, "1767225600.000000", cursor.ApplyRetentionFloor("1767222000.000000"))
	require.Equal(t, "1767229200.000000", cursor.ApplyRetentionFloor("1767229200.000000"))
	require.Equal(t, "1767225600.000000", cursor.ApplyRetentionFloor("invalid"))
	require.Equal(t, "oldest", (ChannelSyncCursor{}).ApplyRetentionFloor("oldest"))
}

func TestShouldEnforceRetention(t *testing.T) {
	floor := "1767225600.500000"
	require.True(t, ShouldEnforceRetention("1767229200.000000", floor, true))
	require.True(t, ShouldEnforceRetention("2026-01-01T00:00:00.500Z", floor, true))
	require.False(t, ShouldEnforceRetention("1767222000.000000", floor, true))
	require.False(t, ShouldEnforceRetention("", floor, true))
	require.True(t, ShouldEnforceRetention("invalid", floor, true))
	require.True(t, ShouldEnforceRetention("1767222000.000000", floor, false))
}

func TestPurgeMessagesUsesThreadRootForRetention(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	rootTime := time.Date(2025, 12, 1, 12, 0, 0, 0, time.UTC)
	replyTime := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rootTS := slackTSFromTime(rootTime)
	require.NoError(t, st.UpsertChannel(ctx, Channel{
		ID: "C1", WorkspaceID: "T1", Name: "general", Kind: "public_channel", RawJSON: "{}", UpdatedAt: replyTime,
	}))
	require.NoError(t, st.UpsertMessage(ctx, Message{
		WorkspaceID: "T1", ChannelID: "C1", TS: rootTS,
		Text: "old root", NormalizedText: "old root", ReplyCount: 1, LatestReply: slackTSFromTime(replyTime),
		SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: rootTime,
	}, nil))
	require.NoError(t, st.UpsertMessage(ctx, Message{
		WorkspaceID: "T1", ChannelID: "C1", TS: slackTSFromTime(replyTime), ThreadTS: rootTS,
		Text: "new reply", NormalizedText: "new reply",
		SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: replyTime,
	}, nil))

	report, err := st.PurgeMessages(ctx, PurgeOptions{Before: cutoff, WorkspaceID: "T1", Delete: true})
	require.NoError(t, err)
	require.Equal(t, int64(2), report.Messages)
	requireTableCount(t, st, "messages", 0)
}

func TestPurgeMessagesSetsFloorForMessageOnlyChannel(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	oldTime := time.Date(2025, 12, 1, 12, 0, 0, 0, time.UTC)
	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, st.UpsertMessage(ctx, Message{
		WorkspaceID: "T1", ChannelID: "C-without-metadata", TS: slackTSFromTime(oldTime),
		Text: "old", NormalizedText: "old", SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: oldTime,
	}, nil))

	_, err = st.PurgeMessages(ctx, PurgeOptions{Before: cutoff, WorkspaceID: "T1", Delete: true})
	require.NoError(t, err)
	floor, err := st.ChannelRetentionFloor(ctx, "T1", "C-without-metadata")
	require.NoError(t, err)
	require.Equal(t, formatRetentionFloor(cutoff), floor)
	seeded, err := st.ChannelRetentionSeeded(ctx, "T1", "C-without-metadata")
	require.NoError(t, err)
	require.True(t, seeded)
	requireTableCount(t, st, "messages", 0)
}

func TestGlobalPurgeFloorAppliesToFutureWorkspaces(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = st.PurgeMessages(ctx, PurgeOptions{Before: cutoff, Delete: true})
	require.NoError(t, err)
	require.NoError(t, st.UpsertChannel(ctx, Channel{
		ID: "C-new", WorkspaceID: "T-new", Name: "new", Kind: "public_channel", RawJSON: "{}", UpdatedAt: cutoff,
	}))
	require.Equal(t, formatRetentionFloor(cutoff), purgeTestCursor(t, st, "T-new", "C-new").RetentionFloor)
}

func TestWithUnreferencedPurgeMediaRechecksReferences(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, st.UpsertMessage(ctx, Message{
		WorkspaceID: "T1", ChannelID: "C1", TS: slackTSFromTime(now),
		Text: "referenced", NormalizedText: "referenced", SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: now,
		Files: []MessageFile{{
			FileID: "F1", MediaPath: "files/aa/referenced.txt", RawJSON: "{}", UpdatedAt: now,
		}},
	}, nil))

	var processed []PurgeMedia
	retained, err := st.WithUnreferencedPurgeMedia(ctx, []PurgeMedia{
		{Path: "files/aa/referenced.txt"},
		{Path: "files/bb/unreferenced.txt"},
	}, func(items []PurgeMedia) error {
		processed = append(processed, items...)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), retained)
	require.Equal(t, []PurgeMedia{{Path: "files/bb/unreferenced.txt"}}, processed)
}

func TestUnreferencedPurgeMediaUsesReadSnapshot(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, st.UpsertMessage(ctx, Message{
		WorkspaceID: "T1", ChannelID: "C1", TS: slackTSFromTime(now),
		Text: "referenced", NormalizedText: "referenced", SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: now,
		Files: []MessageFile{{
			FileID: "F1", MediaPath: "files/aa/referenced.txt", RawJSON: "{}", UpdatedAt: now,
		}},
	}, nil))
	require.NoError(t, st.UpsertMessage(ctx, Message{
		WorkspaceID: "T1", ChannelID: "C1", TS: slackTSFromTime(now.Add(time.Microsecond)),
		Text: "case variant", NormalizedText: "case variant", SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: now,
		Files: []MessageFile{{
			FileID: "F2", MediaPath: "files/aa/REFERENCED.txt", RawJSON: "{}", UpdatedAt: now,
		}},
	}, nil))

	unreferenced, retained, err := st.UnreferencedPurgeMediaAfter(ctx, []PurgeMedia{
		{Path: "files/aa/referenced.txt"},
		{Path: "files/bb/unreferenced.txt"},
	}, []PurgeMedia{{Path: "files/aa/referenced.txt"}})
	require.NoError(t, err)
	require.Equal(t, int64(1), retained)
	require.Equal(t, []PurgeMedia{{Path: "files/bb/unreferenced.txt"}}, unreferenced)
}

func TestRetentionAwareUpsertIsAtomicAndAllowsExistingRows(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = st.PurgeMessages(ctx, PurgeOptions{Before: cutoff, WorkspaceID: "T1", Delete: true})
	require.NoError(t, err)
	oldTime := cutoff.Add(-24 * time.Hour)
	old := Message{
		WorkspaceID: "T1", ChannelID: "C1", TS: slackTSFromTime(oldTime),
		Text: "old", NormalizedText: "old", SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: oldTime,
	}
	written, err := st.UpsertMessageWithRetention(ctx, old, nil)
	require.NoError(t, err)
	require.False(t, written)
	requireTableCount(t, st, "messages", 0)

	require.NoError(t, st.UpsertMessage(ctx, old, nil))
	old.Text = "edited restored old"
	old.NormalizedText = old.Text
	written, err = st.UpsertMessageWithRetention(ctx, old, nil)
	require.NoError(t, err)
	require.True(t, written)
	rows, err := st.QueryReadOnly(ctx, `select text from messages`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"text": "edited restored old"}}, rows)

	reply := Message{
		WorkspaceID: "T1", ChannelID: "C1", TS: slackTSFromTime(cutoff.Add(time.Hour)), ThreadTS: old.TS,
		Text: "new reply", NormalizedText: "new reply", SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: cutoff.Add(time.Hour),
	}
	written, err = st.UpsertMessageWithRetention(ctx, reply, nil)
	require.NoError(t, err)
	require.False(t, written)
	requireTableCount(t, st, "messages", 1)
}

func upsertPurgeTestMessage(t *testing.T, st *Store, workspaceID, channelID string, at time.Time, text, fileID, mediaPath string, mediaSize int64) {
	t.Helper()
	require.NoError(t, st.UpsertMessage(context.Background(), Message{
		WorkspaceID:    workspaceID,
		ChannelID:      channelID,
		TS:             slackTSFromTime(at),
		UserID:         "U1",
		Text:           text,
		NormalizedText: text,
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      at,
		Files: []MessageFile{{
			FileID:        fileID,
			Name:          fileID + ".txt",
			MediaPath:     mediaPath,
			ContentSize:   mediaSize,
			FetchStatus:   "fetched",
			RawJSON:       "{}",
			UpdatedAt:     at,
			ContentSHA256: fileID,
		}},
	}, []Mention{{Type: "user", TargetID: "U1", DisplayText: "alice"}}))
}

func purgeTestCursor(t *testing.T, st *Store, workspaceID, channelID string) ChannelSyncCursor {
	t.Helper()
	cursors, err := st.ChannelSyncCursors(context.Background(), workspaceID)
	require.NoError(t, err)
	for _, cursor := range cursors {
		if cursor.ID == channelID {
			return cursor
		}
	}
	t.Fatalf("missing cursor for %s/%s", workspaceID, channelID)
	return ChannelSyncCursor{}
}

func requireTableCount(t *testing.T, st *Store, table string, want int64) {
	t.Helper()
	rows, err := st.QueryReadOnly(context.Background(), "select count(*) as n from "+table)
	require.NoError(t, err)
	require.Equal(t, want, rows[0]["n"])
}
