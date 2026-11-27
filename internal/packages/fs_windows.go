package packages

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// Whether the path is a mount point junction
func junction(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var info struct{ attributes, tag uint32 }
	if err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return false
	}
	return info.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 && info.tag == windows.IO_REPARSE_TAG_MOUNT_POINT
}
