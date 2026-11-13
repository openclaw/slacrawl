package cli

import (
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
)

const banner = `
      ▄▄                                 ▄▄
      ██                                 ██
▄█▀▀▀ ██  ▀▀█▄ ▄████ ████▄  ▀▀█▄ ██   ██ ██
▀███▄ ██ ▄█▀██ ██    ██ ▀▀ ▄█▀██ ██ █ ██ ██
▄▄▄█▀ ██ ▀█▄██ ▀████ ██    ▀█▄██  ██▀██  ██
`

var ansiEnabled = true

const (
	ansiReset  = "\033[0m"
	ansiDim    = "\033[2m"
	ansiBold   = "\033[1m"
	ansiCyan   = "\033[36m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiRed    = "\033[31m"
	ansiBlue   = "\033[34m"
)

func (a *App) writeHuman(title string, value any, withBanner bool) error {
	var b strings.Builder
	if withBanner {
		writeBanner(&b, title)
	}
	renderBlock(&b, title, value)
	if !strings.HasSuffix(b.String(), "\n") {
		b.WriteByte('\n')
	}
	_, err := io.WriteString(a.Stdout, b.String())
	return err
}

func (a *App) writeOutput(title string, value any, format OutputFormat, withBanner bool) error {
	switch format {
	case FormatJSON:
		return a.writeJSON(value)
	case FormatLog:
		return a.writeLog(title, value)
	case FormatText:
		return a.writeHuman(title, value, withBanner)
	default:
		return fmt.Errorf("unsupported output format: %s", format)
	}
}

func (a *App) printHelp() {
	var b strings.Builder
	writeBanner(&b, "CLI")
	b.WriteString(colorize(ansiDim, "Usage of slacrawl:"))
	b.WriteByte('\n')
	b.WriteString(colorize(ansiCyan, "Usage:"))
	b.WriteByte('\n')
	b.WriteString("  slacrawl [global flags] <command> [args]\n\n")
	b.WriteString(colorize(ansiCyan, "Global flags:"))
	b.WriteByte('\n')
	b.WriteString("  --config <path>   Override config path.\n")
	b.WriteString("  --format <kind>   Output format: text, json, log.\n")
	b.WriteString("  --json            Compatibility alias for --format json.\n")
	b.WriteString("  --no-color        Disable ANSI color in text output.\n\n")
	b.WriteString(colorize(ansiCyan, "Commands:"))
	b.WriteByte('\n')
	b.WriteString("  metadata   Print crawlkit control metadata.\n")
	b.WriteString("  check-update Check for a newer slacrawl release.\n")
	b.WriteString("  init       Create a starter config.\n")
	b.WriteString("  doctor     Check config, DB, tokens, and desktop coverage.\n")
	b.WriteString("  report     Show archive activity and share freshness.\n")
	b.WriteString("  digest     Per-channel activity summary for a window.\n")
	b.WriteString("  analytics  Grouped analytics subcommands.\n")
	b.WriteString("  publish    Export a git-backed archive snapshot.\n")
	b.WriteString("  subscribe  Configure a git-backed archive reader.\n")
	b.WriteString("  update     Pull and import the latest git snapshot.\n")
	b.WriteString("  sync       Run a one-shot crawl from bot/api, wiretap/desktop, or both.\n")
	b.WriteString("  import     Import a Slack export ZIP or directory.\n")
	b.WriteString("  export     Prepare, build, or verify an offline projection.\n")
	b.WriteString("  delivery   Initialize, stage, or deliver an explicit private seed.\n")
	b.WriteString("  purge      Preview or delete messages older than a cutoff.\n")
	b.WriteString("  tail       Listen for live events through Socket Mode.\n")
	b.WriteString("  watch      Refresh desktop-local state on an interval.\n")
	b.WriteString("  status     Show workspace and sync coverage.\n")
	b.WriteString("  search     Run a local FTS query.\n")
	b.WriteString("  tui        Browse archived messages in the terminal.\n")
	b.WriteString("  messages   List stored messages.\n")
	b.WriteString("  files      List file metadata and fetch cached media.\n")
	b.WriteString("  mentions   List extracted mentions.\n")
	b.WriteString("  users      List synced users.\n")
	b.WriteString("  channels   List synced channels.\n")
	b.WriteString("  completion Output shell completion for bash or zsh.\n")
	b.WriteString("  sql        Run a read-only SQL query.\n\n")
	b.WriteString(colorize(ansiCyan, "Examples:"))
	b.WriteByte('\n')
	b.WriteString("  slacrawl init\n")
	b.WriteString("  slacrawl metadata --json\n")
	b.WriteString("  slacrawl doctor\n")
	b.WriteString("  slacrawl report\n")
	b.WriteString("  slacrawl digest --since 7d\n")
	b.WriteString("  slacrawl analytics trends --weeks 4\n")
	b.WriteString("  slacrawl subscribe --db ~/.slacrawl/slacrawl.db https://example.com/private/slacrawl-archive.git\n")
	b.WriteString("  slacrawl sync --source bot --latest-only\n")
	b.WriteString("  slacrawl import ./my-export.zip --workspace T01234567\n")
	b.WriteString("  slacrawl purge --older-than 90d\n")
	b.WriteString("  slacrawl search incident\n")
	b.WriteString("  slacrawl tui\n")
	b.WriteString("  slacrawl files fetch --missing\n")
	b.WriteString("  slacrawl completion bash > /usr/local/etc/bash_completion.d/slacrawl\n")
	b.WriteString("  slacrawl sql 'select count(*) from messages;'\n")
	_, _ = io.WriteString(a.Stdout, b.String())
}

func (a *App) writeLog(title string, value any) error {
	var b strings.Builder
	prefix := strings.ToLower(strings.TrimSpace(title))
	if prefix == "" {
		prefix = "slacrawl"
	}
	renderLogValue(&b, prefix, normalizeValue(value))
	if b.Len() == 0 {
		b.WriteString(prefix)
		b.WriteString(" empty=true\n")
	}
	_, err := io.WriteString(a.Stdout, b.String())
	return err
}

func writeBanner(w *strings.Builder, title string) {
	w.WriteString(colorize(ansiBlue, strings.TrimPrefix(banner, "\n")))
	w.WriteString(colorize(ansiDim, "local-first slack mirror for SQLite"))
	if title != "" {
		w.WriteString(colorize(ansiDim, "  |  "))
		w.WriteString(colorize(ansiCyan, strings.ToLower(title)))
	}
	w.WriteString("\n\n")
}

func renderBlock(w *strings.Builder, title string, value any) {
	normalized := normalizeValue(value)
	if renderSpecialBlock(w, title, normalized) {
		return
	}
	if title != "" {
		w.WriteString(colorize(ansiBold+ansiCyan, strings.ToUpper(title)))
		w.WriteByte('\n')
		w.WriteString(colorize(ansiDim, strings.Repeat("=", len(title))))
		w.WriteString("\n\n")
	}

	if !renderValue(w, normalized, 0) {
		w.WriteString("(empty)\n")
	}
}

func renderSpecialBlock(w *strings.Builder, title string, value any) bool {
	switch title {
	case "Doctor":
		return renderDoctorBlock(w, value)
	case "Status":
		return renderStatusBlock(w, value)
	case "Report":
		return renderReportBlock(w, value)
	case "Digest":
		return renderDigestBlock(w, value)
	case "Analytics Quiet":
		return renderAnalyticsQuietBlock(w, value)
	case "Analytics Trends":
		return renderAnalyticsTrendsBlock(w, value)
	case "Sync", "Watch":
		return renderSyncBlock(w, title, value)
	case "Search":
		return renderMessageListBlock(w, title, value, true)
	case "Messages":
		return renderMessageListBlock(w, title, value, false)
	default:
		return false
	}
}

func renderValue(w *strings.Builder, value any, depth int) bool {
	switch typed := value.(type) {
	case nil:
		w.WriteString(indent(depth))
		w.WriteString("(empty)\n")
		return true
	case map[string]any:
		return renderMap(w, typed, depth)
	case []any:
		return renderSlice(w, typed, depth)
	default:
		w.WriteString(indent(depth))
		w.WriteString(formatScalar(typed))
		w.WriteByte('\n')
		return true
	}
}

func renderMap(w *strings.Builder, value map[string]any, depth int) bool {
	if len(value) == 0 {
		w.WriteString(indent(depth))
		w.WriteString("(empty)\n")
		return true
	}

	keys := orderedKeys(value)
	scalars := make([]string, 0, len(keys))
	nested := make([]string, 0, len(keys))
	maxLabel := 0
	for _, key := range keys {
		if isScalar(value[key]) {
			scalars = append(scalars, key)
			if l := len(humanize(key)); l > maxLabel {
				maxLabel = l
			}
			continue
		}
		nested = append(nested, key)
	}

	wrote := false
	for _, key := range scalars {
		label := humanize(key)
		w.WriteString(indent(depth))
		w.WriteString(colorize(ansiDim, label))
		w.WriteString(strings.Repeat(" ", maxLabel-len(label)))
		w.WriteString(colorize(ansiDim, " : "))
		w.WriteString(formatScalar(value[key]))
		w.WriteByte('\n')
		wrote = true
	}
	for _, key := range nested {
		if wrote {
			w.WriteByte('\n')
		}
		w.WriteString(indent(depth))
		w.WriteString(colorize(ansiCyan, humanize(key)))
		w.WriteByte('\n')
		w.WriteString(indent(depth))
		w.WriteString(colorize(ansiDim, strings.Repeat("-", len(humanize(key)))))
		w.WriteByte('\n')
		renderValue(w, value[key], depth+1)
		wrote = true
	}
	return wrote
}

func renderSlice(w *strings.Builder, value []any, depth int) bool {
	if len(value) == 0 {
		w.WriteString(indent(depth))
		w.WriteString("no rows\n")
		return true
	}
	if allScalar(value) {
		for _, item := range value {
			w.WriteString(indent(depth))
			w.WriteString("- ")
			w.WriteString(formatScalar(item))
			w.WriteByte('\n')
		}
		return true
	}

	rows := make([]map[string]any, 0, len(value))
	for _, item := range value {
		row, ok := item.(map[string]any)
		if !ok {
			w.WriteString(indent(depth))
			w.WriteString(formatScalar(item))
			w.WriteByte('\n')
			return true
		}
		rows = append(rows, row)
	}
	renderTable(w, rows, depth)
	return true
}

func renderTable(w *strings.Builder, rows []map[string]any, depth int) {
	columns := orderedColumns(rows)
	widths := make(map[string]int, len(columns))
	for _, col := range columns {
		widths[col] = displayWidth(humanize(col))
	}
	for _, row := range rows {
		for _, col := range columns {
			// Measure the uncolored text: formatScalar wraps empty, nil and
			// bool cells in ANSI codes, whose bytes are invisible but would
			// otherwise pad the column by roughly eight characters.
			if n := displayWidth(plainScalar(row[col])); n > widths[col] {
				widths[col] = n
			}
		}
	}

	prefix := indent(depth)
	for i, col := range columns {
		if i > 0 {
			w.WriteString("  ")
		}
		header := humanize(col)
		w.WriteString(prefixIfFirst(prefix, i))
		w.WriteString(colorize(ansiDim, header))
		w.WriteString(strings.Repeat(" ", widths[col]-displayWidth(header)))
	}
	w.WriteByte('\n')
	for i, col := range columns {
		if i > 0 {
			w.WriteString("  ")
		}
		w.WriteString(prefixIfFirst(prefix, i))
		w.WriteString(colorize(ansiDim, strings.Repeat("-", widths[col])))
	}
	w.WriteByte('\n')
	for _, row := range rows {
		for i, col := range columns {
			if i > 0 {
				w.WriteString("  ")
			}
			w.WriteString(prefixIfFirst(prefix, i))
			w.WriteString(formatScalar(row[col]))
			w.WriteString(strings.Repeat(" ", widths[col]-displayWidth(plainScalar(row[col]))))
		}
		w.WriteByte('\n')
	}
}

func prefixIfFirst(prefix string, column int) string {
	if column == 0 {
		return prefix
	}
	return ""
}

func orderedColumns(rows []map[string]any) []string {
	seen := map[string]struct{}{}
	var cols []string
	for _, row := range rows {
		for _, key := range orderedKeys(row) {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			cols = append(cols, key)
		}
	}
	return cols
}

func orderedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keyRank(keys[i]) < keyRank(keys[j]) || keyRank(keys[i]) == keyRank(keys[j]) && keys[i] < keys[j]
	})
	return keys
}

