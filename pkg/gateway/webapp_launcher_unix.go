//go:build !windows

package gateway

import "syscall"

// Open flag refusing to follow a symlink at the final path component
const noFollowFlag = syscall.O_NOFOLLOW
