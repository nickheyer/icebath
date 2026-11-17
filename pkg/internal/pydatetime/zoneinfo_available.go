package pydatetime

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Adds the TZif files below dir the way os.walk visits them, skipping right and posix at the root
func walkZoneDir(root, dir string, valid map[string]bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		isLink := e.Type()&fs.ModeSymlink != 0
		isDir := e.IsDir()
		if isLink {
			st, err := os.Stat(path)
			isDir = err == nil && st.IsDir()
		}
		if isDir {
			if dir == root && (e.Name() == "right" || e.Name() == "posix") {
				continue
			}
			if !isLink {
				walkZoneDir(root, path, valid)
			}
			continue
		}
		key, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		key = filepath.ToSlash(key)
		if key == "" || valid[key] {
			continue
		}
		if zoneFileValid(path) {
			valid[key] = true
		}
	}
}

// Whether a file opens and starts with the TZif magic
func zoneFileValid(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 4)
	n, _ := io.ReadFull(f, head)
	return bytes.Equal(head[:n], []byte("TZif"))
}
