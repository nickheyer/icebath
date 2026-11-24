//go:build !windows && !darwin

package dlutil

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Soname or path from the loader's cache, else the first lib<name>.so file in the directories the loader searches
func findLibrary(name string) string {
	if found := findCached(name); found != "" {
		return found
	}
	prefix := "lib" + name + ".so"
	seen := map[string]bool{}
	for _, dir := range searchDirs() {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		for _, file := range soFiles(dir, prefix) {
			if path := filepath.Join(dir, file); isFile(path) {
				return path
			}
		}
	}
	return ""
}

// Names in dir that are the prefix itself or the prefix and a version, sorted
func soFiles(dir, prefix string) []string {
	f, err := os.Open(dir)
	if err != nil {
		return nil
	}
	names, _ := f.Readdirnames(-1)
	_ = f.Close()
	var matches []string
	for _, name := range names {
		if name == prefix || strings.HasPrefix(name, prefix+".") {
			matches = append(matches, name)
		}
	}
	slices.Sort(matches)
	return matches
}

// Library the loader's own cache lists for the name: ld.so.cache on Linux, the elf hints on FreeBSD
func findCached(name string) string {
	switch runtime.GOOS {
	case "linux":
		return ldconfigSoname(name)
	case "freebsd":
		return hintsPath(name)
	}
	return ""
}

// Soname ldconfig -p lists for lib<name> built for this process's architecture, the tag in parentheses compared whole so that a 32-bit process does not take a 64-bit library of a multilib host
func ldconfigSoname(name string) string {
	prefix, abi := []byte("lib"+name+"."), []byte(ldconfigABI())
	for line := range bytes.Lines(ldconfig("-p")) {
		fields := bytes.Fields(line)
		if len(fields) < 2 || len(fields[0]) <= len(prefix) || !bytes.HasPrefix(fields[0], prefix) {
			continue
		}
		if tag, ok := bytes.CutPrefix(fields[1], []byte("(")); ok && bytes.Equal(bytes.TrimRight(tag, "),"), abi) {
			return string(fields[0])
		}
	}
	return ""
}

// Path ldconfig -r lists for lib<name> among the FreeBSD elf hints
func hintsPath(name string) string {
	key := []byte(":-l" + name + ".")
	for line := range bytes.Lines(ldconfig("-r")) {
		at := bytes.Index(line, key)
		if at < 0 {
			continue
		}
		if _, path, ok := bytes.Cut(line[at:], []byte("=> ")); ok {
			if path = bytes.TrimSpace(path); len(path) > 0 {
				return string(path)
			}
		}
	}
	return ""
}

// Output of ldconfig run with one argument in the C locale, nil when it is missing, fails or hangs
func ldconfig(arg string) []byte {
	path := "/sbin/ldconfig"
	if !isFile(path) {
		found, err := exec.LookPath("ldconfig")
		if err != nil {
			return nil
		}
		path = found
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, arg)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return out
}

// Tag ldconfig -p prints in parentheses for libraries of this process's architecture, the OS ABI note that may follow it left out
func ldconfigABI() string {
	switch runtime.GOARCH {
	case "amd64":
		return "libc6,x86-64"
	case "arm64":
		return "libc6,AArch64"
	case "ppc64", "ppc64le", "s390x", "mips64", "mips64le", "sparc64":
		return "libc6,64bit"
	case "riscv64", "loong64":
		return "libc6,double-float"
	case "arm":
		return "libc6,hard-float"
	}
	return "libc6"
}

// Directories the loader searches: LD_LIBRARY_PATH, the nix-ld path, the Termux prefix, musl's path file, then the system defaults
func searchDirs() []string {
	dirs := append(envDirs("LD_LIBRARY_PATH"), envDirs("NIX_LD_LIBRARY_PATH")...)
	if prefix := os.Getenv("PREFIX"); strings.Contains(prefix, "com.termux") {
		dirs = append(dirs, filepath.Join(prefix, "lib"))
	}
	dirs = append(dirs, muslDirs()...)
	switch runtime.GOOS {
	case "android":
		return append(dirs, "/system/lib64", "/vendor/lib64", "/system/lib", "/vendor/lib")
	case "freebsd", "netbsd", "openbsd", "dragonfly":
		return append(dirs, "/usr/local/lib", "/usr/pkg/lib", "/usr/lib", "/lib")
	case "solaris", "illumos":
		return append(dirs, "/usr/local/lib/64", "/usr/lib/64", "/lib/64", "/usr/local/lib", "/usr/lib", "/lib")
	}
	if triplet := multiarchTriplet(); triplet != "" {
		dirs = append(dirs, "/usr/local/lib/"+triplet, "/lib/"+triplet, "/usr/lib/"+triplet)
	}
	if strconv.IntSize == 64 {
		dirs = append(dirs, "/usr/local/lib64", "/lib64", "/usr/lib64")
	}
	return append(dirs, "/usr/local/lib", "/lib", "/usr/lib", "/run/current-system/sw/lib")
}

// Directories musl's /etc/ld-musl-<arch>.path lists, none on other C libraries
func muslDirs() []string {
	files, _ := filepath.Glob("/etc/ld-musl-*.path")
	var dirs []string
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, dir := range strings.FieldsFunc(string(data), func(r rune) bool { return r == ':' || r == '\n' || r == '\r' }) {
			if dir = strings.TrimSpace(dir); dir != "" {
				dirs = append(dirs, dir)
			}
		}
	}
	return dirs
}

// Debian multiarch directory name of this process's architecture, empty where there is none
func multiarchTriplet() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64-linux-gnu"
	case "arm64":
		return "aarch64-linux-gnu"
	case "386":
		return "i386-linux-gnu"
	case "arm":
		return "arm-linux-gnueabihf"
	case "riscv64":
		return "riscv64-linux-gnu"
	case "ppc64le":
		return "powerpc64le-linux-gnu"
	case "ppc64":
		return "powerpc64-linux-gnu"
	case "s390x":
		return "s390x-linux-gnu"
	case "loong64":
		return "loongarch64-linux-gnu"
	case "mips64le":
		return "mips64el-linux-gnuabi64"
	case "mips64":
		return "mips64-linux-gnuabi64"
	case "mipsle":
		return "mipsel-linux-gnu"
	case "mips":
		return "mips-linux-gnu"
	}
	return ""
}
