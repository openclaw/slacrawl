package slackdesktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/comparer"
	"github.com/syndtr/goleveldb/leveldb/opt"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
)

const (
	localStorageDir = "Local Storage/leveldb"
	indexedDBDir    = "IndexedDB/https_app.slack.com_0.indexeddb.leveldb"
	rootStateFile   = "storage/root-state.json"
	sourceName      = "desktop"
	draftSourceName = "desktop-draft"
)

var makeSnapshotTempDir = os.MkdirTemp

type Source struct {
	Path      string              `json:"path"`
	Available bool                `json:"available"`
	Summary   RootStateSummary    `json:"summary"`
	Local     LocalStorageSummary `json:"local_storage"`
	IndexedDB IndexedDBSummary    `json:"indexeddb"`
	Snapshot  string              `json:"snapshot_path,omitempty"`
}

type IngestOptions struct {
	WorkspaceID     string
	Channels        []string
	ExcludeChannels []string
}

type ingestFilter struct {
	workspaceID      string
	channels         map[string]struct{}
	excludeChannels  map[string]struct{}
	excludeSelectors []channelSelector
	hasNameExclude   bool
}

type channelSelector struct {
	raw        string
	normalized string
	explicitID bool
}

type RootStateSummary struct {
	AppTeamsKeys      []string `json:"app_teams_keys"`
	WorkspaceCount    int      `json:"workspace_count"`
	TeamsCount        int      `json:"teams_count"`
	DownloadTeamCount int      `json:"download_team_count"`
	DownloadItemCount int      `json:"download_item_count"`
}

type LocalStorageSummary struct {
	WorkspaceCount     int `json:"workspace_count"`
	DraftCount         int `json:"draft_count"`
	ActivityTeamCount  int `json:"activity_team_count"`
	RecentChannelCount int `json:"recent_channel_count"`
	ReadMarkerCount    int `json:"read_marker_count"`
	CustomStatusCount  int `json:"custom_status_count"`
	ExpandableCount    int `json:"expandable_count"`
}

type IndexedDBSummary struct {
	ObjectStores      []string `json:"object_stores"`
	DecodedStateCount int      `json:"decoded_state_count"`
}

type Snapshot struct {
	Root string
}

type ExtractedData struct {
	RootState   RootStateData
	LocalConfig LocalConfig
	Drafts      []Draft
	Activity    map[string]ActivitySession
	Recent      map[string][]string
	ReadMarkers []ReadMarker
	Statuses    []CustomStatusRecord
	Expandables []ExpandableRecord
	ReduxStates []ReduxDecodedState
	IndexedDB   IndexedDBSummary
}

type RootStateData struct {
	Summary   RootStateSummary
	Downloads map[string]map[string]DownloadRecord
}

type DownloadRecord struct {
	ID         string `json:"id"`
	TeamID     string `json:"teamId"`
	UserID     string `json:"userId"`
	URL        string `json:"url"`
	AppVersion string `json:"appVersion"`
	State      string `json:"downloadState"`
	Path       string `json:"downloadPath"`
}

type rootState struct {
	AppTeams   map[string]json.RawMessage           `json:"appTeams"`
	Downloads  map[string]map[string]DownloadRecord `json:"downloads"`
	Workspaces map[string]json.RawMessage           `json:"workspaces"`
	Teams      map[string]json.RawMessage           `json:"teams"`
}

type LocalConfig struct {
	Teams map[string]DesktopTeam `json:"teams"`
}

type DesktopTeam struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	URL        string      `json:"url"`
	Domain     string      `json:"domain"`
	Token      string      `json:"token,omitempty"`
	UserID     string      `json:"user_id"`
	UserLocale string      `json:"user_locale"`
	Icon       interface{} `json:"icon,omitempty"`
}

type DraftsState struct {
	UnifiedDrafts map[string]Draft `json:"unifiedDrafts"`
}

type Draft struct {
	WorkspaceID    string             `json:"workspace_id,omitempty"`
	UserID         string             `json:"user_id,omitempty"`
	ID             string             `json:"id"`
	ClientDraftID  string             `json:"client_draft_id"`
	IsFromComposer bool               `json:"is_from_composer"`
	DateCreated    float64            `json:"date_created"`
	LastUpdated    float64            `json:"last_updated"`
	LastUpdatedTS  float64            `json:"last_updated_ts"`
	Destinations   []DraftDestination `json:"destinations"`
	Ops            []DraftOp          `json:"ops"`
	FileIDs        []string           `json:"file_ids"`
}

type DraftDestination struct {
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts"`
	Broadcast bool   `json:"broadcast"`
}

type DraftOp struct {
	Insert     interface{}            `json:"insert"`
	Attributes map[string]interface{} `json:"attributes"`
}

type ActivitySession map[string]ActivityRecord

type ActivityRecord struct {
	ID           string `json:"id"`
	StartTime    int64  `json:"startTime"`
	LastActivity int64  `json:"lastActivity"`
	LastLogged   int64  `json:"lastLogged"`
}

