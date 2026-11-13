package remotedelivery

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"

	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/store"
)

var ErrSeedBaselineHeld = errors.New("seed keys are ambiguous; retain this attempt and reconcile before qualifying a new source/binding/outbox")

// SeedRegistration is explicit local admission, not a qualified native baseline.
// Full admission facts stay local; only workspace/channel IDs and user opt-in
// enter the wire manifest. The caller owns raw-payload provenance qualification.
type SeedRegistration struct {
	BindingID, Origin, App, Archive, Profile string
	File                                     store.NativeFileIdentity
	Scope                                    store.NativeCaptureScope
}

// InitSeedOutbox creates a private, unqualified registration without opening the
// native archive or inventing an anchor. It uses the same exclusive file owner.
func InitSeedOutbox(ctx context.Context, path string, registration SeedRegistration) (*Outbox, error) {
	digest, err := store.NativeCaptureScopeSHA256(registration.Scope)
	if err != nil {
		return nil, err
	}
	r := Registration{BindingID: registration.BindingID, Origin: registration.Origin, App: registration.App, Archive: registration.Archive, Profile: registration.Profile,
		Baseline: store.NativeCaptureBaseline{File: registration.File, ScopeSHA256: digest}}
	if err := validateRegistrationTarget(r); err != nil {
		return nil, err
	}
	scope := remote.SlackSeedScope{WorkspaceID: registration.Scope.WorkspaceID, IncludeWorkspaceUsers: registration.Scope.IncludeWorkspaceUsers}
	for _, channel := range registration.Scope.Channels {
		scope.ChannelIDs = append(scope.ChannelIDs, channel.ChannelID)
	}
	slices.Sort(scope.ChannelIDs)
	if err := validateSeedScope(scope); err != nil {
		return nil, err
	}
	body, err := json.Marshal(scope)
	if err != nil {
		return nil, err
	}
	return createOutbox(ctx, path, r, BaselineUnqualified, nil, body)
}

func validateSeedScope(scope remote.SlackSeedScope) error {
	if !outboxIdentifier(scope.WorkspaceID) || len(scope.ChannelIDs) == 0 {
		return ErrOutboxBaseline
	}
	for i, channel := range scope.ChannelIDs {
		if !outboxIdentifier(channel) || (i > 0 && channel <= scope.ChannelIDs[i-1]) {
			return ErrOutboxBaseline
		}
	}
	return nil
}

func validateOutboxRegistration(state OutboxState) error {
	if state.BaselineState == BaselineQualified {
		return validateRegistration(state.Registration)
	}
	if err := validateRegistrationTarget(state.Registration); err != nil {
		return err
	}
	var scope remote.SlackSeedScope
	if err := json.Unmarshal(state.seedScope, &scope); err != nil {
		return err
	}
	return validateSeedScope(scope)
}

