package slackdesktop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang/snappy"
	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/store"
)

func requireNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for redux blob decoding")
	}
}

// serializeReduxFixture builds a synthetic Redux state with node. Current Node
// releases emit V8 wire format 15, so the returned bytes always start with
// ff 0f regardless of the installed Node version.
func serializeReduxFixture(t *testing.T, workspaceID, userID string) []byte {
	t.Helper()
	requireNode(t)

	payloadPath := filepath.Join(t.TempDir(), "redux.bin")
	//nolint:gosec // Test builds a controlled V8 fixture with node.
	cmd := exec.Command("node", "-e", fmt.Sprintf(`
const fs = require("fs");
const v8 = require("v8");
fs.writeFileSync(process.argv[1], v8.serialize({
  selfTeamIds: { teamId: %[1]q, defaultWorkspaceId: %[1]q },
  bootData: { user_id: %[2]q },
  channels: {
    C111: { id: "C111", name: "kept", is_channel: true, context_team_id: %[1]q },
    C222: { id: "C222", name: "excluded", is_channel: true, context_team_id: %[1]q }
  },
  members: {
    %[2]q: { id: %[2]q, name: "alice", team_id: %[1]q }
  },
  messages: {
    C111: { "1710000001.000200": { channel: "C111", ts: "1710000001.000200", type: "message", user: %[2]q, text: "kept message" } },
    C222: { "1710000002.000200": { channel: "C222", ts: "1710000002.000200", type: "message", user: %[2]q, text: "excluded message" } }
  }
}));
`, workspaceID, userID), payloadPath)
	require.NoError(t, cmd.Run())

	serialized, err := os.ReadFile(payloadPath) //nolint:gosec // Test reads the fixture it just wrote.
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(serialized), 2)
	require.Equal(t, byte(0xff), serialized[0])
	require.Equal(t, v8WireFormatV15, serialized[1])
	return serialized
}

func snappyWrap(payload []byte) []byte {
	return append([]byte{0xff, 0x11, 0x02}, snappy.Encode(nil, payload)...)
}

// blink21Wrap reproduces the current Slack Desktop framing: Blink envelope
// version 21 (ff 15 fe), a 12-byte binary header, then the inner V8 payload.
func blink21Wrap(v8Payload []byte) []byte {
	envelope := append([]byte{0xff, blinkEnvelopeVersion21, blinkEnvelopeTag}, make([]byte, 12)...)
	return append(envelope, v8Payload...)
}

func asWireFormatV16(v8Payload []byte) []byte {
	converted := append([]byte(nil), v8Payload...)
	converted[1] = v8WireFormatV16
	return converted
}

func writeBlob(t *testing.T, root, name string, payload []byte) {
	t.Helper()
	dir := filepath.Join(root, indexedDBBlobDir, "1", "00")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), payload, 0o600))
}

func decodeFixtureBlob(t *testing.T, payload []byte) reduxBlobDecode {
	t.Helper()
	blobPath := filepath.Join(t.TempDir(), "redux.blob")
	require.NoError(t, os.WriteFile(blobPath, payload, 0o600))
	return decodeReduxBlob(blobPath)
}

func TestDecodeReduxBlobBareV15(t *testing.T) {
	result := decodeFixtureBlob(t, serializeReduxFixture(t, "T111", "U111"))
	require.True(t, result.candidate)
	require.Equal(t, v8WireFormatV15, result.v8Version)
	require.NoError(t, result.err)
	require.Equal(t, "T111", result.state.WorkspaceID)
	require.Equal(t, "U111", result.state.UserID)
}

func TestDecodeReduxBlobSnappyV15(t *testing.T) {
	result := decodeFixtureBlob(t, snappyWrap(serializeReduxFixture(t, "T111", "U111")))
	require.True(t, result.candidate)
	require.Equal(t, v8WireFormatV15, result.v8Version)
	require.NoError(t, result.err)
	require.Equal(t, "T111", result.state.WorkspaceID)
}

func TestDecodeReduxBlobSplitBlinkV8Framing(t *testing.T) {
	// Historical layout: an outer Blink header directly followed by the inner
	// V8 header at offset 2.
	framed := append([]byte{0xff, v8WireFormatV15}, serializeReduxFixture(t, "T111", "U111")...)
	result := decodeFixtureBlob(t, snappyWrap(framed))
	require.True(t, result.candidate)
	require.Equal(t, v8WireFormatV15, result.v8Version)
	require.NoError(t, result.err)
	require.Equal(t, "T111", result.state.WorkspaceID)
}

