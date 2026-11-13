package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/openclaw/slacrawl/internal/config"
)

type App struct {
	Stdout io.Writer
	Stderr io.Writer

	configPath    string
	outputFormat  OutputFormat
	now           func() time.Time
	httpClient    *http.Client
	apiURL        string
	readBuildInfo func() (*debug.BuildInfo, bool)
}

type OutputFormat string

const (
	FormatText OutputFormat = "text"
	FormatJSON OutputFormat = "json"
	FormatLog  OutputFormat = "log"
)

var version = "dev"

func New() *App {
	return &App{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}

func (a *App) nowUTC() time.Time {
	if a.now != nil {
		return a.now().UTC()
	}
	return time.Now().UTC()
}

func (a *App) Run(ctx context.Context, args []string) error {
	if len(args) == 0 || rootHelpRequested(args, "config", "format") {
		a.setColorEnabled(FormatText, false)
		a.printHelp()
		return nil
	}
	var global slacrawlRootArgs
	if err := parseKongArgs(&global, args, "slacrawl", a.Stdout, a.Stderr); err != nil {
		return err
	}
	rest := global.Args
	if global.Version {
		_, err := fmt.Fprintln(a.Stdout, version)
		return err
	}
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "--help" || rest[0] == "-h" {
		a.setColorEnabled(FormatText, global.NoColor)
		a.printHelp()
		return nil
	}

	// Offline export must not resolve personal config or enter the notifier and
	// automatic archive paths used by the other commands.
	if rest[0] == "export" {
		format, err := resolveOutputFormat(global.Format, global.JSON)
		if err != nil {
			return err
		}
		a.setColorEnabled(format, global.NoColor)
		return normalizeCommandHelp(a.runExport(ctx, rest[1:], format))
	}

	if rest[0] == "delivery" {
		if global.Config != nil {
			return errors.New("delivery does not accept --config; supply explicit --db and --scope")
		}
		format, err := resolveOutputFormat(global.Format, global.JSON)
		if err != nil {
			return err
		}
		a.setColorEnabled(format, global.NoColor)
		return normalizeCommandHelp(a.runDelivery(ctx, rest[1:], format))
	}

	configPath := ""
	if global.Config != nil {
		configPath = *global.Config
	}
	if configPath == "" {
		path, err := config.DefaultConfigPath()
		if err != nil {
			return err
		}
		configPath = path
	}

	outputFormat, err := resolveOutputFormat(global.Format, global.JSON)
	if err != nil {
		return err
	}
	a.configPath = configPath
	a.outputFormat = outputFormat
	a.setColorEnabled(outputFormat, global.NoColor)

	a.maybeNotifyRelease(ctx, rest)

	switch rest[0] {
	case "version":
		return a.writeOutput("Version", map[string]string{"version": version}, outputFormat, false)
	case "check-update":
		return a.runCheckUpdate(ctx, rest[1:], outputFormat)
	case "metadata":
		return a.runMetadata(configPath, rest[1:], outputFormat)
	case "init":
		return normalizeCommandHelp(a.runInit(configPath, rest[1:], outputFormat))
	case "doctor":
		return a.runDoctor(ctx, configPath, rest[1:], outputFormat)
	case "report":
		return normalizeCommandHelp(a.runReport(ctx, configPath, rest[1:], outputFormat))
	case "digest":
		return normalizeCommandHelp(a.runDigest(ctx, configPath, rest[1:], outputFormat))
	case "analytics":
		return normalizeCommandHelp(a.runAnalytics(ctx, configPath, rest[1:], outputFormat))
	case "publish":
		return normalizeCommandHelp(a.runPublish(ctx, configPath, rest[1:], outputFormat))
	case "subscribe":
		return normalizeCommandHelp(a.runSubscribe(ctx, configPath, rest[1:], outputFormat))
	case "update":
		return normalizeCommandHelp(a.runUpdate(ctx, configPath, rest[1:], outputFormat))
	case "status":
		return a.runStatus(ctx, configPath, rest[1:], outputFormat)
	case "sync":
		return normalizeCommandHelp(a.runSync(ctx, configPath, rest[1:], outputFormat))
	case "import":
		return normalizeCommandHelp(a.runImport(ctx, rest[1:]))
	case "purge":
		return a.runPurge(ctx, configPath, rest[1:], outputFormat)
	case "search":
		return a.runSearch(ctx, configPath, rest[1:], outputFormat)
	case "tui":
		return a.runTUI(ctx, configPath, rest[1:], outputFormat)
	case "messages":
		return a.runMessages(ctx, configPath, rest[1:], outputFormat)
	case "files":
		return a.runFiles(ctx, configPath, rest[1:], outputFormat)
	case "mentions":
		return normalizeCommandHelp(a.runMentions(ctx, configPath, rest[1:], outputFormat))
	case "sql":
		return a.runSQL(ctx, configPath, rest[1:], outputFormat)
	case "users":
		return normalizeCommandHelp(a.runUsers(ctx, configPath, rest[1:], outputFormat))
	case "channels":
		return normalizeCommandHelp(a.runChannels(ctx, configPath, rest[1:], outputFormat))
	case "completion":
		return a.runCompletion(rest[1:])
	case "tail":
		return normalizeCommandHelp(a.runTail(ctx, configPath, rest[1:]))
	case "watch":
		return normalizeCommandHelp(a.runWatch(ctx, configPath, rest[1:], outputFormat))
	default:
		return fmt.Errorf("unknown command: %s", rest[0])
	}
}

func (a *App) setColorEnabled(format OutputFormat, noColor bool) {
	ansiEnabled = format == FormatText && !noColor && colorAllowedByEnv() && writerIsTTY(a.Stdout)
}

func colorAllowedByEnv() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	return true
}

func writerIsTTY(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

func resolveOutputFormat(value string, jsonOut bool) (OutputFormat, error) {
	if jsonOut {
		return FormatJSON, nil
	}
	switch OutputFormat(strings.ToLower(strings.TrimSpace(value))) {
	case "", FormatText:
		return FormatText, nil
	case FormatJSON:
		return FormatJSON, nil
	case FormatLog:
		return FormatLog, nil
	default:
		return "", fmt.Errorf("unsupported format %q: use text, json, or log", value)
	}
}

func (a *App) writeJSON(value any) error {
	enc := json.NewEncoder(a.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func coalesce(primary string, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}
