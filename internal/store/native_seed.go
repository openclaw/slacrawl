package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	crawlstore "github.com/openclaw/crawlkit/store"
)

type NativeSeedLimits struct {
	MaxRows  int64
	MaxBytes int64
}

type NativeSeedRequest struct {
	File        NativeFileIdentity
	Scope       NativeCaptureScope
	ScopeSHA256 string
	Limits      NativeSeedLimits
}

type NativeSeedTable struct {
	Table   NativeTable
	Columns []string
	Rows    [][]NativeValue
}

// NativeSeed is local current-state data, not a wire payload or a qualified
// delivery baseline. It includes native cache paths and opaque private data.
type NativeSeed struct {
	File        NativeFileIdentity
	ScopeSHA256 string
	Through     NativeCursor
	Tables      []NativeSeedTable
}

// CaptureNativeSeed reads all six content tables in one bounded snapshot. It
// records the real journal head, including zero, without fabricating progress.
// The archive owner must keep the pathname bound to one physical file during
// the call, with the same stable-path precondition as CaptureNativeChanges.
func CaptureNativeSeed(ctx context.Context, path string, request NativeSeedRequest) (NativeSeed, error) {
	if _, ok := ctx.Deadline(); !ok {
		return NativeSeed{}, errors.New("native seed requires a context deadline")
	}
	if err := ctx.Err(); err != nil {
		return NativeSeed{}, err
	}
	canonical, err := canonicalNativeScope(request.Scope)
	if err != nil || request.ScopeSHA256 != nativeDigest(canonical) {
		return NativeSeed{}, ErrNativeScope
	}
	request.Scope = canonical
	if request.Limits.MaxRows <= 0 || request.Limits.MaxBytes <= 0 {
		return NativeSeed{}, ErrNativeLimit
	}
	identity, err := NativeArchiveFileIdentity(path)
	if err != nil || identity != request.File {
		return NativeSeed{}, ErrNativeBaseline
	}
	source, err := crawlstore.OpenReadOnly(ctx, path)
	if err != nil {
		return NativeSeed{}, errors.New("cannot open native seed source read-only")
	}
	defer source.Close()
	tx, err := source.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return NativeSeed{}, err
	}
	defer tx.Rollback()
	seed, err := readNativeSeed(ctx, tx, request)
	if err != nil {
		return NativeSeed{}, err
	}
	after, err := NativeArchiveFileIdentity(path)
	if err != nil || after != identity {
		return NativeSeed{}, ErrNativeBaseline
	}
	if err := tx.Commit(); err != nil {
		return NativeSeed{}, err
	}
	if err := source.Close(); err != nil {
		return NativeSeed{}, err
	}
	if err := ctx.Err(); err != nil {
		return NativeSeed{}, err
	}
	seed.File = identity
	return seed, nil
}

type nativeSeedPlan struct {
	table   nativeChangeTable
	row     nativeCaptureRowPlan
	suffix  string
	args    []any
	count   int64
	columns int64
}

