package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	crawlstore "github.com/openclaw/crawlkit/store"
	"github.com/openclaw/slacrawl/internal/store/storedb"
)

func (s *Store) Status(ctx context.Context) (Status, error) {
	status, _, err := s.status(ctx, false)
	return status, err
}

// StatusWithAPIThreadCoverage also evaluates retained work for a live Doctor
// decision, even when the stored marker is not full.
func (s *Store) StatusWithAPIThreadCoverage(ctx context.Context) (Status, APIThreadCoverageFacts, error) {
	return s.status(ctx, true)
}

func (s *Store) status(ctx context.Context, includeFacts bool) (Status, APIThreadCoverageFacts, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Status{}, APIThreadCoverageFacts{}, err
	}
	defer tx.Rollback()
	// OpenReadOnly has one connection. Every query must use this transaction,
	// both to share its snapshot and to avoid waiting for our own connection.
	status, facts, err := readStatus(ctx, tx, includeFacts)
	if err != nil {
		return Status{}, APIThreadCoverageFacts{}, err
	}
	if err := tx.Commit(); err != nil {
		return Status{}, APIThreadCoverageFacts{}, err
	}
	if err := ctx.Err(); err != nil {
		return Status{}, APIThreadCoverageFacts{}, err
	}
	return status, facts, nil
}

func readStatus(ctx context.Context, dbtx storedb.DBTX, includeFacts bool) (Status, APIThreadCoverageFacts, error) {
	q := storedb.New(dbtx)
	feed, err := readNativeChangeStatus(ctx, dbtx)
	if err != nil {
		return Status{}, APIThreadCoverageFacts{}, err
	}
	status := Status{NativeChanges: feed}
	countWorkspaces, err := q.CountWorkspaces(ctx)
	if err != nil {
		return Status{}, APIThreadCoverageFacts{}, err
	}
	countChannels, err := q.CountChannels(ctx)
	if err != nil {
		return Status{}, APIThreadCoverageFacts{}, err
	}
	countUsers, err := q.CountUsers(ctx)
	if err != nil {
		return Status{}, APIThreadCoverageFacts{}, err
	}
	countMessages, err := q.CountMessages(ctx)
	if err != nil {
		return Status{}, APIThreadCoverageFacts{}, err
	}
	status.Workspaces = int(countWorkspaces)
	status.Channels = int(countChannels)
	status.Users = int(countUsers)
	status.Messages = int(countMessages)

	lastSync, err := q.LastSyncAt(ctx)
	if err != nil {
		return Status{}, APIThreadCoverageFacts{}, err
	}
	if lastSync != "" {
		parsed, err := time.Parse(time.RFC3339, lastSync)
		if err == nil {
			status.LastSyncAt = parsed
		}
	}

	status.ThreadState = "partial"
	threadState, err := q.ThreadCoverageState(ctx)
	if err == nil {
		status.ThreadState = threadState
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Status{}, APIThreadCoverageFacts{}, err
	}

	var facts APIThreadCoverageFacts
	if includeFacts || status.ThreadState == "full" {
		facts, err = apiThreadCoverageFacts(ctx, dbtx)
		if err != nil {
			return Status{}, APIThreadCoverageFacts{}, err
		}
		if status.ThreadState == "full" && (facts.ThreadWork || facts.IncompleteHistory) {
			status.ThreadState = "partial"
		}
	}
	return status, facts, nil
}

func (s *Store) Search(ctx context.Context, workspaceID string, query string, limit int) ([]MessageRow, error) {
	return s.searchFTS(ctx, workspaceID, query, limit)
}

func (s *Store) SearchMessages(ctx context.Context, opts SearchOptions) ([]MessageRow, error) {
	query := strings.TrimSpace(opts.Query)
	if query == "" {
		return nil, nil
	}
	mode := opts.Mode
	if mode == "" {
		mode = SearchModeAuto
	}
	switch mode {
	case SearchModeRawFTS:
		return s.searchFTS(ctx, opts.WorkspaceID, query, opts.Limit)
	case SearchModePhrase:
		return s.searchFTS(ctx, opts.WorkspaceID, crawlstore.FTS5Phrase(query), opts.Limit)
	case SearchModeTerms:
		return s.searchFTS(ctx, opts.WorkspaceID, termsFTS5Query(query), opts.Limit)
	case SearchModeAuto:
		return s.searchAuto(ctx, opts.WorkspaceID, query, opts.Limit)
	default:
		return nil, fmt.Errorf("unsupported search mode %q", mode)
	}
}

