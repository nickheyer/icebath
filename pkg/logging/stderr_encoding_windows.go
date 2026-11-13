//go:build windows

package logging

import (
	"os"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
)

// utf-8 for a console stderr like the Windows console stream, the ANSI code page once redirected
func platformStderrEncoding() string {
	var mode uint32
	if windows.GetConsoleMode(windows.Handle(os.Stderr.Fd()), &mode) == nil {
		return "utf-8"
	}
	return pycodecs.LocaleEncoding()
}
