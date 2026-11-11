//go:build !windows

package kanban

import "syscall"

// Process attributes starting the worker in its own session so it survives the dispatcher
func workerSpawnAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// Whether the path is executable by this process, os.access with X_OK
func executableAccess(path string) bool { return syscall.Access(path, 0x1) == nil }
