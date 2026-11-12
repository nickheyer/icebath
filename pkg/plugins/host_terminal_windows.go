//go:build windows

package plugins

import (
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Hands the child inheritable copies of this process's standard handles for plugin CLI commands, NUL for a missing one
func attachTerminal(cmd *exec.Cmd) (spec string, release func(), err error) {
	var dups []windows.Handle
	release = func() {
		for _, h := range dups {
			_ = windows.CloseHandle(h)
		}
	}
	self := windows.CurrentProcess()
	values := make([]uint64, 3)
	for i, id := range []uint32{windows.STD_INPUT_HANDLE, windows.STD_OUTPUT_HANDLE, windows.STD_ERROR_HANDLE} {
		src, getErr := windows.GetStdHandle(id)
		if getErr != nil || src == 0 || src == windows.InvalidHandle {
			access := uint32(windows.GENERIC_WRITE)
			if i == 0 {
				access = windows.GENERIC_READ
			}
			null, openErr := windows.CreateFile(windows.StringToUTF16Ptr("NUL"), access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
				nil, windows.OPEN_EXISTING, 0, 0)
			if openErr != nil {
				release()
				return "", nil, openErr
			}
			dups = append(dups, null)
			src = null
		}
		var dup windows.Handle
		if err := windows.DuplicateHandle(self, src, self, &dup, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			release()
			return "", nil, err
		}
		dups = append(dups, dup)
		values[i] = uint64(dup)
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		cmd.SysProcAttr.AdditionalInheritedHandles = append(cmd.SysProcAttr.AdditionalInheritedHandles, syscall.Handle(dup))
	}
	return fmt.Sprintf("%d,%d,%d", values[0], values[1], values[2]), release, nil
}
