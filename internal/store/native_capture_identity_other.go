//go:build !darwin && !linux

package store

import "io/fs"

func nativePhysicalIdentity(fs.FileInfo) (NativeFileIdentity, bool) {
	return NativeFileIdentity{}, false
}
