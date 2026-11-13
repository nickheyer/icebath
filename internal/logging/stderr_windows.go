//go:build windows

package logging

import (
	"os"

	"golang.org/x/sys/windows"
)

// UTF-8 code page identifier a Windows process may run its ANSI code page as
const utf8CodePage = 65001

// Reports whether stderr is a console, which takes UTF-8, or a redirect while the ANSI code page is UTF-8
func stderrIsUTF8(f *os.File) bool {
	var mode uint32
	if windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil {
		return true
	}
	return windows.GetACP() == utf8CodePage
}
