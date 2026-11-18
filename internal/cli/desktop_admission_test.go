package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/syndtr/goleveldb/leveldb"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
)

func TestDesktopDMPolicyAcrossCLIPaths(t *testing.T) {
	for _, route := range []struct {
		name       string
		args       []string
		workspaces int
	}{
		{"desktop", []string{"sync", "--source", "desktop"}, 2},
		{"wiretap", []string{"sync", "--source", "wiretap"}, 2},
		{"watch", []string{"watch", "--desktop-every", "1h"}, 2},
		{"all", []string{"sync", "--source", "all"}, 2},
		{"hybrid", []string{"sync", "--source", "hybrid", "--workspace", "T1"}, 1},
	} {
		for _, setting := range []string{"", "false", "true"} {
			for _, drafts := range []string{"", "false", "true"} {
				t.Run(fmt.Sprintf("%s/dms=%s/drafts=%s", route.name, setting, drafts), func(t *testing.T) {
					cfg, path := desktopDMConfig(t, setting, drafts)
					server := newCLIFanoutSlackServer(t, map[string]string{"xoxb-draft-one": "T1", "xoxb-draft-two": "T2"})
					defer server.Close()
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					var stdout, stderr bytes.Buffer
					app := &App{Stdout: &stdout, Stderr: &stderr, apiURL: server.URL + "/", httpClient: server.Client()}
					if route.name == "watch" {
						app.Stdout = &cancelAfterWrite{Writer: &stdout, cancel: cancel}
					}
					err := app.Run(ctx, append([]string{"--config", path, "--json"}, route.args...))
					if route.name == "watch" {
						require.ErrorIs(t, err, context.Canceled)
					} else {
						require.NoError(t, err)
					}
					st, err := store.OpenReadOnly(cfg.DBPath)
					require.NoError(t, err)
					defer func() { require.NoError(t, st.Close()) }()
					rows, err := st.QueryReadOnly(context.Background(), "select count(*) as n from messages where source_name = 'desktop-indexeddb'")
					require.NoError(t, err)
					want := route.workspaces * 2
					if setting == "false" {
						want = route.workspaces
					}
					require.Equal(t, int64(want), rows[0]["n"])
					wantDrafts := route.workspaces
					if setting == "false" {
						wantDrafts = 1
					}
					if drafts == "false" {
						wantDrafts = 0
					}
					rows, err = st.QueryReadOnly(context.Background(), "select count(*) as n from messages where subtype = 'desktop_draft'")
					require.NoError(t, err)
					require.Equal(t, int64(wantDrafts), rows[0]["n"])
					status, err := st.Status(context.Background())
					require.NoError(t, err)
					require.Equal(t, route.workspaces, status.Workspaces)
					if setting == "false" {
						for _, table := range draftArchiveTables {
							rows, err := st.QueryReadOnly(context.Background(), "select * from "+table)
							require.NoError(t, err)
							body, err := json.Marshal(rows)
							require.NoError(t, err)
							for _, canary := range []string{"desktop-dm-canary", "CDMONLY", "DDRAFTONLY"} {
								require.NotContains(t, string(body), canary, table)
								require.NotContains(t, stdout.String()+stderr.String(), canary)
							}
						}
						require.Contains(t, stdout.String(), `"admission"`)
					}
				})
			}
		}
	}
}

