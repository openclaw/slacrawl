package slackapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"github.com/stretchr/testify/require"

	"github.com/vincentkoc/slacrawl/internal/config"
	"github.com/vincentkoc/slacrawl/internal/store"
)

func TestSyncHandlesRateLimitAndThreadCoverage(t *testing.T) {
	server := newMockSlackServer(t)
	defer server.Close()

	client := NewWithOptions(config.Tokens{
		Bot:  "xoxb-test",
		User: "xoxp-test",
	}, server.URL()+"/", server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }
	client.now = func() time.Time { return time.Date(2026, 3, 8, 1, 2, 3, 0, time.UTC) }

	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	err := client.Sync(context.Background(), st, SyncOptions{})
	require.NoError(t, err)
	require.GreaterOrEqual(t, server.calls("auth.test"), 2)

	status, err := st.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, status.Workspaces)
	require.Equal(t, 1, status.Channels)
	require.Equal(t, 1, status.Users)
	require.Equal(t, 2, status.Messages)
	require.Equal(t, "full", status.ThreadState)

	rows, err := st.Messages(context.Background(), "", "C123", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.Equal(t, "C123", row.ChannelID)
	}
	require.Equal(t, "message_replied", rows[0].Subtype)
}

func TestSyncIndexesRawHistoryBlockPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"team":"Test Team","team_id":"T123","user":"bot","user_id":"Ubot","bot_id":"B123"}`))
		case "/conversations.list":
			_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"C123","name":"general","is_channel":true}],"response_metadata":{"next_cursor":""}}`))
		case "/conversations.history":
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{
				"type":"message",
				"user":"U123",
				"text":"",
				"ts":"1710000000.000100",
				"blocks":[{
					"type":"section",
					"text":{"type":"mrkdwn","text":"section body"},
					"accessory":{
						"type":"icon_button",
						"text":{"type":"plain_text","text":"Delete response"},
						"accessibility_label":"Remove response",
						"value":"delete",
						"url":"https://hidden.example/delete",
						"confirm":{"title":{"type":"plain_text","text":"Hidden confirm"}}
					}
				}]
			}],"response_metadata":{"next_cursor":""}}`))
		case "/users.list":
			_, _ = w.Write([]byte(`{"ok":true,"members":[],"response_metadata":{"next_cursor":""}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewWithOptions(config.Tokens{Bot: "xoxb-test"}, server.URL+"/", server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }
	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	require.NoError(t, client.Sync(context.Background(), st, SyncOptions{}))

	rows, err := st.Search(context.Background(), "T123", "Remove", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Contains(t, rows[0].NormalizedText, "Delete response")
	require.Contains(t, rows[0].NormalizedText, "Remove response")
	require.NotContains(t, rows[0].NormalizedText, "delete")
	require.NotContains(t, rows[0].NormalizedText, "https://hidden.example/delete")
	require.NotContains(t, rows[0].NormalizedText, "Hidden confirm")
}

func TestSyncWithoutUserTokenMarksPartialCoverage(t *testing.T) {
	server := newMockSlackServer(t)
	defer server.Close()

	client := NewWithOptions(config.Tokens{
		Bot: "xoxb-test",
	}, server.URL()+"/", server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }

	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	err := client.Sync(context.Background(), st, SyncOptions{})
	require.NoError(t, err)

	value, err := st.GetSyncState(context.Background(), "doctor", "threads", "coverage")
	require.NoError(t, err)
	require.Equal(t, "partial", value)
}

func TestSyncIncludesDMsAndMPIMsWhenEnabled(t *testing.T) {
	server := newDMSyncSlackServer(t)
	defer server.Close()

	client := NewWithOptions(config.Tokens{
		Bot:  "xoxb-test",
		User: "xoxp-test",
	}, server.URL()+"/", server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }

	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	err := client.Sync(context.Background(), st, SyncOptions{})
	require.NoError(t, err)

	channels, err := st.Channels(context.Background(), "T123", "", 10)
	require.NoError(t, err)
	kindByID := make(map[string]string, len(channels))
	nameByID := make(map[string]string, len(channels))
	for _, row := range channels {
		kindByID[row.ID] = row.Kind
		nameByID[row.ID] = row.Name
	}
	require.Equal(t, "im", kindByID["D123"])
	require.Equal(t, "mpim", kindByID["G123"])
	require.Equal(t, "alice", nameByID["D123"])
	require.Equal(t, "alice,bob", nameByID["G123"])

	rows, err := st.QueryReadOnly(context.Background(), `
select source_name, source_rank
from messages
where channel_id = 'D123'
limit 1
`)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "api-user", rows[0]["source_name"])
	require.Equal(t, int64(1), rows[0]["source_rank"])
}

func TestSyncSkipsDMsWhenDisabled(t *testing.T) {
	server := newDMSyncSlackServer(t)
	defer server.Close()

	client := NewWithOptions(config.Tokens{
		Bot:  "xoxb-test",
		User: "xoxp-test",
	}, server.URL()+"/", server.Client()).WithIncludeDMs(false)
	client.sleep = func(context.Context, time.Duration) error { return nil }

	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	err := client.Sync(context.Background(), st, SyncOptions{})
	require.NoError(t, err)

	channels, err := st.Channels(context.Background(), "T123", "", 10)
	require.NoError(t, err)
	for _, channel := range channels {
		require.NotEqual(t, "im", channel.Kind)
		require.NotEqual(t, "mpim", channel.Kind)
	}
}

func TestSyncWithInvalidUserTokenStillMarksPartialCoverage(t *testing.T) {
	server := newInvalidUserSlackServer(t)
	defer server.Close()

	client := NewWithOptions(config.Tokens{
		Bot:  "xoxb-test",
		User: "xoxp-invalid",
	}, server.URL()+"/", server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }

	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	err := client.Sync(context.Background(), st, SyncOptions{})
	require.NoError(t, err)

	value, err := st.GetSyncState(context.Background(), "doctor", "threads", "coverage")
	require.NoError(t, err)
	require.Equal(t, "partial", value)

	rows, err := st.Messages(context.Background(), "", "C123", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "root message", rows[0].Text)
}

