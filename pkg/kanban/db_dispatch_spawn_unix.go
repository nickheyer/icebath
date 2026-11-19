//go:build !windows

package kanban

import "syscall"

// Process attributes starting the worker in its own session so it survives the dispatcher
func workerSpawnAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