type CustomStatus struct {
	ID          string `json:"id"`
	UserID      string `json:"user_id"`
	Text        string `json:"text"`
	Emoji       string `json:"emoji"`
	Duration    string `json:"duration"`
	IsActive    bool   `json:"is_active"`
	DateCreated int64  `json:"date_created"`
	DateExpire  int64  `json:"date_expire"`
}

type CustomStatusRecord struct {
	WorkspaceID string         `json:"workspace_id"`
	UserID      string         `json:"user_id"`
	Statuses    []CustomStatus `json:"statuses"`
}

type ReadMarker struct {
	WorkspaceID string `json:"workspace_id"`
	UserID      string `json:"user_id"`
	ChannelID   string `json:"channel_id"`
	TS          string `json:"ts"`
	Reason      string `json:"reason"`
	PersistKey  string `json:"persist_key"`
}

type ExpandableRecord struct {
	WorkspaceID string   `json:"workspace_id"`
	UserID      string   `json:"user_id"`
	Keys        []string `json:"keys"`
}

func Discover(path string) (Source, error) {
	if path == "" {
		return Source{}, nil
	}
	source := Source{Path: path}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return source, nil
		}
		return Source{}, err
	}
	if !info.IsDir() {
		return Source{}, errors.New("desktop path is not a directory")
	}
	source.Available = true

	root, err := LoadRootState(filepath.Join(path, rootStateFile))
	if err != nil && !os.IsNotExist(err) {
		return Source{}, err
	}
	source.Summary = root.Summary
	return source, nil
}

func Inspect(path string) (Source, error) {
	source, err := Discover(path)
	if err != nil {
		return Source{}, err
	}
	if !source.Available {
		return source, nil
	}

	snapshot, err := SnapshotPath(path)
	if err != nil {
		return Source{}, err
	}
	defer func() { _ = os.RemoveAll(filepath.Dir(snapshot.Root)) }()

	extracted, err := Extract(snapshot.Root)
	if err != nil {
		return Source{}, err
	}
	source.Summary = extracted.RootState.Summary
	source.Local = localSummary(extracted)
	source.IndexedDB = extracted.IndexedDB
	source.IndexedDB.DecodedStateCount = len(extracted.ReduxStates)
	return source, nil
}

func SnapshotPath(path string) (snapshot Snapshot, err error) {
	root, err := makeSnapshotTempDir("", "slacrawl-desktop-*")
	if err != nil {
		return Snapshot{}, err
	}
	keepSnapshot := false
	defer func() {
		if !keepSnapshot {
			_ = os.RemoveAll(root)
		}
	}()

	target := filepath.Join(root, "Slack")
	if err := os.MkdirAll(target, 0o750); err != nil {
		return Snapshot{}, err
	}
	sourceRoot, err := os.OpenRoot(path)
	if err != nil {
		return Snapshot{}, err
	}
	defer func() { _ = sourceRoot.Close() }()
	targetRoot, err := os.OpenRoot(target)
	if err != nil {
		return Snapshot{}, err
	}
	defer func() { _ = targetRoot.Close() }()

	copyTargets := []string{
		rootStateFile,
		"local-settings.json",
		localStorageDir,
		indexedDBDir,
		indexedDBBlobDir,
	}
	for _, relative := range copyTargets {
		relative = filepath.FromSlash(relative)
		if _, err := sourceRoot.Lstat(relative); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Snapshot{}, err
		}
		if err := copyPath(sourceRoot, targetRoot, relative); err != nil {
			return Snapshot{}, err
		}
	}
	keepSnapshot = true
	return Snapshot{Root: target}, nil
}

func Extract(path string) (ExtractedData, error) {
	root, err := LoadRootState(filepath.Join(path, rootStateFile))
	if err != nil && !os.IsNotExist(err) {
		return ExtractedData{}, err
	}

	local, err := ParseLocalStorage(filepath.Join(path, localStorageDir))
	if err != nil && !os.IsNotExist(err) {
		return ExtractedData{}, err
	}

	indexed, err := ScanIndexedDB(filepath.Join(path, indexedDBDir))
	if err != nil && !os.IsNotExist(err) {
		return ExtractedData{}, err
	}
	reduxStates, err := ExtractIndexedDBStates(path)
	if err != nil {
		return ExtractedData{}, err
	}
	indexed.DecodedStateCount = len(reduxStates)

	return ExtractedData{
		RootState:   root,
		LocalConfig: local.LocalConfig,
		Drafts:      local.Drafts,
		Activity:    local.Activity,
		Recent:      local.Recent,
		ReadMarkers: local.ReadMarkers,
		Statuses:    local.Statuses,
		Expandables: local.Expandables,
		ReduxStates: reduxStates,
		IndexedDB:   indexed,
	}, nil
}

