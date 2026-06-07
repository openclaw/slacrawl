package slackmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/mcpclient"
)

type Client struct {
	mcp          mcpclient.Session
	connectorID  string
	channelTypes string
	pageSize     int
	searchLimit  int
	maxPages     int
}

type toolset struct {
	provider       providerKind
	searchChannels string
	searchUsers    string
	readChannel    string
	readThread     string
}

type providerKind string

const (
	providerCodex     providerKind = "codex"
	providerReference providerKind = "reference"
)

type searchEnvelope struct {
	Results        string `json:"results"`
	PaginationInfo string `json:"pagination_info"`
}

type messagesEnvelope struct {
	Messages       string `json:"messages"`
	PaginationInfo string `json:"pagination_info"`
}

type page[T any] struct {
	Items      []T
	NextCursor string
}

type channelPage struct {
	ChannelID   string
	ChannelName string
	Messages    []MessageRecord
	NextCursor  string
}

type threadPage struct {
	Parent     *MessageRecord
	Replies    []MessageRecord
	NextCursor string
}

func New(ctx context.Context, cfg config.MCPConfig, httpClient *http.Client) (*Client, error) {
	var mcp mcpclient.Session
	var err error
	switch strings.ToLower(strings.TrimSpace(cfg.Transport)) {
	case "", "http":
		auth, authErr := resolveAuth(cfg)
		if authErr != nil {
			return nil, authErr
		}
		mcp, err = mcpclient.New(mcpclient.Options{
			Endpoint:        cfg.BaseURL,
			AccessToken:     auth.AccessToken,
			AccountID:       auth.AccountID,
			ProtocolVersion: cfg.ProtocolVersion,
			ClientName:      "slacrawl",
			ClientVersion:   "dev",
			HTTPClient:      httpClient,
		})
	case "stdio":
		mcp, err = mcpclient.NewStdio(ctx, mcpclient.StdioOptions{
			Command:         cfg.Command,
			Args:            cfg.Args,
			ProtocolVersion: cfg.ProtocolVersion,
			ClientName:      "slacrawl",
			ClientVersion:   "dev",
		})
	default:
		return nil, fmt.Errorf("unsupported MCP transport %q", cfg.Transport)
	}
	if err != nil {
		return nil, err
	}
	client := &Client{
		mcp:          mcp,
		connectorID:  strings.TrimSpace(cfg.ConnectorID),
		channelTypes: cfg.ChannelTypes,
		pageSize:     cfg.PageSize,
		searchLimit:  cfg.SearchLimit,
		maxPages:     cfg.MaxPages,
	}
	if err := client.mcp.Initialize(ctx); err != nil {
		_ = client.mcp.Close()
		return nil, err
	}
	return client, nil
}

func (c *Client) Close() error { return c.mcp.Close() }

func (c *Client) discover(ctx context.Context) (toolset, error) {
	tools, err := c.mcp.ListTools(ctx)
	if err != nil {
		return toolset{}, err
	}
	tools, err = filterSlackTools(tools, c.connectorID)
	if err != nil {
		return toolset{}, err
	}
	if hasTools(tools, "slack_list_channels", "slack_get_channel_history", "slack_get_users") {
		return toolset{
			provider:       providerReference,
			searchChannels: "slack_list_channels",
			searchUsers:    "slack_get_users",
			readChannel:    "slack_get_channel_history",
			readThread:     toolNameOptional(tools, "slack_get_thread_replies"),
		}, nil
	}
	searchChannels, err := matchTool(tools, []string{"search", "channel"}, nil)
	if err != nil {
		return toolset{}, err
	}
	searchUsers, err := matchTool(tools, []string{"search", "user"}, []string{"profile"})
	if err != nil {
		return toolset{}, err
	}
	readChannel, err := matchTool(tools, []string{"read", "channel"}, []string{"search", "thread", "canvas"})
	if err != nil {
		return toolset{}, err
	}
	return toolset{
		provider:       providerCodex,
		searchChannels: searchChannels,
		searchUsers:    searchUsers,
		readChannel:    readChannel,
		readThread:     matchToolOptional(tools, []string{"read", "thread"}, []string{"search", "canvas"}),
	}, nil
}

func hasTools(tools []mcpclient.Tool, names ...string) bool {
	for _, name := range names {
		if toolNameOptional(tools, name) == "" {
			return false
		}
	}
	return true
}

func toolNameOptional(tools []mcpclient.Tool, name string) string {
	for _, tool := range tools {
		if tool.Name == name {
			return tool.Name
		}
	}
	return ""
}

