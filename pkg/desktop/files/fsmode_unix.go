//go:build !windows

package files

import "io/fs"

// True for a symbolic link
func isLinkMode(m fs.FileMode) bool { return m&fs.ModeSymlink != 0 }
