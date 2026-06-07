package slackmcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseThreadMessagesAllowsBlankReplyAuthor(t *testing.T) {
	raw := `{
		"messages": "=== THREAD PARENT MESSAGE ===\nFrom: Ada (U123)\nTime: 2026-05-02 02:17:47 PDT\nMessage TS: 1777713467.400649\nroot\n\n=== THREAD REPLIES (1 total) ===\n\n--- Reply 1 of 1 ---\nFrom: \nTime: 2026-05-02 04:40:56 PDT\nMessage TS: 1777722056.863759\nreply",
		"pagination_info": "There are no more messages in this thread.\n"
	}`

	thread, err := parseThreadMessages(raw, "C123")
	require.NoError(t, err)
	require.Equal(t, "U123", thread.Parent.AuthorID)
	require.Len(t, thread.Replies, 1)
	require.Equal(t, "1777722056.863759", thread.Replies[0].TS)
	require.Empty(t, thread.Replies[0].AuthorID)
	require.Empty(t, thread.Replies[0].AuthorName)
}

func TestParseThreadMessagesAllowsParentOnlyPayload(t *testing.T) {
	raw := `{
		"messages": "=== THREAD PARENT MESSAGE ===\nFrom: Ada (U123)\nTime: 2026-05-05 14:36:24 PDT\nMessage TS: 1778016984.861209\nroot",
		"pagination_info": "There are no more messages in this thread.\n"
	}`

	thread, err := parseThreadMessages(raw, "C123")
	require.NoError(t, err)
	require.Equal(t, "1778016984.861209", thread.Parent.TS)
	require.Empty(t, thread.Replies)
}