func TestDecodeReduxBlobBareV16(t *testing.T) {
	result := decodeFixtureBlob(t, asWireFormatV16(serializeReduxFixture(t, "T111", "U111")))
	require.True(t, result.candidate)
	require.Equal(t, v8WireFormatV16, result.v8Version)
	require.NoError(t, result.err)
	require.Equal(t, "T111", result.state.WorkspaceID)
	require.Equal(t, "U111", result.state.UserID)
	require.Len(t, result.state.Channels, 2)
	require.Len(t, result.state.Messages, 2)
}

func TestDecodeReduxBlobSnappyBlink21V16(t *testing.T) {
	blob := snappyWrap(blink21Wrap(asWireFormatV16(serializeReduxFixture(t, "T111", "U111"))))
	result := decodeFixtureBlob(t, blob)
	require.True(t, result.candidate)
	require.Equal(t, v8WireFormatV16, result.v8Version)
	require.NoError(t, result.err)
	require.Equal(t, "T111", result.state.WorkspaceID)
	require.Equal(t, "U111", result.state.UserID)
	require.Len(t, result.state.Messages, 2)
}

func TestDecodeReduxBlobRejectsUnknownFutureVersion(t *testing.T) {
	result := decodeFixtureBlob(t, []byte{0xff, 17, 0x6f, 0x7b})
	require.True(t, result.candidate)
	require.Equal(t, byte(17), result.v8Version)
	require.Error(t, result.err)
	require.Equal(t, "version", decodeErrorStage(result.err))
	require.EqualError(t, result.err, "unsupported V8 wire format version 17")
}

func TestLocateInnerV8Payload(t *testing.T) {
	blink21 := blink21Wrap([]byte{0xff, v8WireFormatV16, 'o'})

	tests := []struct {
		name    string
		decoded []byte
		offset  int
		version byte
	}{
		{name: "bare v15", decoded: []byte{0xff, v8WireFormatV15, 'o'}, offset: 0, version: v8WireFormatV15},
		{name: "bare v16", decoded: []byte{0xff, v8WireFormatV16, 'o'}, offset: 0, version: v8WireFormatV16},
		{name: "bare unsupported", decoded: []byte{0xff, 17, 'o'}, offset: 0, version: 17},
		{name: "split framing", decoded: []byte{0xff, v8WireFormatV15, 0xff, v8WireFormatV15, 'o'}, offset: 2, version: v8WireFormatV15},
		{name: "blink v21 envelope", decoded: blink21, offset: 15, version: v8WireFormatV16},
		{name: "unrelated content", decoded: []byte("plain blob content"), offset: -1},
		{name: "jpeg magic", decoded: []byte{0xff, 0xd8, 0xff, 0xe0}, offset: -1},
		{name: "inner payload not at anchored offset", decoded: append(append([]byte{0xff, blinkEnvelopeVersion21, blinkEnvelopeTag}, make([]byte, 64)...), 0xff, v8WireFormatV15, 'o'), offset: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			offset, version := locateInnerV8Payload(tt.decoded)
			require.Equal(t, tt.offset, offset)
			if tt.offset >= 0 {
				require.Equal(t, tt.version, version)
			}
		})
	}
}

func TestExtractIndexedDBStatesMixedV15AndV16(t *testing.T) {
	root := t.TempDir()
	writeBlob(t, root, "v15", serializeReduxFixture(t, "T111", "U111"))
	writeBlob(t, root, "v16", snappyWrap(blink21Wrap(asWireFormatV16(serializeReduxFixture(t, "T222", "U222")))))

	states, summary, err := extractIndexedDBStates(root)
	require.NoError(t, err)
	require.Len(t, states, 2)
	require.Equal(t, "T111", states[0].WorkspaceID)
	require.Equal(t, "T222", states[1].WorkspaceID)

	require.True(t, summary.NodeAvailable)
	require.Equal(t, 2, summary.BlobFileCount)
	require.Equal(t, 2, summary.CandidateCount)
	require.Equal(t, 2, summary.DecodedBlobCount)
	require.Equal(t, 2, summary.DecodedStateCount)
	require.Zero(t, summary.DecodeFailureCount)
	require.Equal(t, map[string]int{"15": 1, "16": 1}, summary.V8Versions)
}

