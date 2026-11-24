//go:build windows

package portaudioutil

import (
	"strings"

	"golang.org/x/sys/windows"
)

// CP_ACP, the system ANSI code page
const codePageANSI = 0

// Device name bytes that are not UTF-8: the ANSI code page for the MME, DirectSound and ASIO host APIs, else UTF-8 with the invalid bytes replaced
func deviceName(raw string, host int32) string {
	if host == paHostApiTypeIdToHostApiIndex(paMME) || host == paHostApiTypeIdToHostApiIndex(paDirectSound) ||
		host == paHostApiTypeIdToHostApiIndex(paASIO) {
		if name, ok := ansiString(raw); ok {
			return name
		}
	}
	return strings.ToValidUTF8(raw, "�")
}

// Text of bytes in the ANSI code page as Windows itself converts them, false when the conversion fails
func ansiString(raw string) (string, bool) {
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
