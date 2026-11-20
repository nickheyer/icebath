//go:build !windows && !icebath_core && !icebath_noextras

package selfupdate

import (
	"os"
	"syscall"
)

const noFollowFlag = syscall.O_NOFOLLOW

// Replaces this process image with the command, returning only on failure
func execSelf(exe string, argv []string) error { return syscall.Exec(exe, argv, os.Environ()) }
