package pm

import (
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

// Spawns a console child without a window, the CREATE_NO_WINDOW creation flag
func hideWindow(cmd *exec.Cmd, hide bool) {
	if hide {
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000}
	}
}

var procCopyFile2 = syscall.NewLazyDLL("kernel32.dll").NewProc("CopyFile2")

// COPYFILE2_EXTENDED_PARAMETERS as CopyFile2 reads it
type copyFile2Params struct {
	Size            uint32
	CopyFlags       uint32
	Cancel          *int32
	ProgressRoutine uintptr
	CallbackContext uintptr
}

// Copies with CopyFile2 allowing a decrypted destination, falling back to copyfile and copystat on access denied
func platformCopy2(src, dst string) error {
	if procCopyFile2.Find() == nil {
		from, err := syscall.UTF16PtrFromString(src)
		if err != nil {
			return err
		}
		to, err := syscall.UTF16PtrFromString(dst)
		if err != nil {
			return err
		}
		params := copyFile2Params{CopyFlags: 0x8}
		params.Size = uint32(unsafe.Sizeof(params))
		hr, _, _ := procCopyFile2.Call(uintptr(unsafe.Pointer(from)), uintptr(unsafe.Pointer(to)), uintptr(unsafe.Pointer(&params)))
		if int32(hr) >= 0 {
			return nil
		}
		code := uint32(hr)
		if code&0xFFFF0000 == 0x80070000 {
			code &= 0xFFFF
		}
		if code != 5 {
			return syscall.Errno(code)
		}
	}
	if err := copyFileData(src, dst); err != nil {
		return err
	}
	return copyStat(src, dst)
}

// Last access time Windows records for a file
func fileAccessTime(path string, st os.FileInfo) (time.Time, error) {
	data, ok := st.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return time.Time{}, &os.PathError{Op: "stat", Path: path, Err: syscall.EINVAL}
	}
	return time.Unix(0, data.LastAccessTime.Nanoseconds()), nil
}
