package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/openclaw/slacrawl/internal/share"
	"github.com/openclaw/slacrawl/internal/store"
	"github.com/stretchr/testify/require"
)

const shareDraftCanary = "syntheticsharedraftcanary"

func seedSharePublishArchive(t *testing.T, path string) {
	t.Helper()
	seedArchiveStore(t, path, "public share fixture")
	st, err := store.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	ctx := context.Background()
	now := mustTime(t, "2026-03-08T18:20:43Z")
	require.NoError(t, st.UpsertChannel(ctx, store.Channel{ID: "DDM", WorkspaceID: "T1", Name: "direct", Kind: "im", RawJSON: "{}", UpdatedAt: now}))
	for _, message := range []store.Message{
		{ChannelID: "DDM", TS: "1710000000.000200", Text: shareDMCanary},
		{ChannelID: "C1", TS: "draft:fixture", Subtype: "draft", Text: shareDraftCanary},
	} {
		message.WorkspaceID, message.UserID = "T1", "U1"
		message.NormalizedText, message.RawJSON = message.Text, fmt.Sprintf(`{"text":%q}`, message.Text)
		message.SourceName, message.SourceRank, message.UpdatedAt = "desktop", 3, now
		require.NoError(t, st.UpsertMessage(ctx, message, nil))
	}
}

// Capture bytes, permissions and directories, including Git's index and refs.
func sharePublishTree(t *testing.T, root string) map[string]string {
	t.Helper()
	if !sharePathExists(t, root) {
		return nil
	}
	files := map[string]string{}
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			files[rel] = info.Mode().String()
			return nil
		}
		require.True(t, info.Mode().IsRegular(), "fixture file %s", rel)
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = fmt.Sprintf("%s:%x", info.Mode(), sha256.Sum256(body))
		return nil
	}))
	return files
}

func TestPublishRejectsDMExclusionBeforeSideEffects(t *testing.T) {
	isolateShareGit(t, t.TempDir())
	// The release notifier runs before dispatch; this oracle covers publish work.
	t.Setenv("SLACRAWL_NO_UPDATE_CHECK", "1")
	for _, state := range []string{"missing", "existing"} {
		for _, mode := range []struct {
			name string
			args []string
		}{
			{"plain", nil}, {"no-commit", []string{"--no-commit"}},
			{"no-media", []string{"--no-media"}}, {"tag-push", []string{"--tag", "blocked", "--push"}},
		} {
			t.Run(state+"/"+mode.name, func(t *testing.T) {
				dir := t.TempDir()
				stateDir := filepath.Join(dir, "state")
				cfg := shareAdmissionConfig(stateDir)
				cfg.Share.Remote = filepath.Join(dir, "remote.git")
				configPath := filepath.Join(dir, "config.toml")
				var output bytes.Buffer
				app := &App{Stdout: &output, Stderr: &output}
				if state == "existing" {
					runGit(t, dir, "init", "--bare", cfg.Share.Remote)
					seedSharePublishArchive(t, cfg.DBPath)
					require.NoError(t, cfg.Save(configPath))
					require.NoError(t, app.Run(context.Background(), []string{"--config", configPath, "--json", "publish", "--tag", "baseline", "--push"}))
					require.FileExists(t, filepath.Join(cfg.Share.RepoPath, "manifest.json"))
					require.FileExists(t, filepath.Join(cfg.Share.RepoPath, ".git", "index"))
				}
				cfg.Sync.IncludeDMs = new(false)
				require.NoError(t, cfg.Save(configPath))
				configBefore, err := os.ReadFile(configPath)
				require.NoError(t, err)
				archiveBefore := shareArchiveSnapshot(t, cfg.DBPath)
				refsBefore := shareGitState(t, cfg.Share.RepoPath)
				remoteRefsBefore := shareGitState(t, cfg.Share.Remote)
				stateBefore := sharePublishTree(t, stateDir)
				remoteBefore := sharePublishTree(t, cfg.Share.Remote)
				for attempt := range 2 {
					output.Reset()
					trace := filepath.Join(dir, fmt.Sprintf("publish-%d.jsonl", attempt))
					t.Setenv("GIT_TRACE2_EVENT", trace)
					err = app.Run(context.Background(), append([]string{"--config", configPath, "--json", "publish"}, mode.args...))
					starts := shareGitStarts(t, trace)
					t.Setenv("GIT_TRACE2_EVENT", "")
					require.EqualError(t, err, "legacy Git share exports cannot enforce sync.include_dms=false; keep this archive local")
					require.Zero(t, starts)
					require.Empty(t, output.String())
					require.Equal(t, stateBefore, sharePublishTree(t, stateDir))
					require.Equal(t, remoteBefore, sharePublishTree(t, cfg.Share.Remote))
					require.Equal(t, archiveBefore, shareArchiveSnapshot(t, cfg.DBPath))
					require.Equal(t, refsBefore, shareGitState(t, cfg.Share.RepoPath))
					require.Equal(t, remoteRefsBefore, shareGitState(t, cfg.Share.Remote))
					configAfter, err := os.ReadFile(configPath)
					require.NoError(t, err)
					require.Equal(t, configBefore, configAfter)
				}
			})
		}
	}
}

