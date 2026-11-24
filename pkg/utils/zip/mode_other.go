//go:build !windows

package ziputil

import "io/fs"

// Mode the archive records for a file on disk
func fileMode(info fs.FileInfo) fs.FileMode { return info.Mode() }
