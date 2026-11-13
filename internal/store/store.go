package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	crawlstore "github.com/openclaw/crawlkit/store"
	"github.com/openclaw/slacrawl/internal/store/storedb"
)

type Store struct {
	db                     *sql.DB
	q                      *storedb.Queries
	ftsRowIDAligned        bool
	searchIndexUnavailable atomic.Bool
}

func Open(path string) (*Store, error) {
	base, err := crawlstore.Open(context.Background(), crawlstore.Options{Path: path})
	if err != nil {
		return nil, err
	}
	db := base.DB()
	currentVersion, err := readSchemaVersion(db)
	if err != nil {
		_ = base.Close()
		return nil, err
	}
	if currentVersion > schemaVersion {
		_ = base.Close()
		return nil, fmt.Errorf("database schema version %d is newer than this slacrawl build supports (%d)", currentVersion, schemaVersion)
	}
	if currentVersion == 0 {
		empty, err := storeSchemaEmpty(db)
		if err != nil {
			_ = base.Close()
			return nil, err
		}
		if !empty {
			currentVersion = 1
		}
	}
	if currentVersion == 0 {
		if _, err := db.Exec(schema); err != nil {
			_ = base.Close()
			return nil, err
		}
	} else if currentVersion < schemaVersion {
		if err := migrateSchema(db, currentVersion); err != nil {
			_ = base.Close()
			return nil, err
		}
	} else {
		if _, err := db.Exec(schema); err != nil {
			_ = base.Close()
			return nil, err
		}
	}
	if err := initializeNativeChanges(db, currentVersion == 0); err != nil {
		_ = base.Close()
		return nil, err
	}
	if err := repairPendingSearchIndex(context.Background(), db); err != nil {
		_ = base.Close()
		return nil, err
	}
	aligned, err := searchIndexRowsAligned(context.Background(), db)
	if err != nil {
		_ = base.Close()
		return nil, err
	}
	if !aligned {
		if err := rebuildSearchIndexes(context.Background(), db); err != nil {
			_ = base.Close()
			return nil, fmt.Errorf("align search index rows: %w", err)
		}
	}
	return &Store{db: db, q: storedb.New(db), ftsRowIDAligned: true}, nil
}

func OpenReadOnly(path string) (*Store, error) {
	base, err := crawlstore.OpenReadOnly(context.Background(), path)
	if err != nil {
		return nil, err
	}
	db := base.DB()
	currentVersion, err := readSchemaVersion(db)
	if err != nil {
		_ = base.Close()
		return nil, err
	}
	if currentVersion > schemaVersion {
		_ = base.Close()
		return nil, fmt.Errorf("database schema version %d is newer than this slacrawl build supports (%d)", currentVersion, schemaVersion)
	}
	aligned := false
	if currentVersion >= 6 {
		pending, err := searchIndexRepairPending(context.Background(), db)
		if err != nil {
			_ = base.Close()
			return nil, err
		}
		if pending {
			_ = base.Close()
			return nil, errors.New("database search index rebuild is pending; open it writable to repair")
		}
		aligned, err = searchIndexRowsAligned(context.Background(), db)
		if err != nil {
			_ = base.Close()
			return nil, err
		}
	}
	return &Store{db: db, q: storedb.New(db), ftsRowIDAligned: aligned}, nil
}

func (s *Store) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) UpsertWorkspace(ctx context.Context, workspace Workspace) error {
	return s.q.UpsertWorkspace(ctx, storedb.UpsertWorkspaceParams{
		ID:           workspace.ID,
		Name:         workspace.Name,
		Domain:       dbText(workspace.Domain),
		EnterpriseID: dbText(workspace.EnterpriseID),
		RawJson:      workspace.RawJSON,
		UpdatedAt:    formatDBTime(workspace.UpdatedAt),
	})
}

// EnsureWorkspace inserts sparse provider metadata without replacing richer data
// already collected by another source.
func (s *Store) EnsureWorkspace(ctx context.Context, workspace Workspace) error {
	return ensureWorkspace(ctx, s.db, workspace)
}

