//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Downloads folder, the XDG download directory on Linux and Downloads in the home folder otherwise
func downloadsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if runtime.GOOS != "darwin" {
		if dir := xdgUserDir(home, "DOWNLOAD"); dir != "" {
			if trimmed := strings.TrimRight(dir, "/"); trimmed != "" {
				return trimmed
			}
			return "/"
		}
	}
	return filepath.Join(home, "Downloads")
}
