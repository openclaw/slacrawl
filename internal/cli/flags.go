package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/alecthomas/kong"
)

type slacrawlRootArgs struct {
	Config  string   `help:"Config path."`
	Format  string   `default:"text" help:"Output format: text, json, or log."`
	JSON    bool     `name:"json" help:"Compatibility alias for --format json."`
	NoColor bool     `name:"no-color" help:"Disable ANSI color in text output."`
	Version bool     `name:"version" help:"Print version."`
	Args    []string `arg:"" optional:"" passthrough:"partial" name:"command" help:"Command and arguments."`
}

func rootHelpRequested(args []string, valueFlags ...string) bool {
	valueFlagSet := make(map[string]struct{}, len(valueFlags))
	for _, flag := range valueFlags {
		valueFlagSet[flag] = struct{}{}
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--help" || arg == "-h" || (arg == "help" && i == len(args)-1) {
			return true
		}
		if !strings.HasPrefix(arg, "-") {
			return false
		}
		if name, ok := strings.CutPrefix(arg, "--"); ok {
			if strings.Contains(name, "=") {
				continue
			}
			if _, ok := valueFlagSet[name]; ok {
				i++
			}
		}
	}
	return false
}

func parseKongArgs(target any, args []string, name string, stdout, stderr io.Writer, options ...kong.Option) error {
	opts := []kong.Option{
		kong.Name(name),
		kong.NoDefaultHelp(),
		kong.Writers(stdout, stderr),
		kong.Exit(func(int) {}),
	}
	opts = append(opts, options...)
	parser, err := kong.New(target, opts...)
	if err != nil {
		return err
	}
	_, err = parser.Parse(args)
	return err
}

func csv(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func mergeStringSlices(values ...[]string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, list := range values {
		for _, value := range list {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			key := strings.ToLower(strings.TrimPrefix(value, "#"))
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, value)
		}
	}
	return out
}

// normalizeSinceTimestamp converts the documented "slack ts or RFC3339" flag
// contract into a slack ts for every backend. Only the MCP path used to
// normalize RFC3339, so the API and desktop paths silently passed the raw
// string through to Slack's `oldest` parameter.
func normalizeSinceTimestamp(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if timestamp, err := strconv.ParseFloat(value, 64); err == nil && !math.IsNaN(timestamp) && !math.IsInf(timestamp, 0) {
		return value, nil
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return fmt.Sprintf("%d.%06d", parsed.Unix(), parsed.Nanosecond()/1000), nil
	}
	return "", fmt.Errorf("--since must be a slack timestamp or an RFC3339 time, got %q", value)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	wasSet := false
	fs.Visit(func(candidate *flag.Flag) {
		if candidate.Name == name {
			wasSet = true
		}
	})
	return wasSet
}

func boolPtr(value bool) *bool {
	return &value
}

func isValidChannelKind(kind string) bool {
	switch kind {
	case "im", "mpim", "public_channel", "private_channel":
		return true
	default:
		return false
	}
}

func normalizeChannelKind(kind string) string {
	switch strings.TrimSpace(kind) {
	case "public":
		return "public_channel"
	case "private":
		return "private_channel"
	default:
		return strings.TrimSpace(kind)
	}
}

func reorderFlagsBeforePositionals(args []string, valueFlags map[string]bool) ([]string, error) {
	// Keep the original leading options, including --help and -h, so FlagSet
	// still decides help before it consumes a later value. Only value flags
	// that follow a positional move forward.
	leading := make([]string, 0, len(args))
	hoisted := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	seenPositional := false
	sawHelp := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i:]...)
			break
		}
		isFlag := arg != "-" && strings.HasPrefix(arg, "-")
		if !seenPositional && isFlag {
			leading = append(leading, arg)
			switch flagName(arg) {
			case "h", "help":
				sawHelp = true
			}
			if valueFlags[flagName(arg)] && !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
				leading = append(leading, args[i])
			}
			continue
		}
		if !isFlag {
			seenPositional = true
			positionals = append(positionals, arg)
			continue
		}
		name := flagName(arg)
		if valueFlags[name] {
			if strings.Contains(arg, "=") {
				hoisted = append(hoisted, arg)
				continue
			}
			if i+1 >= len(args) {
				if sawHelp {
					positionals = append(positionals, arg)
					continue
				}
				return nil, fmt.Errorf("flag needs an argument: -%s", name)
			}
			i++
			hoisted = append(hoisted, arg, args[i])
			continue
		}
		positionals = append(positionals, arg)
	}
	return append(append(leading, hoisted...), positionals...), nil
}

func flagName(arg string) string {
	name := strings.TrimLeft(arg, "-")
	if key, _, ok := strings.Cut(name, "="); ok {
		return key
	}
	return name
}

func hasHelpArg(args []string) bool {
	if len(args) == 1 && args[0] == "help" {
		return true
	}
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func normalizeCommandHelp(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func (a *App) parseCommandFlags(fs *flag.FlagSet, args []string) error {
	var output bytes.Buffer
	fs.SetOutput(&output)
	err := fs.Parse(args)
	if output.Len() == 0 {
		return err
	}

	target := a.Stderr
	if errors.Is(err, flag.ErrHelp) {
		target = a.Stdout
	}
	if target == nil {
		target = io.Discard
	}
	if _, writeErr := io.Copy(target, &output); writeErr != nil {
		return writeErr
	}
	return err
}

func normalizeSingleDashLongFlags(args []string, names ...string) []string {
	allowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		allowed[name] = struct{}{}
	}
	out := make([]string, len(args))
	for i, arg := range args {
		if strings.HasPrefix(arg, "--") || !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "-=") {
			out[i] = arg
			continue
		}
		name := strings.TrimPrefix(arg, "-")
		if before, _, ok := strings.Cut(name, "="); ok {
			name = before
		}
		if _, ok := allowed[name]; ok {
			out[i] = "-" + arg
			continue
		}
		out[i] = arg
	}
	return out
}
