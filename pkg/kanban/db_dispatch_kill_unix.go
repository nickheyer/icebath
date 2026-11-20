//go:build unix

package kanban

import "syscall"

const sigkillSignal = syscall.SIGKILL

// Sends sig to pid as os.kill does on POSIX
func osKill(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }
