//go:build !windows

package tools

import "syscall"

// Exclusive spill creates never follow a planted symlink on POSIX
const spillNoFollowFlag = syscall.O_NOFOLLOW
