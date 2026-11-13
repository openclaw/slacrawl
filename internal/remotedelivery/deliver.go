package remotedelivery

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/openclaw/crawlkit/remote"
)

var (
	ErrDeliveryOptions = errors.New("delivery requires authentication and explicit positive time and part limits")
	ErrDeliveryState   = errors.New("delivery outbox operation failed; reconcile the retained capture")
	ErrDeliveryClient  = errors.New("delivery client configuration failed")
	ErrDeliverySend    = errors.New("delivery request or receipt failed; resume the exact persisted part")
)

// DeliveryOptions carries no destination: the immutable registration owns it.
// HTTPClient and TokenProvider remain caller-owned, trusted in-memory adapters.
type DeliveryOptions struct {
	HTTPClient    *http.Client
	TokenProvider remote.TokenProvider
}

type DeliveryLimits struct {
	MaxParts    int
	MaxDuration time.Duration
}

type DeliveryStatus string

const (
	DeliveryIdle      DeliveryStatus = "idle"
	DeliveryPending   DeliveryStatus = "pending"
	DeliveryComplete  DeliveryStatus = "complete"
	DeliveryPartLimit DeliveryStatus = "part_limit"
)

type DeliveryResult struct {
	CaptureID string
	// ReceiptsRecorded counts successful RecordReceipt calls, including receipts
	// another dispatcher also recorded. It is not a count of remote effects.
	ReceiptsRecorded int
	Status           DeliveryStatus
}

// DeliverPending drains only the capture pending at entry, without acquiring or
// staging source data. Each saved part is sent once per invocation; an uncertain
// HTTP or local commit stops the call and leaves recovery to the durable outbox.
// No SQLite transaction is held over HTTP. Caller-owned transports and token
// providers must honor context cancellation; the driver starts no background work.
func (o *Outbox) DeliverPending(ctx context.Context, options DeliveryOptions, limits DeliveryLimits) (DeliveryResult, error) {
	var result DeliveryResult
	if options.TokenProvider == nil || limits.MaxParts < 1 || limits.MaxParts > remote.MaxChangeParts || limits.MaxDuration <= 0 {
		return result, ErrDeliveryOptions
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	state, err := o.State(ctx)
	if err != nil {
		return result, deliveryError(ctx, err, ErrDeliveryState)
	}
	if state.PendingCaptureID == "" {
		result.Status = DeliveryIdle
		return result, nil
	}
	result.CaptureID, result.Status = state.PendingCaptureID, DeliveryPending
	var seedPlan *remote.SlackSeedPlan
	if state.PendingKind == "seed" {
		seedPlan, err = o.seedPlan(ctx, state)
		if err != nil {
			return result, deliveryError(ctx, err, ErrDeliveryState)
		}
	}
	r := state.Registration
	client, err := remote.NewClient(remote.Options{Endpoint: r.Origin, HTTPClient: options.HTTPClient, TokenProvider: options.TokenProvider})
	if err != nil {
		return result, deliveryError(ctx, err, ErrDeliveryClient)
	}
	for result.ReceiptsRecorded < limits.MaxParts {
		part, err := o.NextPart(ctx)
		if err != nil {
			return result, deliveryError(ctx, err, ErrDeliveryState)
		}
		if part == nil || part.Ref.CaptureID != result.CaptureID {
			// Another handle may complete this capture and stage its successor.
			// Completion is pinned to our capture; never send the successor here.
			result.Status = DeliveryComplete
			return result, nil
		}
		var receipt remote.ChangeReceipt
		if seedPlan == nil {
			receipt, err = client.ApplyChanges(ctx, r.App, r.Archive, part.Body)
		} else if part.Ref.Index == 0 {
			receipt, err = client.BeginSlackSeed(ctx, r.Archive, seedPlan, part.Body)
		} else if part.Ref.Index == part.PartCount-1 {
			receipt, err = client.CompleteSlackSeed(ctx, r.Archive, seedPlan, part.Body)
		} else {
			receipt, err = client.ApplySlackSeedChunk(ctx, r.Archive, seedPlan, part.Body)
		}
		if err != nil {
			return result, deliveryError(ctx, err, ErrDeliverySend)
		}
		if err := o.RecordReceipt(ctx, part.Ref, receipt); err != nil {
			return result, deliveryError(ctx, err, ErrDeliveryState)
		}
		result.ReceiptsRecorded++
	}
	state, err = o.State(ctx)
	if err != nil {
		return result, deliveryError(ctx, err, ErrDeliveryState)
	}
	result.Status = DeliveryPartLimit
	if state.PendingCaptureID != result.CaptureID {
		result.Status = DeliveryComplete
	}
	return result, nil
}

// Remote/SQLite errors may contain URLs, credentials or private records. Keep
// only safe operation classification and useful context cancellation identity.
func deliveryError(ctx context.Context, cause, safe error) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(safe, err)
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, ErrSeedBaselineHeld} {
		if errors.Is(cause, err) {
			return errors.Join(safe, err)
		}
	}
	return safe
}
