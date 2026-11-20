//go:build !windows

package pm

import "syscall"

const noCTTY = syscall.O_NOCTTY
