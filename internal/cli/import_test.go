package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/vincentkoc/slacrawl/internal/store"
)

func TestImportCommandJSON(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "config.toml")
	dbPath := filepath.Join(tmp, "slacrawl.db")

	var stdout bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stdout}
	ctx := context.Background()
	require.NoError(t, app.Run(ctx, []string{"--config", configPath, "init", "--db", dbPath}))

	zipPath := writeImportFixtureZip(t, map[string]string{
		"users.json":    `[{"id":"U1","name":"alice","profile":{"display_name":"alice"}}]`,
		"channels.json": `[{"id":"C1","name":"general","is_private":false}]`,
		"general/2026-01-01.json": `[
			{"type":"message","user":"U1","text":"keep-existing","ts":"1735689600.000001"},
			{"type":"message","user":"U1","text":"new-message","ts":"1735689600.000002"},
			{"type":"message","user":"U1","text":"  indented\n","ts":"1735689600.000006"},
			{"type":"message","user":"U1","text":"","ts":"1735689600.000003","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"block only payload"}}],"attachments":[{"title":"attachment title","text":"attachment body"}]},
			{"type":"message","user":"U1","text":"","ts":"1735689600.000004","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"supported block before unknown"}},{"type":"actions","elements":[{"type":"unknown_new","text":{"type":"plain_text","text":"unknown action label"}}]}]},
			{"type":"message","user":"U1","text":"","ts":"1735689600.000005","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"section with accessory"},"accessory":{"type":"icon_button","text":{"type":"plain_text","text":"Delete response"},"accessibility_label":"Remove response","value":"delete","url":"https://hidden.example/delete","confirm":{"title":{"type":"plain_text","text":"Hidden confirm title"},"text":{"type":"mrkdwn","text":"Hidden confirm body"},"confirm":{"type":"plain_text","text":"Hidden yes"},"deny":{"type":"plain_text","text":"Hidden no"}}}}]}
		]`,
	})

	st, err := store.Open(dbPath)
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, st.UpsertWorkspace(ctx, store.Workspace{ID: "T123", Name: "seed", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "C1", WorkspaceID: "T123", Name: "general", Kind: "public_channel", RawJSON: "{}", UpdatedAt: now}))
	require.NoError(t, st.UpsertMessage(ctx, store.Message{
		ChannelID:      "C1",
		TS:             "1735689600.000001",
		WorkspaceID:    "T123",
		UserID:         "U1",
		Text:           "existing",
		NormalizedText: "existing",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      now,
	}, nil))
	require.NoError(t, st.Close())

	stdout.Reset()
	require.NoError(t, app.Run(ctx, []string{"--config", configPath, "--json", "import", zipPath, "--workspace", "T123"}))

	var report ImportReport
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	require.Equal(t, "T123", report.Workspace)
	require.Equal(t, 1, report.Users)
	require.Equal(t, 1, report.Channels)
	require.Equal(t, 5, report.Messages)
	require.Equal(t, 1, report.Skipped)
	require.False(t, report.DryRun)

	st, err = store.Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	rows, err := st.QueryReadOnly(ctx, `
select ts, source_name, source_rank, text, normalized_text
from messages
where channel_id = 'C1'
order by ts asc`)
	require.NoError(t, err)
	require.Len(t, rows, 6)
	require.Equal(t, "api-bot", rows[0]["source_name"])
	require.Equal(t, int64(2), rows[0]["source_rank"])
	require.Equal(t, "slack-export", rows[1]["source_name"])
	require.Equal(t, int64(2), rows[1]["source_rank"])
	require.Contains(t, rows[2]["normalized_text"], "block only payload")
	require.Contains(t, rows[2]["normalized_text"], "attachment title")
	require.Contains(t, rows[2]["normalized_text"], "attachment body")
	require.Contains(t, rows[3]["normalized_text"], "supported block before unknown")
	require.Contains(t, rows[3]["normalized_text"], "unknown action label")
	require.Contains(t, rows[4]["normalized_text"], "section with accessory")
	require.Contains(t, rows[4]["normalized_text"], "Delete response")
	require.Contains(t, rows[4]["normalized_text"], "Remove response")
	require.NotContains(t, rows[4]["normalized_text"], "delete")
	require.NotContains(t, rows[4]["normalized_text"], "https://hidden.example/delete")
	require.NotContains(t, rows[4]["normalized_text"], "Hidden confirm")
	require.Equal(t, "  indented\n", rows[5]["text"])
}

func TestImportRejectsSameChannelTimestampInDifferentWorkspaces(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "config.toml")
	dbPath := filepath.Join(tmp, "slacrawl.db")

	var stdout bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stdout}
	ctx := context.Background()
	require.NoError(t, app.Run(ctx, []string{"--config", configPath, "init", "--db", dbPath}))

	zipPath := writeImportFixtureZip(t, map[string]string{
		"users.json":    `[{"id":"U1","name":"alice"}]`,
		"channels.json": `[{"id":"C1","name":"general","is_private":false}]`,
		"general/2026-01-01.json": `[
			{"type":"message","user":"U1","text":"same ts different workspace","ts":"1735689600.000001"}
		]`,
	})

	stdout.Reset()
	require.NoError(t, app.Run(ctx, []string{"--config", configPath, "--json", "import", zipPath, "--workspace", "T1"}))
	var first ImportReport
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &first))
	require.Equal(t, 1, first.Messages)
	require.Equal(t, 0, first.Skipped)

	stdout.Reset()
	err := app.Run(ctx, []string{"--config", configPath, "--json", "import", zipPath, "--workspace", "T2"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists in workspace T1")

	st, err := store.Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	rows, err := st.QueryReadOnly(ctx, `
select workspace_id, count(*) as messages
from messages
where channel_id = 'C1' and ts = '1735689600.000001'
group by workspace_id
order by workspace_id`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"workspace_id": "T1", "messages": int64(1)}}, rows)

	rows, err = st.QueryReadOnly(ctx, `
select workspace_id
from channels
where id = 'C1'`)
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"workspace_id": "T1"}}, rows)
}

func writeImportFixtureZip(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = w.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())

	zipPath := filepath.Join(t.TempDir(), "import-fixture.zip")
	require.NoError(t, os.WriteFile(zipPath, buf.Bytes(), 0o600))
	return zipPath
}