func Ingest(ctx context.Context, st *store.Store, sourcePath string, opts IngestOptions) (Source, error) {
	source, err := Discover(sourcePath)
	if err != nil {
		return Source{}, err
	}
	if !source.Available {
		return source, nil
	}

	snapshot, err := SnapshotPath(sourcePath)
	if err != nil {
		return Source{}, err
	}
	defer func() { _ = os.RemoveAll(filepath.Dir(snapshot.Root)) }()

	extracted, err := Extract(snapshot.Root)
	if err != nil {
		return Source{}, err
	}
	source.Summary = extracted.RootState.Summary
	source.Local = localSummary(extracted)
	source.IndexedDB = extracted.IndexedDB
	source.IndexedDB.DecodedStateCount = extracted.IndexedDB.DecodedStateCount

	now := time.Now().UTC()
	filter := newIngestFilter(opts)
	filter.resolveKnownChannelIDs(desktopChannelIDs(extracted))
	source.Summary.AppTeamsKeys = filter.workspaceIDs(source.Summary.AppTeamsKeys)
	channelNames := channelNamesByWorkspaceID(extracted.ReduxStates)
	workspaceCandidates := channelWorkspaceCandidates(extracted.ReduxStates)
	statusByWorkspaceUser := map[string][]CustomStatus{}
	for _, status := range extracted.Statuses {
		statusByWorkspaceUser[status.WorkspaceID+":"+status.UserID] = append(statusByWorkspaceUser[status.WorkspaceID+":"+status.UserID], status.Statuses...)
	}
	for teamID, team := range extracted.LocalConfig.Teams {
		if !filter.allowWorkspace(teamID) {
			continue
		}
		sanitized := team
		sanitized.Token = config.Redact(sanitized.Token)
		userPayload := map[string]any{
			"team":            sanitized,
			"custom_statuses": statusByWorkspaceUser[teamID+":"+team.UserID],
		}
		if err := st.UpsertWorkspace(ctx, store.Workspace{
			ID:        teamID,
			Name:      fallback(sanitized.Name, teamID),
			Domain:    sanitized.Domain,
			RawJSON:   store.MarshalRaw(sanitized),
			UpdatedAt: now,
		}); err != nil {
			return Source{}, err
		}
		if team.UserID != "" {
			if err := upsertDesktopUser(ctx, st, store.User{
				ID:          team.UserID,
				WorkspaceID: teamID,
				Name:        fallback(team.UserID, team.UserID),
				DisplayName: fallback(team.Name, team.UserID),
				Title:       userTitle(statusByWorkspaceUser[teamID+":"+team.UserID]),
				RawJSON:     store.MarshalRaw(userPayload),
				UpdatedAt:   now,
			}); err != nil {
				return Source{}, err
			}
		}
	}

	channelHints := map[string]store.Channel{}
	for workspaceID, channelIDs := range extracted.Recent {
		for _, channelID := range channelIDs {
			resolvedWorkspaceID, ok := resolveChannelWorkspace(channelID, workspaceID, workspaceCandidates)
			if !ok {
				continue
			}
			if !filter.allowChannelNames(resolvedWorkspaceID, channelID, channelNames.get(resolvedWorkspaceID, channelID)) {
				continue
			}
			mergeChannelHint(channelHints, store.Channel{
				ID:          channelID,
				WorkspaceID: resolvedWorkspaceID,
				Name:        channelID,
				Kind:        "desktop_recent",
				RawJSON:     store.MarshalRaw(map[string]any{"workspace_id": resolvedWorkspaceID, "persist_workspace_id": workspaceID, "channel_id": channelID, "source": "recentlyJoinedChannels"}),
				UpdatedAt:   now,
			})
		}
	}
	for _, marker := range extracted.ReadMarkers {
		workspaceID, ok := resolveChannelWorkspace(marker.ChannelID, marker.WorkspaceID, workspaceCandidates)
		if !ok {
			continue
		}
		if !filter.allowChannelNames(workspaceID, marker.ChannelID, channelNames.get(workspaceID, marker.ChannelID)) {
			continue
		}
		mergeChannelHint(channelHints, store.Channel{
			ID:          marker.ChannelID,
			WorkspaceID: workspaceID,
			Name:        marker.ChannelID,
			Kind:        "desktop_mark",
			RawJSON:     store.MarshalRaw(marker),
			UpdatedAt:   now,
		})
	}
	for _, draft := range extracted.Drafts {
		if len(draft.Destinations) == 0 {
			continue
		}
		channelID := draft.Destinations[0].ChannelID
		workspaceID := draft.WorkspaceID
		if workspaceID == "" {
			workspaceID = workspaceForDraft(extracted.LocalConfig.Teams, channelID, draft)
		}
		if workspaceID == "" {
			if resolvedWorkspaceID, ok := resolveChannelWorkspace(channelID, "", workspaceCandidates); ok {
				workspaceID = resolvedWorkspaceID
			} else {
				if filter.workspaceID != "" {
					continue
				}
				workspaceID = firstWorkspaceID(extracted.LocalConfig.Teams)
			}
		}
		resolvedWorkspaceID, ok := resolveChannelWorkspace(channelID, workspaceID, workspaceCandidates)
		if !ok {
			continue
		}
		workspaceID = resolvedWorkspaceID
		if !filter.allowChannelNames(workspaceID, channelID, channelNames.get(workspaceID, channelID)) {
			continue
		}

		mergeChannelHint(channelHints, store.Channel{
			ID:          channelID,
			WorkspaceID: workspaceID,
			Name:        inferredChannelName(channelID, draft),
			Kind:        "desktop_draft",
			RawJSON:     store.MarshalRaw(map[string]any{"workspace_id": workspaceID, "channel_id": channelID, "source": "draft"}),
			UpdatedAt:   now,
		})

		message := store.Message{
			ChannelID:      channelID,
			TS:             draftTS(draft),
			WorkspaceID:    workspaceID,
			UserID:         fallback(draft.UserID, extracted.LocalConfig.Teams[workspaceID].UserID),
			Subtype:        "desktop_draft",
			ClientMsgID:    draft.ClientDraftID,
			ThreadTS:       draft.Destinations[0].ThreadTS,
			Text:           draftText(draft),
			NormalizedText: strings.TrimSpace(draftText(draft)),
			SourceRank:     3,
			SourceName:     draftSourceName,
			RawJSON:        store.MarshalRaw(draft),
			UpdatedAt:      now,
		}
		if message.Text == "" {
			continue
		}
		if err := upsertDesktopMessage(ctx, st, message, nil); err != nil {
			return Source{}, err
		}
		if legacyTS := legacyDraftTS(draft); legacyTS != message.TS {
			if _, err := st.DeleteMessageBySource(ctx, workspaceID, channelID, legacyTS, draftSourceName); err != nil {
				return Source{}, err
			}
		}
	}
	for _, channel := range channelHints {
		if err := st.UpsertChannel(ctx, channel); err != nil {
			return Source{}, err
		}
	}
	if err := ingestReduxStates(ctx, st, extracted.ReduxStates, now, filter); err != nil {
		return Source{}, err
	}

	if err := st.SetSyncState(ctx, sourceName, "root_state", "path", source.Path); err != nil {
		return Source{}, err
	}
	if err := st.SetSyncState(ctx, sourceName, "root_state", "app_teams", strings.Join(source.Summary.AppTeamsKeys, ",")); err != nil {
		return Source{}, err
	}
	if err := st.SetSyncState(ctx, sourceName, "local_storage", "draft_count", intString(source.Local.DraftCount)); err != nil {
		return Source{}, err
	}
	if err := st.SetSyncState(ctx, sourceName, "indexeddb", "object_stores", strings.Join(source.IndexedDB.ObjectStores, ",")); err != nil {
		return Source{}, err
	}
	if err := st.SetSyncState(ctx, sourceName, "indexeddb", "decoded_state_count", intString(source.IndexedDB.DecodedStateCount)); err != nil {
		return Source{}, err
	}
	if err := st.SetSyncState(ctx, sourceName, "local_storage", "workspace_count", intString(source.Local.WorkspaceCount)); err != nil {
		return Source{}, err
	}
	if err := st.SetSyncState(ctx, sourceName, "local_storage", "activity_team_count", intString(source.Local.ActivityTeamCount)); err != nil {
		return Source{}, err
	}
	if err := st.SetSyncState(ctx, sourceName, "local_storage", "recent_channel_count", intString(source.Local.RecentChannelCount)); err != nil {
		return Source{}, err
	}
	if err := st.SetSyncState(ctx, sourceName, "local_storage", "read_marker_count", intString(source.Local.ReadMarkerCount)); err != nil {
		return Source{}, err
	}
	if err := st.SetSyncState(ctx, sourceName, "local_storage", "custom_status_count", intString(source.Local.CustomStatusCount)); err != nil {
		return Source{}, err
	}
	if err := st.SetSyncState(ctx, sourceName, "local_storage", "expandable_count", intString(source.Local.ExpandableCount)); err != nil {
		return Source{}, err
	}
	for teamID, downloads := range extracted.RootState.Downloads {
		if !filter.allowWorkspace(teamID) {
			continue
		}
		if err := st.SetSyncState(ctx, sourceName, "downloads", teamID, intString(len(downloads))); err != nil {
			return Source{}, err
		}
	}
	for _, marker := range extracted.ReadMarkers {
		workspaceID, ok := resolveChannelWorkspace(marker.ChannelID, marker.WorkspaceID, workspaceCandidates)
		if !ok {
			continue
		}
		if !filter.allowChannelNames(workspaceID, marker.ChannelID, channelNames.get(workspaceID, marker.ChannelID)) {
			continue
		}
		if err := st.SetSyncState(ctx, sourceName, "read_marker", marker.ChannelID, marker.TS); err != nil {
			return Source{}, err
		}
	}
	for _, expandable := range extracted.Expandables {
		if !filter.allowWorkspace(expandable.WorkspaceID) {
			continue
		}
		if err := st.SetSyncState(ctx, sourceName, "expandables", expandable.WorkspaceID+":"+expandable.UserID, intString(len(expandable.Keys))); err != nil {
			return Source{}, err
		}
	}
	for _, status := range extracted.Statuses {
		if !filter.allowWorkspace(status.WorkspaceID) {
			continue
		}
		if err := st.SetSyncState(ctx, sourceName, "custom_status", status.WorkspaceID+":"+status.UserID, intString(len(status.Statuses))); err != nil {
			return Source{}, err
		}
	}

	return source, nil
}

