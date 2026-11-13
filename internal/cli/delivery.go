package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/remotedelivery"
	"github.com/openclaw/slacrawl/internal/store"
)

const deliveryScopeMaxBytes = 1 << 20

// This operator document is local admission, not a service payload. Explicit
// spelling/order follows the existing export selection input contract.
type deliveryScopeDocument struct {
	Version               int                        `json:"version"`
	WorkspaceID           string                     `json:"workspace_id"`
	Channels              []deliveryChannelAdmission `json:"channels"`
	IncludeWorkspaceUsers bool                       `json:"include_workspace_users"`
	SyncStateKeys         []deliverySyncStateKey     `json:"sync_state_keys"`
}
type deliveryChannelAdmission struct {
	ChannelID       string `json:"channel_id"`
	AdmissionSHA256 string `json:"admission_sha256"`
}
type deliverySyncStateKey struct {
	SourceName string `json:"source_name"`
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
}

type deliveryAnchor struct {
	GenerationID string `json:"generation_id"`
	Sequence     string `json:"sequence"`
	EntryID      string `json:"entry_id"`
}
type deliveryOutput struct {
	Status           string          `json:"status"`
	BaselineState    string          `json:"baseline_state,omitempty"`
	CaptureID        string          `json:"capture_id,omitempty"`
	BaselineHold     string          `json:"baseline_hold,omitempty"`
	NativeAnchor     *deliveryAnchor `json:"native_anchor,omitempty"`
	EventsRead       int             `json:"events_read,omitempty"`
	Removals         int             `json:"removals,omitempty"`
	Rows             int64           `json:"rows,omitempty"`
	ReceiptsRecorded int             `json:"receipts_recorded"`
}

