//go:build !windows

package pm

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Copies of the checkout lock descriptors placed so each lands at its own number in the takeover child
func checkoutCustody(root string, lock UpdateLockHooks) ([]*os.File, error) {
	if lock.CheckoutLockFDs == nil {
		return nil, nil
	}
	fds, err := lock.CheckoutLockFDs(root)
	if err != nil {
		return nil, nil
	}
	var files []*os.File
	for _, fd := range fds {
		if fd < 3 {
			continue
		}
		copied, err := unix.Dup(fd)
		if err != nil {
			for _, file := range files {
				if file != nil {
					file.Close()
				}
			}
			return nil, os.NewSyscallError("dup", err)
		}
		for len(files) < fd-2 {
			files = append(files, nil)
		}
		if files[fd-3] != nil {
			files[fd-3].Close()
		}
		files[fd-3] = os.NewFile(uintptr(copied), "checkout-lock")
	}
	return files, nil
}

// Creation attributes of a suspended child, absent off Windows where no job binds the takeover
func suspendedProcAttr() *syscall.SysProcAttr { return nil }
