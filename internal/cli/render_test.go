package cli

import (
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestNormalizeValueHandlesTimePointers(t *testing.T) {
	now := time.Date(2026, time.April, 22, 3, 4, 5, 0, time.UTC)
	var nilTime *time.Time

	value := normalizeValue(struct {
		At      *time.Time `json:"at"`
		Missing *time.Time `json:"missing"`
	}{
		At:      &now,
		Missing: nilTime,
	})

	row := value.(map[string]any)
	require.Equal(t, now, row["at"])
	require.Nil(t, row["missing"])
}

func TestTrimToKeepsValidUTF8(t *testing.T) {
	// Byte slicing cut multi-byte runes in half, so search and messages output
	// could emit text that is not valid UTF-8 at all.
	trimmed := trimTo(strings.Repeat("a", 108)+"🎉🎉", 110)
	require.True(t, utf8.ValidString(trimmed), "trimTo must cut on a rune boundary")
	require.LessOrEqual(t, displayWidth(trimmed), 110)

	require.Equal(t, "short", trimTo("short", 110))
	require.True(t, utf8.ValidString(trimTo(strings.Repeat("世界", 200), 110)))
}

func TestRenderTableAlignsColorizedAndWideCells(t *testing.T) {
	previous := ansiEnabled
	ansiEnabled = true
	defer func() { ansiEnabled = previous }()

	var w strings.Builder
	// The empty "title" cell gets wrapped in ANSI codes, whose bytes are
	// invisible but used to widen the column; "Ålice" is 6 bytes across 5
	// columns. Both used to shift every following column.
	renderTable(&w, []map[string]any{
		{"name": "Ålice", "title": ""},
		{"name": "bob", "title": "eng"},
	}, 0)

	lines := strings.Split(strings.TrimRight(w.String(), "\n"), "\n")
	require.Len(t, lines, 4)

	gap := regexp.MustCompile(` {2,}`)
	offsets := make([]int, 0, len(lines))
	for _, line := range lines {
		plain := strings.TrimRight(stripANSI(line), " ")
		loc := gap.FindStringIndex(plain)
		require.NotNil(t, loc, "line %q must have two columns", plain)
		offsets = append(offsets, displayWidth(plain[:loc[1]]))
	}
	for i, offset := range offsets[1:] {
		require.Equal(t, offsets[0], offset, "line %d starts its second column at a different display offset", i+1)
	}
}