func (a *App) runDelivery(ctx context.Context, args []string, format OutputFormat) error {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h")) {
		_, err := io.WriteString(a.Stdout, "Usage: slacrawl delivery <init|stage-seed|stage-changes|deliver> [flags]\n\n  init       --db PATH --scope PATH --outbox NEW_PATH --origin URL --archive ID --binding ID\n  stage-seed --db PATH --scope PATH --outbox PATH --max-rows N --max-native-bytes N --max-encoded-bytes N --timeout D\n  stage-changes --db PATH --scope PATH --outbox PATH --max-events N --max-native-bytes N --max-encoded-bytes N --timeout D\n  deliver    --outbox PATH --max-parts N --timeout D [--token-env NAME]\n\nExplicit private delivery only. No config discovery, source sync or schema upgrade.\n")
		return err
	}
	command := args[0]
	if command != "init" && command != "stage-seed" && command != "stage-changes" && command != "deliver" {
		return errors.New("unknown delivery subcommand: use init, stage-seed, stage-changes, or deliver")
	}
	fs := flag.NewFlagSet("delivery "+command, flag.ContinueOnError)
	outboxPath := fs.String("outbox", "", "absolute private outbox path (required)")
	formatFlag := fs.String("format", string(format), "output format: text|json|log")
	jsonOut := fs.Bool("json", false, "json output")
	var db, scopePath, origin, archive, binding, tokenEnv string
	var maxRows, maxNativeBytes, maxEncodedBytes int64
	var maxParts, maxEvents int
	staging := command == "stage-seed" || command == "stage-changes"
	var timeout time.Duration
	if command != "deliver" {
		fs.StringVar(&db, "db", "", "absolute existing native archive (required)")
		fs.StringVar(&scopePath, "scope", "", "explicit private version-1 scope JSON (required)")
	}
	switch command {
	case "init":
		fs.StringVar(&origin, "origin", "", "registered service origin (required)")
		fs.StringVar(&archive, "archive", "", "registered archive ID (required)")
		fs.StringVar(&binding, "binding", "", "registered binding ID (required)")
	case "stage-seed", "stage-changes":
		if command == "stage-seed" {
			fs.Int64Var(&maxRows, "max-rows", 0, "positive total native row limit (required)")
		} else {
			fs.IntVar(&maxEvents, "max-events", 0, "positive whole native journal interval limit, including excluded events (required)")
		}
		fs.Int64Var(&maxNativeBytes, "max-native-bytes", 0, "positive native scalar/metadata byte limit, not RSS (required)")
		fs.Int64Var(&maxEncodedBytes, "max-encoded-bytes", 0, "positive total encoded request byte limit (required)")
	case "deliver":
		fs.IntVar(&maxParts, "max-parts", 0, "request limit, 1..64 (required)")
		fs.StringVar(&tokenEnv, "token-env", remote.DefaultTokenEnv, "environment variable holding the service token")
	}
	if command != "init" {
		fs.DurationVar(&timeout, "timeout", 0, "positive operation deadline, e.g. 30s (required)")
	}
	if err := a.parseCommandFlags(fs, args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("delivery commands do not accept positional arguments")
	}
	if *outboxPath == "" || (command != "deliver" && (db == "" || scopePath == "")) {
		return errors.New("delivery requires explicit --outbox and, for init/staging, --db and --scope")
	}
	if command == "init" && (origin == "" || archive == "" || binding == "") {
		return errors.New("delivery init requires --origin, --archive, and --binding")
	}
	if command != "init" && timeout <= 0 {
		return errors.New("delivery requires a positive --timeout")
	}
	maxPartsForStage := remote.MaxChangeParts
	if command == "stage-seed" {
		maxPartsForStage = remote.MaxSeedChunks + 2
	}
	if staging && (maxNativeBytes <= 0 || maxEncodedBytes <= 0 || maxEncodedBytes > int64(remote.MaxChangeBytes)*int64(maxPartsForStage) || (command == "stage-seed" && maxRows <= 0) || (command == "stage-changes" && maxEvents <= 0)) {
		return errors.New("staging requires positive event/row, native-byte and encoded-byte bounds within the documented per-command maximum")
	}
	if command == "deliver" && (maxParts < 1 || maxParts > remote.MaxChangeParts || strings.TrimSpace(tokenEnv) == "") {
		return errors.New("deliver requires --max-parts 1..64 and a nonempty token environment variable name")
	}
	format, err := resolveOutputFormat(*formatFlag, *jsonOut)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if command != "init" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var scope store.NativeCaptureScope
	if command != "deliver" {
		scope, err = readDeliveryScope(ctx, scopePath)
		if err != nil {
			return deliveryCommandError("read scope", err)
		}
	}
	if command == "init" {
		file, err := store.NativeArchiveFileIdentity(db)
		if err != nil {
			return deliveryCommandError("identify native archive", err)
		}
		outbox, err := remotedelivery.InitSeedOutbox(ctx, *outboxPath, remotedelivery.SeedRegistration{Origin: origin, Archive: archive, BindingID: binding, App: "slacrawl", Profile: remotedelivery.PrivateArchiveProfile, File: file, Scope: scope})
		if err != nil {
			return deliveryCommandError("initialize outbox (any claimed file is retained)", err)
		}
		if err := outbox.Close(); err != nil {
			return deliveryCommandError("close initialized outbox", err)
		}
		return a.writeOutput("Delivery", deliveryOutput{Status: "initialized", BaselineState: remotedelivery.BaselineUnqualified}, format, false)
	}
	outbox, err := remotedelivery.OpenCurrentOutbox(ctx, *outboxPath)
	if err != nil {
		return deliveryCommandError("open current-schema outbox; upgrades require a separate explicit operation", err)
	}
	defer outbox.Close()
	state, err := outbox.State(ctx)
	if err != nil {
		return deliveryCommandError("read outbox state", err)
	}
	output := deliveryOutput{}
	if staging {
		if state.PendingCaptureID != "" {
			return remotedelivery.ErrPendingCapture
		}
		digest, err := store.NativeCaptureScopeSHA256(scope)
		if err != nil {
			return deliveryCommandError("validate scope", err)
		}
		requiredBaseline := remotedelivery.BaselineUnqualified
		if command == "stage-changes" {
			requiredBaseline = remotedelivery.BaselineQualified
		}
		if state.BaselineState != requiredBaseline || digest != state.Baseline.ScopeSHA256 {
			return remotedelivery.ErrOutboxBaseline
		}
		var through store.NativeCursor
		output.Status = "staged"
		if command == "stage-seed" {
			seed, err := store.CaptureNativeSeed(ctx, db, store.NativeSeedRequest{File: state.Baseline.File, Scope: scope, ScopeSHA256: digest, Limits: store.NativeSeedLimits{MaxRows: maxRows, MaxBytes: maxNativeBytes}})
			if err != nil {
				return deliveryCommandError("capture native seed", err)
			}
			output.CaptureID, err = outbox.StageSeed(ctx, seed, remotedelivery.StageLimits{MaxEncodedBytes: maxEncodedBytes})
			if err != nil {
				return deliveryCommandError("stage seed; reconcile retained outbox before retry", err)
			}
			through = seed.Through
			for _, table := range seed.Tables {
				output.Rows += int64(len(table.Rows))
			}
		} else {
			capture, err := store.CaptureNativeChanges(ctx, db, store.NativeCaptureRequest{Scope: scope, ScopeSHA256: digest, Baseline: state.Baseline, Limits: store.NativeCaptureLimits{MaxEvents: maxEvents, MaxBytes: maxNativeBytes}})
			if err != nil {
				return deliveryCommandError("capture native changes", err)
			}
			output.CaptureID, err = outbox.Stage(ctx, capture, remotedelivery.StageLimits{MaxEncodedBytes: maxEncodedBytes})
			if errors.Is(err, remotedelivery.ErrNoNativeProgress) {
				output.Status = "no_native_progress"
			} else if err != nil {
				return deliveryCommandError("stage changes; reconcile retained outbox before retry", err)
			}
			through, output.EventsRead, output.Rows, output.Removals = capture.Through, capture.EventsRead, int64(len(capture.Rows)), len(capture.Removals)
		}
		output.NativeAnchor = &deliveryAnchor{through.GenerationID, strconv.FormatInt(through.Sequence, 10), through.EntryID}
	} else {
		result, err := outbox.DeliverPending(ctx, remotedelivery.DeliveryOptions{HTTPClient: a.httpClient, TokenProvider: remote.EnvTokenProvider{Name: tokenEnv}}, remotedelivery.DeliveryLimits{MaxParts: maxParts, MaxDuration: timeout})
		output.Status, output.CaptureID, output.ReceiptsRecorded = string(result.Status), result.CaptureID, result.ReceiptsRecorded
		if err != nil {
			// Partial receipts remain durable. Do not present the pre-call baseline as
			// current after an uncertain effect; report only the driver's bounded result.
			if writeErr := a.writeOutput("Delivery", output, format, false); writeErr != nil {
				return writeErr
			}
			return deliveryCommandError("deliver pending requests", err)
		}
	}
	state, err = outbox.State(ctx)
	if err != nil {
		return deliveryCommandError("read resulting outbox state; retained state owns recovery", err)
	}
	output.BaselineState, output.BaselineHold = state.BaselineState, state.BaselineHold
	if err := a.writeOutput("Delivery", output, format, false); err != nil {
		return err
	}
	if state.BaselineHold != "" {
		return remotedelivery.ErrSeedBaselineHeld
	}
	return nil
}

