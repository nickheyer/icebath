package cpythonutil

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// CPython installation resolved from an executable path without running it
type Interpreter struct {
	GOOS         string
	Executable   string
	Prefix       string
	BasePrefix   string
	Major, Minor int
	ABIFlags     string
	Library      string
	IncludeDir   string
	StdlibDir    string
	SitePackages string
	Venv         bool
	Macros       map[string]bool
	Sysconfig    map[string]string
}

// Interpreter linked without libpython, naming the uv commands that supply a shared build
type NoSharedLibraryError struct {
	Executable string
	LDLibrary  string
	Version    string
	Tried      []string
}

// The interpreter, why it has no usable libpython and how to get one
func (e *NoSharedLibraryError) Error() string {
	why := "is built without a shared libpython (Py_ENABLE_SHARED=0)"
	if len(e.Tried) > 0 {
		why = fmt.Sprintf("declares a shared libpython that is not installed (tried %v)", e.Tried)
	}
	return fmt.Sprintf("%s %s, LDLIBRARY=%s, install a shared build with `uv python install %s` and host plugins with `uv python find --managed-python %s`",
		e.Executable, why, e.LDLibrary, e.Version, e.Version)
}

// Entries of the build_time_vars literal in a _sysconfigdata module, compiled on first use
var sysconfigVar = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`["']([A-Za-z_][A-Za-z0-9_]*)["']\s*:\s*("([^"\\]*)"|'([^'\\]*)'|(-?\d+))`)
})

// Resolves the interpreter of exe for the running platform
func Discover(exe string) (*Interpreter, error) { return DiscoverFor(exe, runtime.GOOS) }

// Resolves the interpreter of exe for goos, which lets foreign layouts be checked on this machine
func DiscoverFor(exe, goos string) (*Interpreter, error) {
	abs, err := filepath.Abs(exe)
	if err != nil {
		return nil, err
	}
	in := &Interpreter{GOOS: goos, Executable: abs, Macros: map[string]bool{}}
	exeDir := filepath.Dir(abs)
	cfgPath := ""
	for _, dir := range []string{exeDir, filepath.Dir(exeDir)} {
		if isFile(filepath.Join(dir, "pyvenv.cfg")) {
			cfgPath = filepath.Join(dir, "pyvenv.cfg")
			break
		}
	}
	if cfgPath != "" {
		cfg, err := readPyvenvCfg(cfgPath)
		if err != nil {
			return nil, err
		}
		home := cfg["home"]
		if home == "" {
			return nil, fmt.Errorf("%s has no home key", cfgPath)
		}
		in.Venv = true
		in.Prefix = filepath.Dir(cfgPath)
		in.BasePrefix = home
		if goos != "windows" {
			in.BasePrefix = filepath.Dir(home)
		}
		for _, key := range []string{"version_info", "version"} {
			if v := cfg[key]; v != "" {
				if major, rest, ok := strings.Cut(v, "."); ok {
					minor, _, _ := strings.Cut(rest, ".")
					in.Major, _ = strconv.Atoi(major)
					in.Minor, _ = strconv.Atoi(minor)
				}
				break
			}
		}
	} else {
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, err
		}
		in.BasePrefix = filepath.Dir(real)
		if goos != "windows" {
			in.BasePrefix = filepath.Dir(filepath.Dir(real))
		}
		in.Prefix = in.BasePrefix
		if major, minor, abi, ok := versionName(strings.TrimSuffix(filepath.Base(real), ".exe")); ok {
			in.Major, in.Minor, in.ABIFlags = major, minor, abi
		}
	}
	if goos == "windows" {
		return in, in.resolveWindows()
	}
	return in, in.resolveUnix()
}

// The major.minor text of the interpreter's version
func (in *Interpreter) Version() string { return strconv.Itoa(in.Major) + "." + strconv.Itoa(in.Minor) }

// Version of a name python<one digit>.<digits> with an optional t for a free-threaded build
func versionName(name string) (major, minor int, abi string, ok bool) {
	rest, ok := strings.CutPrefix(name, "python")
	if !ok || len(rest) < 3 || !isDigit(rest[0]) || rest[1] != '.' {
		return 0, 0, "", false
	}
	major, rest = int(rest[0]-'0'), rest[2:]
	if r, cut := strings.CutSuffix(rest, "t"); cut {
		rest, abi = r, "t"
	}
	if minor, ok = digits(rest); !ok {
		return 0, 0, "", false
	}
	return major, minor, abi, true
}

