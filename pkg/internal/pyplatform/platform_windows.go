//go:build windows

package pyplatform

import "os"

func system() string { return "Windows" }

// Native architecture the way CPython reads it, the WOW64 variable first
func machine() string {
	if arch := os.Getenv("PROCESSOR_ARCHITEW6432"); arch != "" {
		return arch
	}
	return os.Getenv("PROCESSOR_ARCHITECTURE")
}
