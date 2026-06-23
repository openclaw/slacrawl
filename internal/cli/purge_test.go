package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/media"
	"github.com/openclaw/slacrawl/internal/store"
)

func TestResolvePurgeCutoff(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	cutoff, err := resolvePurgeCutoff(now, "", "90d")
	require.NoError(t, err)
	require.Equal(t, now.Add(-90*24*time.Hour), cutoff)

	cutoff, err = resolvePurgeCutoff(now, "2026-01-15", "")
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC), cutoff)

	_, err = resolvePurgeCutoff(now, "", "0d")
	require.ErrorContains(t, err, "greater than zero")
	_, err = resolvePurgeCutoff(now, "not-a-date", "")
	require.ErrorContains(t, err, "RFC3339 or YYYY-MM-DD")
}

func TestPurgeCommandPreviewsThenDeletes(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	dbPath := filepath.Join(dir, "slacrawl.db")
	cacheDir := filepath.Join(dir, "cache")
	cfg := config.Default()
	cfg.DBPath = dbPath
	cfg.CacheDir = cacheDir
	require.NoError(t, cfg.Save(configPath))

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	oldTime := now.Add(-120 * 24 * time.Hour)
	newTime := now.Add(-10 * 24 * time.Hour)
	mediaPath := "files/aa/old.txt"
	target, err := media.LocalPath(cacheDir, mediaPath)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(target, []byte("old media"), 0o600))
	retainedMediaPath := "files/bb/new.txt"
	retainedTarget, err := media.LocalPath(cacheDir, retainedMediaPath)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(retainedTarget), 0o755))
	require.NoError(t, os.WriteFile(retainedTarget, []byte("new media"), 0o600))

	st, err := store.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessage(context.Background(), store.Message{
		WorkspaceID: "T1", ChannelID: "C1", TS: purgeTestSlackTS(oldTime),
		Text: "old", NormalizedText: "old", SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: oldTime,
		Files: []store.MessageFile{{
			FileID: "F1", Name: "old.txt", MediaPath: mediaPath, ContentSize: int64(len("old media")),
			FetchStatus: "fetched", RawJSON: "{}", UpdatedAt: oldTime,
		}},
	}, nil))
	require.NoError(t, st.UpsertMessage(context.Background(), store.Message{
		WorkspaceID: "T1", ChannelID: "C1", TS: purgeTestSlackTS(newTime),
		Text: "new", NormalizedText: "new", SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: newTime,
		Files: []store.MessageFile{{
			FileID: "F2", Name: "new.txt", MediaPath: retainedMediaPath, ContentSize: int64(len("new media")),
			FetchStatus: "fetched", RawJSON: "{}", UpdatedAt: newTime,
		}},
	}, nil))
	require.NoError(t, st.Close())

	var stdout bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stdout, now: func() time.Time { return now }}
	args := []string{"--config", configPath, "--json", "purge", "--older-than", "90d", "--workspace", "T1"}
	require.NoError(t, app.Run(context.Background(), args))
	var preview purgeResponse
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &preview))
	require.True(t, preview.DryRun)
	require.Equal(t, int64(1), preview.Messages)
	require.Equal(t, int64(1), preview.CachedMediaFiles)
	require.Equal(t, int64(1), preview.CachedMediaRetained)
	require.FileExists(t, target)
	require.FileExists(t, retainedTarget)
	require.Equal(t, int64(2), purgeTestMessageCount(t, dbPath))

	stdout.Reset()
	args = append(args, "--force", "--vacuum")
	require.NoError(t, app.Run(context.Background(), args))
	var executed purgeResponse
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &executed))
	require.False(t, executed.DryRun)
	require.True(t, executed.Vacuumed)
	require.Equal(t, int64(1), executed.CachedMediaDeleted)
	require.Equal(t, int64(1), executed.CachedMediaRetained)
	require.NoFileExists(t, target)
	require.FileExists(t, retainedTarget)
	require.Equal(t, int64(1), purgeTestMessageCount(t, dbPath))
}