func keyRank(key string) int {
	switch key {
	case "config_path":
		return 0
	case "database_path", "db_path":
		return 1
	case "status":
		return 2
	case "summary":
		return 3
	case "tokens":
		return 4
	case "slack_api":
		return 5
	case "desktop_source", "desktop":
		return 6
	case "archive_profile":
		return 7
	case "fts_available":
		return 8
	case "api_channel_skips":
		return 9
	case "tail_state":
		return 10
	default:
		return 100
	}
}

func humanize(value string) string {
	value = strings.ReplaceAll(value, "_", " ")
	value = strings.ReplaceAll(value, "-", " ")
	return value
}

func indent(depth int) string {
	return strings.Repeat("  ", depth)
}

func allScalar(values []any) bool {
	for _, value := range values {
		if !isScalar(value) {
			return false
		}
	}
	return true
}

func isScalar(value any) bool {
	switch value.(type) {
	case nil, string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, time.Time:
		return true
	default:
		return false
	}
}

func formatScalar(value any) string {
	plain := plainScalar(value)
	switch typed := value.(type) {
	case nil:
		return colorize(ansiDim, plain)
	case bool:
		if typed {
			return colorize(ansiGreen, plain)
		}
		return colorize(ansiYellow, plain)
	case time.Time:
		if typed.IsZero() {
			return colorize(ansiDim, plain)
		}
		return plain
	case string:
		if typed == "" {
			return colorize(ansiDim, plain)
		}
		return plain
	default:
		return plain
	}
}