func (c *Client) channels(ctx context.Context, tools toolset, query string) ([]ChannelRecord, error) {
	if tools.provider == providerReference {
		channels, err := c.referenceChannels(ctx, tools)
		if err != nil || strings.TrimSpace(query) == "" {
			return channels, err
		}
		query = strings.TrimPrefix(strings.TrimSpace(query), "#")
		filtered := channels[:0]
		for _, channel := range channels {
			if strings.EqualFold(channel.ID, query) || strings.EqualFold(channel.Name, query) {
				filtered = append(filtered, channel)
			}
		}
		return filtered, nil
	}
	return collectPages(c.maxPages, func(cursor string) (page[ChannelRecord], error) {
		raw, err := c.mcp.CallToolText(ctx, tools.searchChannels, map[string]any{
			"query":           requiredQuery(query),
			"cursor":          cursor,
			"limit":           c.searchLimit,
			"channel_types":   c.channelTypes,
			"response_format": "detailed",
		})
		if err != nil {
			return page[ChannelRecord]{}, err
		}
		return parseChannels(raw)
	})
}

func (c *Client) users(ctx context.Context, tools toolset) ([]UserRecord, error) {
	if tools.provider == providerReference {
		return c.referenceUsers(ctx, tools)
	}
	return collectPages(c.maxPages, func(cursor string) (page[UserRecord], error) {
		raw, err := c.mcp.CallToolText(ctx, tools.searchUsers, map[string]any{
			"query":           requiredQuery(""),
			"cursor":          cursor,
			"limit":           c.searchLimit,
			"response_format": "detailed",
		})
		if err != nil {
			return page[UserRecord]{}, err
		}
		return parseUsers(raw)
	})
}

func (c *Client) channelMessages(ctx context.Context, tools toolset, channelID, oldest string) (channelPage, error) {
	if tools.provider == providerReference {
		return c.referenceChannelMessages(ctx, tools, channelID, oldest)
	}
	var result channelPage
	err := walkPages(c.maxPages, func(cursor string) (string, error) {
		raw, err := c.mcp.CallToolText(ctx, tools.readChannel, map[string]any{
			"channel_id":      channelID,
			"cursor":          cursor,
			"oldest":          emptyToNil(oldest),
			"limit":           c.pageSize,
			"response_format": "detailed",
		})
		if err != nil {
			return "", err
		}
		next, err := parseChannelMessages(raw)
		if err != nil {
			return "", err
		}
		if result.ChannelID == "" {
			result.ChannelID = next.ChannelID
			result.ChannelName = next.ChannelName
		}
		result.Messages = append(result.Messages, next.Messages...)
		result.NextCursor = next.NextCursor
		return next.NextCursor, nil
	})
	return result, err
}

func (c *Client) threadMessages(ctx context.Context, tools toolset, channelID, threadTS string) (threadPage, error) {
	if tools.readThread == "" {
		return threadPage{}, errors.New("Slack MCP connector does not provide a read-thread tool")
	}
	if tools.provider == providerReference {
		return c.referenceThreadMessages(ctx, tools, channelID, threadTS)
	}
	var result threadPage
	err := walkPages(c.maxPages, func(cursor string) (string, error) {
		raw, err := c.mcp.CallToolText(ctx, tools.readThread, map[string]any{
			"channel_id":      channelID,
			"message_ts":      threadTS,
			"cursor":          cursor,
			"limit":           c.pageSize,
			"response_format": "detailed",
		})
		if err != nil {
			return "", err
		}
		next, err := parseThreadMessages(raw, channelID)
		if err != nil {
			return "", err
		}
		if result.Parent == nil {
			result.Parent = next.Parent
		}
		result.Replies = append(result.Replies, next.Replies...)
		result.NextCursor = next.NextCursor
		return next.NextCursor, nil
	})
	return result, err
}

func collectPages[T any](maxPages int, fetch func(string) (page[T], error)) ([]T, error) {
	var items []T
	err := walkPages(maxPages, func(cursor string) (string, error) {
		result, err := fetch(cursor)
		if err != nil {
			return "", err
		}
		items = append(items, result.Items...)
		return result.NextCursor, nil
	})
	return items, err
}

func walkPages(maxPages int, fetch func(string) (string, error)) error {
	cursor := ""
	seen := map[string]struct{}{}
	for pages := 0; ; pages++ {
		if maxPages > 0 && pages >= maxPages {
			return fmt.Errorf("MCP pagination exceeded max_pages=%d", maxPages)
		}
		next, err := fetch(cursor)
		if err != nil {
			return err
		}
		next = strings.TrimSpace(next)
		if next == "" {
			return nil
		}
		if _, ok := seen[next]; ok {
			return fmt.Errorf("MCP pagination repeated cursor %q", next)
		}
		seen[next] = struct{}{}
		cursor = next
	}
}

type authInfo struct {
	AccessToken string
	AccountID   string
}

