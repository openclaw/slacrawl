package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const DefaultProtocolVersion = "2025-03-26"

type ToolMeta struct {
	ConnectorID   string `json:"connector_id"`
	ConnectorName string `json:"connector_name"`
	ResourceURI   string `json:"resource_uri"`
}

type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Meta        *ToolMeta       `json:"_meta"`
}

type Options struct {
	Endpoint        string
	AccessToken     string
	AccountID       string
	ProtocolVersion string
	ClientName      string
	ClientVersion   string
	HTTPClient      *http.Client
}

type Client struct {
	endpoint        string
	accessToken     string
	accountID       string
	protocolVersion string
	clientName      string
	clientVersion   string
	httpClient      *http.Client
	nextID          atomic.Int64
}

type Session interface {
	Initialize(context.Context) error
	ListTools(context.Context) ([]Tool, error)
	CallToolText(context.Context, string, map[string]any) (string, error)
	Close() error
}

type rpcEnvelope struct {
	Result *json.RawMessage `json:"result"`
	Error  *rpcError        `json:"error"`
}

type rpcError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type toolsPage struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor"`
}

type toolCallResult struct {
	IsError bool `json:"isError"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func New(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.Endpoint) == "" {
		return nil, errors.New("MCP endpoint is required")
	}
	if opts.ProtocolVersion == "" {
		opts.ProtocolVersion = DefaultProtocolVersion
	}
	if opts.ClientName == "" {
		opts.ClientName = "slacrawl"
	}
	if opts.ClientVersion == "" {
		opts.ClientVersion = "dev"
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{
		endpoint:        strings.TrimSpace(opts.Endpoint),
		accessToken:     strings.TrimSpace(opts.AccessToken),
		accountID:       strings.TrimSpace(opts.AccountID),
		protocolVersion: opts.ProtocolVersion,
		clientName:      opts.ClientName,
		clientVersion:   opts.ClientVersion,
		httpClient:      opts.HTTPClient,
	}, nil
}

func (c *Client) Close() error { return nil }

func (c *Client) Initialize(ctx context.Context) error {
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": c.protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    c.clientName,
			"version": c.clientVersion,
		},
	}, &result); err != nil {
		return err
	}
	if strings.TrimSpace(result.ProtocolVersion) == "" {
		return errors.New("MCP initialize response missing protocolVersion")
	}
	return c.notify(ctx, "notifications/initialized", map[string]any{})
}

func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var tools []Tool
	cursor := ""
	seen := map[string]bool{}
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page toolsPage
		if err := c.call(ctx, "tools/list", params, &page); err != nil {
			return nil, err
		}
		tools = append(tools, page.Tools...)
		if strings.TrimSpace(page.NextCursor) == "" {
			return tools, nil
		}
		if seen[page.NextCursor] {
			return nil, fmt.Errorf("MCP tools/list repeated cursor %q", page.NextCursor)
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
}

func (c *Client) CallToolText(ctx context.Context, name string, arguments map[string]any) (string, error) {
	var result toolCallResult
	if err := c.call(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": stripEmptyArguments(arguments),
	}, &result); err != nil {
		return "", err
	}
	parts := make([]string, 0, len(result.Content))
	for _, item := range result.Content {
		if item.Type != "" && item.Type != "text" {
			continue
		}
		if strings.TrimSpace(item.Text) != "" {
			parts = append(parts, item.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if result.IsError {
		if text == "" {
			return "", fmt.Errorf("MCP tool %q reported an error", name)
		}
		return "", fmt.Errorf("MCP tool %q reported an error: %s", name, text)
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("MCP tool %q returned no text content", name)
	}
	return text, nil
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
	}
	if c.accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", c.accountID)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call MCP endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read MCP response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("MCP endpoint returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var envelope rpcEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode MCP JSON-RPC response: %w", err)
	}
	if envelope.Error != nil {
		if len(envelope.Error.Data) > 0 {
			return fmt.Errorf("MCP JSON-RPC error %d: %s (%s)", envelope.Error.Code, envelope.Error.Message, envelope.Error.Data)
		}
		return fmt.Errorf("MCP JSON-RPC error %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	if envelope.Result == nil {
		return errors.New("MCP response missing result")
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(*envelope.Result, out); err != nil {
		return fmt.Errorf("decode MCP %s result: %w", method, err)
	}
	return nil
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
	}
	if c.accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", c.accountID)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("notify MCP endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read MCP notification response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("MCP notification returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func stripEmptyArguments(arguments map[string]any) map[string]any {
	filtered := make(map[string]any, len(arguments))
	for key, value := range arguments {
		if value == nil {
			continue
		}
		if text, ok := value.(string); ok && text == "" {
			continue
		}
		filtered[key] = value
	}
	return filtered
}