func TestDesktopSentRetentionAcrossCLIPaths(t *testing.T) {
	for _, route := range []struct {
		name       string
		args       []string
		workspaces int
	}{
		{"desktop", []string{"sync", "--source", "desktop"}, 2},
		{"wiretap", []string{"sync", "--source", "wiretap"}, 2},
		{"watch", []string{"watch", "--desktop-every", "1h"}, 2},
		{"all", []string{"sync", "--source", "all"}, 2},
		{"hybrid", []string{"sync", "--source", "hybrid", "--workspace", "T1"}, 1},
	} {
		for _, setting := range []string{"", "false", "true"} {
			t.Run(route.name+"/dms="+setting, func(t *testing.T) {
				cfg, path := desktopDMBaseConfig(t, setting, "false")
				for _, workspace := range []string{"T1", "T2"} {
					publicID, dmID := "CDRAFTONLY", "CDMONLY"
					if workspace == "T2" {
						publicID, dmID = "CPUBLIC2", "DDRAFTONLY"
					}
					writeDesktopDMBlob(t, cfg.Slack.Desktop.Path, workspace, map[string]any{
						"selfTeamIds": map[string]any{"teamId": workspace}, "bootData": map[string]any{"user_id": "U" + workspace},
						"channels": map[string]any{publicID: map[string]any{"id": publicID, "is_channel": true, "context_team_id": workspace}, dmID: map[string]any{"id": dmID, "is_im": true, "context_team_id": workspace}},
						"messages": map[string]any{
							publicID: map[string]any{
								"1710000000.000001": map[string]any{"ts": "1710000000.000001", "type": "message", "text": "expired-desktop-public <@UEXPIRED>"},
								"1710000100.000000": map[string]any{"ts": "1710000100.000000", "type": "message", "text": "retained-desktop-public <@UCURRENT>"},
							},
							dmID: map[string]any{
								"1710000001.000001": map[string]any{"ts": "1710000001.000001", "type": "message", "text": "expired-desktop-dm <@UEXPIRED>"},
								"1710000100.000001": map[string]any{"ts": "1710000100.000001", "type": "message", "text": "retained-desktop-dm <@UCURRENTDM>"},
							},
						},
					})
				}
				beforeConfig, err := os.ReadFile(path)
				require.NoError(t, err)
				// API channels C111/C222 stay distinct from the cached Desktop
				// identities; all/hybrid may legitimately refresh API checkpoints.
				server := newCLIFanoutSlackServer(t, map[string]string{"xoxb-draft-one": "T1", "xoxb-draft-two": "T2"})
				defer server.Close()
				run := func() string {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					var stdout, stderr bytes.Buffer
					app := &App{Stdout: &stdout, Stderr: &stderr, apiURL: server.URL + "/", httpClient: server.Client()}
					if route.name == "watch" {
						app.Stdout = &cancelAfterWrite{Writer: &stdout, cancel: cancel}
					}
					err := app.Run(ctx, append([]string{"--config", path, "--json"}, route.args...))
					if route.name == "watch" {
						require.ErrorIs(t, err, context.Canceled)
					} else {
						require.NoError(t, err)
					}
					require.NotContains(t, stdout.String()+stderr.String(), "expired-desktop")
					return stdout.String()
				}
				require.Contains(t, run(), "\"desktop\"")
				ctx := context.Background()
				st, err := store.Open(cfg.DBPath)
				require.NoError(t, err)
				desktopRows := func(st *store.Store) []map[string]any {
					rows, err := st.QueryReadOnly(ctx, "select workspace_id,channel_id,ts from messages where source_name='desktop-indexeddb' order by workspace_id,channel_id,ts")
					require.NoError(t, err)
					return rows
				}
				want := route.workspaces
				if setting != "false" {
					want *= 2
				}
				require.Len(t, desktopRows(st), want*2, "the initial cache contains both expired and retained sent rows")
				report, err := st.PurgeMessages(ctx, store.PurgeOptions{Before: time.Unix(1710000050, 0).UTC(), Delete: true, RequireNoMedia: true})
				require.NoError(t, err)
				require.GreaterOrEqual(t, report.Messages, int64(want))
				retained := desktopRows(st)
				require.Len(t, retained, want)
				floors := apiKeyRows(t, st, "select * from sync_state where source_name='retention' order by entity_type,entity_id")
				require.NoError(t, st.Close())
				require.Contains(t, run(), "\"desktop\"")
				st, err = store.OpenReadOnly(cfg.DBPath)
				require.NoError(t, err)
				defer func() { require.NoError(t, st.Close()) }()
				require.Equal(t, retained, desktopRows(st), "replayed cache must not restore purged Desktop identities")
				require.Equal(t, floors, apiKeyRows(t, st, "select * from sync_state where source_name='retention' order by entity_type,entity_id"))
				for _, table := range draftArchiveTables {
					rows := apiKeyRows(t, st, "select * from "+table)
					body, err := json.Marshal(rows)
					require.NoError(t, err)
					require.NotContains(t, string(body), "expired-desktop", table)
					require.NotContains(t, string(body), "UEXPIRED", table)
					if setting == "false" {
						require.NotContains(t, string(body), "retained-desktop-dm", table)
					}
				}
				for _, table := range []string{"message_events", "message_event_heads"} {
					rows := apiKeyRows(t, st, "select count(*) as n from "+table+" where source_name='desktop-indexeddb'")
					require.Equal(t, int64(want), rows[0]["n"], table)
				}
				keys := apiKeyRows(t, st, "select message_key from message_fts where message_key like 'CDRAFTONLY|%' or message_key like 'CPUBLIC2|%' or message_key like 'CDMONLY|%' or message_key like 'DDRAFTONLY|%' order by message_key")
				require.Len(t, keys, want)
				afterConfig, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, beforeConfig, afterConfig)
			})
		}
	}
}

