package search

import (
	"encoding/json"
	"html"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/slack-go/slack"
)

var (
	userMentionRe    = regexp.MustCompile(`<@([A-Z0-9]+)(?:\|([^>]+))?>`)
	channelMentionRe = regexp.MustCompile(`<#([A-Z0-9]+)(?:\|([^>]+))?>`)
	slackTokenRe     = regexp.MustCompile(`<([^>|]+)(?:\|([^>]*))?>`)
)

type Mention struct {
	Type        string
	TargetID    string
	DisplayText string
}

var rawVisibleTextKeys = map[string]struct{}{
	"accessibility_label": {},
	"alt_text":            {},
	"author_name":         {},
	"author_subname":      {},
	"description":         {},
	"fallback":            {},
	"footer":              {},
	"initial_date":        {},
	"initial_time":        {},
	"initial_value":       {},
	"label":               {},
	"placeholder":         {},
	"pretext":             {},
	"service_name":        {},
	"text":                {},
	"title":               {},
}

var rawHiddenTextContainers = map[string]struct{}{
	"confirm": {},
}

func NormalizeMessage(msg slack.Message) string {
	text := normalizeMessageText(msg.Text)

	parts := []string{strings.TrimSpace(text)}
	appendBlockParts(&parts, msg.Blocks)
	appendAttachmentParts(&parts, msg.Attachments)
	for _, file := range msg.Files {
		if file.Title != "" {
			parts = append(parts, sanitizeText(file.Title))
		}
		if file.Name != "" && file.Name != file.Title {
			parts = append(parts, sanitizeText(file.Name))
		}
		if file.PlainText != "" {
			parts = append(parts, sanitizeText(file.PlainText))
		}
		if file.PreviewPlainText != "" && file.PreviewPlainText != file.PlainText {
			parts = append(parts, sanitizeText(file.PreviewPlainText))
		}
	}
	if msg.Edited != nil {
		parts = append(parts, "[edited]")
	}
	if msg.SubType == "message_deleted" || msg.DeletedTimestamp != "" {
		parts = append(parts, "[deleted]")
	}
	if msg.ThreadTimestamp != "" && msg.ThreadTimestamp != msg.Timestamp {
		parts = append(parts, "[thread-reply]")
	}
	return joinNormalizedParts(parts)
}

func NormalizeRawPayloadText(value any) string {
	return strings.Join(normalizeRawPayloadParts(value), " ")
}

func normalizeRawPayloadParts(value any) []string {
	parts := make([]string, 0)
	appendRawVisibleText(&parts, value, "")
	return uniqueNormalizedParts(parts)
}

func NormalizeMessageWithRawPayload(msg slack.Message, rawPayload any) string {
	normalized := NormalizeMessage(msg)
	parts := []string{normalized}
	for _, rawPart := range normalizeRawPayloadParts(rawPayload) {
		if rawPart != "" {
			parts = append(parts, rawPart)
		}
	}
	return joinNormalizedParts(parts)
}

func appendRawVisibleText(parts *[]string, value any, key string) {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for childKey := range v {
			keys = append(keys, childKey)
		}
		sort.Strings(keys)
		for _, childKey := range keys {
			if _, hidden := rawHiddenTextContainers[childKey]; hidden {
				continue
			}
			if key == "fields" && childKey == "value" {
				appendRawVisibleText(parts, v[childKey], "text")
				continue
			}
			appendRawVisibleText(parts, v[childKey], childKey)
		}
	case []any:
		for _, item := range v {
			appendRawVisibleText(parts, item, key)
		}
	case string:
		if _, ok := rawVisibleTextKeys[key]; ok {
			appendNormalizedText(parts, v)
		}
	}
}