func newIngestFilter(opts IngestOptions) ingestFilter {
	excludeChannels, excludeSelectors := channelSelectorSet(opts.ExcludeChannels)
	return ingestFilter{
		workspaceID:      strings.TrimSpace(opts.WorkspaceID),
		channels:         stringSet(opts.Channels),
		excludeChannels:  excludeChannels,
		excludeSelectors: excludeSelectors,
	}
}

func stringSet(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			set[value] = struct{}{}
		}
	}
	return set
}

func channelSelectorSet(values []string) (map[string]struct{}, []channelSelector) {
	if len(values) == 0 {
		return nil, nil
	}
	set := make(map[string]struct{}, len(values))
	selectors := make([]channelSelector, 0, len(values))
	for _, value := range values {
		raw := strings.TrimSpace(value)
		selectorValue := raw
		explicitID := false
		if strings.HasPrefix(strings.ToLower(selectorValue), "id:") {
			explicitID = true
			selectorValue = strings.TrimSpace(selectorValue[len("id:"):])
		}
		selector := normalizeChannelSelector(selectorValue)
		if selector != "" {
			set[selector] = struct{}{}
			selectors = append(selectors, channelSelector{raw: raw, normalized: selector, explicitID: explicitID})
		}
	}
	return set, selectors
}

