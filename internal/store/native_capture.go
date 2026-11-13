package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	crawlstore "github.com/openclaw/crawlkit/store"
)

var (
	ErrNativeBaseline = errors.New("native capture requires a qualified matching baseline")
	ErrNativeScope    = errors.New("native capture scope is invalid or changed; reconcile its admission")
	ErrNativeLimit    = errors.New("native capture exceeds its bound; reconcile locally without advancing the cursor")
	ErrNativeData     = errors.New("native capture encountered uninterpretable data; reconcile locally")
)

type NativeCursor struct {
	GenerationID string
	Sequence     int64
	EntryID      string
}
type NativeFileIdentity struct{ Platform, VolumeID, FileID string }
type NativeCaptureBaseline struct {
	File        NativeFileIdentity
	ScopeSHA256 string
	Cursor      NativeCursor
}
type NativeCaptureLimits struct {
	MaxEvents int
	MaxBytes  int64
}
type NativeCaptureRequest struct {
	Scope       NativeCaptureScope
	ScopeSHA256 string
	Baseline    NativeCaptureBaseline
	Limits      NativeCaptureLimits
}

type NativeTable string

const (
	NativeWorkspaces      NativeTable = "workspaces"
	NativeChannels        NativeTable = "channels"
	NativeUsers           NativeTable = "users"
	NativeMessages        NativeTable = "messages"
	NativeMessageFiles    NativeTable = "message_files"
	NativeMessageMentions NativeTable = "message_mentions"
	NativeSyncState       NativeTable = "sync_state"
)

type NativeValueKind uint8

const (
	NativeNull NativeValueKind = iota
	NativeText
	NativeInteger
)

type NativeValue struct {
	Kind    NativeValueKind
	Text    string
	Integer int64
}
type NativeKey struct{ Values []NativeValue }
type NativeCaptureRow struct {
	Table   NativeTable
	Key     NativeKey
	Columns []string
	Values  []NativeValue
}
type NativeRemoval struct {
	Table       NativeTable
	Key         NativeKey
	WorkspaceID *string
	Sequence    int64
	EntryID     string
}

// NativeCapture is local source data, NOT a service payload. Rows can contain raw
// payloads and local media/cache paths. A future egress owner must project them.
// File identity and archive paths must never be serialized into that payload.
type NativeCapture struct {
	File          NativeFileIdentity
	ScopeSHA256   string
	From, Through NativeCursor
	Rows          []NativeCaptureRow
	Removals      []NativeRemoval
	EventsRead    int
}

// NativeArchiveFileIdentity observes physical identity only. It does not create
// or qualify a baseline, establish scope authority, or open/migrate SQLite.
func NativeArchiveFileIdentity(path string) (NativeFileIdentity, error) {
	if !filepath.IsAbs(path) {
		return NativeFileIdentity{}, ErrNativeBaseline
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return NativeFileIdentity{}, ErrNativeBaseline
	}
	identity, ok := nativePhysicalIdentity(info)
	if !ok {
		return NativeFileIdentity{}, ErrNativeBaseline
	}
	return identity, nil
}

// CaptureNativeChanges owns one read transaction and returns a bounded,
// materialized observation. No acquisition or delivery progress is written.
func CaptureNativeChanges(ctx context.Context, path string, request NativeCaptureRequest) (NativeCapture, error) {
	if _, ok := ctx.Deadline(); !ok {
		return NativeCapture{}, errors.New("native capture requires a context deadline")
	}
	if err := ctx.Err(); err != nil {
		return NativeCapture{}, err
	}
	canonical, err := canonicalNativeScope(request.Scope)
	if err != nil {
		return NativeCapture{}, err
	}
	request.Scope = canonical
	if request.Limits.MaxEvents <= 0 || request.Limits.MaxBytes <= 0 {
		return NativeCapture{}, ErrNativeLimit
	}
	if request.ScopeSHA256 != nativeDigest(canonical) || request.Baseline.ScopeSHA256 != request.ScopeSHA256 {
		return NativeCapture{}, ErrNativeScope
	}
	identity, err := NativeArchiveFileIdentity(path)
	if err != nil || identity != request.Baseline.File {
		return NativeCapture{}, ErrNativeBaseline
	}
	source, err := crawlstore.OpenReadOnly(ctx, path)
	if err != nil {
		return NativeCapture{}, errors.New("cannot open native capture source read-only")
	}
	defer source.Close()
	tx, err := source.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return NativeCapture{}, err
	}
	defer tx.Rollback()
	captured, err := readNativeCapture(ctx, tx, request)
	if err != nil {
		return NativeCapture{}, err
	}
	after, err := NativeArchiveFileIdentity(path)
	if err != nil || after != identity {
		return NativeCapture{}, ErrNativeBaseline
	}
	if err := tx.Commit(); err != nil {
		return NativeCapture{}, err
	}
	if err := source.Close(); err != nil {
		return NativeCapture{}, err
	}
	if err := ctx.Err(); err != nil {
		return NativeCapture{}, err
	}
	captured.File = identity
	return captured, nil
}

