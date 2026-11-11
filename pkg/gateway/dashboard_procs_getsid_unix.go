//go:build !windows

package gateway

import "golang.org/x/sys/unix"

// Session id of pid from the kernel
func procGetsid(pid int) (int, error) { return unix.Getsid(pid) }
