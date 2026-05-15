package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/vincentkoc/slacrawl/internal/report"
)

func (a *App) runAnalytics(ctx context.Context, configPath string, args []string, format OutputFormat) error {
	if len(args) == 0 || analyticsHelpRequested(args) {
		a.printAnalyticsUsage()
		return nil
	}

	subcommand := args[0]
	subArgs := args[1:]
	switch subcommand {
	case "digest":
		return a.runDigest(ctx, configPath, subArgs, format)
	case "quiet":
		return a.runAnalyticsQuiet(ctx, configPath, subArgs, format)
	case "trends":
		return a.runAnalyticsTrends(ctx, configPath, subArgs, format)
	default:
		return fmt.Errorf("unknown analytics subcommand: %s; known: digest, quiet, trends", subcommand)
	}
}

func analyticsHelpRequested(args []string) bool {
	return len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h")
}

func (a *App) printAnalyticsUsage() {
	_, _ = fmt.Fprintln(a.Stdout, "Usage: slacrawl analytics <subcommand> [flags]")
	_, _ = fmt.Fprintln(a.Stdout, "")
	_, _ = fmt.Fprintln(a.Stdout, "Subcommands:")
	_, _ = fmt.Fprintln(a.Stdout, "  digest  Per-channel activity summary for a window.")
	_, _ = fmt.Fprintln(a.Stdout, "  quiet   Channels with no activity in the lookback window.")
	_, _ = fmt.Fprintln(a.Stdout, "  trends  Week-over-week message counts per channel.")
}

func (a *App) runAnalyticsQuiet(ctx context.Context, configPath string, args []string, defaultFormat OutputFormat) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("analytics quiet", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	since := fs.String("since", "30d", "lookback window, e.g. 7d, 30d")
	workspaceID := fs.String("workspace", "", "workspace id")
	formatFlag := fs.String("format", string(defaultFormat), "output format: text|json|log")
	jsonOut := fs.Bool("json", false, "json output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := rejectUnexpectedArgs("analytics quiet", fs.Args()); err != nil {
		return err
	}
	lookback, err := parseLookback(*since)
	if err != nil {
		return fmt.Errorf("parse --since: %w", err)
	}
	outputFormat, err := resolveOutputFormat(*formatFlag, *jsonOut)
	if err != nil {
		return err
	}

	st, err := a.openReadableStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	quiet, err := report.BuildQuiet(ctx, st, report.QuietOptions{
		Now:         a.nowUTC(),
		Since:       lookback,
		WorkspaceID: coalesce(*workspaceID, cfg.WorkspaceID),
	})
	if err != nil {
		return err
	}
	return a.writeOutput("Analytics Quiet", quiet, outputFormat, true)
}

func (a *App) runAnalyticsTrends(ctx context.Context, configPath string, args []string, defaultFormat OutputFormat) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("analytics trends", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	weeks := fs.Int("weeks", 8, "number of weeks")
	workspaceID := fs.String("workspace", "", "workspace id")
	channel := fs.String("channel", "", "channel id or name")
	formatFlag := fs.String("format", string(defaultFormat), "output format: text|json|log")
	jsonOut := fs.Bool("json", false, "json output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := rejectUnexpectedArgs("analytics trends", fs.Args()); err != nil {
		return err
	}
	if *weeks < 0 {
		return errors.New("--weeks must be zero or greater")
	}
	outputFormat, err := resolveOutputFormat(*formatFlag, *jsonOut)
	if err != nil {
		return err
	}

	st, err := a.openReadableStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	trends, err := report.BuildTrends(ctx, st, report.TrendsOptions{
		Now:         a.nowUTC(),
		Weeks:       *weeks,
		WorkspaceID: coalesce(*workspaceID, cfg.WorkspaceID),
		Channel:     *channel,
	})
	if err != nil {
		return err
	}
	return a.writeOutput("Analytics Trends", trends, outputFormat, true)
}

func rejectUnexpectedArgs(command string, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return fmt.Errorf("%s does not accept positional arguments: %q", command, args)
}
