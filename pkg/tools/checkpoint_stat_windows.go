//go:build windows

package tools

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Volume serial and file index of the path, the pair Python reports as st_dev and st_ino
func checkpointDevIno(path string) (uint64, uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, 0, err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return 0, 0, err
	}
	return uint64(info.VolumeSerialNumber), uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), nil
}

// Reports a drive root, a UNC share root or a volume mount point the way ntpath.ismount does
func checkpointIsMount(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	vol := filepath.VolumeName(abs)
	rest := strings.TrimLeft(abs[len(vol):], `\/`)
	hasRoot := len(abs) > len(vol)
	if vol != "" && (vol[0] == '\\' || vol[0] == '/') {
		return rest == ""
	}
	if hasRoot && rest == "" {
		return true
	}
	p, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return false
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	if err := windows.GetVolumePathName(p, &buf[0], uint32(len(buf))); err != nil {
		return false
	}
	x := strings.TrimRight(abs, `\/`)
	y := strings.TrimRight(windows.UTF16ToString(buf), `\/`)
	return strings.EqualFold(x, y)
}