func plainScalar(value any) string {
	switch typed := value.(type) {
	case nil:
		return "-"
	case bool:
		if typed {
			return "yes"
		}
		return "no"
	case time.Time:
		if typed.IsZero() {
			return "never"
		}
		return typed.Format(time.RFC3339)
	case string:
		if typed == "" {
			return "-"
		}
		return typed
	default:
		return fmt.Sprint(typed)
	}
}

func normalizeValue(value any) any {
	if value == nil {
		return nil
	}
	if scalar := normalizeScalar(value); scalar != nil {
		return scalar
	}

	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if scalar := normalizeScalar(rv.Interface()); scalar != nil {
		return scalar
	}

	switch rv.Kind() {
	case reflect.Struct:
		out := make(map[string]any)
		rt := rv.Type()
		for i := 0; i < rv.NumField(); i++ {
			field := rt.Field(i)
			if !field.IsExported() {
				continue
			}
			if field.Anonymous {
				flattened := normalizeValue(rv.Field(i).Interface())
				if child, ok := flattened.(map[string]any); ok {
					for key, value := range child {
						out[key] = value
					}
					continue
				}
			}
			key := field.Tag.Get("json")
			if key == "" {
				key = field.Name
			}
			key = strings.Split(key, ",")[0]
			if key == "" || key == "-" {
				continue
			}
			out[key] = normalizeValue(rv.Field(i).Interface())
		}
		return out
	case reflect.Map:
		out := make(map[string]any)
		iter := rv.MapRange()
		for iter.Next() {
			out[fmt.Sprint(iter.Key().Interface())] = normalizeValue(iter.Value().Interface())
		}
		return out
	case reflect.Slice, reflect.Array:
		out := make([]any, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out = append(out, normalizeValue(rv.Index(i).Interface()))
		}
		return out
	default:
		return fmt.Sprint(value)
	}
}

