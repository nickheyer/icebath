//go:build windows

package app

import "golang.org/x/sys/windows"

// Switches the console input and output code pages to UTF-8, failures without a console ignored
func flipConsoleCodePageToUTF8() {
	_ = windows.SetConsoleCP(65001)
	_ = windows.SetConsoleOutputCP(65001)
}
