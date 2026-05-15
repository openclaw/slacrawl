package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/vincentkoc/slacrawl/internal/store"
)

func TestFetchStoresFileMediaWithToken(t *testing.T) {
	ctx := context.Background()
	body := []byte("file bytes")
	st := seedFileStore(t, "https://files.slack.com/file.png")
	defer func() { require.NoError(t, st.Close()) }()

	now := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	stats, err := Fetch(ctx, st, FetchOptions{
		CacheDir: t.TempDir(),
		Token:    "xoxp-test",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			require.Equal(t, "Bearer xoxp-test", r.Header.Get("Authorization"))
			return testHTTPResponse(r, body, int64(len(body))), nil
		})},
		StatusUpdate: true,
		Now:          func() time.Time { return now },
	})
	require.NoError(t, err)
	require.Equal(t, 1, stats.Files)
	require.Equal(t, 1, stats.Fetched)

	rows, err := st.Files(ctx, store.FileListOptions{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotEmpty(t, rows[0].MediaPath)
	require.Equal(t, "fetched", rows[0].FetchStatus)
	sum := sha256.Sum256(body)
	require.Equal(t, hex.EncodeToString(sum[:]), rows[0].ContentSHA256)
}

func TestFetchSkipsTooLargeFile(t *testing.T) {
	ctx := context.Background()
	st := seedFileStore(t, "https://files.example/large.bin")
	defer func() { require.NoError(t, st.Close()) }()

	stats, err := Fetch(ctx, st, FetchOptions{
		CacheDir: t.TempDir(),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return testHTTPResponse(r, []byte("too-large"), 10), nil
		})},
		MaxBytes:     4,
		StatusUpdate: true,
	})
	require.NoError(t, err)
	require.Equal(t, 1, stats.Skipped)

	rows, err := st.Files(ctx, store.FileListOptions{})
	require.NoError(t, err)
	require.Equal(t, "too_large", rows[0].FetchStatus)
}

func TestFetchRefusesTokenOnNonSlackURL(t *testing.T) {
	ctx := context.Background()
	st := seedFileStore(t, "https://files.example/file.png")
	defer func() { require.NoError(t, st.Close()) }()

	stats, err := Fetch(ctx, st, FetchOptions{
		CacheDir: t.TempDir(),
		Token:    "xoxp-test",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			t.Fatal("unexpected HTTP request")
			return nil, nil
		})},
		StatusUpdate: true,
	})
	require.NoError(t, err)
	require.Equal(t, 1, stats.Failed)
}

func TestFetchRequiresTokenForSlackURL(t *testing.T) {
	ctx := context.Background()
	st := seedFileStore(t, "https://files.slack.com/file.png")
	defer func() { require.NoError(t, st.Close()) }()

	stats, err := Fetch(ctx, st, FetchOptions{
		CacheDir: t.TempDir(),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			t.Fatal("unexpected HTTP request")
			return nil, nil
		})},
		StatusUpdate: true,
	})
	require.NoError(t, err)
	require.Equal(t, 1, stats.Failed)
}

func TestLocalAndRepoPathRejectEscapes(t *testing.T) {
	_, err := LocalPath(t.TempDir(), "../escape")
	require.Error(t, err)
	_, err = RepoPath(t.TempDir(), "/absolute")
	require.Error(t, err)
}

func TestSafeFilenameTruncatesLongNames(t *testing.T) {
	hash := strings.Repeat("a", 64)
	path := fileMediaPath(hash, strings.Repeat("x", 260)+".png", "image/png")
	require.True(t, strings.HasPrefix(path, "files/aa/"))
	require.LessOrEqual(t, len(filepath.Base(path)), 255)
	require.True(t, strings.HasSuffix(filepath.Base(path), ".png"))
}

func seedFileStore(t *testing.T, url string) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	now := time.Date(2026, 5, 15, 9, 0, 0, 0, time.UTC)
	require.NoError(t, st.UpsertMessage(context.Background(), store.Message{
		WorkspaceID:    "T1",
		ChannelID:      "C1",
		TS:             "1710000000.000100",
		UserID:         "U1",
		Text:           "file",
		NormalizedText: "file",
		SourceRank:     2,
		SourceName:     "api-bot",
		RawJSON:        "{}",
		UpdatedAt:      now,
		Files: []store.MessageFile{{
			FileID:             "F1",
			Name:               "file.png",
			Mimetype:           "image/png",
			URLPrivateDownload: url,
			RawJSON:            "{}",
		}},
	}, nil))
	return st
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

func testHTTPResponse(r *http.Request, body []byte, contentLength int64) *http.Response {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        make(http.Header),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: contentLength,
		Request:       r,
	}
}
