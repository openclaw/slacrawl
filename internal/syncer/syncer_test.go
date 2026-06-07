package syncer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSourceAliases(t *testing.T) {
	cases := map[string]Source{
		"":          SourceAPI,
		"api":       SourceAPI,
		"bot":       SourceAPI,
		"desktop":   SourceDesktop,
		"wiretap":   SourceDesktop,
		"mcp":       SourceMCP,
		"connector": SourceMCP,
		"all":       SourceAll,
		"hybrid":    SourceAll,
	}
	for input, want := range cases {
		got, err := ParseSource(input)
		require.NoError(t, err, input)
		require.Equal(t, want, got, input)
	}
}

func TestParseSourceRejectsUnknown(t *testing.T) {
	_, err := ParseSource("github")
	require.ErrorContains(t, err, "unsupported source")
}