func (s *Store) searchAuto(ctx context.Context, workspaceID string, query string, limit int) ([]MessageRow, error) {
	candidates := []string{crawlstore.FTS5Phrase(query)}
	if terms := termsFTS5Query(query); terms != "" && terms != candidates[0] {
		candidates = append(candidates, terms)
	}

	for _, candidate := range candidates {
		rows, err := s.searchFTS(ctx, workspaceID, candidate, limit)
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			return rows, nil
		}
	}
	return s.searchLike(ctx, workspaceID, query, limit)
}

func (s *Store) searchFTS(ctx context.Context, workspaceID string, query string, limit int) ([]MessageRow, error) {
	if s.searchIndexUnavailable.Load() {
		return nil, errors.New("search index is temporarily unavailable; retry or reopen the database")
	}
	messageJoin := "join messages m on f.message_key = m.channel_id || '|' || m.ts"
	if s.ftsRowIDAligned {
		messageJoin = "join messages m on f.rowid = m.rowid"
	}
	sqlQuery := `
select ` + messageRowSelect + `
from message_fts f
` + messageJoin + `
` + messageRowJoins + `
where message_fts match ?
  and (? = '' or m.workspace_id = ?)
order by m.ts desc
limit ?
`
	rows, err := s.queryMessageRows(ctx, sqlQuery, query, workspaceID, workspaceID, RequireLimit(limit))
	if err != nil {
		return nil, err
	}
	if s.searchIndexUnavailable.Load() {
		return nil, errors.New("search index is temporarily unavailable; retry or reopen the database")
	}
	return rows, nil
}

func (s *Store) searchLike(ctx context.Context, workspaceID string, query string, limit int) ([]MessageRow, error) {
	pattern := "%" + escapeLike(strings.ToLower(strings.TrimSpace(query))) + "%"
	sqlQuery := `
select ` + messageRowSelect + `
from messages m
` + messageRowJoins + `
where (? = '' or m.workspace_id = ?)
  and (lower(m.text) like ? escape '\' or lower(m.normalized_text) like ? escape '\')
order by m.ts desc
limit ?
`
	return s.queryMessageRows(ctx, sqlQuery, workspaceID, workspaceID, pattern, pattern, RequireLimit(limit))
}

func termsFTS5Query(query string) string {
	terms := searchTerms(query)
	if len(terms) == 0 {
		return crawlstore.FTS5Phrase(query)
	}
	quoted := make([]string, 0, len(terms))
	for _, term := range terms {
		quoted = append(quoted, crawlstore.FTS5Phrase(term))
	}
	return strings.Join(quoted, " AND ")
}

func searchTerms(query string) []string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return !(r == '_' || r == '-' || r == '@' || r == '#' || r == '.' || unicode.IsLetter(r) || unicode.IsDigit(r))
	})
	out := make([]string, 0, len(fields))
	seen := map[string]struct{}{}
	for _, field := range fields {
		field = strings.Trim(field, "_-.@#")
		if field == "" {
			continue
		}
		key := strings.ToLower(field)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, field)
	}
	return out
}

func escapeLike(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '\\', '%', '_':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *Store) Messages(ctx context.Context, workspaceID string, channelID string, userID string, limit int) ([]MessageRow, error) {
	query := `
select ` + messageRowSelect + `
from messages m
` + messageRowJoins + `
where 1=1`
	args := []any{}
	if workspaceID != "" {
		query += ` and m.workspace_id = ?`
		args = append(args, workspaceID)
	}
	if channelID != "" {
		query += ` and m.channel_id = ?`
		args = append(args, channelID)
	}
	if userID != "" {
		query += ` and m.user_id = ?`
		args = append(args, userID)
	}
	query += ` order by m.ts desc limit ?`
	args = append(args, limit)
	return s.queryMessageRows(ctx, query, args...)
}

func (s *Store) MessagesWithThreadContext(ctx context.Context, workspaceID string, channelID string, userID string, limit int) ([]MessageRow, error) {
	rows, err := s.Messages(ctx, workspaceID, channelID, userID, limit)
	if err != nil {
		return nil, err
	}
	return s.hydrateThreadContext(ctx, rows, limit)
}

