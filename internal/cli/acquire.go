package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/openclaw/slacrawl/internal/acquisition"
	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/slackapi"
)

func (a *App) runAcquire(ctx context.Context, args []string, configPath *string, format OutputFormat) error {
	fs := flag.NewFlagSet("acquire", flag.ContinueOnError)
	origin := fs.String("origin", "", "PostgreSQL acquisition API origin")
	archive := fs.String("archive", "", "registered source ID")
	workspace := fs.String("workspace", "", "admitted Slack workspace ID")
	channel := fs.String("channel", "", "admitted Slack channel ID")
	source := fs.String("source", "", "api-bot or api-user (no fallback)")
	sinceText := fs.String("since", "", "first window UTC timestamp; otherwise server checkpoint")
	untilText := fs.String("until", "", "UTC horizon; active runs retain their original horizon")
	runID := fs.String("run-id", "", "optional new run identity; active run resumes automatically")
	tokenEnv := fs.String("token-env", "CRAWL_REMOTE_TOKEN", "environment variable holding publisher token")
	maxPages := fs.Int("max-pages", 0, "bounded provider page budget, 1..1000")
	timeout := fs.Duration("timeout", 0, "positive total operation deadline")
	if err := a.parseCommandFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *timeout <= 0 || *maxPages < 1 || *maxPages > 1000 {
		return errors.New("acquire requires --max-pages 1..1000 and positive --timeout")
	}
	parseTime := func(s string) (time.Time, error) {
		t, e := time.Parse(time.RFC3339Nano, s)
		if e != nil || t.Location() != time.UTC || t.Nanosecond()%1000 != 0 {
			return time.Time{}, errors.New("acquire timestamps require UTC microsecond precision")
		}
		return t, nil
	}
	var since, until *time.Time
	if *sinceText != "" {
		t, err := parseTime(*sinceText)
		if err != nil {
			return err
		}
		since = &t
	}
	if *untilText != "" {
		t, err := parseTime(*untilText)
		if err != nil {
			return err
		}
		until = &t
	}
	api, err := acquisition.NewClient(*origin, *archive, os.Getenv(*tokenEnv))
	if err != nil {
		return err
	}
	cfg := config.Default()
	if configPath != nil {
		cfg, err = config.Load(*configPath)
		if err != nil {
			return errors.New("cannot read explicit acquisition credential config")
		}
	}
	provider := slackapi.NewWithOptions(cfg.ResolveTokensForWorkspace(*workspace), a.apiURL, a.httpClient)
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	result, err := acquisition.Collect(ctx, api, provider, acquisition.Options{Workspace: *workspace, Channel: *channel, SourceName: *source, RunID: *runID, Since: since, Until: until, MaxPages: *maxPages})
	if outputErr := a.writeOutput("Acquisition", result, format, false); outputErr != nil {
		return outputErr
	}
	if err != nil {
		return fmt.Errorf("acquire stopped: %w", err)
	}
	return nil
}
