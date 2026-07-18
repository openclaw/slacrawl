package report

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/openclaw/slacrawl/internal/store"
)

// DigestOptions controls how a Digest is built.
//
// Since is the lookback duration from Now. Zero means the caller did not
// specify a window and the default (7 days) is used. A negative Since is
// treated as its absolute value.
type DigestOptions struct {
	Now         time.Time
	Since       time.Duration
	WorkspaceID string
	Channel     string
	TopN        int
}

// Digest summarizes recent activity for each channel inside a window.
type Digest struct {
	GeneratedAt time.Time       `json:"generated_at"`
	Since       time.Time       `json:"since"`
	Until       time.Time       `json:"until"`
	WindowLabel string          `json:"window_label"`
	Workspace   string          `json:"workspace,omitempty"`
	Channel     string          `json:"channel,omitempty"`
	TopN        int             `json:"top_n"`
	Channels    []ChannelDigest `json:"channels"`
	Totals      DigestTotals    `json:"totals"`
}

// ChannelDigest is the per-channel roll-up inside a Digest.
type ChannelDigest struct {
	ChannelID     string        `json:"channel_id"`
	ChannelName   string        `json:"channel_name"`
	Kind          string        `json:"kind,omitempty"`
	WorkspaceID   string        `json:"workspace_id"`
	Messages      int           `json:"messages"`
	Threads       int           `json:"threads"`
	ActiveAuthors int           `json:"active_authors"`
	TopPosters    []RankedCount `json:"top_posters"`
	TopMentions   []RankedCount `json:"top_mentions"`
}

// DigestTotals sums message and channel counts across the digest window.
type DigestTotals struct {
	Messages      int `json:"messages"`
	Threads       int `json:"threads"`
	Channels      int `json:"channels"`
	ActiveAuthors int `json:"active_authors"`
}

// BuildDigest computes a per-channel activity digest from the local store.
func BuildDigest(ctx context.Context, s *store.Store, opts DigestOptions) (Digest, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	since := opts.Since
	if since < 0 {
		since = -since
	}
	if since == 0 {
		since = 7 * 24 * time.Hour
	}
	topN := opts.TopN
	if topN <= 0 {
		topN = 1
	}

	digest := Digest{
		GeneratedAt: now,
		Since:       now.Add(-since),
		Until:       now,
		WindowLabel: humanDuration(since),
		Workspace:   opts.WorkspaceID,
		Channel:     opts.Channel,
		TopN:        topN,
	}

	channels, err := perChannel(ctx, s.DB(), digest.Since, digest.Until, opts.WorkspaceID, opts.Channel)
	if err != nil {
		return Digest{}, err
	}
	for i := range channels {
		channels[i].TopPosters, err = topPostersForChannel(ctx, s.DB(), digest.Since, digest.Until, channels[i].WorkspaceID, channels[i].ChannelID, topN)
		if err != nil {
			return Digest{}, err
		}
		channels[i].TopMentions, err = topMentionsForChannel(ctx, s.DB(), digest.Since, digest.Until, channels[i].WorkspaceID, channels[i].ChannelID, topN)
		if err != nil {
			return Digest{}, err
		}
	}
	digest.Channels = channels

	totals, err := digestTotals(ctx, s.DB(), digest.Since, digest.Until, opts.WorkspaceID, opts.Channel)
	if err != nil {
		return Digest{}, err
	}
	digest.Totals = totals

	return digest, nil
}

