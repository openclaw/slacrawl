package slackdesktop

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/golang/snappy"
	"github.com/slack-go/slack"

	"github.com/openclaw/slacrawl/internal/search"
	"github.com/openclaw/slacrawl/internal/store"
)

const (
	indexedDBBlobDir    = "IndexedDB/https_app.slack.com_0.indexeddb.blob"
	indexedDBSourceName = "desktop-indexeddb"
)

var reduxV8Header = []byte{0xff, 0x0f}

//go:embed redux_decoder.js
var reduxDecoderScript string

type ReduxDecodedState struct {
	WorkspaceID string         `json:"workspace_id"`
	UserID      string         `json:"user_id"`
	Channels    []ReduxChannel `json:"channels"`
	Members     []ReduxMember  `json:"members"`
	Messages    []ReduxMessage `json:"messages"`
}

type ReduxChannel struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	User          string        `json:"user"`
	Members       []string      `json:"members"`
	IsChannel     bool          `json:"is_channel"`
	IsGroup       bool          `json:"is_group"`
	IsIM          bool          `json:"is_im"`
	IsMPIM        bool          `json:"is_mpim"`
	IsPrivate     bool          `json:"is_private"`
	IsArchived    bool          `json:"is_archived"`
	IsGeneral     bool          `json:"is_general"`
	ContextTeamID string        `json:"context_team_id"`
	Topic         ReduxTextMeta `json:"topic"`
	Purpose       ReduxTextMeta `json:"purpose"`
}

type ReduxTextMeta struct {
	Value string `json:"value"`
}

type ReduxMember struct {
	ID      string             `json:"id"`
	Name    string             `json:"name"`
	IsBot   bool               `json:"is_bot"`
	Deleted bool               `json:"deleted"`
	TeamID  string             `json:"team_id"`
	Real    string             `json:"real_name"`
	Profile ReduxMemberProfile `json:"profile"`
}

type ReduxMemberProfile struct {
	RealName    string `json:"real_name"`
	DisplayName string `json:"display_name"`
	Title       string `json:"title"`
}

type ReduxMessage struct {
	Channel      string       `json:"channel"`
	TS           string       `json:"ts"`
	ThreadTS     string       `json:"thread_ts"`
	User         string       `json:"user"`
	Subtype      string       `json:"subtype"`
	Type         string       `json:"type"`
	ClientMsgID  string       `json:"client_msg_id"`
	ParentUserID string       `json:"parent_user_id"`
	Text         string       `json:"text"`
	Blocks       any          `json:"blocks,omitempty"`
	Attachments  any          `json:"attachments,omitempty"`
	ReplyCount   int          `json:"reply_count"`
	LatestReply  string       `json:"latest_reply"`
	Hidden       bool         `json:"hidden"`
	Edited       *ReduxEdited `json:"edited"`
}

type ReduxEdited struct {
	TS string `json:"ts"`
}

type reduxBlobRef struct {
	Path        string
	WorkspaceID string
	UserID      string
}

func nodeAvailable() bool {
	_, err := exec.LookPath("node")
	return err == nil
}

func ExtractIndexedDBStates(path string) ([]ReduxDecodedState, error) {
	if !nodeAvailable() {
		return nil, nil
	}

	refs, err := scanReduxBlobRefs(filepath.Join(path, indexedDBBlobDir))
	if err != nil {
		return nil, err
	}

	byIdentity := map[string]ReduxDecodedState{}
	for _, ref := range refs {
		state, err := decodeReduxBlob(ref.Path)
		if err != nil {
			continue
		}
		if state.WorkspaceID == "" {
			state.WorkspaceID = ref.WorkspaceID
		}
		if state.UserID == "" {
			state.UserID = ref.UserID
		}
		if state.WorkspaceID == "" && state.UserID == "" {
			continue
		}
		key := state.WorkspaceID + "|" + state.UserID
		current, ok := byIdentity[key]
		if !ok || reduxStateScore(state) > reduxStateScore(current) {
			byIdentity[key] = state
		}
	}

	states := make([]ReduxDecodedState, 0, len(byIdentity))
	for _, state := range byIdentity {
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool {
		if states[i].WorkspaceID == states[j].WorkspaceID {
			return states[i].UserID < states[j].UserID
		}
		return states[i].WorkspaceID < states[j].WorkspaceID
	})
	return states, nil
}