func TestDesktopAdmissionHumanOmissions(t *testing.T) {
	for _, decoder := range []string{"available", "unavailable", "partial"} {
		t.Run(decoder, func(t *testing.T) {
			cfg, path := desktopDMConfig(t, "false", "false")
			if decoder == "unavailable" {
				t.Setenv("PATH", t.TempDir())
			}
			if decoder == "partial" {
				require.NoError(t, os.WriteFile(filepath.Join(cfg.Slack.Desktop.Path, "IndexedDB", "https_app.slack.com_0.indexeddb.blob", "invalid"), []byte{0xff, 0x11, 0x02, 0xff}, 0o600))
			}
			var output bytes.Buffer
			app := &App{Stdout: &output, Stderr: &output}
			require.NoError(t, app.Run(context.Background(), []string{"--config", path, "sync", "--source", "desktop"}))
			require.Contains(t, output.String(), "Completed with omissions")
			require.NotContains(t, output.String(), "desktop-dm-canary")
			if decoder == "unavailable" {
				require.Contains(t, output.String(), "decoder unavailable")
			}
			if decoder == "partial" {
				require.Contains(t, output.String(), "decode failures")
			}
		})
	}
}

func desktopDMBaseConfig(t *testing.T, setting, drafts string) (config.Config, string) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for desktop fixtures")
	}
	cfg, path := desktopDraftConfig(t, drafts)
	if setting != "" {
		cfg.Sync.IncludeDMs = new(setting == "true")
	}
	require.NoError(t, cfg.Save(path))
	return cfg, path
}

func desktopDMConfig(t *testing.T, setting, drafts string) (config.Config, string) {
	t.Helper()
	cfg, path := desktopDMBaseConfig(t, setting, drafts)
	for _, workspace := range []string{"T1", "T2"} {
		publicID, dmID := "CDRAFTONLY", "CDMONLY"
		if workspace == "T2" {
			publicID, dmID = "CPUBLIC2", "DDRAFTONLY"
		}
		value := map[string]any{
			"selfTeamIds": map[string]any{"teamId": workspace}, "bootData": map[string]any{"user_id": "U" + workspace},
			"channels": map[string]any{publicID: map[string]any{"id": publicID, "is_channel": true, "context_team_id": workspace}, dmID: map[string]any{"id": dmID, "is_im": true, "context_team_id": workspace}},
			"messages": map[string]any{publicID: map[string]any{"1": map[string]any{"ts": "1710000010.000001", "text": "desktop public message", "type": "message"}}, dmID: map[string]any{"2": map[string]any{"ts": "1710000020.000001", "text": "desktop-dm-canary <@UDMCANARY>", "type": "message"}}},
		}
		writeDesktopDMBlob(t, cfg.Slack.Desktop.Path, workspace, value)
	}
	return cfg, path
}

