//go:build !windows

package app

// Console code pages exist only on Windows so nothing changes here
func flipConsoleCodePageToUTF8() {}
