package slackapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/admission"
	"github.com/openclaw/slacrawl/internal/config"
)

const nativeDiagnosticCanary = "native-diagnostic-canary"

func TestNativeRetryAfterDurationRange(t *testing.T) {
	const maxSeconds = int64((1<<63 - 1) / time.Second)
	for _, tt := range []struct {
		name    string
		header  string
		invalid bool
		want    time.Duration
	}{
		{name: "negative", header: "-1", invalid: true},
		{name: "minimum integer", header: "-9223372036854775808", invalid: true},
		{name: "first overflow", header: strconv.FormatInt(maxSeconds+1, 10), invalid: true},
		{name: "maximum integer", header: "9223372036854775807", invalid: true},
		{name: "zero", header: "0"},
		{name: "normal", header: "2", want: 2 * time.Second},
		{name: "maximum duration", header: strconv.FormatInt(maxSeconds, 10), want: time.Duration(maxSeconds) * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls, sleeps := 0, 0
			client := NewWithOptions(config.Tokens{}, "https://fixture.invalid/", &http.Client{Transport: primaryOwnerRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     http.Header{"Retry-After": {tt.header}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    r,
				}, nil
			})})
			client.sleep = func(_ context.Context, delay time.Duration) error {
				sleeps++
				require.Equal(t, tt.want, delay)
				return nil
			}
			_, err := client.authTest(t.Context(), "fixture-token")
			if tt.invalid {
				require.EqualError(t, err, "slack auth.test rate-limit header failed")
				var diagnostic *nativeDiagnosticError
				require.ErrorAs(t, err, &diagnostic)
				require.Equal(t, 1, calls)
				require.Zero(t, sleeps)
				return
			}
			var limited *slack.RateLimitedError
			require.ErrorAs(t, err, &limited)
			require.Equal(t, tt.want, limited.RetryAfter)
			require.Equal(t, 3, calls)
			require.Equal(t, 2, sleeps)
		})
	}
}

func TestNativeDecodeDiagnosticsOmitPayload(t *testing.T) {
	for _, method := range []string{"auth.test", "conversations.info", "conversations.join", "conversations.list", "users.list", "conversations.history", "conversations.replies"} {
		for _, success := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ok=%t", method, success), func(t *testing.T) {
				calls := 0
				client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, func(r *http.Request, _ url.Values) (any, error) {
					calls++
					require.Equal(t, "/"+method, r.URL.Path)
					return json.RawMessage(fmt.Sprintf(`{"ok":%t,"error":"missing_scope","errors":[{"private":"native-diagnostic-canary"}],"team_id":"T123","channel":{"id":"C123","is_channel":true},"channels":[],"members":[],"messages":[],"response_metadata":{"next_cursor":"native-diagnostic-canary"}}`, success)), nil
				})
				err := nativeResponseCall(t, context.Background(), client, method)
				require.EqualError(t, err, "slack "+method+" response decode failed")
				require.NotContains(t, fmt.Sprintf("%+v", err), nativeDiagnosticCanary)
				require.NotContains(t, fmt.Errorf("caller: %w", err).Error(), nativeDiagnosticCanary)
				var diagnostic *nativeDiagnosticError
				require.ErrorAs(t, err, &diagnostic)
				require.Contains(t, errors.Unwrap(diagnostic).Error(), nativeDiagnosticCanary, "the retained cause is inspectable, not a redacted object")
				var native slack.SlackErrorResponse
				require.False(t, errors.As(err, &native), "typed decoding still precedes native success/error handling")
				require.Equal(t, 1, calls)
			})
		}
	}
}

func TestNativeMessageDecodeDiagnosticsOmitPayload(t *testing.T) {
	for _, method := range []string{"conversations.history", "conversations.replies"} {
		t.Run(method, func(t *testing.T) {
			calls := 0
			client := primaryOwnerClient(t, config.Tokens{User: "fixture-user"}, func(r *http.Request, _ url.Values) (any, error) {
				calls++
				require.Equal(t, "/"+method, r.URL.Path)
				return json.RawMessage(`{"ok":true,"messages":[{"type":"message","ts":"1710000000.000000","text":"private body","blocks":[{"type":"input","element":{"type":"native-diagnostic-canary"}}]}]}`), nil
			})
			err := nativeResponseCall(t, context.Background(), client, method)
			require.EqualError(t, err, "slack "+method+" message decode failed")
			require.NotContains(t, fmt.Sprintf("%+v", err), nativeDiagnosticCanary)
			require.Contains(t, errors.Unwrap(err).Error(), nativeDiagnosticCanary)
			require.Equal(t, 1, calls)
		})
	}
}

type nativeDiagnosticBody struct {
	io.Reader
	closes int
}

func (b *nativeDiagnosticBody) Close() error { b.closes++; return nil }