func TestPublishDMPolicyPreservesAllowedSnapshots(t *testing.T) {
	isolateShareGit(t, t.TempDir())
	t.Setenv("SLACRAWL_NO_UPDATE_CHECK", "1")
	for _, policy := range []string{"omitted", "true"} {
		t.Run(policy, func(t *testing.T) {
			dir := t.TempDir()
			cfg := shareAdmissionConfig(filepath.Join(dir, "publisher"))
			cfg.Share.Remote = filepath.Join(dir, "private-remote.git")
			if policy == "true" {
				cfg.Sync.IncludeDMs = new(true)
			}
			cfg.Slack.Desktop.IncludeDrafts = new(false)
			configPath := filepath.Join(dir, "config.toml")
			require.NoError(t, cfg.Save(configPath))
			configBefore, err := os.ReadFile(configPath)
			require.NoError(t, err)
			runGit(t, dir, "init", "--bare", cfg.Share.Remote)
			seedSharePublishArchive(t, cfg.DBPath)
			before := shareArchiveSnapshot(t, cfg.DBPath)
			var output bytes.Buffer
			app := &App{Stdout: &output, Stderr: &output}
			require.NoError(t, app.Run(context.Background(), []string{"--config", configPath, "--json", "publish", "--tag", "allowed", "--push"}))
			var result map[string]any
			require.NoError(t, json.Unmarshal(output.Bytes(), &result))
			require.Equal(t, true, result["committed"])
			require.Equal(t, true, result["pushed"])
			require.Equal(t, "allowed", result["tag"])
			configAfter, err := os.ReadFile(configPath)
			require.NoError(t, err)
			require.Equal(t, configBefore, configAfter)
			require.Equal(t, before["messages"], shareArchiveSnapshot(t, cfg.DBPath)["messages"])
			require.Contains(t, shareGitState(t, cfg.Share.Remote), "refs/tags/allowed")
			clone := filepath.Join(dir, "reader-share")
			runGit(t, dir, "clone", "--branch", "main", cfg.Share.Remote, clone)
			runGit(t, clone, "diff", "--exit-code", "HEAD", "refs/tags/allowed")
			reader, err := store.Open(filepath.Join(dir, "reader.db"))
			require.NoError(t, err)
			defer func() { require.NoError(t, reader.Close()) }()
			_, err = share.Import(context.Background(), reader, share.Options{RepoPath: clone})
			require.NoError(t, err)
			rows, err := reader.QueryReadOnly(context.Background(), "select * from messages order by rowid")
			require.NoError(t, err)
			require.ElementsMatch(t, before["messages"], rows)
			require.Len(t, rows, 3)
			byKey := map[[2]string]map[string]any{}
			for _, row := range rows {
				byKey[[2]string{row["channel_id"].(string), row["ts"].(string)}] = row
			}
			require.Equal(t, shareDMCanary, byKey[[2]string{"DDM", "1710000000.000200"}]["text"])
			require.Equal(t, shareDraftCanary, byKey[[2]string{"C1", "draft:fixture"}]["text"])
		})
	}
}

func TestPublishDMPolicyPreservesArgumentAndHelpPrecedence(t *testing.T) {
	isolateShareGit(t, t.TempDir())
	t.Setenv("SLACRAWL_NO_UPDATE_CHECK", "1")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown-flag", []string{"--unknown"}, "flag provided but not defined"},
		{"argument", []string{"extra"}, "publish takes no positional arguments"},
		{"no-commit-tag", []string{"--no-commit", "--tag", "snapshot"}, "publish --tag requires a commit"},
		{"no-commit-push", []string{"--no-commit", "--push"}, "publish --push requires a commit"},
		{"missing-config", nil, "no such file"},
		{"malformed-config", nil, "toml"},
		{"help", []string{"--help"}, ""},
		{"help-malformed-config", []string{"--help"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stateDir := filepath.Join(dir, "state")
			cfg := shareAdmissionConfig(stateDir)
			cfg.Sync.IncludeDMs = new(false)
			configPath := filepath.Join(dir, "config.toml")
			if tc.name != "missing-config" {
				require.NoError(t, cfg.Save(configPath))
				if tc.name == "malformed-config" || tc.name == "help-malformed-config" {
					require.NoError(t, os.WriteFile(configPath, []byte("[sync\n"), 0o600))
				}
			}
			var output bytes.Buffer
			app := &App{Stdout: &output, Stderr: &output}
			trace := filepath.Join(dir, "trace.jsonl")
			t.Setenv("GIT_TRACE2_EVENT", trace)
			err := app.Run(context.Background(), append([]string{"--config", configPath, "--json", "publish"}, tc.args...))
			starts := shareGitStarts(t, trace)
			t.Setenv("GIT_TRACE2_EVENT", "")
			if tc.want == "" {
				require.NoError(t, err)
				require.Contains(t, output.String(), "Usage of publish:")
			} else {
				require.ErrorContains(t, err, tc.want)
				require.NotContains(t, err.Error(), "sync.include_dms")
			}
			require.Zero(t, starts)
			require.NoDirExists(t, stateDir)
		})
	}
}
