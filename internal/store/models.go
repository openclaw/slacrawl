package store

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

type Workspace struct {
	ID           string
	Name         string
	Domain       string
	EnterpriseID string
	RawJSON      string
	UpdatedAt    time.Time
}

type Channel struct {
	ID          string
	WorkspaceID string
	Name        string
	Kind        string
	Topic       string
	Purpose     string
	IsPrivate   bool
	IsArchived  bool
	IsShared    bool
	IsGeneral   bool
	RawJSON     string
	UpdatedAt   time.Time
}

type User struct {
	ID          string
	WorkspaceID string
	Name        string
	RealName    string
	DisplayName string
	Title       string
	IsBot       bool
	IsDeleted   bool
	RawJSON     string
	UpdatedAt   time.Time
}

type Message struct {
	ChannelID      string
	TS             string
	WorkspaceID    string
	UserID         string
	Subtype        string
	ClientMsgID    string
	ThreadTS       string
	ParentUserID   string
	Text           string
	NormalizedText string
	ReplyCount     int
	LatestReply    string
	EditedTS       string
	DeletedTS      string
	SourceRank     int
	SourceName     string
	RawJSON        string
	UpdatedAt      time.Time
	Files          []MessageFile
}

type MessageWrite struct {
	Message                Message
	Mentions               []Mention
	PreserveHigherPriority bool
	EnforceRetention       bool
	SkipWorkspaceCollision bool
	// SkipUnchangedProviderRow avoids derived-index rewrites for provider rows
	// whose scalar state, mentions, and event head already match. It is not a
	// general repair path and deliberately refuses messages with file payloads.
	SkipUnchangedProviderRow bool
}

type SyncStateWrite struct {
	SourceName string
	EntityType string
	EntityID   string
	Value      string
}

type SyncStateDelete struct {
	SourceName string
	EntityType string
	EntityID   string
}

type WriteBatch struct {
	Workspaces               []Workspace
	Channels                 []Channel
	Users                    []User
	Messages                 []MessageWrite
	SyncStates               []SyncStateWrite
	SyncStateDeletes         []SyncStateDelete
	HistoryGuard             *APIHistoryAttempt
	MCPHistoryGuard          *MCPHistoryWork
	PendingThreads           []ThreadWork
	ThreadGuard              *ThreadWork
	ThreadDiscovery          *ThreadWorkDiscovery
	PendingThreadOnCollision *ThreadWork
}

type CollisionSkip struct {
	ChannelID string
	TS        string
	Err       error
}

type WriteBatchResult struct {
	MessagesWritten   int
	CollisionsSkipped []CollisionSkip
	PendingThreads    []ThreadWork
	ThreadWorkRevoked bool
}

type Mention struct {
	Type        string
	TargetID    string
	DisplayText string
}

type MessageFile struct {
	WorkspaceID        string
	ChannelID          string
	TS                 string
	FileID             string
	UserID             string
	Name               string
	Title              string
	Mimetype           string
	Filetype           string
	PrettyType         string
	Mode               string
	Size               int64
	URLPrivate         string
	URLPrivateDownload string
	Permalink          string
	IsPublic           bool
	PlainText          string
	PreviewPlainText   string
	MediaPath          string
	ContentSHA256      string
	ContentSize        int64
	FetchedAt          string
	FetchStatus        string
	FetchError         string
	RawJSON            string
	UpdatedAt          time.Time
}

type Status struct {
	NativeChanges *NativeChangeStatus `json:"native_changes,omitempty"`
	Workspaces    int                 `json:"workspaces"`
	Channels      int                 `json:"channels"`
	Users         int                 `json:"users"`
	Messages      int                 `json:"messages"`
	LastSyncAt    time.Time           `json:"last_sync_at"`
	ThreadState   string              `json:"thread_state"`
}

type MessageRow struct {
	WorkspaceID    string `json:"workspace_id"`
	WorkspaceName  string `json:"workspace_name,omitempty"`
	ChannelID      string `json:"channel_id"`
	ChannelName    string `json:"channel_name,omitempty"`
	TS             string `json:"ts"`
	UserID         string `json:"user_id"`
	UserName       string `json:"user_name,omitempty"`
	Text           string `json:"text"`
	NormalizedText string `json:"normalized_text"`
	ThreadTS       string `json:"thread_ts"`
	ReplyCount     int    `json:"reply_count"`
	LatestReply    string `json:"latest_reply"`
	Subtype        string `json:"subtype"`
	SourceName     string `json:"source_name,omitempty"`
}

// Keep this projection in the same order as scanMessageRows.
const messageRowSelect = `m.workspace_id, coalesce(w.name, ''), m.channel_id, coalesce(c.name, ''),
       m.ts, coalesce(m.user_id, ''),
       coalesce(nullif(u.display_name, ''), nullif(u.real_name, ''), nullif(u.name, ''), ''),
       m.text, m.normalized_text, coalesce(m.thread_ts, ''), m.reply_count,
       coalesce(m.latest_reply, ''), coalesce(m.subtype, ''), m.source_name`

