package importer

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/slack-go/slack"
)

type Export struct {
	fs     fs.FS
	closer io.Closer
}

type ChannelInfo struct {
	ID        string
	Name      string
	Kind      string
	IsPrivate bool
	RawJSON   []byte
}

type MessageEnvelope struct {
	Date string
	Raw  map[string]any
}

func Open(path string) (*Export, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return &Export{fs: os.DirFS(path)}, nil
	}
	if strings.EqualFold(filepath.Ext(path), ".zip") {
		reader, err := zip.OpenReader(path)
		if err != nil {
			return nil, err
		}
		return &Export{fs: reader, closer: reader}, nil
	}
	return nil, fmt.Errorf("unsupported export path %q: expected .zip file or directory", path)
}

func (e *Export) Close() error {
	if e == nil || e.closer == nil {
		return nil
	}
	return e.closer.Close()
}

func (e *Export) Users() ([]slack.User, error) {
	var users []slack.User
	_, err := e.readJSONOptional("users.json", &users)
	if err != nil {
		return nil, err
	}
	if users == nil {
		return []slack.User{}, nil
	}
	return users, nil
}

func (e *Export) Channels() ([]ChannelInfo, error) {
	channels, err := e.readChannelList("channels.json", "public", false)
	if err != nil {
		return nil, err
	}
	groups, err := e.readChannelList("groups.json", "private", true)
	if err != nil {
		return nil, err
	}
	out := make([]ChannelInfo, 0, len(channels)+len(groups))
	out = append(out, channels...)
	out = append(out, groups...)
	return out, nil
}

func (e *Export) DMs() ([]ChannelInfo, error) {
	return e.readChannelList("dms.json", "im", true)
}

func (e *Export) MPIMs() ([]ChannelInfo, error) {
	return e.readChannelList("mpims.json", "mpim", true)
}

func (e *Export) Messages(channelName string) iter.Seq2[MessageEnvelope, error] {
	return func(yield func(MessageEnvelope, error) bool) {
		if err := validateChannelDirName(channelName); err != nil {
			yield(MessageEnvelope{}, err)
			return
		}
		entries, err := fs.ReadDir(e.fs, channelName)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return
			}
			yield(MessageEnvelope{}, fmt.Errorf("read channel %q: %w", channelName, err))
			return
		}

		files := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if strings.HasSuffix(name, ".json") {
				files = append(files, name)
			}
		}
		sort.Strings(files)

		for _, name := range files {
			fullPath := path.Join(channelName, name)
			blob, err := fs.ReadFile(e.fs, fullPath)
			if err != nil {
				yield(MessageEnvelope{}, fmt.Errorf("read messages file %q: %w", fullPath, err))
				return
			}
			if len(bytes.TrimSpace(blob)) == 0 {
				continue
			}

			var rows []map[string]any
			if err := json.Unmarshal(blob, &rows); err != nil {
				yield(MessageEnvelope{}, fmt.Errorf("parse messages file %q: %w", fullPath, err))
				return
			}

			date := strings.TrimSuffix(name, ".json")
			for _, raw := range rows {
				if raw == nil {
					raw = map[string]any{}
				}
				if !yield(MessageEnvelope{Date: date, Raw: raw}, nil) {
					return
				}
			}
		}
	}
}

type channelRecord struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsPrivate bool   `json:"is_private"`
}

func (e *Export) readChannelList(fileName, kind string, defaultPrivate bool) ([]ChannelInfo, error) {
	var rows []json.RawMessage
	ok, err := e.readJSONOptional(fileName, &rows)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []ChannelInfo{}, nil
	}

	out := make([]ChannelInfo, 0, len(rows))
	for _, row := range rows {
		var rec channelRecord
		if err := json.Unmarshal(row, &rec); err != nil {
			return nil, fmt.Errorf("parse %s record: %w", fileName, err)
		}
		if rec.ID == "" {
			continue
		}
		if rec.Name == "" {
			rec.Name = rec.ID
		}
		if err := validateChannelDirName(rec.ID); err != nil {
			return nil, fmt.Errorf("invalid %s id %q: %w", fileName, rec.ID, err)
		}
		if err := validateChannelDirName(rec.Name); err != nil {
			return nil, fmt.Errorf("invalid %s name %q: %w", fileName, rec.Name, err)
		}
		isPrivate := defaultPrivate
		if rec.IsPrivate {
			isPrivate = true
		}
		out = append(out, ChannelInfo{
			ID:        rec.ID,
			Name:      rec.Name,
			Kind:      kind,
			IsPrivate: isPrivate,
			RawJSON:   append([]byte(nil), row...),
		})
	}
	return out, nil
}

func validateChannelDirName(name string) error {
	if name == "." || !fs.ValidPath(name) || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid channel directory %q", name)
	}
	return nil
}

func (e *Export) readJSONOptional(fileName string, out any) (bool, error) {
	blob, err := fs.ReadFile(e.fs, fileName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if err := json.Unmarshal(blob, out); err != nil {
		return false, fmt.Errorf("parse %s: %w", fileName, err)
	}
	return true, nil
}
