package slackapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/admission"
	"github.com/openclaw/slacrawl/internal/config"
)

const diagnosticContractCanary = "diagnostic-contract-canary"

func TestNativeDiagnosticContractDoesNotExposePrivateData(t *testing.T) {
	for _, fault := range []string{"decode", "api-error", "cursor"} {
		t.Run(fault, func(t *testing.T) {
			client := primaryOwnerClient(t, config.Tokens{Bot: "fixture-bot"}, func(r *http.Request, _ url.Values) (any, error) {
				switch fault {
				case "decode":
					return json.RawMessage(`{"ok":true,"team_id":"T123","errors":[{"private":"diagnostic-contract-canary"}]}`), nil
				case "api-error":
					return map[string]any{"ok": false, "error": diagnosticContractCanary}, nil
				default:
					return map[string]any{"ok": true, "channels": []any{}, "response_metadata": map[string]any{"next_cursor": diagnosticContractCanary}}, nil
				}
			})
			var err error
			if fault == "cursor" {
				_, err = client.fetchChannels(context.Background(), "T123")
			} else {
				_, err = client.authTest(context.Background(), client.tokens.Bot)
			}
			require.Error(t, err)
			require.NotContains(t, err.Error(), diagnosticContractCanary)
			if fault == "decode" {
				cause := errors.Unwrap(err)
				require.NotNil(t, cause)
				require.Contains(t, cause.Error(), diagnosticContractCanary)
			} else if fault == "api-error" {
				var native slack.SlackErrorResponse
				require.ErrorAs(t, err, &native)
				require.Equal(t, diagnosticContractCanary, native.Err)
			}
		})
	}
}

func TestDoctorContractReportsFailedDMProbe(t *testing.T) {
	client := primaryOwnerClient(t, config.Tokens{User: "fixture-user"}, func(r *http.Request, _ url.Values) (any, error) {
		if r.URL.Path == "/conversations.list" {
			return map[string]any{"ok": false, "error": diagnosticContractCanary}, nil
		}
		return primaryOwnerResponse(r.URL.Path), nil
	}).WithDMPolicy(admission.Include)
	diag, err := client.Doctor(context.Background())
	require.NoError(t, err)
	data, err := json.Marshal(diag)
	require.NoError(t, err)
	var report map[string]any
	require.NoError(t, json.Unmarshal(data, &report))
	require.Equal(t, "catalog_failed", report["dm_probe_error"])
	require.NotContains(t, string(data), diagnosticContractCanary)
}

func TestDoctorContractRejectsCanceledCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	client := primaryOwnerClient(t, config.Tokens{User: "fixture-user"}, func(r *http.Request, _ url.Values) (any, error) {
		calls++
		return primaryOwnerResponse(r.URL.Path), nil
	})
	_, err := client.Doctor(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, calls)
}

func TestNativeDiagnosticContractKeepsKnownThreadError(t *testing.T) {
	client := primaryOwnerClient(t, config.Tokens{User: "fixture-user"}, func(*http.Request, url.Values) (any, error) {
		return map[string]any{"ok": false, "error": "thread_not_found"}, nil
	})
	_, err := client.getConversationReplies(context.Background(), client.tokens.User, &slack.GetConversationRepliesParameters{ChannelID: "C123", Timestamp: "1710000001.000000"}, nil)
	require.EqualError(t, err, "thread_not_found")
	var native slack.SlackErrorResponse
	require.ErrorAs(t, err, &native)
	require.Equal(t, "thread_not_found", native.Err)
}