func TestPurgeCommandValidatesSafetyFlags(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, now: func() time.Time { return now }}

	err := app.Run(context.Background(), []string{"purge"})
	require.ErrorContains(t, err, "exactly one")
	err = app.Run(context.Background(), []string{"purge", "--before", "2026-01-01", "--older-than", "90d"})
	require.ErrorContains(t, err, "exactly one")
	err = app.Run(context.Background(), []string{"purge", "--before", "2026-01-01", "--vacuum"})
	require.ErrorContains(t, err, "--vacuum requires --force")
	err = app.Run(context.Background(), []string{"purge", "--before", "2026-06-11"})
	require.ErrorContains(t, err, "future")
	err = app.Run(context.Background(), []string{"purge", "--before", "2026-01-01", "--workspace", " "})
	require.ErrorContains(t, err, "--workspace cannot be empty")
	err = app.Run(context.Background(), []string{"purge", "--before", "2026-01-01", "--workspace", "T1", "--all-workspaces"})
	require.ErrorContains(t, err, "--workspace and --all-workspaces cannot be combined")
}

func TestPurgeCommandKeepMedia(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	dbPath := filepath.Join(dir, "slacrawl.db")
	cacheDir := filepath.Join(dir, "cache")
	cfg := config.Default()
	cfg.DBPath = dbPath
	cfg.CacheDir = cacheDir
	require.NoError(t, cfg.Save(configPath))

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	mediaPath := "files/aa/kept.txt"
	target, err := media.LocalPath(cacheDir, mediaPath)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(target, []byte("keep"), 0o600))

	st, err := store.Open(dbPath)
	require.NoError(t, err)
	oldTime := now.Add(-120 * 24 * time.Hour)
	require.NoError(t, st.UpsertMessage(context.Background(), store.Message{
		WorkspaceID: "T1", ChannelID: "C1", TS: purgeTestSlackTS(oldTime),
		Text: "old", NormalizedText: "old", SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: oldTime,
		Files: []store.MessageFile{{
			FileID: "F1", Name: "kept.txt", MediaPath: mediaPath, ContentSize: 4,
			FetchStatus: "fetched", RawJSON: "{}", UpdatedAt: oldTime,
		}},
	}, nil))
	require.NoError(t, st.Close())

	var stdout bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stdout, now: func() time.Time { return now }}
	require.NoError(t, app.Run(context.Background(), []string{
		"--config", configPath, "--json", "purge", "--older-than", "90d", "--force", "--keep-media",
	}))
	var response purgeResponse
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &response))
	require.True(t, response.KeepMedia)
	require.Zero(t, response.CachedMediaDeleted)
	require.FileExists(t, target)
	require.Zero(t, purgeTestMessageCount(t, dbPath))
}

func TestPurgeCommandUsesConfigWorkspaceByDefault(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	dbPath := filepath.Join(dir, "slacrawl.db")
	cfg := config.Default()
	cfg.DBPath = dbPath
	cfg.WorkspaceID = "T1"
	require.NoError(t, cfg.Save(configPath))

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	oldTime := now.Add(-120 * 24 * time.Hour)
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessage(context.Background(), store.Message{
		WorkspaceID: "T1", ChannelID: "C1", TS: purgeTestSlackTS(oldTime),
		Text: "old t1", NormalizedText: "old t1", SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: oldTime,
	}, nil))
	require.NoError(t, st.UpsertMessage(context.Background(), store.Message{
		WorkspaceID: "T2", ChannelID: "C2", TS: purgeTestSlackTS(oldTime),
		Text: "old t2", NormalizedText: "old t2", SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: oldTime,
	}, nil))
	require.NoError(t, st.Close())

	var stdout bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stdout, now: func() time.Time { return now }}
	require.NoError(t, app.Run(context.Background(), []string{
		"--config", configPath, "--json", "purge", "--older-than", "90d", "--force", "--keep-media",
	}))
	var response purgeResponse
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &response))
	require.Equal(t, "T1", response.WorkspaceID)
	require.Equal(t, int64(1), response.Messages)
	require.Equal(t, map[string]int64{"T2": 1}, purgeTestMessageCountsByWorkspace(t, dbPath))
	require.Equal(t, map[string]string{"T1": purgeTestSlackTS(now.Add(-90 * 24 * time.Hour))}, purgeTestWorkspaceFloors(t, dbPath))

	stdout.Reset()
	require.NoError(t, app.Run(context.Background(), []string{
		"--config", configPath, "--json", "purge", "--older-than", "90d", "--force", "--keep-media", "--all-workspaces",
	}))
	require.Zero(t, purgeTestMessageCount(t, dbPath))
	require.Contains(t, purgeTestWorkspaceFloors(t, dbPath), "*")
}