type nativeCaptureBudget struct{ remaining int64 }

func (b *nativeCaptureBudget) take(size int64) error {
	if size < 0 || size > b.remaining {
		return ErrNativeLimit
	}
	b.remaining -= size
	return nil
}

type nativeCaptureEvent struct {
	sequence         int64
	entry, operation string
	workspace        *string
	table            nativeChangeTable
	key              NativeKey
}

func readNativeCapture(ctx context.Context, tx *sql.Tx, request NativeCaptureRequest) (NativeCapture, error) {
	var version int
	if err := tx.QueryRowContext(ctx, "pragma user_version").Scan(&version); err != nil {
		return NativeCapture{}, err
	}
	if version != schemaVersion {
		return NativeCapture{}, ErrNativeBaseline
	}
	status, err := readNativeChangeStatus(ctx, tx)
	if err != nil {
		return NativeCapture{}, fmt.Errorf("%w: journal validation failed", ErrNativeBaseline)
	}
	cursor := request.Baseline.Cursor
	if !nativeAnchor(cursor.GenerationID) || !nativeAnchor(cursor.EntryID) || cursor.GenerationID != status.GenerationID || cursor.Sequence < status.FloorSequence || cursor.Sequence > status.HeadSequence {
		return NativeCapture{}, ErrNativeBaseline
	}
	anchor := status.FloorEntryID
	if cursor.Sequence > status.FloorSequence {
		if err := tx.QueryRowContext(ctx, "select entry_id from native_changes where sequence = ?", cursor.Sequence).Scan(&anchor); err != nil {
			return NativeCapture{}, ErrNativeBaseline
		}
	}
	if anchor != cursor.EntryID {
		return NativeCapture{}, ErrNativeBaseline
	}
	budget := nativeCaptureBudget{request.Limits.MaxBytes}
	if err := validateCaptureAdmission(ctx, tx, request.Scope, &budget); err != nil {
		return NativeCapture{}, err
	}
	// Native triggers append without conflict-skipping inserts; rolled-back
	// AUTOINCREMENT values are reused. Every committed interval is contiguous.
	count := status.HeadSequence - cursor.Sequence
	if count > int64(request.Limits.MaxEvents) {
		return NativeCapture{}, ErrNativeLimit
	}
	var bytes int64
	if err := tx.QueryRowContext(ctx, `select coalesce(sum(8 + length(cast(entry_id as blob)) + length(cast(table_name as blob)) + length(cast(operation as blob)) + length(cast(key_json as blob)) + coalesce(length(cast(workspace_id as blob)),0)),0) from native_changes where sequence > ? and sequence <= ?`, cursor.Sequence, status.HeadSequence).Scan(&bytes); err != nil {
		return NativeCapture{}, err
	}
	if err := budget.take(bytes); err != nil {
		return NativeCapture{}, err
	}
	events, err := tx.QueryContext(ctx, `select sequence,entry_id,table_name,operation,key_json,workspace_id from native_changes where sequence > ? and sequence <= ? order by sequence`, cursor.Sequence, status.HeadSequence)
	if err != nil {
		return NativeCapture{}, err
	}
	changes := map[string]nativeCaptureEvent{}
	var order []string
	lastSequence, lastEntry := cursor.Sequence, cursor.EntryID
	for events.Next() {
		var event nativeCaptureEvent
		var table, key string
		if err := events.Scan(&event.sequence, &event.entry, &table, &event.operation, &key, &event.workspace); err != nil {
			_ = events.Close()
			return NativeCapture{}, err
		}
		if event.sequence != lastSequence+1 {
			_ = events.Close()
			return NativeCapture{}, ErrNativeBaseline
		}
		lastSequence, lastEntry = event.sequence, event.entry
		event.table, err = nativeCaptureTable(table)
		if err == nil {
			event.key, err = parseNativeKey(event.table, key)
		}
		if err != nil || !nativeAnchor(event.entry) || (event.operation != "i" && event.operation != "u" && event.operation != "d") {
			_ = events.Close()
			return NativeCapture{}, ErrNativeData
		}
		relevant, err := nativeEventInScope(event, request.Scope)
		if err != nil {
			_ = events.Close()
			return NativeCapture{}, err
		}
		if !relevant {
			continue
		}
		identity := table + "/" + nativeDigest(event.key)
		if _, exists := changes[identity]; !exists {
			order = append(order, identity)
		}
		changes[identity] = event
	}
	err = events.Err()
	_ = events.Close()
	if err != nil {
		return NativeCapture{}, err
	}
	if lastSequence != status.HeadSequence || lastEntry != status.HeadEntryID {
		return NativeCapture{}, ErrNativeBaseline
	}
	result := NativeCapture{ScopeSHA256: request.ScopeSHA256, From: cursor, Through: NativeCursor{status.GenerationID, status.HeadSequence, status.HeadEntryID}, EventsRead: int(count)}
	plans := map[string]nativeCaptureRowPlan{}
	for _, identity := range order {
		if err := ctx.Err(); err != nil {
			return NativeCapture{}, err
		}
		event := changes[identity]
		plan, ok := plans[event.table.name]
		if !ok {
			plan, err = prepareNativeCaptureRow(ctx, tx, event.table)
			if err != nil {
				return NativeCapture{}, err
			}
			plans[event.table.name] = plan
		}
		row, found, err := readNativeCaptureRow(ctx, tx, event.table, plan, event.key, request.Scope, &budget)
		if err != nil {
			return NativeCapture{}, err
		}

		if found {
			if event.operation == "d" {
				return NativeCapture{}, ErrNativeData
			}
			result.Rows = append(result.Rows, row)
		} else {
			if event.operation != "d" {
				return NativeCapture{}, ErrNativeData
			}
			result.Removals = append(result.Removals, NativeRemoval{NativeTable(event.table.name), event.key, event.workspace, event.sequence, event.entry})
		}
	}
	return result, nil
}