func readNativeSeed(ctx context.Context, tx *sql.Tx, request NativeSeedRequest) (NativeSeed, error) {
	var version int
	if err := tx.QueryRowContext(ctx, "pragma user_version").Scan(&version); err != nil {
		return NativeSeed{}, err
	}
	if version != schemaVersion {
		return NativeSeed{}, ErrNativeBaseline
	}
	status, err := readNativeChangeStatus(ctx, tx)
	if err != nil {
		return NativeSeed{}, ErrNativeBaseline
	}
	budget := nativeCaptureBudget{request.Limits.MaxBytes}
	if err := validateCaptureAdmission(ctx, tx, request.Scope, &budget); err != nil {
		return NativeSeed{}, err
	}
	seed := NativeSeed{ScopeSHA256: request.ScopeSHA256,
		Through: NativeCursor{status.GenerationID, status.HeadSequence, status.HeadEntryID}}
	// Preflight every table before selecting any bodies. Loading rechecks the
	// actual count and scalar bytes; both passes share this read snapshot.
	estimate, remainingRows := budget, request.Limits.MaxRows
	var plans []nativeSeedPlan
	for _, table := range nativeChangeTables {
		if table.name == string(NativeSyncState) {
			continue
		}
		plan := nativeSeedPlan{table: table}
		plan.row, err = prepareNativeCaptureRow(ctx, tx, table)
		if err != nil {
			return NativeSeed{}, err
		}
		for _, column := range plan.row.columns {
			plan.columns += int64(len(column))
		}
		plan.suffix, plan.args = nativeSeedSelection(table, request.Scope)
		var bytes int64
		if err := tx.QueryRowContext(ctx, "select count(*),coalesce(sum("+plan.row.sizeSQL+"),0)"+plan.suffix, plan.args...).Scan(&plan.count, &bytes); err != nil {
			return NativeSeed{}, err
		}
		if plan.count > remainingRows {
			return NativeSeed{}, ErrNativeLimit
		}
		remainingRows -= plan.count
		if err := estimate.take(plan.columns); err != nil {
			return NativeSeed{}, err
		}
		if err := estimate.take(bytes); err != nil {
			return NativeSeed{}, err
		}
		plans = append(plans, plan)
	}
	remainingRows = request.Limits.MaxRows
	for _, plan := range plans {
		if err := budget.take(plan.columns); err != nil {
			return NativeSeed{}, err
		}
		table, err := loadNativeSeedTable(ctx, tx, plan, &budget, &remainingRows)
		if err != nil {
			return NativeSeed{}, err
		}
		seed.Tables = append(seed.Tables, table)
	}
	return seed, nil
}

func nativeSeedSelection(table nativeChangeTable, scope NativeCaptureScope) (string, []any) {
	where := "workspace_id = ?"
	args := []any{scope.WorkspaceID}
	switch NativeTable(table.name) {
	case NativeWorkspaces:
		where = "id = ?"
	case NativeUsers:
		if !scope.IncludeWorkspaceUsers {
			where = "workspace_id = ? and 0"
		}
	default:
		column := "channel_id"
		if table.name == string(NativeChannels) {
			column = "id"
		}
		selected := column + " in (" + strings.TrimSuffix(strings.Repeat("?,", len(scope.Channels)), ",") + ")"
		if table.name == string(NativeMessageMentions) {
			where = `not exists (select 1 from messages m where m.channel_id = message_mentions.channel_id and m.ts = message_mentions.ts and m.workspace_id <> ?)`
		}
		where += " and " + selected
		for _, channel := range scope.Channels {
			args = append(args, channel.ChannelID)
		}
	}
	return " from " + nativeSQLIdentifier(table.name) + " where " + where, args
}

func loadNativeSeedTable(ctx context.Context, tx *sql.Tx, plan nativeSeedPlan, budget *nativeCaptureBudget, remainingRows *int64) (NativeSeedTable, error) {
	table := NativeSeedTable{Table: NativeTable(plan.table.name), Columns: plan.row.columns, Rows: [][]NativeValue{}}
	var order []string
	for _, key := range plan.table.keys {
		order = append(order, nativeSQLIdentifier(key)+" collate binary")
	}
	// Nullable native primary keys can have multiple rows. The local tie-breaker
	// preserves them without exposing rowid as a service identity.
	order = append(order, "rowid")
	rows, err := tx.QueryContext(ctx, "select *"+plan.suffix+" order by "+strings.Join(order, ","), plan.args...)
	if err != nil {
		return NativeSeedTable{}, err
	}
	defer rows.Close()
	values, ptrs := make([]any, len(table.Columns)), make([]any, len(table.Columns))
	for i := range values {
		ptrs[i] = &values[i]
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return NativeSeedTable{}, err
		}
		if *remainingRows <= 0 {
			return NativeSeedTable{}, ErrNativeLimit
		}
		*remainingRows--
		if err := rows.Scan(ptrs...); err != nil {
			return NativeSeedTable{}, err
		}
		decoded, size, err := decodeNativeValues(values)
		if err != nil {
			return NativeSeedTable{}, err
		}
		if err := budget.take(size); err != nil {
			return NativeSeedTable{}, err
		}
		table.Rows = append(table.Rows, decoded)
	}
	if err := rows.Err(); err != nil {
		return NativeSeedTable{}, err
	}
	if int64(len(table.Rows)) != plan.count {
		return NativeSeedTable{}, ErrNativeData
	}
	return table, nil
}