func TestPurgeCommandRequiresCacheDirBeforeDeletingMedia(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	dbPath := filepath.Join(dir, "slacrawl.db")
	cfg := config.Default()
	cfg.DBPath = dbPath
	cfg.CacheDir = ""
	require.NoError(t, cfg.Save(configPath))

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	oldTime := now.Add(-120 * 24 * time.Hour)
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessage(context.Background(), store.Message{
		WorkspaceID: "T1", ChannelID: "C1", TS: purgeTestSlackTS(oldTime),
		Text: "old", NormalizedText: "old", SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: oldTime,
		Files: []store.MessageFile{{
			FileID: "F1", Name: "old.txt", MediaPath: "files/aa/old.txt",
			FetchStatus: "fetched", RawJSON: "{}", UpdatedAt: oldTime,
		}},
	}, nil))
	require.NoError(t, st.Close())

	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, now: func() time.Time { return now }}
	err = app.Run(context.Background(), []string{
		"--config", configPath, "purge", "--older-than", "90d", "--force",
	})
	require.ErrorContains(t, err, "cache dir is required")
	require.Equal(t, int64(1), purgeTestMessageCount(t, dbPath))
}

func TestPurgeCommandWithoutCacheDirDeletesDatabaseOnlyRows(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	dbPath := filepath.Join(dir, "slacrawl.db")
	cfg := config.Default()
	cfg.DBPath = dbPath
	cfg.CacheDir = ""
	require.NoError(t, cfg.Save(configPath))

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	oldTime := now.Add(-120 * 24 * time.Hour)
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessage(context.Background(), store.Message{
		WorkspaceID: "T1", ChannelID: "C1", TS: purgeTestSlackTS(oldTime),
		Text: "old", NormalizedText: "old", SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: oldTime,
	}, nil))
	require.NoError(t, st.Close())

	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, now: func() time.Time { return now }}
	require.NoError(t, app.Run(context.Background(), []string{
		"--config", configPath, "purge", "--older-than", "90d", "--force",
	}))
	require.Zero(t, purgeTestMessageCount(t, dbPath))
}

func TestPurgeCommandRetriesOrphanedMediaCleanup(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	dbPath := filepath.Join(dir, "slacrawl.db")
	cacheDir := filepath.Join(dir, "cache")
	cfg := config.Default()
	cfg.DBPath = dbPath
	cfg.CacheDir = cacheDir
	require.NoError(t, cfg.Save(configPath))
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, st.Close())

	target, err := media.LocalPath(cacheDir, "files/aa/orphan.txt")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(target, []byte("orphan"), 0o600))

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	var stdout bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stdout, now: func() time.Time { return now }}
	require.NoError(t, app.Run(context.Background(), []string{
		"--config", configPath, "--json", "purge", "--older-than", "90d",
	}))
	var preview purgeResponse
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &preview))
	require.True(t, preview.DryRun)
	require.Equal(t, int64(1), preview.CachedMediaFiles)
	require.FileExists(t, target)

	stdout.Reset()
	require.NoError(t, app.Run(context.Background(), []string{
		"--config", configPath, "--json", "purge", "--older-than", "90d", "--force",
	}))
	var response purgeResponse
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &response))
	require.Zero(t, response.Messages)
	require.Equal(t, int64(1), response.CachedMediaDeleted)
	require.NoFileExists(t, target)
}

func TestPurgeCommandReportsMissingSelectedMedia(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	dbPath := filepath.Join(dir, "slacrawl.db")
	cacheDir := filepath.Join(dir, "cache")
	cfg := config.Default()
	cfg.DBPath = dbPath
	cfg.CacheDir = cacheDir
	require.NoError(t, cfg.Save(configPath))

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	oldTime := now.Add(-120 * 24 * time.Hour)
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessage(context.Background(), store.Message{
		WorkspaceID: "T1", ChannelID: "C1", TS: purgeTestSlackTS(oldTime),
		Text: "old", NormalizedText: "old", SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: oldTime,
		Files: []store.MessageFile{{
			FileID: "F1", Name: "missing.txt", MediaPath: "files/aa/missing.txt", ContentSize: 42,
			FetchStatus: "fetched", RawJSON: "{}", UpdatedAt: oldTime,
		}},
	}, nil))
	require.NoError(t, st.Close())

	var stdout bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stdout, now: func() time.Time { return now }}
	require.NoError(t, app.Run(context.Background(), []string{
		"--config", configPath, "--json", "purge", "--older-than", "90d", "--force",
	}))
	var response purgeResponse
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &response))
	require.Equal(t, int64(1), response.CachedMediaFiles)
	require.Equal(t, int64(42), response.CachedMediaBytes)
	require.Zero(t, response.CachedMediaDeleted)
	require.Equal(t, int64(1), response.CachedMediaMissing)
	require.Zero(t, purgeTestMessageCount(t, dbPath))
}