func normalizeChannelSelector(value string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(value), "#"))
}

func (f ingestFilter) allowWorkspace(workspaceID string) bool {
	workspaceID = strings.TrimSpace(workspaceID)
	if f.workspaceID == "" {
		return true
	}
	return workspaceID == f.workspaceID
}

func (f ingestFilter) allowChannelNames(workspaceID, channelID string, channelNames []string) bool {
	channelID = strings.TrimSpace(channelID)
	if !f.allowWorkspace(workspaceID) {
		return false
	}
	if f.excludesChannel(channelID, channelNames) {
		return false
	}
	if len(f.channels) == 0 {
		return true
	}
	if channelID == "" {
		return false
	}
	_, allowed := f.channels[channelID]
	return allowed
}

func (f ingestFilter) excludesChannel(channelID string, channelNames []string) bool {
	if len(f.excludeChannels) == 0 {
		return false
	}
	if _, excluded := f.excludeChannels[normalizeChannelSelector(channelID)]; excluded {
		return true
	}
	if len(channelNames) == 0 {
		return false
	}
	sawName := false
	for _, channelName := range channelNames {
		channelName = normalizeChannelSelector(channelName)
		if channelName == "" {
			continue
		}
		sawName = true
		if _, excluded := f.excludeChannels[channelName]; excluded {
			return true
		}
	}
	if !sawName {
		return false
	}
	return false
}

func (f *ingestFilter) resolveKnownChannelIDs(channelIDs map[string]struct{}) {
	f.hasNameExclude = false
}

func desktopChannelIDs(extracted ExtractedData) map[string]struct{} {
	ids := map[string]struct{}{}
	add := func(channelID string) {
		channelID = normalizeChannelSelector(channelID)
		if channelID != "" {
			ids[channelID] = struct{}{}
		}
	}
	for _, channelIDs := range extracted.Recent {
		for _, channelID := range channelIDs {
			add(channelID)
		}
	}
	for _, marker := range extracted.ReadMarkers {
		add(marker.ChannelID)
	}
	for _, draft := range extracted.Drafts {
		for _, destination := range draft.Destinations {
			add(destination.ChannelID)
		}
	}
	for _, state := range extracted.ReduxStates {
		for _, channel := range state.Channels {
			add(channel.ID)
		}
		for _, message := range state.Messages {
			add(message.Channel)
		}
	}
	return ids
}

func (f ingestFilter) workspaceIDs(ids []string) []string {
	if f.workspaceID == "" {
		return ids
	}
	filtered := make([]string, 0, len(ids))
	for _, id := range ids {
		if f.allowWorkspace(id) {
			filtered = append(filtered, id)
		}
	}
	return filtered
}

type channelNameHints map[string][]string

func channelNamesByWorkspaceID(states []ReduxDecodedState) channelNameHints {
	names := channelNameHints{}
	for _, state := range states {
		memberNames := map[string]string{}
		for _, member := range state.Members {
			memberNames[member.ID] = firstNonEmpty(member.Profile.DisplayName, member.Profile.RealName, member.Name, member.Real, member.ID)
		}
		for _, channel := range state.Channels {
			workspaceID := channelContextWorkspaceID(channel.ContextTeamID, state.WorkspaceID)
			key := channelNameKey(workspaceID, channel.ID)
			if channel.ID == "" {
				continue
			}
			aliases := reduxResolvedChannelNames(channel, memberNames)
			if len(aliases) > 0 {
				names[key] = uniqueNonEmptyStrings(append(names[key], aliases...))
			} else if _, exists := names[key]; !exists {
				names[key] = nil
			}
		}
	}
	return names
}

func (h channelNameHints) get(workspaceID, channelID string) []string {
	return h[channelNameKey(workspaceID, channelID)]
}

func channelNameKey(workspaceID, channelID string) string {
	return strings.TrimSpace(workspaceID) + "\x00" + strings.TrimSpace(channelID)
}

