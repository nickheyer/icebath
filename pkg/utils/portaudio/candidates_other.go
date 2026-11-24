//go:build !windows

package portaudioutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// PortAudio library names and paths in the order they are tried: sonames on ELF systems with the Termux prefix after them, the dyld paths, Homebrew, MacPorts and the executable's directory on macOS
func libraryCandidates() []string {
	if runtime.GOOS != "darwin" && runtime.GOOS != "ios" {
		out := []string{"libportaudio.so.2", "libportaudio.so"}
		if prefix := os.Getenv("PREFIX"); strings.Contains(prefix, "com.termux") {
			out = append(out, filepath.Join(prefix, "lib", "libportaudio.so"))
		}
		return out
	}
	var dirs []string
	for _, key := range []string{"DYLD_LIBRARY_PATH", "DYLD_FALLBACK_LIBRARY_PATH"} {
		for _, dir := range filepath.SplitList(os.Getenv(key)) {
			if dir != "" {
				dirs = append(dirs, dir)
			}
		}
	}
	if os.Getenv("DYLD_FALLBACK_LIBRARY_PATH") == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dirs = append(dirs, filepath.Join(home, "lib"))
		}
		dirs = append(dirs, "/usr/local/lib", "/lib", "/usr/lib")
	}
	dirs = append(dirs, "/opt/homebrew/lib", "/opt/local/lib")
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	out := make([]string, 0, 3*len(dirs))
	for _, dir := range dirs {
		for _, name := range []string{"libportaudio.dylib", "portaudio.dylib", "libportaudio.2.dylib"} {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out
}