func TestPurgeCommandReportsPostCommitCleanupFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional privileges on Windows")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	dbPath := filepath.Join(dir, "slacrawl.db")
	cacheDir := filepath.Join(dir, "cache")
	cfg := config.Default()
	cfg.DBPath = dbPath
	cfg.CacheDir = cacheDir
	require.NoError(t, cfg.Save(configPath))
	require.NoError(t, os.MkdirAll(cacheDir, 0o755))
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(cacheDir, "media")))

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	oldTime := now.Add(-120 * 24 * time.Hour)
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessage(context.Background(), store.Message{
		WorkspaceID: "T1", ChannelID: "C1", TS: purgeTestSlackTS(oldTime),
		Text: "old", NormalizedText: "old", SourceRank: 2, SourceName: "api-bot", RawJSON: "{}", UpdatedAt: oldTime,
	}, nil))
	require.NoError(t, st.Close())

	var stdout bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stdout, now: func() time.Time { return now }}
	err = app.Run(context.Background(), []string{
		"--config", configPath, "--json", "purge", "--older-than", "90d", "--force",
	})
	require.ErrorContains(t, err, "database purge committed")
	var response purgeResponse
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &response))
	require.False(t, response.DryRun)
	require.Equal(t, int64(1), response.Messages)
	require.Zero(t, purgeTestMessageCount(t, dbPath))
}

func TestRemovePurgeMediaContinuesAfterFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional privileges on Windows")
	}
	cacheDir := t.TempDir()
	root := filepath.Join(cacheDir, "media")
	outside := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "files", "aa"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "files", "bb"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "bad.txt"), []byte("bad"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "bad.txt"), filepath.Join(root, "files", "aa", "bad.txt")))
	goodPath := filepath.Join(root, "files", "bb", "good.txt")
	require.NoError(t, os.WriteFile(goodPath, []byte("good"), 0o600))

	deleted, missing, failures, completed, errs := removePurgeMedia(cacheDir, []store.PurgeMedia{
		{Path: "files/aa/bad.txt"},
		{Path: "files/bb/good.txt"},
		{Path: "files/cc/missing.txt"},
	})
	require.Equal(t, int64(1), deleted)
	require.Equal(t, int64(1), missing)
	require.Equal(t, []string{"files/aa/bad.txt"}, failures)
	require.Equal(t, []string{"files/bb/good.txt", "files/cc/missing.txt"}, completed)
	require.Len(t, errs, 1)
	require.NoFileExists(t, goodPath)
	require.FileExists(t, filepath.Join(outside, "bad.txt"))
}

func purgeTestMessageCount(t *testing.T, dbPath string) int64 {
	t.Helper()
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	rows, err := st.QueryReadOnly(context.Background(), "select count(*) as n from messages")
	require.NoError(t, err)
	return rows[0]["n"].(int64)
}

func purgeTestMessageCountsByWorkspace(t *testing.T, dbPath string) map[string]int64 {
	t.Helper()
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	rows, err := st.QueryReadOnly(context.Background(), "select workspace_id, count(*) as n from messages group by workspace_id order by workspace_id")
	require.NoError(t, err)
	counts := map[string]int64{}
	for _, row := range rows {
		counts[row["workspace_id"].(string)] = row["n"].(int64)
	}
	return counts
}

func purgeTestWorkspaceFloors(t *testing.T, dbPath string) map[string]string {
	t.Helper()
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	rows, err := st.QueryReadOnly(context.Background(), "select entity_id, value from sync_state where source_name = 'retention' and entity_type = 'workspace_floor' order by entity_id")
	require.NoError(t, err)
	floors := map[string]string{}
	for _, row := range rows {
		floors[row["entity_id"].(string)] = row["value"].(string)
	}
	return floors
}

func purgeTestSlackTS(value time.Time) string {
	return fmt.Sprintf("%d.%06d", value.UTC().Unix(), value.UTC().Nanosecond()/1000)
}
