//go:build (freebsd || linux || netbsd) && !386 && !arm

package dl

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Loader name or absolute path of an installed library given its short name such as opus, in loader search order
func Find(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty library name: %w", ErrNotFound)
	}
	loaderPath := strings.FieldsFunc(os.Getenv("LD_LIBRARY_PATH"), func(r rune) bool { return r == ':' || r == ';' })
	if path, ok := findInDirs(absDirs(loaderPath), name); ok {
		return path, nil
	}
	if soname, ok := findInCache(name); ok {
		return soname, nil
	}
	if path, ok := findInDirs(systemDirs(), name); ok {
		return path, nil
	}
	if path, ok := findInDirs(absDirs(filepath.SplitList(os.Getenv("LIBRARY_PATH"))), name); ok {
		return path, nil
	}
	if path, ok := findInDirs(compilerDirs(), name); ok {
		return path, nil
	}
	return "", fmt.Errorf("%s: %w", name, ErrNotFound)
}

// Private library directories of every installed GCC, newest version first, which the compiler driver links from
func compilerDirs() []string {
	if runtime.GOOS != "linux" {
		return nil
	}
	var dirs []string
	for _, root := range []string{"/usr/lib/gcc", "/usr/lib64/gcc"} {
		versions, _ := filepath.Glob(filepath.Join(root, "*", "*"))
		slices.SortFunc(versions, func(a, b string) int { return compareSonames(filepath.Base(b), filepath.Base(a)) })
		dirs = append(dirs, versions...)
	}
	return dirs
}

// Whether a file name is the named library with or without a version suffix
func isLibraryFile(file, name string) bool {
	base := "lib" + name + ".so"
	return file == base || strings.HasPrefix(file, base+".")
}

// Newest loadable build of the library in the first directory holding one
func findInDirs(dirs []string, name string) (string, bool) {
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		var files []string
		for _, e := range entries {
			if isLibraryFile(e.Name(), name) {
				files = append(files, e.Name())
			}
		}
		slices.SortFunc(files, func(a, b string) int { return compareSonames(b, a) })
		for _, file := range files {
			if path := filepath.Join(dir, file); loadable(path) {
				return path, true
			}
		}
	}
	return "", false
}

// Order of library file names the glibc loader cache keeps, runs of digits compared as numbers
func compareSonames(a, b string) int {
	for a != "" && b != "" {
		da, db := digitRun(a), digitRun(b)
		switch {
		case da > 0 && db > 0:
			na, nb := strings.TrimLeft(a[:da], "0"), strings.TrimLeft(b[:db], "0")
			if c := len(na) - len(nb); c != 0 {
				return c
			}
			if c := strings.Compare(na, nb); c != 0 {
				return c
			}
			a, b = a[da:], b[db:]
		case da > 0:
			return 1
		case db > 0:
			return -1
		case a[0] != b[0]:
			return int(a[0]) - int(b[0])
		default:
			a, b = a[1:], b[1:]
		}
	}
	return len(a) - len(b)
}

// Length of the run of ASCII digits a string starts with
func digitRun(s string) int {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return n
}

// Soname of the first loadable entry the glibc loader cache holds for the library, which the loader resolves itself
func findInCache(name string) (string, bool) {
	if runtime.GOOS != "linux" {
		return "", false
	}
	data, err := os.ReadFile("/etc/ld.so.cache")
	if err != nil {
		return "", false
	}
	for _, e := range cacheEntries(data) {
		if isLibraryFile(e.soname, name) && loadable(e.path) {
			return e.soname, true
		}
	}
	return "", false
}

// One soname and the file the glibc loader cache maps it to
type cacheEntry struct{ soname, path string }

// Entries of a glibc loader cache in stored order, the newer table preferred when it trails the older one
func cacheEntries(data []byte) []cacheEntry {
	const oldMagic, newMagic = "ld.so-1.7.0", "glibc-ld.so.cache1.1"
	order := binary.NativeEndian
	table := func(count, first, stride, strtab int) []cacheEntry {
		var out []cacheEntry
		for i := range count {
			at := first + i*stride
			if at+12 > len(data) {
				break
			}
			soname, okName := cacheString(data, strtab+int(order.Uint32(data[at+4:])))
			path, okPath := cacheString(data, strtab+int(order.Uint32(data[at+8:])))
			if okName && okPath {
				out = append(out, cacheEntry{soname: soname, path: path})
			}
		}
		return out
	}
	newTable := func(base int) []cacheEntry {
		if base+48 > len(data) {
			return nil
		}
		return table(int(order.Uint32(data[base+20:])), base+48, 24, base)
	}
	if bytes.HasPrefix(data, []byte(newMagic)) {
		return newTable(0)
	}
	if !bytes.HasPrefix(data, []byte(oldMagic)) || len(data) < 16 {
		return nil
	}
	count := int(order.Uint32(data[12:]))
	end := 16 + count*12
	if end > len(data) {
		return nil
	}
	if next := (end + 7) &^ 7; next <= len(data) && bytes.HasPrefix(data[next:], []byte(newMagic)) {
		return newTable(next)
	}
	return table(count, 16, 12, end)
}

