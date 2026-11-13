package share

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/openclaw/crawlkit/mirror"
	"github.com/openclaw/slacrawl/internal/admission"
	"github.com/openclaw/slacrawl/internal/media"
	"github.com/openclaw/slacrawl/internal/store"
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
	Tag          string
	CacheDir     string
	IncludeMedia bool
	DMPolicy     admission.DMPolicy
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

// ValidateImportPolicy runs before acquisition because legacy snapshots carry
// no admission evidence for all archived content and derived state.
func ValidateImportPolicy(policy admission.DMPolicy) error {
	if policy == admission.Exclude {
		return errors.New("legacy Git share imports cannot enforce sync.include_dms=false; set share.auto_update=false to continue local queries or source sync without importing snapshots")
	}
	return nil
}

// ValidateExportPolicy precedes archive and Git work because legacy snapshots
// cannot exclude all DM-derived content already present in the archive.
func ValidateExportPolicy(policy admission.DMPolicy) error {
	if policy == admission.Exclude {
		return errors.New("legacy Git share exports cannot enforce sync.include_dms=false; keep this archive local")
	}
	return nil
}

func EnsureRepo(ctx context.Context, opts Options) error {
	return mirror.EnsureRepo(ctx, mirrorOptions(opts))
}

func Pull(ctx context.Context, opts Options) error {
	if strings.TrimSpace(opts.Remote) == "" {
		return EnsureRepo(ctx, opts)
	}
	if err := mirror.EnsureRemote(ctx, mirrorOptions(opts)); err != nil {
		return err
	}
	pullOpts := mirrorOptions(opts)
	pullOpts.Remote = ""
	return mirror.PullCurrent(ctx, pullOpts)
}

func Commit(ctx context.Context, opts Options, message string) (bool, error) {
	if strings.TrimSpace(message) == "" {
		message = "sync: slack archive"
	}
	return mirror.CommitPaths(ctx, mirrorOptions(opts), message, []string{"."})
}

func Push(ctx context.Context, opts Options) error {
	if strings.TrimSpace(opts.Tag) == "" {
		return mirror.Push(ctx, mirrorOptions(opts))
	}
	return mirror.PushSnapshot(ctx, mirrorOptions(opts), opts.Tag)
}

func ValidateTag(ctx context.Context, opts Options) error {
	if strings.TrimSpace(opts.Tag) == "" {
		return nil
	}
	if strings.TrimSpace(opts.Remote) != "" {
		if err := mirror.EnsureRemote(ctx, mirrorOptions(opts)); err != nil {
			return err
		}
	} else if err := mirror.EnsureRepo(ctx, mirrorOptions(opts)); err != nil {
		return err
	}
	if err := mirror.SyncForWrite(ctx, mirrorOptions(opts)); err != nil {
		return err
	}
	return mirror.ValidateTag(ctx, mirrorOptions(opts), opts.Tag)
}

func CreateImmutableTag(ctx context.Context, opts Options) (string, error) {
	return mirror.CreateImmutableTag(ctx, mirrorOptions(opts), opts.Tag)
}

func Export(ctx context.Context, s *store.Store, opts Options) (Manifest, error) {
	if err := ValidateExportPolicy(opts.DMPolicy); err != nil {
		return Manifest{}, err
	}
	if opts.IncludeMedia && strings.TrimSpace(opts.CacheDir) != "" {
		var manifest Manifest
		err := media.WithCacheLock(ctx, opts.CacheDir, func() error {
			var err error
			manifest, err = exportLocked(ctx, s, opts)
			return err
		})
		return manifest, err
	}
	return exportLocked(ctx, s, opts)
}

