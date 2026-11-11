//go:build !windows

package dashboard

import "syscall"

// Never follow a final link and never block on a FIFO
const desktopPluginOpenFlags = syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
