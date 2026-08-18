package slackapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/config"
)

func TestFetchDMsReturnsNilWhenUserClientMissing(t *testing.T) {
	client := New(config.Tokens{Bot: "xoxb-test"})
	dms, err := client.fetchDMs(context.Background(), "T123")
	require.NoError(t, err)
	require.Nil(t, dms)
}

func TestFetchDMsHappyPath(t *testing.T) {
	var seenTeamID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := mustDMFormValues(r)
		seenTeamID = values.Get("team_id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channels":[
			{"id":"D1","is_im":true,"is_private":true,"user":"U1"},
			{"id":"D2","is_im":true,"is_private":true,"user":"U2"},
			{"id":"G1","is_mpim":true,"is_private":true,"members":["U1","U2","U3"]}
		],"response_metadata":{"next_cursor":""}}`))
	}))
	defer server.Close()

	client := NewWithOptions(config.Tokens{User: "xoxp-test"}, server.URL+"/", server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }

	dms, err := client.fetchDMs(context.Background(), "T123")
	require.NoError(t, err)
	require.Len(t, dms, 3)
	require.Equal(t, "T123", seenTeamID)
	require.True(t, dms[0].IsIM)
	require.True(t, dms[2].IsMpIM)
}

func TestFetchDMsHandlesPagination(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		values := mustDMFormValues(r)
		cursor := values.Get("cursor")
		w.Header().Set("Content-Type", "application/json")
		switch cursor {
		case "":
			_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"D1","is_im":true,"user":"U1"}],"response_metadata":{"next_cursor":"page2"}}`))
		case "page2":
			_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"G1","is_mpim":true,"members":["U1","U2"]}],"response_metadata":{"next_cursor":""}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewWithOptions(config.Tokens{User: "xoxp-test"}, server.URL+"/", server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }

	dms, err := client.fetchDMs(context.Background(), "T123")
	require.NoError(t, err)
	require.Len(t, dms, 2)
	require.Equal(t, 2, calls)
}

func TestFetchDMsRejectsRepeatedCursor(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/conversations.list", r.URL.Path)
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"D1","is_im":true,"user":"U1"}],"response_metadata":{"next_cursor":"stuck"}}`))
	}))
	defer server.Close()

	client := NewWithOptions(config.Tokens{User: "xoxp-test"}, server.URL+"/", server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := client.fetchDMs(ctx, "T123")
	require.ErrorContains(t, err, `conversations.list repeated cursor "stuck"`)
	require.Equal(t, 2, calls)
}

func TestFetchDMsRetriesOnRateLimit(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		current := calls
		mu.Unlock()

		if current == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"D1","is_im":true,"user":"U1"}],"response_metadata":{"next_cursor":""}}`))
	}))
	defer server.Close()

	client := NewWithOptions(config.Tokens{User: "xoxp-test"}, server.URL+"/", server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }

	dms, err := client.fetchDMs(context.Background(), "T123")
	require.NoError(t, err)
	require.Len(t, dms, 1)
	require.Equal(t, 2, calls)
}

func TestDMChannelNameForIMAndMPIM(t *testing.T) {
	userByID := map[string]slack.User{
		"U1": {
			ID:       "U1",
			Name:     "alice-user",
			RealName: "Alice Real",
			Profile:  slack.UserProfile{DisplayName: "alice"},
		},
		"U2": {
			ID:       "U2",
			Name:     "bob-user",
			RealName: "Bob Real",
		},
		"U3": {
			ID:      "U3",
			Name:    "carol-user",
			Profile: slack.UserProfile{DisplayName: "carol"},
		},
	}

	im := slack.Channel{
		GroupConversation: slack.GroupConversation{
			Conversation: slack.Conversation{ID: "D1", IsIM: true, User: "U1"},
		},
	}
	require.Equal(t, "alice", dmChannelName(im, userByID))
	require.Equal(t, "U1", dmChannelName(im, nil))

	mpim := slack.Channel{
		GroupConversation: slack.GroupConversation{
			Conversation: slack.Conversation{ID: "G1", IsMpIM: true},
			Members:      []string{"U3", "U2", "U1"},
		},
	}
	require.Equal(t, "Bob Real,alice,carol", dmChannelName(mpim, userByID))

	longMPIM := slack.Channel{
		GroupConversation: slack.GroupConversation{
			Conversation: slack.Conversation{ID: "G2", IsMpIM: true},
			Members:      []string{"U1234567890", "U2234567890", "U3234567890", "U4234567890", "U5234567890"},
		},
	}
	name := dmChannelName(longMPIM, nil)
	require.True(t, strings.HasSuffix(name, "..."))
	require.LessOrEqual(t, len([]rune(name)), 40)
}

func mustDMFormValues(r *http.Request) url.Values {
	_ = r.ParseForm()
	if r.Form != nil {
		return r.Form
	}
	panic("missing form values")
}
