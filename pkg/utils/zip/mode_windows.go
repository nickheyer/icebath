package ziputil

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// Mode the archive records for a file on disk, program files given the execute bits Windows does not keep
func fileMode(info fs.FileInfo) fs.FileMode {
	mode := info.Mode()
	if mode.IsRegular() {
		switch strings.ToLower(filepath.Ext(info.Name())) {
		case ".exe", ".bat", ".cmd", ".com":
			mode |= 0o111
		}
	}
	return mode
}
