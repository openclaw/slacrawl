package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openclaw/slacrawl/internal/store/storedb"
)

const purgeMessageKeysTable = "temp.slacrawl_purge_message_keys"

const (
	retentionFloorSource     = "retention"
	retentionFloorEntityType = "channel_floor"
	retentionScopeEntityType = "workspace_floor"
	retentionSeedEntityType  = "channel_seed"
	timestampedDraftSQL      = `
instr(substr(ts, 7), ':') > 1
and substr(ts, 7, instr(substr(ts, 7), ':') - 1) not glob '*[^0-9.]*'
and cast(substr(ts, 7, instr(substr(ts, 7), ':') - 1) as real) >= 1000000000`
)

type PurgeOptions struct {
	Before         time.Time
	WorkspaceID    string
	Delete         bool
	RequireNoMedia bool
}

type PurgeMedia struct {
	Path string
	Size int64
}

type PurgeReport struct {
	Messages      int64
	MessageEvents int64
	MessageFiles  int64
	Mentions      int64
	EmbeddingJobs int64
	FTSEntries    int64
	Media         []PurgeMedia
}

func (s *Store) PurgeMessages(ctx context.Context, opts PurgeOptions) (PurgeReport, error) {
	if opts.Before.IsZero() {
		return PurgeReport{}, errors.New("purge cutoff is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PurgeReport{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `drop table if exists `+purgeMessageKeysTable); err != nil {
		return PurgeReport{}, fmt.Errorf("reset purge selection: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
create temporary table slacrawl_purge_message_keys (
  channel_id text not null,
  ts text not null,
  primary key (channel_id, ts)
)
`); err != nil {
		return PurgeReport{}, fmt.Errorf("create purge selection: %w", err)
	}

	before := opts.Before.UTC()
	args := []any{purgeCutoffMicroseconds(before), purgeCutoffMicroseconds(before)}
	workspaceClause := ""
	if workspaceID := strings.TrimSpace(opts.WorkspaceID); workspaceID != "" {
		workspaceClause = " and workspace_id = ?"
		args = append(args, workspaceID)
	}
	if _, err := tx.ExecContext(ctx, `
insert into `+purgeMessageKeysTable+` (channel_id, ts)
select channel_id, ts
from messages
where (
  (
    ts like 'draft:%'
    and `+timestampedDraftSQL+`
    and cast(round(cast(substr(ts, 7, instr(substr(ts, 7), ':') - 1) as real) * 1000000.0) as integer) < ?
  )
  or (
    ts not like 'draft:%'
    and (
      case
        when trim(coalesce(thread_ts, '')) <> '' then cast(round(cast(thread_ts as real) * 1000000.0) as integer)
        else cast(round(cast(ts as real) * 1000000.0) as integer)
      end
    ) < ?
  )
)`+workspaceClause, args...); err != nil {
		return PurgeReport{}, fmt.Errorf("select purge messages: %w", err)
	}
	if err := selectUntimestampedPurgeDrafts(ctx, tx, before, opts.WorkspaceID); err != nil {
		return PurgeReport{}, err
	}

	report, err := readPurgeReport(ctx, tx)
	if err != nil {
		return PurgeReport{}, err
	}
	if opts.Delete && opts.RequireNoMedia && len(report.Media) > 0 {
		return PurgeReport{}, errors.New("purge requires cached media cleanup")
	}
	if opts.Delete {
		if err := setPurgeScopeFloor(ctx, tx, before, opts.WorkspaceID); err != nil {
			return PurgeReport{}, err
		}
		if err := setPurgeChannelSeeds(ctx, tx, opts.WorkspaceID); err != nil {
			return PurgeReport{}, err
		}
		if err := setPurgeRetentionFloors(ctx, tx, before, opts.WorkspaceID); err != nil {
			return PurgeReport{}, err
		}
		if err := deletePurgeSelection(ctx, tx); err != nil {
			return PurgeReport{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `drop table `+purgeMessageKeysTable); err != nil {
		return PurgeReport{}, fmt.Errorf("drop purge selection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return PurgeReport{}, err
	}
	return report, nil
}

func setPurgeScopeFloor(ctx context.Context, tx *sql.Tx, before time.Time, workspaceID string) error {
	entityID := strings.TrimSpace(workspaceID)
	if entityID == "" {
		entityID = "*"
	}
	_, err := tx.ExecContext(ctx, `
insert into sync_state (source_name, entity_type, entity_id, value, updated_at)
values (?, ?, ?, ?, ?)
on conflict(source_name, entity_type, entity_id) do update set
  value = case
    when cast(excluded.value as real) > cast(sync_state.value as real) then excluded.value
    else sync_state.value
  end,
  updated_at = excluded.updated_at
`, retentionFloorSource, retentionScopeEntityType, entityID, formatRetentionFloor(before), formatDBTime(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("set purge scope floor: %w", err)
	}
	return nil
}

func setPurgeChannelSeeds(ctx context.Context, tx *sql.Tx, workspaceID string) error {
	workspaceID = strings.TrimSpace(workspaceID)
	_, err := tx.ExecContext(ctx, `
insert into sync_state (source_name, entity_type, entity_id, value, updated_at)
select ?, ?, m.workspace_id || '|' || m.channel_id, '1', ?
from messages m
where (? = '' or m.workspace_id = ?)
  and m.ts not like 'draft:%'
group by m.workspace_id, m.channel_id
on conflict(source_name, entity_type, entity_id) do update set
  value = excluded.value,
  updated_at = excluded.updated_at
`, retentionFloorSource, retentionSeedEntityType, formatDBTime(time.Now().UTC()), workspaceID, workspaceID)
	if err != nil {
		return fmt.Errorf("set purge channel seeds: %w", err)
	}
	return nil
}

func setPurgeRetentionFloors(ctx context.Context, tx *sql.Tx, before time.Time, workspaceID string) error {
	workspaceID = strings.TrimSpace(workspaceID)
	floor := formatRetentionFloor(before)
	_, err := tx.ExecContext(ctx, `
insert into sync_state (source_name, entity_type, entity_id, value, updated_at)
select ?, ?, scoped.workspace_id || '|' || scoped.channel_id, ?, ?
from (
  select c.workspace_id, c.id as channel_id
  from channels c
  where (? = '' or c.workspace_id = ?)
  union
  select m.workspace_id, m.channel_id
  from messages m
  where (? = '' or m.workspace_id = ?)
) scoped
where true
on conflict(source_name, entity_type, entity_id) do update set
  value = case
    when cast(excluded.value as real) > cast(sync_state.value as real) then excluded.value
    else sync_state.value
  end,
  updated_at = excluded.updated_at
`,
		retentionFloorSource,
		retentionFloorEntityType,
		floor,
		formatDBTime(time.Now().UTC()),
		workspaceID,
		workspaceID,
		workspaceID,
		workspaceID,
	)
	if err != nil {
		return fmt.Errorf("set purge retention floors: %w", err)
	}
	return nil
}

func formatRetentionFloor(before time.Time) string {
	before = before.UTC()
	seconds := before.Unix()
	microseconds := (before.Nanosecond() + int(time.Microsecond) - 1) / int(time.Microsecond)
	if microseconds == int(time.Second/time.Microsecond) {
		seconds++
		microseconds = 0
	}
	return fmt.Sprintf("%d.%06d", seconds, microseconds)
}

func purgeCutoffMicroseconds(before time.Time) int64 {
	before = before.UTC()
	microseconds := int64((before.Nanosecond() + int(time.Microsecond) - 1) / int(time.Microsecond))
	return before.Unix()*int64(time.Second/time.Microsecond) + microseconds
}

func selectUntimestampedPurgeDrafts(ctx context.Context, tx *sql.Tx, before time.Time, workspaceID string) error {
	args := []any{}
	workspaceClause := ""
	if workspaceID = strings.TrimSpace(workspaceID); workspaceID != "" {
		workspaceClause = " and workspace_id = ?"
		args = append(args, workspaceID)
	}
	rows, err := tx.QueryContext(ctx, `
select channel_id, ts, updated_at
from messages
where ts like 'draft:%'
  and not (`+timestampedDraftSQL+`)
`+workspaceClause, args...)
	if err != nil {
		return fmt.Errorf("select untimestamped purge drafts: %w", err)
	}
	type draftKey struct {
		channelID string
		ts        string
	}
	var selected []draftKey
	for rows.Next() {
		var channelID, ts, updatedAtValue string
		if err := rows.Scan(&channelID, &ts, &updatedAtValue); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read untimestamped purge draft: %w", err)
		}
		updatedAt, err := time.Parse(time.RFC3339Nano, updatedAtValue)
		if err != nil {
			_ = rows.Close()
			return fmt.Errorf("parse draft %s/%s updated_at %q: %w", channelID, ts, updatedAtValue, err)
		}
		draftCutoff := before
		if updatedAt.Nanosecond() == 0 {
			draftCutoff = before.Truncate(time.Second)
		}
		if updatedAt.Before(draftCutoff) {
			selected = append(selected, draftKey{channelID: channelID, ts: ts})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read untimestamped purge drafts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close untimestamped purge drafts: %w", err)
	}
	for _, key := range selected {
		if _, err := tx.ExecContext(ctx, `
insert or ignore into `+purgeMessageKeysTable+` (channel_id, ts)
values (?, ?)
`, key.channelID, key.ts); err != nil {
			return fmt.Errorf("select untimestamped purge draft %s/%s: %w", key.channelID, key.ts, err)
		}
	}
	return nil
}

func readPurgeReport(ctx context.Context, tx *sql.Tx) (PurgeReport, error) {
	report := PurgeReport{}
	counts := []struct {
		label string
		query string
		dest  *int64
	}{
		{"messages", `select count(*) from ` + purgeMessageKeysTable, &report.Messages},
		{"message events", `
select count(*)
from message_events e
join ` + purgeMessageKeysTable + ` p on p.channel_id = e.channel_id and p.ts = e.ts
`, &report.MessageEvents},
		{"message files", `
select count(*)
from message_files f
join ` + purgeMessageKeysTable + ` p on p.channel_id = f.channel_id and p.ts = f.ts
`, &report.MessageFiles},
		{"mentions", `
select count(*)
from message_mentions m
join ` + purgeMessageKeysTable + ` p on p.channel_id = m.channel_id and p.ts = m.ts
`, &report.Mentions},
		{"embedding jobs", `
select count(*)
from embedding_jobs e
join ` + purgeMessageKeysTable + ` p on p.channel_id = e.channel_id and p.ts = e.ts
`, &report.EmbeddingJobs},
		{"FTS entries", `
select count(*)
from message_fts f
join ` + purgeMessageKeysTable + ` p on f.message_key = p.channel_id || '|' || p.ts
`, &report.FTSEntries},
	}
	for _, count := range counts {
		if err := tx.QueryRowContext(ctx, count.query).Scan(count.dest); err != nil {
			return PurgeReport{}, fmt.Errorf("count purge %s: %w", count.label, err)
		}
	}

	rows, err := tx.QueryContext(ctx, `
select f.media_path, max(f.content_size)
from message_files f
join `+purgeMessageKeysTable+` p on p.channel_id = f.channel_id and p.ts = f.ts
where trim(coalesce(f.media_path, '')) <> ''
  and not exists (
    select 1
    from message_files other
    where other.media_path = f.media_path
      and not exists (
        select 1
        from `+purgeMessageKeysTable+` selected
        where selected.channel_id = other.channel_id and selected.ts = other.ts
      )
  )
group by f.media_path
order by f.media_path
`)
	if err != nil {
		return PurgeReport{}, fmt.Errorf("list purge media: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var item PurgeMedia
		if err := rows.Scan(&item.Path, &item.Size); err != nil {
			return PurgeReport{}, err
		}
		report.Media = append(report.Media, item)
	}
	if err := rows.Err(); err != nil {
		return PurgeReport{}, err
	}
	return report, nil
}

func deletePurgeSelection(ctx context.Context, tx *sql.Tx) error {
	deletes := []struct {
		label string
		query string
	}{
		{"message events", `
delete from message_events
where exists (
  select 1 from ` + purgeMessageKeysTable + ` p
  where p.channel_id = message_events.channel_id and p.ts = message_events.ts
)`},
		{"message files", `
delete from message_files
where exists (
  select 1 from ` + purgeMessageKeysTable + ` p
  where p.channel_id = message_files.channel_id and p.ts = message_files.ts
)`},
		{"mentions", `
delete from message_mentions
where exists (
  select 1 from ` + purgeMessageKeysTable + ` p
  where p.channel_id = message_mentions.channel_id and p.ts = message_mentions.ts
)`},
		{"embedding jobs", `
delete from embedding_jobs
where exists (
  select 1 from ` + purgeMessageKeysTable + ` p
  where p.channel_id = embedding_jobs.channel_id and p.ts = embedding_jobs.ts
)`},
		{"FTS entries", `
delete from message_fts
where exists (
  select 1 from ` + purgeMessageKeysTable + ` p
  where message_fts.message_key = p.channel_id || '|' || p.ts
)`},
		{"messages", `
delete from messages
where exists (
  select 1 from ` + purgeMessageKeysTable + ` p
  where p.channel_id = messages.channel_id and p.ts = messages.ts
)`},
	}
	for _, deletion := range deletes {
		if _, err := tx.ExecContext(ctx, deletion.query); err != nil {
			return fmt.Errorf("delete purge %s: %w", deletion.label, err)
		}
	}
	return nil
}

func (s *Store) Vacuum(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "vacuum")
	return err
}

func (s *Store) UnreferencedPurgeMediaAfter(ctx context.Context, items, removed []PurgeMedia) ([]PurgeMedia, int64, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	removedPaths := make(map[string]struct{}, len(removed))
	for _, item := range removed {
		removedPaths[item.Path] = struct{}{}
	}
	unreferenced, retained, err := filterUnreferencedPurgeMedia(ctx, tx, items, removedPaths)
	if err != nil {
		return nil, 0, err
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return unreferenced, retained, nil
}

func (s *Store) WithUnreferencedPurgeMedia(ctx context.Context, items []PurgeMedia, fn func([]PurgeMedia) error) (int64, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "begin immediate"); err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "rollback")
		}
	}()

	unreferenced, retained, err := filterUnreferencedPurgeMedia(ctx, conn, items, nil)
	if err != nil {
		return 0, err
	}
	if err := fn(unreferenced); err != nil {
		return retained, err
	}
	if _, err := conn.ExecContext(ctx, "commit"); err != nil {
		return retained, err
	}
	committed = true
	return retained, nil
}

func filterUnreferencedPurgeMedia(ctx context.Context, q storedb.DBTX, items []PurgeMedia, removedPaths map[string]struct{}) ([]PurgeMedia, int64, error) {
	rows, err := q.QueryContext(ctx, `
select distinct media_path
from message_files
where trim(coalesce(media_path, '')) <> ''
`)
	if err != nil {
		return nil, 0, err
	}
	referencedPaths := make(map[string]struct{})
	for rows.Next() {
		var mediaPath string
		if err := rows.Scan(&mediaPath); err != nil {
			_ = rows.Close()
			return nil, 0, err
		}
		if _, removed := removedPaths[mediaPath]; removed {
			continue
		}
		referencedPaths[strings.ToLower(mediaPath)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, 0, err
	}
	if err := rows.Close(); err != nil {
		return nil, 0, err
	}

	unreferenced := make([]PurgeMedia, 0, len(items))
	var retained int64
	for _, item := range items {
		_, referenced := referencedPaths[strings.ToLower(item.Path)]
		if referenced {
			retained++
			continue
		}
		unreferenced = append(unreferenced, item)
	}
	return unreferenced, retained, nil
}