const messageRowJoins = `
left join workspaces w on w.id = m.workspace_id
left join channels c on c.id = m.channel_id
left join users u on u.id = m.user_id`

type SearchMode string

const (
	SearchModeAuto   SearchMode = "auto"
	SearchModePhrase SearchMode = "phrase"
	SearchModeTerms  SearchMode = "terms"
	SearchModeRawFTS SearchMode = "raw-fts"
)

type SearchOptions struct {
	WorkspaceID string
	Query       string
	Limit       int
	Mode        SearchMode
}

type WorkspaceCollisionError struct {
	Entity              string
	ID                  string
	ExistingWorkspaceID string
	WorkspaceID         string
}

func (e *WorkspaceCollisionError) Error() string {
	return fmt.Sprintf("%s %q already belongs to workspace %q, not %q", e.Entity, e.ID, e.ExistingWorkspaceID, e.WorkspaceID)
}

func IsWorkspaceCollision(err error, entity string) bool {
	var collision *WorkspaceCollisionError
	if !errors.As(err, &collision) {
		return false
	}
	return entity == "" || collision.Entity == entity
}

type MentionRow struct {
	WorkspaceID string `json:"workspace_id"`
	ChannelID   string `json:"channel_id"`
	TS          string `json:"ts"`
	MentionType string `json:"mention_type"`
	TargetID    string `json:"target_id"`
	DisplayText string `json:"display_text"`
}

type messageMentionDisplay struct {
	target  string
	display string
}

type UserRow struct {
	WorkspaceID string `json:"workspace_id"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	RealName    string `json:"real_name"`
	DisplayName string `json:"display_name"`
	Title       string `json:"title"`
}

type ChannelRow struct {
	WorkspaceID string `json:"workspace_id"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
}

type FileListOptions struct {
	WorkspaceID string
	ChannelID   string
	UserID      string
	FileID      string
	Filename    string
	ContentType string
	Since       time.Time
	Before      time.Time
	Limit       int
	MissingOnly bool
}

type FileRow struct {
	WorkspaceID        string    `json:"workspace_id"`
	ChannelID          string    `json:"channel_id"`
	TS                 string    `json:"ts"`
	FileID             string    `json:"file_id"`
	UserID             string    `json:"user_id,omitempty"`
	Name               string    `json:"name"`
	Title              string    `json:"title,omitempty"`
	Mimetype           string    `json:"mimetype,omitempty"`
	Filetype           string    `json:"filetype,omitempty"`
	PrettyType         string    `json:"pretty_type,omitempty"`
	Mode               string    `json:"mode,omitempty"`
	Size               int64     `json:"size"`
	URLPrivate         string    `json:"url_private,omitempty"`
	URLPrivateDownload string    `json:"url_private_download,omitempty"`
	Permalink          string    `json:"permalink,omitempty"`
	IsPublic           bool      `json:"is_public"`
	PlainText          string    `json:"plain_text,omitempty"`
	PreviewPlainText   string    `json:"preview_plain_text,omitempty"`
	MediaPath          string    `json:"media_path,omitempty"`
	ContentSHA256      string    `json:"content_sha256,omitempty"`
	ContentSize        int64     `json:"content_size,omitempty"`
	FetchedAt          time.Time `json:"fetched_at,omitzero"`
	FetchStatus        string    `json:"fetch_status,omitempty"`
	FetchError         string    `json:"fetch_error,omitempty"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type FileMediaUpdate struct {
	ChannelID     string
	TS            string
	FileID        string
	MediaPath     string
	ContentSHA256 string
	ContentSize   int64
	FetchedAt     string
	FetchStatus   string
	FetchError    string
}

type ChannelSyncCursor struct {
	ID              string
	LatestTS        string
	RetentionFloor  string
	RetentionSeeded bool
}

func (c ChannelSyncCursor) ApplyRetentionFloor(oldest string) string {
	if c.RetentionFloor == "" {
		return oldest
	}
	if oldest == "" {
		return c.RetentionFloor
	}
	oldestValue, oldestErr := strconv.ParseFloat(oldest, 64)
	floorValue, floorErr := strconv.ParseFloat(c.RetentionFloor, 64)
	if oldestErr != nil || floorErr != nil || oldestValue < floorValue {
		return c.RetentionFloor
	}
	return oldest
}

func ShouldEnforceRetention(oldest, floor string, restoreRequested bool) bool {
	if !restoreRequested {
		return true
	}
	if oldest == "" || floor == "" {
		return false
	}
	oldestValue, oldestOK := parseRetentionTimestamp(oldest)
	floorValue, floorOK := parseRetentionTimestamp(floor)
	if !oldestOK || !floorOK {
		return true
	}
	return oldestValue >= floorValue
}

type ThreadRoot struct {
	ChannelID string
	TS        string
}

type SyncStateRow struct {
	SourceName string `json:"source_name"`
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Value      string `json:"value"`
}
