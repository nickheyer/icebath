package home

import (
	"io/fs"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Final path of an existing location with every symlink and junction followed, an error when any part is missing
func realPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	p, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return "", &fs.PathError{Op: "realpath", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", &fs.PathError{Op: "realpath", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	final, err := finalPath(h)
	if err != nil {
		return "", &fs.PathError{Op: "realpath", Path: path, Err: err}
	}
	return final, nil
}

// Final path the handle resolves to, without the long path prefix
func finalPath(h windows.Handle) (string, error) {
	n := uint32(windows.MAX_PATH)
	for {
		buf := make([]uint16, n)
		got, err := windows.GetFinalPathNameByHandle(h, &buf[0], n, 0)
		if err != nil {
			return "", err
		}
		if got < n {
			p := windows.UTF16ToString(buf[:got])
			if rest, ok := strings.CutPrefix(p, `\\?\UNC\`); ok {
				return `\\` + rest, nil
			}
			return strings.TrimPrefix(p, `\\?\`), nil
		}
		n = got
	}
}
