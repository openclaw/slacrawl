package slackapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/admission"
	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
)

const sdkArchiveRoot = "1710000000.000100"
const sdkArchiveReply = "1710000001.000200"

// The fixture is actual v0.10.6 / slack-go v0.33.0 writer output, not a
// reconstruction made with the SDK under test. Its SQL header records provenance.
func TestSDKArchiveUpgrade(t *testing.T) {
	fixture := filepath.Join("testdata", "sdk033_archive")
	archiveSQL, err := os.ReadFile(filepath.Join(fixture, "archive.sql"))
	require.NoError(t, err)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "archive.db")
	sdkArchiveRestore(t, dbPath, string(archiveSQL))
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	// Reopening must preserve every logical row, raw byte, rowid, sequence,
	// schema object and FTS row before any new ingestion.
	require.Equal(t, sha256.Sum256(archiveSQL), sha256.Sum256([]byte(sdkArchiveDump(t, st.DB()))))
	configPath := filepath.Join(fixture, "config.toml")
	cfg, err := config.Load(configPath)
	require.NoError(t, err)
	require.NoError(t, cfg.Save(filepath.Join(dir, "config.toml")))
	wantConfig, err := os.ReadFile(configPath)
	require.NoError(t, err)
	gotConfig, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	require.NoError(t, err)
	require.Equal(t, string(wantConfig), string(gotConfig))
	require.Equal(t, "TSDKFIXTURE", cfg.WorkspaceID)
	require.Equal(t, 1, cfg.Sync.Concurrency)
	require.True(t, cfg.Sync.FullHistory)
	require.False(t, cfg.Sync.AutoJoinResolved())

	oldEvents := sdkArchiveRows(t, st.DB(), "SELECT * FROM message_events ORDER BY id")
	require.Len(t, oldEvents.values, 3, "two root revisions and one reply from the old writer")
	require.Equal(t, [][]any{
		{int64(1), sdkArchiveRoot, "message", SourceUser, "alpha <@UMENTION>"},
		{int64(2), sdkArchiveReply, "message", SourceUser, "replymarker"},
		{int64(3), sdkArchiveRoot, "message_changed", SourceUser, "beta <@UMENTION>"},
	}, sdkArchiveRows(t, st.DB(), "SELECT id,ts,event_type,source_name,json_extract(payload_json,'$.text') FROM message_events ORDER BY id").values)
	identities := sdkArchiveRows(t, st.DB(), "SELECT rowid,channel_id,ts,workspace_id,user_id,thread_ts FROM messages ORDER BY rowid")
	files := sdkArchiveRows(t, st.DB(), "SELECT channel_id,ts,file_id,name,plain_text,raw_json FROM message_files ORDER BY channel_id,ts,file_id")
	mentions := sdkArchiveRows(t, st.DB(), "SELECT channel_id,ts,mention_type,target_id,display_text FROM message_mentions ORDER BY channel_id,ts,mention_type,target_id")
	require.Len(t, identities.values, 2)
	require.Len(t, files.values, 1)
	require.Len(t, mentions.values, 1)
	sdkArchiveSearch(t, st, "beta")
	oldRaw := sdkArchiveRaw(t, st)

	// v0.34 retained explicitly false display_as_bot, previously omitted when
	// marshaling. Only this documented serialization change may add an event.
	sdkArchiveReplay(t, st, fixture, "old-b", 0)
	newRaw := sdkArchiveRaw(t, st)
	for ts, before := range oldRaw {
		var oldObject, newObject map[string]any
		require.NoError(t, json.Unmarshal([]byte(before), &oldObject))
		require.NoError(t, json.Unmarshal([]byte(newRaw[ts]), &newObject))
		if ts == sdkArchiveRoot {
			_, hadFlag := oldObject["display_as_bot"]
			require.False(t, hadFlag)
			require.Equal(t, false, newObject["display_as_bot"])
			delete(newObject, "display_as_bot")
		} else {
			require.Equal(t, before, newRaw[ts], "unchanged reply must keep its raw bytes")
		}
		require.Equal(t, oldObject, newObject, "no unexplained SDK serialization changes")
	}
	events := sdkArchiveRows(t, st.DB(), "SELECT * FROM message_events ORDER BY id")
	require.Len(t, events.values, len(oldEvents.values)+1)
	require.Equal(t, oldEvents, sdkArchiveTable{columns: events.columns, values: events.values[:len(oldEvents.values)]}, "old history and event keys remain byte-identical")
	sdkArchiveRequireLastEvent(t, st, newRaw[sdkArchiveRoot])
	sdkArchiveStable(t, st, identities, files, mentions)
	sdkArchiveSearch(t, st, "beta")
	sdkArchiveReplay(t, st, fixture, "old-b", 1)
	require.Equal(t, events, sdkArchiveRows(t, st.DB(), "SELECT * FROM message_events ORDER BY id"))

	sdkArchiveReplay(t, st, fixture, "changed", 2)
	changed := sdkArchiveRows(t, st.DB(), "SELECT * FROM message_events ORDER BY id")
	require.Len(t, changed.values, len(events.values)+1, "one real edit produces exactly one event")
	require.Equal(t, events.values, changed.values[:len(events.values)])
	sdkArchiveRequireLastEvent(t, st, sdkArchiveRaw(t, st)[sdkArchiveRoot])
	sdkArchiveStable(t, st, identities, files, mentions)
	sdkArchiveSearch(t, st, "gamma")
	heads := sdkArchiveRows(t, st.DB(), "SELECT * FROM message_event_heads ORDER BY channel_id,ts,event_type,source_name")
	sdkArchiveReplay(t, st, fixture, "changed", 3)
	require.Equal(t, changed, sdkArchiveRows(t, st.DB(), "SELECT * FROM message_events ORDER BY id"), "identical replay adds zero events")
	require.Equal(t, heads, sdkArchiveRows(t, st.DB(), "SELECT * FROM message_event_heads ORDER BY channel_id,ts,event_type,source_name"))
	sdkArchiveStable(t, st, identities, files, mentions)
	sdkArchiveSearch(t, st, "gamma")
}