func ingestReduxStates(ctx context.Context, st *store.Store, states []ReduxDecodedState, now time.Time, filter ingestFilter) error {
	mergedChannelNames := channelNamesByWorkspaceID(states)
	workspaceCandidates := channelWorkspaceCandidates(states)
	for _, state := range states {
		if state.WorkspaceID == "" {
			continue
		}
		if filter.allowWorkspace(state.WorkspaceID) {
			if err := upsertIndexedDBWorkspace(ctx, st, state.WorkspaceID, now); err != nil {
				return err
			}
		}
		allowedChannels := map[string]struct{}{}
		channelWorkspaces := map[string]string{}
		knownChannelNames := map[string][]string{}
		referencedUsers := map[string]struct{}{}
		memberNames := map[string]string{}
		for _, member := range state.Members {
			memberNames[member.ID] = firstNonEmpty(member.Profile.DisplayName, member.Profile.RealName, member.Name, member.Real, member.ID)
		}
		for _, channel := range state.Channels {
			workspaceID := channelContextWorkspaceID(channel.ContextTeamID, state.WorkspaceID)
			channelWorkspaces[channel.ID] = workspaceID
			channelName := reduxChannelName(channel, memberNames)
			filterChannelNames := mergedChannelNames.get(workspaceID, channel.ID)
			knownChannelNames[channel.ID] = filterChannelNames
			if !filter.allowChannelNames(workspaceID, channel.ID, filterChannelNames) {
				continue
			}
			if err := upsertIndexedDBWorkspace(ctx, st, workspaceID, now); err != nil {
				return err
			}
			allowedChannels[channel.ID] = struct{}{}
			if channel.User != "" {
				referencedUsers[channel.User] = struct{}{}
			}
			for _, memberID := range channel.Members {
				referencedUsers[memberID] = struct{}{}
			}
			if err := st.UpsertChannel(ctx, store.Channel{
				ID:          channel.ID,
				WorkspaceID: workspaceID,
				Name:        channelName,
				Kind:        reduxChannelKind(channel),
				Topic:       channel.Topic.Value,
				Purpose:     channel.Purpose.Value,
				IsPrivate:   channel.IsPrivate || channel.IsGroup || channel.IsIM || channel.IsMPIM,
				IsArchived:  channel.IsArchived,
				IsGeneral:   channel.IsGeneral,
				RawJSON:     store.MarshalRaw(channel),
				UpdatedAt:   now,
			}); err != nil {
				if store.IsWorkspaceCollision(err, "channel") {
					continue
				}
				return err
			}
		}
		for _, message := range state.Messages {
			if message.Channel == "" || message.TS == "" {
				continue
			}
			workspaceID, ok := channelWorkspaces[message.Channel]
			if !ok {
				workspaceID, ok = resolveChannelWorkspace(message.Channel, state.WorkspaceID, workspaceCandidates)
				if !ok {
					continue
				}
			}
			if !filter.allowWorkspace(workspaceID) {
				continue
			}
			channelNames, knownChannel := knownChannelNames[message.Channel]
			if len(channelNames) == 0 {
				channelNames = mergedChannelNames.get(workspaceID, message.Channel)
			}
			if _, ok := allowedChannels[message.Channel]; !ok && (knownChannel || !isSlackConversationID(message.Channel)) {
				continue
			}
			if !filter.allowChannelNames(workspaceID, message.Channel, channelNames) {
				continue
			}
			text := strings.TrimSpace(message.Text)
			if text == "" && message.Subtype == "" && message.Type == "" {
				continue
			}
			if message.User != "" {
				referencedUsers[message.User] = struct{}{}
			}
			if message.ParentUserID != "" {
				referencedUsers[message.ParentUserID] = struct{}{}
			}
			if err := upsertDesktopMessage(ctx, st, store.Message{
				ChannelID:      message.Channel,
				TS:             message.TS,
				WorkspaceID:    workspaceID,
				UserID:         message.User,
				Subtype:        message.Subtype,
				ClientMsgID:    message.ClientMsgID,
				ThreadTS:       message.ThreadTS,
				ParentUserID:   message.ParentUserID,
				Text:           text,
				NormalizedText: normalizeReduxMessage(message),
				ReplyCount:     message.ReplyCount,
				LatestReply:    message.LatestReply,
				EditedTS:       editedTS(message),
				SourceRank:     3,
				SourceName:     indexedDBSourceName,
				RawJSON:        store.MarshalRaw(message),
				UpdatedAt:      now,
			}, reduxMentions(message.Text)); err != nil {
				return err
			}
		}
		for _, member := range state.Members {
			workspaceID := fallback(member.TeamID, state.WorkspaceID)
			if !filter.allowWorkspace(workspaceID) {
				if _, referenced := referencedUsers[member.ID]; !referenced {
					continue
				}
			}
			if err := upsertDesktopUser(ctx, st, store.User{
				ID:          member.ID,
				WorkspaceID: workspaceID,
				Name:        fallback(member.Name, member.ID),
				RealName:    fallback(member.Profile.RealName, member.Real),
				DisplayName: member.Profile.DisplayName,
				Title:       member.Profile.Title,
				IsBot:       member.IsBot,
				IsDeleted:   member.Deleted,
				RawJSON:     store.MarshalRaw(member),
				UpdatedAt:   now,
			}); err != nil {
				return err
			}
		}
		if err := st.SetSyncState(ctx, sourceName, "indexeddb", "decoded_states", fmt.Sprintf("%d", len(states))); err != nil {
			return err
		}
	}
	return nil
}