func TestSyncSkipsUnreadableThreadsAndContinues(t *testing.T) {
	server := newUnreadableThreadSlackServer(t)
	defer server.Close()

	client := NewWithOptions(config.Tokens{
		Bot:  "xoxb-test",
		User: "xoxp-test",
	}, server.URL()+"/", server.Client()).WithIncludeDMs(false)
	client.sleep = func(context.Context, time.Duration) error { return nil }

	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	err := client.Sync(context.Background(), st, SyncOptions{})
	require.NoError(t, err)

	rows, err := st.Messages(context.Background(), "", "", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 4)
	require.Equal(t, 2, server.calls("conversations.replies"))

	reason, err := st.GetSyncState(context.Background(), SourceUser, "thread_skip", "T123|C111|1710000000.000100")
	require.NoError(t, err)
	require.Equal(t, "missing_scope", reason)
	skips, err := st.ListSyncState(context.Background(), SourceUser, "thread_skip", 10)
	require.NoError(t, err)
	require.Len(t, skips, 2)

	value, err := st.GetSyncState(context.Background(), "doctor", "threads", "coverage")
	require.NoError(t, err)
	require.Equal(t, "partial", value)

	server.Close()
	readable := newReadableThreadSlackServer(t)
	defer readable.Close()
	client = NewWithOptions(config.Tokens{
		Bot:  "xoxb-test",
		User: "xoxp-test",
	}, readable.URL()+"/", readable.Client()).WithIncludeDMs(false)
	client.sleep = func(context.Context, time.Duration) error { return nil }

	require.NoError(t, client.Sync(context.Background(), st, SyncOptions{Channels: []string{"C222"}}))
	value, err = st.GetSyncState(context.Background(), "doctor", "threads", "coverage")
	require.NoError(t, err)
	require.Equal(t, "partial", value)

	require.NoError(t, client.Sync(context.Background(), st, SyncOptions{Full: true}))
	value, err = st.GetSyncState(context.Background(), "doctor", "threads", "coverage")
	require.NoError(t, err)
	require.Equal(t, "full", value)

	skips, err = st.ListSyncState(context.Background(), SourceUser, "thread_skip", 10)
	require.NoError(t, err)
	require.Empty(t, skips)
}

func TestDoctorWithInvalidUserTokenDoesNotReportFullCoverage(t *testing.T) {
	server := newInvalidUserSlackServer(t)
	defer server.Close()

	client := NewWithOptions(config.Tokens{
		Bot:  "xoxb-test",
		App:  "xapp-test",
		User: "xoxp-invalid",
	}, server.URL()+"/", server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }

	diag, err := client.Doctor(context.Background())
	require.NoError(t, err)
	require.True(t, diag.BotConfigured)
	require.True(t, diag.UserConfigured)
	require.False(t, diag.UserAuthAvailable)
	require.Equal(t, "partial", diag.ThreadCoverage)
	require.Equal(t, "invalid_auth", diag.UserAuthError)
	require.False(t, diag.DMsIncluded)
}

func TestDoctorWithValidUserTokenReportsDMInclusion(t *testing.T) {
	server := newMockSlackServer(t)
	defer server.Close()

	client := NewWithOptions(config.Tokens{
		Bot:  "xoxb-test",
		App:  "xapp-test",
		User: "xoxp-test",
	}, server.URL()+"/", server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }

	diag, err := client.Doctor(context.Background())
	require.NoError(t, err)
	require.True(t, diag.UserAuthAvailable)
	require.True(t, diag.DMsIncluded)
	require.Equal(t, "", diag.DMsMissingScope)
}

func TestDoctorCanDisableDMInclusion(t *testing.T) {
	server := newMockSlackServer(t)
	defer server.Close()

	client := NewWithOptions(config.Tokens{
		Bot:  "xoxb-test",
		User: "xoxp-test",
	}, server.URL()+"/", server.Client()).WithIncludeDMs(false)
	client.sleep = func(context.Context, time.Duration) error { return nil }

	diag, err := client.Doctor(context.Background())
	require.NoError(t, err)
	require.True(t, diag.UserAuthAvailable)
	require.False(t, diag.DMsIncluded)
}

func TestSyncSkipsChannelsTheBotCannotRead(t *testing.T) {
	server := newSkipChannelSlackServer(t)
	defer server.Close()

	client := NewWithOptions(config.Tokens{
		Bot: "xoxb-test",
	}, server.URL()+"/", server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }

	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	err := client.Sync(context.Background(), st, SyncOptions{})
	require.NoError(t, err)

	rows, err := st.Messages(context.Background(), "", "C222", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "accessible message", rows[0].Text)

	reason, err := st.GetSyncState(context.Background(), SourceBot, "channel_skip", "C111")
	require.NoError(t, err)
	require.Equal(t, "not_in_channel", reason)
}

