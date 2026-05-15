package share

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vincentkoc/slacrawl/internal/media"
	"github.com/vincentkoc/slacrawl/internal/store"
)

const (
	ManifestName = "manifest.json"

	importSyncSource           = "share"
	importSyncEntityType       = "import"
	lastImportEntityID         = "last_import_at"
	lastManifestEntityID       = "last_manifest_generated_at"
	defaultBranch              = "main"
	shardFlushRows             = 1024
	defaultMaxShardBytes int64 = 40 * 1024 * 1024
)

var ErrNoManifest = errors.New("share manifest not found")

var errUnsafeMediaPath = errors.New("unsafe media path")

var maxShardBytes = defaultMaxShardBytes

var SnapshotTables = []string{
	"workspaces",
	"channels",
	"users",
	"messages",
	"message_files",
	"message_events",
	"message_mentions",
	"sync_state",
	"embedding_jobs",
}

type Options struct {
	RepoPath     string
	Remote       string
	Branch       string
	CacheDir     string
	IncludeMedia bool
}

type Manifest struct {
	Version     int               `json:"version"`
	GeneratedAt time.Time         `json:"generated_at"`
	Tables      []TableManifest   `json:"tables"`
	Media       *MediaManifest    `json:"media,omitempty"`
	Files       map[string]string `json:"files,omitempty"`
}

type MediaManifest struct {
	Files int                 `json:"files"`
	Bytes int64               `json:"bytes"`
	Items []MediaFileManifest `json:"items,omitempty"`
}

type MediaFileManifest struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type TableManifest struct {
	Name    string   `json:"name"`
	File    string   `json:"file,omitempty"`
	Files   []string `json:"files,omitempty"`
	Columns []string `json:"columns"`
	Rows    int      `json:"rows"`
}

type SyncState struct {
	LastImportAt            time.Time `json:"last_import_at"`
	LastManifestGeneratedAt time.Time `json:"last_manifest_generated_at"`
}

func EnsureRepo(ctx context.Context, opts Options) error {
	if strings.TrimSpace(opts.RepoPath) == "" {
		return errors.New("share repo path is empty")
	}
	if _, err := os.Stat(filepath.Join(opts.RepoPath, ".git")); err == nil {
		return nil
	}
	if strings.TrimSpace(opts.Remote) != "" {
		if err := os.MkdirAll(filepath.Dir(opts.RepoPath), 0o750); err != nil {
			return fmt.Errorf("mkdir share parent: %w", err)
		}
		if err := gitRun(ctx, "", "clone", opts.Remote, opts.RepoPath); err != nil {
			return err
		}
		if branch := normalizeBranch(opts.Branch); branch != "" {
			if err := gitRun(ctx, opts.RepoPath, "checkout", "-B", branch); err != nil {
				return err
			}
		}
		return nil
	}
	if err := os.MkdirAll(opts.RepoPath, 0o750); err != nil {
		return fmt.Errorf("mkdir share repo: %w", err)
	}
	if err := gitRun(ctx, opts.RepoPath, "init"); err != nil {
		return err
	}
	if branch := normalizeBranch(opts.Branch); branch != "" {
		if err := gitRun(ctx, opts.RepoPath, "checkout", "-B", branch); err != nil {
			return err
		}
	}
	return nil
}

func Pull(ctx context.Context, opts Options) error {
	if strings.TrimSpace(opts.Remote) == "" {
		return EnsureRepo(ctx, opts)
	}
	if err := EnsureRepo(ctx, opts); err != nil {
		return err
	}
	if err := gitRun(ctx, opts.RepoPath, "fetch", "--prune", "origin"); err != nil {
		return err
	}
	branch := normalizeBranch(opts.Branch)
	remoteRef := "refs/remotes/origin/" + branch
	if _, err := gitOutput(ctx, opts.RepoPath, "rev-parse", "--verify", remoteRef); err != nil {
		return gitRun(ctx, opts.RepoPath, "checkout", "-B", branch)
	}
	return gitRun(ctx, opts.RepoPath, "checkout", "-B", branch, "origin/"+branch)
}

