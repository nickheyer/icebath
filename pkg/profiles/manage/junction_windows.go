//go:build windows

package manage

import (
	"encoding/binary"
	"os"
	"strings"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"golang.org/x/sys/windows"
)

// Target of an NTFS directory junction with the verbatim prefix removed, false for anything else
func junctionTarget(path string) (string, bool) {
	if pyfs.ReparseTag(path) != windows.IO_REPARSE_TAG_MOUNT_POINT {
		return "", false
	}
	target, err := os.Readlink(path)
	if err != nil {
		return "", false
	}
	if strings.HasPrefix(target, `\\?\UNC\`) {
		return `\` + target[7:], true
	}
	return strings.TrimPrefix(target, `\\?\`), true
}

// Creates link as a directory junction to target the way the winapi CreateJunction call does
func createJunction(target, link string) error {
	full, err := windows.FullPath(target)
	if err != nil {
		return err
	}
	sub, err := windows.UTF16FromString(`\??\` + full)
	if err != nil {
		return err
	}
	prt, err := windows.UTF16FromString(full)
	if err != nil {
		return err
	}
	subLen, prtLen := (len(sub)-1)*2, (len(prt)-1)*2
	pathBytes := len(sub)*2 + len(prt)*2
	buf := make([]byte, 16+pathBytes)
	binary.LittleEndian.PutUint32(buf[0:], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buf[4:], uint16(8+pathBytes))
	binary.LittleEndian.PutUint16(buf[8:], 0)
	binary.LittleEndian.PutUint16(buf[10:], uint16(subLen))
	binary.LittleEndian.PutUint16(buf[12:], uint16(subLen+2))
	binary.LittleEndian.PutUint16(buf[14:], uint16(prtLen))
	off := 16
	for _, u := range append(sub, prt...) {
		binary.LittleEndian.PutUint16(buf[off:], u)
		off += 2
	}
	linkPtr, err := windows.UTF16PtrFromString(link)
	if err != nil {
		return err
	}
	if err := windows.CreateDirectory(linkPtr, nil); err != nil {
		return &os.PathError{Op: "CreateJunction", Path: link, Err: err}
	}
	h, err := windows.CreateFile(linkPtr, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		windows.RemoveDirectory(linkPtr)
		return &os.PathError{Op: "CreateJunction", Path: link, Err: err}
	}
	defer windows.CloseHandle(h)
	var returned uint32
	if err := windows.DeviceIoControl(h, windows.FSCTL_SET_REPARSE_POINT, (*byte)(unsafe.Pointer(&buf[0])), uint32(len(buf)), nil, 0, &returned, nil); err != nil {
		windows.CloseHandle(h)
		windows.RemoveDirectory(linkPtr)
		return &os.PathError{Op: "CreateJunction", Path: link, Err: err}
	}
	return nil
}