func TestSyncUsesConfiguredConcurrencyForChannelHistory(t *testing.T) {
	server := newConcurrentHistorySlackServer(t)
	defer server.Close()

	client := NewWithOptions(config.Tokens{
		Bot: "xoxb-test",
	}, server.URL()+"/", server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }

	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	err := client.Sync(context.Background(), st, SyncOptions{Concurrency: 2})
	require.NoError(t, err)
	require.GreaterOrEqual(t, server.maxConcurrentHistory(), 2)

	rows, err := st.Messages(context.Background(), "", "", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
}

func TestSyncJoinsPublicChannelBeforeRetryingHistory(t *testing.T) {
	server := newJoinRetrySlackServer(t)
	defer server.Close()

	client := NewWithOptions(config.Tokens{
		Bot: "xoxb-test",
	}, server.URL()+"/", server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }

	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	err := client.Sync(context.Background(), st, SyncOptions{})
	require.NoError(t, err)

	rows, err := st.Messages(context.Background(), "", "C111", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "joined message", rows[0].Text)

	joinState, err := st.GetSyncState(context.Background(), SourceBot, "channel_join", "C111")
	require.NoError(t, err)
	require.Equal(t, "joined", joinState)
}

func TestSyncDefaultsToIncrementalHistoryWhenNotFull(t *testing.T) {
	server := newRepairSlackServer(t)
	defer server.Close()

	client := NewWithOptions(config.Tokens{
		Bot: "xoxb-test",
	}, server.URL()+"/", server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }
	client.now = func() time.Time { return time.Date(2026, 3, 8, 4, 0, 0, 0, time.UTC) }

	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{
		ID:        "T123",
		Name:      "team",
		RawJSON:   "{}",
		UpdatedAt: client.now(),
	}))
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{
		ID:          "C123",
		WorkspaceID: "T123",
		Name:        "general",
		Kind:        "public_channel",
		RawJSON:     "{}",
		UpdatedAt:   client.now(),
	}))
	require.NoError(t, st.UpsertMessage(ctx, store.Message{
		ChannelID:      "C123",
		TS:             "1710000000.000100",
		WorkspaceID:    "T123",
		UserID:         "U123",
		Text:           "existing root",
		NormalizedText: "existing root",
		SourceRank:     2,
		SourceName:     SourceBot,
		RawJSON:        "{}",
		UpdatedAt:      client.now(),
	}, nil))

	err := client.Sync(ctx, st, SyncOptions{WorkspaceID: "T123", Channels: []string{"C123"}})
	require.NoError(t, err)
	require.Equal(t, "1709996400.000100", server.lastHistoryOldest("C123"))
}

func TestHandleEventsAPIEventUpdatesStore(t *testing.T) {
	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	now := time.Date(2026, 3, 8, 3, 0, 0, 0, time.UTC)
	require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{
		ID:        "T123",
		Name:      "team",
		RawJSON:   "{}",
		UpdatedAt: now,
	}))
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{
		ID:          "C123",
		WorkspaceID: "T123",
		Name:        "general",
		Kind:        "public_channel",
		RawJSON:     "{}",
		UpdatedAt:   now,
	}))

	client := New(config.Tokens{Bot: "xoxb-test", App: "xapp-test"})
	client.now = func() time.Time { return now }

	rawMessage := []byte(`{
	  "token":"ignored",
	  "team_id":"T123",
	  "api_app_id":"A123",
	  "type":"event_callback",
	  "event":{
	    "type":"message",
	    "channel":"C123",
	    "user":"U123",
	    "text":"hello <@U999|alex>",
	    "ts":"1710000000.000100",
	    "event_ts":"1710000000.000100"
	  }
	}`)
	event, err := slackevents.ParseEvent(rawMessage, slackevents.OptionNoVerifyToken())
	require.NoError(t, err)
	require.NoError(t, client.HandleEventsAPIEvent(ctx, st, "T123", event))

	rawBlockMessage := []byte(`{
	  "token":"ignored",
	  "team_id":"T123",
	  "api_app_id":"A123",
	  "type":"event_callback",
	  "event":{
	    "type":"message",
	    "channel":"C123",
	    "user":"U123",
	    "text":"",
	    "ts":"1710000000.000200",
	    "event_ts":"1710000000.000200",
	    "blocks":[{
	      "type":"section",
	      "text":{"type":"mrkdwn","text":"section with accessory"},
	      "accessory":{
	        "type":"icon_button",
	        "text":{"type":"plain_text","text":"Delete response"},
	        "accessibility_label":"Remove response",
	        "value":"delete",
	        "url":"https://hidden.example/delete",
	        "confirm":{
	          "title":{"type":"plain_text","text":"Hidden confirm title"},
	          "text":{"type":"mrkdwn","text":"Hidden confirm body"},
	          "confirm":{"type":"plain_text","text":"Hidden yes"},
	          "deny":{"type":"plain_text","text":"Hidden no"}
	        }
	      }
	    }]
	  }
	}`)
	blockEvent, err := slackevents.ParseEvent(rawBlockMessage, slackevents.OptionNoVerifyToken())
	require.NoError(t, err)
	require.NoError(t, client.HandleEventsAPIEvent(ctx, st, "T123", blockEvent))

	rawRename := []byte(`{
	  "token":"ignored",
	  "team_id":"T123",
	  "api_app_id":"A123",
	  "type":"event_callback",
	  "event":{
	    "type":"channel_rename",
	    "channel":{"id":"C123","name":"renamed","created":1},
	    "event_ts":"1710000001.000100"
	  }
	}`)
	renameEvent, err := slackevents.ParseEvent(rawRename, slackevents.OptionNoVerifyToken())
	require.NoError(t, err)
	require.NoError(t, client.HandleEventsAPIEvent(ctx, st, "T123", renameEvent))

	rows, err := st.Messages(ctx, "", "C123", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.True(t, strings.Contains(rows[1].NormalizedText, "@alex"))

	searchRows, err := st.Search(ctx, "T123", "Remove", 10)
	require.NoError(t, err)
	require.Len(t, searchRows, 1)
	require.Equal(t, "1710000000.000200", searchRows[0].TS)
	require.Contains(t, searchRows[0].NormalizedText, "Delete response")
	require.Contains(t, searchRows[0].NormalizedText, "Remove response")
	require.NotContains(t, searchRows[0].NormalizedText, "delete")
	require.NotContains(t, searchRows[0].NormalizedText, "https://hidden.example/delete")
	require.NotContains(t, searchRows[0].NormalizedText, "Hidden confirm")

	rawBlockEdit := []byte(`{
	  "token":"ignored",
	  "team_id":"T123",
	  "api_app_id":"A123",
	  "type":"event_callback",
	  "event":{
	    "type":"message",
	    "subtype":"message_changed",
	    "channel":"C123",
	    "event_ts":"1710000000.000300",
	    "message":{
	      "type":"message",
	      "channel":"C123",
	      "user":"U123",
	      "text":"",
	      "ts":"1710000000.000200",
	      "edited":{"user":"U123","ts":"1710000000.000300"},
	      "blocks":[{
	        "type":"section",
	        "text":{"type":"mrkdwn","text":"edited section"},
	        "accessory":{
	          "type":"icon_button",
	          "text":{"type":"plain_text","text":"Archive response"},
	          "accessibility_label":"Hide response",
	          "value":"archive"
	        }
	      }]
	    },
	    "previous_message":{
	      "type":"message",
	      "channel":"C123",
	      "user":"U123",
	      "text":"",
	      "ts":"1710000000.000200"
	    }
	  }
	}`)
	editEvent, err := slackevents.ParseEvent(rawBlockEdit, slackevents.OptionNoVerifyToken())
	require.NoError(t, err)
	require.NoError(t, client.HandleEventsAPIEvent(ctx, st, "T123", editEvent))

	editRows, err := st.Search(ctx, "T123", "Hide", 10)
	require.NoError(t, err)
	require.Len(t, editRows, 1)
	require.Contains(t, editRows[0].NormalizedText, "Archive response")
	require.Contains(t, editRows[0].NormalizedText, "Hide response")
	require.NotContains(t, editRows[0].NormalizedText, "archive")

	channels, err := st.Channels(ctx, "", "renamed", 10)
	require.NoError(t, err)
	require.Len(t, channels, 1)
}

