package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

const DefaultProtocolVersion = "2025-03-26"

var errCredentialOrigin = errors.New("MCP redirect would leave the credential origin")

// Transport errors can contain response-controlled URLs or text. Preserve
// errors.Is checks without exposing those causes through rendering or Unwrap.
type diagnosticError struct {
	message string
	cause   error
}

func (e *diagnosticError) Error() string        { return e.message }
func (e *diagnosticError) Is(target error) bool { return errors.Is(e.cause, target) }

func ioDiagnostic(message string, cause error) error {
	for _, reason := range []error{context.Canceled, context.DeadlineExceeded, errCredentialOrigin} {
		if errors.Is(cause, reason) {
			message += ": " + reason.Error()
			break
		}
	}
	return &diagnosticError{message: message, cause: cause}
}

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
	Endpoint           string
	AccessToken        string
	AccountID          string
	ProtocolVersion    string
	ClientName         string
	ClientVersion      string
	HTTPClient         *http.Client
	RestrictAuthOrigin bool
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
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

var ErrUpstreamHTTP = errors.New("MCP upstream HTTP failure")

func (e *rpcError) err(method string) error {
	message := fmt.Sprintf("MCP %s JSON-RPC error %d", method, e.Code)
	if e.Code == -32603 && strings.Contains(e.Message, "(CLIENT_HTTP_NOT_IMPLEMENTED)") {
		return &diagnosticError{message: message, cause: ErrUpstreamHTTP}
	}
	return errors.New(message)
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
	opts.Endpoint = strings.TrimSpace(opts.Endpoint)
	if opts.Endpoint == "" {
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
	if opts.RestrictAuthOrigin {
		origin, err := url.Parse(opts.Endpoint)
		if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil {
			return nil, errors.New("automatic MCP authentication requires an HTTPS origin")
		}
		client := *opts.HTTPClient
		previous := client.CheckRedirect
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if req.URL.User != nil || !sameHTTPSOrigin(origin, req.URL) {
				return errCredentialOrigin
			}
			if previous != nil {
				if err := previous(req, via); err != nil {
					return err
				}
			} else if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			// Caller policy may rewrite the request before it is sent.
			if req.URL.User != nil || !sameHTTPSOrigin(origin, req.URL) {
				return errCredentialOrigin
			}
			return nil
		}
		opts.HTTPClient = &client
	}
	return &Client{
		endpoint:        opts.Endpoint,
		accessToken:     strings.TrimSpace(opts.AccessToken),
		accountID:       strings.TrimSpace(opts.AccountID),
		protocolVersion: opts.ProtocolVersion,
		clientName:      opts.ClientName,
		clientVersion:   opts.ClientVersion,
		httpClient:      opts.HTTPClient,
	}, nil
}

func sameHTTPSOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if u.Port() == "" {
			return "443"
		}
		return u.Port()
	}
	return a.Scheme == "https" && b.Scheme == "https" &&
		strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
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
	return listTools(ctx, c.call)
}

func (c *Client) CallToolText(ctx context.Context, name string, arguments map[string]any) (string, error) {
	return callToolText(ctx, c.call, name, arguments)
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
		return fmt.Errorf("encode MCP %s request: invalid parameters", method)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create MCP %s request: invalid endpoint", method)
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
		return ioDiagnostic(fmt.Sprintf("MCP %s request failed", method), err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return ioDiagnostic(fmt.Sprintf("read MCP %s HTTP %d response failed", method, resp.StatusCode), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("MCP %s returned HTTP %d", method, resp.StatusCode)
	}
	var envelope rpcEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode MCP %s JSON-RPC response: invalid response", method)
	}
	if envelope.Error != nil {
		return envelope.Error.err(method)
	}
	if envelope.Result == nil {
		return fmt.Errorf("MCP %s response missing result", method)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(*envelope.Result, out); err != nil {
		return fmt.Errorf("decode MCP %s result: invalid response", method)
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
		return fmt.Errorf("encode MCP %s request: invalid parameters", method)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create MCP %s request: invalid endpoint", method)
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
		return ioDiagnostic(fmt.Sprintf("MCP %s request failed", method), err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	if err != nil {
		return ioDiagnostic(fmt.Sprintf("read MCP %s HTTP %d response failed", method, resp.StatusCode), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("MCP %s returned HTTP %d", method, resp.StatusCode)
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