func nativeCaptureTable(name string) (nativeChangeTable, error) {
	for _, table := range nativeChangeTables {
		if table.name == name {
			return table, nil
		}
	}
	return nativeChangeTable{}, ErrNativeData
}

func parseNativeKey(table nativeChangeTable, raw string) (NativeKey, error) {
	object, err := nativeJSONObject(raw, false)
	if err != nil || len(object) != len(table.keys) {
		return NativeKey{}, ErrNativeData
	}
	key := NativeKey{}
	for _, name := range table.keys {
		value, ok := object[name]
		if !ok {
			return NativeKey{}, ErrNativeData
		}
		if string(value) == "null" {
			key.Values = append(key.Values, NativeValue{Kind: NativeNull})
			continue
		}
		var text string
		if json.Unmarshal(value, &text) != nil || !utf8.ValidString(text) {
			return NativeKey{}, ErrNativeData
		}
		key.Values = append(key.Values, NativeValue{Kind: NativeText, Text: text})
	}
	return key, nil
}

func captureChannelSelected(scope NativeCaptureScope, id string) bool {
	for _, channel := range scope.Channels {
		if channel.ChannelID == id {
			return true
		}
	}
	return false
}

func nativeEventInScope(event nativeCaptureEvent, scope NativeCaptureScope) (bool, error) {
	values := event.key.Values
	switch NativeTable(event.table.name) {
	case NativeWorkspaces:
		return values[0].Kind == NativeText && values[0].Text == scope.WorkspaceID, nil
	case NativeChannels:
		return captureChannelSelected(scope, values[0].Text), nil
	case NativeUsers:
		if !scope.IncludeWorkspaceUsers {
			return false, nil
		}
	case NativeMessages, NativeMessageFiles, NativeMessageMentions:
		if !captureChannelSelected(scope, values[0].Text) {
			return false, nil
		}
	case NativeSyncState:
		for _, key := range scope.SyncStateKeys {
			if values[0].Text == key.SourceName && values[1].Text == key.EntityType && values[2].Text == key.EntityID {
				return true, nil
			}
		}
		return false, nil
	}
	if event.workspace == nil {
		return false, ErrNativeData
	}
	return *event.workspace == scope.WorkspaceID, nil
}

