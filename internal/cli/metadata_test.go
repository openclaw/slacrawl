package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMetadataFormatFlag(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		args     []string
		wantErr  string
		wantJSON bool
		wantLog  bool
	}{
		{name: "equals json", args: []string{"metadata", "--format=json"}, wantJSON: true},
		{name: "split json", args: []string{"metadata", "--format", "json"}, wantJSON: true},
		{name: "json alias", args: []string{"metadata", "--json"}, wantJSON: true},
		{name: "single dash json", args: []string{"metadata", "-json"}, wantJSON: true},
		{name: "json wins over format", args: []string{"metadata", "--format=log", "--json"}, wantJSON: true},
		{name: "subcommand format overrides global", args: []string{"--format=log", "metadata", "--format=json"}, wantJSON: true},
		{name: "log", args: []string{"metadata", "--format=log"}, wantLog: true},
		{name: "help", args: []string{"metadata", "--help"}},
		{name: "short help", args: []string{"metadata", "-h"}},
		{name: "unknown format", args: []string{"metadata", "--format=xml"}, wantErr: `unsupported format "xml"`},
		{name: "positional", args: []string{"metadata", "extra"}, wantErr: "metadata takes flags only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			app := New()
			app.Stdout = &stdout
			app.Stderr = &stderr
			err := app.Run(context.Background(), tc.args)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			out := stdout.String()
			if tc.wantJSON {
				require.True(t, strings.HasPrefix(strings.TrimSpace(out), "{"))
				require.Contains(t, out, `"schema_version"`)
				return
			}
			require.NotContains(t, out, `"schema_version"`)
			if tc.wantLog {
				require.Contains(t, out, "schema_version=")
			}
		})
	}
}
