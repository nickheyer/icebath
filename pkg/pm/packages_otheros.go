//go:build !linux && !darwin && !freebsd

package pm

import "os"

// Python exposes no xattr calls on this platform, so copystat copies none
func copyXattrs(src, dst string) error { return nil }

// This platform has no BSD file flags for copystat to copy
func copyFlags(dst string, st os.FileInfo) error { return nil }
