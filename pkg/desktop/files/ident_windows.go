//go:build windows

package files

import (
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// identityInfo is a stat answer that states its own device and inode
type identityInfo interface {
	Identity() (dev, ino uint64)
}

func handleInfo(p string) (windows.ByHandleFileInformation, bool) {
	var info windows.ByHandleFileInformation
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return info, false
	}
	h, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return info, false
	}
	defer windows.CloseHandle(h)
	if windows.GetFileInformationByHandle(h, &info) != nil {
		return info, false
	}
	return info, true
}

// Volume serial and file index of the path, the dev and ino Node reports on Windows
func fileIdentity(st fs.FileInfo, p string) (uint64, uint64, bool) {
	if id, ok := st.(identityInfo); ok {
		dev, ino := id.Identity()
		return dev, ino, true
	}
	info, ok := handleInfo(p)
	if !ok {
		return 0, 0, false
	}
	return uint64(info.VolumeSerialNumber), uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), true
}

// Hard link count of the path
func fileLinks(_ fs.FileInfo, p string) uint64 {
	info, ok := handleInfo(p)
	if !ok {
		return 1
	}
	return uint64(info.NumberOfLinks)
}

// Opens the file, Node has no no follow or non blocking flag on Windows
func openNoFollow(p string, write bool) (*os.File, error) {
	if write {
		return os.OpenFile(p, os.O_RDWR, 0)
	}
	return os.Open(p)
}
