package cli

import (
	"context"
	"errors"
	"flag"
	"strings"
	"time"

	"github.com/openclaw/crawlkit/remote"
	"github.com/openclaw/slacrawl/internal/config"
)

// Cloud health describes the archive reader. Local Slack credentials, desktop
// discovery and SQLite checks cannot establish the remote archive's health.
func (a *App) runRemoteHealth(ctx context.Context, cfg config.Config, command string, args []string, format OutputFormat) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "write remote archive status JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(command + " takes flags only")
	}
	if *jsonOut {
		format = FormatJSON
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, err := remote.NewClientFromConfig(cfg.Remote, remote.Options{})
	if err != nil {
		return err
	}
	status, err := client.Status(ctx, "slacrawl", cfg.Remote.Archive)
	if err != nil {
		return err
	}
	if status.App != "slacrawl" || status.Archive != cfg.Remote.Archive || status.Mode != "cloud" {
		return errors.New("remote archive status identity differs")
	}
	return a.writeOutput("Remote "+strings.ToUpper(command[:1])+command[1:], status, format, false)
}