func Commit(ctx context.Context, opts Options, message string) (bool, error) {
	if err := gitRun(ctx, opts.RepoPath, "add", "."); err != nil {
		return false, err
	}
	out, err := gitOutput(ctx, opts.RepoPath, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(out) == "" {
		return false, nil
	}
	if strings.TrimSpace(message) == "" {
		message = "sync: slack archive"
	}
	if err := gitRun(ctx, opts.RepoPath,
		"-c", "commit.gpgsign=false",
		"-c", "user.name=slacrawl",
		"-c", "user.email=slacrawl@example.invalid",
		"commit", "-m", message,
	); err != nil {
		return false, err
	}
	return true, nil
}

func Push(ctx context.Context, opts Options) error {
	branch := normalizeBranch(opts.Branch)
	out, err := gitOutput(ctx, opts.RepoPath, "push", "-u", "origin", branch)
	if err == nil {
		return nil
	}
	if !isNonFastForwardPush(out) {
		return fmt.Errorf("git push -u origin %s: %w\n%s", branch, err, strings.TrimSpace(out))
	}
	if pullErr := gitRun(ctx, opts.RepoPath, "pull", "--rebase", "--autostash", "origin", branch); pullErr != nil {
		return fmt.Errorf("rebase before push retry: %w", pullErr)
	}
	return gitRun(ctx, opts.RepoPath, "push", "-u", "origin", branch)
}

func Export(ctx context.Context, s *store.Store, opts Options) (Manifest, error) {
	if err := EnsureRepo(ctx, opts); err != nil {
		return Manifest{}, err
	}
	dataDir := filepath.Join(opts.RepoPath, "tables")
	if err := os.RemoveAll(dataDir); err != nil {
		return Manifest{}, fmt.Errorf("reset tables dir: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return Manifest{}, fmt.Errorf("mkdir tables dir: %w", err)
	}
	manifest := Manifest{
		Version:     1,
		GeneratedAt: time.Now().UTC(),
		Files:       map[string]string{"manifest": ManifestName},
	}
	for _, table := range SnapshotTables {
		entry, err := exportTable(ctx, s.DB(), dataDir, table)
		if err != nil {
			return Manifest{}, err
		}
		manifest.Tables = append(manifest.Tables, entry)
	}
	if opts.IncludeMedia {
		entry, err := exportMedia(ctx, s.DB(), opts)
		if err != nil {
			return Manifest{}, err
		}
		if entry != nil {
			manifest.Media = entry
		}
	} else if err := os.RemoveAll(filepath.Join(opts.RepoPath, "media")); err != nil {
		return Manifest{}, fmt.Errorf("reset media dir: %w", err)
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	body = append(body, '\n')
	if err := os.WriteFile(filepath.Join(opts.RepoPath, ManifestName), body, 0o600); err != nil {
		return Manifest{}, fmt.Errorf("write manifest: %w", err)
	}
	return manifest, nil
}

func Import(ctx context.Context, s *store.Store, opts Options) (Manifest, error) {
	manifest, err := ReadManifest(opts.RepoPath)
	if err != nil {
		return Manifest{}, err
	}
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return Manifest{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	existingMedia, err := fileMediaByKey(ctx, tx)
	if err != nil {
		return Manifest{}, err
	}
	if _, err := tx.ExecContext(ctx, `delete from message_fts`); err != nil {
		return Manifest{}, fmt.Errorf("clear message_fts: %w", err)
	}
	for i := len(SnapshotTables) - 1; i >= 0; i-- {
		table := SnapshotTables[i]
		if _, err := tx.ExecContext(ctx, "delete from "+quoteIdent(table)); err != nil { //nolint:gosec // Snapshot table names are quoted identifiers from the fixed schema list.
			return Manifest{}, fmt.Errorf("clear %s: %w", table, err)
		}
	}
	for _, table := range manifest.Tables {
		if err := importTable(ctx, tx, opts.RepoPath, table); err != nil {
			return Manifest{}, err
		}
	}
	var mediaManifest *MediaManifest
	if opts.IncludeMedia {
		mediaManifest = manifest.Media
	}
	if err := clearUnmanifestedFileMedia(ctx, tx, mediaManifest); err != nil {
		return Manifest{}, err
	}
	if err := preserveImportedFileMedia(ctx, tx, existingMedia); err != nil {
		return Manifest{}, err
	}
	if opts.IncludeMedia {
		if _, err := importMedia(ctx, opts, mediaManifest); err != nil {
			return Manifest{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Manifest{}, err
	}
	committed = true
	if err := s.RebuildSearchIndexes(ctx); err != nil {
		return Manifest{}, err
	}
	if err := MarkImported(ctx, s, manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func ImportIfChanged(ctx context.Context, s *store.Store, opts Options) (Manifest, bool, error) {
	manifest, err := ReadManifest(opts.RepoPath)
	if err != nil {
		return Manifest{}, false, err
	}
	if ManifestAlreadyImported(ctx, s, manifest) {
		if opts.IncludeMedia {
			missing, err := mediaMetadataMissing(ctx, s.DB(), manifest.Media)
			if err != nil {
				return Manifest{}, false, err
			}
			if missing {
				imported, err := Import(ctx, s, opts)
				return imported, false, err
			}
			if _, err := importMedia(ctx, opts, manifest.Media); err != nil {
				return Manifest{}, false, err
			}
		}
		if err := MarkImported(ctx, s, manifest); err != nil {
			return Manifest{}, false, err
		}
		return manifest, false, nil
	}
	imported, err := Import(ctx, s, opts)
	if err != nil {
		return Manifest{}, false, err
	}
	return imported, true, nil
}

func ManifestAlreadyImported(ctx context.Context, s *store.Store, manifest Manifest) bool {
	if manifest.GeneratedAt.IsZero() {
		return false
	}
	last, err := s.GetSyncState(ctx, importSyncSource, importSyncEntityType, lastManifestEntityID)
	if err != nil || strings.TrimSpace(last) == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, last)
	if err != nil {
		return false
	}
	return t.Equal(manifest.GeneratedAt)
}

func MarkImported(ctx context.Context, s *store.Store, manifest Manifest) error {
	if err := s.SetSyncState(ctx, importSyncSource, importSyncEntityType, lastImportEntityID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if manifest.GeneratedAt.IsZero() {
		return nil
	}
	return s.SetSyncState(ctx, importSyncSource, importSyncEntityType, lastManifestEntityID, manifest.GeneratedAt.Format(time.RFC3339Nano))
}

func NeedsImport(ctx context.Context, s *store.Store, staleAfter time.Duration) bool {
	if staleAfter <= 0 {
		staleAfter = 15 * time.Minute
	}
	last, err := s.GetSyncState(ctx, importSyncSource, importSyncEntityType, lastImportEntityID)
	if err != nil || strings.TrimSpace(last) == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339Nano, last)
	if err != nil {
		return true
	}
	return time.Since(t) >= staleAfter
}

func ReadSyncState(ctx context.Context, s *store.Store) (SyncState, error) {
	var state SyncState
	lastImport, err := s.GetSyncState(ctx, importSyncSource, importSyncEntityType, lastImportEntityID)
	if err == nil {
		state.LastImportAt = parseSyncTime(lastImport)
	}
	lastManifest, err := s.GetSyncState(ctx, importSyncSource, importSyncEntityType, lastManifestEntityID)
	if err == nil {
		state.LastManifestGeneratedAt = parseSyncTime(lastManifest)
	}
	return state, nil
}

func ReadManifest(repoPath string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(repoPath, ManifestName)) //nolint:gosec // Repo path is explicit user configuration.
	if err != nil {
		if os.IsNotExist(err) {
			return Manifest{}, ErrNoManifest
		}
		return Manifest{}, fmt.Errorf("read share manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse share manifest: %w", err)
	}
	if manifest.Version != 1 {
		return Manifest{}, fmt.Errorf("unsupported share manifest version %d", manifest.Version)
	}
	return manifest, nil
}

func exportTable(ctx context.Context, db *sql.DB, dataDir, table string) (TableManifest, error) {
	rows, err := db.QueryContext(ctx, "select * from "+quoteIdent(table)) //nolint:gosec // Table names are emitted through quoteIdent from export metadata.
	if err != nil {
		return TableManifest{}, fmt.Errorf("query %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	columns, err := rows.Columns()
	if err != nil {
		return TableManifest{}, fmt.Errorf("columns %s: %w", table, err)
	}
	tableDir := filepath.Join(dataDir, table)
	if err := os.MkdirAll(tableDir, 0o750); err != nil {
		return TableManifest{}, fmt.Errorf("mkdir %s: %w", table, err)
	}
	writer := tableShardWriter{dataDir: dataDir, table: table}
	if err := writer.open(); err != nil {
		return TableManifest{}, err
	}
	defer func() { _ = writer.close() }()

	values := make([]any, len(columns))
	ptrs := make([]any, len(columns))
	for i := range values {
		ptrs[i] = &values[i]
	}
	count := 0
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return TableManifest{}, fmt.Errorf("scan %s: %w", table, err)
		}
		row := make(map[string]any, len(columns))
		for i, column := range columns {
			row[column] = exportValue(values[i])
		}
		body, err := json.Marshal(row)
		if err != nil {
			return TableManifest{}, fmt.Errorf("marshal %s row: %w", table, err)
		}
		if err := writer.rotateIfNeeded(); err != nil {
			return TableManifest{}, err
		}
		if _, err := writer.Write(body); err != nil {
			return TableManifest{}, fmt.Errorf("write %s row: %w", table, err)
		}
		if _, err := writer.Write([]byte{'\n'}); err != nil {
			return TableManifest{}, fmt.Errorf("write %s newline: %w", table, err)
		}
		count++
		if err := writer.finishRow(); err != nil {
			return TableManifest{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return TableManifest{}, fmt.Errorf("iterate %s: %w", table, err)
	}
	if err := writer.close(); err != nil {
		return TableManifest{}, err
	}
	return TableManifest{Name: table, Files: writer.files, Columns: columns, Rows: count}, nil
}

func importTable(ctx context.Context, tx *sql.Tx, repoPath string, table TableManifest) error {
	files := table.Files
	if len(files) == 0 && strings.TrimSpace(table.File) != "" {
		files = []string{table.File}
	}
	if len(files) == 0 {
		return fmt.Errorf("manifest table %s has no files", table.Name)
	}
	stmt, err := tx.PrepareContext(ctx, insertSQL(table.Name, table.Columns))
	if err != nil {
		return fmt.Errorf("prepare import %s: %w", table.Name, err)
	}
	defer func() { _ = stmt.Close() }()
	for _, rel := range files {
		if err := importTableFile(ctx, stmt, repoPath, table, rel); err != nil {
			return err
		}
	}
	return nil
}

func importTableFile(ctx context.Context, stmt *sql.Stmt, repoPath string, table TableManifest, rel string) error {
	path := filepath.Join(repoPath, filepath.FromSlash(rel))
	file, err := os.Open(path) //nolint:gosec // Import reads files from the configured backup repo.
	if err != nil {
		return fmt.Errorf("open %s: %w", rel, err)
	}
	defer func() { _ = file.Close() }()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("read gzip %s: %w", rel, err)
	}
	defer func() { _ = gz.Close() }()
	dec := json.NewDecoder(gz)
	dec.UseNumber()
	for {
		row := map[string]any{}
		err := dec.Decode(&row)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("decode %s: %w", rel, err)
		}
		values := make([]any, len(table.Columns))
		for i, column := range table.Columns {
			values[i] = importValue(row[column])
		}
		if _, err := stmt.ExecContext(ctx, values...); err != nil {
			return fmt.Errorf("insert %s: %w", table.Name, err)
		}
	}
	return nil
}

func exportMedia(ctx context.Context, db *sql.DB, opts Options) (*MediaManifest, error) {
	if strings.TrimSpace(opts.CacheDir) == "" {
		return nil, nil
	}
	if err := os.RemoveAll(filepath.Join(opts.RepoPath, "media")); err != nil {
		return nil, fmt.Errorf("reset media dir: %w", err)
	}
	rows, err := db.QueryContext(ctx, `
select f.workspace_id, f.channel_id, f.ts, f.file_id, coalesce(f.media_path, ''), coalesce(f.content_sha256, '')
from message_files f
join channels c on c.id = f.channel_id and c.workspace_id = f.workspace_id
where coalesce(f.media_path, '') <> ''
  and c.kind in ('public_channel', 'desktop_channel')
order by f.media_path, f.channel_id, f.ts, f.file_id
`)
	if err != nil {
		return nil, fmt.Errorf("query media files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	manifest := &MediaManifest{}
	seen := map[string]struct{}{}
	for rows.Next() {
		var workspaceID, channelID, ts, fileID, mediaPath, expectedHash string
		if err := rows.Scan(&workspaceID, &channelID, &ts, &fileID, &mediaPath, &expectedHash); err != nil {
			return nil, err
		}
		if _, ok := seen[mediaPath]; ok {
			manifest.Files++
			continue
		}
		source, err := media.LocalPath(opts.CacheDir, mediaPath)
		if err != nil {
			return nil, err
		}
		info, err := regularMediaFile(filepath.Join(opts.CacheDir, "media"), source, mediaPath)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, errUnsafeMediaPath) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stat media %s: %w", mediaPath, err)
		}
		manifest.Files++
		seen[mediaPath] = struct{}{}
		rel := filepath.ToSlash(filepath.Join("media", mediaPath))
		target, err := media.RepoPath(opts.RepoPath, mediaPath)
		if err != nil {
			return nil, err
		}
		if err := copyFile(target, source); err != nil {
			return nil, fmt.Errorf("copy media %s: %w", mediaPath, err)
		}
		hash, err := fileSHA256(target)
		if err != nil {
			return nil, err
		}
		if expectedHash != "" && hash != expectedHash {
			return nil, fmt.Errorf("media hash mismatch for %s: got %s want %s", mediaPath, hash, expectedHash)
		}
		manifest.Items = append(manifest.Items, MediaFileManifest{Path: rel, Size: info.Size(), SHA256: hash})
		manifest.Bytes += info.Size()
		_ = workspaceID
		_ = channelID
		_ = ts
		_ = fileID
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(manifest.Items) == 0 {
		return nil, nil
	}
	return manifest, nil
}

func importMedia(ctx context.Context, opts Options, manifest *MediaManifest) (int, error) {
	if manifest == nil || strings.TrimSpace(opts.CacheDir) == "" {
		return 0, nil
	}
	copied := 0
	for _, item := range manifest.Items {
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		mediaPath, ok := strings.CutPrefix(filepath.ToSlash(item.Path), "media/")
		if !ok || strings.TrimSpace(mediaPath) == "" {
			return copied, fmt.Errorf("invalid media manifest path %q", item.Path)
		}
		source, err := media.RepoPath(opts.RepoPath, mediaPath)
		if err != nil {
			return copied, err
		}
		if _, err := regularMediaFile(filepath.Join(opts.RepoPath, "media"), source, item.Path); err != nil {
			return copied, err
		}
		hash, err := fileSHA256(source)
		if err != nil {
			return copied, fmt.Errorf("hash media %s: %w", item.Path, err)
		}
		if item.SHA256 != "" && hash != item.SHA256 {
			return copied, fmt.Errorf("media hash mismatch for %s: got %s want %s", item.Path, hash, item.SHA256)
		}
		target, err := media.LocalPath(opts.CacheDir, mediaPath)
		if err != nil {
			return copied, err
		}
		if sameFileHash(target, hash) {
			continue
		}
		if err := copyFile(target, source); err != nil {
			return copied, fmt.Errorf("copy media %s: %w", item.Path, err)
		}
		copied++
	}
	return copied, nil
}

func mediaMetadataMissing(ctx context.Context, db *sql.DB, manifest *MediaManifest) (bool, error) {
	if manifest == nil || len(manifest.Items) == 0 {
		return false, nil
	}
	for _, item := range manifest.Items {
		mediaPath, ok := strings.CutPrefix(filepath.ToSlash(item.Path), "media/")
		if !ok || strings.TrimSpace(mediaPath) == "" {
			continue
		}
		var found int
		err := db.QueryRowContext(ctx, `select 1 from message_files where media_path = ? limit 1`, mediaPath).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
	return false, nil
}

type fileMediaRecord struct {
	MediaPath     string
	ContentSHA256 string
	ContentSize   int64
	FetchedAt     string
	FetchStatus   string
	FetchError    string
}

func fileMediaByKey(ctx context.Context, tx *sql.Tx) (map[string]fileMediaRecord, error) {
	rows, err := tx.QueryContext(ctx, `
select channel_id, ts, file_id, coalesce(media_path, ''), coalesce(content_sha256, ''),
       content_size, coalesce(fetched_at, ''), fetch_status, fetch_error
from message_files
where coalesce(media_path, '') <> ''
`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]fileMediaRecord{}
	for rows.Next() {
		var channelID, ts, fileID string
		var record fileMediaRecord
		if err := rows.Scan(&channelID, &ts, &fileID, &record.MediaPath, &record.ContentSHA256, &record.ContentSize, &record.FetchedAt, &record.FetchStatus, &record.FetchError); err != nil {
			return nil, err
		}
		out[fileMediaKey(channelID, ts, fileID)] = record
	}
	return out, rows.Err()
}

func clearUnmanifestedFileMedia(ctx context.Context, tx *sql.Tx, manifest *MediaManifest) error {
	manifested := map[string]struct{}{}
	if manifest != nil {
		for _, item := range manifest.Items {
			mediaPath, ok := strings.CutPrefix(filepath.ToSlash(item.Path), "media/")
			if ok && strings.TrimSpace(mediaPath) != "" {
				manifested[mediaPath] = struct{}{}
			}
		}
	}
	rows, err := tx.QueryContext(ctx, `
select channel_id, ts, file_id, coalesce(media_path, '')
from message_files
where coalesce(media_path, '') <> ''
`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type key struct {
		channelID string
		ts        string
		fileID    string
	}
	clear := []key{}
	for rows.Next() {
		var channelID, ts, fileID, mediaPath string
		if err := rows.Scan(&channelID, &ts, &fileID, &mediaPath); err != nil {
			return err
		}
		if _, ok := manifested[mediaPath]; !ok {
			clear = append(clear, key{channelID: channelID, ts: ts, fileID: fileID})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(clear) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
update message_files
set media_path = null, content_sha256 = null, content_size = 0, fetched_at = null,
    fetch_status = '', fetch_error = ''
where channel_id = ? and ts = ? and file_id = ?
`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, row := range clear {
		if _, err := stmt.ExecContext(ctx, row.channelID, row.ts, row.fileID); err != nil {
			return err
		}
	}
	return nil
}

func preserveImportedFileMedia(ctx context.Context, tx *sql.Tx, existing map[string]fileMediaRecord) error {
	if len(existing) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
update message_files
set media_path = ?, content_sha256 = ?, content_size = ?, fetched_at = ?,
    fetch_status = ?, fetch_error = ?
where channel_id = ? and ts = ? and file_id = ? and coalesce(media_path, '') = ''
`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for key, record := range existing {
		channelID, ts, fileID, ok := splitFileMediaKey(key)
		if !ok {
			continue
		}
		if _, err := stmt.ExecContext(ctx, record.MediaPath, nullableString(record.ContentSHA256), record.ContentSize, nullableString(record.FetchedAt), record.FetchStatus, record.FetchError, channelID, ts, fileID); err != nil {
			return err
		}
	}
	return nil
}

func fileMediaKey(channelID, ts, fileID string) string {
	return channelID + "\x00" + ts + "\x00" + fileID
}

func splitFileMediaKey(key string) (string, string, string, bool) {
	parts := strings.Split(key, "\x00")
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func regularMediaFile(root, path, label string) (os.FileInfo, error) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return nil, fmt.Errorf("%w: media %s escapes media root", errUnsafeMediaPath, label)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return nil, fmt.Errorf("%w: invalid media path %q", errUnsafeMediaPath, label)
	}
	current := root
	if info, err := os.Lstat(current); err == nil && !info.IsDir() {
		return nil, fmt.Errorf("%w: media root for %s is not a directory", errUnsafeMediaPath, label)
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: media %s contains symlinked path component", errUnsafeMediaPath, label)
		}
		if current == path {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%w: media %s is not a regular file", errUnsafeMediaPath, label)
			}
			return info, nil
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%w: media %s parent is not a directory", errUnsafeMediaPath, label)
		}
	}
	return nil, fmt.Errorf("%w: invalid media path %q", errUnsafeMediaPath, label)
}

func copyFile(dst, src string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src) //nolint:gosec // Source path is validated by caller.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".copy-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // Caller confines path.
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func sameFileHash(path, hash string) bool {
	current, err := fileSHA256(path)
	return err == nil && current == hash
}

type tableShardWriter struct {
	dataDir     string
	table       string
	nextShard   int
	rowsInShard int
	files       []string
	file        *os.File
	counter     *countingWriter
	gz          *gzip.Writer
}

func (w *tableShardWriter) open() error {
	rel := filepath.ToSlash(filepath.Join("tables", w.table, fmt.Sprintf("%06d.jsonl.gz", w.nextShard)))
	path := filepath.Join(w.dataDir, w.table, fmt.Sprintf("%06d.jsonl.gz", w.nextShard))
	file, err := os.Create(path) //nolint:gosec // Export creates files below the configured backup repo.
	if err != nil {
		return fmt.Errorf("create %s: %w", rel, err)
	}
	w.nextShard++
	w.rowsInShard = 0
	w.files = append(w.files, rel)
	w.file = file
	w.counter = &countingWriter{w: file}
	w.gz = gzip.NewWriter(w.counter)
	return nil
}

func (w *tableShardWriter) Write(p []byte) (int, error) {
	return w.gz.Write(p)
}

func (w *tableShardWriter) rotateIfNeeded() error {
	if maxShardBytes <= 0 || w.rowsInShard == 0 || w.counter.n < maxShardBytes {
		return nil
	}
	if err := w.close(); err != nil {
		return err
	}
	return w.open()
}

func (w *tableShardWriter) finishRow() error {
	w.rowsInShard++
	if maxShardBytes > 1024*1024 && w.rowsInShard%shardFlushRows != 0 {
		return nil
	}
	if err := w.gz.Flush(); err != nil {
		return fmt.Errorf("flush %s shard: %w", w.table, err)
	}
	return nil
}

func (w *tableShardWriter) close() error {
	var closeErr error
	if w.gz != nil {
		if err := w.gz.Close(); err != nil {
			closeErr = err
		}
		w.gz = nil
	}
	if w.file != nil {
		if err := w.file.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
		w.file = nil
	}
	if closeErr != nil {
		return fmt.Errorf("close %s shard: %w", w.table, closeErr)
	}
	return nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

func exportValue(value any) any {
	switch v := value.(type) {
	case []byte:
		return string(v)
	default:
		return v
	}
}

func importValue(value any) any {
	switch v := value.(type) {
	case json.Number:
		if i, err := strconv.ParseInt(v.String(), 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(v.String(), 64); err == nil {
			return f
		}
		return v.String()
	default:
		return v
	}
}

func insertSQL(table string, columns []string) string {
	quoted := make([]string, len(columns))
	placeholders := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = quoteIdent(column)
		placeholders[i] = "?"
	}
	return "insert into " + quoteIdent(table) + "(" + strings.Join(quoted, ",") + ") values(" + strings.Join(placeholders, ",") + ")"
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func normalizeBranch(branch string) string {
	if strings.TrimSpace(branch) == "" {
		return defaultBranch
	}
	return strings.TrimSpace(branch)
}

func gitRun(ctx context.Context, dir string, args ...string) error {
	out, err := gitOutput(ctx, dir, args...)
	if err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(out))
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	//nolint:gosec // This helper only invokes git with caller-controlled subcommands.
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	body, err := cmd.CombinedOutput()
	return string(body), err
}

func isNonFastForwardPush(out string) bool {
	lower := strings.ToLower(out)
	return strings.Contains(lower, "non-fast-forward") ||
		strings.Contains(lower, "fetch first") ||
		strings.Contains(lower, "failed to push some refs")
}

func parseSyncTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}
