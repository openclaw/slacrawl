package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/vincentkoc/slacrawl/internal/store"
)

const (
	DefaultMaxBytes int64 = 100 << 20
	cacheSubdir           = "media"
)

type FetchOptions struct {
	CacheDir     string
	List         store.FileListOptions
	MaxBytes     int64
	Force        bool
	Token        string
	HTTPClient   *http.Client
	Now          func() time.Time
	StatusUpdate bool
}

type FetchStats struct {
	Files   int   `json:"files"`
	Fetched int   `json:"fetched"`
	Reused  int   `json:"reused"`
	Skipped int   `json:"skipped"`
	Failed  int   `json:"failed"`
	Bytes   int64 `json:"bytes"`
}

func Fetch(ctx context.Context, s *store.Store, opts FetchOptions) (FetchStats, error) {
	if strings.TrimSpace(opts.CacheDir) == "" {
		return FetchStats{}, errors.New("cache dir is required")
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	list := opts.List
	limit := list.Limit
	missingOnly := list.MissingOnly
	if missingOnly || !opts.Force {
		list.MissingOnly = false
		list.Limit = 0
	}
	files, err := s.Files(ctx, list)
	if err != nil {
		return FetchStats{}, err
	}
	stats := FetchStats{}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if file.MediaPath != "" && (missingOnly || !opts.Force) {
			if mediaFileReusable(opts.CacheDir, file) {
				stats.Reused++
				continue
			}
		}
		if limit > 0 && stats.Files >= limit {
			break
		}
		stats.Files++
		result, err := fetchOne(ctx, opts, file)
		switch {
		case err != nil:
			stats.Failed++
			if opts.StatusUpdate {
				if err := s.UpdateFileFetchStatus(ctx, file.ChannelID, file.TS, file.FileID, opts.Now().UTC().Format(time.RFC3339Nano), "failed", clampError(err.Error())); err != nil {
					return stats, err
				}
			}
		case result.status == "skipped":
			stats.Skipped++
			if opts.StatusUpdate {
				if err := s.UpdateFileFetchStatus(ctx, file.ChannelID, file.TS, file.FileID, opts.Now().UTC().Format(time.RFC3339Nano), result.reason, ""); err != nil {
					return stats, err
				}
			}
		default:
			stats.Fetched++
			stats.Bytes += result.size
			if err := s.UpdateFileMedia(ctx, store.FileMediaUpdate{
				ChannelID:     file.ChannelID,
				TS:            file.TS,
				FileID:        file.FileID,
				MediaPath:     result.mediaPath,
				ContentSHA256: result.sha256,
				ContentSize:   result.size,
				FetchedAt:     opts.Now().UTC().Format(time.RFC3339Nano),
				FetchStatus:   "fetched",
			}); err != nil {
				return stats, err
			}
		}
	}
	return stats, nil
}

func mediaFileReusable(cacheDir string, file store.FileRow) bool {
	path, err := LocalPath(cacheDir, file.MediaPath)
	if err != nil {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if file.ContentSHA256 == "" {
		return true
	}
	current, err := fileSHA256(path)
	return err == nil && current == file.ContentSHA256
}

type fetchResult struct {
	status    string
	reason    string
	mediaPath string
	sha256    string
	size      int64
}

func fetchOne(ctx context.Context, opts FetchOptions, file store.FileRow) (fetchResult, error) {
	urls := candidateURLs(file)
	if len(urls) == 0 {
		return fetchResult{status: "skipped", reason: "no_url"}, nil
	}
	var lastErr error
	for _, url := range urls {
		result, err := fetchURL(ctx, opts, file, url)
		if err == nil || result.status == "skipped" {
			return result, err
		}
		lastErr = err
	}
	return fetchResult{}, lastErr
}

func candidateURLs(file store.FileRow) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, raw := range []string{file.URLPrivateDownload, file.URLPrivate} {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if _, ok := seen[raw]; ok {
			continue
		}
		seen[raw] = struct{}{}
		out = append(out, raw)
	}
	return out
}