func appendBlockParts(parts *[]string, blocks slack.Blocks) {
	for _, block := range blocks.BlockSet {
		switch b := block.(type) {
		case *slack.SectionBlock:
			if b == nil {
				continue
			}
			appendTextObject(parts, b.Text)
			for _, field := range b.Fields {
				appendTextObject(parts, field)
			}
			appendAccessory(parts, b.Accessory)
		case *slack.HeaderBlock:
			if b == nil {
				continue
			}
			appendTextObject(parts, b.Text)
		case *slack.ContextBlock:
			if b == nil {
				continue
			}
			for _, element := range b.ContextElements.Elements {
				appendMixedElement(parts, element)
			}
		case *slack.ActionBlock:
			if b == nil || b.Elements == nil {
				continue
			}
			appendBlockElements(parts, b.Elements.ElementSet)
		case *slack.ContextActionsBlock:
			if b == nil || b.Elements == nil {
				continue
			}
			appendBlockElements(parts, b.Elements.ElementSet)
		case *slack.ImageBlock:
			if b == nil {
				continue
			}
			appendTextObject(parts, b.Title)
			appendNormalizedText(parts, b.AltText)
		case *slack.VideoBlock:
			if b == nil {
				continue
			}
			appendTextObject(parts, b.Title)
			appendTextObject(parts, b.Description)
			appendNormalizedText(parts, b.AltText)
			appendNormalizedText(parts, b.AuthorName)
			appendNormalizedText(parts, b.ProviderName)
		case *slack.MarkdownBlock:
			if b == nil {
				continue
			}
			appendNormalizedText(parts, b.Text)
		case *slack.CardBlock:
			appendCardBlock(parts, b)
		case *slack.CarouselBlock:
			if b == nil {
				continue
			}
			for _, card := range b.Elements {
				appendCardBlock(parts, card)
			}
		case *slack.InputBlock:
			if b == nil {
				continue
			}
			appendTextObject(parts, b.Label)
			appendTextObject(parts, b.Hint)
			appendBlockElement(parts, b.Element)
		case *slack.RichTextBlock:
			appendRichTextBlock(parts, b)
		case *slack.TableBlock:
			if b == nil {
				continue
			}
			for _, row := range b.Rows {
				for _, cell := range row {
					appendTableCell(parts, cell)
				}
			}
		case *slack.TaskCardBlock:
			appendTaskCardBlock(parts, b)
		case *slack.PlanBlock:
			if b == nil {
				continue
			}
			appendNormalizedText(parts, b.Title)
			for i := range b.Tasks {
				appendTaskCardBlock(parts, &b.Tasks[i])
			}
		case *slack.AlertBlock:
			if b == nil {
				continue
			}
			appendTextObject(parts, b.Text)
		case *slack.UnknownBlock:
			appendJSONVisibleText(parts, b)
		}
	}
}

func appendTableCell(parts *[]string, cell slack.TableCell) {
	switch c := cell.(type) {
	case nil:
		return
	case *slack.TableRichTextCell:
		if c == nil {
			return
		}
		appendRichTextBlock(parts, &slack.RichTextBlock{Type: slack.MBTRichText, Elements: c.Elements})
	case slack.TableRichTextCell:
		appendRichTextBlock(parts, &slack.RichTextBlock{Type: slack.MBTRichText, Elements: c.Elements})
	case *slack.TableRawTextCell:
		if c == nil {
			return
		}
		appendNormalizedText(parts, c.Text)
	case slack.TableRawTextCell:
		appendNormalizedText(parts, c.Text)
	case *slack.TableRawNumberCell:
		if c == nil {
			return
		}
		appendTableRawNumberCell(parts, *c)
	case slack.TableRawNumberCell:
		appendTableRawNumberCell(parts, c)
	}
}

func appendTableRawNumberCell(parts *[]string, cell slack.TableRawNumberCell) {
	if strings.TrimSpace(cell.Text) != "" {
		appendNormalizedText(parts, cell.Text)
		return
	}
	appendNormalizedText(parts, strconv.FormatFloat(cell.Value, 'f', -1, 64))
}

func appendCardBlock(parts *[]string, card *slack.CardBlock) {
	if card == nil {
		return
	}
	appendTextObject(parts, card.Title)
	appendTextObject(parts, card.Subtitle)
	appendTextObject(parts, card.Body)
	appendBlockElement(parts, card.Icon)
	appendBlockElement(parts, card.HeroImage)
	if card.Actions != nil {
		appendBlockElements(parts, card.Actions.ElementSet)
	}
}

func appendAttachmentParts(parts *[]string, attachments []slack.Attachment) {
	for _, attachment := range attachments {
		appendNormalizedText(parts, attachment.Fallback)
		appendNormalizedText(parts, attachment.AuthorName)
		appendNormalizedText(parts, attachment.AuthorSubname)
		appendNormalizedText(parts, attachment.Pretext)
		appendNormalizedText(parts, attachment.Title)
		appendNormalizedText(parts, attachment.Text)
		appendNormalizedText(parts, attachment.ServiceName)
		appendNormalizedText(parts, attachment.Footer)
		for _, field := range attachment.Fields {
			appendNormalizedText(parts, field.Title)
			appendNormalizedText(parts, field.Value)
		}
		for _, action := range attachment.Actions {
			appendNormalizedText(parts, action.Text)
			for _, option := range action.Options {
				appendNormalizedText(parts, option.Text)
				appendNormalizedText(parts, option.Description)
			}
			for _, group := range action.OptionGroups {
				appendNormalizedText(parts, group.Text)
				for _, option := range group.Options {
					appendNormalizedText(parts, option.Text)
					appendNormalizedText(parts, option.Description)
				}
			}
		}
		appendBlockParts(parts, attachment.Blocks)
	}
}

