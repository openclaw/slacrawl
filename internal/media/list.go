package media

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type CachedFile struct {
	Path string
	Size int64
}

func ListCachedFiles(cacheDir string) ([]CachedFile, error) {
	if strings.TrimSpace(cacheDir) == "" {
		return nil, errors.New("cache dir is required")
	}
	root := filepath.Clean(filepath.Join(cacheDir, cacheSubdir))
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("unsafe media root %q", root)
	}

	var files []CachedFile
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, CachedFile{Path: filepath.ToSlash(relative), Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}