func (s *Store) hydrateThreadContext(ctx context.Context, rows []MessageRow, limit int) ([]MessageRow, error) {
	if len(rows) == 0 {
		return rows, nil
	}
	type threadRef struct {
		workspaceID string
		channelID   string
		threadTS    string
	}
	refs := make([]threadRef, 0)
	seenRefs := map[string]struct{}{}
	for _, row := range rows {
		threadTS := slackThreadRootTS(row)
		if threadTS == "" {
			continue
		}
		key := row.WorkspaceID + "\x00" + row.ChannelID + "\x00" + threadTS
		if _, ok := seenRefs[key]; ok {
			continue
		}
		seenRefs[key] = struct{}{}
		refs = append(refs, threadRef{workspaceID: row.WorkspaceID, channelID: row.ChannelID, threadTS: threadTS})
	}
	if len(refs) == 0 {
		return rows, nil
	}
	clauses := make([]string, 0, len(refs))
	args := make([]any, 0, len(refs)*4+1)
	for _, ref := range refs {
		clauses = append(clauses, `(m.workspace_id = ? and m.channel_id = ? and (m.ts = ? or m.thread_ts = ?))`)
		args = append(args, ref.workspaceID, ref.channelID, ref.threadTS, ref.threadTS)
	}
	contextLimit := limit * 5
	if contextLimit < len(rows) {
		contextLimit = len(rows)
	}
	if contextLimit < 200 {
		contextLimit = 200
	}
	if contextLimit > 2000 {
		contextLimit = 2000
	}
	query := `
select ` + messageRowSelect + `
from messages m
` + messageRowJoins + `
where ` + strings.Join(clauses, " or ") + `
order by m.ts desc
limit ?`
	args = append(args, contextLimit)
	extra, err := s.queryMessageRows(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return mergeMessageRows(rows, extra), nil
}

func slackThreadRootTS(row MessageRow) string {
	threadTS := strings.TrimSpace(row.ThreadTS)
	ts := strings.TrimSpace(row.TS)
	if threadTS != "" {
		return threadTS
	}
	if row.ReplyCount > 0 || strings.TrimSpace(row.LatestReply) != "" {
		return ts
	}
	return ""
}

func mergeMessageRows(primary, extra []MessageRow) []MessageRow {
	out := make([]MessageRow, 0, len(primary)+len(extra))
	seen := map[string]struct{}{}
	appendRow := func(row MessageRow) {
		key := row.WorkspaceID + "\x00" + row.ChannelID + "\x00" + row.TS
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, row)
	}
	for _, row := range primary {
		appendRow(row)
	}
	for _, row := range extra {
		appendRow(row)
	}
	return out
}

func (s *Store) resolveMessageRowMentions(ctx context.Context, rows []MessageRow) error {
	if len(rows) == 0 {
		return nil
	}
	byKey := map[providerMessageIdentity][]messageMentionDisplay{}
	keys := make([]providerMessageIdentity, 0, len(rows))
	seen := map[providerMessageIdentity]struct{}{}
	for _, row := range rows {
		key := providerMessageIdentity{channelID: row.ChannelID, ts: row.TS}
		if strings.TrimSpace(key.channelID) == "" && strings.TrimSpace(key.ts) == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for start := 0; start < len(keys); start += 400 {
		end := min(start+400, len(keys))
		predicate, args := providerMessageKeyPredicate(keys[start:end])
		query := `
select mm.channel_id, mm.ts, mm.target_id,
       coalesce(nullif(u.display_name, ''), nullif(u.real_name, ''), nullif(u.name, ''), nullif(mm.display_text, ''), '')
from message_mentions mm
left join users u on u.id = mm.target_id
where mm.mention_type = 'user'
  and mm.deleted_at is null
  and (` + predicate + `)
`
		mentionRows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		for mentionRows.Next() {
			var channelID, ts, target, display string
			if err := mentionRows.Scan(&channelID, &ts, &target, &display); err != nil {
				_ = mentionRows.Close()
				return err
			}
			display = strings.TrimSpace(display)
			target = strings.TrimSpace(target)
			if target == "" || display == "" || strings.EqualFold(display, target) {
				continue
			}
			key := providerMessageIdentity{channelID: channelID, ts: ts}
			byKey[key] = append(byKey[key], messageMentionDisplay{target: target, display: display})
		}
		if err := mentionRows.Err(); err != nil {
			_ = mentionRows.Close()
			return err
		}
		if err := mentionRows.Close(); err != nil {
			return err
		}
	}
	for index := range rows {
		key := providerMessageIdentity{channelID: rows[index].ChannelID, ts: rows[index].TS}
		mentions := byKey[key]
		if len(mentions) == 0 {
			continue
		}
		rows[index].NormalizedText = replaceUserMentions(rows[index].NormalizedText, mentions)
	}
	return nil
}

func replaceUserMentions(value string, mentions []messageMentionDisplay) string {
	value = strings.TrimSpace(value)
	if value == "" || len(mentions) == 0 {
		return value
	}
	ordered := append([]messageMentionDisplay(nil), mentions...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return len(ordered[i].target) > len(ordered[j].target)
	})
	for _, mention := range ordered {
		target := strings.TrimSpace(mention.target)
		display := strings.TrimSpace(mention.display)
		if target == "" || display == "" || strings.EqualFold(target, display) {
			continue
		}
		if !strings.HasPrefix(display, "@") {
			display = "@" + display
		}
		value = mentionTokenRegexp(target).ReplaceAllString(value, display)
		value = strings.ReplaceAll(value, "@"+target, display)
		value = strings.ReplaceAll(value, "@"+strings.ToLower(target), display)
	}
	return value
}

