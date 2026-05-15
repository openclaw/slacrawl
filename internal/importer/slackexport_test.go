package importer

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExportZIPMode(t *testing.T) {
	zipPath := writeFixtureZIP(t, fullFixtureFiles())
	ex, err := Open(zipPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, ex.Close()) }()

	users, err := ex.Users()
	require.NoError(t, err)
	require.Len(t, users, 2)
	require.Equal(t, "U1", users[0].ID)

	channels, err := ex.Channels()
	require.NoError(t, err)
	require.Len(t, channels, 3)
	require.Equal(t, "public", channels[0].Kind)
	require.Equal(t, "private", channels[2].Kind)

	dms, err := ex.DMs()
	require.NoError(t, err)
	require.Len(t, dms, 1)
	require.Equal(t, "im", dms[0].Kind)

	mpims, err := ex.MPIMs()
	require.NoError(t, err)
	require.Len(t, mpims, 1)
	require.Equal(t, "mpim", mpims[0].Kind)

	messages, iterErr := collectMessages(ex, "general")
	require.NoError(t, iterErr)
	require.Len(t, messages, 3)
	require.Equal(t, "2026-01-01", messages[0].Date)
	require.Equal(t, "2026-01-02", messages[2].Date)

	text, ok := messages[0].Raw["text"].(string)
	require.True(t, ok)
	require.Contains(t, text, "<@U2|bob>")
	require.Contains(t, text, "<#C2|random>")

	edited, ok := messages[1].Raw["edited"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "1735689900.000010", edited["ts"])

	_, ok = messages[2].Raw["deleted_ts"]
	require.True(t, ok)
}

func TestExportDirectoryModeWithMissingOptionalFiles(t *testing.T) {
	dir := writeFixtureDir(t, minimalFixtureFiles())
	ex, err := Open(dir)
	require.NoError(t, err)
	defer func() { require.NoError(t, ex.Close()) }()

	channels, err := ex.Channels()
	require.NoError(t, err)
	require.Len(t, channels, 2)

	dms, err := ex.DMs()
	require.NoError(t, err)
	require.Empty(t, dms)

	mpims, err := ex.MPIMs()
	require.NoError(t, err)
	require.Empty(t, mpims)

	messages, iterErr := collectMessages(ex, "general")
	require.NoError(t, iterErr)
	require.Len(t, messages, 3)
}

func TestMalformedJSON(t *testing.T) {
	files := fullFixtureFiles()
	files["channels.json"] = `{"broken": true`
	zipPath := writeFixtureZIP(t, files)

	ex, err := Open(zipPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, ex.Close()) }()

	_, err = ex.Channels()
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse channels.json")
}

func TestMalformedMessagesJSON(t *testing.T) {
	files := minimalFixtureFiles()
	files["general/2026-01-02.json"] = `{"not":"an array"}`
	dir := writeFixtureDir(t, files)

	ex, err := Open(dir)
	require.NoError(t, err)
	defer func() { require.NoError(t, ex.Close()) }()

	messages, iterErr := collectMessages(ex, "general")
	require.Error(t, iterErr)
	require.Contains(t, iterErr.Error(), "parse messages file")
	require.Len(t, messages, 2)
}

func TestMissingChannelDirectoryDoesNotError(t *testing.T) {
	dir := writeFixtureDir(t, minimalFixtureFiles())
	ex, err := Open(dir)
	require.NoError(t, err)
	defer func() { require.NoError(t, ex.Close()) }()

	messages, iterErr := collectMessages(ex, "does-not-exist")
	require.NoError(t, iterErr)
	require.Empty(t, messages)
}

