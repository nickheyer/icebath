//go:build !unix && !windows

package browserutil

import "syscall"

// No detaching on systems without sessions or process groups
func detachAttr() *syscall.SysProcAttr { return nil }
