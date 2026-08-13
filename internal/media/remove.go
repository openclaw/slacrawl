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
	root, target, _, err := verifiedFile(cacheDir, mediaPath)
	return root, target, err
}

// VerifiedFile returns the resolved on-disk path and FileInfo for mediaPath
// under base's media root, enforcing the containment invariant on the real
// filesystem: the root itself may be a symlink (resolved once), every
// directory component below it must be a real directory — never a symlink —
// and the target must be a regular file. This is the single owner of that
// walk; the share export/import paths and the cache read paths all use it.
func VerifiedFile(base, mediaPath string) (string, os.FileInfo, error) {
	_, target, info, err := verifiedFile(base, mediaPath)
	return target, info, err
}

func verifiedFile(base, mediaPath string) (string, string, os.FileInfo, error) {
	if strings.TrimSpace(base) == "" {
		return "", "", nil, errors.New("media base dir is required")
	}
	target, err := LocalPath(base, mediaPath)
	if err != nil {
		return "", "", nil, err
	}
	relative, err := filepath.Rel(filepath.Clean(filepath.Join(base, cacheSubdir)), target)
	if err != nil {
		return "", "", nil, err
	}
	root, err := ResolveRoot(filepath.Clean(filepath.Join(base, cacheSubdir)))
	if err != nil {
		return "", "", nil, err
	}
	target = filepath.Join(root, relative)
	current := root
	parts := splitPath(relative)
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", "", nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", "", nil, fmt.Errorf("unsafe media path component %q", current)
		}
	}
	info, err := os.Lstat(target)
	if err != nil {
		return "", "", nil, err
	}
	if !info.Mode().IsRegular() {
		return "", "", nil, fmt.Errorf("cached media %q is not a regular file", mediaPath)
	}
	return root, target, info, nil
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
