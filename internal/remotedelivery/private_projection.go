// Package remotedelivery owns Slacrawl's private projection, durable outbox and
// explicit delivery driver. Callers qualify bindings and own source acquisition.
package remotedelivery

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/store"
)

// PrivateArchiveProfile identifies the fixed private content layout below.
const PrivateArchiveProfile = "slacrawl-private-archive-v1"

// Column order is a wire contract, independent of native CREATE/ALTER order.
// Only named exclusions may disappear; new native columns need profile review.
type privateTable struct {
	table                             store.NativeTable
	keys, columns, integers, excluded string
}

var privateTables = []privateTable{
	{store.NativeWorkspaces, "id",
		"id name domain enterprise_id raw_json updated_at", "", ""},
	{store.NativeChannels, "id",
		"id workspace_id name kind topic purpose is_private is_archived is_shared is_general raw_json updated_at",
		"is_private is_archived is_shared is_general", ""},
	{store.NativeUsers, "id",
		"id workspace_id name real_name display_name title is_bot is_deleted raw_json updated_at",
		"is_bot is_deleted", ""},
	{store.NativeMessages, "channel_id ts",
		"channel_id ts workspace_id user_id subtype client_msg_id thread_ts parent_user_id text normalized_text reply_count latest_reply edited_ts deleted_ts source_rank source_name raw_json updated_at",
		"reply_count source_rank", ""},
	{store.NativeMessageFiles, "channel_id ts file_id",
		"workspace_id channel_id ts file_id user_id name title mimetype filetype pretty_type mode size url_private url_private_download permalink is_public plain_text preview_plain_text content_sha256 content_size fetched_at raw_json updated_at deleted_at deletion_source deletion_reason",
		"size is_public content_size", "media_path fetch_status fetch_error"},
	{store.NativeMessageMentions, "channel_id ts mention_type target_id",
		"channel_id ts mention_type target_id display_text deleted_at deletion_source deletion_reason updated_at", "", ""},
}

// ProjectPrivateArchive projects already captured local data into an explicitly
// PRIVATE profile. The caller must qualify raw-payload provenance and approve
// the binding; this function grants no authority and applies no text redaction.
// All sync_state observations are excluded. An empty result still leaves the
// capture's native watermark available to its future delivery owner.
func ProjectPrivateArchive(capture store.NativeCapture) ([]remote.Change, error) {
	changes := make([]remote.Change, 0, len(capture.Rows)+len(capture.Removals))
	for _, row := range capture.Rows {
		if row.Table == store.NativeSyncState {
			continue
		}
		layout, err := privateLayout(row.Table)
		if err != nil {
			return nil, err
		}
		positions, err := privateColumnPositions(layout, row.Columns)
		if err != nil {
			return nil, err
		}
		change, err := projectPrivateRow(layout, positions, row.Values)
		if err != nil {
			return nil, err
		}
		keys := strings.Fields(layout.keys)
		if len(row.Key.Values) != len(keys) {
			return nil, errors.New("private projection key width differs")
		}
		for i, key := range keys {
			if row.Key.Values[i] != row.Values[positions[key]] {
				return nil, errors.New("private projection key differs from row")
			}
		}
		changes = append(changes, change)
	}
	for _, removal := range capture.Removals {
		if removal.Table == store.NativeSyncState {
			continue
		}
		layout, err := privateLayout(removal.Table)
		if err != nil {
			return nil, err
		}
		key, err := projectPrivateKey(removal.Key, len(strings.Fields(layout.keys)))
		if err != nil {
			return nil, err
		}
		changes = append(changes, remote.Change{Table: string(removal.Table), Operation: "remove", Key: key})
	}
	return changes, nil
}

func privateColumnPositions(layout privateTable, columns []string) (map[string]int, error) {
	if len(columns) != len(strings.Fields(layout.columns))+len(strings.Fields(layout.excluded)) {
		return nil, errors.New("private projection native column count differs")
	}
	positions := make(map[string]int, len(columns))
	for i, column := range columns {
		if _, duplicate := positions[column]; duplicate {
			return nil, errors.New("private projection has duplicate native columns")
		}
		positions[column] = i
	}
	for _, column := range strings.Fields(layout.columns + " " + layout.excluded) {
		if _, exists := positions[column]; !exists {
			return nil, errors.New("private projection required native column is missing or replaced")
		}
	}
	return positions, nil
}

func projectPrivateRow(layout privateTable, positions map[string]int, values []store.NativeValue) (remote.Change, error) {
	if len(values) != len(positions) {
		return remote.Change{}, errors.New("private projection native column count differs")
	}
	change := remote.Change{Table: string(layout.table), Operation: "upsert"}
	for _, column := range strings.Fields(layout.columns) {
		value, err := projectPrivateValue(values[positions[column]], strings.Contains(" "+layout.integers+" ", " "+column+" "))
		if err != nil {
			return remote.Change{}, err
		}
		change.Row = append(change.Row, value)
	}
	var key store.NativeKey
	for _, column := range strings.Fields(layout.keys) {
		key.Values = append(key.Values, values[positions[column]])
	}
	var err error
	change.Key, err = projectPrivateKey(key, len(key.Values))
	return change, err
}

func privateLayout(table store.NativeTable) (privateTable, error) {
	for _, layout := range privateTables {
		if layout.table == table {
			return layout, nil
		}
	}
	return privateTable{}, errors.New("private projection does not support the native table")
}

func projectPrivateKey(key store.NativeKey, width int) ([]remote.ChangeValue, error) {
	if len(key.Values) != width {
		return nil, errors.New("private projection key width differs")
	}
	values := make([]remote.ChangeValue, 0, width)
	for _, native := range key.Values {
		value, err := projectPrivateValue(native, false)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func projectPrivateValue(value store.NativeValue, integer bool) (remote.ChangeValue, error) {
	switch value.Kind {
	case store.NativeNull:
		return remote.ChangeValue{Kind: "null"}, nil
	case store.NativeInteger:
		if integer {
			text := strconv.FormatInt(value.Integer, 10)
			return remote.ChangeValue{Kind: "integer", Value: &text}, nil
		}
	case store.NativeText:
		if !integer && utf8.ValidString(value.Text) {
			text := value.Text
			return remote.ChangeValue{Kind: "text", Value: &text}, nil
		}
	}
	return remote.ChangeValue{}, errors.New("private projection native scalar type or UTF-8 differs")
}
