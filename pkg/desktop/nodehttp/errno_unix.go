//go:build !windows

package nodehttp

import "syscall"

// libuv names of the socket errors a connect or read reports
var socketCodes = map[syscall.Errno]string{
	syscall.ECONNREFUSED: "ECONNREFUSED", syscall.ECONNRESET: "ECONNRESET", syscall.ETIMEDOUT: "ETIMEDOUT",
	syscall.ENETUNREACH: "ENETUNREACH", syscall.EHOSTUNREACH: "EHOSTUNREACH", syscall.ECONNABORTED: "ECONNABORTED",
	syscall.EADDRNOTAVAIL: "EADDRNOTAVAIL", syscall.EADDRINUSE: "EADDRINUSE", syscall.ENETDOWN: "ENETDOWN",
	syscall.EPIPE: "EPIPE", syscall.EHOSTDOWN: "EHOSTDOWN", syscall.EPROTO: "EPROTO",
}