// perChannel returns one row per channel with messages, threads, and active-author counts for the window.
func perChannel(ctx context.Context, db *sql.DB, since time.Time, until time.Time, workspaceID, channel string) ([]ChannelDigest, error) {
	query := &strings.Builder{}
	query.WriteString(`
select
	m.workspace_id,
	m.channel_id,
	coalesce(nullif(c.name, ''), m.channel_id) as channel_name,
	coalesce(c.kind, '') as kind,
	count(*) as messages,
	count(distinct case when m.thread_ts != '' and m.thread_ts = m.ts then m.ts else null end) as threads,
	count(distinct case when nullif(m.user_id, '') is not null then m.workspace_id || '|' || m.user_id end) as active_authors
from messages m
left join channels c on c.id = m.channel_id and c.workspace_id = m.workspace_id
where m.ts not like 'draft:%'
  and instr(m.ts, '.') > 0
  and cast(substr(m.ts, 1, instr(m.ts, '.') - 1) as integer) >= ?
  and m.ts <= ?
`)
	args := []any{since.Unix(), slackTSBoundary(until)}
	if workspaceID != "" {
		query.WriteString("  and m.workspace_id = ?\n")
		args = append(args, workspaceID)
	}
	if channel != "" {
		query.WriteString("  and (m.channel_id = ? or c.name = ?)\n")
		args = append(args, channel, channel)
	}
	query.WriteString(`group by m.workspace_id, m.channel_id
order by messages desc, channel_name asc
`)

	rows, err := db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("digest per-channel query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ChannelDigest
	for rows.Next() {
		var row ChannelDigest
		if err := rows.Scan(&row.WorkspaceID, &row.ChannelID, &row.ChannelName, &row.Kind, &row.Messages, &row.Threads, &row.ActiveAuthors); err != nil {
			return nil, fmt.Errorf("digest per-channel scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// topPostersForChannel returns the top posters for a single channel.
func topPostersForChannel(ctx context.Context, db *sql.DB, since time.Time, until time.Time, workspaceID, channelID string, limit int) ([]RankedCount, error) {
	return ranked(ctx, db, `
select
	coalesce(
		nullif(u.display_name, ''),
		nullif(u.real_name, ''),
		nullif(u.name, ''),
		nullif(m.user_id, ''),
		'unknown'
	) as name,
	count(*) as total
from messages m
left join users u on u.id = m.user_id and u.workspace_id = m.workspace_id
where m.ts not like 'draft:%'
  and instr(m.ts, '.') > 0
  and cast(substr(m.ts, 1, instr(m.ts, '.') - 1) as integer) >= ?
  and m.ts <= ?
  and m.workspace_id = ?
  and m.channel_id = ?
group by m.workspace_id, m.user_id
order by total desc, name asc
limit ?
`, since.Unix(), slackTSBoundary(until), workspaceID, channelID, limit)
}

// topMentionsForChannel returns the top mention targets for a single channel.
func topMentionsForChannel(ctx context.Context, db *sql.DB, since time.Time, until time.Time, workspaceID, channelID string, limit int) ([]RankedCount, error) {
	return ranked(ctx, db, `
select
	coalesce(
		nullif(mm.display_text, ''),
		nullif(u.display_name, ''),
		nullif(u.real_name, ''),
		nullif(u.name, ''),
		nullif(c.name, ''),
		mm.target_id
	) as name,
	count(*) as total
from message_mentions mm
join messages m on m.channel_id = mm.channel_id and m.ts = mm.ts
left join users u on u.id = mm.target_id and u.workspace_id = m.workspace_id
left join channels c on c.id = mm.target_id and c.workspace_id = m.workspace_id
where m.ts not like 'draft:%'
  and mm.deleted_at is null
  and instr(m.ts, '.') > 0
  and cast(substr(m.ts, 1, instr(m.ts, '.') - 1) as integer) >= ?
  and m.ts <= ?
  and m.workspace_id = ?
  and m.channel_id = ?
group by mm.target_id
order by total desc, name asc
limit ?
`, since.Unix(), slackTSBoundary(until), workspaceID, channelID, limit)
}

// digestTotals sums messages/threads/channels/authors across the whole window.
func digestTotals(ctx context.Context, db *sql.DB, since time.Time, until time.Time, workspaceID, channel string) (DigestTotals, error) {
	query := &strings.Builder{}
	query.WriteString(`
select
	count(*) as messages,
	count(distinct case when m.thread_ts != '' and m.thread_ts = m.ts then m.ts else null end) as threads,
	count(distinct m.workspace_id || '|' || m.channel_id) as channels,
	count(distinct case when nullif(m.user_id, '') is not null then m.workspace_id || '|' || m.user_id end) as active_authors
from messages m
left join channels c on c.id = m.channel_id and c.workspace_id = m.workspace_id
where m.ts not like 'draft:%'
  and instr(m.ts, '.') > 0
  and cast(substr(m.ts, 1, instr(m.ts, '.') - 1) as integer) >= ?
  and m.ts <= ?
`)
	args := []any{since.Unix(), slackTSBoundary(until)}
	if workspaceID != "" {
		query.WriteString("  and m.workspace_id = ?\n")
		args = append(args, workspaceID)
	}
	if channel != "" {
		query.WriteString("  and (m.channel_id = ? or c.name = ?)\n")
		args = append(args, channel, channel)
	}

	var totals DigestTotals
	if err := db.QueryRowContext(ctx, query.String(), args...).Scan(&totals.Messages, &totals.Threads, &totals.Channels, &totals.ActiveAuthors); err != nil {
		return DigestTotals{}, fmt.Errorf("digest totals: %w", err)
	}
	return totals, nil
}

func slackTSBoundary(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

// humanDuration renders a window duration as a compact human string (e.g. "7d", "36h").
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "0"
	}
	if d%(24*time.Hour) == 0 {
		days := int(d / (24 * time.Hour))
		return fmt.Sprintf("%dd", days)
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return d.String()
}
