//go:build windows

package nodehttp

import "syscall"

// libuv names of the Winsock errors a connect or read reports
var socketCodes = map[syscall.Errno]string{
	10061: "ECONNREFUSED", 10054: "ECONNRESET", 10060: "ETIMEDOUT", 10051: "ENETUNREACH", 10065: "EHOSTUNREACH",
	10053: "ECONNABORTED", 10049: "EADDRNOTAVAIL", 10048: "EADDRINUSE", 10050: "ENETDOWN", 10064: "EHOSTDOWN",
	109: "EPIPE",
}