func upsertDesktopUser(ctx context.Context, st *store.Store, user store.User) error {
	err := st.UpsertUser(ctx, user)
	if err == nil {
		return nil
	}
	if store.IsWorkspaceCollision(err, "user") {
		return nil
	}
	return err
}

func upsertDesktopMessage(ctx context.Context, st *store.Store, message store.Message, mentions []store.Mention) error {
	err := st.UpsertMessage(ctx, message, mentions)
	if err == nil {
		return nil
	}
	if store.IsWorkspaceCollision(err, "message") {
		return nil
	}
	return err
}

func localSummary(extracted ExtractedData) LocalStorageSummary {
	return LocalStorageSummary{
		WorkspaceCount:     len(extracted.LocalConfig.Teams),
		DraftCount:         len(extracted.Drafts),
		ActivityTeamCount:  len(extracted.Activity),
		RecentChannelCount: countRecentChannels(extracted.Recent),
		ReadMarkerCount:    len(extracted.ReadMarkers),
		CustomStatusCount:  countCustomStatuses(extracted.Statuses),
		ExpandableCount:    countExpandables(extracted.Expandables),
	}
}

func countRecentChannels(recent map[string][]string) int {
	total := 0
	for _, ids := range recent {
		total += len(ids)
	}
	return total
}

func countCustomStatuses(records []CustomStatusRecord) int {
	total := 0
	for _, record := range records {
		total += len(record.Statuses)
	}
	return total
}

func countExpandables(records []ExpandableRecord) int {
	total := 0
	for _, record := range records {
		total += len(record.Keys)
	}
	return total
}

