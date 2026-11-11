package pyplatform

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Python platform.version of this host, the kernel build text or the Windows version triple
func Version() string { return version() }

// Python platform.node of this host, empty when the name cannot be read
func Node() string { return node() }

// Python platform.platform of this host with default arguments, computed once per process
var Platform = sync.OnceValue(platformLabel)

var platformReplacer = strings.NewReplacer(" ", "_", "/", "-", "\\", "-", ":", "-", ";", "-", "\"", "-", "(", "-", ")", "-")

// Non empty stripped parts joined with dashes in the file name safe form of the platform string
func joinPlatform(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, strings.TrimSpace(p))
		}
	}
	label := platformReplacer.Replace(strings.Join(kept, "-"))
	label = strings.ReplaceAll(label, "unknown", "")
	for strings.Contains(label, "--") {
		label = strings.ReplaceAll(label, "--", "-")
	}
	return strings.TrimRight(label, "-")
}

var libcSearchRE = regexp.MustCompile(`(__libc_init)|(GLIBC_([0-9.]+))|(libc(_[0-9A-Za-z_]+)?\.so(?:\.(\d[0-9.]*))?)|(musl-([0-9.]+))|((?:libc\.|ld-)musl(?:-[0-9A-Za-z_]+)?.so(?:\.(\d[0-9.]*))?)`)

// Whether dotted numeric version a orders after b component by component
func versionAfter(a, b string) bool {
	parse := func(s string) []int {
		var out []int
		for _, part := range strings.Split(s, ".") {
			if part != "" {
				n, _ := strconv.Atoi(part)
				out = append(out, n)
			}
		}
		return out
	}
	x, y := parse(a), parse(b)
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return len(x) > len(y)
}

// Path with every symlink at its last component followed, the absolute path of a plain file
func followSymlinks(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	for {
		st, err := os.Lstat(path)
		if err != nil || st.Mode()&os.ModeSymlink == 0 {
			return path
		}
		target, err := os.Readlink(path)
		if err != nil {
			return path
		}
		if filepath.IsAbs(target) {
			path = filepath.Clean(target)
		} else {
			path = filepath.Clean(filepath.Join(filepath.Dir(path), target))
		}
	}
}

// C library name and version the markers inside an executable name, both empty when it carries none
func libcVer(executable string) (string, string) { return libcVerChunked(executable, 16384) }

// C library scan of an executable read in chunks of the given size
func libcVerChunked(executable string, chunkSize int) (string, string) {
	lib, ver, found := "", "", false
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	f, err := os.Open(executable)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	read := func() []byte {
		buf := make([]byte, chunkSize)
		n, _ := f.Read(buf)
		return buf[:n]
	}
	binary := read()
	pos := 0
	for pos < len(binary) {
		var m []int
		text := string(binary)
		if strings.Contains(text, "libc") || strings.Contains(text, "GLIBC") || strings.Contains(text, "musl") {
			if loc := libcSearchRE.FindSubmatchIndex(binary[pos:]); loc != nil {
				m = make([]int, len(loc))
				for i, v := range loc {
					if v >= 0 {
						v += pos
					}
					m[i] = v
				}
			}
		}
		if m == nil || m[1] == len(binary) {
			if chunk := read(); len(chunk) > 0 {
				binary = append(append([]byte{}, binary[max(pos, len(binary)-1000):]...), chunk...)
				pos = 0
				continue
			}
			if m == nil {
				break
			}
		}
		group := func(i int) string {
			if m[2*i] < 0 {
				return ""
			}
			return string(binary[m[2*i]:m[2*i+1]])
		}
		switch {
		case group(1) != "" && lib == "":
			lib = "libc"
		case group(1) != "":
		case group(2) != "":
			if lib != "glibc" {
				lib, ver, found = "glibc", group(3), true
			} else if versionAfter(group(3), ver) {
				ver = group(3)
			}
		case group(4) != "":
			if lib != "glibc" && lib != "musl" {
				lib = "libc"
				if so := group(6); so != "" && (ver == "" || versionAfter(so, ver)) {
					ver, found = so, true
				}
				if threads := group(5); threads != "" && !strings.HasSuffix(ver, threads) {
					ver, found = ver+threads, true
				}
			}
		case group(7) != "":
			lib = "musl"
			if ver == "" || versionAfter(group(8), ver) {
				ver, found = group(8), true
			}
		case group(9) != "":
			lib = "musl"
			if so := group(10); so != "" && (ver == "" || versionAfter(so, ver)) {
				ver, found = so, true
			}
		}
		pos = m[1]
	}
	if !found {
		ver = ""
	}
	return lib, ver
}

// Output of the file command over a path in the C locale, empty when the command fails
func syscmdFile(target string) string {
	cmd := exec.Command("file", "-b", followSymlinks(target))
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// Pointer width and linkage format the file command reports for the running executable
func architecture() (string, string) {
	bits, linkage := strconv.Itoa(strconv.IntSize)+"bit", ""
	executable, err := os.Executable()
	if err != nil {
		return bits, linkage
	}
	fileout := syscmdFile(executable)
	if fileout == "" || (!strings.Contains(fileout, "executable") && !strings.Contains(fileout, "shared object")) {
		return bits, linkage
	}
	if strings.Contains(fileout, "32-bit") {
		bits = "32bit"
	} else if strings.Contains(fileout, "64-bit") {
		bits = "64bit"
	}
	switch {
	case strings.Contains(fileout, "ELF"):
		linkage = "ELF"
	case strings.Contains(fileout, "Mach-O"):
		linkage = "Mach-O"
	case strings.Contains(fileout, "PE"):
		linkage = "PE"
		if strings.Contains(fileout, "Windows") {
			linkage = "WindowsPE"
		}
	case strings.Contains(fileout, "COFF"):
		linkage = "COFF"
	case strings.Contains(fileout, "MS-DOS"):
		linkage = "MSDOS"
	}
	return bits, linkage
}
