package dashboard

import (
	"bytes"
	"io/fs"
	"os"
	"sync"
	"time"
)

// Stat of a compiled in file whose modification time is the one of the running binary
type compiledFileInfo struct {
	fs.FileInfo
	modified time.Time
}

func (i compiledFileInfo) ModTime() time.Time { return i.modified }

var binaryModified = sync.OnceValue(func() time.Time {
	exe, err := os.Executable()
	if err != nil {
		return time.Unix(0, 0)
	}
	st, err := os.Stat(exe)
	if err != nil {
		return time.Unix(0, 0)
	}
	return st.ModTime()
})

// File response over a regular file of a file system, the same headers and ranges as one over a path
func NewFSFileResponse(fsys fs.FS, name, path string, status int, mediaType string, headers ...string) (*FileResponse, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	info, err := fs.Stat(fsys, name)
	if err != nil {
		return nil, err
	}
	if info.ModTime().IsZero() {
		info = compiledFileInfo{info, binaryModified()}
	}
	r := NewFileResponse(path, status, mediaType, nil, "", headers...)
	r.pinned, r.stat = bytes.NewReader(data), info
	r.setStatHeaders(info)
	return r, nil
}