func LoadRootState(path string) (RootStateData, error) {
	data, err := os.ReadFile(path) //nolint:gosec // Reads the explicit Slack desktop state file selected by discovery.
	if err != nil {
		return RootStateData{}, err
	}

	var state rootState
	if err := json.Unmarshal(data, &state); err != nil {
		return RootStateData{}, err
	}

	keys := make([]string, 0, len(state.AppTeams))
	for key := range state.AppTeams {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	downloadItemCount := 0
	for _, teamDownloads := range state.Downloads {
		downloadItemCount += len(teamDownloads)
	}

	return RootStateData{
		Summary: RootStateSummary{
			AppTeamsKeys:      keys,
			WorkspaceCount:    len(state.Workspaces),
			TeamsCount:        len(state.Teams),
			DownloadTeamCount: len(state.Downloads),
			DownloadItemCount: downloadItemCount,
		},
		Downloads: state.Downloads,
	}, nil
}

type localStorageData struct {
	Summary     LocalStorageSummary
	LocalConfig LocalConfig
	Drafts      []Draft
	Activity    map[string]ActivitySession
	Recent      map[string][]string
	ReadMarkers []ReadMarker
	Statuses    []CustomStatusRecord
	Expandables []ExpandableRecord
}

func ParseLocalStorage(path string) (localStorageData, error) {
	db, err := leveldb.OpenFile(path, &opt.Options{ReadOnly: true})
	if err != nil {
		return localStorageData{}, err
	}
	defer func() { _ = db.Close() }()

	var (
		configData LocalConfig
		drafts     []Draft
		activity   = map[string]ActivitySession{}
		recent     = map[string][]string{}
		markers    []ReadMarker
		statuses   []CustomStatusRecord
		expand     []ExpandableRecord
	)

	iter := db.NewIterator(nil, nil)
	defer iter.Release()
	for iter.Next() {
		key := cleanKey(iter.Key())
		if !strings.HasPrefix(key, "_https://app.slack.com") {
			continue
		}
		value := jsonPayload(iter.Value())
		if len(value) == 0 {
			continue
		}

		switch {
		case strings.Contains(key, "localConfig_v2"):
			var payload struct {
				Teams map[string]DesktopTeam `json:"teams"`
			}
			if err := json.Unmarshal(value, &payload); err == nil {
				configData.Teams = payload.Teams
			}
		case strings.Contains(key, "persist-v1::") && strings.HasSuffix(key, "::drafts"):
			workspaceID, userID, ok := persistContext(key)
			if !ok {
				continue
			}
			var payload DraftsState
			if err := json.Unmarshal(value, &payload); err == nil {
				for id, draft := range payload.UnifiedDrafts {
					if draft.ClientDraftID == "" {
						draft.ClientDraftID = id
					}
					if draft.ID == "" {
						draft.ID = id
					}
					draft.WorkspaceID = workspaceID
					draft.UserID = userID
					drafts = append(drafts, draft)
				}
			}
		case strings.Contains(key, "activitySession_"):
			teamID := strings.TrimPrefix(key, "_https://app.slack.comactivitySession_")
			var payload ActivitySession
			if err := json.Unmarshal(value, &payload); err == nil {
				activity[teamID] = payload
			}
		case strings.Contains(key, "persist-v1::") && strings.HasSuffix(key, "::recentlyJoinedChannels"):
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(value, &payload); err == nil {
				parts := strings.Split(key, "::")
				if len(parts) >= 2 {
					teamID := parts[1]
					for channelID := range payload {
						recent[teamID] = append(recent[teamID], channelID)
					}
				}
			}
		case strings.Contains(key, "persist-v1::") && strings.HasSuffix(key, "::customStatus"):
			workspaceID, userID, ok := persistContext(key)
			if !ok {
				continue
			}
			var payload map[string]CustomStatus
			if err := json.Unmarshal(value, &payload); err == nil {
				record := CustomStatusRecord{WorkspaceID: workspaceID, UserID: userID}
				for id, status := range payload {
					if status.ID == "" {
						status.ID = id
					}
					if status.UserID == "" {
						status.UserID = userID
					}
					record.Statuses = append(record.Statuses, status)
				}
				sort.Slice(record.Statuses, func(i, j int) bool {
					return record.Statuses[i].DateCreated < record.Statuses[j].DateCreated
				})
				statuses = append(statuses, record)
			}
		case strings.Contains(key, "persist-v1::") && strings.HasSuffix(key, "::persistedApiCalls"):
			workspaceID, userID, ok := persistContext(key)
			if !ok {
				continue
			}
			var payload map[string]persistedAPICall
			if err := json.Unmarshal(value, &payload); err == nil {
				for persistKey, call := range payload {
					if call.Method != "conversations.mark" {
						continue
					}
					channelID, _ := call.Args["channel"].(string)
					ts, _ := call.Args["ts"].(string)
					if channelID == "" || ts == "" {
						continue
					}
					markers = append(markers, ReadMarker{
						WorkspaceID: workspaceID,
						UserID:      userID,
						ChannelID:   channelID,
						TS:          ts,
						Reason:      call.Reason,
						PersistKey:  fallback(call.PersistKey, persistKey),
					})
				}
			}
		case strings.Contains(key, "persist-v1::") && strings.HasSuffix(key, "::expandables"):
			workspaceID, userID, ok := persistContext(key)
			if !ok {
				continue
			}
			var payload map[string]bool
			if err := json.Unmarshal(value, &payload); err == nil {
				record := ExpandableRecord{WorkspaceID: workspaceID, UserID: userID}
				for expandableKey := range payload {
					record.Keys = append(record.Keys, expandableKey)
				}
				sort.Strings(record.Keys)
				expand = append(expand, record)
			}
		}
	}
	if err := iter.Error(); err != nil {
		return localStorageData{}, err
	}

	for teamID := range recent {
		sort.Strings(recent[teamID])
	}

	recentCount := 0
	for _, ids := range recent {
		recentCount += len(ids)
	}
	customStatusCount := 0
	for _, record := range statuses {
		customStatusCount += len(record.Statuses)
	}
	expandableCount := 0
	for _, record := range expand {
		expandableCount += len(record.Keys)
	}

	return localStorageData{
		Summary: LocalStorageSummary{
			WorkspaceCount:     len(configData.Teams),
			DraftCount:         len(drafts),
			ActivityTeamCount:  len(activity),
			RecentChannelCount: recentCount,
			ReadMarkerCount:    len(markers),
			CustomStatusCount:  customStatusCount,
			ExpandableCount:    expandableCount,
		},
		LocalConfig: configData,
		Drafts:      drafts,
		Activity:    activity,
		Recent:      recent,
		ReadMarkers: markers,
		Statuses:    statuses,
		Expandables: expand,
	}, nil
}

type persistedAPICall struct {
	Method     string         `json:"method"`
	Args       map[string]any `json:"args"`
	Reason     string         `json:"reason"`
	PersistKey string         `json:"persistKey"`
}

func ScanIndexedDB(path string) (IndexedDBSummary, error) {
	db, err := leveldb.OpenFile(path, &opt.Options{ReadOnly: true, Comparer: indexedDBComparer{}})
	if err != nil {
		return IndexedDBSummary{}, err
	}
	defer func() { _ = db.Close() }()

	stores := map[string]struct{}{}
	iter := db.NewIterator(nil, nil)
	defer iter.Release()
	for iter.Next() {
		key := cleanKey(iter.Key())
		if !strings.Contains(key, "#objectStore-") {
			continue
		}
		idx := strings.Index(key, "#objectStore-")
		stores[key[idx+1:]] = struct{}{}
	}
	if err := iter.Error(); err != nil {
		return IndexedDBSummary{}, err
	}

	names := make([]string, 0, len(stores))
	for name := range stores {
		names = append(names, name)
	}
	sort.Strings(names)
	return IndexedDBSummary{ObjectStores: names}, nil
}

type indexedDBComparer struct{}

func (indexedDBComparer) Compare(a, b []byte) int { return bytes.Compare(a, b) }
func (indexedDBComparer) Name() string            { return "idb_cmp1" }
func (indexedDBComparer) Separator(dst, a, b []byte) []byte {
	return comparer.DefaultComparer.Separator(dst, a, b)
}

func (indexedDBComparer) Successor(dst, b []byte) []byte {
	return comparer.DefaultComparer.Successor(dst, b)
}

func draftText(draft Draft) string {
	var builder strings.Builder
	for _, op := range draft.Ops {
		switch value := op.Insert.(type) {
		case string:
			builder.WriteString(value)
		default:
			continue
		}
	}
	return strings.TrimSpace(builder.String())
}

func draftTS(draft Draft) string {
	id := draftID(draft)
	if draft.LastUpdatedTS > 0 {
		return "draft:" + trimFloat(draft.LastUpdatedTS) + ":" + id
	}
	if draft.LastUpdated > 0 {
		return "draft:" + trimFloat(draft.LastUpdated) + ":" + id
	}
	return "draft:" + id
}

func legacyDraftTS(draft Draft) string {
	id := draftID(draft)
	if draft.LastUpdatedTS > 0 {
		return "draft:" + legacyTrimFloat(draft.LastUpdatedTS) + ":" + id
	}
	if draft.LastUpdated > 0 {
		return "draft:" + legacyTrimFloat(draft.LastUpdated) + ":" + id
	}
	return "draft:" + id
}

func draftID(draft Draft) string {
	id := fallback(draft.ClientDraftID, draft.ID)
	if draft.WorkspaceID == "" {
		return id
	}
	return draft.WorkspaceID + ":" + id
}

func trimFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func legacyTrimFloat(value float64) string {
	return strings.TrimRight(strings.TrimRight(trimFloat(value), "0"), ".")
}

func firstWorkspaceID(teams map[string]DesktopTeam) string {
	ids := make([]string, 0, len(teams))
	for id := range teams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func workspaceForDraft(teams map[string]DesktopTeam, channelID string, draft Draft) string {
	_ = channelID
	for workspaceID, team := range teams {
		if team.UserID != "" && hasDraftForWorkspace(workspaceID, draft) {
			return workspaceID
		}
	}
	return ""
}

func hasDraftForWorkspace(workspaceID string, draft Draft) bool {
	for _, destination := range draft.Destinations {
		if strings.HasPrefix(destination.ChannelID, "C") || strings.HasPrefix(destination.ChannelID, "D") || strings.HasPrefix(destination.ChannelID, "G") {
			return true
		}
		if strings.Contains(destination.ChannelID, workspaceID) {
			return true
		}
	}
	return false
}

func inferredChannelName(channelID string, draft Draft) string {
	_ = draft
	return channelID
}

func persistContext(key string) (workspaceID string, userID string, ok bool) {
	parts := strings.Split(key, "::")
	if len(parts) < 4 {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func userTitle(statuses []CustomStatus) string {
	active := activeStatus(statuses)
	if active == "" {
		return "desktop_local_user"
	}
	return "desktop_local_user | " + active
}

func activeStatus(statuses []CustomStatus) string {
	for _, status := range statuses {
		if !status.IsActive {
			continue
		}
		if status.Emoji != "" && status.Text != "" {
			return status.Emoji + " " + status.Text
		}
		if status.Text != "" {
			return status.Text
		}
		if status.Emoji != "" {
			return status.Emoji
		}
	}
	return ""
}

func mergeChannelHint(hints map[string]store.Channel, candidate store.Channel) {
	current, ok := hints[candidate.ID]
	if !ok {
		hints[candidate.ID] = candidate
		return
	}
	if channelHintPriority(candidate.Kind) < channelHintPriority(current.Kind) {
		hints[candidate.ID] = candidate
		return
	}
	if current.WorkspaceID == "" && candidate.WorkspaceID != "" {
		current.WorkspaceID = candidate.WorkspaceID
	}
	if current.Name == "" && candidate.Name != "" {
		current.Name = candidate.Name
	}
	if current.RawJSON == "" || current.RawJSON == "{}" {
		current.RawJSON = candidate.RawJSON
	}
	current.UpdatedAt = candidate.UpdatedAt
	hints[candidate.ID] = current
}

func channelHintPriority(kind string) int {
	switch kind {
	case "desktop_draft":
		return 1
	case "desktop_recent":
		return 2
	case "desktop_mark":
		return 3
	default:
		return 100
	}
}

func fallback(value string, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func intString(value int) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func copyPath(srcRoot *os.Root, dstRoot *os.Root, relative string) error {
	info, err := srcRoot.Lstat(relative)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("desktop snapshot source contains a symbolic link")
	}
	if info.IsDir() {
		if err := dstRoot.MkdirAll(relative, info.Mode().Perm()); err != nil {
			return err
		}
		dir, err := srcRoot.Open(relative)
		if err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(-1)
		closeErr := dir.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		for _, entry := range entries {
			if err := copyPath(srcRoot, dstRoot, filepath.Join(relative, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return errors.New("desktop snapshot source contains a special file")
	}
	if err := dstRoot.MkdirAll(filepath.Dir(relative), 0o750); err != nil {
		return err
	}
	data, err := srcRoot.ReadFile(relative)
	if err != nil {
		return err
	}
	return dstRoot.WriteFile(relative, data, info.Mode().Perm())
}

func cleanKey(key []byte) string {
	return strings.Map(func(r rune) rune {
		if r < 32 {
			return -1
		}
		return r
	}, string(key))
}

func jsonPayload(value []byte) []byte {
	for i, b := range value {
		if b == '{' || b == '[' {
			return value[i:]
		}
	}
	return nil
}