func fetchURL(ctx context.Context, opts FetchOptions, file store.FileRow, url string) (fetchResult, error) {
	if err := validateFileURL(url, opts.Token != ""); err != nil {
		return fetchResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fetchResult{}, err
	}
	if opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+opts.Token)
	}
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return fetchResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fetchResult{}, fmt.Errorf("GET %s returned HTTP %d", url, resp.StatusCode)
	}
	if resp.ContentLength > opts.MaxBytes {
		return fetchResult{status: "skipped", reason: "too_large"}, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, opts.MaxBytes+1))
	if err != nil {
		return fetchResult{}, err
	}
	if int64(len(body)) > opts.MaxBytes {
		return fetchResult{status: "skipped", reason: "too_large"}, nil
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	mediaPath := fileMediaPath(hash, file.Name, file.Mimetype)
	target, err := LocalPath(opts.CacheDir, mediaPath)
	if err != nil {
		return fetchResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fetchResult{}, err
	}
	needsWrite, err := mediaTargetNeedsWrite(target, hash)
	if err != nil {
		return fetchResult{}, err
	}
	if needsWrite || opts.Force {
		tmp, err := os.CreateTemp(filepath.Dir(target), ".download-*")
		if err != nil {
			return fetchResult{}, err
		}
		tmpPath := tmp.Name()
		if _, err := tmp.Write(body); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return fetchResult{}, err
		}
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmpPath)
			return fetchResult{}, err
		}
		if info, err := os.Lstat(target); err == nil && !info.Mode().IsRegular() {
			_ = os.Remove(target)
		}
		if err := os.Rename(tmpPath, target); err != nil {
			_ = os.Remove(tmpPath)
			return fetchResult{}, err
		}
	}
	return fetchResult{mediaPath: mediaPath, sha256: hash, size: int64(len(body))}, nil
}

func validateFileURL(raw string, withToken bool) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("refusing non-https file URL")
	}
	host := strings.ToLower(parsed.Hostname())
	if !withToken {
		if isSlackFileHost(host) {
			return fmt.Errorf("Slack file URL requires a token")
		}
		return nil
	}
	if isSlackFileHost(host) {
		return nil
	}
	return fmt.Errorf("refusing to send Slack token to non-Slack file host %q", host)
}

func isSlackFileHost(host string) bool {
	switch {
	case host == "slack.com" || strings.HasSuffix(host, ".slack.com"):
		return true
	case host == "slack-files.com" || strings.HasSuffix(host, ".slack-files.com"):
		return true
	case host == "slack-edge.com" || strings.HasSuffix(host, ".slack-edge.com"):
		return true
	default:
		return false
	}
}

func mediaTargetNeedsWrite(target, hash string) (bool, error) {
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return true, nil
	}
	current, err := fileSHA256(target)
	if err != nil {
		return false, err
	}
	return current != hash, nil
}

func fileMediaPath(hash, filename, contentType string) string {
	name := safeFilename(filename)
	if name == "" {
		name = "file" + extensionForContentType(contentType)
	}
	name = truncateFilename(name, 190)
	return filepath.ToSlash(filepath.Join("files", hash[:2], hash+"-"+name))
}

func truncateFilename(name string, maxLen int) string {
	if len(name) <= maxLen {
		return name
	}
	ext := filepath.Ext(name)
	if len(ext) >= maxLen {
		return name[:maxLen]
	}
	baseLen := maxLen - len(ext)
	if baseLen <= 0 {
		return name[:maxLen]
	}
	base := strings.TrimRight(name[:baseLen], ".-")
	if base == "" {
		return strings.TrimLeft(ext, ".")
	}
	return base + ext
}

func extensionForContentType(contentType string) string {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil || mediaType == "" {
		return ""
	}
	exts, err := mime.ExtensionsByType(mediaType)
	if err != nil || len(exts) == 0 {
		return ""
	}
	return exts[0]
}

func safeFilename(raw string) string {
	raw = filepath.Base(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		case r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteByte('-')
		}
	}
	return strings.Trim(strings.TrimSpace(b.String()), ".-")
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // Callers pass confined cache paths.
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func LocalPath(cacheDir, mediaPath string) (string, error) {
	root := filepath.Clean(filepath.Join(cacheDir, cacheSubdir))
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(mediaPath)))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid media path %q", mediaPath)
	}
	full := filepath.Clean(filepath.Join(root, clean))
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", fmt.Errorf("media path escapes cache: %q", mediaPath)
	}
	return full, nil
}

func RepoPath(repoPath, mediaPath string) (string, error) {
	root := filepath.Clean(filepath.Join(repoPath, cacheSubdir))
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(mediaPath)))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid media path %q", mediaPath)
	}
	full := filepath.Clean(filepath.Join(root, clean))
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", fmt.Errorf("media path escapes repo: %q", mediaPath)
	}
	return full, nil
}

func clampError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= 512 {
		return message
	}
	return message[:512]
}