func channelWorkspaceCandidates(states []ReduxDecodedState) map[string]map[string]struct{} {
	candidates := map[string]map[string]struct{}{}
	for _, state := range states {
		for _, channel := range state.Channels {
			if channel.ID == "" {
				continue
			}
			workspaceID := channelContextWorkspaceID(channel.ContextTeamID, state.WorkspaceID)
			if workspaceID == "" {
				continue
			}
			if _, ok := candidates[channel.ID]; !ok {
				candidates[channel.ID] = map[string]struct{}{}
			}
			candidates[channel.ID][workspaceID] = struct{}{}
		}
	}
	return candidates
}

func channelContextWorkspaceID(contextTeamID, stateWorkspaceID string) string {
	contextTeamID = strings.TrimSpace(contextTeamID)
	if strings.HasPrefix(contextTeamID, "T") {
		return contextTeamID
	}
	return strings.TrimSpace(stateWorkspaceID)
}

func resolveChannelWorkspace(channelID, fallbackWorkspace string, candidates map[string]map[string]struct{}) (string, bool) {
	fallbackWorkspace = strings.TrimSpace(fallbackWorkspace)
	workspaces := candidates[channelID]
	if len(workspaces) == 0 {
		return fallbackWorkspace, fallbackWorkspace != ""
	}
	if fallbackWorkspace != "" {
		if _, ok := workspaces[fallbackWorkspace]; ok {
			return fallbackWorkspace, true
		}
	}
	if len(workspaces) == 1 {
		for workspaceID := range workspaces {
			return workspaceID, true
		}
	}
	return "", false
}

func upsertIndexedDBWorkspace(ctx context.Context, st *store.Store, workspaceID string, now time.Time) error {
	if workspaceID == "" {
		return nil
	}
	return st.EnsureWorkspace(ctx, store.Workspace{
		ID:        workspaceID,
		Name:      workspaceID,
		RawJSON:   store.MarshalRaw(map[string]any{"workspace_id": workspaceID, "source": indexedDBSourceName}),
		UpdatedAt: now,
	})
}

