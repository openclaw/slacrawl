package slackmcp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type ChannelRecord struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Topic      string `json:"topic"`
	Purpose    string `json:"purpose"`
	Permalink  string `json:"permalink"`
	IsPrivate  bool   `json:"is_private"`
	IsArchived bool   `json:"is_archived"`
}

type UserRecord struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	RealName  string `json:"real_name"`
	Title     string `json:"title"`
	Email     string `json:"email"`
	Timezone  string `json:"timezone"`
	Permalink string `json:"permalink"`
	IsBot     bool   `json:"is_bot"`
}

type MessageRecord struct {
	ChannelID     string   `json:"channel_id"`
	ChannelName   string   `json:"channel_name"`
	TS            string   `json:"ts"`
	ThreadTS      string   `json:"thread_ts"`
	AuthorID      string   `json:"author_id"`
	AuthorName    string   `json:"author_name"`
	OccurredAt    string   `json:"occurred_at"`
	Text          string   `json:"text"`
	ReplyCount    int      `json:"reply_count"`
	LatestReply   string   `json:"latest_reply"`
	Reactions     []string `json:"reactions,omitempty"`
	FileSummaries []string `json:"file_summaries,omitempty"`
}

type messageMetadata struct {
	ReplyCount  int
	LatestReply string
	Reactions   []string
	Files       []string
}

var (
	identityRE          = regexp.MustCompile(`^(?P<name>.+?) \((?:ID: )?(?:<@)?(?P<id>[A-Z0-9]+)(?:\|[^>]+)?(?:>)?\)\s*$`)
	linkRE              = regexp.MustCompile(`\((https?://[^)]+)\)`)
	cursorRE            = regexp.MustCompile("`([^`]+)`")
	readChannelHeaderRE = regexp.MustCompile(`^Channel: #?(?P<name>.+?) \((?P<id>[A-Z0-9]+)\)$`)
	threadFooterRE      = regexp.MustCompile(`^(?P<count>\d+) replies \(latest: (?P<latest>.+?)\)$`)
)

func parseChannels(raw string) (page[ChannelRecord], error) {
	var envelope searchEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return page[ChannelRecord]{}, fmt.Errorf("decode Slack MCP channel search payload: %w", err)
	}
	var channels []ChannelRecord
	for _, block := range resultBlocks(envelope.Results) {
		fields := parseFields(block)
		id := firstNonEmpty(fields["Channel ID"], fields["ID"], permalinkChannelID(fields["Permalink"]))
		if id == "" {
			return page[ChannelRecord]{}, errorsf("channel result missing ID", block)
		}
		kind, private := channelKind(fields)
		channels = append(channels, ChannelRecord{
			ID:         id,
			Name:       strings.TrimPrefix(fields["Name"], "#"),
			Kind:       kind,
			Topic:      fields["Topic"],
			Purpose:    fields["Purpose"],
			Permalink:  parseMarkdownLink(fields["Permalink"]),
			IsPrivate:  private,
			IsArchived: strings.EqualFold(fields["Is Archived"], "true"),
		})
	}
	return page[ChannelRecord]{Items: channels, NextCursor: extractCursor(envelope.PaginationInfo)}, nil
}

func parseUsers(raw string) (page[UserRecord], error) {
	var envelope searchEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return page[UserRecord]{}, fmt.Errorf("decode Slack MCP user search payload: %w", err)
	}
	var users []UserRecord
	for _, block := range resultBlocks(envelope.Results) {
		fields := parseFields(block)
		id := firstNonEmpty(fields["User ID"], fields["ID"])
		if id == "" {
			return page[UserRecord]{}, errorsf("user result missing ID", block)
		}
		users = append(users, UserRecord{
			ID:        id,
			Name:      firstNonEmpty(fields["Username"], fields["Name"]),
			RealName:  firstNonEmpty(fields["Real Name"], fields["Name"]),
			Title:     fields["Title"],
			Email:     fields["Email"],
			Timezone:  fields["Timezone"],
			Permalink: parseMarkdownLink(fields["Permalink"]),
		})
	}
	return page[UserRecord]{Items: users, NextCursor: extractCursor(envelope.PaginationInfo)}, nil
}