// NUL terminated string at an offset of the cache, false when it runs past the end
func cacheString(data []byte, off int) (string, bool) {
	if off < 0 || off >= len(data) {
		return "", false
	}
	n := bytes.IndexByte(data[off:], 0)
	if n < 0 {
		return "", false
	}
	return string(data[off : off+n]), true
}

// Directories the loader and linker of this system search by default, in their order
func systemDirs() []string {
	switch runtime.GOOS {
	case "freebsd":
		return append(hintsDirs(), "/lib", "/usr/lib", "/usr/local/lib")
	case "netbsd":
		return []string{"/usr/lib", "/usr/pkg/lib", "/usr/local/lib"}
	}
	t := elfArch().triplet
	dirs := append(muslDirs(), "/usr/local/lib/"+t, "/lib/"+t, "/usr/lib/"+t)
	return append(dirs, "/usr/local/lib64", "/lib64", "/usr/lib64", "/usr/local/lib", "/lib", "/usr/lib", "/usr/"+t+"/lib64", "/usr/"+t+"/lib")
}

// Directories the musl loader path file of this architecture lists, none when the file is absent
func muslDirs() []string {
	raw, err := os.ReadFile("/etc/ld-musl-" + elfArch().musl + ".path")
	if err != nil {
		return nil
	}
	return absDirs(strings.FieldsFunc(string(raw), func(r rune) bool { return r == ':' || r == '\n' }))
}

// Directories the FreeBSD loader hints file lists, none when the file is absent or malformed
func hintsDirs() []string {
	const magic = 0x746e6845
	data, err := os.ReadFile("/var/run/ld-elf.so.hints")
	if err != nil || len(data) < 24 {
		return nil
	}
	var order binary.ByteOrder = binary.LittleEndian
	if order.Uint32(data) != magic {
		if order = binary.BigEndian; order.Uint32(data) != magic {
			return nil
		}
	}
	start := uint64(order.Uint32(data[8:])) + uint64(order.Uint32(data[16:]))
	end := start + uint64(order.Uint32(data[20:]))
	if end > uint64(len(data)) {
		return nil
	}
	return absDirs(strings.Split(string(data[start:end]), ":"))
}

// Object file identity, Debian multiarch triplet and musl loader name of shared libraries for one architecture
type elfIdentity struct {
	machine elf.Machine
	class   elf.Class
	order   elf.Data
	triplet string
	musl    string
}

// Identity of the shared libraries this process can load
func elfArch() elfIdentity {
	switch runtime.GOARCH {
	case "amd64":
		return elfIdentity{elf.EM_X86_64, elf.ELFCLASS64, elf.ELFDATA2LSB, "x86_64-linux-gnu", "x86_64"}
	case "arm64":
		return elfIdentity{elf.EM_AARCH64, elf.ELFCLASS64, elf.ELFDATA2LSB, "aarch64-linux-gnu", "aarch64"}
	case "loong64":
		return elfIdentity{elf.EM_LOONGARCH, elf.ELFCLASS64, elf.ELFDATA2LSB, "loongarch64-linux-gnu", "loongarch64"}
	case "mips":
		return elfIdentity{elf.EM_MIPS, elf.ELFCLASS32, elf.ELFDATA2MSB, "mips-linux-gnu", "mips"}
	case "mipsle":
		return elfIdentity{elf.EM_MIPS, elf.ELFCLASS32, elf.ELFDATA2LSB, "mipsel-linux-gnu", "mipsel"}
	case "mips64":
		return elfIdentity{elf.EM_MIPS, elf.ELFCLASS64, elf.ELFDATA2MSB, "mips64-linux-gnuabi64", "mips64"}
	case "mips64le":
		return elfIdentity{elf.EM_MIPS, elf.ELFCLASS64, elf.ELFDATA2LSB, "mips64el-linux-gnuabi64", "mips64el"}
	case "ppc64":
		return elfIdentity{elf.EM_PPC64, elf.ELFCLASS64, elf.ELFDATA2MSB, "powerpc64-linux-gnu", "powerpc64"}
	case "ppc64le":
		return elfIdentity{elf.EM_PPC64, elf.ELFCLASS64, elf.ELFDATA2LSB, "powerpc64le-linux-gnu", "powerpc64le"}
	case "riscv64":
		return elfIdentity{elf.EM_RISCV, elf.ELFCLASS64, elf.ELFDATA2LSB, "riscv64-linux-gnu", "riscv64"}
	case "s390x":
		return elfIdentity{elf.EM_S390, elf.ELFCLASS64, elf.ELFDATA2MSB, "s390x-linux-gnu", "s390x"}
	}
	return elfIdentity{}
}

// Whether a file is a shared object built for the architecture of this process
func loadable(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	want := elfArch()
	return want.machine != elf.EM_NONE && f.Type == elf.ET_DYN && f.Machine == want.machine && f.Class == want.class && f.Data == want.order
}