func resolveAuth(cfg config.MCPConfig) (authInfo, error) {
	tokenEnv := cfg.TokenEnv
	if tokenEnv == "" {
		tokenEnv = "CODEX_APPS_ACCESS_TOKEN"
	}
	accountEnv := cfg.AccountIDEnv
	if accountEnv == "" {
		accountEnv = "CODEX_APPS_ACCOUNT_ID"
	}
	if token := strings.TrimSpace(os.Getenv(tokenEnv)); token != "" {
		return authInfo{AccessToken: token, AccountID: strings.TrimSpace(os.Getenv(accountEnv))}, nil
	}
	if token := strings.TrimSpace(os.Getenv("CODEX_APPS_ACCESS_TOKEN")); token != "" {
		return authInfo{AccessToken: token, AccountID: strings.TrimSpace(os.Getenv(accountEnv))}, nil
	}
	if token := strings.TrimSpace(os.Getenv("CODEX_CONNECTORS_TOKEN")); token != "" {
		return authInfo{AccessToken: token, AccountID: strings.TrimSpace(os.Getenv(accountEnv))}, nil
	}
	if strings.TrimSpace(cfg.AuthPath) == "" {
		return authInfo{}, fmt.Errorf("MCP token environment variable %s is unset and auth_path is empty", tokenEnv)
	}
	raw, err := os.ReadFile(cfg.AuthPath)
	if err != nil {
		return authInfo{}, fmt.Errorf("read MCP auth file %s: %w", cfg.AuthPath, err)
	}
	var payload struct {
		Tokens *struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return authInfo{}, fmt.Errorf("parse MCP auth file %s: %w", cfg.AuthPath, err)
	}
	if payload.Tokens == nil || strings.TrimSpace(payload.Tokens.AccessToken) == "" {
		return authInfo{}, fmt.Errorf("MCP auth file %s does not contain tokens.access_token", cfg.AuthPath)
	}
	return authInfo{AccessToken: strings.TrimSpace(payload.Tokens.AccessToken), AccountID: strings.TrimSpace(payload.Tokens.AccountID)}, nil
}

func filterSlackTools(tools []mcpclient.Tool, connectorID string) ([]mcpclient.Tool, error) {
	if connectorID != "" {
		exact := make([]mcpclient.Tool, 0, len(tools))
		for _, tool := range tools {
			if tool.Meta != nil && strings.TrimSpace(tool.Meta.ConnectorID) == connectorID {
				exact = append(exact, tool)
			}
		}
		if len(exact) > 0 {
			return exact, nil
		}
		return nil, fmt.Errorf("configured Slack MCP connector %q was not found", connectorID)
	}

	filtered := make([]mcpclient.Tool, 0, len(tools))
	for _, tool := range tools {
		if strings.Contains(toolIdentity(tool), "slack") {
			filtered = append(filtered, tool)
		}
	}
	if len(filtered) == 0 {
		return nil, fmt.Errorf("Slack MCP connector was not found; tools/list returned %d tools but none matched Slack", len(tools))
	}
	return filtered, nil
}

func matchTool(tools []mcpclient.Tool, required, forbidden []string) (string, error) {
	if name := matchToolOptional(tools, required, forbidden); name != "" {
		return name, nil
	}
	return "", fmt.Errorf("could not resolve Slack MCP tool requiring %q and excluding %q", required, forbidden)
}

func matchToolOptional(tools []mcpclient.Tool, required, forbidden []string) string {
	type candidate struct {
		name  string
		score int
	}
	var candidates []candidate
	for _, tool := range tools {
		identity := toolIdentity(tool)
		if !containsAll(identity, required) || containsAny(identity, forbidden) {
			continue
		}
		score := len(required) * 10
		if strings.Contains(strings.ToLower(tool.Name), strings.Join(required, "_")) {
			score += 100
		}
		if strings.Contains(identity, "slack") {
			score++
		}
		candidates = append(candidates, candidate{name: tool.Name, score: score})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].name < candidates[j].name
		}
		return candidates[i].score > candidates[j].score
	})
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0].name
}

func toolIdentity(tool mcpclient.Tool) string {
	parts := []string{strings.ToLower(tool.Name), strings.ToLower(tool.Title)}
	if tool.Meta != nil {
		parts = append(parts, strings.ToLower(tool.Meta.ConnectorName), strings.ToLower(tool.Meta.ResourceURI))
	}
	return strings.Join(parts, " ")
}

func containsAll(value string, needles []string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}

func containsAny(value string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func requiredQuery(value string) string {
	if value == "" {
		// Codex Slack search treats a present whitespace-only query as list-all;
		// an empty string would be removed by the generic MCP argument filter.
		return " "
	}
	return value
}

func emptyToNil(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