func parseChannelMessages(raw string) (channelPage, error) {
	var envelope messagesEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return channelPage{}, fmt.Errorf("decode Slack MCP channel payload: %w", err)
	}
	header := firstLine(envelope.Messages)
	matches := readChannelHeaderRE.FindStringSubmatch(header)
	if matches == nil {
		return channelPage{}, fmt.Errorf("parse Slack MCP channel header %q", header)
	}
	result := channelPage{
		ChannelID:   matches[2],
		ChannelName: matches[1],
		NextCursor:  extractCursor(envelope.PaginationInfo),
	}
	start := strings.Index(envelope.Messages, "=== Message from ")
	if start < 0 {
		return result, nil
	}
	body := strings.TrimPrefix(envelope.Messages[start:], "=== Message from ")
	for _, block := range strings.Split(body, "\n\n=== Message from ") {
		header, remainder, ok := strings.Cut(block, " === \nMessage TS: ")
		if !ok {
			continue
		}
		ts, messageBody, ok := strings.Cut(remainder, "\n")
		if !ok {
			continue
		}
		identity, occurredAt, ok := strings.Cut(header, " at ")
		if !ok {
			return channelPage{}, fmt.Errorf("parse Slack MCP message time from %q", header)
		}
		name, id := parseNameID(identity)
		text, metadata := splitMessageBodyAndMetadata(messageBody)
		result.Messages = append(result.Messages, MessageRecord{
			ChannelID:     result.ChannelID,
			ChannelName:   result.ChannelName,
			TS:            strings.TrimSpace(ts),
			AuthorID:      id,
			AuthorName:    firstNonEmpty(name, identity),
			OccurredAt:    occurredAt,
			Text:          text,
			ReplyCount:    metadata.ReplyCount,
			LatestReply:   metadata.LatestReply,
			Reactions:     metadata.Reactions,
			FileSummaries: metadata.Files,
		})
	}
	return result, nil
}

func parseThreadMessages(raw, channelID string) (threadPage, error) {
	var envelope messagesEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return threadPage{}, fmt.Errorf("decode Slack MCP thread payload: %w", err)
	}
	parentPart, repliesPart, ok := strings.Cut(envelope.Messages, "\n\n=== THREAD REPLIES")
	if !ok {
		parent, err := parseThreadMessage(envelope.Messages, channelID, "")
		if err != nil {
			return threadPage{}, fmt.Errorf("Slack MCP thread payload missing replies separator")
		}
		return threadPage{Parent: &parent, NextCursor: extractCursor(envelope.PaginationInfo)}, nil
	}
	parent, err := parseThreadMessage(parentPart, channelID, "")
	if err != nil {
		return threadPage{}, err
	}
	var replies []MessageRecord
	repliesBody := repliesPart
	if _, rest, ok := strings.Cut(repliesPart, "\n\n"); ok {
		repliesBody = rest
	}
	repliesBody = strings.TrimPrefix(repliesBody, "--- Reply ")
	for _, block := range strings.Split(repliesBody, "\n\n--- Reply ") {
		_, body, ok := strings.Cut(block, " ---\n")
		if !ok {
			continue
		}
		reply, err := parseThreadMessage(body, channelID, parent.TS)
		if err != nil {
			return threadPage{}, err
		}
		replies = append(replies, reply)
	}
	return threadPage{Parent: &parent, Replies: replies, NextCursor: extractCursor(envelope.PaginationInfo)}, nil
}

func parseThreadMessage(raw, channelID, threadTS string) (MessageRecord, error) {
	lines := strings.Split(raw, "\n")
	from := fieldValue(lines, "From: ")
	occurredAt := fieldValue(lines, "Time: ")
	ts := fieldValue(lines, "Message TS: ")
	if occurredAt == "" || ts == "" {
		return MessageRecord{}, fmt.Errorf("Slack MCP thread message missing required fields")
	}
	name, id := parseNameID(from)
	text, metadata := splitMessageBodyAndMetadata(collectAfterMessageTS(lines))
	return MessageRecord{
		ChannelID:     channelID,
		TS:            ts,
		ThreadTS:      threadTS,
		AuthorID:      id,
		AuthorName:    firstNonEmpty(name, from),
		OccurredAt:    occurredAt,
		Text:          text,
		ReplyCount:    metadata.ReplyCount,
		LatestReply:   metadata.LatestReply,
		Reactions:     metadata.Reactions,
		FileSummaries: metadata.Files,
	}, nil
}

