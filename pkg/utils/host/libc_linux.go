package hostutil

import (
	"debug/elf"
	"io"
	"os"
	"strconv"
	"strings"
)

// C library and version of this process such as glibc2.40, musl or libc6, from the loaded libc first and the executable's dynamic linking second, empty for a static executable
func libcLabel() string {
	if name, version, ok := confstrLibc(); ok {
		return name + version
	}
	return executableLibc()
}

// C library the running executable links against, read from its program interpreter, version needs and needed libraries
func executableLibc() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	f, err := elf.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	for _, prog := range f.Progs {
		if prog.Type != elf.PT_INTERP {
			continue
		}
		if interp, err := io.ReadAll(io.LimitReader(prog.Open(), 4096)); err == nil && strings.Contains(string(interp), "musl") {
			return "musl"
		}
	}
	glibc := ""
	if needs, err := f.DynamicVersionNeeds(); err == nil {
		for _, need := range needs {
			for _, dep := range need.Needs {
				if version, ok := strings.CutPrefix(dep.Dep, "GLIBC_"); ok && startsWithDigit(version) && versionAfter(version, glibc) {
					glibc = version
				}
			}
		}
	}
	if glibc != "" {
		return "glibc" + glibc
	}
	libs, err := f.ImportedLibraries()
	if err != nil {
		return ""
	}
	for _, lib := range libs {
		switch {
		case strings.Contains(lib, "musl"):
			return "musl"
		case strings.HasPrefix(lib, "libc.so"):
			return "libc" + strings.TrimPrefix(strings.TrimPrefix(lib, "libc.so"), ".")
		}
	}
	return ""
}

func startsWithDigit(s string) bool { return s != "" && s[0] >= '0' && s[0] <= '9' }

// Whether dotted version a orders after b, compared number by number, an empty b ordering before everything
func versionAfter(a, b string) bool {
	as := strings.Split(a, ".")
	var bs []string
	if b != "" {
		bs = strings.Split(b, ".")
	}
	for i := range min(len(as), len(bs)) {
		x, _ := strconv.Atoi(as[i])
		y, _ := strconv.Atoi(bs[i])
		if x != y {
			return x > y
		}
	}
	return len(as) > len(bs)
}
