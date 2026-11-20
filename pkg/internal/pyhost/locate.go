package pyhost

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Interpreter is a CPython installation resolved from an executable path without running it
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

// NoSharedLibraryError names an interpreter linked without libpython and the uv commands that supply one
type NoSharedLibraryError struct {
	Executable string
	LDLibrary  string
	Version    string
	Tried      []string
}

func (e *NoSharedLibraryError) Error() string {
	why := "is built without a shared libpython (Py_ENABLE_SHARED=0)"
	if len(e.Tried) > 0 {
		why = fmt.Sprintf("declares a shared libpython that is not installed (tried %v)", e.Tried)
	}
	return fmt.Sprintf("%s %s, LDLIBRARY=%s, install a shared build with `uv python install %s` and host plugins with `uv python find --managed-python %s`",
		e.Executable, why, e.LDLibrary, e.Version, e.Version)
}

var (
	exeVersionRe = regexp.MustCompile(`^python(\d)\.(\d+)(t?)(\.exe)?$`)
	stdlibDirRe  = regexp.MustCompile(`^python(\d)\.(\d+)(t?)$`)
	dllRe        = regexp.MustCompile(`^(?i)python(\d)(\d+)(t?)(_d)?\.dll$`)
	sysVarRe     = regexp.MustCompile(`["']([A-Za-z_][A-Za-z0-9_]*)["']\s*:\s*("([^"\\]*)"|'([^'\\]*)'|(-?\d+))`)
)

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
				parts := strings.Split(v, ".")
				if len(parts) >= 2 {
					in.Major, _ = strconv.Atoi(parts[0])
					in.Minor, _ = strconv.Atoi(parts[1])
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
		if m := exeVersionRe.FindStringSubmatch(filepath.Base(real)); m != nil {
			in.Major, _ = strconv.Atoi(m[1])
			in.Minor, _ = strconv.Atoi(m[2])
			in.ABIFlags = m[3]
		}
	}
	if goos == "windows" {
		return in, in.resolveWindows()
	}
	return in, in.resolveUnix()
}

// Version string major.minor
func (in *Interpreter) Version() string { return fmt.Sprintf("%d.%d", in.Major, in.Minor) }

// Finds the stdlib directory, sysconfig data, shared library and headers of a POSIX layout
func (in *Interpreter) resolveUnix() error {
	libRoot := filepath.Join(in.BasePrefix, "lib")
	entries, err := os.ReadDir(libRoot)
	if err != nil {
		return err
	}
	var stdlibs []string
	for _, e := range entries {
		m := stdlibDirRe.FindStringSubmatch(e.Name())
		if m == nil || !isFile(filepath.Join(libRoot, e.Name(), "os.py")) {
			continue
		}
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		if in.Major != 0 && (major != in.Major || minor != in.Minor) {
			continue
		}
		stdlibs = append(stdlibs, e.Name())
	}
	sort.Strings(stdlibs)
	if in.ABIFlags != "" {
		filtered := stdlibs[:0]
		for _, s := range stdlibs {
			if strings.HasSuffix(s, in.ABIFlags) {
				filtered = append(filtered, s)
			}
		}
		stdlibs = filtered
	}
	if len(stdlibs) != 1 {
		return fmt.Errorf("cannot pick one stdlib directory under %s for %s: %v", libRoot, in.Executable, stdlibs)
	}
	m := stdlibDirRe.FindStringSubmatch(stdlibs[0])
	in.Major, _ = strconv.Atoi(m[1])
	in.Minor, _ = strconv.Atoi(m[2])
	in.StdlibDir = filepath.Join(libRoot, stdlibs[0])
	vars, err := readSysconfigData(in.StdlibDir, m[3], in.GOOS)
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
	for _, c := range candidates {
		if isFile(c) {
			in.Library = c
			break
		}
	}
	if in.Library == "" {
		return &NoSharedLibraryError{Executable: in.Executable, LDLibrary: vars["LDLIBRARY"], Version: in.Version(), Tried: candidates}
	}
	tag := fmt.Sprintf("python%d.%d%s", in.Major, in.Minor, in.ABIFlags)
	in.IncludeDir = filepath.Join(in.BasePrefix, "include", tag)
	if !isFile(filepath.Join(in.IncludeDir, "pyconfig.h")) && vars["INCLUDEPY"] != "" {
		in.IncludeDir = vars["INCLUDEPY"]
	}
	if defs, err := readDefines(filepath.Join(in.IncludeDir, "pyconfig.h")); err == nil {
		for _, name := range []string{"Py_GIL_DISABLED", "Py_DEBUG", "Py_STATS"} {
			in.Macros[name] = in.Macros[name] || defs[name]
		}
	}
	site := filepath.Join(in.Prefix, "lib", fmt.Sprintf("python%d.%d%s", in.Major, in.Minor, in.ABIFlags), "site-packages")
	in.SitePackages = site
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
		m := dllRe.FindStringSubmatch(e.Name())
		if m == nil || m[2] == "" {
			continue
		}
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		if in.Major != 0 && (major != in.Major || minor != in.Minor) {
			continue
		}
		if in.ABIFlags != "" && m[3] != in.ABIFlags {
			continue
		}
		if m[4] != "" {
			continue
		}
		dlls = append(dlls, e.Name())
	}
	if len(dlls) != 1 {
		return fmt.Errorf("cannot pick one python3X.dll in %s for %s: %v", in.BasePrefix, in.Executable, dlls)
	}
	m := dllRe.FindStringSubmatch(dlls[0])
	in.Major, _ = strconv.Atoi(m[1])
	in.Minor, _ = strconv.Atoi(m[2])
	in.ABIFlags = m[3]
	in.Library = filepath.Join(in.BasePrefix, dlls[0])
	in.Macros["MS_WINDOWS"] = true
	in.Macros["Py_GIL_DISABLED"] = m[3] == "t"
	in.StdlibDir = filepath.Join(in.BasePrefix, "Lib")
	in.IncludeDir = filepath.Join(in.BasePrefix, "include")
	in.SitePackages = filepath.Join(in.Prefix, "Lib", "site-packages")
	if !isFile(filepath.Join(in.StdlibDir, "os.py")) {
		return fmt.Errorf("%s has no os.py", in.StdlibDir)
	}
	return nil
}

// Reads key = value lines of a venv configuration file
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

// Parses the build_time_vars literal of the _sysconfigdata module Python itself would import
func readSysconfigData(stdlib, abiflags, goos string) (map[string]string, error) {
	platform := goos
	if goos == "windows" {
		platform = "win32"
	}
	matches, _ := filepath.Glob(filepath.Join(stdlib, "_sysconfigdata_"+abiflags+"_"+platform+"_*.py"))
	sort.Strings(matches)
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	pick := ""
	for _, m := range matches {
		multiarch := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "_sysconfigdata_"+abiflags+"_"+platform+"_"), ".py")
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
	for _, m := range sysVarRe.FindAllSubmatch(data, -1) {
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

// Collects the macro names a flat autoconf pyconfig.h defines
func readDefines(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
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
