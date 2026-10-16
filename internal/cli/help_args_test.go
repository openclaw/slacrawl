package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHelpArgsDoNotSwallowOrdinaryTokens(t *testing.T) {
	t.Setenv("SLACRAWL_NO_UPDATE_CHECK", "1")
	missing := filepath.Join(t.TempDir(), "missing.toml")
	ctx := context.Background()

	usage := [][]string{
		{"search", "--help"},
		{"search", "-h"},
		{"search", "help"},
		{"messages", "--help"},
		{"sql", "-h"},
		{"files", "help"},
		{"purge", "help"},
	}
	for _, args := range usage {
		t.Run("usage/"+args[0]+"/"+args[len(args)-1], func(t *testing.T) {
			var output bytes.Buffer
			app := &App{Stdout: &output, Stderr: &output}
			err := app.Run(ctx, append([]string{"--config", missing}, args...))
			require.NoError(t, err)
			require.Contains(t, output.String(), "Usage:")
		})
	}

	runs := [][]string{
		{"search", "outage", "help"},
		{"search", "--", "help"},
		{"files", "--filename", "help"},
		{"sql", "select", "help"},
		{"purge", "--older-than", "help"},
		{"messages", "--channel", "help"},
	}
	for _, args := range runs {
		t.Run("run/"+args[0]+"/"+args[len(args)-1], func(t *testing.T) {
			var output bytes.Buffer
			app := &App{Stdout: &output, Stderr: &output}
			err := app.Run(ctx, append([]string{"--config", missing}, args...))
			require.Error(t, err)
			require.NotContains(t, output.String(), "Usage:")
		})
	}
}
