package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/openclaw/slacrawl/internal/admission"
)

type NativeChannelAdmission struct{ ChannelID, SHA256 string }
type NativeSyncStateKey struct{ SourceName, EntityType, EntityID string }
type NativeCaptureScope struct {
	WorkspaceID           string
	Channels              []NativeChannelAdmission
	IncludeWorkspaceUsers bool
	SyncStateKeys         []NativeSyncStateKey
}

// NativeChannelFacts are local stored facts, not proof of permission to read or
// publish a channel. The baseline owner supplies the explicitly authorized scope.
type NativeChannelFacts struct {
	ChannelID, WorkspaceID, Kind string
	IsPrivate, IsShared          int64
	RawJSON                      string
}

// NativeCaptureScopeSHA256 binds the complete explicit scope. It grants no access.
func NativeCaptureScopeSHA256(scope NativeCaptureScope) (string, error) {
	canonical, err := canonicalNativeScope(scope)
	if err != nil {
		return "", err
	}
	return nativeDigest(canonical), nil
}

func canonicalNativeScope(scope NativeCaptureScope) (NativeCaptureScope, error) {
	if !nativeIdentityText(scope.WorkspaceID) || len(scope.Channels) == 0 {
		return NativeCaptureScope{}, ErrNativeScope
	}
	scope.Channels = slices.Clone(scope.Channels)
	scope.SyncStateKeys = slices.Clone(scope.SyncStateKeys)
	slices.SortFunc(scope.Channels, func(a, b NativeChannelAdmission) int { return strings.Compare(a.ChannelID, b.ChannelID) })
	for i, channel := range scope.Channels {
		if !nativeIdentityText(channel.ChannelID) || !nativeSHA256(channel.SHA256) || (i > 0 && channel.ChannelID == scope.Channels[i-1].ChannelID) {
			return NativeCaptureScope{}, ErrNativeScope
		}
	}
	slices.SortFunc(scope.SyncStateKeys, func(a, b NativeSyncStateKey) int {
		if n := strings.Compare(a.SourceName, b.SourceName); n != 0 {
			return n
		}
		if n := strings.Compare(a.EntityType, b.EntityType); n != 0 {
			return n
		}
		return strings.Compare(a.EntityID, b.EntityID)
	})
	for i, key := range scope.SyncStateKeys {
		if !nativeIdentityText(key.SourceName) || !nativeIdentityText(key.EntityType) || !nativeIdentityText(key.EntityID) || (i > 0 && key == scope.SyncStateKeys[i-1]) {
			return NativeCaptureScope{}, ErrNativeScope
		}
	}
	return scope, nil
}

func nativeIdentityText(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}
func nativeSHA256(value string) bool {
	body, err := hex.DecodeString(value)
	return err == nil && len(body) == 32 && strings.ToLower(value) == value
}
func nativeAnchor(value string) bool {
	body, err := hex.DecodeString(value)
	return err == nil && len(body) == 16 && strings.ToLower(value) == value
}
func nativeDigest(value any) string {
	body, _ := json.Marshal(value)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// NativeChannelAdmissionSHA256 binds id/workspace/kind/private/shared and the
// recognized raw identity/type fields documented in SPEC.md. Other raw fields
// remain local row data; a name/topic/timestamp refresh does not change admission.
func NativeChannelAdmissionSHA256(facts NativeChannelFacts) (string, error) {
	if !nativeIdentityText(facts.ChannelID) || !nativeIdentityText(facts.WorkspaceID) || (facts.IsPrivate != 0 && facts.IsPrivate != 1) || (facts.IsShared != 0 && facts.IsShared != 1) {
		return "", ErrNativeScope
	}
	raw, err := nativeJSONObject(facts.RawJSON, true)
	if err != nil {
		return "", ErrNativeScope
	}
	flags := map[string]bool{}
	for _, name := range []string{"is_channel", "is_group", "is_im", "is_mpim", "is_private", "is_shared"} {
		value, ok := raw[name]
		if !ok || (string(value) != "true" && string(value) != "false") {
			return "", ErrNativeScope
		}
		flags[name] = string(value) == "true"
	}
	ids := map[string]*string{}
	for _, name := range []string{"id", "channel_id", "workspace_id", "team_id", "context_team_id"} {
		value, ok := raw[name]
		if !ok {
			if name == "id" {
				return "", ErrNativeScope
			}
			continue
		}
		var id string
		if string(value) == "null" || json.Unmarshal(value, &id) != nil || !utf8.ValidString(id) {
			return "", ErrNativeScope
		}
		expected := facts.WorkspaceID
		if name == "id" || name == "channel_id" {
			expected = facts.ChannelID
		}
		if id != expected && (id != "" || name == "id" || name == "channel_id") {
			return "", ErrNativeScope
		}
		ids[name] = &id
	}
	native := admission.NativeFlags{IsChannel: flags["is_channel"], IsGroup: flags["is_group"], IsIM: flags["is_im"], IsMPIM: flags["is_mpim"], IsPrivate: flags["is_private"]}
	if native.IsIM && (native.IsMPIM || native.IsChannel || native.IsGroup) || native.IsMPIM && (native.IsIM || native.IsChannel || !native.IsPrivate) || native.IsChannel && native.IsGroup {
		return "", ErrNativeScope
	}
	kind := admission.Unknown
	switch facts.Kind {
	case "public", "public_channel", "desktop_channel":
		kind = admission.PublicChannel
	case "private", "private_channel", "desktop_private_channel":
		kind = admission.PrivateChannel
	case "im", "desktop_im":
		kind = admission.IM
	case "mpim", "desktop_mpim":
		kind = admission.MPIM
	}
	if kind == admission.Unknown || kind != native.Kind() || native.IsPrivate != (facts.IsPrivate == 1) || flags["is_shared"] != (facts.IsShared == 1) {
		return "", ErrNativeScope
	}
	return nativeDigest(struct {
		Version                      int
		ChannelID, WorkspaceID, Kind string
		IsPrivate, IsShared          int64
		Identities                   map[string]*string
		Flags                        map[string]bool
	}{1, facts.ChannelID, facts.WorkspaceID, facts.Kind, facts.IsPrivate, facts.IsShared, ids, flags}), nil
}

// Reject duplicate fields, including differently cased native identity facts.
// Values stay raw until their owning contract interprets them.
func nativeJSONObject(raw string, fold bool) (map[string]json.RawMessage, error) {
	if !utf8.ValidString(raw) {
		return nil, ErrNativeData
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrNativeData
	}
	result := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, ErrNativeData
		}
		key, ok := token.(string)
		if !ok {
			return nil, ErrNativeData
		}
		if fold {
			key = strings.ToLower(key)
		}
		if _, exists := result[key]; exists {
			return nil, ErrNativeData
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, ErrNativeData
		}
		result[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, ErrNativeData
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, ErrNativeData
	}
	return result, nil
}
