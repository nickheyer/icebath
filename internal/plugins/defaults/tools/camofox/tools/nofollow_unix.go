//go:build !windows

package tools

import "syscall"

// Never writes through a planted link
const noFollow = syscall.O_NOFOLLOW
