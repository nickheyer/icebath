//go:build !windows && !darwin

package native

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

// Soname of the loader's cache, else the first lib<name>.so file of the directories the loader searches
func find(name string) string {
	if found := cached(name); found != "" {
		return found
	}
	prefix := "lib" + name + ".so"
	seen := map[string]bool{}
	for _, dir := range searchDirs() {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		var names []string
		for _, entry := range entries {
			if entry.Name() == prefix || strings.HasPrefix(entry.Name(), prefix+".") {
				names = append(names, entry.Name())
			}
		}
		slices.Sort(names)
		for _, file := range names {
			if path := filepath.Join(dir, file); isFile(path) {
				return path
			}
		}
	}
	return ""
}

// Library the loader's own cache lists: ld.so.cache on Linux, the elf hints on FreeBSD
func cached(name string) string {
	switch runtime.GOOS {
	case "linux":
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
	case "freebsd":
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
	}
	return ""
}

// Output of ldconfig with one argument in the C locale, nil when it is missing, fails or hangs
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

// Tag ldconfig -p prints for libraries of this process's architecture, so a 32 bit process never takes a 64 bit library
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

// Directories the loader searches: LD_LIBRARY_PATH, nix-ld's path, the Termux prefix, musl's path file, then the system's
func searchDirs() []string {
	dirs := append(envDirs("LD_LIBRARY_PATH"), envDirs("NIX_LD_LIBRARY_PATH")...)
	if prefix := os.Getenv("PREFIX"); strings.Contains(prefix, "com.termux") {
		dirs = append(dirs, filepath.Join(prefix, "lib"))
	}
	files, _ := filepath.Glob("/etc/ld-musl-*.path")
	for _, file := range files {
		if data, err := os.ReadFile(file); err == nil {
			for _, dir := range strings.FieldsFunc(string(data), func(r rune) bool { return r == ':' || r == '\n' || r == '\r' }) {
				if dir = strings.TrimSpace(dir); dir != "" {
					dirs = append(dirs, dir)
				}
			}
		}
	}
	switch runtime.GOOS {
	case "android":
		return append(dirs, "/system/lib64", "/vendor/lib64", "/system/lib", "/vendor/lib")
	case "freebsd", "netbsd", "openbsd", "dragonfly":
		return append(dirs, "/usr/local/lib", "/usr/pkg/lib", "/usr/lib", "/lib")
	case "solaris", "illumos":
		return append(dirs, "/usr/local/lib/64", "/usr/lib/64", "/lib/64", "/usr/local/lib", "/usr/lib", "/lib")
	}
	if triplet := multiarch[runtime.GOARCH]; triplet != "" {
		dirs = append(dirs, "/usr/local/lib/"+triplet, "/lib/"+triplet, "/usr/lib/"+triplet)
	}
	if strconv.IntSize == 64 {
		dirs = append(dirs, "/usr/local/lib64", "/lib64", "/usr/lib64")
	}
	return append(dirs, "/usr/local/lib", "/lib", "/usr/lib", "/run/current-system/sw/lib")
}

// Debian multiarch directory of each architecture
var multiarch = map[string]string{"amd64": "x86_64-linux-gnu", "arm64": "aarch64-linux-gnu", "386": "i386-linux-gnu", "arm": "arm-linux-gnueabihf",
	"riscv64": "riscv64-linux-gnu", "ppc64le": "powerpc64le-linux-gnu", "ppc64": "powerpc64-linux-gnu", "s390x": "s390x-linux-gnu",
	"loong64": "loongarch64-linux-gnu", "mips64le": "mips64el-linux-gnuabi64", "mips64": "mips64-linux-gnuabi64", "mipsle": "mipsel-linux-gnu",
	"mips": "mips-linux-gnu"}
