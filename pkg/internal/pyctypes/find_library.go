package pyctypes

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Library name or path the platform loader resolves for a bare name, empty when none is installed
func FindLibrary(name string) string {
	switch runtime.GOOS {
	case "windows":
		return findLibraryWindows(name)
	case "darwin", "ios":
		return findLibraryDarwin(name)
	default:
		if soname := findSonameLdconfig(name); soname != "" {
			return soname
		}
		return findLibraryLDPath(name)
	}
}

// Machine tag ldconfig prints for libraries of this architecture
func ldconfigABIType() string {
	switch runtime.GOARCH {
	case "amd64":
		return "libc6,x86-64"
	case "arm64":
		return "libc6,AArch64"
	case "ppc64", "ppc64le":
		return "libc6,64bit"
	case "s390x":
		return "libc6,64bit"
	case "riscv64":
		return "libc6,64bit"
	default:
		return "libc6"
	}
}

// Soname the ldconfig cache lists for lib<name> of the host architecture
func findSonameLdconfig(name string) string {
	ldconfig := "/sbin/ldconfig"
	if _, err := os.Stat(ldconfig); err != nil {
		if p, lerr := exec.LookPath("ldconfig"); lerr == nil {
			ldconfig = p
		} else {
			return ""
		}
	}
	cmd := exec.Command(ldconfig, "-p")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	re := regexp.MustCompile(`\s+(lib` + regexp.QuoteMeta(name) + `\.[^\s]+)\s+\(` + regexp.QuoteMeta(ldconfigABIType()))
	if m := re.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return ""
}

// First lib<name>.so file found on LD_LIBRARY_PATH
func findLibraryLDPath(name string) string {
	for _, dir := range filepath.SplitList(os.Getenv("LD_LIBRARY_PATH")) {
		if dir == "" {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(dir, "lib"+name+".so*"))
		for _, m := range matches {
			if st, err := os.Stat(m); err == nil && !st.IsDir() {
				return m
			}
		}
	}
	return ""
}

// First dylib or framework the dyld search and fallback paths hold for the name
func findLibraryDarwin(name string) string {
	candidates := []string{"lib" + name + ".dylib", name + ".dylib", name + ".framework/" + name}
	var dirs []string
	dirs = append(dirs, filepath.SplitList(os.Getenv("DYLD_LIBRARY_PATH"))...)
	fallback := os.Getenv("DYLD_FALLBACK_LIBRARY_PATH")
	if fallback == "" {
		homeDir, _ := os.UserHomeDir()
		fallback = strings.Join([]string{filepath.Join(homeDir, "lib"), "/usr/local/lib", "/lib", "/usr/lib"}, ":")
	}
	dirs = append(dirs, filepath.SplitList(fallback)...)
	for _, c := range candidates {
		for _, dir := range dirs {
			if dir == "" {
				continue
			}
			p := filepath.Join(dir, c)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	return ""
}

// First name.dll file on PATH, the bare name tried before the dll suffix
func findLibraryWindows(name string) string {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		for _, file := range []string{name, name + ".dll"} {
			p := filepath.Join(dir, file)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	return ""
}
