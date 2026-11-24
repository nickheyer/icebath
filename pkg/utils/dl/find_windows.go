//go:build windows

package dlutil

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// First name or name.dll file beside the executable, in the system directory, then in the PATH directories
func findLibrary(name string) string {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	if system, err := windows.GetSystemDirectory(); err == nil {
		dirs = append(dirs, system)
	}
	dirs = append(dirs, envDirs("PATH")...)
	files := []string{name}
	if !strings.EqualFold(filepath.Ext(name), ".dll") {
		files = append(files, name+".dll")
	}
	for _, dir := range dirs {
		for _, file := range files {
			if path := filepath.Join(dir, file); isFile(path) {
				return path
			}
		}
	}
	return ""
}
