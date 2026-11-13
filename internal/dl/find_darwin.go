package dl

import (
	"debug/macho"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Absolute path of an installed library or framework given its short name such as opus, in dyld search order
func Find(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty library name: %w", ErrNotFound)
	}
	var inSharedCache func(path string) bool
	if err := Self().Bind(&inSharedCache, "_dyld_shared_cache_contains_path"); err != nil {
		return "", fmt.Errorf("find %s: %w", name, err)
	}
	for _, candidate := range dyldCandidates(name) {
		if loadable(candidate) || inSharedCache(candidate) {
			abs, err := filepath.Abs(candidate)
			if err != nil {
				return "", fmt.Errorf("find %s: %w", name, err)
			}
			return abs, nil
		}
	}
	return "", fmt.Errorf("%s: %w", name, ErrNotFound)
}

// Paths dyld tries for the dylib and framework forms of a name, then the Homebrew and MacPorts library directories
func dyldCandidates(name string) []string {
	defaultFrameworks := []string{"/Library/Frameworks", "/Network/Library/Frameworks", "/System/Library/Frameworks"}
	defaultLibraries := []string{"/usr/local/lib", "/lib", "/usr/lib"}
	if home, err := os.UserHomeDir(); err == nil {
		defaultFrameworks = append([]string{filepath.Join(home, "Library", "Frameworks")}, defaultFrameworks...)
		defaultLibraries = append([]string{filepath.Join(home, "lib")}, defaultLibraries...)
	}
	libraryPath, frameworkPath := dyldDirs("DYLD_LIBRARY_PATH"), dyldDirs("DYLD_FRAMEWORK_PATH")
	fallbackLibraries, librarySet := os.LookupEnv("DYLD_FALLBACK_LIBRARY_PATH")
	fallbackFrameworks, frameworkSet := os.LookupEnv("DYLD_FALLBACK_FRAMEWORK_PATH")
	var out []string
	for _, form := range []struct {
		file      string
		framework bool
	}{{"lib" + name + ".dylib", false}, {name + ".dylib", false}, {name + ".framework/" + name, true}} {
		leaf := filepath.Base(form.file)
		var tries []string
		if form.framework {
			tries = joinEach(tries, frameworkPath, form.file)
		}
		tries = joinEach(tries, libraryPath, leaf)
		tries = append(tries, form.file)
		if form.framework && frameworkSet {
			tries = joinEach(tries, absDirs(filepath.SplitList(fallbackFrameworks)), form.file)
		}
		if librarySet {
			tries = joinEach(tries, absDirs(filepath.SplitList(fallbackLibraries)), leaf)
		}
		if form.framework && !frameworkSet {
			tries = joinEach(tries, defaultFrameworks, form.file)
		}
		if !librarySet {
			tries = joinEach(tries, defaultLibraries, leaf)
		}
		out = append(out, withImageSuffix(tries)...)
	}
	return joinEach(out, []string{"/opt/homebrew/lib", "/usr/local/lib", "/opt/local/lib"}, "lib"+name+".dylib")
}

// Absolute directories of a dyld search path variable
func dyldDirs(variable string) []string { return absDirs(filepath.SplitList(os.Getenv(variable))) }

// List extended with one file name joined onto each directory
func joinEach(list, dirs []string, file string) []string {
	for _, dir := range dirs {
		list = append(list, filepath.Join(dir, file))
	}
	return list
}

// Paths each preceded by its DYLD_IMAGE_SUFFIX variant, the order dyld tries them in
func withImageSuffix(paths []string) []string {
	suffix := os.Getenv("DYLD_IMAGE_SUFFIX")
	if suffix == "" {
		return paths
	}
	out := make([]string, 0, 2*len(paths))
	for _, p := range paths {
		if stem, ok := strings.CutSuffix(p, ".dylib"); ok {
			out = append(out, stem+suffix+".dylib", p)
		} else {
			out = append(out, p+suffix, p)
		}
	}
	return out
}

// Whether a file is a dylib or bundle with a slice for the architecture of this process
func loadable(file string) bool {
	cpu := macho.CpuAmd64
	if runtime.GOARCH == "arm64" {
		cpu = macho.CpuArm64
	}
	matches := func(f *macho.File) bool {
		return f.Cpu == cpu && (f.Type == macho.TypeDylib || f.Type == macho.TypeBundle)
	}
	if fat, err := macho.OpenFat(file); err == nil {
		defer fat.Close()
		for _, arch := range fat.Arches {
			if matches(arch.File) {
				return true
			}
		}
		return false
	}
	f, err := macho.Open(file)
	if err != nil {
		return false
	}
	defer f.Close()
	return matches(f)
}
