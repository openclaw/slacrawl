package search

import (
	"encoding/json"
	"testing"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/require"
)

func TestNormalizeMessage(t *testing.T) {
	msg := slack.Message{}
	msg.Text = "Hello <@U123|alice> in <#C123|eng> <https://example.com|docs>"
	msg.Files = []slack.File{{Title: "runbook", Name: "runbook.md", PlainText: "deploy notes", PreviewPlainText: "preview copy"}}
	msg.Edited = &slack.Edited{Timestamp: "123.45"}

	normalized := NormalizeMessage(msg)
	require.Contains(t, normalized, "@alice")
	require.Contains(t, normalized, "#eng")
	require.Contains(t, normalized, "docs https://example.com")
	require.Contains(t, normalized, "runbook")
	require.Contains(t, normalized, "deploy notes")
	require.Contains(t, normalized, "preview copy")
	require.Contains(t, normalized, "[edited]")
}

func TestExtractMentions(t *testing.T) {
	mentions := ExtractMentions("hello <@U123|alice> and <#C123|eng>")
	require.Len(t, mentions, 2)
	require.Equal(t, "user", mentions[0].Type)
	require.Equal(t, "U123", mentions[0].TargetID)
	require.Equal(t, "channel", mentions[1].Type)
}

func TestNormalizeMessageSanitizesMalformedUnicodeAndWhitespace(t *testing.T) {
	msg := slack.Message{}
	msg.Text = "A\x00\u200b  cafe\u0301\tteam\uff01"

	normalized := NormalizeMessage(msg)
	require.Equal(t, "A caf\u00e9 team!", normalized)
}

func TestNormalizeMessageUnescapesSlackEntities(t *testing.T) {
	msg := slack.Message{}
	msg.Text = "AT&amp;T &lt;tag&gt; <https://example.com?q=AT&amp;T|docs &amp; faq> <https://example.com/math|1 &gt; 0> <https://example.com/literal|docs &amp;lt;tag&amp;gt;>"
	msg.Files = []slack.File{{Title: "AT&amp;T.md", PlainText: "&lt;div&gt;"}}

	normalized := NormalizeMessage(msg)
	require.Contains(t, normalized, "AT&T")
	require.Contains(t, normalized, "tag")
	require.Contains(t, normalized, "docs & faq https://example.com?q=AT&T")
	require.Contains(t, normalized, "1 > 0 https://example.com/math")
	require.Contains(t, normalized, "docs &lt;tag&gt; https://example.com/literal")
	require.Contains(t, normalized, "AT&amp;T.md")
	require.Contains(t, normalized, "&lt;div&gt;")
	require.NotContains(t, normalized, "AT&amp;T &lt;tag&gt;")
	require.NotContains(t, normalized, "docs &amp; faq")
}

func TestNormalizeMessageIncludesBlocksAndAttachments(t *testing.T) {
	msg := slack.Message{}
	msg.Blocks = slack.Blocks{BlockSet: []slack.Block{
		slack.NewHeaderBlock(slack.NewTextBlockObject("plain_text", "Release Notes", false, false)),
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", "Deploy <https://example.com/runbook|runbook> &amp; checklist", false, false),
			[]*slack.TextBlockObject{
				slack.NewTextBlockObject("mrkdwn", "*Impact* checkout", false, false),
			},
			nil,
		),
		slack.NewContextBlock("ctx", slack.NewTextBlockObject("mrkdwn", "Context <@U123|alice>", false, false)),
		slack.NewActionBlock("actions", slack.NewButtonBlockElement("ack", "ack", slack.NewTextBlockObject("plain_text", "Acknowledge", false, false))),
		slack.NewContextActionsBlock("ctx-actions", slack.NewButtonBlockElement("open", "open", slack.NewTextBlockObject("plain_text", "Open issue", false, false))),
		slack.NewImageBlock("https://example.com/diagram.png", "Architecture Diagram", "img", nil),
	}}
	msg.Attachments = []slack.Attachment{{
		Pretext: "PagerDuty",
		Title:   "Incident &amp; response",
		Text:    "service degraded in <#C123|eng>",
		Fields: []slack.AttachmentField{
			{Title: "Severity", Value: "SEV2"},
		},
	}}

	normalized := NormalizeMessage(msg)
	require.Contains(t, normalized, "Release Notes")
	require.Contains(t, normalized, "Deploy runbook https://example.com/runbook & checklist")
	require.Contains(t, normalized, "Impact")
	require.Contains(t, normalized, "checkout")
	require.Contains(t, normalized, "Context @alice")
	require.Contains(t, normalized, "Acknowledge")
	require.Contains(t, normalized, "Open issue")
	require.Contains(t, normalized, "Architecture Diagram")
	require.Contains(t, normalized, "PagerDuty")
	require.Contains(t, normalized, "Incident & response")
	require.Contains(t, normalized, "service degraded in #eng")
	require.Contains(t, normalized, "Severity")
	require.Contains(t, normalized, "SEV2")
}