func appendTextObject(parts *[]string, object *slack.TextBlockObject) {
	if object == nil {
		return
	}
	appendNormalizedText(parts, object.Text)
}

func appendAccessory(parts *[]string, accessory *slack.Accessory) {
	if accessory == nil {
		return
	}
	appendBlockElement(parts, accessory.ImageElement)
	appendBlockElement(parts, accessory.ButtonElement)
	appendBlockElement(parts, accessory.OverflowElement)
	appendBlockElement(parts, accessory.SelectElement)
	appendBlockElement(parts, accessory.MultiSelectElement)
	appendBlockElement(parts, accessory.DatePickerElement)
	appendBlockElement(parts, accessory.TimePickerElement)
	appendBlockElement(parts, accessory.PlainTextInputElement)
	appendBlockElement(parts, accessory.RichTextInputElement)
	appendBlockElement(parts, accessory.CheckboxGroupsBlockElement)
	appendBlockElement(parts, accessory.RadioButtonsElement)
	appendBlockElement(parts, accessory.WorkflowButtonElement)
	appendBlockElement(parts, accessory.UnknownElement)
}

func appendMixedElement(parts *[]string, element slack.MixedElement) {
	switch e := element.(type) {
	case *slack.TextBlockObject:
		appendTextObject(parts, e)
	case *slack.ImageBlockElement:
		if e == nil {
			return
		}
		appendBlockElement(parts, e)
	}
}

func appendBlockElements(parts *[]string, elements []slack.BlockElement) {
	for _, element := range elements {
		appendBlockElement(parts, element)
	}
}

func appendBlockElement(parts *[]string, element slack.BlockElement) {
	switch e := element.(type) {
	case *slack.ImageBlockElement:
		if e == nil {
			return
		}
		appendNormalizedText(parts, e.AltText)
	case *slack.ButtonBlockElement:
		if e == nil {
			return
		}
		appendTextObject(parts, e.Text)
	case *slack.OverflowBlockElement:
		if e == nil {
			return
		}
		appendOptions(parts, e.Options)
	case *slack.SelectBlockElement:
		if e == nil {
			return
		}
		appendTextObject(parts, e.Placeholder)
		appendOptions(parts, e.Options)
		appendOptionGroups(parts, e.OptionGroups)
		appendOption(parts, e.InitialOption)
		appendNormalizedText(parts, e.InitialUser)
		appendNormalizedText(parts, e.InitialConversation)
		appendNormalizedText(parts, e.InitialChannel)
	case *slack.MultiSelectBlockElement:
		if e == nil {
			return
		}
		appendTextObject(parts, e.Placeholder)
		appendOptions(parts, e.Options)
		appendOptionGroups(parts, e.OptionGroups)
		appendOptions(parts, e.InitialOptions)
		for _, user := range e.InitialUsers {
			appendNormalizedText(parts, user)
		}
		for _, conversation := range e.InitialConversations {
			appendNormalizedText(parts, conversation)
		}
		for _, channel := range e.InitialChannels {
			appendNormalizedText(parts, channel)
		}
	case *slack.CheckboxGroupsBlockElement:
		if e == nil {
			return
		}
		appendOptions(parts, e.Options)
		appendOptions(parts, e.InitialOptions)
	case *slack.RadioButtonsBlockElement:
		if e == nil {
			return
		}
		appendOptions(parts, e.Options)
		appendOption(parts, e.InitialOption)
	case *slack.WorkflowButtonBlockElement:
		if e == nil {
			return
		}
		appendTextObject(parts, e.Text)
		appendNormalizedText(parts, e.AccessibilityLabel)
	case *slack.DatePickerBlockElement:
		if e == nil {
			return
		}
		appendTextObject(parts, e.Placeholder)
		appendNormalizedText(parts, e.InitialDate)
	case *slack.TimePickerBlockElement:
		if e == nil {
			return
		}
		appendTextObject(parts, e.Placeholder)
		appendNormalizedText(parts, e.InitialTime)
	case *slack.DateTimePickerBlockElement:
		if e == nil {
			return
		}
		if e.InitialDateTime != 0 {
			appendNormalizedText(parts, strconv.FormatInt(e.InitialDateTime, 10))
		}
	case *slack.PlainTextInputBlockElement:
		if e == nil {
			return
		}
		appendTextObject(parts, e.Placeholder)
		appendNormalizedText(parts, e.InitialValue)
	case *slack.RichTextInputBlockElement:
		if e == nil {
			return
		}
		appendTextObject(parts, e.Placeholder)
		appendRichTextBlock(parts, e.InitialValue)
	case *slack.EmailTextInputBlockElement:
		if e == nil {
			return
		}
		appendTextObject(parts, e.Placeholder)
		appendNormalizedText(parts, e.InitialValue)
	case *slack.URLTextInputBlockElement:
		if e == nil {
			return
		}
		appendTextObject(parts, e.Placeholder)
		appendNormalizedText(parts, e.InitialValue)
	case *slack.NumberInputBlockElement:
		if e == nil {
			return
		}
		appendTextObject(parts, e.Placeholder)
		appendNormalizedText(parts, e.InitialValue)
	case *slack.FeedbackButtonsBlockElement:
		if e == nil {
			return
		}
		appendFeedbackButton(parts, e.PositiveButton)
		appendFeedbackButton(parts, e.NegativeButton)
	case *slack.IconButtonBlockElement:
		if e == nil {
			return
		}
		appendTextObject(parts, e.Text)
		appendNormalizedText(parts, e.AccessibilityLabel)
	}
}

