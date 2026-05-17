package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/openclaw/crawlkit/releasecheck"
	"github.com/openclaw/slacrawl/internal/config"
)

const slacrawlUpgradeHint = "brew upgrade openclaw/tap/slacrawl"

func slacrawlReleaseCheckOptions(force bool) releasecheck.Options {
	cfg := config.Default()
	return releasecheck.Options{
		AppName:        "slacrawl",
		Owner:          "openclaw",
		Repo:           "slacrawl",
		CurrentVersion: version,
		CacheDir:       cfg.CacheDir,
		Force:          force,
	}
}

func (a *App) maybeNotifyRelease(ctx context.Context, args []string) {
	_, _ = releasecheck.Notify(ctx, releasecheck.NotifyOptions{
		Options:     slacrawlReleaseCheckOptions(false),
		Stderr:      a.Stderr,
		InstallHint: slacrawlUpgradeHint,
		Args:        args,
		JSONOutput:  a.outputFormat == FormatJSON,
		IsTerminal:  releasecheck.StderrIsTerminal(),
	})
}

func (a *App) runCheckUpdate(ctx context.Context, args []string, format OutputFormat) error {
	fs := flag.NewFlagSet("check-update", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	jsonOut := fs.Bool("json", false, "write JSON output")
	force := fs.Bool("force", false, "force a fresh release check")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("check-update takes flags only")
	}
	if *jsonOut {
		format = FormatJSON
	}
	result, err := releasecheck.Check(ctx, slacrawlReleaseCheckOptions(*force))
	if err != nil && !errors.Is(err, releasecheck.ErrSkipped) {
		return err
	}
	if format == FormatJSON {
		return a.writeJSON(result)
	}
	_, err = fmt.Fprint(a.Stdout, releasecheck.StatusText("slacrawl", slacrawlUpgradeHint, result))
	return err
}