func TestMessagesRejectsTraversalChannelName(t *testing.T) {
	root := t.TempDir()
	exportDir := filepath.Join(root, "export")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "leak"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "leak", "2026-01-01.json"), []byte(`[{"type":"message","text":"outside","ts":"1.0"}]`), 0o600))
	require.NoError(t, os.MkdirAll(exportDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(exportDir, "channels.json"), []byte(`[{"id":"C1","name":"general"}]`), 0o600))

	ex, err := Open(exportDir)
	require.NoError(t, err)
	defer func() { require.NoError(t, ex.Close()) }()

	messages, iterErr := collectMessages(ex, "../leak")
	require.Error(t, iterErr)
	require.Contains(t, iterErr.Error(), "invalid channel directory")
	require.Empty(t, messages)
}

func TestChannelsRejectTraversalNames(t *testing.T) {
	dir := writeFixtureDir(t, map[string]string{
		"channels.json": `[{"id":"C1","name":"../leak"}]`,
	})
	ex, err := Open(dir)
	require.NoError(t, err)
	defer func() { require.NoError(t, ex.Close()) }()

	_, err = ex.Channels()
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid channels.json name")
}

func collectMessages(ex *Export, channel string) ([]MessageEnvelope, error) {
	messages := []MessageEnvelope{}
	for env, err := range ex.Messages(channel) {
		if err != nil {
			return messages, err
		}
		messages = append(messages, env)
	}
	return messages, nil
}

func writeFixtureZIP(t *testing.T, files map[string]string) string {
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

	path := filepath.Join(t.TempDir(), "fixture.zip")
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
	return path
}

func writeFixtureDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		fullPath := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o750))
		require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o600))
	}
	return dir
}

func fullFixtureFiles() map[string]string {
	files := minimalFixtureFiles()
	files["groups.json"] = `[{"id":"G1","name":"secret","is_private":true}]`
	files["dms.json"] = `[{"id":"D1","name":"U2"}]`
	files["mpims.json"] = `[{"id":"GDM1","name":"alice-bob-group"}]`
	files["secret/2026-01-01.json"] = `[{"type":"message","user":"U1","text":"private","ts":"1735689600.000100"}]`
	files["D1/2026-01-01.json"] = `[{"type":"message","user":"U2","text":"dm","ts":"1735689600.000200"}]`
	files["GDM1/2026-01-01.json"] = `[{"type":"message","user":"U1","text":"mpim","ts":"1735689600.000300"}]`
	return files
}

func minimalFixtureFiles() map[string]string {
	return map[string]string{
		"users.json": `[
			{"id":"U1","name":"alice","real_name":"Alice Example","profile":{"display_name":"alice"}},
			{"id":"U2","name":"bob","real_name":"Bob Example","profile":{"display_name":"bob"}}
		]`,
		"channels.json": `[
			{"id":"C1","name":"general","is_private":false},
			{"id":"C2","name":"random","is_private":false}
		]`,
		"general/2026-01-01.json": strings.Join([]string{
			`[`,
			`{"type":"message","user":"U1","text":"hello <@U2|bob> in <#C2|random>","ts":"1735689600.000001","thread_ts":"1735689600.000001","reply_count":1,"latest_reply":"1735689660.000005"},`,
			`{"type":"message","user":"U2","text":"reply","ts":"1735689660.000005","thread_ts":"1735689600.000001","edited":{"user":"U2","ts":"1735689900.000010"}}`,
			`]`,
		}, "\n"),
		"general/2026-01-02.json": `[
			{"type":"message_deleted","user":"U1","text":"removed","ts":"1735776000.000002","deleted_ts":"1735777000.000001"}
		]`,
		"general/2026-01-03.json": `[]`,
		"random/2026-01-01.json": `[
			{"type":"message","user":"U1","text":"random-one","ts":"1735689601.000001"},
			{"type":"message","user":"U2","text":"random-two","ts":"1735689602.000001"}
		]`,
	}
}

func TestOpenRejectsUnknownFileType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.txt")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	_, err := Open(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported export path")
}

func TestOpenMissingPath(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "missing.zip"))
	require.Error(t, err)
	require.True(t, errors.Is(err, os.ErrNotExist))
}