func validateCaptureAdmission(ctx context.Context, tx *sql.Tx, scope NativeCaptureScope, budget *nativeCaptureBudget) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, "select exists(select 1 from workspaces where id = ?)", scope.WorkspaceID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNativeScope
	}
	for _, binding := range scope.Channels {
		var bytes int64
		err := tx.QueryRowContext(ctx, `select length(cast(id as blob))+length(cast(workspace_id as blob))+length(cast(kind as blob))+length(cast(raw_json as blob))+16 from channels where id = ?`, binding.ChannelID).Scan(&bytes)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNativeScope
		}
		if err != nil {
			return err
		}
		if err := budget.take(bytes); err != nil {
			return err
		}
		var facts NativeChannelFacts
		err = tx.QueryRowContext(ctx, `select id,workspace_id,kind,is_private,is_shared,raw_json from channels where id = ?`, binding.ChannelID).Scan(&facts.ChannelID, &facts.WorkspaceID, &facts.Kind, &facts.IsPrivate, &facts.IsShared, &facts.RawJSON)
		if err != nil {
			return err
		}
		digest, err := NativeChannelAdmissionSHA256(facts)
		if err != nil || facts.WorkspaceID != scope.WorkspaceID || digest != binding.SHA256 {
			return ErrNativeScope
		}
	}
	return nil
}

func nativeSQLIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

type nativeCaptureRowPlan struct {
	columns []string
	sizeSQL string
}

func prepareNativeCaptureRow(ctx context.Context, tx *sql.Tx, table nativeChangeTable) (nativeCaptureRowPlan, error) {
	info, err := tx.QueryContext(ctx, "pragma table_info("+table.name+")")
	if err != nil {
		return nativeCaptureRowPlan{}, err
	}
	plan := nativeCaptureRowPlan{}
	var sizes []string
	for info.Next() {
		var cid, notNull, pk int
		var name, typ string
		var def any
		if err := info.Scan(&cid, &name, &typ, &notNull, &def, &pk); err != nil {
			_ = info.Close()
			return nativeCaptureRowPlan{}, err
		}
		plan.columns = append(plan.columns, name)
		quoted := nativeSQLIdentifier(name)
		sizes = append(sizes, "case typeof("+quoted+") when 'null' then 1 when 'integer' then 9 else 1+length(cast("+quoted+" as blob)) end")
	}
	err = info.Err()
	_ = info.Close()
	if err != nil {
		return nativeCaptureRowPlan{}, err
	}
	if len(sizes) == 0 {
		return nativeCaptureRowPlan{}, ErrNativeData
	}
	plan.sizeSQL = strings.Join(sizes, " + ")
	return plan, nil
}

func readNativeCaptureRow(ctx context.Context, tx *sql.Tx, table nativeChangeTable, plan nativeCaptureRowPlan, key NativeKey, scope NativeCaptureScope, budget *nativeCaptureBudget) (NativeCaptureRow, bool, error) {
	row := NativeCaptureRow{Table: NativeTable(table.name), Key: key, Columns: plan.columns}
	var where []string
	var args []any
	for i, name := range table.keys {
		where = append(where, nativeSQLIdentifier(name)+" is ?")
		if key.Values[i].Kind == NativeNull {
			args = append(args, nil)
		} else {
			args = append(args, key.Values[i].Text)
		}
	}
	// Filter ownership before selecting native bodies. A moved key can produce
	// its observed old-scope removal without reading a foreign row's content.
	if table.workspace == "%s.workspace_id" {
		where = append(where, "workspace_id = ?")
		args = append(args, scope.WorkspaceID)
	}
	if table.name == "message_mentions" {
		where = append(where, `not exists (select 1 from messages m where m.channel_id = message_mentions.channel_id and m.ts = message_mentions.ts and m.workspace_id <> ?)`)
		args = append(args, scope.WorkspaceID)
	}
	suffix := " from " + nativeSQLIdentifier(table.name) + " where " + strings.Join(where, " and ")
	var size int64
	err := tx.QueryRowContext(ctx, "select "+plan.sizeSQL+suffix, args...).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return NativeCaptureRow{}, false, nil
	}
	if err != nil {
		return NativeCaptureRow{}, false, err
	}
	for _, name := range row.Columns {
		size += int64(len(name))
	}
	if err := budget.take(size); err != nil {
		return NativeCaptureRow{}, false, err
	}
	values := make([]any, len(row.Columns))
	ptrs := make([]any, len(values))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if err := tx.QueryRowContext(ctx, "select *"+suffix, args...).Scan(ptrs...); err != nil {
		return NativeCaptureRow{}, false, err
	}
	for _, value := range values {
		scalar := NativeValue{}
		switch typed := value.(type) {
		case nil:
			scalar.Kind = NativeNull
		case int64:
			scalar.Kind = NativeInteger
			scalar.Integer = typed
		case string:
			if !utf8.ValidString(typed) {
				return NativeCaptureRow{}, false, ErrNativeData
			}
			scalar.Kind = NativeText
			scalar.Text = typed
		default:
			return NativeCaptureRow{}, false, ErrNativeData
		}
		row.Values = append(row.Values, scalar)
	}
	return row, true, nil
}
