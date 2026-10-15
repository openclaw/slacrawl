package slackmcp

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/openclaw/slacrawl/internal/mcpclient"
	"github.com/stretchr/testify/require"
)

var errThrottled = fmt.Errorf("MCP tools/call JSON-RPC error -32603: %w", mcpclient.ErrUpstreamHTTP)

func retryClient(failures int, failure error) (*Client, *[]string, *[]time.Duration) {
	var cursors []string
	var delays []time.Duration
	client := &Client{
		pageSize: 100,
		maxPages: 5,
		sleep: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
		mcp: threadWorkSession(func(_ context.Context, _ string, args map[string]any) (string, error) {
			cursor, _ := args["cursor"].(string)
			cursors = append(cursors, cursor)
			if cursor == "next" && failures > 0 {
				failures--
				return "", failure
			}
			if cursor == "" {
				return `{"messages":"Channel: one (CONE)\n\n=== Message from User (UONE) at 2024-03-09T16:00:00Z === \nMessage TS: 1.000000\nfirst","pagination_info":"cursor ` + "`next`" + `"}`, nil
			}
			return `{"messages":"Channel: one (CONE)\n\n=== Message from User (UONE) at 2024-03-09T16:00:00Z === \nMessage TS: 2.000000\nsecond"}`, nil
		}),
	}
	return client, &cursors, &delays
}

func TestChannelMessagesRetriesUpstreamHTTPOnSameCursor(t *testing.T) {
	client, cursors, delays := retryClient(3, errThrottled)
	page, err := client.channelMessages(context.Background(), toolset{provider: providerCodex, readChannel: "read"}, "TLOCAL", "CONE", "", nil)
	require.NoError(t, err)
	require.Len(t, page.Messages, 2)
	require.Equal(t, []string{"", "next", "next", "next", "next"}, *cursors)
	require.Equal(t, []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}, *delays)
}

func TestChannelMessagesDoesNotRetryOtherErrors(t *testing.T) {
	other := errors.New("MCP tools/call JSON-RPC error -32603")
	client, cursors, delays := retryClient(1, other)
	_, err := client.channelMessages(context.Background(), toolset{provider: providerCodex, readChannel: "read"}, "TLOCAL", "CONE", "", nil)
	require.ErrorIs(t, err, other)
	require.Equal(t, []string{"", "next"}, *cursors)
	require.Empty(t, *delays)
}

func TestChannelMessagesGivesUpAfterUpstreamHTTPRetryCap(t *testing.T) {
	client, cursors, delays := retryClient(100, errThrottled)
	_, err := client.channelMessages(context.Background(), toolset{provider: providerCodex, readChannel: "read"}, "TLOCAL", "CONE", "", nil)
	require.ErrorIs(t, err, mcpclient.ErrUpstreamHTTP)
	require.Len(t, *cursors, 1+1+len(upstreamHTTPBackoff))
	require.Equal(t, upstreamHTTPBackoff, *delays)
}

func TestUpstreamHTTPRetryStopsWhenSleepIsCancelled(t *testing.T) {
	client, cursors, _ := retryClient(100, errThrottled)
	client.sleep = func(context.Context, time.Duration) error { return context.Canceled }
	_, err := client.channelMessages(context.Background(), toolset{provider: providerCodex, readChannel: "read"}, "TLOCAL", "CONE", "", nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, []string{"", "next"}, *cursors)
}

func TestUpstreamHTTPRetryStopsWhenWorkRevokedDuringBackoff(t *testing.T) {
	current := true
	calls := 0
	var delays []time.Duration
	client := &Client{
		pageSize: 100,
		maxPages: 5,
		sleep: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			current = false
			return nil
		},
		mcp: threadWorkSession(func(context.Context, string, map[string]any) (string, error) {
			calls++
			return "", errThrottled
		}),
	}
	page, err := client.channelMessages(context.Background(), toolset{provider: providerCodex, readChannel: "read"}, "TLOCAL", "CONE", "", func() (bool, error) {
		return current, nil
	})
	require.NoError(t, err)
	require.Equal(t, channelPage{revoked: true}, page)
	require.Equal(t, 1, calls)
	require.Equal(t, []time.Duration{2 * time.Second}, delays)
}

func TestUpstreamHTTPRetryRecoversWhenWorkStaysCurrent(t *testing.T) {
	client, cursors, delays := retryClient(1, errThrottled)
	checks := 0
	page, err := client.channelMessages(context.Background(), toolset{provider: providerCodex, readChannel: "read"}, "TLOCAL", "CONE", "", func() (bool, error) {
		checks++
		return true, nil
	})
	require.NoError(t, err)
	require.False(t, page.revoked)
	require.Len(t, page.Messages, 2)
	require.Greater(t, checks, 0)
	require.Equal(t, []string{"", "next", "next"}, *cursors)
	require.Equal(t, []time.Duration{2 * time.Second}, *delays)
}
