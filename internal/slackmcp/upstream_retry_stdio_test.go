package slackmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/slacrawl/internal/mcpclient"
	"github.com/stretchr/testify/require"
)

func TestStdioUpstreamRetryRechecksOwnership(t *testing.T) {
	for _, mode := range []string{"recover", "revoke"} {
		t.Run(mode, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "requests.log")
			current := true
			session, err := mcpclient.NewStdio(context.Background(), mcpclient.StdioOptions{
				Command: os.Args[0],
				Args:    []string{"-test.run=^TestMCPUpstreamRetryStdioHelper$"},
				Env:     []string{"SLACRAWL_MCP_RETRY_LOG=" + logPath},
			})
			require.NoError(t, err)
			t.Cleanup(func() { _ = session.Close() })
			client := &Client{
				mcp:      session,
				pageSize: 100,
				maxPages: 2,
				sleep: func(context.Context, time.Duration) error {
					if mode == "revoke" {
						current = false
					}
					return nil
				},
			}
			page, err := client.channelMessages(context.Background(), toolset{provider: providerCodex, readChannel: "read"}, "TLOCAL", "CONE", "", func() (bool, error) {
				return current, nil
			})
			require.NoError(t, session.Close())
			logged, readErr := os.ReadFile(logPath)
			require.NoError(t, readErr)
			lines := strings.FieldsFunc(string(logged), func(r rune) bool { return r == '\n' })
			t.Logf("mode=%s requests=%d\n%s", mode, len(lines), logged)
			if mode == "revoke" {
				require.NoError(t, err)
				require.Equal(t, channelPage{revoked: true}, page)
				require.Len(t, lines, 1)
				return
			}
			require.NoError(t, err)
			require.False(t, page.revoked)
			require.Len(t, page.Messages, 1)
			require.Len(t, lines, 2)
			require.Equal(t, lines[0], lines[1])
		})
	}
}

func TestMCPUpstreamRetryStdioHelper(t *testing.T) {
	logPath := os.Getenv("SLACRAWL_MCP_RETRY_LOG")
	if logPath == "" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	encoder := json.NewEncoder(os.Stdout)
	calls := 0
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			os.Exit(2)
		}
		if request.Method != "tools/call" {
			os.Exit(3)
		}
		calls++
		encoded, err := json.Marshal(request.Params.Arguments)
		if err != nil {
			os.Exit(4)
		}
		if err := appendRetryLog(logPath, "tools/call "+string(encoded)); err != nil {
			os.Exit(5)
		}
		if calls == 1 {
			if err := encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"error": map[string]any{
					"code":    -32603,
					"message": "MCP operation failed (CLIENT_HTTP_NOT_IMPLEMENTED); the request was not replayed.",
				},
			}); err != nil {
				os.Exit(6)
			}
			continue
		}
		if err := encoder.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"result": map[string]any{"content": []map[string]any{{
				"type": "text",
				"text": `{"messages":"Channel: one (CONE)\n\n=== Message from User (UONE) at 2024-03-09T16:00:00Z === \nMessage TS: 1.000000\nfirst"}`,
			}}},
		}); err != nil {
			os.Exit(7)
		}
	}
	os.Exit(0)
}

func appendRetryLog(path, line string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = file.WriteString(line + "\n")
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
