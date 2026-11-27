package packages

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Every platform the package manager pins archives for
var Targets = []string{
	"win32-x64",
	"win32-arm64",
	"linux-x64",
	"linux-arm64",
	"linux-x64-musl",
	"linux-arm64-musl",
	"linux-arm64-bionic",
	"darwin-x64",
	"darwin-arm64",
}

// Linux userlands on musl
var Musl = map[string]bool{"linux-x64-musl": true, "linux-arm64-musl": true}

// Android's Termux userland, whose tools come as Termux packages
const Bionic = "linux-arm64-bionic"

// Rust target triple of each target
var RustTriple = map[string]string{
	"win32-x64":        "x86_64-pc-windows-msvc",
	"win32-arm64":      "aarch64-pc-windows-msvc",
	"linux-x64":        "x86_64-unknown-linux-gnu",
	"linux-arm64":      "aarch64-unknown-linux-gnu",
	"linux-x64-musl":   "x86_64-unknown-linux-musl",
	"linux-arm64-musl": "aarch64-unknown-linux-musl",
	"darwin-x64":       "x86_64-apple-darwin",
	"darwin-arm64":     "aarch64-apple-darwin",
}

var current = sync.OnceValue(detect)

// Target of this machine, the libc of its userland deciding the Linux flavour
func CurrentTarget() string {
	return current()
}

func detect() string {
	arch := "x64"
	if nativeArm64() {
		arch = "arm64"
	}
	switch runtime.GOOS {
	case "windows":
		return "win32-" + arch
	case "darwin":
		return "darwin-" + arch
	}
	if bionic() {
		return "linux-" + arch + "-bionic"
	}
	if musl() {
		return "linux-" + arch + "-musl"
	}
	return "linux-" + arch
}

// Whether the machine itself is arm64, even when this binary runs emulated on it under Rosetta or Windows' x64 emulation
func nativeArm64() bool {
	return runtime.GOARCH == "arm64" || emulatedOnArm64()
}

// Termux on Android: a build for Android, or a shell whose loader is Android's unless /bin/sh loads glibc or musl
func bionic() bool {
	if runtime.GOOS == "android" {
		return true
	}
	if _, known := loaderMusl("/bin/sh"); known {
		return false
	}
	for _, shell := range []string{"/system/bin/sh", "/data/data/com.termux/files/usr/bin/sh"} {
		if head, ok := elfHead(shell); ok && bytes.Contains(head, []byte("/system/bin/linker")) {
			return true
		}
	}
	return false
}

// Whether the userland's own programs load musl, this binary's loader and a musl loader on disk breaking a tie
func musl() bool {
	for _, program := range []string{"/bin/sh", "/bin/ls"} {
		if isMusl, known := loaderMusl(program); known {
			return isMusl
		}
	}
	if exe, err := os.Executable(); err == nil {
		if isMusl, known := loaderMusl(exe); known {
			return isMusl
		}
	}
	matches, _ := filepath.Glob("/lib/ld-musl-*.so.1")
	return len(matches) > 0
}

// First bytes of an ELF file through links, false when it is no ELF file
func elfHead(path string) ([]byte, bool) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	head := make([]byte, 8192)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	return head, bytes.HasPrefix(head, []byte("\x7fELF"))
}

// Whether an ELF file's loader is musl's, unknown when it names neither libc
func loaderMusl(path string) (bool, bool) {
	head, ok := elfHead(path)
	switch {
	case !ok:
		return false, false
	case bytes.Contains(head, []byte("ld-musl-")):
		return true, true
	case bytes.Contains(head, []byte("ld-linux")):
		return false, true
	}
	return false, false
}

// Whether a binary is built for the target's architecture, unknown for scripts and anything not an executable image
func archMatches(bin, target string) (bool, bool) {
	parts := strings.Split(target, "-")
	if len(parts) < 2 {
		return false, true
	}
	arch := parts[1]
	f, err := os.Open(bin)
	if err != nil {
		return false, false
	}
	defer f.Close()
	head := make([]byte, 64)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	switch {
	case len(head) >= 64 && string(head[:2]) == "MZ":
		offset := binary.LittleEndian.Uint32(head[60:64])
		sig := make([]byte, 6)
		if m, _ := f.ReadAt(sig, int64(offset)); m < 6 || string(sig[:4]) != "PE\x00\x00" {
			return false, false
		}
		want, ok := map[string]uint16{"x64": 0x8664, "arm64": 0xAA64}[arch]
		return ok && binary.LittleEndian.Uint16(sig[4:6]) == want, true
	case len(head) >= 20 && string(head[:4]) == "\x7fELF":
		want, ok := map[string]uint16{"x64": 0x3E, "arm64": 0xB7}[arch]
		return ok && binary.LittleEndian.Uint16(head[18:20]) == want, true
	case len(head) >= 8 && (string(head[:4]) == "\xcf\xfa\xed\xfe" || string(head[:4]) == "\xfe\xed\xfa\xcf"):
		want, ok := map[string]uint32{"x64": 0x01000007, "arm64": 0x0100000C}[arch]
		return ok && binary.LittleEndian.Uint32(head[4:8]) == want, true
	case len(head) >= 4 && (string(head[:4]) == "\xca\xfe\xba\xbe" || string(head[:4]) == "\xbe\xba\xfe\xca"):
		return true, true
	}
	return false, false
}