// Version of a Windows python<digit><digits>[t][_d].dll name in any letter case, and whether it is a debug build
func dllName(name string) (major, minor int, abi string, debug, ok bool) {
	rest, ok := strings.CutPrefix(strings.ToLower(name), "python")
	if !ok {
		return 0, 0, "", false, false
	}
	if rest, ok = strings.CutSuffix(rest, ".dll"); !ok {
		return 0, 0, "", false, false
	}
	rest, debug = strings.CutSuffix(rest, "_d")
	if r, cut := strings.CutSuffix(rest, "t"); cut {
		rest, abi = r, "t"
	}
	if len(rest) < 2 || !isDigit(rest[0]) {
		return 0, 0, "", false, false
	}
	if minor, ok = digits(rest[1:]); !ok {
		return 0, 0, "", false, false
	}
	return int(rest[0] - '0'), minor, abi, debug, true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// Value of a non-empty run of ASCII digits
func digits(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for i := range len(s) {
		if !isDigit(s[i]) {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// Finds the stdlib directory, sysconfig data, shared library and headers of a POSIX layout
func (in *Interpreter) resolveUnix() error {
	libRoot := filepath.Join(in.BasePrefix, "lib")
	entries, err := os.ReadDir(libRoot)
	if err != nil {
		return err
	}
	var stdlibs []string
	for _, e := range entries {
		major, minor, _, ok := versionName(e.Name())
		if !ok || !isFile(filepath.Join(libRoot, e.Name(), "os.py")) {
			continue
		}
		if in.Major != 0 && (major != in.Major || minor != in.Minor) {
			continue
		}
		stdlibs = append(stdlibs, e.Name())
	}
	slices.Sort(stdlibs)
	if in.ABIFlags != "" {
		stdlibs = slices.DeleteFunc(stdlibs, func(s string) bool { return !strings.HasSuffix(s, in.ABIFlags) })
	}
	if len(stdlibs) != 1 {
		return fmt.Errorf("cannot pick one stdlib directory under %s for %s: %v", libRoot, in.Executable, stdlibs)
	}
	major, minor, abi, _ := versionName(stdlibs[0])
	in.Major, in.Minor = major, minor
	in.StdlibDir = filepath.Join(libRoot, stdlibs[0])
	vars, err := readSysconfigData(in.StdlibDir, abi, in.GOOS)
	if err != nil {
		return err
	}
	in.Sysconfig = vars
	in.ABIFlags = vars["ABIFLAGS"]
	in.Macros["Py_GIL_DISABLED"] = vars["Py_GIL_DISABLED"] == "1"
	in.Macros["Py_DEBUG"] = vars["Py_DEBUG"] == "1" || strings.Contains(in.ABIFlags, "d")
	in.Macros["__APPLE__"] = in.GOOS == "darwin"
	shared := vars["Py_ENABLE_SHARED"] == "1" || vars["PYTHONFRAMEWORK"] != ""
	if !shared {
		return &NoSharedLibraryError{Executable: in.Executable, LDLibrary: vars["LDLIBRARY"], Version: in.Version()}
	}
	var candidates []string
	for _, name := range []string{vars["INSTSONAME"], vars["LDLIBRARY"]} {
		if name == "" {
			continue
		}
		candidates = append(candidates, filepath.Join(libRoot, name))
		if vars["LIBDIR"] != "" {
			candidates = append(candidates, filepath.Join(vars["LIBDIR"], name))
			if vars["MULTIARCH"] != "" && filepath.Base(vars["LIBDIR"]) != vars["MULTIARCH"] {
				candidates = append(candidates, filepath.Join(vars["LIBDIR"], vars["MULTIARCH"], name))
			}
		}
		if vars["PYTHONFRAMEWORKPREFIX"] != "" {
			candidates = append(candidates, filepath.Join(vars["PYTHONFRAMEWORKPREFIX"], name))
		}
	}
	if i := slices.IndexFunc(candidates, isFile); i >= 0 {
		in.Library = candidates[i]
	}
	if in.Library == "" {
		return &NoSharedLibraryError{Executable: in.Executable, LDLibrary: vars["LDLIBRARY"], Version: in.Version(), Tried: candidates}
	}
	tag := "python" + in.Version() + in.ABIFlags
	in.IncludeDir = filepath.Join(in.BasePrefix, "include", tag)
	if !isFile(filepath.Join(in.IncludeDir, "pyconfig.h")) && vars["INCLUDEPY"] != "" {
		in.IncludeDir = vars["INCLUDEPY"]
	}
	if defs, err := readDefines(filepath.Join(in.IncludeDir, "pyconfig.h")); err == nil {
		for _, name := range []string{"Py_GIL_DISABLED", "Py_DEBUG", "Py_STATS"} {
			in.Macros[name] = in.Macros[name] || defs[name]
		}
	}
	in.SitePackages = filepath.Join(in.Prefix, "lib", tag, "site-packages")
	return nil
}

// Finds python3X.dll, the stdlib and the flat include directory of a Windows layout
func (in *Interpreter) resolveWindows() error {
	entries, err := os.ReadDir(in.BasePrefix)
	if err != nil {
		return err
	}
	var dlls []string
	for _, e := range entries {
		major, minor, abi, debug, ok := dllName(e.Name())
		if !ok || debug {
			continue
		}
		if in.Major != 0 && (major != in.Major || minor != in.Minor) {
			continue
		}
		if in.ABIFlags != "" && abi != in.ABIFlags {
			continue
		}
		dlls = append(dlls, e.Name())
	}
	if len(dlls) != 1 {
		return fmt.Errorf("cannot pick one python3X.dll in %s for %s: %v", in.BasePrefix, in.Executable, dlls)
	}
	major, minor, abi, _, _ := dllName(dlls[0])
	in.Major, in.Minor, in.ABIFlags = major, minor, abi
	in.Library = filepath.Join(in.BasePrefix, dlls[0])
	in.Macros["MS_WINDOWS"] = true
	in.Macros["Py_GIL_DISABLED"] = abi == "t"
	in.StdlibDir = filepath.Join(in.BasePrefix, "Lib")
	in.IncludeDir = filepath.Join(in.BasePrefix, "include")
	in.SitePackages = filepath.Join(in.Prefix, "Lib", "site-packages")
	if !isFile(filepath.Join(in.StdlibDir, "os.py")) {
		return fmt.Errorf("%s has no os.py", in.StdlibDir)
	}
	return nil
}

// Reads the key = value lines of a venv configuration file, keys lower cased
func readPyvenvCfg(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), "=")
		if ok {
			out[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
		}
	}
	return out, sc.Err()
}

// Parses the build_time_vars literal of the _sysconfigdata module the interpreter itself would import
func readSysconfigData(stdlib, abiflags, goos string) (map[string]string, error) {
	platform := goos
	if goos == "windows" {
		platform = "win32"
	}
	prefix := "_sysconfigdata_" + abiflags + "_" + platform + "_"
	matches, _ := filepath.Glob(filepath.Join(stdlib, prefix+"*.py"))
	slices.Sort(matches)
	// The first part of the multiarch triplet CPython names the module after, x86_64-linux-gnu and so on
	var arch string
	switch runtime.GOARCH {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	case "loong64":
		arch = "loongarch64"
	case "ppc64le":
		arch = "powerpc64le"
	case "riscv64", "s390x":
		arch = runtime.GOARCH
	}
	pick := ""
	for _, m := range matches {
		multiarch := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), prefix), ".py")
		if (goos == "darwin" && multiarch == "darwin") || (arch != "" && strings.HasPrefix(multiarch, arch+"-")) {
			pick = m
			break
		}
	}
	if pick == "" && len(matches) == 1 {
		pick = matches[0]
	}
	if pick == "" {
		return nil, fmt.Errorf("no single _sysconfigdata module for %s %s in %s: %v", platform, runtime.GOARCH, stdlib, matches)
	}
	data, err := os.ReadFile(pick)
	if err != nil {
		return nil, err
	}
	out := map[string]string{"_file": pick}
	for _, m := range sysconfigVar().FindAllSubmatch(data, -1) {
		key := string(m[1])
		if _, seen := out[key]; seen {
			continue
		}
		switch {
		case m[5] != nil:
			out[key] = string(m[5])
		case m[3] != nil || strings.HasPrefix(string(m[2]), `"`):
			out[key] = string(m[3])
		default:
			out[key] = string(m[4])
		}
	}
	return out, nil
}

// Macro names a flat autoconf pyconfig.h defines
func readDefines(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "#define" {
			out[fields[1]] = true
		}
	}
	return out, nil
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
