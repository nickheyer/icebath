//go:build windows

package files

import "io/fs"

// True for a symbolic link or a junction, which Go reports as an irregular reparse point
func isLinkMode(m fs.FileMode) bool { return m&(fs.ModeSymlink|fs.ModeIrregular) != 0 }
