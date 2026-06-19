package share

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/media"
	"github.com/openclaw/slacrawl/internal/store"
)

func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()

	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	manifest, err := Export(ctx, source, opts)
	require.NoError(t, err)
	require.Equal(t, 1, manifest.Version)
	require.NotEmpty(t, manifest.Tables)
	require.FileExists(t, filepath.Join(opts.RepoPath, ManifestName))

	reader, err := store.Open(filepath.Join(dir, "reader.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()

	imported, err := Import(ctx, reader, opts)
	require.NoError(t, err)
	require.Equal(t, manifest.GeneratedAt.UTC().Format(time.RFC3339Nano), imported.GeneratedAt.UTC().Format(time.RFC3339Nano))

	rows, err := reader.Search(ctx, "", "archive", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "git backed archive works", rows[0].Text)
}

func TestImportRejectsIncompleteManifestBeforeClearing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	manifest, err := Export(ctx, source, opts)
	require.NoError(t, err)
	manifest.Tables = manifest.Tables[:1]
	writeManifest(t, opts.RepoPath, manifest)

	reader := seedStore(t, filepath.Join(dir, "reader.db"))
	defer func() { require.NoError(t, reader.Close()) }()

	_, err = Import(ctx, reader, opts)
	require.ErrorContains(t, err, "manifest missing table")
	assertArchiveStillPresent(t, ctx, reader)
}

func TestImportRejectsTableRowCountMismatch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	manifest, err := Export(ctx, source, opts)
	require.NoError(t, err)
	for i := range manifest.Tables {
		if manifest.Tables[i].Name == "messages" {
			manifest.Tables[i].Rows++
			break
		}
	}
	writeManifest(t, opts.RepoPath, manifest)

	reader := seedStore(t, filepath.Join(dir, "reader.db"))
	defer func() { require.NoError(t, reader.Close()) }()

	_, err = Import(ctx, reader, opts)
	require.ErrorContains(t, err, "row count mismatch")
	assertArchiveStillPresent(t, ctx, reader)
}

func TestImportRejectsEscapedManifestTablePath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	manifest, err := Export(ctx, source, opts)
	require.NoError(t, err)
	for i := range manifest.Tables {
		if manifest.Tables[i].Name == "messages" {
			manifest.Tables[i].Files = []string{"../outside.jsonl.gz"}
			manifest.Tables[i].File = ""
			break
		}
	}
	writeManifest(t, opts.RepoPath, manifest)

	reader := seedStore(t, filepath.Join(dir, "reader.db"))
	defer func() { require.NoError(t, reader.Close()) }()

	_, err = Import(ctx, reader, opts)
	require.ErrorContains(t, err, "path escapes share repo")
	assertArchiveStillPresent(t, ctx, reader)
}

func TestImportRejectsSymlinkedManifestTableDir(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	_, err := Export(ctx, source, opts)
	require.NoError(t, err)

	outside := filepath.Join(dir, "outside-messages")
	require.NoError(t, os.MkdirAll(outside, 0o750))
	tableDir := filepath.Join(opts.RepoPath, "tables", "messages")
	require.NoError(t, os.RemoveAll(tableDir))
	if err := os.Symlink(outside, tableDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	reader := seedStore(t, filepath.Join(dir, "reader.db"))
	defer func() { require.NoError(t, reader.Close()) }()

	_, err = Import(ctx, reader, opts)
	require.ErrorContains(t, err, "path escapes share repo")
	assertArchiveStillPresent(t, ctx, reader)
}

func TestPullPreservesLocalCommitsAheadOfOrigin(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	remoteWork := filepath.Join(dir, "remote-work")
	remoteRepo := filepath.Join(dir, "remote.git")
	shareRepo := filepath.Join(dir, "share")

	require.NoError(t, testGitRun(ctx, "", "init", "-b", "main", remoteWork))
	require.NoError(t, os.WriteFile(filepath.Join(remoteWork, "manifest.json"), []byte("{}\n"), 0o600))
	testGitCommit(t, ctx, remoteWork, "seed")
	require.NoError(t, testGitRun(ctx, "", "clone", "--bare", remoteWork, remoteRepo))

	opts := Options{RepoPath: shareRepo, Remote: remoteRepo, Branch: "main"}
	require.NoError(t, Pull(ctx, opts))
	require.NoError(t, os.WriteFile(filepath.Join(shareRepo, "local.txt"), []byte("local\n"), 0o600))
	testGitCommit(t, ctx, shareRepo, "local")
	localHead, err := testGitOutput(ctx, shareRepo, "rev-parse", "HEAD")
	require.NoError(t, err)
	originHead, err := testGitOutput(ctx, shareRepo, "rev-parse", "origin/main")
	require.NoError(t, err)
	require.NotEqual(t, strings.TrimSpace(originHead), strings.TrimSpace(localHead))

	require.NoError(t, Pull(ctx, opts))
	afterHead, err := testGitOutput(ctx, shareRepo, "rev-parse", "HEAD")
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(localHead), strings.TrimSpace(afterHead))
}

