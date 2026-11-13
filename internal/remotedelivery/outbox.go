package remotedelivery

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/openclaw/crawlkit/remote"
	crawlstore "github.com/openclaw/crawlkit/store"
	"github.com/openclaw/slacrawl/internal/store"
)

var (
	ErrPendingCapture   = errors.New("outbox has a pending capture; resume its persisted parts")
	ErrOutboxBaseline   = errors.New("outbox baseline differs; reconcile without replacing pending data")
	ErrOutboxLimit      = errors.New("outbox capture exceeds its encoded byte or part limit")
	ErrNoNativeProgress = errors.New("outbox capture has no native progress")
)

// Registration is explicitly qualified by its caller, never inferred from a
// native archive or service response. It contains no credential or source path.
type Registration struct {
	BindingID, Origin, App, Archive, Profile string
	Baseline                                 store.NativeCaptureBaseline
	ServerCursor                             string
}

// BaselineQualified identifies an explicitly qualified delivery baseline.
const BaselineQualified = "qualified"
const BaselineUnqualified = "unqualified"

type OutboxState struct {
	BaselineState    string
	PendingKind      string
	BaselineHold     string
	seedScope        []byte
	Registration     Registration
	Baseline         store.NativeCaptureBaseline
	ServerCursor     string // last completed capture, not an intermediate part
	PendingCaptureID string
}

type StageLimits struct{ MaxEncodedBytes int64 }
type PartRef struct {
	CaptureID string
	Index     int
}
type PendingPart struct {
	Ref       PartRef
	Body      []byte
	SHA256    string
	Kind      string
	PartCount int
}

// Outbox owns a separate private SQLite file. Its owner must keep the pathname
// bound to that file while open, as with native capture. There is no rebinding,
// pruning, source writing or credential storage.
type Outbox struct{ base *crawlstore.Store }

// InitOutbox exclusively creates a new file; a failed initialization retains it
// for diagnosis. It never opens an existing database writable or infers a seed.
func InitOutbox(ctx context.Context, path string, registration Registration) (*Outbox, error) {
	if err := validateRegistration(registration); err != nil {
		return nil, err
	}
	anchor, _ := encodeNativeAnchor(registration.Baseline.Cursor)
	return createOutbox(ctx, path, registration, BaselineQualified, &anchor, nil)
}

