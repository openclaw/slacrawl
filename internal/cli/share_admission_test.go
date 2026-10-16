package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/stretchr/testify/require"
)

const shareDMCanary = "syntheticsharedmcanary"

func shareAdmissionConfig(dir string) config.Config {
	cfg := config.Default()
	cfg.DBPath = filepath.Join(dir, "archive.db")
	cfg.CacheDir = filepath.Join(dir, "cache")
	cfg.LogDir = filepath.Join(dir, "logs")
	cfg.Share.RepoPath = filepath.Join(dir, "share")
	cfg.Share.Branch = "main"
	cfg.Share.StaleAfter = "1h"
	cfg.Share.AutoUpdate = true
	cfg.Slack.Bot.Enabled = false
	cfg.Slack.App.Enabled = false
	cfg.Slack.User.Enabled = false
	cfg.Slack.Desktop.Enabled = false
	cfg.Slack.Desktop.Path = filepath.Join(dir, "unused-desktop")
	cfg.Slack.MCP.Enabled = false
	cfg.Slack.MCP.AuthPath = filepath.Join(dir, "unused-auth")
	return cfg
}

func isolateShareGit(t *testing.T, dir string) {
	t.Helper()
	hooks := filepath.Join(dir, "empty-hooks")
	require.NoError(t, os.MkdirAll(hooks, 0o700))
	gitConfig := filepath.Join(dir, "gitconfig")
	// Background repacking must not race the byte-for-byte rejection snapshots.
	require.NoError(t, os.WriteFile(gitConfig, []byte(fmt.Sprintf(`[user]
name = Slacrawl Fixture
email = fixture@example.invalid
[commit]
gpgSign = false
[tag]
gpgSign = false
[init]
defaultBranch = main
[core]
hooksPath = %q
[maintenance]
auto = false
[gc]
auto = 0
`, filepath.ToSlash(hooks))), 0o600))
	for key, value := range map[string]string{
		"GIT_CONFIG_GLOBAL": gitConfig, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_COUNT": "0", "GIT_TERMINAL_PROMPT": "0", "GIT_ALLOW_PROTOCOL": "file",
		"GIT_AUTHOR_NAME": "Slacrawl Fixture", "GIT_COMMITTER_NAME": "Slacrawl Fixture",
		"GIT_AUTHOR_EMAIL": "fixture@example.invalid", "GIT_COMMITTER_EMAIL": "fixture@example.invalid",
		"GIT_TRACE2_EVENT": "",
	} {
		t.Setenv(key, value)
	}
	calibration := filepath.Join(dir, "trace-calibration.jsonl")
	t.Setenv("GIT_TRACE2_EVENT", calibration)
	runGit(t, dir, "--version")
	t.Setenv("GIT_TRACE2_EVENT", "")
	require.Positive(t, shareGitStarts(t, calibration), "Git Trace2 must observe a real child process")
}

func shareGitStarts(t *testing.T, path string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	require.NoError(t, err)
	decoder := json.NewDecoder(bytes.NewReader(body))
	starts := 0
	for {
		var event struct {
			Event string
		}
		err := decoder.Decode(&event)
		if errors.Is(err, io.EOF) {
			return starts
		}
		require.NoError(t, err, "invalid Git Trace2 record")
		require.NotEmpty(t, event.Event, "Git Trace2 record must name an event")
		if event.Event == "start" {
			starts++
		}
	}
}

func sharePathExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	require.NoError(t, err)
	return true
}