func TestPullInitializesRequestedRemoteBranchOnClone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	remoteWork := filepath.Join(dir, "remote-work")
	remoteRepo := filepath.Join(dir, "remote.git")
	shareRepo := filepath.Join(dir, "share")

	require.NoError(t, testGitRun(ctx, "", "init", "-b", "main", remoteWork))
	require.NoError(t, os.WriteFile(filepath.Join(remoteWork, "manifest.json"), []byte("release\n"), 0o600))
	testGitCommit(t, ctx, remoteWork, "release")
	require.NoError(t, testGitRun(ctx, remoteWork, "branch", "release"))
	require.NoError(t, os.WriteFile(filepath.Join(remoteWork, "manifest.json"), []byte("main\n"), 0o600))
	testGitCommit(t, ctx, remoteWork, "main")
	require.NoError(t, testGitRun(ctx, "", "clone", "--bare", remoteWork, remoteRepo))

	opts := Options{RepoPath: shareRepo, Remote: remoteRepo, Branch: "release"}
	require.NoError(t, Pull(ctx, opts))
	body, err := os.ReadFile(filepath.Join(shareRepo, "manifest.json"))
	require.NoError(t, err)
	require.Equal(t, "release\n", string(body))
}

func TestImportIfChangedSkipsCurrentManifest(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()

	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main"}
	manifest, err := Export(ctx, source, opts)
	require.NoError(t, err)

	reader, err := store.Open(filepath.Join(dir, "reader.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()

	imported, changed, err := ImportIfChanged(ctx, reader, opts)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, manifest.GeneratedAt.UTC().Format(time.RFC3339Nano), imported.GeneratedAt.UTC().Format(time.RFC3339Nano))

	_, changed, err = ImportIfChanged(ctx, reader, opts)
	require.NoError(t, err)
	require.False(t, changed)
}

func TestImportAtRestoresTaggedSnapshotWithoutMovingCheckout(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()

	opts := Options{RepoPath: filepath.Join(dir, "share"), Branch: "main", Tag: "snapshot-old"}
	_, err := Export(ctx, source, opts)
	require.NoError(t, err)
	committed, err := Commit(ctx, opts, "old snapshot")
	require.NoError(t, err)
	require.True(t, committed)
	tag, err := CreateImmutableTag(ctx, opts)
	require.NoError(t, err)
	require.Equal(t, "snapshot-old", tag)

	_, err = source.DB().ExecContext(ctx, `update messages set text = 'new snapshot', normalized_text = 'new snapshot'`)
	require.NoError(t, err)
	opts.Tag = ""
	_, err = Export(ctx, source, opts)
	require.NoError(t, err)
	committed, err = Commit(ctx, opts, "new snapshot")
	require.NoError(t, err)
	require.True(t, committed)
	headBefore, err := testGitOutput(ctx, opts.RepoPath, "rev-parse", "HEAD")
	require.NoError(t, err)

	reader, err := store.Open(filepath.Join(dir, "reader.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	manifest, err := ImportAt(ctx, reader, opts, "snapshot-old")
	require.NoError(t, err)
	require.False(t, manifest.GeneratedAt.IsZero())
	rows, err := reader.Search(ctx, "", "archive", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "git backed archive works", rows[0].Text)
	headAfter, err := testGitOutput(ctx, opts.RepoPath, "rev-parse", "HEAD")
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(headBefore), strings.TrimSpace(headAfter))
}

func TestExportImportRestoresMediaFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	body := []byte("cached file")
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	mediaPath := "files/" + hash[:2] + "/" + hash + "-incident.txt"
	fullPath, err := media.LocalPath(cacheDir, mediaPath)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o750))
	require.NoError(t, os.WriteFile(fullPath, body, 0o600))

	source := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, source.Close()) }()
	require.NoError(t, source.UpsertChannel(ctx, store.Channel{
		ID:          "C1",
		WorkspaceID: "T1",
		Name:        "general",
		Kind:        "desktop_channel",
		RawJSON:     "{}",
		UpdatedAt:   time.Now().UTC(),
	}))
	require.NoError(t, source.UpsertMessage(ctx, store.Message{
		ChannelID:      "C1",
		TS:             "123.789",
		WorkspaceID:    "T1",
		UserID:         "U1",
		Text:           "media backup",
		NormalizedText: "media backup incident.txt",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      time.Now().UTC(),
		Files: []store.MessageFile{{
			FileID:        "F1",
			Name:          "incident.txt",
			Mimetype:      "text/plain",
			MediaPath:     mediaPath,
			ContentSHA256: hash,
			ContentSize:   int64(len(body)),
			FetchStatus:   "fetched",
			RawJSON:       "{}",
		}},
	}, nil))

	opts := Options{RepoPath: filepath.Join(dir, "share"), CacheDir: cacheDir, Branch: "main", IncludeMedia: true}
	manifest, err := Export(ctx, source, opts)
	require.NoError(t, err)
	require.NotNil(t, manifest.Media)
	require.Len(t, manifest.Media.Items, 1)
	require.FileExists(t, filepath.Join(opts.RepoPath, filepath.FromSlash(manifest.Media.Items[0].Path)))

	reader, err := store.Open(filepath.Join(dir, "reader.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	dstCache := filepath.Join(dir, "dst-cache")
	imported, err := Import(ctx, reader, Options{RepoPath: opts.RepoPath, CacheDir: dstCache, Branch: "main", IncludeMedia: false})
	require.NoError(t, err)
	require.NotNil(t, imported.Media)
	rows, err := reader.Files(ctx, store.FileListOptions{FileID: "F1"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Empty(t, rows[0].MediaPath)

	imported, changed, err := ImportIfChanged(ctx, reader, Options{RepoPath: opts.RepoPath, CacheDir: dstCache, Branch: "main", IncludeMedia: true})
	require.NoError(t, err)
	require.False(t, changed)
	require.NotNil(t, imported.Media)
	rows, err = reader.Files(ctx, store.FileListOptions{FileID: "F1"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, mediaPath, rows[0].MediaPath)
	dstPath, err := media.LocalPath(dstCache, mediaPath)
	require.NoError(t, err)
	require.FileExists(t, dstPath)

	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- media.WithCacheLock(ctx, dstCache, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, _, err = ImportIfChanged(waitCtx, reader, Options{
		RepoPath: opts.RepoPath, CacheDir: dstCache, Branch: "main", IncludeMedia: true,
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	close(release)
	require.NoError(t, <-lockDone)
}

func TestNeedsImportUsesLastImportTime(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := seedStore(t, filepath.Join(dir, "source.db"))
	defer func() { require.NoError(t, s.Close()) }()

	require.True(t, NeedsImport(ctx, s, time.Hour))

	require.NoError(t, s.SetSyncState(ctx, importSyncSource, importSyncEntityType, lastImportEntityID, time.Now().UTC().Format(time.RFC3339Nano)))
	require.False(t, NeedsImport(ctx, s, time.Hour))
	require.NoError(t, s.SetSyncState(ctx, importSyncSource, importSyncEntityType, lastImportEntityID, time.Now().UTC().Add(-2*time.Hour).Format(time.RFC3339Nano)))
	require.True(t, NeedsImport(ctx, s, time.Hour))
}

func writeManifest(t *testing.T, repoPath string, manifest Manifest) {
	t.Helper()
	body, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	body = append(body, '\n')
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, ManifestName), body, 0o600))
}

func assertArchiveStillPresent(t *testing.T, ctx context.Context, s *store.Store) {
	t.Helper()
	rows, err := s.Search(ctx, "", "archive", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "git backed archive works", rows[0].Text)
}

func testGitCommit(t *testing.T, ctx context.Context, repoPath string, message string) {
	t.Helper()
	require.NoError(t, testGitRun(ctx, repoPath, "add", "."))
	require.NoError(t, testGitRun(ctx, repoPath,
		"-c", "commit.gpgsign=false",
		"-c", "user.name=slacrawl-test",
		"-c", "user.email=slacrawl-test@example.invalid",
		"commit", "-m", message,
	))
}

func testGitRun(ctx context.Context, dir string, args ...string) error {
	_, err := testGitOutput(ctx, dir, args...)
	return err
}

func testGitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	body, err := cmd.CombinedOutput()
	if err != nil {
		return string(body), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

func seedStore(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(path)
	require.NoError(t, err)

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s.UpsertWorkspace(ctx, store.Workspace{
		ID:        "T1",
		Name:      "team",
		RawJSON:   "{}",
		UpdatedAt: now,
	}))
	require.NoError(t, s.UpsertChannel(ctx, store.Channel{
		ID:          "C1",
		WorkspaceID: "T1",
		Name:        "general",
		Kind:        "public_channel",
		RawJSON:     "{}",
		UpdatedAt:   now,
	}))
	require.NoError(t, s.UpsertUser(ctx, store.User{
		ID:          "U1",
		WorkspaceID: "T1",
		Name:        "alice",
		RawJSON:     "{}",
		UpdatedAt:   now,
	}))
	require.NoError(t, s.UpsertMessage(ctx, store.Message{
		ChannelID:      "C1",
		TS:             "123.456",
		WorkspaceID:    "T1",
		UserID:         "U1",
		Text:           "git backed archive works",
		NormalizedText: "git backed archive works",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      now,
	}, []store.Mention{{Type: "user", TargetID: "U1", DisplayText: "alice"}}))
	require.NoError(t, s.SetSyncState(ctx, "api-bot", "workspace", "T1", now.Format(time.RFC3339Nano)))
	return s
}
