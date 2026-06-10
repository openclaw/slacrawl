package media

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ValidateCachedFile(cacheDir, mediaPath string) error {
	_, _, err := cachedRegularFile(cacheDir, mediaPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func RemoveCachedFile(cacheDir, mediaPath string) (bool, error) {
	root, target, err := cachedRegularFile(cacheDir, mediaPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := os.Remove(target); err != nil {
		return false, err
	}
	removeEmptyMediaParents(root, filepath.Dir(target))
	return true, nil
}

func cachedRegularFile(cacheDir, mediaPath string) (string, string, error) {
	if strings.TrimSpace(cacheDir) == "" {
		return "", "", errors.New("cache dir is required")
	}
	target, err := LocalPath(cacheDir, mediaPath)
	if err != nil {
		return "", "", err
	}
	root := filepath.Clean(filepath.Join(cacheDir, cacheSubdir))
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return "", "", err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", "", fmt.Errorf("unsafe media root %q", root)
	}
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return "", "", err
	}
	current := root
	parts := splitPath(relative)
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", "", fmt.Errorf("unsafe media path component %q", current)
		}
	}
	info, err := os.Lstat(target)
	if err != nil {
		return "", "", err
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("cached media %q is not a regular file", mediaPath)
	}
	return root, target, nil
}

func splitPath(path string) []string {
	volume := filepath.VolumeName(path)
	path = path[len(volume):]
	parts := []string{}
	for path != "." && path != string(filepath.Separator) && path != "" {
		dir, file := filepath.Split(path)
		if file != "" {
			parts = append([]string{file}, parts...)
		}
		path = filepath.Clean(dir)
	}
	return parts
}

func removeEmptyMediaParents(root, dir string) {
	for dir != root && dir != "." && dir != string(filepath.Separator) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
