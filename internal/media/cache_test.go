package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/store"
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

func TestFetchStoresFileWhenMaxBytesIsUnbounded(t *testing.T) {
	ctx := context.Background()
	body := []byte("file bytes")
	st := seedFileStore(t, "https://files.example/file.png")
	defer func() { require.NoError(t, st.Close()) }()

	// math.MaxInt64 is the natural way to ask for "no cap". Incrementing it for
	// the over-limit probe wrapped negative, so io.LimitReader returned EOF
	// immediately and every attachment was recorded as a fetched empty file.
	stats, err := Fetch(ctx, st, FetchOptions{
		CacheDir: t.TempDir(),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return testHTTPResponse(r, body, int64(len(body))), nil
		})},
		MaxBytes:     math.MaxInt64,
		StatusUpdate: true,
	})
	require.NoError(t, err)
	require.Equal(t, 1, stats.Fetched)

	rows, err := st.Files(ctx, store.FileListOptions{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "fetched", rows[0].FetchStatus)
	require.Equal(t, int64(len(body)), rows[0].ContentSize, "the body must not be truncated to zero")
	sum := sha256.Sum256(body)
	require.Equal(t, hex.EncodeToString(sum[:]), rows[0].ContentSHA256)
}

func TestReadLimitDoesNotOverflow(t *testing.T) {
	require.Equal(t, int64(5), readLimit(4))
	require.Equal(t, int64(math.MaxInt64), readLimit(math.MaxInt64))
	require.Positive(t, readLimit(math.MaxInt64))
}

func TestFileMediaPathKeepsExtensionForNonLatinNames(t *testing.T) {
	hash := strings.Repeat("a", 64)
	// safeFilename strips non-ASCII runes, so "写真.png" survived only as "png"
	// and the cached file lost the extension anything content-types by.
	require.Equal(t, ".png", filepath.Ext(fileMediaPath(hash, "写真.png", "image/png")))
	require.Equal(t, ".pdf", filepath.Ext(fileMediaPath(hash, "отчёт.pdf", "application/pdf")))
	require.Equal(t, ".png", filepath.Ext(fileMediaPath(hash, "photo.png", "image/png")), "ASCII names keep working")
}

func TestClampErrorKeepsValidUTF8(t *testing.T) {
	clamped := clampError(strings.Repeat("é", 400))
	require.LessOrEqual(t, len(clamped), 512)
	require.True(t, utf8.ValidString(clamped), "a clamped error must not be cut mid-rune")
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

func TestFetchValidatesRedirectTargets(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		initial  string
		redirect string
		token    string
	}{
		{
			name:     "rejects https downgrade with token",
			initial:  "https://files.slack.com/file.png",
			redirect: "http://files.slack.com/file.png",
			token:    "xoxp-test",
		},
		{
			name:     "rejects non-slack host with token",
			initial:  "https://files.slack.com/file.png",
			redirect: "https://files.example/file.png",
			token:    "xoxp-test",
		},
		{
			name:     "rejects slack host without token",
			initial:  "https://files.example/file.png",
			redirect: "https://files.slack.com/file.png",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := seedFileStore(t, tc.initial)
			defer func() { require.NoError(t, st.Close()) }()

			requests := []string{}
			stats, err := Fetch(ctx, st, FetchOptions{
				CacheDir: t.TempDir(),
				Token:    tc.token,
				HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					requests = append(requests, r.URL.String())
					if r.URL.String() == tc.initial {
						return testHTTPRedirectResponse(r, tc.redirect), nil
					}
					return testHTTPResponse(r, []byte("redirected"), int64(len("redirected"))), nil
				})},
				StatusUpdate: true,
			})
			require.NoError(t, err)
			require.Equal(t, 1, stats.Failed)
			require.Equal(t, []string{tc.initial}, requests)
		})
	}
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

func testHTTPRedirectResponse(r *http.Request, location string) *http.Response {
	resp := testHTTPResponse(r, nil, 0)
	resp.StatusCode = http.StatusFound
	resp.Header.Set("Location", location)
	return resp
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
