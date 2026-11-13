package remotedelivery

import (
	"encoding/json"

	"github.com/openclaw/crawlkit/remote"
)

func encodeOutboxParts(state OutboxState, id, from, through string, changes []remote.Change, limits StageLimits) ([][]byte, error) {
	if limits.MaxEncodedBytes <= 0 || limits.MaxEncodedBytes > int64(remote.MaxChangeBytes)*remote.MaxChangeParts {
		return nil, ErrOutboxLimit
	}
	r := state.Registration
	batch := remote.ChangeBatch{Contract: remote.ChangeContract, App: r.App, Archive: r.Archive, BindingID: r.BindingID, CaptureID: id, NativeFrom: from, NativeThrough: through, ExpectedCursor: state.ServerCursor, PartCount: remote.MaxChangeParts, PartIndex: remote.MaxChangeParts - 1, Changes: []remote.Change{}}
	// Reserve the larger first/continuation envelope, with maximum-width part
	// numbers. Final bytes are still encoded and bounded by the shared owner.
	first, err := remote.EncodeChangeBatch(batch)
	if err != nil {
		return nil, err
	}
	batch.ExpectedCursor = batch.BatchID()
	later, err := remote.EncodeChangeBatch(batch)
	if err != nil {
		return nil, err
	}
	overhead := max(len(first), len(later))
	var groups [][]remote.Change
	start, size := 0, overhead
	for i, change := range changes {
		encoded, err := json.Marshal(change)
		if err != nil {
			return nil, err
		}
		if overhead+len(encoded) > remote.MaxChangeBytes {
			return nil, ErrOutboxLimit
		}
		comma := 0
		if i > start {
			comma = 1
		}
		if i-start == remote.MaxChanges || size+comma+len(encoded) > remote.MaxChangeBytes {
			groups = append(groups, changes[start:i])
			if len(groups) == remote.MaxChangeParts {
				return nil, ErrOutboxLimit
			}
			start, size, comma = i, overhead, 0
		}
		size += comma + len(encoded)
	}
	groups = append(groups, changes[start:])
	parts := make([][]byte, 0, len(groups))
	batch.PartCount = len(groups)
	batch.ExpectedCursor = state.ServerCursor
	var total int64
	for i, group := range groups {
		batch.PartIndex, batch.Changes = i, group
		body, err := remote.EncodeChangeBatch(batch)
		if err != nil {
			return nil, err
		}
		total += int64(len(body))
		if total > limits.MaxEncodedBytes {
			return nil, ErrOutboxLimit
		}
		parts = append(parts, body)
		batch.ExpectedCursor = batch.BatchID()
	}
	return parts, nil
}