func TestNormalizeMessageDeduplicatesFallbackBlockText(t *testing.T) {
	msg := slack.Message{}
	msg.Text = "hello from fallback"
	msg.Blocks = slack.Blocks{BlockSet: []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", "hello from fallback", false, false), nil, nil),
	}}

	require.Equal(t, "hello from fallback", NormalizeMessage(msg))
}

func TestNormalizeMessageWithRawPayloadKeepsSubstringLabels(t *testing.T) {
	msg := slack.Message{}
	msg.Text = "dragon"
	raw := []any{map[string]any{
		"type": "unknown_new",
		"text": "go",
		"fields": []any{
			map[string]any{"title": "Impact", "value": "customer visible"},
		},
		"value": "hidden action value",
	}}

	require.Equal(t, "dragon Impact customer visible go", NormalizeMessageWithRawPayload(msg, raw))
}

func TestNormalizeMessageIncludesRichBlocksAndActionElements(t *testing.T) {
	msg := slack.Message{}
	initialOption := slack.NewOptionBlockObject(
		"release-manager",
		slack.NewTextBlockObject("plain_text", "Release manager", false, false),
		slack.NewTextBlockObject("plain_text", "Owns deployment", false, false),
	)
	secondInitialOption := slack.NewOptionBlockObject(
		"incident-commander",
		slack.NewTextBlockObject("plain_text", "Incident commander", false, false),
		nil,
	)
	richDetails := slack.NewRichTextBlock("details",
		slack.NewRichTextSection(
			slack.NewRichTextSectionTextElement("rich detail", nil),
			slack.NewRichTextSectionLinkElement("https://example.com/rich", "rich link", nil),
		),
	)
	msg.Blocks = slack.Blocks{BlockSet: []slack.Block{
		richDetails,
		slack.NewTableBlock("table").AddRow(
			slack.NewTableRichTextCell(slack.NewRichTextSection(slack.NewRichTextSectionTextElement("table cell", nil))),
		),
		slack.NewTaskCardBlock("task-1", "Task title").WithDetails(
			slack.NewRichTextBlock("task-details", slack.NewRichTextSection(slack.NewRichTextSectionTextElement("task detail", nil))),
		).WithSources(slack.NewTaskCardSource("https://example.com/source", "source label")),
		slack.NewPlanBlock("Plan title").WithTasks(
			slack.NewTaskCardBlock("task-2", "Nested task"),
		),
		slack.NewCarouselBlock(slack.NewCardBlock().
			WithTitle(slack.NewTextBlockObject("plain_text", "Carousel card", false, false)).
			WithHeroImage(slack.NewImageBlockElement("https://example.com/hero.png", "Carousel hero")).
			WithActions(slack.NewButtonBlockElement("view", "view", slack.NewTextBlockObject("plain_text", "View card", false, false))),
		),
		slack.NewActionBlock("pickers",
			&slack.DatePickerBlockElement{
				Type:        slack.METDatepicker,
				Placeholder: slack.NewTextBlockObject("plain_text", "Pick date", false, false),
				InitialDate: "2026-05-15",
			},
			&slack.TimePickerBlockElement{
				Type:        slack.METTimepicker,
				Placeholder: slack.NewTextBlockObject("plain_text", "Pick time", false, false),
				InitialTime: "09:30",
			},
			&slack.DateTimePickerBlockElement{
				Type:            slack.METDatetimepicker,
				InitialDateTime: 1778847000,
			},
			slack.NewFeedbackButtonsBlockElement(
				"feedback",
				slack.NewFeedbackButton(slack.NewTextBlockObject("plain_text", "Helpful", false, false), "yes").WithAccessibilityLabel("Mark helpful"),
				slack.NewFeedbackButton(slack.NewTextBlockObject("plain_text", "Not helpful", false, false), "no"),
			),
			slack.NewIconButtonBlockElement(
				"trash",
				slack.NewTextBlockObject("plain_text", "Delete response", false, false),
				"delete",
			).WithAccessibilityLabel("Remove response"),
			slack.NewOptionsSelectBlockElement(
				slack.OptTypeExternal,
				slack.NewTextBlockObject("plain_text", "Select owner", false, false),
				"owner",
			).WithInitialOption(initialOption),
			slack.NewOptionsMultiSelectBlockElement(
				slack.MultiOptTypeExternal,
				slack.NewTextBlockObject("plain_text", "Select roles", false, false),
				"roles",
			).WithInitialOptions(secondInitialOption),
		),
	}}

	normalized := NormalizeMessage(msg)
	for _, want := range []string{
		"rich detail",
		"rich link",
		"https://example.com/rich",
		"table cell",
		"Task title",
		"task detail",
		"source label",
		"https://example.com/source",
		"Plan title",
		"Nested task",
		"Carousel card",
		"Carousel hero",
		"View card",
		"Pick date",
		"2026-05-15",
		"Pick time",
		"09:30",
		"1778847000",
		"Helpful",
		"Mark helpful",
		"Not helpful",
		"Delete response",
		"Remove response",
		"Select owner",
		"Release manager",
		"Owns deployment",
		"Select roles",
		"Incident commander",
	} {
		require.Contains(t, normalized, want)
	}
}

