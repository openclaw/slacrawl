package acquisition

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/slackapi"
)

type Options struct {
	Workspace, Channel, SourceName, RunID string
	Since, Until                          *time.Time
	MaxPages                              int
}
type Result struct {
	Status   string `json:"status"`
	RunID    string `json:"runId"`
	Pages    int    `json:"pages"`
	Messages int    `json:"messages"`
	Sequence int64  `json:"sequence"`
	Complete bool   `json:"complete"`
}

// Collect writes only through the source API. An uncertain acknowledgement stops;
// the next invocation resumes the server's durable run and cursor.
func Collect(ctx context.Context, api *Client, provider *slackapi.Client, o Options) (Result, error) {
	var result Result
	if api == nil || provider == nil || o.Workspace == "" || o.Channel == "" || (o.SourceName != slackapi.SourceBot && o.SourceName != slackapi.SourceUser) || o.MaxPages < 1 || o.MaxPages > 1000 {
		return result, errors.New("explicit workspace, channel, source and max-pages 1..1000 are required")
	}
	if err := provider.AcquisitionIdentity(ctx, o.SourceName, o.Workspace); err != nil {
		return result, err
	}
	state, err := api.State(ctx, o.Channel, o.SourceName)
	if err != nil {
		return result, err
	}
	var run Run
	if state.ActiveRun != nil {
		run = *state.ActiveRun
		if run.WorkspaceID != o.Workspace || run.ChannelID != o.Channel || run.SourceName != o.SourceName || (o.RunID != "" && o.RunID != run.ID) || (o.Since != nil && !o.Since.Equal(run.Since)) || (o.Until != nil && !o.Until.Equal(run.Horizon)) {
			return result, errors.New("active acquisition run differs from requested scope or window")
		}
	} else {
		var since time.Time
		switch {
		case o.Since != nil:
			since = *o.Since
		case state.CompletedThrough != nil:
			since = *state.CompletedThrough
		default:
			return result, errors.New("first acquisition requires explicit --since")
		}
		until := time.Now().UTC().Truncate(time.Microsecond)
		if o.Until != nil {
			until = *o.Until
		}
		if !validTime(since) || !validTime(until) || !since.Before(until) || until.After(time.Now()) {
			return result, errors.New("acquisition window requires past UTC microsecond boundaries")
		}
		if state.CompletedThrough != nil && !state.CompletedThrough.Equal(since) {
			return result, errors.New("since differs from completed source horizon")
		}
		id := o.RunID
		if id == "" {
			var b [16]byte
			if _, err = rand.Read(b[:]); err != nil {
				return result, errors.New("cannot allocate acquisition run identity")
			}
			id = hex.EncodeToString(b[:])
		}
		run = Run{ID: id, ExpectedRevision: state.Revision, WorkspaceID: o.Workspace, SourceName: o.SourceName, ChannelID: o.Channel, Since: since, Horizon: until}
		if err = api.Begin(ctx, run); err != nil {
			return result, err
		}
		state, err = api.State(ctx, o.Channel, o.SourceName)
		if err != nil {
			return result, err
		}
		if state.ActiveRun == nil || *state.ActiveRun != run {
			return result, errors.New("server did not retain the requested acquisition run")
		}
	}
	result.RunID = run.ID
	for result.Pages < o.MaxPages {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		source, err := provider.AcquisitionHistory(ctx, run.SourceName, run.ChannelID, run.Since, run.Horizon, state.Cursor)
		if err != nil {
			return result, err
		}
		if source.IsLimited || (source.HasMore != (source.NextCursor != "")) || (source.HasMore && source.NextCursor == state.Cursor) {
			return result, errors.New("slack history page is limited or has an invalid continuation")
		}
		changes := []remote.Change{}
		seen := map[string]bool{}
		messages := 0
		for _, raw := range source.Messages {
			var key struct {
				TS string `json:"ts"`
			}
			if json.Unmarshal(raw, &key) != nil {
				return result, errors.New("invalid Slack history message")
			}
			timestamp, e := slackapi.AcquisitionTimestamp(key.TS)
			if e != nil {
				return result, e
			}
			if !timestamp.Before(run.Horizon) {
				continue
			} // Slack inclusive latest includes the next window boundary.
			if timestamp.Before(run.Since) {
				return result, errors.New("slack history returned a message before the requested window")
			}
			if seen[key.TS] {
				return result, errors.New("slack history repeated a message")
			}
			seen[key.TS] = true
			rows, e := slackapi.AcquisitionChanges(raw, run.WorkspaceID, run.ChannelID, run.SourceName, run.Horizon)
			if e != nil {
				return result, e
			}
			changes = append(changes, rows...)
			messages++
		}
		if len(changes) > 500 {
			return result, errors.New("slack page exceeds the atomic change limit")
		}
		if len(changes) > 0 {
			keys := make([]RowKey, 0, len(changes))
			for _, row := range changes {
				keys = append(keys, RowKey{row.Table, row.Key})
			}
			retained, e := api.Rows(ctx, run, state, keys)
			if e != nil {
				return result, e
			}
			if e = reconcile(changes, retained); e != nil {
				return result, e
			}
		}
		digest := sha256.Sum256([]byte(run.ID + "\x00" + state.Cursor))
		page := Page{RunID: run.ID, ID: hex.EncodeToString(digest[:16]), ExpectedRevision: state.Revision, Cursor: state.Cursor, NextCursor: source.NextCursor, Complete: !source.HasMore, HasMore: source.HasMore, IsLimited: source.IsLimited, Changes: changes}
		receipt, e := api.Commit(ctx, state, page)
		if e != nil {
			return result, e
		}
		result.Pages++
		result.Messages += messages
		result.Sequence = receipt.Sequence
		if page.Complete {
			result.Status = "complete"
			result.Complete = true
			return result, nil
		}
		state.Revision = receipt.Revision
		state.Sequence = receipt.Sequence
		state.Cursor = source.NextCursor
	}
	result.Status = "bounded"
	return result, nil
}

func validTime(t time.Time) bool {
	return !t.IsZero() && t.Location() == time.UTC && t.Nanosecond()%1000 == 0 && t.Unix() >= 0
}

func keyOf(c remote.Change) string { b, _ := json.Marshal(RowKey{c.Table, c.Key}); return string(b) }

func reconcile(changes, retained []remote.Change) error {
	byKey := map[string]remote.Change{}
	for _, c := range retained {
		byKey[keyOf(c)] = c
	}
	for i, c := range changes {
		old, ok := byKey[keyOf(c)]
		if !ok {
			continue
		}
		if c.Table == "messages" {
			if len(old.Row) != 18 {
				return errors.New("retained message layout differs")
			}
			// Provider edits require a separate refresh owner. Keep enriched fields and
			// exact old raw bytes for the same source revision instead of clobbering them.
			for _, column := range []int{2, 3, 4, 5, 6, 7, 8, 12, 13} {
				if value(old.Row[column]) != value(c.Row[column]) {
					return fmt.Errorf("retained message changed at column %d; refresh required", column)
				}
			}
		}
		changes[i] = old
	}
	return nil
}

func value(v remote.ChangeValue) string {
	if v.Value == nil {
		return ""
	}
	return *v.Value
}
