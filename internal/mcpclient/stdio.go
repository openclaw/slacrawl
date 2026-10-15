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
	Env             []string
	EnvAllowlist    []string
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

func NewStdio(ctx context.Context, opts StdioOptions) (*StdioClient, error) {
	if strings.TrimSpace(opts.Command) == "" {
		return nil, errors.New("MCP stdio command is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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
	cmd := exec.CommandContext(ctx, opts.Command, opts.Args...)
	cmd.Env = stdioEnvironment(opts)
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
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		client.readLoop(stdout)
	}()
	go func() {
		// cmd.Wait closes the stdout pipe, so let the read loop drain it
		// first: a server that writes its final response and exits at once
		// would otherwise have that response turned into a decode error.
		<-readDone
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

func stdioEnvironment(opts StdioOptions) []string {
	keys := append([]string{
		"COMSPEC",
		"HOME",
		"LOGNAME",
		"PATH",
		"PATHEXT",
		"SHELL",
		"SYSTEMROOT",
		"TEMP",
		"TMP",
		"TMPDIR",
		"USER",
		"USERNAME",
		"WINDIR",
	}, opts.EnvAllowlist...)
	seen := make(map[string]int, len(keys))
	env := make([]string, 0, len(keys)+len(opts.Env))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		value, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		seen[key] = len(env)
		env = append(env, key+"="+value)
	}
	for _, item := range opts.Env {
		key, _, ok := strings.Cut(item, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		if idx, ok := seen[key]; ok {
			env[idx] = item
			continue
		}
		seen[key] = len(env)
		env = append(env, item)
	}
	return env
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
	return listTools(ctx, c.call)
}

func (c *StdioClient) CallToolText(ctx context.Context, name string, arguments map[string]any) (string, error) {
	return callToolText(ctx, c.call, name, arguments)
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
			return result.response.Error.err(method)
		}
		if result.response.Result == nil {
			return fmt.Errorf("MCP %s response missing result", method)
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(*result.response.Result, out); err != nil {
			return fmt.Errorf("decode MCP %s result: invalid response", method)
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
		return errors.New("encode MCP stdio request: invalid parameters")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Write from a goroutine so a child that stops reading stdin cannot
	// park the caller after context cancel. Closing stdin and killing the
	// process unblocks a full pipe, matching provider.Sync.
	errCh := make(chan error, 1)
	go func() {
		_, writeErr := c.stdin.Write(append(raw, '\n'))
		errCh <- writeErr
	}()
	select {
	case <-ctx.Done():
		_ = c.stdin.Close()
		if c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		<-errCh
		return ctx.Err()
	case err := <-errCh:
		if err != nil {
			return ioDiagnostic("write MCP stdio request failed", err)
		}
		return nil
	}
}

func (c *StdioClient) readLoop(stdout io.Reader) {
	decoder := json.NewDecoder(bufio.NewReader(stdout))
	for {
		var response stdioResponse
		if err := decoder.Decode(&response); err != nil {
			c.failPending(errors.New("decode MCP stdio JSON-RPC response: invalid or closed response stream"))
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
			// A killed server normally reaches waitCh via stdout EOF, but a
			// grandchild that inherited the pipe can hold it open forever;
			// give up after a bound instead of hanging Close.
			select {
			case c.waitErr = <-c.waitCh:
			case <-time.After(5 * time.Second):
				c.waitErr = errors.New("MCP stdio process did not exit after kill")
			}
		}
	})
	return c.waitErr
}
