package remotedelivery

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openclaw/crawlkit/remote"
	"github.com/stretchr/testify/require"
)

func TestOutboxPackingBounds(t *testing.T) {
	state := OutboxState{Registration: Registration{App: "slacrawl", Archive: "fixture", BindingID: "binding"}, ServerCursor: strings.Repeat("\"", 512)}
	value := "x"
	change := remote.Change{Table: "messages", Operation: "upsert", Key: []remote.ChangeValue{{Kind: "text", Value: &value}}, Row: []remote.ChangeValue{{Kind: "text", Value: &value}}}
	limit := StageLimits{MaxEncodedBytes: 256 << 20}
	encode := func(changes []remote.Change) ([][]byte, error) {
		return encodeOutboxParts(state, strings.Repeat("a", 32), "from", "through", changes, limit)
	}
	changes := make([]remote.Change, remote.MaxChanges*remote.MaxChangeParts)
	for i := range changes {
		changes[i] = change
	}
	parts, err := encode(changes)
	require.NoError(t, err)
	require.Len(t, parts, remote.MaxChangeParts)
	_, err = encode(append(changes, change))
	require.ErrorIs(t, err, ErrOutboxLimit)
	value = strings.Repeat("\x00", 360000)
	parts, err = encode([]remote.Change{change, change})
	// NUL escaping makes each observation exceed 4 MiB across its key and row.
	require.ErrorIs(t, err, ErrOutboxLimit)
	require.Nil(t, parts)
	value = strings.Repeat("\x00", 180000)
	parts, err = encode([]remote.Change{change, change})
	require.NoError(t, err)
	require.Len(t, parts, 2, "encoded size, not change count alone, splits parts")
	for _, body := range parts {
		require.LessOrEqual(t, len(body), remote.MaxChangeBytes)
		var batch remote.ChangeBatch
		require.NoError(t, json.Unmarshal(body, &batch))
		require.Len(t, batch.Changes, 1)
	}
	value = "x"
	limit.MaxEncodedBytes = 1
	_, err = encode([]remote.Change{change})
	require.ErrorIs(t, err, ErrOutboxLimit)
}
