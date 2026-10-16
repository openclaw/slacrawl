package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/config"
)

type notifyingReader struct {
	io.Reader
	started chan struct{}
}

func (r notifyingReader) Read(p []byte) (int, error) {
	select {
	case r.started <- struct{}{}:
	default:
	}
	return r.Reader.Read(p)
}

func TestSQLStdinCancellationDuringRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	started := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := readAllContext(ctx, notifyingReader{Reader: reader, started: started})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("stdin read did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not interrupt a blocked stdin read")
	}
}

func TestSQLStdinReturnsWhenContextIsCanceled(t *testing.T) {
	t.Setenv("SLACRAWL_NO_UPDATE_CHECK", "1")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	cfg := config.Default()
	cfg.DBPath = filepath.Join(dir, "slacrawl.db")
	cfg.CacheDir = filepath.Join(dir, "cache")
	cfg.Share.Remote = ""
	cfg.Share.AutoUpdate = false
	cfg.Slack.Bot.Enabled = false
	cfg.Slack.App.Enabled = false
	cfg.Slack.User.Enabled = false
	cfg.Slack.Desktop.Enabled = false
	require.NoError(t, cfg.Save(configPath))

	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	previous := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() { os.Stdin = previous })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	done := make(chan error, 1)
	go func() {
		done <- app.Run(ctx, []string{"--config", configPath, "sql"})
	}()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("sql kept reading stdin after the context was canceled")
	}
}

func TestSQLStdinReadsQueryWhenContextStaysOpen(t *testing.T) {
	t.Setenv("SLACRAWL_NO_UPDATE_CHECK", "1")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	dbPath := filepath.Join(dir, "slacrawl.db")
	cfg := config.Default()
	cfg.DBPath = dbPath
	cfg.CacheDir = filepath.Join(dir, "cache")
	cfg.Share.Remote = ""
	cfg.Share.AutoUpdate = false
	cfg.Slack.Bot.Enabled = false
	cfg.Slack.App.Enabled = false
	cfg.Slack.User.Enabled = false
	cfg.Slack.Desktop.Enabled = false
	require.NoError(t, cfg.Save(configPath))
	seedArchiveStore(t, dbPath, "stdin seed")

	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	previous := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() { os.Stdin = previous })
	_, err = writer.WriteString("select count(*) as n from messages\n")
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	var stdout bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stdout}
	require.NoError(t, app.Run(context.Background(), []string{"--config", configPath, "--json", "sql"}))
	require.Contains(t, stdout.String(), `"n"`)
}
