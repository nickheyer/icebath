//go:build darwin

package native

import (
	"os"
	"path/filepath"
	"sync"
)

// First dylib of the name in the dyld library paths, Homebrew and MacPorts, else its framework, on disk or in the dyld shared cache
func find(name string) string {
	libs := dyldDirs("DYLD_LIBRARY_PATH", "DYLD_FALLBACK_LIBRARY_PATH", "lib", "/usr/local/lib", "/lib", "/usr/lib")
	libs = append(libs, "/opt/homebrew/lib", "/opt/local/lib")
	for _, file := range []string{"lib" + name + ".dylib", name + ".dylib"} {
		if path := present(libs, file); path != "" {
			return path
		}
	}
	frameworks := dyldDirs("DYLD_FRAMEWORK_PATH", "DYLD_FALLBACK_FRAMEWORK_PATH", "Library/Frameworks", "/Library/Frameworks", "/Network/Library/Frameworks",
		"/System/Library/Frameworks")
	return present(frameworks, filepath.Join(name+".framework", name))
}

// Directories of a dyld search variable, then of its fallback, else the home relative fallback and the system ones
func dyldDirs(key, fallbackKey, home string, system ...string) []string {
	dirs := envDirs(key)
	if fallback := envDirs(fallbackKey); len(fallback) > 0 {
		return append(dirs, fallback...)
	}
	if dir, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(dir, home))
	}
	return append(dirs, system...)
}

func present(dirs []string, file string) string {
	for _, dir := range dirs {
		if path := filepath.Join(dir, file); isFile(path) || inSharedCache(path) {
			return path
		}
	}
	return ""
}

// The dyld query of macOS 11 and later for libraries that live only in the shared cache, nil where it is missing
var sharedCache = sync.OnceValue(func() func(path string) bool {
	addr, err := Lookup("_dyld_shared_cache_contains_path")
	if err != nil {
		return nil
	}
	var query func(path string) bool
	RegisterFunc(&query, addr)
	return query
})

func inSharedCache(path string) bool {
	query := sharedCache()
	return query != nil && query(path)
}