func readDeliveryScope(ctx context.Context, path string) (store.NativeCaptureScope, error) {
	body, err := readPrivateInput(ctx, path, deliveryScopeMaxBytes)
	if err != nil {
		return store.NativeCaptureScope{}, err
	}
	var doc deliveryScopeDocument
	if json.Unmarshal(body, &doc) != nil {
		return store.NativeCaptureScope{}, store.ErrNativeScope
	}
	canonical, err := json.Marshal(doc)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, body) != nil || !bytes.Equal(compact.Bytes(), canonical) || doc.Version != 1 || doc.Channels == nil || doc.SyncStateKeys == nil {
		return store.NativeCaptureScope{}, store.ErrNativeScope
	}
	scope := store.NativeCaptureScope{WorkspaceID: doc.WorkspaceID, IncludeWorkspaceUsers: doc.IncludeWorkspaceUsers}
	for _, channel := range doc.Channels {
		scope.Channels = append(scope.Channels, store.NativeChannelAdmission{ChannelID: channel.ChannelID, SHA256: channel.AdmissionSHA256})
	}
	for _, key := range doc.SyncStateKeys {
		scope.SyncStateKeys = append(scope.SyncStateKeys, store.NativeSyncStateKey{SourceName: key.SourceName, EntityType: key.EntityType, EntityID: key.EntityID})
	}
	if _, err := store.NativeCaptureScopeSHA256(scope); err != nil {
		return store.NativeCaptureScope{}, err
	}
	return scope, nil
}

// Source/SQLite/network errors can carry private rows, paths or credentials.
// Preserve only known content-free classifications and cancellation identity.
func deliveryCommandError(phase string, cause error) error {
	safe := errors.New("delivery: " + phase + " failed")
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, store.ErrNativeScope, store.ErrNativeBaseline, store.ErrNativeLimit, store.ErrNativeData, remotedelivery.ErrPendingCapture, remotedelivery.ErrOutboxBaseline, remotedelivery.ErrOutboxLimit, remotedelivery.ErrSeedBaselineHeld, remotedelivery.ErrDeliveryClient, remotedelivery.ErrDeliverySend, remotedelivery.ErrDeliveryState} {
		if errors.Is(cause, known) {
			return errors.Join(safe, known)
		}
	}
	return safe
}