func writeDesktopDMBlob(t *testing.T, root, workspace string, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	require.NoError(t, err)
	cmd := exec.Command("node", "-e", `process.stdout.write(require("v8").serialize(JSON.parse(require("fs").readFileSync(0,"utf8"))))`)
	cmd.Stdin = strings.NewReader(string(body))
	payload, err := cmd.Output()
	require.NoError(t, err)
	dir := filepath.Join(root, "IndexedDB", "https_app.slack.com_0.indexeddb.blob")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, workspace), payload, 0o600))
}

func TestDesktopReadMarkersAcrossCLIPaths(t *testing.T) {
	for _, route := range []struct {
		name       string
		args       []string
		workspaces int
	}{
		{"desktop", []string{"sync", "--source", "desktop"}, 2},
		{"wiretap", []string{"sync", "--source", "wiretap"}, 2},
		{"watch", []string{"watch", "--desktop-every", "1h"}, 2},
		{"all", []string{"sync", "--source", "all"}, 2},
		{"hybrid", []string{"sync", "--source", "hybrid", "--workspace", "T1"}, 1},
	} {
		t.Run(route.name, func(t *testing.T) {
			cfg, path := desktopDMConfig(t, "", "false")
			db, err := leveldb.OpenFile(filepath.Join(cfg.Slack.Desktop.Path, "Local Storage", "leveldb"), nil)
			require.NoError(t, err)
			for _, marker := range []struct{ workspace, channel, ts string }{{"T1", "CDRAFTONLY", "11"}, {"T2", "CPUBLIC2", "22"}} {
				body, err := json.Marshal(map[string]any{"mark": map[string]any{"method": "conversations.mark", "args": map[string]any{"channel": marker.channel, "ts": marker.ts}}})
				require.NoError(t, err)
				require.NoError(t, db.Put([]byte("_https://app.slack.compersist-v1::"+marker.workspace+"::U1::persistedApiCalls"), body, nil))
			}
			require.NoError(t, db.Close())
			beforeConfig, err := os.ReadFile(path)
			require.NoError(t, err)
			st, err := store.Open(cfg.DBPath)
			require.NoError(t, err)
			require.NoError(t, st.SetSyncState(context.Background(), "desktop", "read_marker", "CDRAFTONLY", "historical"))
			legacy := apiKeyRows(t, st, "select * from sync_state where source_name='desktop' and entity_type='read_marker'")
			require.NoError(t, st.Close())
			server := newCLIFanoutSlackServer(t, map[string]string{"xoxb-draft-one": "T1", "xoxb-draft-two": "T2"})
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var stdout, stderr bytes.Buffer
			app := &App{Stdout: &stdout, Stderr: &stderr, apiURL: server.URL + "/", httpClient: server.Client()}
			if route.name == "watch" {
				app.Stdout = &cancelAfterWrite{Writer: &stdout, cancel: cancel}
			}
			err = app.Run(ctx, append([]string{"--config", path, "--json"}, route.args...))
			if route.name == "watch" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NoError(t, err)
			}
			st, err = store.OpenReadOnly(cfg.DBPath)
			require.NoError(t, err)
			defer func() { require.NoError(t, st.Close()) }()
			expected := []map[string]any{{"entity_id": "[\"T1\",\"CDRAFTONLY\"]", "value": "11"}}
			if route.workspaces == 2 {
				expected = append(expected, map[string]any{"entity_id": "[\"T2\",\"CPUBLIC2\"]", "value": "22"})
			}
			require.Equal(t, expected, apiKeyRows(t, st, "select entity_id,value from sync_state where source_name='desktop' and entity_type='read_marker_v1' order by entity_id"))
			require.Equal(t, legacy, apiKeyRows(t, st, "select * from sync_state where source_name='desktop' and entity_type='read_marker'"))
			count, err := st.GetSyncState(context.Background(), "desktop", "local_storage", "read_marker_count")
			require.NoError(t, err)
			require.Equal(t, fmt.Sprint(route.workspaces), count)
			require.Contains(t, stdout.String(), "read_marker_count")
			afterConfig, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, beforeConfig, afterConfig)
		})
	}
}