func sdkArchiveStable(t *testing.T, st *store.Store, identities, files, mentions sdkArchiveTable) {
	t.Helper()
	require.Equal(t, identities, sdkArchiveRows(t, st.DB(), "SELECT rowid,channel_id,ts,workspace_id,user_id,thread_ts FROM messages ORDER BY rowid"))
	require.Equal(t, files, sdkArchiveRows(t, st.DB(), "SELECT channel_id,ts,file_id,name,plain_text,raw_json FROM message_files ORDER BY channel_id,ts,file_id"))
	require.Equal(t, mentions, sdkArchiveRows(t, st.DB(), "SELECT channel_id,ts,mention_type,target_id,display_text FROM message_mentions ORDER BY channel_id,ts,mention_type,target_id"))
}

func sdkArchiveRequireLastEvent(t *testing.T, st *store.Store, payload string) {
	t.Helper()
	rows := sdkArchiveRows(t, st.DB(), "SELECT channel_id,ts,event_type,source_name,payload_json FROM message_events ORDER BY id DESC LIMIT 1")
	require.Equal(t, [][]any{{"CSDKFIXTURE", sdkArchiveRoot, "message_changed", SourceUser, payload}}, rows.values)
}

func sdkArchiveRaw(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	rows := sdkArchiveRows(t, st.DB(), "SELECT ts,raw_json FROM messages ORDER BY ts")
	result := make(map[string]string, len(rows.values))
	for _, row := range rows.values {
		result[row[0].(string)] = row[1].(string)
	}
	return result
}

func sdkArchiveSearch(t *testing.T, st *store.Store, rootTerm string) {
	t.Helper()
	for term, wantTS := range map[string]string{rootTerm: sdkArchiveRoot, "blockmarker": sdkArchiveRoot, "filemarker": sdkArchiveRoot, "replymarker": sdkArchiveReply} {
		rows, err := st.SearchMessages(context.Background(), store.SearchOptions{WorkspaceID: "TSDKFIXTURE", Query: term, Mode: store.SearchModeRawFTS, Limit: 10})
		require.NoError(t, err)
		require.Len(t, rows, 1, term)
		require.Equal(t, wantTS, rows[0].TS)
	}
}

type sdkArchiveTransport struct {
	endpoint *url.URL
	base     http.RoundTripper
}

func (s sdkArchiveTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != s.endpoint.Scheme || r.URL.Host != s.endpoint.Host {
		return nil, fmt.Errorf("fixture transport rejected non-loopback destination")
	}
	return s.base.RoundTrip(r)
}

func sdkArchiveReplay(t *testing.T, st *store.Store, fixture, phase string, clock int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixture, "api.json"))
	require.NoError(t, err)
	var responses map[string]map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &responses))
	require.Contains(t, responses, phase)
	var mu sync.Mutex
	counts := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts[r.URL.Path]++
		mu.Unlock()
		response, ok := responses[phase][r.URL.Path]
		if !ok {
			http.Error(w, "unexpected fixture method", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(response)
	}))
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	require.NoError(t, err)
	httpClient := server.Client()
	httpClient.Transport = sdkArchiveTransport{endpoint: endpoint, base: httpClient.Transport}
	client := NewWithOptions(config.Tokens{User: "fixture-user"}, server.URL+"/", httpClient).WithDMPolicy(admission.Exclude)
	client.now = func() time.Time { return time.Unix(1710000100+int64(clock)*100, 0).UTC() }
	require.NoError(t, client.Sync(context.Background(), st, SyncOptions{WorkspaceID: "TSDKFIXTURE", Full: true, Concurrency: 1, AutoJoin: new(false)}))
	mu.Lock()
	defer mu.Unlock()
	for _, method := range []string{"/auth.test", "/conversations.list", "/conversations.history", "/conversations.replies", "/users.list"} {
		require.Positive(t, counts[method], "forced replay must exercise %s", method)
	}
}