func TestNormalizeMessageIncludesTableCellVariants(t *testing.T) {
	msg := slack.Message{}
	msg.Blocks = slack.Blocks{BlockSet: []slack.Block{
		slack.NewTableBlock("table").AddRow(
			slack.NewTableRawTextCell("raw text cell"),
			slack.NewTableRawNumberCell(42.5),
			slack.NewTableRawNumberCell(7).WithText("seven displayed"),
			slack.NewTableRichTextCell(slack.NewRichTextSection(slack.NewRichTextSectionTextElement("rich text cell", nil))),
			nil,
		),
	}}

	normalized := NormalizeMessage(msg)
	require.Contains(t, normalized, "raw text cell")
	require.Contains(t, normalized, "42.5")
	require.Contains(t, normalized, "seven displayed")
	require.Contains(t, normalized, "rich text cell")
}

func TestNormalizeMessageIncludesUnknownBlockText(t *testing.T) {
	var msg slack.Message
	require.NoError(t, json.Unmarshal([]byte(`{
		"type": "message",
		"ts": "1.0",
		"blocks": [
			{"type": "new_block", "title": "unknown title", "text": {"type": "mrkdwn", "text": "unknown body"}}
		]
	}`), &msg))

	normalized := NormalizeMessage(msg)
	require.Contains(t, normalized, "unknown title")
	require.Contains(t, normalized, "unknown body")
}

func TestExtractMentionsSanitizesNoisyText(t *testing.T) {
	mentions := ExtractMentions("hello\u200b <@U123|alice>\x00 and <#C123|eng>")
	require.Len(t, mentions, 2)
	require.Equal(t, "alice", mentions[0].DisplayText)
	require.Equal(t, "eng", mentions[1].DisplayText)
}

func TestExtractMentionsUnescapesSlackEntities(t *testing.T) {
	mentions := ExtractMentions("hello &lt;@U123|alice&gt; and &lt;#C123|eng&gt;")
	require.Len(t, mentions, 2)
	require.Equal(t, "alice", mentions[0].DisplayText)
	require.Equal(t, "eng", mentions[1].DisplayText)
}
