package fsutil

import "io/fs"

// Linux cannot change the mode of a symlink and has no file flags, so nothing is copied
func copyLinkModeAndFlags(fs.FileInfo, string) error { return nil }
