package mcpclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type redirectTransport func(*http.Request) (*http.Response, error)

func (fn redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestAutomaticAuthRedirects(t *testing.T) {
	for _, target := range []string{"https://other.example/mcp", "http://chatgpt.com/mcp", "https://chatgpt.com:444/mcp", "https://chatgpt.com/next"} {
		t.Run(target, func(t *testing.T) {
			for _, notification := range []bool{false, true} {
				calls := 0
				original := &http.Client{Transport: redirectTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						return &http.Response{StatusCode: 307, Header: http.Header{"Location": []string{target}}, Body: io.NopCloser(strings.NewReader(""))}, nil
					}
					require.Equal(t, "https://chatgpt.com/next", r.URL.String())
					require.Equal(t, "Bearer token", r.Header.Get("Authorization"))
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"result":{}}`))}, nil
				})}
				c, err := New(Options{Endpoint: "https://chatgpt.com/mcp", AccessToken: "token", AccountID: "account", HTTPClient: original, RestrictAuthOrigin: true})
				require.NoError(t, err)
				if notification {
					err = c.notify(context.Background(), "notifications/initialized", nil)
				} else {
					err = c.call(context.Background(), "tools/list", nil, &struct{}{})
				}
				if target == "https://chatgpt.com/next" {
					require.NoError(t, err)
					require.Equal(t, 2, calls)
				} else {
					require.ErrorContains(t, err, "credential origin")
					require.Equal(t, 1, calls)
				}
				require.Nil(t, original.CheckRedirect)
			}
		})
	}
}

func TestAutomaticAuthRedirectCallerPolicy(t *testing.T) {
	callerError := errors.New("caller declined redirect")
	for _, tc := range []struct {
		name, target string
		callbackErr  error
		wantCalls    int
	}{
		{"changed origin", "https://other.example/mcp", nil, 1},
		{"downgrade", "http://chatgpt.com/mcp", nil, 1},
		{"userinfo", "https://user@chatgpt.com/mcp", nil, 1},
		{"same origin", "https://chatgpt.com/rewritten", nil, 2},
		{"caller error", "https://other.example/mcp", callerError, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, notification := range []bool{false, true} {
				calls, callbacks := 0, 0
				client := &http.Client{
					CheckRedirect: func(req *http.Request, via []*http.Request) error {
						callbacks++
						changed, err := url.Parse(tc.target)
						require.NoError(t, err)
						req.URL = changed
						return tc.callbackErr
					},
					Transport: redirectTransport(func(r *http.Request) (*http.Response, error) {
						calls++
						if calls == 1 {
							return &http.Response{StatusCode: 307, Header: http.Header{"Location": {"https://chatgpt.com/next"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
						}
						require.Equal(t, "https://chatgpt.com/rewritten", r.URL.String())
						require.Equal(t, "Bearer token", r.Header.Get("Authorization"))
						require.Equal(t, "account", r.Header.Get("ChatGPT-Account-ID"))
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"result":{}}`))}, nil
					}),
				}
				c, err := New(Options{Endpoint: "https://chatgpt.com/mcp", AccessToken: "token", AccountID: "account", HTTPClient: client, RestrictAuthOrigin: true})
				require.NoError(t, err)
				if notification {
					err = c.notify(context.Background(), "notifications/initialized", nil)
				} else {
					err = c.call(context.Background(), "tools/list", nil, &struct{}{})
				}
				if tc.callbackErr != nil {
					require.ErrorIs(t, err, tc.callbackErr)
				} else if tc.wantCalls == 1 {
					require.ErrorContains(t, err, "credential origin")
				} else {
					require.NoError(t, err)
				}
				require.Equal(t, tc.wantCalls, calls)
				require.Equal(t, 1, callbacks)
			}
		})
	}
	c, err := New(Options{Endpoint: "https://chatgpt.com/mcp", RestrictAuthOrigin: true})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodGet, "https://chatgpt.com/next", nil)
	require.NoError(t, err)
	require.ErrorContains(t, c.httpClient.CheckRedirect(req, make([]*http.Request, 10)), "stopped after 10 redirects")
}