func channelKind(fields map[string]string) (string, bool) {
	kind := strings.ToLower(firstNonEmpty(fields["Type"], fields["Channel Type"]))
	private := strings.EqualFold(fields["Is Private"], "true") || strings.Contains(kind, "private")
	if private {
		return "private_channel", true
	}
	if strings.Contains(kind, "public") || strings.EqualFold(fields["Is Private"], "false") {
		return "public_channel", false
	}
	return "mcp_channel", false
}

func parseFields(block string) map[string]string {
	fields := map[string]string{}
	currentKey := ""
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimRight(line, " \t")
		if trimmed == "---" {
			continue
		}
		if key, value, ok := splitField(trimmed); ok {
			currentKey = key
			fields[key] = value
			continue
		}
		if strings.TrimSpace(trimmed) != "" && currentKey != "" {
			if fields[currentKey] != "" {
				fields[currentKey] += "\n"
			}
			fields[currentKey] += trimmed
		}
	}
	return fields
}

func splitField(line string) (string, string, bool) {
	key, value, ok := strings.Cut(line, ":")
	if !ok {
		return "", "", false
	}
	for _, ch := range key {
		if (ch < 'A' || ch > 'Z') && (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != ' ' && ch != '_' {
			return "", "", false
		}
	}
	return key, strings.TrimSpace(value), true
}

func resultBlocks(results string) []string {
	parts := strings.Split(results, "### Result ")
	blocks := make([]string, 0, len(parts)-1)
	for _, part := range parts[1:] {
		if _, body, ok := strings.Cut(part, "\n"); ok {
			blocks = append(blocks, body)
		}
	}
	return blocks
}

func parseNameID(value string) (string, string) {
	matches := identityRE.FindStringSubmatch(strings.TrimSpace(value))
	if matches == nil {
		return strings.TrimSpace(value), ""
	}
	return matches[1], matches[2]
}

func parseMarkdownLink(value string) string {
	matches := linkRE.FindStringSubmatch(value)
	if matches == nil {
		return ""
	}
	return matches[1]
}

func permalinkChannelID(value string) string {
	link := parseMarkdownLink(value)
	index := strings.Index(link, "/archives/")
	if index < 0 {
		return ""
	}
	rest := link[index+len("/archives/"):]
	if id, _, ok := strings.Cut(rest, "/"); ok {
		return id
	}
	return rest
}

func extractCursor(value string) string {
	matches := cursorRE.FindStringSubmatch(value)
	if matches == nil {
		return ""
	}
	return matches[1]
}

func splitMessageBodyAndMetadata(raw string) (string, messageMetadata) {
	lines := strings.Split(raw, "\n")
	metadata := messageMetadata{}
	for len(lines) > 0 {
		last := strings.TrimSpace(lines[len(lines)-1])
		switch {
		case last == "":
			lines = lines[:len(lines)-1]
		case strings.HasPrefix(last, "Files: "):
			metadata.Files = splitMetadataList(strings.TrimPrefix(last, "Files: "))
			lines = lines[:len(lines)-1]
		case strings.HasPrefix(last, "Reactions: "):
			metadata.Reactions = splitMetadataList(strings.TrimPrefix(last, "Reactions: "))
			lines = lines[:len(lines)-1]
		case strings.HasPrefix(last, "Thread: "):
			matches := threadFooterRE.FindStringSubmatch(strings.TrimPrefix(last, "Thread: "))
			if matches == nil {
				return strings.TrimSpace(strings.Join(lines, "\n")), metadata
			}
			metadata.ReplyCount, _ = strconv.Atoi(matches[1])
			metadata.LatestReply = matches[2]
			lines = lines[:len(lines)-1]
		default:
			return strings.TrimSpace(strings.Join(lines, "\n")), metadata
		}
	}
	return "", metadata
}

func splitMetadataList(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func collectAfterMessageTS(lines []string) string {
	var result []string
	capture := false
	for _, line := range lines {
		if strings.HasPrefix(line, "Message TS: ") {
			capture = true
			continue
		}
		if capture {
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}

func fieldValue(lines []string, prefix string) string {
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}

func firstLine(value string) string {
	if line, _, ok := strings.Cut(value, "\n"); ok {
		return line
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func errorsf(message, payload string) error {
	payload = strings.TrimSpace(payload)
	if len(payload) > 120 {
		payload = payload[:120] + "..."
	}
	return fmt.Errorf("%s: %s", message, payload)
}