func TestHandleSocketModeEventAcksAndPersists(t *testing.T) {
	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	now := time.Date(2026, 3, 8, 3, 30, 0, 0, time.UTC)
	require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{
		ID:        "T123",
		Name:      "team",
		RawJSON:   "{}",
		UpdatedAt: now,
	}))

	client := New(config.Tokens{Bot: "xoxb-test", App: "xapp-test"})
	client.now = func() time.Time { return now }

	rawMessage := []byte(`{
	  "token":"ignored",
	  "team_id":"T123",
	  "api_app_id":"A123",
	  "type":"event_callback",
	  "event":{
	    "type":"message",
	    "channel":"C123",
	    "user":"U123",
	    "text":"tail message",
	    "ts":"1710000005.000100",
	    "event_ts":"1710000005.000100"
	  }
	}`)
	event, err := slackevents.ParseEvent(rawMessage, slackevents.OptionNoVerifyToken())
	require.NoError(t, err)

	socket := &fakeSocketMode{events: make(chan socketmode.Event)}
	err = client.handleSocketModeEvent(ctx, st, "T123", socket, socketmode.Event{
		Type:    socketmode.EventTypeEventsAPI,
		Data:    event,
		Request: &socketmode.Request{EnvelopeID: "1", Type: "events_api"},
	})
	require.NoError(t, err)
	require.Equal(t, 1, socket.acks)

	rows, err := st.Messages(ctx, "", "C123", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "tail message", rows[0].Text)
}

func TestHandleSocketModeEventDoesNotAckOnStoreError(t *testing.T) {
	st := mustStore(t)
	require.NoError(t, st.Close())

	ctx := context.Background()
	client := New(config.Tokens{Bot: "xoxb-test", App: "xapp-test"})

	rawMessage := []byte(`{
	  "token":"ignored",
	  "team_id":"T123",
	  "api_app_id":"A123",
	  "type":"event_callback",
	  "event":{
	    "type":"message",
	    "channel":"C123",
	    "user":"U123",
	    "text":"tail message",
	    "ts":"1710000005.000100",
	    "event_ts":"1710000005.000100"
	  }
	}`)
	event, err := slackevents.ParseEvent(rawMessage, slackevents.OptionNoVerifyToken())
	require.NoError(t, err)

	socket := &fakeSocketMode{events: make(chan socketmode.Event)}
	err = client.handleSocketModeEvent(ctx, st, "T123", socket, socketmode.Event{
		Type:    socketmode.EventTypeEventsAPI,
		Data:    event,
		Request: &socketmode.Request{EnvelopeID: "1", Type: "events_api"},
	})
	require.Error(t, err)
	require.Equal(t, 0, socket.acks)
}

func TestRepairWorkspaceReconcilesIncrementalHistory(t *testing.T) {
	server := newRepairSlackServer(t)
	defer server.Close()

	client := NewWithOptions(config.Tokens{
		Bot:  "xoxb-test",
		User: "xoxp-test",
	}, server.URL()+"/", server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }
	client.now = func() time.Time { return time.Date(2026, 3, 8, 4, 0, 0, 0, time.UTC) }

	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{
		ID:        "T123",
		Name:      "team",
		RawJSON:   "{}",
		UpdatedAt: client.now(),
	}))
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{
		ID:          "C123",
		WorkspaceID: "T123",
		Name:        "general",
		Kind:        "public_channel",
		RawJSON:     "{}",
		UpdatedAt:   client.now(),
	}))
	require.NoError(t, st.UpsertMessage(ctx, store.Message{
		ChannelID:      "C123",
		TS:             "1710000000.000100",
		WorkspaceID:    "T123",
		UserID:         "U123",
		Text:           "existing root",
		NormalizedText: "existing root",
		ReplyCount:     1,
		LatestReply:    "1710000001.000200",
		SourceRank:     2,
		SourceName:     SourceBot,
		RawJSON:        "{}",
		UpdatedAt:      client.now(),
	}, nil))

	require.NoError(t, client.repairWorkspace(ctx, st, "T123"))

	rows, err := st.Messages(ctx, "", "C123", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "new repair message", rows[0].Text)
	require.Equal(t, "existing root", rows[1].Text)

	require.Equal(t, "1709996400.000100", server.lastHistoryOldest("C123"))
}

