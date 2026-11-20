//go:build !windows

package cron

import "syscall"

var transientErrnos = []syscall.Errno{
	syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.ENETDOWN,
	syscall.ETIMEDOUT, syscall.EAGAIN,
}
