package hostinfo

import (
	"context"
	"debug/elf"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/nickheyer/icebath/internal/dl"
)

const (
	maxInterpreterBytes = 4096
	csGNULibcVersion    = 2
	confstrBufferBytes  = 256
)

var (
	androidShells  = []string{"/system/bin/sh", "/data/data/com.termux/files/usr/bin/sh"}
	nativeBinaries = []string{"/bin/sh", "/bin/ls"}
)

// Flavor and version of the C library the native userland links against
func probeLibC(ctx context.Context) (LibC, error) {
	flavor, loader := libcFlavor()
	var version string
	var err error
	switch flavor {
	case GLibC:
		version, err = glibcVersion(ctx)
	case Musl:
		version, err = muslVersion(ctx, loader)
	}
	if err != nil {
		return LibC{}, err
	}
	return LibC{Flavor: flavor, Version: version}, nil
}

// Library family from the loaders native binaries name, then a musl loader on disk, else glibc
func libcFlavor() (LibCFlavor, string) {
	for _, shell := range androidShells {
		if loader, ok := elfInterpreter(shell); ok && strings.Contains(loader, "/system/bin/linker") {
			return Bionic, loader
		}
	}
	candidates := append([]string{}, nativeBinaries...)
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, exe)
	}
	for _, binary := range candidates {
		loader, ok := elfInterpreter(binary)
		switch {
		case !ok:
		case strings.Contains(loader, "ld-musl-"):
			return Musl, loader
		case strings.Contains(loader, "ld-linux"):
			return GLibC, loader
		}
	}
	if matches, _ := filepath.Glob("/lib/ld-musl-*.so.1"); len(matches) > 0 {
		return Musl, matches[0]
	}
	return GLibC, ""
}

// Program interpreter an ELF file names, false when the file is unreadable, not ELF or static
func elfInterpreter(path string) (string, bool) {
	f, err := elf.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	for _, prog := range f.Progs {
		if prog.Type != elf.PT_INTERP || prog.Filesz > maxInterpreterBytes {
			continue
		}
		buf := make([]byte, prog.Filesz)
		if _, err := prog.ReadAt(buf, 0); err != nil {
			return "", false
		}
		return strings.TrimRight(string(buf), "\x00"), true
	}
	return "", false
}

// Release the GNU C library reports through confstr, else through getconf, empty when neither names one
func glibcVersion(ctx context.Context) (string, error) {
	if confstr, err := dl.Self().Symbol("confstr"); err == nil {
		buf := make([]byte, confstrBufferBytes)
		n, _ := dl.Call(confstr, csGNULibcVersion, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n > 0 && int(n) <= len(buf) {
			if version, ok := parseGlibcVersion(string(buf[:n-1])); ok {
				return version, nil
			}
		}
	}
	out, _, err := runProgram(ctx, "getconf", []string{"/usr/bin/getconf", "/bin/getconf"}, "GNU_LIBC_VERSION")
	if err != nil {
		return "", ctx.Err()
	}
	version, _ := parseGlibcVersion(string(out))
	return version, nil
}

// Release in version text such as glibc 2.39, false for text naming another library
func parseGlibcVersion(text string) (string, bool) {
	if fields := strings.Fields(text); len(fields) == 2 && fields[0] == "glibc" {
		return fields[1], true
	}
	return "", false
}

// Release the musl loader prints to stderr when run without arguments, empty when it cannot run or prints none
func muslVersion(ctx context.Context, loader string) (string, error) {
	_, stderr, err := runProgram(ctx, loader, nil)
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return "", ctx.Err()
	}
	for _, line := range strings.Split(string(stderr), "\n") {
		if version, ok := strings.CutPrefix(strings.TrimSpace(line), "Version "); ok {
			return strings.TrimSpace(version), nil
		}
	}
	return "", nil
}