func normalizeScalar(value any) any {
	switch typed := value.(type) {
	case string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, time.Time:
		return typed
	default:
		return nil
	}
}

func renderLogValue(w *strings.Builder, prefix string, value any) {
	switch typed := value.(type) {
	case nil:
		w.WriteString(prefix)
		w.WriteString(" value=-\n")
	case map[string]any:
		renderLogMap(w, prefix, typed)
	case []any:
		renderLogSlice(w, prefix, typed)
	default:
		w.WriteString(prefix)
		w.WriteString(" value=")
		w.WriteString(strconv.Quote(plainScalar(typed)))
		w.WriteByte('\n')
	}
}

func renderLogMap(w *strings.Builder, prefix string, value map[string]any) {
	if len(value) == 0 {
		w.WriteString(prefix)
		w.WriteString(" empty=true\n")
		return
	}

	keys := orderedKeys(value)
	scalars := make([]string, 0, len(keys))
	nested := make([]string, 0, len(keys))
	for _, key := range keys {
		if isScalar(value[key]) {
			scalars = append(scalars, key)
		} else {
			nested = append(nested, key)
		}
	}

	if len(scalars) > 0 {
		w.WriteString(prefix)
		for _, key := range scalars {
			w.WriteByte(' ')
			w.WriteString(key)
			w.WriteByte('=')
			w.WriteString(strconv.Quote(plainScalar(value[key])))
		}
		w.WriteByte('\n')
	}

	for _, key := range nested {
		renderLogValue(w, prefix+"."+key, value[key])
	}
}

