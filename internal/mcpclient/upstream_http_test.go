package mcpclient

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRPCErrorClassifiesUpstreamHTTPFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rpc      rpcError
		upstream bool
	}{
		{"mcporter upstream HTTP", rpcError{Code: -32603, Message: "MCP operation failed (CLIENT_HTTP_NOT_IMPLEMENTED); the request was not replayed. " + diagnosticCanary}, true},
		{"other internal error", rpcError{Code: -32603, Message: "MCP operation failed (runtime_error); " + diagnosticCanary}, false},
		{"other code", rpcError{Code: -32000, Message: "MCP operation failed (CLIENT_HTTP_NOT_IMPLEMENTED)"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rpc.err("tools/call")
			require.EqualError(t, err, fmt.Sprintf("MCP tools/call JSON-RPC error %d", tc.rpc.Code))
			require.Equal(t, tc.upstream, errors.Is(err, ErrUpstreamHTTP))
			require.Nil(t, errors.Unwrap(err))
			assertSafeDiagnostic(t, err)
		})
	}
}
