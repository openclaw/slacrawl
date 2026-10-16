package cli

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openclaw/crawlkit/tui"
	"github.com/openclaw/slacrawl/internal/store"
)

func TestSlackTUIRowsKeepsThreadParentOutsideTheLimit(t *testing.T) {
	reply := store.MessageRow{
		WorkspaceID: "T", ChannelID: "C", TS: "300.000", ThreadTS: "100.000",
		Text: "reply", NormalizedText: "reply",
	}
	other := store.MessageRow{
		WorkspaceID: "T", ChannelID: "C", TS: "200.000",
		Text: "other", NormalizedText: "other",
	}
	parent := store.MessageRow{
		WorkspaceID: "T", ChannelID: "C", TS: "100.000", ThreadTS: "100.000",
		Text: "parent", NormalizedText: "parent", ReplyCount: 1,
	}

	got := slackTUIRows([]store.MessageRow{reply, other, parent}, false, false, 1)
	require.Equal(t, []string{"C/100.000", "C/300.000"}, tuiRowIDs(got))

	got = slackTUIRows([]store.MessageRow{reply, other, parent}, false, false, 2)
	require.Equal(t, []string{"C/100.000", "C/300.000", "C/200.000"}, tuiRowIDs(got))

	alreadyTogether := slackTUIRows([]store.MessageRow{parent, reply}, false, false, 2)
	require.Equal(t, []string{"C/100.000", "C/300.000"}, tuiRowIDs(alreadyTogether))

	missingParent := slackTUIRows([]store.MessageRow{{
		WorkspaceID: "T", ChannelID: "C", TS: "300.000", ThreadTS: "999.000",
		Text: "reply", NormalizedText: "reply",
	}}, false, false, 1)
	require.Equal(t, []string{"C/300.000"}, tuiRowIDs(missingParent))

	withDraft := slackTUIRows([]store.MessageRow{
		{WorkspaceID: "T", ChannelID: "C", TS: "draft:1", Subtype: "desktop_draft", Text: "draft", NormalizedText: "draft"},
		reply,
		parent,
	}, false, false, 1)
	require.Equal(t, []string{"C/100.000", "C/300.000"}, tuiRowIDs(withDraft))
}

func tuiRowIDs(rows []tui.Row) []string {
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	return ids
}