func shareArchiveSnapshot(t *testing.T, path string) map[string][]map[string]any {
	t.Helper()
	if !sharePathExists(t, path) {
		return nil
	}
	st, err := store.OpenReadOnly(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	snapshot := map[string][]map[string]any{}
	for _, table := range []string{"workspaces", "channels", "users", "messages", "message_files", "message_events", "message_event_heads", "message_mentions", "message_fts", "embedding_jobs", "sync_state"} {
		orderBy := "rowid"
		if table == "message_event_heads" {
			// This WITHOUT ROWID table needs its full primary key for stable snapshots.
			orderBy = "channel_id,ts,event_type,source_name"
		}
		snapshot[table], err = st.QueryReadOnly(context.Background(), "select * from "+table+" order by "+orderBy)
		require.NoError(t, err, "snapshot table %s", table)
	}
	return snapshot
}

func shareGitState(t *testing.T, repo string) string {
	t.Helper()
	if !sharePathExists(t, repo) {
		return ""
	}
	//nolint:gosec // The fixture owns this repository and fixed Git arguments.
	output, err := exec.Command("git", "-C", repo, "show-ref", "--head").CombinedOutput()
	require.NoError(t, err, "read fixture refs")
	return string(output)
}

func TestGitShareDMPolicyFromCLIConfig(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	isolateShareGit(t, dir)
	remote := filepath.Join(dir, "remote.git")
	runGit(t, dir, "init", "--bare", remote)
	publisher := shareAdmissionConfig(filepath.Join(dir, "publisher"))
	publisher.Share.Remote = remote
	publisherPath := filepath.Join(dir, "publisher.toml")
	require.NoError(t, publisher.Save(publisherPath))
	seedArchiveStore(t, publisher.DBPath, "published baseline")
	st, err := store.Open(publisher.DBPath)
	require.NoError(t, err)
	now := mustTime(t, "2026-03-08T18:20:43Z")
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "DDM", WorkspaceID: "T1", Name: "direct", Kind: "im", RawJSON: "{\"is_im\":true}", UpdatedAt: now}))
	require.NoError(t, st.UpsertMessage(ctx, store.Message{
		ChannelID: "DDM", TS: "1710000000.000300", WorkspaceID: "T1", UserID: "U1",
		Text: shareDMCanary, NormalizedText: shareDMCanary, RawJSON: "{\"text\":\"" + shareDMCanary + "\"}",
		SourceRank: 2, SourceName: "api-bot", UpdatedAt: now,
		Files: []store.MessageFile{{FileID: "FDM", Name: shareDMCanary, RawJSON: "{\"title\":\"" + shareDMCanary + "\"}"}},
	}, nil))
	require.NoError(t, st.Close())
	var published bytes.Buffer
	app := &App{Stdout: &published, Stderr: &published}
	require.NoError(t, app.Run(ctx, []string{"--config", publisherPath, "--json", "publish", "--tag", "baseline", "--push", "--no-media"}))
	appendArchiveMessage(t, publisher.DBPath, "published current")
	require.NoError(t, app.Run(ctx, []string{"--config", publisherPath, "--json", "publish", "--push", "--no-media"}))

	type policyCase struct {
		name  string
		value *bool
	}
	type routeCase struct {
		name   string
		policy policyCase
	}
	var cases []routeCase
	for _, route := range []string{"subscribe", "update", "restore", "historical", "stale-auto-search"} {
		for _, policy := range []policyCase{{"omitted", nil}, {"true", new(true)}, {"false", new(false)}} {
			cases = append(cases, routeCase{route, policy})
		}
	}
	cases = append(cases, routeCase{"no-import", policyCase{"false", new(false)}}, routeCase{"fresh-auto-search", policyCase{"false", new(false)}})
	for _, tc := range cases {
		t.Run(tc.name+"/"+tc.policy.name, func(t *testing.T) {
			caseDir := t.TempDir()
			cfg := shareAdmissionConfig(caseDir)
			cfg.Share.Remote = remote
			cfg.Sync.IncludeDMs = tc.policy.value
			configPath := filepath.Join(caseDir, "config.toml")
			require.NoError(t, cfg.Save(configPath))
			dbPath, repoPath := cfg.DBPath, cfg.Share.RepoPath
			var args []string
			switch tc.name {
			case "subscribe", "no-import":
				dbPath, repoPath = filepath.Join(caseDir, "subscribed.db"), filepath.Join(caseDir, "subscribed-share")
				args = []string{"subscribe", "--repo", repoPath, "--db", dbPath, "--no-media"}
				if tc.name == "no-import" {
					args = append(args, "--no-import", "--no-auto-update")
				}
				args = append(args, remote)
			case "update":
				args = []string{"update", "--no-media"}
			case "restore", "historical":
				args = []string{"update", "--restore", "--no-media"}
				if tc.name == "historical" {
					args = append(args, "--ref", "baseline")
				}
				runGit(t, caseDir, "clone", "--branch", "main", remote, repoPath)
			case "stale-auto-search", "fresh-auto-search":
				args = []string{"search", shareDMCanary}
			}
			if tc.name == "restore" || tc.name == "historical" || strings.HasSuffix(tc.name, "auto-search") {
				seedArchiveStore(t, dbPath, "local marker")
				reader, err := store.Open(dbPath)
				require.NoError(t, err)
				importedAt := time.Now().UTC().Add(-2 * time.Hour)
				if tc.name == "fresh-auto-search" {
					importedAt = time.Now().UTC()
				}
				require.NoError(t, reader.SetSyncState(ctx, "share", "import", "last_import_at", importedAt.Format(time.RFC3339Nano)))
				require.NoError(t, reader.SetSyncState(ctx, "share", "import", "last_manifest_generated_at", "2000-01-01T00:00:00Z"))
				require.NoError(t, reader.Close())
			}
			beforeConfig, err := os.ReadFile(configPath)
			require.NoError(t, err)
			beforeDBExists, beforeRepoExists := sharePathExists(t, dbPath), sharePathExists(t, repoPath)
			beforeArchive := shareArchiveSnapshot(t, dbPath)
			beforeRepo := shareGitState(t, repoPath)
			tracePath := filepath.Join(caseDir, "target-trace.jsonl")
			var output bytes.Buffer
			target := &App{Stdout: &output, Stderr: &output}
			t.Setenv("GIT_TRACE2_EVENT", tracePath)
			runErr := target.Run(ctx, append([]string{"--config", configPath, "--json"}, args...))
			// Capture acquisition before any verification command can add a Git event.
			starts := shareGitStarts(t, tracePath)
			t.Setenv("GIT_TRACE2_EVENT", "")
			afterConfig, err := os.ReadFile(configPath)
			require.NoError(t, err)
			afterDBExists, afterRepoExists := sharePathExists(t, dbPath), sharePathExists(t, repoPath)
			afterArchive := shareArchiveSnapshot(t, dbPath)
			afterRepo := shareGitState(t, repoPath)
			dmRows, currentRows := 0, 0
			for _, row := range afterArchive["messages"] {
				if row["channel_id"] == "DDM" {
					dmRows++
				}
				if row["channel_id"] == "C1" && row["ts"] == "1710003600.000200" {
					currentRows++
				}
			}
			configChanged := !bytes.Equal(beforeConfig, afterConfig)
			archiveChanged := !reflect.DeepEqual(beforeArchive, afterArchive)
			repoChanged := beforeRepo != afterRepo || beforeRepoExists != afterRepoExists
			archiveChanged = archiveChanged || beforeDBExists != afterDBExists
			diagnostics := output.String()
			if runErr != nil {
				diagnostics += runErr.Error()
			}
			outputLeak := strings.Contains(diagnostics, shareDMCanary)
			active := tc.name != "no-import" && tc.name != "fresh-auto-search"
			if tc.policy.name == "false" && active {
				policyError := runErr != nil && strings.Contains(runErr.Error(), "sync.include_dms")
				if !policyError || starts != 0 || configChanged || archiveChanged || repoChanged || dmRows != 0 || outputLeak {
					t.Fatalf("share admission boundary: error=%t policy_error=%t git_starts=%d config_changed=%t archive_changed=%t repo_changed=%t dm_rows=%d output_leak=%t",
						runErr != nil, policyError, starts, configChanged, archiveChanged, repoChanged, dmRows, outputLeak)
				}
				return
			}
			require.NoError(t, runErr)
			if tc.name == "no-import" {
				require.Zero(t, starts)
				require.Nil(t, afterArchive)
				require.Empty(t, afterRepo)
				saved, err := config.Load(configPath)
				require.NoError(t, err)
				require.Equal(t, dbPath, saved.DBPath)
				require.Equal(t, repoPath, saved.Share.RepoPath)
				require.False(t, saved.Share.AutoUpdate)
				require.NotNil(t, saved.Sync.IncludeDMs)
				require.False(t, *saved.Sync.IncludeDMs)
			} else if tc.name == "fresh-auto-search" {
				require.Zero(t, starts)
				require.False(t, configChanged)
				require.False(t, archiveChanged)
				require.Empty(t, afterRepo)
			} else {
				require.Positive(t, starts)
				require.Equal(t, 1, dmRows)
				if tc.name == "historical" {
					require.Zero(t, currentRows)
					require.Equal(t, beforeRepo, afterRepo)
				} else {
					require.Equal(t, 1, currentRows)
				}
			}
		})
	}
}
