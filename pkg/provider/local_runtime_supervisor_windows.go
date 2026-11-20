//go:build windows

package provider

import (
	"io"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows listeners carry no address reuse, the same as a plain Python bind
var lrBindControl func(network, address string, c syscall.RawConn) error

// Every live pid from a toolhelp process snapshot
func lrAllPids() []int {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var out []int
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
		out = append(out, int(entry.ProcessID))
	}
	return out
}

// Non blocking lock of the first byte of the boot lock file, as msvcrt.locking LK_NBLCK takes it
func lrTryLockBootFile(f *os.File) bool {
	if st, err := f.Stat(); err == nil && st.Size() == 0 {
		_, _ = f.Write([]byte{0})
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false
	}
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol) == nil
}

func lrUnlockBootFile(f *os.File) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}
