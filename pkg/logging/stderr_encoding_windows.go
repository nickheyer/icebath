//go:build windows

package logging

import (
	"os"

	charsetutil "github.com/nickheyer/icebath/pkg/utils/charset"
	"golang.org/x/sys/windows"
)

// utf-8 for a console stderr like the Windows console stream, the ANSI code page once redirected
func platformStderrEncoding() string {
	var mode uint32
	if windows.GetConsoleMode(windows.Handle(os.Stderr.Fd()), &mode) == nil {
		return "utf-8"
	}
	return charsetutil.Locale()
}