func TestRepairWorkspaceDowngradesThreadCoverageOnUnreadableThreads(t *testing.T) {
	server := newUnreadableThreadSlackServer(t)
	defer server.Close()

	client := NewWithOptions(config.Tokens{
		Bot:  "xoxb-test",
		User: "xoxp-test",
	}, server.URL()+"/", server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }

	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	ctx := context.Background()
	require.NoError(t, st.SetSyncState(ctx, "doctor", "threads", "coverage", "full"))
	require.NoError(t, client.repairWorkspace(ctx, st, "T123"))

	value, err := st.GetSyncState(ctx, "doctor", "threads", "coverage")
	require.NoError(t, err)
	require.Equal(t, "partial", value)

	reason, err := st.GetSyncState(ctx, SourceUser, "thread_skip", "T123|C111|1710000000.000100")
	require.NoError(t, err)
	require.Equal(t, "missing_scope", reason)
}

type mockSlackServer struct {
	server        *httptest.Server
	mu            sync.Mutex
	counts        map[string]int
	lastOld       map[string]string
	activeHistory int
	maxHistory    int
}

type fakeSocketMode struct {
	events chan socketmode.Event
	acks   int
}

func (f *fakeSocketMode) Run() error { return nil }
func (f *fakeSocketMode) Ack(req socketmode.Request, payload ...interface{}) {
	f.acks++
}
func (f *fakeSocketMode) Events() <-chan socketmode.Event { return f.events }

func newMockSlackServer(t *testing.T) *mockSlackServer {
	t.Helper()
	mock := &mockSlackServer{counts: map[string]int{}, lastOld: map[string]string{}}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mock.mu.Lock()
		mock.counts[r.URL.Path]++
		count := mock.counts[r.URL.Path]
		mock.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			if count == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"ok":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"team":"Test Team","team_id":"T123","user":"bot","user_id":"Ubot","bot_id":"B123"}`))
		case "/conversations.list":
			_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"C123","name":"general","is_channel":true,"is_private":false,"is_archived":false,"is_shared":false,"is_general":true,"topic":{"value":"topic"},"purpose":{"value":"purpose"}}],"response_metadata":{"next_cursor":""}}`))
		case "/conversations.history":
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","user":"U123","text":"root message","ts":"1710000000.000100","reply_count":1,"latest_reply":"1710000001.000200"}],"response_metadata":{"next_cursor":""}}`))
		case "/conversations.replies":
			_, _ = w.Write([]byte(`{"ok":true,"has_more":false,"messages":[{"type":"message","subtype":"message_replied","user":"U234","text":"reply message","thread_ts":"1710000000.000100","ts":"1710000001.000200"}],"response_metadata":{"next_cursor":""}}`))
		case "/users.list":
			_, _ = w.Write([]byte(`{"ok":true,"members":[{"id":"U123","name":"alice","real_name":"Alice Example","profile":{"display_name":"alice","title":"Engineer"}}],"response_metadata":{"next_cursor":""}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	return mock
}

func newDMSyncSlackServer(t *testing.T) *mockSlackServer {
	t.Helper()
	mock := &mockSlackServer{counts: map[string]int{}, lastOld: map[string]string{}}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"team":"Test Team","team_id":"T123","user":"bot","user_id":"Ubot","bot_id":"B123"}`))
		case "/conversations.list":
			values := mustFormValues(r)
			switch values.Get("types") {
			case "im,mpim":
				_, _ = w.Write([]byte(`{"ok":true,"channels":[
					{"id":"D123","is_im":true,"is_private":true,"user":"U123"},
					{"id":"G123","is_mpim":true,"is_private":true,"members":["U123","U456"]}
				],"response_metadata":{"next_cursor":""}}`))
			default:
				_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"C123","name":"general","is_channel":true,"is_private":false,"is_archived":false,"is_shared":false,"is_general":true,"topic":{"value":"topic"},"purpose":{"value":"purpose"}}],"response_metadata":{"next_cursor":""}}`))
			}
		case "/conversations.history":
			values := mustFormValues(r)
			switch values.Get("channel") {
			case "C123":
				_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","user":"U123","text":"root message","ts":"1710000000.000100"}],"response_metadata":{"next_cursor":""}}`))
			case "D123":
				_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","user":"U123","text":"dm message","ts":"1710000002.000100"}],"response_metadata":{"next_cursor":""}}`))
			case "G123":
				_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","user":"U456","text":"mpim message","ts":"1710000003.000100"}],"response_metadata":{"next_cursor":""}}`))
			default:
				http.NotFound(w, r)
			}
		case "/users.list":
			_, _ = w.Write([]byte(`{"ok":true,"members":[
				{"id":"U123","name":"alice-user","real_name":"Alice Example","profile":{"display_name":"alice","title":"Engineer"}},
				{"id":"U456","name":"bob-user","real_name":"Bob Example","profile":{"display_name":"bob","title":"Engineer"}}
			],"response_metadata":{"next_cursor":""}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	return mock
}

func newRepairSlackServer(t *testing.T) *mockSlackServer {
	t.Helper()
	mock := &mockSlackServer{counts: map[string]int{}, lastOld: map[string]string{}}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mock.mu.Lock()
		mock.counts[r.URL.Path]++
		mock.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.list":
			_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"C123","name":"general","is_channel":true,"is_private":false,"is_archived":false,"is_shared":false,"is_general":true,"topic":{"value":"topic"},"purpose":{"value":"purpose"}}],"response_metadata":{"next_cursor":""}}`))
		case "/conversations.history":
			values := mustFormValues(r)
			mock.mu.Lock()
			mock.lastOld[values.Get("channel")] = values.Get("oldest")
			mock.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","user":"U234","text":"new repair message","ts":"1710001200.000200"}],"response_metadata":{"next_cursor":""}}`))
		case "/conversations.replies":
			_, _ = w.Write([]byte(`{"ok":true,"has_more":false,"messages":[{"type":"message","subtype":"message_replied","user":"U234","text":"thread repair","thread_ts":"1710000000.000100","ts":"1710000001.000200"}],"response_metadata":{"next_cursor":""}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	return mock
}

func newSkipChannelSlackServer(t *testing.T) *mockSlackServer {
	t.Helper()
	mock := &mockSlackServer{counts: map[string]int{}, lastOld: map[string]string{}}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"team":"Test Team","team_id":"T123","user":"bot","user_id":"Ubot","bot_id":"B123"}`))
		case "/conversations.list":
			_, _ = w.Write([]byte(`{"ok":true,"channels":[
				{"id":"C111","name":"private-ish","is_channel":true,"is_private":true,"is_archived":false,"is_shared":false,"is_general":false,"topic":{"value":""},"purpose":{"value":""}},
				{"id":"C222","name":"general","is_channel":true,"is_private":false,"is_archived":false,"is_shared":false,"is_general":true,"topic":{"value":"topic"},"purpose":{"value":"purpose"}}
			],"response_metadata":{"next_cursor":""}}`))
		case "/conversations.history":
			values := mustFormValues(r)
			switch values.Get("channel") {
			case "C111":
				_, _ = w.Write([]byte(`{"ok":false,"error":"not_in_channel"}`))
			case "C222":
				_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","channel":"C222","user":"U123","text":"accessible message","ts":"1710000000.000100"}],"response_metadata":{"next_cursor":""}}`))
			default:
				http.NotFound(w, r)
			}
		case "/users.list":
			_, _ = w.Write([]byte(`{"ok":true,"members":[],"response_metadata":{"next_cursor":""}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	return mock
}

func newInvalidUserSlackServer(t *testing.T) *mockSlackServer {
	t.Helper()
	mock := &mockSlackServer{counts: map[string]int{}, lastOld: map[string]string{}}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		values := mustFormValues(r)
		token := values.Get("token")
		auth := r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/auth.test":
			if strings.Contains(auth, "xoxp-invalid") || token == "xoxp-invalid" { //nolint:gosec // Test token sentinel, not a real credential.
				_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"team":"Test Team","team_id":"T123","user":"bot","user_id":"Ubot","bot_id":"B123"}`))
		case "/conversations.list":
			_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"C123","name":"general","is_channel":true,"is_private":false,"is_archived":false,"is_shared":false,"is_general":true,"topic":{"value":"topic"},"purpose":{"value":"purpose"}}],"response_metadata":{"next_cursor":""}}`))
		case "/conversations.history":
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","user":"U123","text":"root message","ts":"1710000000.000100","reply_count":1,"latest_reply":"1710000001.000200"}],"response_metadata":{"next_cursor":""}}`))
		case "/users.list":
			_, _ = w.Write([]byte(`{"ok":true,"members":[],"response_metadata":{"next_cursor":""}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	return mock
}

func newUnreadableThreadSlackServer(t *testing.T) *mockSlackServer {
	t.Helper()
	mock := &mockSlackServer{counts: map[string]int{}, lastOld: map[string]string{}}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mock.mu.Lock()
		mock.counts[r.URL.Path]++
		mock.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"team":"Test Team","team_id":"T123","user":"bot","user_id":"Ubot","bot_id":"B123"}`))
		case "/conversations.list":
			_, _ = w.Write([]byte(`{"ok":true,"channels":[
				{"id":"C111","name":"one","is_channel":true,"is_private":true,"is_archived":false,"is_shared":false,"is_general":false,"topic":{"value":""},"purpose":{"value":""}},
				{"id":"C222","name":"two","is_channel":true,"is_private":false,"is_archived":false,"is_shared":false,"is_general":false,"topic":{"value":""},"purpose":{"value":""}}
			],"response_metadata":{"next_cursor":""}}`))
		case "/conversations.history":
			values := mustFormValues(r)
			switch values.Get("channel") {
			case "C111":
				_, _ = w.Write([]byte(`{"ok":true,"messages":[
					{"type":"message","channel":"C111","user":"U123","text":"root message","ts":"1710000000.000100","reply_count":1,"latest_reply":"1710000001.000200"},
					{"type":"message","channel":"C111","user":"U123","text":"second root","ts":"1710000002.000100","reply_count":1,"latest_reply":"1710000003.000200"}
				],"response_metadata":{"next_cursor":""}}`))
			case "C222":
				_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","channel":"C222","user":"U123","text":"next channel","ts":"1710000004.000100","reply_count":1,"latest_reply":"1710000005.000200"}],"response_metadata":{"next_cursor":""}}`))
			default:
				http.NotFound(w, r)
			}
		case "/conversations.replies":
			values := mustFormValues(r)
			if values.Get("channel") == "C111" {
				_, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"has_more":false,"messages":[{"type":"message","subtype":"message_replied","channel":"C222","user":"U234","text":"public reply","thread_ts":"1710000004.000100","ts":"1710000005.000200"}],"response_metadata":{"next_cursor":""}}`))
		case "/users.list":
			_, _ = w.Write([]byte(`{"ok":true,"members":[],"response_metadata":{"next_cursor":""}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	return mock
}