func renderLogSlice(w *strings.Builder, prefix string, value []any) {
	if len(value) == 0 {
		w.WriteString(prefix)
		w.WriteString(" count=0\n")
		return
	}

	for _, item := range value {
		switch typed := item.(type) {
		case map[string]any:
			renderLogMap(w, prefix, typed)
		case []any:
			renderLogSlice(w, prefix, typed)
		default:
			w.WriteString(prefix)
			w.WriteString(" value=")
			w.WriteString(strconv.Quote(plainScalar(typed)))
			w.WriteByte('\n')
		}
	}
}

type metric struct {
	label string
	value string
	color string
}

func writeTitle(w *strings.Builder, title string) {
	w.WriteString(colorize(ansiBold+ansiCyan, title))
	w.WriteByte('\n')
	w.WriteString(colorize(ansiDim, strings.Repeat("=", len(title))))
	w.WriteString("\n\n")
}

func writeCheck(w *strings.Builder, label string, ok bool, detail string) {
	color := ansiYellow
	icon := "▲"
	if ok {
		color = ansiGreen
		icon = "●"
	}
	if detail == "" {
		detail = "-"
	}
	w.WriteString("  ")
	w.WriteString(colorize(color, icon))
	w.WriteString(" ")
	w.WriteString(colorize(ansiDim, padRight(label, 16)))
	w.WriteString(detail)
	w.WriteByte('\n')
}

func writeMetricRow(w *strings.Builder, metrics []metric) {
	w.WriteString("  ")
	for i, item := range metrics {
		if i > 0 {
			w.WriteString(colorize(ansiDim, "  |  "))
		}
		w.WriteString(colorize(ansiDim, item.label+"="))
		w.WriteString(colorize(item.color, item.value))
	}
	w.WriteByte('\n')
}

func shortValue(value any) string {
	return stripANSI(formatScalar(value))
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return typed != "" && typed != "-" && typed != "false" && typed != "0"
	default:
		return false
	}
}

func teamLabel(value map[string]any) string {
	team := shortValue(value["bot_auth_team"])
	teamID := shortValue(value["bot_auth_team_id"])
	if team != "-" && teamID != "-" {
		return team + " (" + teamID + ")"
	}
	if team != "-" {
		return team
	}
	if teamID != "-" {
		return teamID
	}
	return "bot token missing"
}

func ternary(ok bool, a string, b string) string {
	if ok {
		return a
	}
	return b
}

// trimTo clips value to limit display columns. It counts width rather than
// bytes so a message containing emoji or CJK is neither cut mid-rune into
// invalid UTF-8 nor truncated far earlier than intended.
func trimTo(value string, limit int) string {
	if limit <= 0 || displayWidth(value) <= limit {
		return value
	}
	if limit <= 3 {
		return truncateToWidth(value, limit)
	}
	return truncateToWidth(value, limit-3) + "..."
}

func truncateToWidth(value string, width int) string {
	if width <= 0 {
		return ""
	}
	used := 0
	for i, r := range value {
		w := runewidth.RuneWidth(r)
		if used+w > width {
			return value[:i]
		}
		used += w
	}
	return value
}

func displayWidth(value string) int {
	return runewidth.StringWidth(value)
}

func padRight(value string, width int) string {
	pad := width - displayWidth(value)
	if pad <= 0 {
		return value
	}
	return value + strings.Repeat(" ", pad)
}

func colorize(code string, value string) string {
	if value == "" {
		return ""
	}
	if !ansiEnabled {
		return value
	}
	return code + value + ansiReset
}

func stripANSI(value string) string {
	replacer := strings.NewReplacer(
		ansiReset, "",
		ansiDim, "",
		ansiBold, "",
		ansiCyan, "",
		ansiGreen, "",
		ansiYellow, "",
		ansiRed, "",
		ansiBlue, "",
	)
	return replacer.Replace(value)
}
