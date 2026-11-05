//go:build windows

package cron

import "syscall"

// Winsock codes CPython binds to the errno names on Windows, EAGAIN keeps the C runtime value
var transientErrnos = []syscall.Errno{10061, 10054, 10065, 10051, 10050, 10060, 11}
