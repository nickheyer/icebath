//go:build !windows

package camofox

import "syscall"

// O_NOFOLLOW so a planted symlink is never written through
const openNoFollow = syscall.O_NOFOLLOW