func appendRichTextBlock(parts *[]string, block *slack.RichTextBlock) {
	if block == nil {
		return
	}
	for _, element := range block.Elements {
		appendRichTextElement(parts, element)
	}
}

func appendRichTextElement(parts *[]string, element slack.RichTextElement) {
	switch e := element.(type) {
	case *slack.RichTextSection:
		if e == nil {
			return
		}
		appendRichTextSectionElements(parts, e.Elements)
	case *slack.RichTextList:
		if e == nil {
			return
		}
		for _, child := range e.Elements {
			appendRichTextElement(parts, child)
		}
	case *slack.RichTextQuote:
		if e == nil {
			return
		}
		appendRichTextSectionElements(parts, e.Elements)
	case *slack.RichTextPreformatted:
		if e == nil {
			return
		}
		appendRichTextSectionElements(parts, e.Elements)
	case *slack.RichTextUnknown:
		appendJSONVisibleText(parts, e)
	}
}

func appendRichTextSectionElements(parts *[]string, elements []slack.RichTextSectionElement) {
	for _, element := range elements {
		switch e := element.(type) {
		case *slack.RichTextSectionTextElement:
			if e != nil {
				appendNormalizedText(parts, e.Text)
			}
		case *slack.RichTextSectionChannelElement:
			if e != nil {
				appendNormalizedText(parts, "#"+e.ChannelID)
			}
		case *slack.RichTextSectionUserElement:
			if e != nil {
				appendNormalizedText(parts, "@"+e.UserID)
			}
		case *slack.RichTextSectionEmojiElement:
			if e != nil {
				appendNormalizedText(parts, ":"+e.Name+":")
				appendNormalizedText(parts, e.Unicode)
			}
		case *slack.RichTextSectionLinkElement:
			if e != nil {
				appendNormalizedText(parts, e.Text)
				appendNormalizedText(parts, e.URL)
			}
		case *slack.RichTextSectionTeamElement:
			if e != nil {
				appendNormalizedText(parts, e.TeamID)
			}
		case *slack.RichTextSectionUserGroupElement:
			if e != nil {
				appendNormalizedText(parts, "@"+e.UsergroupID)
			}
		case *slack.RichTextSectionDateElement:
			if e != nil {
				if e.Fallback != nil {
					appendNormalizedText(parts, *e.Fallback)
				} else {
					appendNormalizedText(parts, e.Format)
				}
			}
		case *slack.RichTextSectionBroadcastElement:
			if e != nil {
				appendNormalizedText(parts, "@"+e.Range)
			}
		case *slack.RichTextSectionUnknownElement:
			appendJSONVisibleText(parts, e)
		}
	}
}

func appendJSONVisibleText(parts *[]string, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		return
	}
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return
	}
	appendRawVisibleText(parts, raw, "")
}

