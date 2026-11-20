//go:build windows

package gateway

import (
	"os"

	"golang.org/x/sys/windows"
)

// Hard link count os.lstat reports for a path, a final reparse point not followed
func lstatNlink(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return 0, &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	return uint64(info.NumberOfLinks), nil
}
