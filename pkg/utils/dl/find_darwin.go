//go:build darwin

package dlutil

import (
	"os"
	"path/filepath"
	"sync"
)

// First dylib for the name in the dyld library paths, Homebrew and MacPorts, else its framework in the dyld framework paths, on disk or in the dyld shared cache
func findLibrary(name string) string {
	libDirs := dyldDirs("DYLD_LIBRARY_PATH", "DYLD_FALLBACK_LIBRARY_PATH", "lib", "/usr/local/lib", "/lib", "/usr/lib")
	libDirs = append(libDirs, "/opt/homebrew/lib", "/opt/local/lib")
	for _, file := range []string{"lib" + name + ".dylib", name + ".dylib"} {
		if path := firstPresent(libDirs, file); path != "" {
			return path
		}
	}
	frameworkDirs := dyldDirs("DYLD_FRAMEWORK_PATH", "DYLD_FALLBACK_FRAMEWORK_PATH", "Library/Frameworks",
		"/Library/Frameworks", "/Network/Library/Frameworks", "/System/Library/Frameworks")
	return firstPresent(frameworkDirs, filepath.Join(name+".framework", name))
}

// Directories of a dyld search variable, then of its fallback variable or, when that is unset, the home relative fallback and the system ones
func dyldDirs(key, fallbackKey, homeFallback string, systemFallback ...string) []string {
	dirs := envDirs(key)
	if fallback := envDirs(fallbackKey); len(fallback) > 0 {
		return append(dirs, fallback...)
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, homeFallback))
	}
	return append(dirs, systemFallback...)
}

// First dir/file that exists on disk or in the dyld shared cache, empty when none does
func firstPresent(dirs []string, file string) string {
	for _, dir := range dirs {
		if path := filepath.Join(dir, file); isFile(path) || inSharedCache(path) {
			return path
		}
	}
	return ""
}

// The dyld query macOS 11 and later answer for libraries that live only in the shared cache, nil where it is missing
var sharedCacheQuery = sync.OnceValue(func() func(path string) bool {
	addr, err := Lookup("_dyld_shared_cache_contains_path")
	if err != nil {
		return nil
	}
	var query func(path string) bool
	RegisterFunc(&query, addr)
	return query
})

// Whether the dyld shared cache holds a library path that has no file on disk
func inSharedCache(path string) bool {
	query := sharedCacheQuery()
	return query != nil && query(path)
}
