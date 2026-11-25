package browser

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// Process attributes of a browser started without this console and outside its process group
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}