// mentionTokenRegexps caches per-target regexps: replaceUserMentions runs once
// per rendered row times its mentions, and recompiling the same target pattern
// dominated large listing renders. Target IDs are a small, stable set.
var (
	mentionTokenRegexpsMu sync.Mutex
	mentionTokenRegexps   = map[string]*regexp.Regexp{}
)

func mentionTokenRegexp(target string) *regexp.Regexp {
	mentionTokenRegexpsMu.Lock()
	defer mentionTokenRegexpsMu.Unlock()
	if re, ok := mentionTokenRegexps[target]; ok {
		return re
	}
	re := regexp.MustCompile(`<@` + regexp.QuoteMeta(target) + `(?:\|[^>]+)?>`)
	mentionTokenRegexps[target] = re
	return re
}

func (s *Store) Mentions(ctx context.Context, workspaceID string, target string, limit int) ([]MentionRow, error) {
	rows, err := s.q.ListMentions(ctx, storedb.ListMentionsParams{
		WorkspaceID: workspaceID,
		Target:      target,
		TargetLike:  dbText("%" + target + "%"),
		Limit:       int64(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]MentionRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, MentionRow{
			WorkspaceID: row.WorkspaceID,
			ChannelID:   row.ChannelID,
			TS:          row.Ts,
			MentionType: row.MentionType,
			TargetID:    row.TargetID,
			DisplayText: row.DisplayText,
		})
	}
	return out, nil
}

func (s *Store) Users(ctx context.Context, workspaceID string, query string, limit int) ([]UserRow, error) {
	rows, err := s.q.ListUsers(ctx, storedb.ListUsersParams{
		WorkspaceID: workspaceID,
		Query:       query,
		QueryLike:   "%" + query + "%",
		Limit:       int64(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]UserRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, UserRow{
			WorkspaceID: row.WorkspaceID,
			ID:          row.ID,
			Name:        row.Name,
			RealName:    row.RealName,
			DisplayName: row.DisplayName,
			Title:       row.Title,
		})
	}
	return out, nil
}

func (s *Store) Channels(ctx context.Context, workspaceID string, query string, limit int) ([]ChannelRow, error) {
	return s.ChannelsByKind(ctx, workspaceID, query, "", limit)
}

func (s *Store) ChannelsByKind(ctx context.Context, workspaceID string, query string, kind string, limit int) ([]ChannelRow, error) {
	rows, err := s.q.ListChannelsByKind(ctx, storedb.ListChannelsByKindParams{
		WorkspaceID: workspaceID,
		Query:       query,
		QueryLike:   "%" + query + "%",
		Kind:        kind,
		Limit:       int64(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]ChannelRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, ChannelRow{
			WorkspaceID: row.WorkspaceID,
			ID:          row.ID,
			Name:        row.Name,
			Kind:        row.Kind,
		})
	}
	return out, nil
}

func scanMessageRows(rows *sql.Rows) ([]MessageRow, error) {
	var out []MessageRow
	for rows.Next() {
		var row MessageRow
		if err := rows.Scan(&row.WorkspaceID, &row.WorkspaceName, &row.ChannelID, &row.ChannelName, &row.TS, &row.UserID, &row.UserName, &row.Text, &row.NormalizedText, &row.ThreadTS, &row.ReplyCount, &row.LatestReply, &row.Subtype, &row.SourceName); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) queryMessageRows(ctx context.Context, query string, args ...any) ([]MessageRow, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out, err := scanMessageRows(rows)
	if err != nil {
		return nil, err
	}
	return out, s.resolveMessageRowMentions(ctx, out)
}
