package slackmcp

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/stretchr/testify/require"
)

type authTransport func(*http.Request) (*http.Response, error)

func (fn authTransport) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestAutomaticAuthOriginBoundary(t *testing.T) {
	t.Setenv("CODEX_APPS_ACCESS_TOKEN", "synthetic-codex")
	t.Setenv("CODEX_APPS_ACCOUNT_ID", "synthetic-account")
	t.Setenv("DEDICATED_MCP_TOKEN", "")
	for _, endpoint := range []string{
		"https://other.example/mcp", "http://chatgpt.com/mcp",
		"https://chatgpt.com:444/mcp", "https://chatgpt.com.example/mcp",
		"https://user@chatgpt.com/mcp",
	} {
		t.Run(endpoint, func(t *testing.T) {
			client := &http.Client{Transport: authTransport(func(*http.Request) (*http.Response, error) {
				t.Fatal("custom origin received an implicit request")
				return nil, nil
			})}
			_, err := New(context.Background(), config.MCPConfig{BaseURL: endpoint, TokenEnv: "DEDICATED_MCP_TOKEN", AuthPath: filepath.Join(t.TempDir(), "absent-auth")}, client)
			require.ErrorContains(t, err, "dedicated token")
			require.NotContains(t, err.Error(), "read MCP auth file")
		})
	}
	t.Setenv("DEDICATED_MCP_TOKEN", "dedicated")
	auth, err := resolveAuth(config.MCPConfig{BaseURL: "http://local.example/mcp", TokenEnv: "DEDICATED_MCP_TOKEN"})
	require.NoError(t, err)
	require.Equal(t, "dedicated", auth.AccessToken)
	require.Empty(t, auth.AccountID)
	require.False(t, auth.Automatic)
	t.Setenv("CODEX_CONNECTORS_TOKEN", "selected-connector")
	auth, err = resolveAuth(config.MCPConfig{BaseURL: "https://chatgpt.com/mcp", TokenEnv: "CODEX_CONNECTORS_TOKEN"})
	require.NoError(t, err)
	require.Equal(t, "selected-connector", auth.AccessToken)
	require.True(t, auth.Automatic)
}

func TestDefaultGatewayAutomaticAuthAndNotifications(t *testing.T) {
	t.Setenv("CODEX_APPS_ACCESS_TOKEN", "synthetic-codex")
	t.Setenv("CODEX_APPS_ACCOUNT_ID", "synthetic-account")
	calls := 0
	client := &http.Client{Transport: authTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "chatgpt.com", r.URL.Host)
		require.Equal(t, "Bearer synthetic-codex", r.Header.Get("Authorization"))
		require.Equal(t, "synthetic-account", r.Header.Get("ChatGPT-Account-ID"))
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`))}, nil
	})}
	for _, endpoint := range []string{"https://chatgpt.com/backend-api/wham/apps", " \thttps://chatgpt.com/backend-api/wham/apps\n"} {
		c, err := New(context.Background(), config.MCPConfig{BaseURL: endpoint}, client)
		require.NoError(t, err)
		require.NoError(t, c.Close())
	}
	require.Equal(t, 4, calls)
	require.Nil(t, client.CheckRedirect)

	t.Setenv("CODEX_APPS_ACCESS_TOKEN", "")
	t.Setenv("CODEX_CONNECTORS_TOKEN", "")
	path := filepath.Join(t.TempDir(), "auth.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"tokens":{"access_token":"test-auth-token","account_id":"file-account"}}`), 0o600))
	auth, err := resolveAuth(config.MCPConfig{BaseURL: "https://chatgpt.com:443/mcp", AuthPath: path})
	require.NoError(t, err)
	require.Equal(t, "test-auth-token", auth.AccessToken)
	require.True(t, auth.Automatic)
}