// StageSeed projects and encodes a complete snapshot outside the write lock,
// then atomically persists all exact request bodies, including completion.
// NULL/duplicate keys are retained but permanently hold this attempt's baseline.
func (o *Outbox) StageSeed(ctx context.Context, seed store.NativeSeed, limits StageLimits) (string, error) {
	state, err := o.State(ctx)
	if err != nil {
		return "", err
	}
	if state.PendingCaptureID != "" {
		return "", ErrPendingCapture
	}
	if state.BaselineState != BaselineUnqualified || seed.File != state.Baseline.File || seed.ScopeSHA256 != state.Baseline.ScopeSHA256 {
		return "", ErrOutboxBaseline
	}
	through, err := encodeNativeAnchor(seed.Through)
	if err != nil {
		return "", err
	}
	if limits.MaxEncodedBytes <= 0 || limits.MaxEncodedBytes > int64(remote.MaxChangeBytes)*(remote.MaxSeedChunks+2) {
		return "", ErrOutboxLimit
	}
	changes, err := ProjectPrivateSeed(seed)
	if err != nil {
		return "", err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(random[:])
	parts, hold, err := encodeSeedParts(state, id, through, changes, limits)
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
		if current.BaselineState != BaselineUnqualified || current.Registration != state.Registration {
			return ErrOutboxBaseline
		}
		if _, err := conn.ExecContext(ctx, `insert into captures(capture_id,native_from,native_through,initial_expected_cursor,part_count,kind,baseline_hold) values(?,null,?,'',?,'seed',?)`, id, through, len(parts), hold); err != nil {
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

func outboxBatchID(kind, id string, index, count int) string {
	if kind == "changes" {
		return (remote.ChangeBatch{CaptureID: id, PartIndex: index}).BatchID()
	}
	suffix := strconv.Itoa(index - 1)
	if index == 0 {
		suffix = "begin"
	} else if index == count-1 {
		suffix = "complete"
	}
	return "seed1:" + id + ":" + suffix
}

func encodeSeedParts(state OutboxState, id, through string, changes []remote.Change, limits StageLimits) ([][]byte, string, error) {
	r := state.Registration
	manifest := remote.SlackSeedManifest{Contract: remote.SlackSeedContract, App: r.App, Archive: r.Archive, BindingID: r.BindingID, SeedID: id,
		Profile: r.Profile, NativeSchema: 9, NativeThrough: through}
	if err := json.Unmarshal(state.seedScope, &manifest.Scope); err != nil {
		return nil, "", err
	}
	parts := [][]byte{nil} // manifest follows chunk hashes, without a hash cycle
	var total int64
	hold := ""
	for _, layout := range privateTables {
		table := remote.SeedTable{Name: string(layout.table), Chunks: []remote.SeedChunkInfo{}}
		hash := sha256.New()
		seen := map[string]bool{}
		rows := 0
		for len(changes) > 0 && changes[0].Table == table.Name {
			chunk := remote.SlackSeedChunk{Contract: manifest.Contract, App: r.App, Archive: r.Archive, BindingID: r.BindingID, SeedID: id,
				Index: len(parts) - 1, ExpectedCursor: outboxBatchID("seed", id, len(parts)-1, remote.MaxSeedChunks+2)}
			// Reserve maximum-width indices and the longer begin/continuation
			// envelope. Shared encoding below remains the final wire validator.
			probe := chunk
			probe.Index = remote.MaxSeedChunks - 1
			probe.ExpectedCursor = outboxBatchID("seed", id, remote.MaxSeedChunks-1, remote.MaxSeedChunks+2)
			probe.Changes = []remote.Change{changes[0]}
			encodedFirst, err := json.Marshal(changes[0])
			if err != nil {
				return nil, "", err
			}
			encodedProbe, err := json.Marshal(probe)
			if err != nil {
				return nil, "", err
			}
			size := len(encodedProbe) - len(encodedFirst) + 1 // begin suffix can be one byte longer
			for len(changes) > 0 && changes[0].Table == table.Name && len(chunk.Changes) < remote.MaxChanges {
				change := changes[0]
				encoded, err := json.Marshal(change)
				if err != nil {
					return nil, "", err
				}
				needed := len(encoded)
				if len(chunk.Changes) > 0 {
					needed++
				}
				if size+needed > remote.MaxChangeBytes {
					if len(chunk.Changes) == 0 {
						return nil, "", ErrOutboxLimit
					}
					break
				}
				key, _ := json.Marshal(change.Key)
				if seen[string(key)] {
					hold = "ambiguous_keys"
				}
				seen[string(key)] = true
				for _, value := range change.Key {
					if value.Kind == "null" {
						hold = "ambiguous_keys"
					}
				}
				size += needed
				chunk.Changes = append(chunk.Changes, change)
				changes = changes[1:]
			}
			if chunk.Index >= remote.MaxSeedChunks {
				return nil, "", ErrOutboxLimit
			}
			body, err := remote.EncodeSlackSeedChunk(chunk)
			if err != nil {
				return nil, "", err
			}
			total += int64(len(body))
			if total > limits.MaxEncodedBytes {
				return nil, "", ErrOutboxLimit
			}
			digest := sha256.Sum256(body)
			_, _ = hash.Write(digest[:])
			table.Chunks = append(table.Chunks, remote.SeedChunkInfo{Rows: len(chunk.Changes), Bytes: len(body), SHA256: hex.EncodeToString(digest[:])})
			rows += len(chunk.Changes)
			parts = append(parts, body)
		}
		table.Rows, table.SHA256 = strconv.Itoa(rows), hex.EncodeToString(hash.Sum(nil))
		manifest.Tables = append(manifest.Tables, table)
	}
	body, err := remote.EncodeSlackSeedManifest(manifest)
	if err != nil {
		return nil, "", err
	}
	parts[0] = body
	complete, err := remote.EncodeSlackSeedComplete(remote.SlackSeedComplete{Contract: manifest.Contract, App: r.App, Archive: r.Archive, BindingID: r.BindingID, SeedID: id,
		ManifestSHA256: bodySHA256(body), ExpectedCursor: outboxBatchID("seed", id, len(parts)-1, len(parts)+1)})
	if err != nil {
		return nil, "", err
	}
	total += int64(len(body) + len(complete))
	if total > limits.MaxEncodedBytes {
		return nil, "", ErrOutboxLimit
	}
	return append(parts, complete), hold, nil
}

// Load one immutable plan per delivery invocation, including after a restart.
// The single SQL read closes before decoding or any HTTP call.
func (o *Outbox) seedPlan(ctx context.Context, state OutboxState) (*remote.SlackSeedPlan, error) {
	var body []byte
	var digest, through string
	var count int
	if err := o.base.DB().QueryRowContext(ctx, `select p.body,p.payload_sha256,c.native_through,c.part_count from parts p join captures c using(capture_id) where p.capture_id=? and p.part_index=0`, state.PendingCaptureID).Scan(&body, &digest, &through, &count); err != nil {
		return nil, err
	}
	if bodySHA256(body) != digest {
		return nil, errors.New("persisted seed manifest hash differs")
	}
	plan, err := remote.DecodeSlackSeedManifest(body)
	if err != nil {
		return nil, err
	}
	m := plan.Manifest()
	r := state.Registration
	parts := 2
	for _, table := range m.Tables {
		parts += len(table.Chunks)
	}
	if m.App != r.App || m.Archive != r.Archive || m.BindingID != r.BindingID || m.Profile != r.Profile || m.SeedID != state.PendingCaptureID || m.NativeThrough != through || parts != count {
		return nil, ErrOutboxBaseline
	}
	return plan, nil
}
