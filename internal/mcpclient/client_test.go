package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClientInitializeListsToolsAndCallsTool(t *testing.T) {
	listCalls := 0
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer token", r.Header.Get("Authorization"))
		require.Equal(t, "account", r.Header.Get("ChatGPT-Account-ID"))
		var req struct {
			Method string           `json:"method"`
			Params map[string]any   `json:"params"`
			ID     *json.RawMessage `json:"id"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		methods = append(methods, req.Method)
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": DefaultProtocolVersion}
		case "notifications/initialized":
			require.Nil(t, req.ID)
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			listCalls++
			if listCalls == 1 {
				result = map[string]any{"tools": []map[string]any{{"name": "first"}}, "nextCursor": "next"}
			} else {
				require.Equal(t, "next", req.Params["cursor"])
				result = map[string]any{"tools": []map[string]any{{"name": "second"}}, "nextCursor": ""}
			}
		case "tools/call":
			args := req.Params["arguments"].(map[string]any)
			require.NotContains(t, args, "empty")
			result = map[string]any{"isError": false, "content": []map[string]any{{"type": "text", "text": "ok"}}}
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}))
	}))
	defer server.Close()

	client, err := New(Options{Endpoint: server.URL, AccessToken: "token", AccountID: "account"})
	require.NoError(t, err)
	require.NoError(t, client.Initialize(context.Background()))
	tools, err := client.ListTools(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"first", "second"}, []string{tools[0].Name, tools[1].Name})
	text, err := client.CallToolText(context.Background(), "first", map[string]any{"empty": "", "value": "yes"})
	require.NoError(t, err)
	require.Equal(t, "ok", text)
	require.Equal(t, []string{"initialize", "notifications/initialized", "tools/list", "tools/list", "tools/call"}, methods)
}

func TestStdioClientInitializeListsToolsAndCallsTool(t *testing.T) {
	t.Setenv("GO_WANT_MCP_STDIO_HELPER", "1")
	client, err := NewStdio(context.Background(), StdioOptions{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestMCPStdioHelperProcess"},
	})
	require.NoError(t, err)
	require.NoError(t, client.Initialize(context.Background()))
	tools, err := client.ListTools(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"slack_list_channels"}, []string{tools[0].Name})
	text, err := client.CallToolText(context.Background(), "slack_list_channels", map[string]any{"limit": 1})
	require.NoError(t, err)
	require.JSONEq(t, `{"ok":true}`, text)
	require.NoError(t, client.Close())
}

func TestStdioClientHandlesLargeOutputAndStderr(t *testing.T) {
	t.Setenv("GO_WANT_MCP_STDIO_HELPER", "1")
	client, err := NewStdio(context.Background(), StdioOptions{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestMCPStdioHelperProcess"},
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, client.Close()) }()
	require.NoError(t, client.Initialize(context.Background()))

	text, err := client.CallToolText(context.Background(), "large", nil)
	require.NoError(t, err)
	require.Len(t, text, 128<<10)
	require.Equal(t, strings.Repeat("x", 128<<10), text)
}

func TestStdioClientIgnoresLateCanceledResponse(t *testing.T) {
	t.Setenv("GO_WANT_MCP_STDIO_HELPER", "1")
	client, err := NewStdio(context.Background(), StdioOptions{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestMCPStdioHelperProcess"},
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, client.Close()) }()
	require.NoError(t, client.Initialize(context.Background()))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = client.CallToolText(ctx, "slow", nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	time.Sleep(100 * time.Millisecond)
	text, err := client.CallToolText(context.Background(), "fast", nil)
	require.NoError(t, err)
	require.Equal(t, "ok", text)
}

func TestMCPStdioHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_MCP_STDIO_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &request))
		if request.Method == "notifications/initialized" {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": DefaultProtocolVersion}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "slack_list_channels"}}}
		case "tools/call":
			var text string
			switch request.Params.Name {
			case "large":
				_, err := fmt.Fprint(os.Stderr, strings.Repeat("e", 128<<10))
				require.NoError(t, err)
				text = strings.Repeat("x", 128<<10)
			case "slow":
				time.Sleep(75 * time.Millisecond)
				text = "late"
			case "fast":
				text = "ok"
			default:
				text = `{"ok":true}`
			}
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
		default:
			t.Fatalf("unexpected method %q", request.Method)
		}
		require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}))
	}
	require.NoError(t, scanner.Err())
}

func TestClientRejectsRepeatedToolCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1, "result": map[string]any{"tools": []any{}, "nextCursor": "same"},
		}))
	}))
	defer server.Close()
	client, err := New(Options{Endpoint: server.URL})
	require.NoError(t, err)
	_, err = client.ListTools(context.Background())
	require.ErrorContains(t, err, "repeated cursor")
}
