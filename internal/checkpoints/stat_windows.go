package checkpoints

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Volume serial and file index of the path, what stands for its device and inode
func identity(path string) (uint64, uint64, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	handle, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, 0, err
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return 0, 0, err
	}
	return uint64(info.VolumeSerialNumber), uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), nil
}

// Whether a failed stat says the path is not there, any other failure being no evidence of that
func missing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) || errors.Is(err, windows.ERROR_INVALID_NAME)
}

// Whether the folder is a drive root, a share root or a volume mounted on a folder
func mount(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	volume := filepath.VolumeName(abs)
	rest := strings.TrimLeft(abs[len(volume):], `\/`)
	if volume != "" && (volume[0] == '\\' || volume[0] == '/') || rest == "" {
		return rest == ""
	}
	name, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return false
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	if err := windows.GetVolumePathName(name, &buf[0], uint32(len(buf))); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimRight(abs, `\/`), strings.TrimRight(windows.UTF16ToString(buf), `\/`))
}