func exportLocked(ctx context.Context, s *store.Store, opts Options) (Manifest, error) {
	if strings.TrimSpace(opts.Remote) != "" {
		if err := mirror.EnsureRemote(ctx, mirrorOptions(opts)); err != nil {
			return Manifest{}, err
		}
	}
	if err := mirror.SyncForWrite(ctx, mirrorOptions(opts)); err != nil {
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
		entry, err := exportMediaLocked(ctx, s.DB(), opts)
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
	return importWithMode(ctx, s, opts, false)
}

// Restore replaces every snapshot table. Callers must expose this destructive
// mode explicitly; routine imports merge and never remove destination rows.
func Restore(ctx context.Context, s *store.Store, opts Options) (Manifest, error) {
	return importWithMode(ctx, s, opts, true)
}

func importWithMode(ctx context.Context, s *store.Store, opts Options, restore bool) (Manifest, error) {
	if err := ValidateImportPolicy(opts.DMPolicy); err != nil {
		return Manifest{}, err
	}
	if opts.IncludeMedia && strings.TrimSpace(opts.CacheDir) != "" {
		var manifest Manifest
		err := media.WithCacheLock(ctx, opts.CacheDir, func() error {
			var err error
			manifest, err = importLocked(ctx, s, opts, restore)
			return err
		})
		return manifest, err
	}
	return importLocked(ctx, s, opts, restore)
}

func importLocked(ctx context.Context, s *store.Store, opts Options, restore bool) (Manifest, error) {
	manifest, err := ReadManifest(opts.RepoPath)
	if err != nil {
		return Manifest{}, err
	}
	if err := validateManifest(ctx, s.DB(), opts.RepoPath, manifest); err != nil {
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
	if restore {
		if _, err := tx.ExecContext(ctx, `delete from message_event_heads`); err != nil {
			return Manifest{}, fmt.Errorf("clear message event heads: %w", err)
		}
		for i := len(SnapshotTables) - 1; i >= 0; i-- {
			table := SnapshotTables[i]
			if _, err := tx.ExecContext(ctx, "delete from "+quoteIdent(table)); err != nil { //nolint:gosec // Snapshot table names are quoted identifiers from the fixed schema list.
				return Manifest{}, fmt.Errorf("clear %s: %w", table, err)
			}
		}
	}
	mergeState := &mergeImportState{
		rejectedMessages:          map[string]struct{}{},
		retentionRejectedMessages: map[string]struct{}{},
		importedFiles:             map[string]struct{}{},
	}
	manifestTables := make(map[string]TableManifest, len(manifest.Tables))
	for _, table := range manifest.Tables {
		manifestTables[table.Name] = table
	}
	for _, tableName := range SnapshotTables {
		table := manifestTables[tableName]
		rows, err := importTable(ctx, tx, opts.RepoPath, table, restore, opts.IncludeMedia, mergeState)
		if err != nil {
			return Manifest{}, err
		}
		if rows != table.Rows {
			return Manifest{}, fmt.Errorf("manifest table %s row count mismatch: imported %d, expected %d", table.Name, rows, table.Rows)
		}
	}
	if err := store.BackfillDeletedSubordinates(ctx, tx); err != nil {
		return Manifest{}, err
	}
	if err := rebuildMessageFTS(ctx, tx); err != nil {
		return Manifest{}, err
	}
	if err := rebuildMessageEventHeads(ctx, tx); err != nil {
		return Manifest{}, err
	}
	var mediaManifest *MediaManifest
	if opts.IncludeMedia {
		mediaManifest = manifest.Media
	}
	if restore {
		if err := clearUnmanifestedFileMedia(ctx, tx, mediaManifest); err != nil {
			return Manifest{}, err
		}
	} else if opts.IncludeMedia {
		if err := clearUnmanifestedImportedFileMedia(ctx, tx, mediaManifest, mergeState.importedFiles); err != nil {
			return Manifest{}, err
		}
	}
	if err := preserveImportedFileMedia(ctx, tx, existingMedia); err != nil {
		return Manifest{}, err
	}
	if opts.IncludeMedia {
		if _, err := importMedia(ctx, opts, mediaManifest); err != nil {
			return Manifest{}, err
		}
	}
	// Keep base rows and their derived search index in one visibility boundary:
	// readers see either the previous complete snapshot or the imported one.
	if err := store.RebuildSearchIndexesInTransaction(ctx, tx); err != nil {
		return Manifest{}, fmt.Errorf("rebuild imported search index: %w", err)
	}
	if restore {
		if err := store.ResetNativeChangesInTransaction(ctx, tx); err != nil {
			return Manifest{}, fmt.Errorf("reset restored native journal: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Manifest{}, err
	}
	committed = true
	if err := MarkImported(ctx, s, manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func rebuildMessageFTS(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
delete from message_fts;
insert into message_fts (message_key, content)
select m.channel_id || '|' || m.ts,
       trim(m.normalized_text || ' ' || coalesce((
         select group_concat(trim(f.name || ' ' || f.title || ' ' || f.plain_text || ' ' || f.preview_plain_text), ' ')
         from message_files f
         where f.channel_id = m.channel_id and f.ts = m.ts and f.deleted_at is null
       ), ''))
from messages m;
`); err != nil {
		return fmt.Errorf("rebuild message_fts: %w", err)
	}
	return nil
}

func rebuildMessageEventHeads(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
insert into message_event_heads (channel_id, ts, event_type, source_name, payload_json)
select e.channel_id, e.ts, e.event_type, e.source_name, e.payload_json
from message_events e
join (
  select channel_id, ts, event_type, source_name, max(id) as id
  from message_events
  group by channel_id, ts, event_type, source_name
) latest on latest.id = e.id
where not exists (
  select 1 from message_event_heads h
  where h.channel_id = e.channel_id
    and h.ts = e.ts
    and h.event_type = e.event_type
    and h.source_name = e.source_name
);

insert into message_event_heads (channel_id, ts, event_type, source_name, payload_json)
select channel_id, ts,
       case
         when trim(coalesce(deleted_ts, '')) <> '' then 'message_deleted'
         when trim(coalesce(edited_ts, '')) <> '' then 'message_changed'
         else 'message'
       end,
       source_name, raw_json
from messages
where true
on conflict(channel_id, ts, event_type, source_name) do update set
  payload_json = excluded.payload_json;
`); err != nil {
		return fmt.Errorf("rebuild message event heads: %w", err)
	}
	return nil
}

func validateManifest(ctx context.Context, db *sql.DB, repoPath string, manifest Manifest) error {
	expected := make(map[string]struct{}, len(SnapshotTables))
	for _, table := range SnapshotTables {
		expected[table] = struct{}{}
	}
	seen := make(map[string]struct{}, len(manifest.Tables))
	for _, table := range manifest.Tables {
		if _, ok := expected[table.Name]; !ok {
			return fmt.Errorf("manifest contains unknown table %q", table.Name)
		}
		if _, ok := seen[table.Name]; ok {
			return fmt.Errorf("manifest contains duplicate table %q", table.Name)
		}
		seen[table.Name] = struct{}{}
		if table.Rows < 0 {
			return fmt.Errorf("manifest table %s has negative row count", table.Name)
		}
		files := tableManifestFiles(table)
		if len(files) == 0 {
			return fmt.Errorf("manifest table %s has no files", table.Name)
		}
		for _, rel := range files {
			if _, err := resolveManifestTableFile(repoPath, table.Name, rel); err != nil {
				return fmt.Errorf("manifest table %s file %q: %w", table.Name, rel, err)
			}
		}
		columns, err := tableColumns(ctx, db, table.Name)
		if err != nil {
			return err
		}
		if !compatibleTableColumns(table.Name, table.Columns, columns) {
			return fmt.Errorf("manifest table %s columns mismatch", table.Name)
		}
	}
	for _, table := range SnapshotTables {
		if _, ok := seen[table]; !ok {
			return fmt.Errorf("manifest missing table %q", table)
		}
	}
	return nil
}

func ImportIfChanged(ctx context.Context, s *store.Store, opts Options) (Manifest, bool, error) {
	if err := ValidateImportPolicy(opts.DMPolicy); err != nil {
		return Manifest{}, false, err
	}
	manifest, err := ReadManifest(opts.RepoPath)
	if err != nil {
		return Manifest{}, false, err
	}
	if ManifestAlreadyImported(ctx, s, manifest) {
		if opts.IncludeMedia && strings.TrimSpace(opts.CacheDir) != "" {
			var imported Manifest
			err := media.WithCacheLock(ctx, opts.CacheDir, func() error {
				var err error
				imported, err = importCurrentManifestLocked(ctx, s, opts, manifest)
				return err
			})
			return imported, false, err
		}
		imported, err := importCurrentManifestLocked(ctx, s, opts, manifest)
		return imported, false, err
	}
	imported, err := Import(ctx, s, opts)
	if err != nil {
		return Manifest{}, false, err
	}
	return imported, true, nil
}

func importCurrentManifestLocked(ctx context.Context, s *store.Store, opts Options, manifest Manifest) (Manifest, error) {
	if opts.IncludeMedia {
		missing, err := mediaMetadataMissing(ctx, s.DB(), manifest.Media)
		if err != nil {
			return Manifest{}, err
		}
		if missing {
			return importLocked(ctx, s, opts, false)
		}
		if _, err := importMedia(ctx, opts, manifest.Media); err != nil {
			return Manifest{}, err
		}
	}
	if err := MarkImported(ctx, s, manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
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
	return parseManifest(data)
}

func parseManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse share manifest: %w", err)
	}
	if manifest.Version != 1 {
		return Manifest{}, fmt.Errorf("unsupported share manifest version %d", manifest.Version)
	}
	return manifest, nil
}

// RestoreAt exactly restores a snapshot from a Git ref without changing the share checkout.
func RestoreAt(ctx context.Context, s *store.Store, opts Options, ref string) (Manifest, error) {
	if err := ValidateImportPolicy(opts.DMPolicy); err != nil {
		return Manifest{}, err
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Restore(ctx, s, opts)
	}
	if err := mirror.Fetch(ctx, mirrorOptions(opts)); err != nil {
		return Manifest{}, err
	}
	manifestBody, commit, err := mirror.ReadFileAt(ctx, mirrorOptions(opts), ref, ManifestName)
	if err != nil {
		return Manifest{}, err
	}
	manifest, err := parseManifest(manifestBody)
	if err != nil {
		return Manifest{}, err
	}
	tempDir, err := os.MkdirTemp("", "slacrawl-share-ref-*")
	if err != nil {
		return Manifest{}, fmt.Errorf("create historical share directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()
	if err := os.WriteFile(filepath.Join(tempDir, ManifestName), manifestBody, 0o600); err != nil {
		return Manifest{}, fmt.Errorf("write historical manifest: %w", err)
	}
	for _, table := range manifest.Tables {
		for _, file := range tableManifestFiles(table) {
			if err := materializeRefFile(ctx, mirrorOptions(opts), commit, file, tempDir); err != nil {
				return Manifest{}, err
			}
		}
	}
	if opts.IncludeMedia && manifest.Media != nil {
		for _, item := range manifest.Media.Items {
			if err := materializeRefFile(ctx, mirrorOptions(opts), commit, item.Path, tempDir); err != nil {
				return Manifest{}, err
			}
		}
	}
	historicalOpts := opts
	historicalOpts.RepoPath = tempDir
	historicalOpts.Remote = ""
	historicalOpts.Tag = ""
	return Restore(ctx, s, historicalOpts)
}

func materializeRefFile(ctx context.Context, opts mirror.Options, ref, filePath, targetRoot string) error {
	clean := path.Clean(filepath.ToSlash(strings.TrimSpace(filePath)))
	if clean == "." || clean == ".." || path.IsAbs(clean) || strings.HasPrefix(clean, "../") || strings.ContainsRune(clean, '\x00') {
		return fmt.Errorf("invalid historical share path %q", filePath)
	}
	body, _, err := mirror.ReadFileAt(ctx, opts, ref, clean)
	if err != nil {
		return err
	}
	target := filepath.Join(targetRoot, filepath.FromSlash(clean))
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("create historical share directory: %w", err)
	}
	if err := os.WriteFile(target, body, 0o600); err != nil {
		return fmt.Errorf("write historical share file %s: %w", clean, err)
	}
	return nil
}

func normalizeBranch(branch string) string {
	if strings.TrimSpace(branch) == "" {
		return defaultBranch
	}
	return strings.TrimSpace(branch)
}

func mirrorOptions(opts Options) mirror.Options {
	return mirror.Options{
		RepoPath: strings.TrimSpace(opts.RepoPath),
		Remote:   strings.TrimSpace(opts.Remote),
		Branch:   normalizeBranch(opts.Branch),
		DirMode:  0o750,
	}
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