func TestExtractIndexedDBStatesRetainsValidStatesWhenACandidateIsCorrupt(t *testing.T) {
	root := t.TempDir()
	writeBlob(t, root, "valid", serializeReduxFixture(t, "T111", "U111"))
	// Recognized V8 v15 header with a truncated payload: node must reject it.
	writeBlob(t, root, "truncated", []byte{0xff, v8WireFormatV15, 0x6f})

	states, summary, err := extractIndexedDBStates(root)
	require.NoError(t, err)
	require.Len(t, states, 1)
	require.Equal(t, "T111", states[0].WorkspaceID)

	require.Equal(t, 2, summary.BlobFileCount)
	require.Equal(t, 2, summary.CandidateCount)
	require.Equal(t, 1, summary.DecodedBlobCount)
	require.Equal(t, 1, summary.DecodeFailureCount)
	require.Equal(t, map[string]int{"node": 1}, summary.DecodeFailures)
}

func TestExtractIndexedDBStatesIgnoresUnrelatedBlobFiles(t *testing.T) {
	root := t.TempDir()
	writeBlob(t, root, "valid", serializeReduxFixture(t, "T111", "U111"))
	writeBlob(t, root, "unrelated", []byte("exported attachment bytes, not a redux state"))

	states, summary, err := extractIndexedDBStates(root)
	require.NoError(t, err)
	require.Len(t, states, 1)
	require.Equal(t, 2, summary.BlobFileCount)
	require.Equal(t, 1, summary.CandidateCount)
	require.Equal(t, 1, summary.DecodedBlobCount)
	require.Zero(t, summary.DecodeFailureCount)
}

func TestExtractIndexedDBStatesUnreadableFileIsNotACandidate(t *testing.T) {
	root := t.TempDir()
	writeBlob(t, root, "valid", serializeReduxFixture(t, "T111", "U111"))
	unreadable := filepath.Join(root, indexedDBBlobDir, "1", "00", "unreadable")
	require.NoError(t, os.WriteFile(unreadable, []byte("anything"), 0o600))
	require.NoError(t, os.Chmod(unreadable, 0o000))
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })

	states, summary, err := extractIndexedDBStates(root)
	require.NoError(t, err)
	require.Len(t, states, 1)
	require.Equal(t, 2, summary.BlobFileCount)
	require.Equal(t, 1, summary.CandidateCount)
	require.Equal(t, 1, summary.DecodedBlobCount)
	require.Equal(t, 1, summary.DecodeFailureCount)
	require.Equal(t, map[string]int{"read": 1}, summary.DecodeFailures)
}

func TestExtractIndexedDBStatesNoCandidatesIsSuccessful(t *testing.T) {
	root := t.TempDir()
	writeBlob(t, root, "unrelated", []byte("not a redux blob"))

	states, summary, err := extractIndexedDBStates(root)
	require.NoError(t, err)
	require.Empty(t, states)
	require.Equal(t, 1, summary.BlobFileCount)
	require.Zero(t, summary.CandidateCount)
	require.Zero(t, summary.DecodeFailureCount)
}

func TestExtractIndexedDBStatesClassifiesMalformedSnappy(t *testing.T) {
	root := t.TempDir()
	writeBlob(t, root, "malformed", append([]byte{0xff, 0x11, 0x02}, []byte("%%%not-snappy%%%")...))

	states, summary, err := extractIndexedDBStates(root)
	require.NoError(t, err)
	require.Empty(t, states)
	require.Equal(t, 1, summary.CandidateCount)
	require.Equal(t, 1, summary.DecodeFailureCount)
	require.Equal(t, map[string]int{"snappy": 1}, summary.DecodeFailures)
}

func TestExtractIndexedDBStatesReportsUnsupportedVersion(t *testing.T) {
	root := t.TempDir()
	writeBlob(t, root, "future", []byte{0xff, 17, 0x6f, 0x7b})

	states, summary, err := extractIndexedDBStates(root)
	require.NoError(t, err)
	require.Empty(t, states)
	require.Equal(t, 1, summary.CandidateCount)
	require.Equal(t, 1, summary.DecodeFailureCount)
	require.Equal(t, map[string]int{"version": 1}, summary.DecodeFailures)
	require.Equal(t, map[string]int{"17": 1}, summary.V8Versions)
}

func TestExtractIndexedDBStatesNodeUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	root := t.TempDir()
	writeBlob(t, root, "v15", []byte{0xff, v8WireFormatV15, 0x6f})

	states, summary, err := extractIndexedDBStates(root)
	require.NoError(t, err)
	require.Empty(t, states)
	require.False(t, summary.NodeAvailable)
	require.Equal(t, 1, summary.BlobFileCount)
	// Candidates are still classified without node; only decoding is skipped.
	require.Equal(t, 1, summary.CandidateCount)
	require.Equal(t, map[string]int{"15": 1}, summary.V8Versions)
	require.Zero(t, summary.DecodedBlobCount)
	require.Zero(t, summary.DecodeFailureCount)
}

func TestInspectReportsAllCandidateFailureWithoutError(t *testing.T) {
	root := t.TempDir()
	writeBlob(t, root, "truncated", []byte{0xff, v8WireFormatV15, 0x6f})

	source, err := Inspect(root)
	require.NoError(t, err)
	require.True(t, source.IndexedDB.NodeAvailable)
	require.Equal(t, 1, source.IndexedDB.BlobFileCount)
	require.Equal(t, 1, source.IndexedDB.CandidateCount)
	require.Zero(t, source.IndexedDB.DecodedBlobCount)
	require.Zero(t, source.IndexedDB.DecodedStateCount)
	require.Equal(t, 1, source.IndexedDB.DecodeFailureCount)
	require.Equal(t, map[string]int{"node": 1}, source.IndexedDB.DecodeFailures)
}

func TestIngestFailsWhenEveryCandidateFails(t *testing.T) {
	root := t.TempDir()
	writeBlob(t, root, "truncated", []byte{0xff, v8WireFormatV15, 0x6f})

	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	_, err = Ingest(context.Background(), st, root, IngestOptions{})
	require.EqualError(t, err, "desktop IndexedDB: failed to decode all 1 candidate blob(s): node=1")
}

func TestIngestDecodesV16WithFilteringAndUpserts(t *testing.T) {
	root := t.TempDir()
	writeBlob(t, root, "v16", snappyWrap(blink21Wrap(asWireFormatV16(serializeReduxFixture(t, "T111", "U111")))))

	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	source, err := Ingest(context.Background(), st, root, IngestOptions{
		WorkspaceID:     "T111",
		ExcludeChannels: []string{"#excluded"},
	})
	require.NoError(t, err)
	require.Equal(t, 1, source.IndexedDB.DecodedStateCount)
	require.Equal(t, map[string]int{"16": 1}, source.IndexedDB.V8Versions)

	ctx := context.Background()
	messages, err := st.Messages(ctx, "T111", "", "", 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "C111", messages[0].ChannelID)
	require.Equal(t, "kept message", messages[0].Text)
	require.Equal(t, indexedDBSourceName, messages[0].SourceName)

	channels, err := st.Channels(ctx, "T111", "", 10)
	require.NoError(t, err)
	require.Len(t, channels, 1)
	require.Equal(t, "kept", channels[0].Name)
}

func TestDoctorJSONIncludesIndexedDBDiagnostics(t *testing.T) {
	root := t.TempDir()
	writeBlob(t, root, "malformed", append([]byte{0xff, 0x11, 0x02}, []byte("supersecretpayload")...))

	source, err := Inspect(root)
	require.NoError(t, err)

	rendered, err := json.Marshal(source)
	require.NoError(t, err)

	var decoded struct {
		IndexedDB map[string]any `json:"indexeddb"`
	}
	require.NoError(t, json.Unmarshal(rendered, &decoded))
	for _, key := range []string{
		"node_available",
		"blob_file_count",
		"candidate_count",
		"decoded_blob_count",
		"decoded_state_count",
		"decode_failure_count",
		"decode_failures",
	} {
		require.Contains(t, decoded.IndexedDB, key)
	}
	require.NotContains(t, strings.ToLower(string(rendered)), "supersecretpayload")
}
