package pypath

import (
	"io/fs"
	"unsafe"

	winapi "golang.org/x/sys/windows"
)

// os.unlink, RemoveDirectoryW for a directory symlink or junction and DeleteFileW for everything else
func Unlink(path string) error {
	p, err := winapi.UTF16PtrFromString(path)
	if err != nil {
		return &fs.PathError{Op: "unlink", Path: path, Err: err}
	}
	isDir, isLink := false, false
	var data winapi.Win32FileAttributeData
	if winapi.GetFileAttributesEx(p, winapi.GetFileExInfoStandard, (*byte)(unsafe.Pointer(&data))) == nil {
		isDir = data.FileAttributes&winapi.FILE_ATTRIBUTE_DIRECTORY != 0
		if isDir && data.FileAttributes&winapi.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			var find winapi.Win32finddata
			if h, ferr := winapi.FindFirstFile(p, &find); ferr == nil {
				isLink = find.Reserved0 == winapi.IO_REPARSE_TAG_SYMLINK || find.Reserved0 == winapi.IO_REPARSE_TAG_MOUNT_POINT
				winapi.FindClose(h)
			}
		}
	}
	if isDir && isLink {
		err = winapi.RemoveDirectory(p)
	} else {
		err = winapi.DeleteFile(p)
	}
	if err != nil {
		return &fs.PathError{Op: "unlink", Path: path, Err: err}
	}
	return nil
}
