package app

import (
	"os"
	"path/filepath"
)

// Example media path for the Termux storage layout present on this device
func termuxExampleImagePath(filename string) string {
	if filename == "" {
		filename = "cat.png"
	}
	home, _ := os.UserHomeDir()
	for _, root := range []string{filepath.Join(home, "storage", "shared"), "/sdcard", "/storage/emulated/0", "/storage/self/primary"} {
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			return filepath.ToSlash(root) + "/Pictures/" + filename
		}
	}
	return "~/storage/shared/Pictures/" + filename
}
