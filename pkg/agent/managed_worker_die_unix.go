//go:build !windows

package agent

import (
	"os"
	"syscall"
	"time"
)

// Ends this worker and its process group after the orphan grace, never a group this process does not lead
func dieWithOwner() {
	time.AfterFunc(orphanGraceSeconds*time.Second, func() {
		if syscall.Getpgrp() == os.Getpid() {
			_ = syscall.Kill(-os.Getpid(), syscall.SIGKILL)
		}
		os.Exit(1)
	})
}