func newReadableThreadSlackServer(t *testing.T) *mockSlackServer {
	t.Helper()
	mock := &mockSlackServer{counts: map[string]int{}, lastOld: map[string]string{}}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"team":"Test Team","team_id":"T123","user":"bot","user_id":"Ubot","bot_id":"B123"}`))
		case "/conversations.list":
			_, _ = w.Write([]byte(`{"ok":true,"channels":[
				{"id":"C111","name":"one","is_channel":true,"is_private":false,"is_archived":false,"is_shared":false,"is_general":false,"topic":{"value":""},"purpose":{"value":""}},
				{"id":"C222","name":"two","is_channel":true,"is_private":false,"is_archived":false,"is_shared":false,"is_general":false,"topic":{"value":""},"purpose":{"value":""}}
			],"response_metadata":{"next_cursor":""}}`))
		case "/conversations.history":
			values := mustFormValues(r)
			switch values.Get("channel") {
			case "C111":
				_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","channel":"C111","user":"U123","text":"root message","ts":"1710000000.000100","reply_count":1,"latest_reply":"1710000001.000200"}],"response_metadata":{"next_cursor":""}}`))
			case "C222":
				_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","channel":"C222","user":"U123","text":"next channel","ts":"1710000004.000100"}],"response_metadata":{"next_cursor":""}}`))
			default:
				http.NotFound(w, r)
			}
		case "/conversations.replies":
			_, _ = w.Write([]byte(`{"ok":true,"has_more":false,"messages":[{"type":"message","subtype":"message_replied","channel":"C111","user":"U234","text":"reply message","thread_ts":"1710000000.000100","ts":"1710000001.000200"}],"response_metadata":{"next_cursor":""}}`))
		case "/users.list":
			_, _ = w.Write([]byte(`{"ok":true,"members":[],"response_metadata":{"next_cursor":""}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	return mock
}

func newConcurrentHistorySlackServer(t *testing.T) *mockSlackServer {
	t.Helper()
	mock := &mockSlackServer{counts: map[string]int{}, lastOld: map[string]string{}}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"team":"Test Team","team_id":"T123","user":"bot","user_id":"Ubot","bot_id":"B123"}`))
		case "/conversations.list":
			_, _ = w.Write([]byte(`{"ok":true,"channels":[
				{"id":"C111","name":"one","is_channel":true,"is_private":false,"is_archived":false,"is_shared":false,"is_general":false,"topic":{"value":""},"purpose":{"value":""}},
				{"id":"C222","name":"two","is_channel":true,"is_private":false,"is_archived":false,"is_shared":false,"is_general":true,"topic":{"value":""},"purpose":{"value":""}}
			],"response_metadata":{"next_cursor":""}}`))
		case "/conversations.history":
			mock.mu.Lock()
			mock.activeHistory++
			if mock.activeHistory > mock.maxHistory {
				mock.maxHistory = mock.activeHistory
			}
			mock.mu.Unlock()
			time.Sleep(150 * time.Millisecond)
			values := mustFormValues(r)
			channel := values.Get("channel")
			mock.mu.Lock()
			mock.activeHistory--
			mock.mu.Unlock()
			//nolint:gosec // Test server echoes controlled query values.
			_, _ = fmt.Fprintf(w, `{"ok":true,"messages":[{"type":"message","channel":"%s","user":"U123","text":"msg-%s","ts":"1710000000.000100"}],"response_metadata":{"next_cursor":""}}`, channel, channel)
		case "/users.list":
			_, _ = w.Write([]byte(`{"ok":true,"members":[],"response_metadata":{"next_cursor":""}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	return mock
}

func newJoinRetrySlackServer(t *testing.T) *mockSlackServer {
	t.Helper()
	mock := &mockSlackServer{counts: map[string]int{}, lastOld: map[string]string{}}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"team":"Test Team","team_id":"T123","user":"bot","user_id":"Ubot","bot_id":"B123"}`))
		case "/conversations.list":
			_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"C111","name":"public","is_channel":true,"is_private":false,"is_archived":false,"is_shared":false,"is_general":false,"topic":{"value":""},"purpose":{"value":""}}],"response_metadata":{"next_cursor":""}}`))
		case "/conversations.history":
			mock.mu.Lock()
			mock.counts[r.URL.Path]++
			count := mock.counts[r.URL.Path]
			mock.mu.Unlock()
			if count == 1 {
				_, _ = w.Write([]byte(`{"ok":false,"error":"not_in_channel"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","channel":"C111","user":"U123","text":"joined message","ts":"1710000000.000100"}],"response_metadata":{"next_cursor":""}}`))
		case "/conversations.join":
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"C111","name":"public"}}`))
		case "/users.list":
			_, _ = w.Write([]byte(`{"ok":true,"members":[],"response_metadata":{"next_cursor":""}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	return mock
}

func (m *mockSlackServer) Close() {
	m.server.Close()
}

func (m *mockSlackServer) lastHistoryOldest(channelID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastOld[channelID]
}

func (m *mockSlackServer) URL() string {
	return m.server.URL
}

func (m *mockSlackServer) Client() *http.Client {
	return m.server.Client()
}

func (m *mockSlackServer) calls(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts["/"+path]
}

func (m *mockSlackServer) maxConcurrentHistory() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maxHistory
}

func mustStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	return st
}

func mustFormValues(r *http.Request) url.Values {
	_ = r.ParseForm()
	if r.Form != nil {
		return r.Form
	}
	panic(fmt.Sprintf("missing form for %s", r.URL.Path))
}

func TestMessageFromEventPreservesDeleteAndThreadFields(t *testing.T) {
	raw := []byte(`{
	  "token":"ignored",
	  "team_id":"T123",
	  "api_app_id":"A123",
	  "type":"event_callback",
	  "event":{
	    "type":"message",
	    "subtype":"message_deleted",
	    "channel":"C123",
	    "deleted_ts":"1710000000.000100",
	    "previous_message":{"text":"gone","ts":"1710000000.000100","thread_ts":"1710000000.000100"},
	    "event_ts":"1710000002.000200"
	  }
	}`)
	event, err := slackevents.ParseEvent(raw, slackevents.OptionNoVerifyToken())
	require.NoError(t, err)
	ev, ok := event.InnerEvent.Data.(*slackevents.MessageEvent)
	require.True(t, ok)
	msg := messageFromEvent(ev)
	require.Equal(t, "1710000000.000100", msg.Timestamp)
	require.Equal(t, "1710000000.000100", msg.DeletedTimestamp)
	require.Equal(t, "1710000000.000100", msg.ThreadTimestamp)

	raw = []byte(`{
	  "token":"ignored",
	  "team_id":"T123",
	  "api_app_id":"A123",
	  "type":"event_callback",
	  "event":{
	    "type":"message",
	    "subtype":"message_deleted",
	    "channel":"C123",
	    "ts":"1710000002.000200",
	    "deleted_ts":"1710000000.000100",
	    "event_ts":"1710000002.000200"
	  }
	}`)
	event, err = slackevents.ParseEvent(raw, slackevents.OptionNoVerifyToken())
	require.NoError(t, err)
	ev, ok = event.InnerEvent.Data.(*slackevents.MessageEvent)
	require.True(t, ok)
	msg = messageFromEvent(ev)
	require.Equal(t, "1710000000.000100", msg.Timestamp)
	require.Equal(t, "1710000000.000100", msg.DeletedTimestamp)
}