func appendTaskCardBlock(parts *[]string, block *slack.TaskCardBlock) {
	if block == nil {
		return
	}
	appendNormalizedText(parts, block.Title)
	appendRichTextBlock(parts, block.Details)
	appendRichTextBlock(parts, block.Output)
	for _, source := range block.Sources {
		appendNormalizedText(parts, source.Text)
		appendNormalizedText(parts, source.URL)
	}
}

func appendFeedbackButton(parts *[]string, button *slack.FeedbackButton) {
	if button == nil {
		return
	}
	appendTextObject(parts, button.Text)
	appendNormalizedText(parts, button.AccessibilityLabel)
}

func appendOptions(parts *[]string, options []*slack.OptionBlockObject) {
	for _, option := range options {
		appendOption(parts, option)
	}
}

func appendOption(parts *[]string, option *slack.OptionBlockObject) {
	if option == nil {
		return
	}
	appendTextObject(parts, option.Text)
	appendTextObject(parts, option.Description)
}

func appendOptionGroups(parts *[]string, groups []*slack.OptionGroupBlockObject) {
	for _, group := range groups {
		if group == nil {
			continue
		}
		appendTextObject(parts, group.Label)
		appendOptions(parts, group.Options)
	}
}

func appendNormalizedText(parts *[]string, raw string) {
	if text := normalizeMessageText(raw); text != "" {
		*parts = append(*parts, text)
	}
}

func joinNormalizedParts(parts []string) string {
	return strings.Join(uniqueNormalizedParts(parts), " ")
}

func uniqueNormalizedParts(parts []string) []string {
	filtered := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		filtered = append(filtered, part)
	}
	return filtered
}

func normalizeMessageText(raw string) string {
	text := sanitizeText(raw)
	if text == "" {
		return ""
	}
	matches := slackTokenRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return sanitizeText(html.UnescapeString(text))
	}
	var b strings.Builder
	last := 0
	for _, match := range matches {
		b.WriteString(html.UnescapeString(text[last:match[0]]))
		target := text[match[2]:match[3]]
		label := ""
		if match[4] >= 0 {
			label = text[match[4]:match[5]]
		}
		b.WriteString(renderSlackToken(target, label))
		last = match[1]
	}
	b.WriteString(html.UnescapeString(text[last:]))
	return sanitizeText(b.String())
}

func renderSlackToken(target string, label string) string {
	switch {
	case strings.HasPrefix(target, "@"):
		if label != "" {
			return "@" + html.UnescapeString(label)
		}
		return "@" + html.UnescapeString(strings.TrimPrefix(target, "@"))
	case strings.HasPrefix(target, "#"):
		if label != "" {
			return "#" + html.UnescapeString(label)
		}
		return "#" + html.UnescapeString(strings.TrimPrefix(target, "#"))
	case label != "":
		return html.UnescapeString(label) + " " + html.UnescapeString(target)
	default:
		return html.UnescapeString(target)
	}
}

func ExtractMentions(text string) []Mention {
	text = sanitizeDisplayText(text)
	var mentions []Mention
	for _, match := range userMentionRe.FindAllStringSubmatch(text, -1) {
		mentions = append(mentions, Mention{
			Type:        "user",
			TargetID:    match[1],
			DisplayText: display(match[2], match[1]),
		})
	}
	for _, match := range channelMentionRe.FindAllStringSubmatch(text, -1) {
		mentions = append(mentions, Mention{
			Type:        "channel",
			TargetID:    match[1],
			DisplayText: display(match[2], match[1]),
		})
	}
	return mentions
}

func display(label string, fallback string) string {
	if label != "" {
		return label
	}
	return fallback
}

func filterEmpty(parts []string) []string {
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			filtered = append(filtered, strings.TrimSpace(part))
		}
	}
	return filtered
}

func sanitizeText(raw string) string {
	if raw == "" {
		return ""
	}
	raw = strings.ToValidUTF8(raw, "\uFFFD")
	raw = norm.NFKC.String(raw)
	var b strings.Builder
	b.Grow(len(raw))
	lastSpace := false
	for _, r := range raw {
		switch {
		case isIgnoredRune(r):
			continue
		case unicode.IsSpace(r):
			if lastSpace {
				continue
			}
			b.WriteByte(' ')
			lastSpace = true
		default:
			b.WriteRune(r)
			lastSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}

func sanitizeDisplayText(raw string) string {
	return sanitizeText(html.UnescapeString(raw))
}

func isIgnoredRune(r rune) bool {
	switch r {
	case '\u200b', '\u200c', '\u200d', '\ufeff':
		return true
	}
	if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
		return true
	}
	return false
}
