//go:build (darwin || freebsd || linux || netbsd || windows) && !386 && !arm

package dl

import "path/filepath"

// Absolute forms of search list directories, relative ones resolved against the working directory as the loader does
func absDirs(dirs []string) []string {
	var out []string
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if abs, err := filepath.Abs(dir); err == nil {
			out = append(out, abs)
		}
	}
	return out
}
