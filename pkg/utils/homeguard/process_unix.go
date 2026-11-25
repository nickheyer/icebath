//go:build !windows

package homeguard

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

// Whether pid names a live process, a process of another user counting as live
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Inode of the pid namespace of this process, zero where the platform has none
func pidNamespace() string {
	link, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return "0"
	}
	inode := strings.TrimSuffix(strings.TrimPrefix(link, "pid:["), "]")
	if inode == "" || strings.ContainsAny(inode, "-/[]:") {
		return "0"
	}
	return inode
}