type sdkArchiveTable struct {
	columns []string
	values  [][]any
}

func sdkArchiveRows(t *testing.T, db *sql.DB, query string) sdkArchiveTable {
	t.Helper()
	rows, err := db.Query(query)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	columns, err := rows.Columns()
	require.NoError(t, err)
	result := sdkArchiveTable{columns: columns}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		require.NoError(t, rows.Scan(pointers...))
		for i, value := range values {
			if b, ok := value.([]byte); ok {
				values[i] = append([]byte(nil), b...)
			}
		}
		result.values = append(result.values, values)
	}
	require.NoError(t, rows.Err())
	return result
}

func sdkArchiveRestore(t *testing.T, path, dump string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(dump)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

func sdkArchiveIdentifier(s string) string { return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\"" }

func sdkArchiveLiteral(t *testing.T, value any) string {
	t.Helper()
	switch v := value.(type) {
	case nil:
		return "NULL"
	case string:
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	case []byte:
		return "X'" + hex.EncodeToString(v) + "'"
	case int64, float64:
		return fmt.Sprint(v)
	default:
		t.Fatalf("unsupported SQLite fixture value %T", value)
		return ""
	}
}

// Dump public schema and logical rows, never writable_schema or FTS shadow
// tables. Preserve explicit rowids; restore FTS through its ordinary insert API.
func sdkArchiveDump(t *testing.T, db *sql.DB) string {
	t.Helper()
	schema := sdkArchiveRows(t, db, "SELECT type,name,sql FROM sqlite_schema WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' AND name NOT LIKE 'message_fts_%' ORDER BY type,name")
	var out strings.Builder
	out.WriteString("-- Actual slacrawl v0.10.6 Go sources; slack-go v0.33.0; SQLite schema 8.\n-- Producer af7814cc2f246d6a29998465e5416c6f0927e136 has identical Go/mod/sum to v0.10.6.\n-- Generated offline by real Client.Sync (old-a, old-b), config.Save and store.Open.\n-- Tables, rowids, raw bytes, event keys, heads, sync state and FTS were old-writer roundtrip-verified.\nBEGIN;\n")
	for _, object := range schema.values {
		if object[0] == "table" {
			out.WriteString(object[2].(string) + ";\n")
		}
	}
	for _, object := range schema.values {
		if object[0] != "table" {
			continue
		}
		name, ddl := object[1].(string), object[2].(string)
		selectColumns, order := "rowid AS fixture_rowid,*", "rowid"
		if strings.Contains(strings.ToUpper(ddl), "WITHOUT ROWID") {
			selectColumns = "*"
			info := sdkArchiveRows(t, db, "PRAGMA table_info("+sdkArchiveIdentifier(name)+")")
			var columns []string
			for _, column := range info.values {
				columns = append(columns, sdkArchiveIdentifier(column[1].(string)))
			}
			order = strings.Join(columns, ",")
		}
		rows := sdkArchiveRows(t, db, "SELECT "+selectColumns+" FROM "+sdkArchiveIdentifier(name)+" ORDER BY "+order)
		var columns []string
		for _, column := range rows.columns {
			if column == "fixture_rowid" {
				column = "rowid"
			}
			columns = append(columns, sdkArchiveIdentifier(column))
		}
		for _, row := range rows.values {
			var values []string
			for _, value := range row {
				values = append(values, sdkArchiveLiteral(t, value))
			}
			fmt.Fprintf(&out, "INSERT INTO %s (%s) VALUES (%s);\n", sdkArchiveIdentifier(name), strings.Join(columns, ","), strings.Join(values, ","))
		}
	}
	out.WriteString("DELETE FROM sqlite_sequence;\n")
	for _, row := range sdkArchiveRows(t, db, "SELECT name,seq FROM sqlite_sequence ORDER BY name").values {
		fmt.Fprintf(&out, "INSERT INTO sqlite_sequence (name,seq) VALUES (%s,%s);\n", sdkArchiveLiteral(t, row[0]), sdkArchiveLiteral(t, row[1]))
	}
	for _, object := range schema.values {
		if object[0] != "table" {
			out.WriteString(object[2].(string) + ";\n")
		}
	}
	version := sdkArchiveRows(t, db, "PRAGMA user_version")
	fmt.Fprintf(&out, "PRAGMA user_version=%v;\nCOMMIT;\n", version.values[0][0])
	return out.String()
}
