//go:build windows

package portaudio

import (
	"strings"

	"golang.org/x/sys/windows"
)

// C long and unsigned long, 32 bits on Windows
type (
	clong  = int32
	culong = uint32
)

// CP_ACP, the system ANSI code page
const codePageANSI = 0

// Name bytes that are not UTF-8: the ANSI code page for MME, DirectSound and ASIO, else UTF-8 with the invalid bytes replaced
func deviceName(raw string, host int32) string {
	if host == paHostApiTypeIdToHostApiIndex(paMME) || host == paHostApiTypeIdToHostApiIndex(paDirectSound) ||
		host == paHostApiTypeIdToHostApiIndex(paASIO) {
		if name, ok := ansi(raw); ok {
			return name
		}
	}
	return strings.ToValidUTF8(raw, "�")
}

// Text of ANSI code page bytes as Windows converts them
func ansi(raw string) (string, bool) {
	src := []byte(raw)
	if len(src) == 0 || len(src) > 1<<30 {
		return "", false
	}
	n, err := windows.MultiByteToWideChar(codePageANSI, 0, &src[0], int32(len(src)), nil, 0)
	if err != nil || n <= 0 {
		return "", false
	}
	wide := make([]uint16, n)
	if n, err = windows.MultiByteToWideChar(codePageANSI, 0, &src[0], int32(len(src)), &wide[0], n); err != nil || n <= 0 {
		return "", false
	}
	return windows.UTF16ToString(wide[:n]), true
}