func createOutbox(ctx context.Context, path string, registration Registration, baselineState string, anchor *string, scope []byte) (*Outbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := literalOutboxPath(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	base, err := crawlstore.Open(ctx, crawlstore.Options{Path: path})
	if err != nil {
		return nil, err
	}
	o := &Outbox{base: base}
	err = o.write(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, outboxSchema); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `insert into binding values(1,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			registration.BindingID, registration.Origin, registration.App, registration.Archive, registration.Profile,
			registration.Baseline.ScopeSHA256, registration.Baseline.File.Platform, registration.Baseline.File.VolumeID, registration.Baseline.File.FileID,
			anchor, registration.ServerCursor, anchor, registration.ServerCursor, baselineState, scope)
		return err
	})
	if err != nil {
		_ = base.Close()
		return nil, err
	}
	return o, nil
}

// OpenOutbox recognizes an existing file read-only before any writable helper
// can alter permissions or journal mode. Recognized v1 outboxes are upgraded
// transactionally; unknown/native archives are refused.
func OpenOutbox(ctx context.Context, path string) (*Outbox, error) {
	return openOutbox(ctx, path, true)
}

// OpenCurrentOutbox refuses older schemas before opening writable. Explicit
// operator delivery commands must not silently upgrade an existing outbox.
func OpenCurrentOutbox(ctx context.Context, path string) (*Outbox, error) {
	return openOutbox(ctx, path, false)
}

func openOutbox(ctx context.Context, path string, allowUpgrade bool) (*Outbox, error) {
	if err := literalOutboxPath(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("outbox path must be a regular file")
	}
	readonly, err := crawlstore.OpenReadOnly(ctx, path)
	if err != nil {
		return nil, err
	}
	var version int
	err = readonly.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		version, err = outboxVersion(ctx, tx)
		if err != nil {
			return err
		}
		if version != outboxSchemaVersion && !allowUpgrade {
			return errors.New("outbox schema requires an explicit upgrade outside delivery commands")
		}
		state, err := readOutboxStateVersion(ctx, tx, version)
		if err != nil {
			return err
		}
		if err := validateOutboxRegistration(state); err != nil {
			return err
		}
		// Prepare the parts contract even when there is no pending capture.
		_, err = tx.ExecContext(ctx, `select capture_id,part_index,body,payload_sha256,receipt_json from parts limit 0`)
		return err
	})
	closeErr := readonly.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	base, err := crawlstore.Open(ctx, crawlstore.Options{Path: path})
	if err != nil {
		return nil, err
	}
	o := &Outbox{base: base}
	if version == 1 {
		if err := o.upgrade(ctx); err != nil {
			_ = base.Close()
			return nil, err
		}
	}
	return o, nil
}

func literalOutboxPath(path string) error {
	if !filepath.IsAbs(path) || strings.HasPrefix(path, "file:") || strings.ContainsRune(path, 0) {
		return errors.New("outbox requires a literal absolute file path")
	}
	return nil
}

func (o *Outbox) Close() error { return o.base.Close() }

func (o *Outbox) State(ctx context.Context) (OutboxState, error) {
	var state OutboxState
	err := o.base.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		state, err = readOutboxState(ctx, tx)
		return err
	})
	return state, err
}

type outboxQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readOutboxState(ctx context.Context, q outboxQuery) (OutboxState, error) {
	return readOutboxStateVersion(ctx, q, outboxSchemaVersion)
}

func readOutboxStateVersion(ctx context.Context, q outboxQuery, version int) (OutboxState, error) {
	var state OutboxState
	r := &state.Registration
	var initial, saved sql.NullString
	baselineState, captureKind, seedScope, hold := "baseline_state", "kind", "seed_scope_json", "baseline_hold"
	if version == 1 {
		baselineState, captureKind, seedScope, hold = "'qualified'", "'changes'", "null", "''"
	}
	err := q.QueryRowContext(ctx, `select binding_id,origin,app,archive,profile,scope_sha256,file_platform,file_volume,file_id,initial_native,initial_server_cursor,saved_native,server_cursor,`+baselineState+`,`+seedScope+` from binding where singleton=1`).Scan(
		&r.BindingID, &r.Origin, &r.App, &r.Archive, &r.Profile, &r.Baseline.ScopeSHA256, &r.Baseline.File.Platform, &r.Baseline.File.VolumeID, &r.Baseline.File.FileID,
		&initial, &r.ServerCursor, &saved, &state.ServerCursor, &state.BaselineState, &state.seedScope)
	if err != nil {
		return OutboxState{}, err
	}
	state.Baseline = r.Baseline
	switch state.BaselineState {
	case BaselineQualified:
		if !initial.Valid || !saved.Valid {
			return OutboxState{}, ErrOutboxBaseline
		}
		r.Baseline.Cursor, err = decodeNativeAnchor(initial.String)
		if err != nil {
			return OutboxState{}, err
		}
		state.Baseline.Cursor, err = decodeNativeAnchor(saved.String)
		if err != nil {
			return OutboxState{}, err
		}
		if state.Baseline.Cursor.GenerationID != r.Baseline.Cursor.GenerationID || state.Baseline.Cursor.Sequence < r.Baseline.Cursor.Sequence {
			return OutboxState{}, ErrOutboxBaseline
		}
	case BaselineUnqualified:
		if initial.Valid || saved.Valid || r.ServerCursor != "" || state.ServerCursor != "" || state.seedScope == nil {
			return OutboxState{}, ErrOutboxBaseline
		}
	default:
		return OutboxState{}, ErrOutboxBaseline
	}
	err = q.QueryRowContext(ctx, `select capture_id,`+captureKind+`,`+hold+` from captures where completed=0`).Scan(&state.PendingCaptureID, &state.PendingKind, &state.BaselineHold)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return OutboxState{}, err
	}
	if state.PendingCaptureID != "" && ((state.PendingKind == "changes" && state.BaselineState != BaselineQualified) || (state.PendingKind == "seed" && state.BaselineState != BaselineUnqualified)) {
		return OutboxState{}, ErrOutboxBaseline
	}
	return state, nil
}

// write serializes point-of-effect checks across processes. FULL is set on the
// actual checked-out connection, overriding Crawlkit's NORMAL default, before
// each transaction. No transaction survives an API return.
func (o *Outbox) write(ctx context.Context, fn func(*sql.Conn) error) error {
	conn, err := o.base.DB().Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `pragma synchronous=FULL`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `begin immediate`); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `rollback`) }()
	if err := fn(conn); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `commit`)
	return err
}

// Stage projects a materialized native capture, builds all parts outside the
// write transaction, then rechecks its baseline before persisting the complete
// capture atomically. An uncertain commit is resolved with State/NextPart.
func (o *Outbox) Stage(ctx context.Context, capture store.NativeCapture, limits StageLimits) (string, error) {
	state, err := o.State(ctx)
	if err != nil {
		return "", err
	}
	if state.PendingCaptureID != "" {
		return "", ErrPendingCapture
	}
	if state.BaselineState != BaselineQualified || capture.File != state.Baseline.File || capture.ScopeSHA256 != state.Baseline.ScopeSHA256 || capture.From != state.Baseline.Cursor || capture.Through.GenerationID != capture.From.GenerationID {
		return "", ErrOutboxBaseline
	}
	if capture.Through.Sequence == capture.From.Sequence {
		return "", ErrNoNativeProgress
	}
	if capture.Through.Sequence < capture.From.Sequence {
		return "", ErrOutboxBaseline
	}
	from, err := encodeNativeAnchor(capture.From)
	if err != nil {
		return "", err
	}
	through, err := encodeNativeAnchor(capture.Through)
	if err != nil {
		return "", err
	}
	changes, err := ProjectPrivateArchive(capture)
	if err != nil {
		return "", err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(random[:])
	parts, err := encodeOutboxParts(state, id, from, through, changes, limits)
	if err != nil {
		return "", err
	}
	err = o.write(ctx, func(conn *sql.Conn) error {
		current, err := readOutboxState(ctx, conn)
		if err != nil {
			return err
		}
		if current.PendingCaptureID != "" {
			return ErrPendingCapture
		}
		if current.Registration != state.Registration || current.Baseline != state.Baseline || current.ServerCursor != state.ServerCursor {
			return ErrOutboxBaseline
		}
		if _, err := conn.ExecContext(ctx, `insert into captures(capture_id,native_from,native_through,initial_expected_cursor,part_count) values(?,?,?,?,?)`, id, from, through, state.ServerCursor, len(parts)); err != nil {
			return err
		}
		for i, body := range parts {
			if _, err := conn.ExecContext(ctx, `insert into parts(capture_id,part_index,body,payload_sha256) values(?,?,?,?)`, id, i, body, bodySHA256(body)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// NextPart returns persisted bytes after releasing its read transaction. More
// than one dispatcher may receive the same part; the receiver owns idempotency.
func (o *Outbox) NextPart(ctx context.Context) (*PendingPart, error) {
	var part *PendingPart
	err := o.base.WithTx(ctx, func(tx *sql.Tx) error {
		state, err := readOutboxState(ctx, tx)
		if err != nil || state.PendingCaptureID == "" {
			return err
		}
		part = &PendingPart{Ref: PartRef{CaptureID: state.PendingCaptureID}, Kind: state.PendingKind}
		if err := tx.QueryRowContext(ctx, `select p.part_index,p.body,p.payload_sha256,c.part_count from parts p join captures c using(capture_id) where p.capture_id=? and p.receipt_json is null order by p.part_index limit 1`, part.Ref.CaptureID).Scan(&part.Ref.Index, &part.Body, &part.SHA256, &part.PartCount); err != nil {
			return err
		}
		if state.BaselineHold != "" && part.Ref.Index == part.PartCount-1 {
			return ErrSeedBaselineHeld
		}
		if bodySHA256(part.Body) != part.SHA256 {
			return errors.New("persisted outbox part hash differs")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return part, nil
}

// RecordReceipt validates the receipt at its durable effect. The last receipt,
// capture completion and completed native/service cursors share one commit.
func (o *Outbox) RecordReceipt(ctx context.Context, ref PartRef, receipt remote.ChangeReceipt) error {
	return o.write(ctx, func(conn *sql.Conn) error {
		state, err := readOutboxState(ctx, conn)
		if err != nil {
			return err
		}
		var digest, through, expected, kind, hold string
		var from sql.NullString
		var recorded []byte
		var count int
		if err := conn.QueryRowContext(ctx, `select p.payload_sha256,p.receipt_json,c.native_from,c.native_through,c.initial_expected_cursor,c.part_count,c.kind,c.baseline_hold from parts p join captures c using(capture_id) where p.capture_id=? and p.part_index=?`, ref.CaptureID, ref.Index).Scan(&digest, &recorded, &from, &through, &expected, &count, &kind, &hold); err != nil {
			return err
		}
		batchID := outboxBatchID(kind, ref.CaptureID, ref.Index, count)
		if kind == "seed" && hold != "" && ref.Index == count-1 {
			return ErrSeedBaselineHeld
		}
		sequence, err := strconv.ParseInt(receipt.Sequence, 10, 64)
		if err != nil || sequence <= 0 || strconv.FormatInt(sequence, 10) != receipt.Sequence || receipt.App != state.Registration.App || receipt.Archive != state.Registration.Archive || receipt.BindingID != state.Registration.BindingID || receipt.BatchID != batchID || receipt.Cursor != batchID || receipt.PayloadSHA256 != digest {
			return errors.New("receipt does not match the persisted outbox part")
		}
		if recorded != nil {
			var prior remote.ChangeReceipt
			if err := json.Unmarshal(recorded, &prior); err != nil {
				return err
			}
			prior.Replayed = receipt.Replayed
			if prior != receipt {
				return errors.New("receipt differs from the recorded receipt")
			}
			return nil
		}
		if state.PendingCaptureID != ref.CaptureID {
			return ErrOutboxBaseline
		}
		var next int
		if err := conn.QueryRowContext(ctx, `select min(part_index) from parts where capture_id=? and receipt_json is null`, ref.CaptureID).Scan(&next); err != nil {
			return err
		}
		if next != ref.Index {
			return errors.New("outbox requires receipts in part order")
		}
		body, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `update parts set receipt_json=? where capture_id=? and part_index=?`, body, ref.CaptureID, ref.Index); err != nil {
			return err
		}
		if ref.Index != count-1 {
			return nil
		}
		var received int
		if err := conn.QueryRowContext(ctx, `select count(*) from parts where capture_id=? and receipt_json is not null`, ref.CaptureID).Scan(&received); err != nil {
			return err
		}
		if received != count {
			return errors.New("outbox capture is missing parts")
		}
		if _, err := conn.ExecContext(ctx, `update captures set completed=1 where capture_id=?`, ref.CaptureID); err != nil {
			return err
		}
		var result sql.Result
		if kind == "seed" {
			result, err = conn.ExecContext(ctx, `update binding set baseline_state='qualified',initial_native=?,initial_server_cursor=?,saved_native=?,server_cursor=? where singleton=1 and baseline_state='unqualified' and server_cursor=?`, through, receipt.Cursor, through, receipt.Cursor, expected)
		} else {
			result, err = conn.ExecContext(ctx, `update binding set saved_native=?,server_cursor=? where singleton=1 and baseline_state='qualified' and saved_native=? and server_cursor=?`, through, receipt.Cursor, from, expected)
		}
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrOutboxBaseline
		}
		return nil
	})
}

func validateRegistrationTarget(r Registration) error {
	u, err := url.Parse(r.Origin)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("outbox requires an explicit service origin without credentials, query or fragment")
	}
	if r.App != "slacrawl" || r.Profile != PrivateArchiveProfile {
		return errors.New("unsupported outbox app or private profile")
	}
	if !lowerHex(r.Baseline.ScopeSHA256, 32) || r.Baseline.File.Platform == "" || r.Baseline.File.VolumeID == "" || r.Baseline.File.FileID == "" {
		return ErrOutboxBaseline
	}
	for _, value := range []string{r.Archive, r.BindingID} {
		if !outboxIdentifier(value) {
			return errors.New("invalid outbox archive/binding identifier")
		}
	}
	return nil
}

func outboxIdentifier(value string) bool {
	return value != "" && len(value) <= 128 && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validateRegistration(r Registration) error {
	if err := validateRegistrationTarget(r); err != nil {
		return err
	}
	anchor, err := encodeNativeAnchor(r.Baseline.Cursor)
	if err != nil {
		return err
	}
	_, err = remote.EncodeChangeBatch(remote.ChangeBatch{Contract: remote.ChangeContract, App: r.App, Archive: r.Archive, BindingID: r.BindingID, CaptureID: strings.Repeat("0", 32), PartCount: 1, NativeFrom: anchor, NativeThrough: anchor, ExpectedCursor: r.ServerCursor, Changes: []remote.Change{}})
	return err
}

func encodeNativeAnchor(cursor store.NativeCursor) (string, error) {
	if !lowerHex(cursor.GenerationID, 16) || !lowerHex(cursor.EntryID, 16) || cursor.Sequence < 0 {
		return "", ErrOutboxBaseline
	}
	return "slacrawl-native-v1:" + cursor.GenerationID + ":" + strconv.FormatInt(cursor.Sequence, 10) + ":" + cursor.EntryID, nil
}

func decodeNativeAnchor(value string) (store.NativeCursor, error) {
	fields := strings.Split(value, ":")
	if len(fields) != 4 || fields[0] != "slacrawl-native-v1" {
		return store.NativeCursor{}, ErrOutboxBaseline
	}
	sequence, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return store.NativeCursor{}, ErrOutboxBaseline
	}
	cursor := store.NativeCursor{GenerationID: fields[1], Sequence: sequence, EntryID: fields[3]}
	encoded, err := encodeNativeAnchor(cursor)
	if err != nil || encoded != value {
		return store.NativeCursor{}, ErrOutboxBaseline
	}
	return cursor, nil
}

func lowerHex(value string, bytes int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == bytes && strings.ToLower(value) == value
}

func bodySHA256(body []byte) string { return fmt.Sprintf("%x", sha256.Sum256(body)) }
