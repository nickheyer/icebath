package toolchain

import "golang.org/x/sys/windows"

// Connection failures a later attempt can get past
var connectionErrnos = []error{
	windows.WSAECONNABORTED, windows.WSAECONNREFUSED, windows.WSAECONNRESET, windows.WSAENETDOWN, windows.WSAENETRESET,
	windows.WSAENETUNREACH, windows.WSAEHOSTUNREACH, windows.WSAETIMEDOUT, windows.ERROR_BROKEN_PIPE,
}

// Failures that mean no route leaves this machine
var unreachableErrnos = []error{windows.WSAENETDOWN, windows.WSAENETUNREACH, windows.WSAEHOSTUNREACH}
