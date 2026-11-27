//go:build windows

package terminal

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Marks both pipe handles inheritable for this child alone and hands it their values
func attachPipe(cmd *exec.Cmd, replies, requests *os.File) (ChildPipe, error) {
	handles := []windows.Handle{windows.Handle(replies.Fd()), windows.Handle(requests.Fd())}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	for _, h := range handles {
		if err := windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return ChildPipe{}, err
		}
		cmd.SysProcAttr.AdditionalInheritedHandles = append(cmd.SysProcAttr.AdditionalInheritedHandles, syscall.Handle(h))
	}
	return ChildPipe{Replies: uintptr(handles[0]), Requests: uintptr(handles[1]), Handles: true}, nil
}