func TestNativeTransportDiagnosticsKeepCauses(t *testing.T) {
	for _, phase := range []string{"construction", "execution", "cancellation", "read", "retry-after", "status", "syntax", "type"} {
		t.Run(phase, func(t *testing.T) {
			failure := errors.New(nativeDiagnosticCanary)
			if phase == "cancellation" {
				failure = fmt.Errorf("%s: %w", nativeDiagnosticCanary, context.Canceled)
			}
			body := &nativeDiagnosticBody{Reader: strings.NewReader(`{"ok":true}`)}
			endpoint := "https://fixture.invalid/" + nativeDiagnosticCanary + "/"
			if phase == "construction" {
				endpoint = "https://fixture.invalid/\n" + nativeDiagnosticCanary + "/"
			}
			calls := 0
			client := NewWithOptions(config.Tokens{User: "fixture-user"}, endpoint, &http.Client{Transport: primaryOwnerRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if phase == "execution" || phase == "cancellation" {
					return nil, failure
				}
				response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body, Request: r}
				switch phase {
				case "read":
					body.Reader = io.MultiReader(strings.NewReader(`{"ok":true}`), iotest.ErrReader(failure))
				case "retry-after":
					response.StatusCode = http.StatusTooManyRequests
					response.Header.Set("Retry-After", nativeDiagnosticCanary)
				case "status":
					response.StatusCode = http.StatusBadGateway
					response.Status = "502 " + nativeDiagnosticCanary
					body.Reader = strings.NewReader(nativeDiagnosticCanary)
				case "syntax":
					body.Reader = strings.NewReader(`{"ok":true} {"private":"native-diagnostic-canary"}`)
				case "type":
					body.Reader = strings.NewReader(`{"ok":"native-diagnostic-canary"}`)
				}
				return response, nil
			})})
			client.sleep = func(context.Context, time.Duration) error { t.Fatal("these failures must not retry"); return nil }
			err := nativeResponseCall(t, context.Background(), client, "auth.test")
			want := map[string]string{
				"construction": "request construction", "execution": "request execution", "cancellation": "request execution",
				"read": "response body", "retry-after": "rate-limit header", "status": "HTTP status", "syntax": "response decode", "type": "response decode",
			}
			message := "slack auth.test " + want[phase] + " failed"
			if phase == "status" {
				message += " (HTTP 502)"
			}
			require.EqualError(t, err, message)
			require.NotContains(t, fmt.Sprintf("%+v", err), nativeDiagnosticCanary)
			switch phase {
			case "construction", "execution", "cancellation":
				var requestErr *url.Error
				require.ErrorAs(t, err, &requestErr)
				require.Contains(t, requestErr.Error(), nativeDiagnosticCanary)
				if phase != "construction" {
					require.ErrorIs(t, err, failure)
				}
				if phase == "cancellation" {
					require.ErrorIs(t, err, context.Canceled)
				}
			case "read":
				require.ErrorIs(t, err, failure)
			case "retry-after":
				var number *strconv.NumError
				require.ErrorAs(t, err, &number)
				require.Equal(t, nativeDiagnosticCanary, number.Num)
				require.ErrorIs(t, err, strconv.ErrSyntax)
			case "status":
				var status slack.StatusCodeError
				require.ErrorAs(t, err, &status)
				require.Equal(t, slack.StatusCodeError{Code: 502, Status: "502 " + nativeDiagnosticCanary}, status)
			case "syntax":
				var syntax *json.SyntaxError
				require.ErrorAs(t, err, &syntax)
			case "type":
				var typeErr *json.UnmarshalTypeError
				require.ErrorAs(t, err, &typeErr)
				require.Equal(t, "string", typeErr.Value)
				require.Equal(t, "bool", typeErr.Type.String())
			}
			if phase == "construction" {
				require.Zero(t, calls)
			} else {
				require.Equal(t, 1, calls)
			}
			if phase == "construction" || phase == "execution" || phase == "cancellation" {
				require.Zero(t, body.closes, "no response body was returned")
			} else {
				require.Equal(t, 1, body.closes, "every received body closes on error")
			}
		})
	}
}

func TestDoctorOmitsOptionalAuthDecodePayload(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(fmt.Sprintf("ok=%t", success), func(t *testing.T) {
			var calls []string
			client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot", User: "fixture-user"}, func(r *http.Request, form url.Values) (any, error) {
				require.Equal(t, "/auth.test", r.URL.Path, "unavailable optional auth must not reach any DM probe")
				calls = append(calls, form.Get("token"))
				if form.Get("token") == "fixture-bot" {
					return primaryOwnerResponse(r.URL.Path), nil
				}
				return json.RawMessage(fmt.Sprintf(`{"ok":%t,"team_id":"T123","errors":[{"private":"native-diagnostic-canary"}]}`, success)), nil
			}).WithDMPolicy(admission.Include)
			diag, err := client.Doctor(context.Background())
			require.NoError(t, err)
			require.Equal(t, []string{"fixture-bot", "fixture-user"}, calls)
			require.Equal(t, "T123", diag.BotAuthTeamID)
			require.True(t, diag.UserConfigured)
			require.False(t, diag.UserAuthAvailable)
			require.False(t, diag.DMsIncluded)
			require.Empty(t, diag.DMsMissingScope)
			require.Equal(t, "partial", diag.ThreadCoverage)
			require.Equal(t, "slack auth.test response decode failed", diag.UserAuthError)
			encoded, err := json.Marshal(diag)
			require.NoError(t, err)
			require.Contains(t, string(encoded), `"user_auth_error":"slack auth.test response decode failed"`)
			require.NotContains(t, string(encoded), nativeDiagnosticCanary)
		})
	}
}
