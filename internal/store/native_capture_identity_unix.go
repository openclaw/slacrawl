//go:build darwin || linux

package store

import (
	"io/fs"
	"runtime"
	"strconv"
	"syscall"
)

func nativePhysicalIdentity(info fs.FileInfo) (NativeFileIdentity, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return NativeFileIdentity{}, false
	}
	return NativeFileIdentity{Platform: runtime.GOOS, VolumeID: strconv.FormatUint(uint64(stat.Dev), 10), FileID: strconv.FormatUint(uint64(stat.Ino), 10)}, true
}
