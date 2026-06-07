package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type StdioOptions struct {
	Command         string
	Args            []string
	ProtocolVersion string
	ClientName      string
	ClientVersion   string
}

type stdioResponse struct {
	ID     json.RawMessage  `json:"id"`
	Result *json.RawMessage `json:"result"`
	Error  *rpcError        `json:"error"`
}

type stdioResult struct {
	response stdioResponse
	err      error
}

type StdioClient struct {
	cmd             *exec.Cmd
	stdin           io.WriteCloser
	protocolVersion string
	clientName      string
	clientVersion   string
	nextID          atomic.Int64
	writeMu         sync.Mutex
	pendingMu       sync.Mutex
	pending         map[int64]chan stdioResult
	waitCh          chan error
	closeOnce       sync.Once
	waitErr         error
}

func NewStdio(_ context.Context, opts StdioOptions) (*StdioClient, error) {
	if strings.TrimSpace(opts.Command) == "" {
		return nil, errors.New("MCP stdio command is required")
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
	cmd := exec.Command(opts.Command, opts.Args...)
	cmd.Env = os.Environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open MCP stdio input: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open MCP stdio output: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open MCP stdio error output: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start MCP stdio command %q: %w", opts.Command, err)
	}
	client := &StdioClient{
		cmd: cmd, stdin: stdin, protocolVersion: opts.ProtocolVersion,
		clientName: opts.ClientName, clientVersion: opts.ClientVersion,
		pending: make(map[int64]chan stdioResult), waitCh: make(chan error, 1),
	}
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	go client.readLoop(stdout)
	go func() {
		err := cmd.Wait()
		processErr := errors.New("MCP stdio process exited")
		if err != nil {
			processErr = fmt.Errorf("MCP stdio process exited: %w", err)
		}
		client.failPending(processErr)
		client.waitCh <- err
	}()
	return client, nil
}

func (c *StdioClient) Initialize(ctx context.Context) error {
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": c.protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": c.clientName, "version": c.clientVersion},
	}, &result); err != nil {
		return err
	}
	if strings.TrimSpace(result.ProtocolVersion) == "" {
		return errors.New("MCP initialize response missing protocolVersion")
	}
	return c.notify(ctx, "notifications/initialized", map[string]any{})
}

func (c *StdioClient) ListTools(ctx context.Context) ([]Tool, error) {
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

func (c *StdioClient) CallToolText(ctx context.Context, name string, arguments map[string]any) (string, error) {
	var result toolCallResult
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": stripEmptyArguments(arguments)}, &result); err != nil {
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

func (c *StdioClient) call(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	resultCh := make(chan stdioResult, 1)
	c.pendingMu.Lock()
	c.pending[id] = resultCh
	c.pendingMu.Unlock()
	if err := c.write(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.removePending(id)
		return err
	}
	select {
	case <-ctx.Done():
		c.removePending(id)
		return ctx.Err()
	case result := <-resultCh:
		if result.err != nil {
			return result.err
		}
		if result.response.Error != nil {
			return fmt.Errorf("MCP JSON-RPC error %d: %s", result.response.Error.Code, result.response.Error.Message)
		}
		if result.response.Result == nil {
			return errors.New("MCP response missing result")
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(*result.response.Result, out); err != nil {
			return fmt.Errorf("decode MCP %s result: %w", method, err)
		}
		return nil
	}
}

func (c *StdioClient) notify(ctx context.Context, method string, params any) error {
	return c.write(ctx, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *StdioClient) write(ctx context.Context, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("write MCP stdio request: %w", err)
	}
	return nil
}

func (c *StdioClient) readLoop(stdout io.Reader) {
	decoder := json.NewDecoder(bufio.NewReader(stdout))
	for {
		var response stdioResponse
		if err := decoder.Decode(&response); err != nil {
			c.failPending(fmt.Errorf("decode MCP stdio JSON-RPC response: %w", err))
			return
		}
		var id int64
		if len(response.ID) == 0 || json.Unmarshal(response.ID, &id) != nil {
			continue
		}
		c.pendingMu.Lock()
		resultCh := c.pending[id]
		delete(c.pending, id)
		c.pendingMu.Unlock()
		if resultCh != nil {
			resultCh <- stdioResult{response: response}
		}
	}
}

func (c *StdioClient) removePending(id int64) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

func (c *StdioClient) failPending(err error) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[int64]chan stdioResult)
	c.pendingMu.Unlock()
	for _, resultCh := range pending {
		resultCh <- stdioResult{err: err}
	}
}

func (c *StdioClient) Close() error {
	c.closeOnce.Do(func() {
		_ = c.stdin.Close()
		select {
		case c.waitErr = <-c.waitCh:
		case <-time.After(2 * time.Second):
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			c.waitErr = <-c.waitCh
		}
	})
	return c.waitErr
}
