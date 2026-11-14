//go:build unix

package toolchain

import "syscall"

// Connection failures a later attempt can get past
var connectionErrnos = []error{
	syscall.ECONNABORTED, syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ENETDOWN, syscall.ENETRESET,
	syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.EPIPE, syscall.ETIMEDOUT,
}

// Failures that mean no route leaves this machine
var unreachableErrnos = []error{syscall.ENETDOWN, syscall.ENETUNREACH, syscall.EHOSTUNREACH}