func ensureWorkspace(ctx context.Context, dbtx storedb.DBTX, workspace Workspace) error {
	_, err := dbtx.ExecContext(ctx, `
insert into workspaces (id, name, domain, enterprise_id, raw_json, updated_at)
values (?, ?, ?, ?, ?, ?)
on conflict(id) do nothing
`, workspace.ID, workspace.Name, dbText(workspace.Domain), dbText(workspace.EnterpriseID), workspace.RawJSON, formatDBTime(workspace.UpdatedAt))
	return err
}

func (s *Store) UpsertChannel(ctx context.Context, channel Channel) error {
	rows, err := s.q.UpsertChannel(ctx, storedb.UpsertChannelParams{
		ID:          channel.ID,
		WorkspaceID: channel.WorkspaceID,
		Name:        channel.Name,
		Kind:        channel.Kind,
		Topic:       dbText(channel.Topic),
		Purpose:     dbText(channel.Purpose),
		IsPrivate:   boolInt(channel.IsPrivate),
		IsArchived:  boolInt(channel.IsArchived),
		IsShared:    boolInt(channel.IsShared),
		IsGeneral:   boolInt(channel.IsGeneral),
		RawJson:     channel.RawJSON,
		UpdatedAt:   formatDBTime(channel.UpdatedAt),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		existing, err := s.getChannelWorkspaceKind(ctx, channel.ID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("channel %q upsert affected no rows", channel.ID)
			}
			return err
		}
		if isDesktopHintKind(channel.Kind) {
			return nil
		}
		if isDesktopHintKind(existing.Kind) && strings.HasPrefix(channel.Kind, "desktop_") {
			return s.replaceChannel(ctx, channel)
		}
		if strings.HasPrefix(existing.Kind, "desktop_") && strings.HasPrefix(channel.Kind, "desktop_") {
			return nil
		}
		if existing.WorkspaceID != channel.WorkspaceID {
			return &WorkspaceCollisionError{Entity: "channel", ID: channel.ID, ExistingWorkspaceID: existing.WorkspaceID, WorkspaceID: channel.WorkspaceID}
		}
		if err := rejectWorkspaceCollision(ctx, channel.WorkspaceID, "channel", channel.ID, s.q.GetChannelWorkspace); err != nil {
			return err
		}
		return fmt.Errorf("channel %q upsert affected no rows", channel.ID)
	}
	return nil
}

// EnsureChannel inserts lower-fidelity provider metadata without replacing an
// existing channel collected by a richer source.
func (s *Store) EnsureChannel(ctx context.Context, channel Channel) error {
	return ensureChannel(ctx, s.db, channel)
}

func ensureChannel(ctx context.Context, dbtx storedb.DBTX, channel Channel) error {
	result, err := dbtx.ExecContext(ctx, `
insert into channels (id, workspace_id, name, kind, topic, purpose, is_private, is_archived, is_shared, is_general, raw_json, updated_at)
values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
on conflict(id) do nothing
`, channel.ID, channel.WorkspaceID, channel.Name, channel.Kind, dbText(channel.Topic), dbText(channel.Purpose), boolInt(channel.IsPrivate), boolInt(channel.IsArchived), boolInt(channel.IsShared), boolInt(channel.IsGeneral), channel.RawJSON, formatDBTime(channel.UpdatedAt))
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows > 0 {
		return err
	}
	existing, err := getChannelWorkspaceKind(ctx, dbtx, channel.ID)
	if err != nil {
		return err
	}
	if existing.WorkspaceID != channel.WorkspaceID {
		return &WorkspaceCollisionError{Entity: "channel", ID: channel.ID, ExistingWorkspaceID: existing.WorkspaceID, WorkspaceID: channel.WorkspaceID}
	}
	return nil
}

type channelWorkspaceKind struct {
	WorkspaceID string
	Kind        string
}

func (s *Store) getChannelWorkspaceKind(ctx context.Context, channelID string) (channelWorkspaceKind, error) {
	return getChannelWorkspaceKind(ctx, s.db, channelID)
}

func getChannelWorkspaceKind(ctx context.Context, q queryRower, channelID string) (channelWorkspaceKind, error) {
	var row channelWorkspaceKind
	err := q.QueryRowContext(ctx, `select workspace_id, kind from channels where id = ?`, channelID).Scan(&row.WorkspaceID, &row.Kind)
	return row, err
}

func (s *Store) ChannelWorkspaceID(ctx context.Context, channelID string) (string, error) {
	return s.q.GetChannelWorkspace(ctx, channelID)
}