func TestHandleEventsAPIEventMarksOriginalMessageDeleted(t *testing.T) {
	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()
	client := New(config.Tokens{Bot: "xoxb-test"})
	client.now = func() time.Time { return time.Date(2026, 3, 8, 1, 2, 3, 0, time.UTC) }
	ctx := context.Background()
	require.NoError(t, st.UpsertMessage(ctx, store.Message{
		ChannelID:      "C123",
		TS:             "1710000000.000100",
		WorkspaceID:    "T123",
		UserID:         "U123",
		ClientMsgID:    "client-1",
		Text:           "gone",
		NormalizedText: "gone",
		ReplyCount:     2,
		LatestReply:    "1710000001.000200",
		SourceRank:     2,
		SourceName:     SourceBot,
		RawJSON:        "{}",
		UpdatedAt:      client.now(),
		Files: []store.MessageFile{{
			FileID:        "F123",
			Name:          "incident.txt",
			Title:         "Incident",
			PlainText:     "archived file text",
			MediaPath:     "files/ab/hash-incident.txt",
			ContentSHA256: "hash",
		}},
	}, nil))
	raw := []byte(`{
	  "token":"ignored",
	  "team_id":"T123",
	  "api_app_id":"A123",
	  "type":"event_callback",
	  "event":{
	    "type":"message",
	    "subtype":"message_deleted",
	    "channel":"C123",
	    "deleted_ts":"1710000000.000100",
	    "previous_message":{"text":"gone","ts":"1710000000.000100","thread_ts":"1710000000.000100","user":"U123"},
	    "event_ts":"1710000002.000200"
	  }
	}`)
	event, err := slackevents.ParseEvent(raw, slackevents.OptionNoVerifyToken())
	require.NoError(t, err)
	require.NoError(t, client.HandleEventsAPIEvent(ctx, st, "T123", event))

	rows, err := st.QueryReadOnly(ctx, "select ts, deleted_ts, client_msg_id, reply_count, latest_reply, normalized_text from messages where channel_id = 'C123' order by ts")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "1710000000.000100", rows[0]["ts"])
	require.Equal(t, "1710000000.000100", rows[0]["deleted_ts"])
	require.Equal(t, "client-1", rows[0]["client_msg_id"])
	require.Equal(t, int64(2), rows[0]["reply_count"])
	require.Equal(t, "1710000001.000200", rows[0]["latest_reply"])
	require.Equal(t, "gone [deleted]", rows[0]["normalized_text"])
	files, err := st.Files(ctx, store.FileListOptions{FileID: "F123"})
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, "files/ab/hash-incident.txt", files[0].MediaPath)
	matches, err := st.Search(ctx, "T123", "deleted", 10)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	matches, err = st.Search(ctx, "T123", "archived", 10)
	require.NoError(t, err)
	require.Len(t, matches, 1)
}

func TestHandleEventsAPIEventIndexesMentionsForDeletedTombstone(t *testing.T) {
	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()
	client := New(config.Tokens{Bot: "xoxb-test"})
	client.now = func() time.Time { return time.Date(2026, 3, 8, 1, 2, 3, 0, time.UTC) }
	ctx := context.Background()
	raw := []byte(`{
	  "token":"ignored",
	  "team_id":"T123",
	  "api_app_id":"A123",
	  "type":"event_callback",
	  "event":{
	    "type":"message",
	    "subtype":"message_deleted",
	    "channel":"C123",
	    "deleted_ts":"1710000000.000100",
	    "previous_message":{"text":"gone <@U234|sam>","ts":"1710000000.000100","user":"U123"},
	    "event_ts":"1710000002.000200"
	  }
	}`)
	event, err := slackevents.ParseEvent(raw, slackevents.OptionNoVerifyToken())
	require.NoError(t, err)
	require.NoError(t, client.HandleEventsAPIEvent(ctx, st, "T123", event))

	mentions, err := st.Mentions(ctx, "T123", "U234", 10)
	require.NoError(t, err)
	require.Len(t, mentions, 1)
	require.Equal(t, "1710000000.000100", mentions[0].TS)
	require.Equal(t, "sam", mentions[0].DisplayText)
}

func TestHandleEventsAPIEventIgnoresUnknown(t *testing.T) {
	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	client := New(config.Tokens{Bot: "xoxb-test"})
	require.NoError(t, client.HandleEventsAPIEvent(context.Background(), st, "T123", slackevents.EventsAPIEvent{}))
}

func TestChannelSyncPlanLatestOnlySkipsUnsyncedChannels(t *testing.T) {
	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	now := time.Date(2026, 3, 8, 1, 2, 3, 0, time.UTC)
	require.NoError(t, st.UpsertChannel(context.Background(), store.Channel{
		ID:          "C123",
		WorkspaceID: "T123",
		Name:        "general",
		Kind:        "public_channel",
		RawJSON:     "{}",
		UpdatedAt:   now,
	}))
	require.NoError(t, st.UpsertMessage(context.Background(), store.Message{
		ChannelID:      "C123",
		TS:             "1710000000.000100",
		WorkspaceID:    "T123",
		Text:           "already synced",
		NormalizedText: "already synced",
		SourceRank:     2,
		SourceName:     SourceBot,
		RawJSON:        "{}",
		UpdatedAt:      now,
	}, nil))

	client := &Client{}
	channels, oldestByChannel, err := client.channelSyncPlan(context.Background(), st, "T123", []slack.Channel{
		{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C123"}, Name: "general"}},
		{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C999"}, Name: "new-channel"}},
	}, SyncOptions{LatestOnly: true})
	require.NoError(t, err)
	require.Len(t, channels, 1)
	require.Equal(t, "C123", channels[0].ID)
	require.Contains(t, oldestByChannel, "C123")
	require.NotContains(t, oldestByChannel, "C999")
}