func scanReduxBlobRefs(blobRoot string) ([]reduxBlobRef, error) {
	var refs []reduxBlobRef
	err := filepath.Walk(blobRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info == nil || info.IsDir() {
			return nil
		}
		refs = append(refs, reduxBlobRef{Path: path})
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(refs, func(i, j int) bool {
		return refs[i].Path < refs[j].Path
	})
	return refs, nil
}

func decodeReduxBlob(blobPath string) (ReduxDecodedState, error) {
	raw, err := os.ReadFile(blobPath) //nolint:gosec // Blob path is discovered inside the Slack IndexedDB directory.
	if err != nil {
		return ReduxDecodedState{}, err
	}

	decoded := raw
	if len(raw) >= 3 && raw[0] == 0xff && raw[1] == 0x11 && raw[2] == 0x02 {
		decoded, err = snappy.Decode(nil, raw[3:])
		if err != nil {
			return ReduxDecodedState{}, err
		}
	}

	offset := bytes.Index(decoded, reduxV8Header)
	if offset < 0 {
		return ReduxDecodedState{}, fmt.Errorf("v8 payload not found in %s", blobPath)
	}

	tempFile, err := os.CreateTemp("", "slacrawl-redux-*.bin")
	if err != nil {
		return ReduxDecodedState{}, err
	}
	tempPath := tempFile.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if _, err := tempFile.Write(decoded[offset:]); err != nil {
		_ = tempFile.Close()
		return ReduxDecodedState{}, err
	}
	if err := tempFile.Close(); err != nil {
		return ReduxDecodedState{}, err
	}

	cmd := exec.Command("node", "-e", reduxDecoderScript, tempPath) //nolint:gosec // Node decodes a temporary V8 payload copied from the Slack data directory.
	output, err := cmd.Output()
	if err != nil {
		return ReduxDecodedState{}, err
	}

	var state ReduxDecodedState
	if err := json.Unmarshal(output, &state); err != nil {
		return ReduxDecodedState{}, err
	}
	return state, nil
}

func reduxChannelKind(channel ReduxChannel) string {
	switch {
	case channel.IsIM:
		return "desktop_im"
	case channel.IsMPIM:
		return "desktop_mpim"
	case channel.IsGroup || channel.IsPrivate:
		return "desktop_private_channel"
	default:
		return "desktop_channel"
	}
}

func reduxChannelName(channel ReduxChannel, memberNames map[string]string) string {
	if strings.TrimSpace(channel.Name) != "" {
		return strings.TrimSpace(channel.Name)
	}
	if channel.IsIM {
		return firstNonEmpty(memberNames[channel.User], channel.User, channel.ID)
	}
	if channel.IsMPIM {
		names := make([]string, 0, len(channel.Members))
		for _, memberID := range channel.Members {
			names = append(names, firstNonEmpty(memberNames[memberID], memberID))
		}
		if len(names) > 0 {
			return strings.Join(names, ", ")
		}
	}
	return channel.ID
}

func reduxResolvedChannelNames(channel ReduxChannel, memberNames map[string]string) []string {
	aliases := make([]string, 0, 2)
	if name := strings.TrimSpace(channel.Name); name != "" && !channel.IsIM && !channel.IsMPIM {
		aliases = append(aliases, name)
	}
	if channel.IsIM {
		if name, ok := resolvedMemberName(channel.User, memberNames); ok {
			aliases = append(aliases, name)
		}
	}
	if channel.IsMPIM {
		names := make([]string, 0, len(channel.Members))
		allResolved := len(channel.Members) > 0
		for _, memberID := range channel.Members {
			if name, ok := resolvedMemberName(memberID, memberNames); ok {
				names = append(names, name)
			} else {
				allResolved = false
			}
		}
		if allResolved {
			if name := strings.TrimSpace(channel.Name); name != "" {
				aliases = append(aliases, name)
			}
			sort.Strings(names)
			aliases = append(aliases, strings.Join(names, ","))
			aliases = append(aliases, strings.Join(names, ", "))
		}
	}
	return uniqueNonEmptyStrings(aliases)
}

func resolvedMemberName(memberID string, memberNames map[string]string) (string, bool) {
	name := strings.TrimSpace(memberNames[memberID])
	return name, name != "" && name != memberID
}

func uniqueNonEmptyStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func isSlackConversationID(value string) bool {
	return strings.HasPrefix(value, "C") || strings.HasPrefix(value, "G") || strings.HasPrefix(value, "D")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func normalizeReduxMessage(message ReduxMessage) string {
	slackMessage := slack.Message{
		Msg: slack.Msg{
			Text:            message.Text,
			Timestamp:       message.TS,
			ThreadTimestamp: message.ThreadTS,
			SubType:         message.Subtype,
			DeletedTimestamp: func() string {
				if message.Hidden && message.Subtype == "tombstone" {
					return message.TS
				}
				return ""
			}(),
		},
	}
	rawPayload := map[string]any{}
	if message.Blocks != nil {
		rawPayload["blocks"] = message.Blocks
	}
	if message.Attachments != nil {
		rawPayload["attachments"] = message.Attachments
	}
	if len(rawPayload) > 0 {
		return search.NormalizeMessageWithRawPayload(slackMessage, rawPayload)
	}
	return search.NormalizeMessage(slackMessage)
}

func reduxMentions(text string) []store.Mention {
	raw := search.ExtractMentions(text)
	mentions := make([]store.Mention, 0, len(raw))
	seen := map[string]struct{}{}
	for _, mention := range raw {
		key := mention.Type + "|" + mention.TargetID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		mentions = append(mentions, store.Mention{
			Type:        mention.Type,
			TargetID:    mention.TargetID,
			DisplayText: mention.DisplayText,
		})
	}
	return mentions
}

func editedTS(message ReduxMessage) string {
	if message.Edited == nil {
		return ""
	}
	return message.Edited.TS
}

func reduxStateScore(state ReduxDecodedState) int {
	return len(state.Channels)*1000 + len(state.Members)*10 + len(state.Messages)
}