func (s *Store) replaceChannel(ctx context.Context, channel Channel) error {
	_, err := s.db.ExecContext(ctx, `
update channels
set workspace_id = ?, name = ?, kind = ?, topic = ?, purpose = ?, is_private = ?, is_archived = ?, is_shared = ?, is_general = ?, raw_json = ?, updated_at = ?
where id = ?
`, channel.WorkspaceID, channel.Name, channel.Kind, dbText(channel.Topic), dbText(channel.Purpose), boolInt(channel.IsPrivate), boolInt(channel.IsArchived), boolInt(channel.IsShared), boolInt(channel.IsGeneral), channel.RawJSON, formatDBTime(channel.UpdatedAt), channel.ID)
	return err
}

func isDesktopHintKind(kind string) bool {
	switch kind {
	case "desktop_draft", "desktop_recent", "desktop_mark":
		return true
	default:
		return false
	}
}

func (s *Store) UpsertUser(ctx context.Context, user User) error {
	rows, err := s.q.UpsertUser(ctx, storedb.UpsertUserParams{
		ID:          user.ID,
		WorkspaceID: user.WorkspaceID,
		Name:        user.Name,
		RealName:    dbText(user.RealName),
		DisplayName: dbText(user.DisplayName),
		Title:       dbText(user.Title),
		IsBot:       boolInt(user.IsBot),
		IsDeleted:   boolInt(user.IsDeleted),
		RawJson:     user.RawJSON,
		UpdatedAt:   formatDBTime(user.UpdatedAt),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		if err := rejectWorkspaceCollision(ctx, user.WorkspaceID, "user", user.ID, s.q.GetUserWorkspace); err != nil {
			return err
		}
		return fmt.Errorf("user %q upsert affected no rows", user.ID)
	}
	return nil
}

func (s *Store) UserWorkspaceID(ctx context.Context, userID string) (string, error) {
	return s.q.GetUserWorkspace(ctx, userID)
}

// EnsureUser inserts lower-fidelity provider metadata without replacing an
// existing user collected by a richer source.
func (s *Store) EnsureUser(ctx context.Context, user User) error {
	return ensureUser(ctx, s.db, user)
}

func ensureUser(ctx context.Context, dbtx storedb.DBTX, user User) error {
	result, err := dbtx.ExecContext(ctx, `
insert into users (id, workspace_id, name, real_name, display_name, title, is_bot, is_deleted, raw_json, updated_at)
values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
on conflict(id) do update set
  workspace_id = excluded.workspace_id,
  name = excluded.name,
  real_name = excluded.real_name,
  display_name = excluded.display_name,
  title = excluded.title,
  is_bot = excluded.is_bot,
  is_deleted = excluded.is_deleted,
  raw_json = excluded.raw_json,
  updated_at = excluded.updated_at
where users.workspace_id = excluded.workspace_id
  and users.raw_json = '`+PlaceholderUserRawJSON+`'
`, user.ID, user.WorkspaceID, user.Name, dbText(user.RealName), dbText(user.DisplayName), dbText(user.Title), boolInt(user.IsBot), boolInt(user.IsDeleted), user.RawJSON, formatDBTime(user.UpdatedAt))
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows > 0 {
		return err
	}
	existingWorkspaceID, err := storedb.New(dbtx).GetUserWorkspace(ctx, user.ID)
	if err != nil {
		return err
	}
	if existingWorkspaceID != user.WorkspaceID {
		return &WorkspaceCollisionError{Entity: "user", ID: user.ID, ExistingWorkspaceID: existingWorkspaceID, WorkspaceID: user.WorkspaceID}
	}
	return nil
}

// ApplyWriteBatch commits normalized archive records and their sync cursor as
// one unit. Message writes share the single-message priority, retention, FTS,
// file, mention, and event path.
func MarshalRaw(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func boolInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func dbText(value string) sql.NullString {
	return sql.NullString{String: value, Valid: true}
}

func formatDBTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseDBTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed
	}
	return time.Time{}
}

func slackTSFromTime(value time.Time) string {
	return fmt.Sprintf("%d.%06d", value.UTC().Unix(), value.UTC().Nanosecond()/1000)
}

func RequireLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	return limit
}
