package pyimport

import (
	"archive/zip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// CPython 3.14 builtin modules on posix hosts, origin built-in
var posixBuiltins = "_abc _ast _codecs _collections _contextvars _datetime _functools _imp _io _locale _opcode _operator _signal _sre _stat _string _suggestions _symtable _sysconfig _thread _tokenize _tracemalloc _types _typing _warnings _weakref atexit builtins errno faulthandler gc itertools marshal posix pwd sys time"

// CPython 3.14 builtin modules on windows hosts, origin built-in
var windowsBuiltins = "_abc _ast _codecs _collections _contextvars _datetime _functools _imp _io _locale _opcode _operator _overlapped _signal _sre _stat _string _suggestions _symtable _sysconfig _thread _tokenize _tracemalloc _types _typing _warnings _weakref _winapi atexit builtins errno faulthandler gc itertools marshal msvcrt nt sys time winreg"

// CPython 3.14 frozen modules, origin frozen
var frozenModules = "__hello__ __hello_alias__ __hello_only__ __phello__ __phello_alias__ _collections_abc _frozen_importlib _frozen_importlib_external _sitebuiltins abc codecs genericpath io ntpath os posixpath runpy site stat zipimport"

// Builtin or frozen top level name that find_spec answers before any path entry
func metaPathModule(name string) bool {
	builtins := posixBuiltins
	if runtime.GOOS == "windows" {
		builtins = windowsBuiltins
	}
	for _, table := range []string{builtins, frozenModules} {
		for _, known := range strings.Fields(table) {
			if known == name {
				return true
			}
		}
	}
	return false
}

// File suffixes FileFinder tries in loader order, extension modules first, then source, then bytecode
func loaderSuffixes(name string, listing map[string]string) []string {
	var exts []string
	for low, real := range listing {
		if runtime.GOOS == "windows" {
			if strings.HasPrefix(low, name+".cp") && strings.HasSuffix(low, ".pyd") {
				exts = append(exts, strings.TrimPrefix(real, real[:len(name)]))
			}
		} else if strings.HasPrefix(low, name+".cpython-") && strings.HasSuffix(low, ".so") {
			exts = append(exts, strings.TrimPrefix(real, real[:len(name)]))
		}
	}
	tail := []string{".abi3.so", ".so", ".py", ".pyc"}
	if runtime.GOOS == "windows" {
		tail = []string{".pyd", ".py", ".pyw", ".pyc"}
	}
	return append(exts, tail...)
}

// Directory listing keyed the way FileFinder caches it, lowercased on case insensitive hosts
func cachedListing(dir string) map[string]string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	fold := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		key := e.Name()
		if fold {
			key = strings.ToLower(key)
		}
		out[key] = e.Name()
	}
	return out
}

// Origin file of a top level module or package in one directory entry, namespace true for a bare package dir
func findInDir(base, name string) (origin string, pkgDir string, namespace bool) {
	listing := cachedListing(base)
	if listing == nil {
		return "", "", false
	}
	key := name
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		key = strings.ToLower(name)
	}
	if real, ok := listing[key]; ok {
		dir := filepath.Join(base, real)
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			inner := cachedListing(dir)
			for _, suffix := range loaderSuffixes("__init__", inner) {
				init := filepath.Join(dir, "__init__"+suffix)
				if st, err := os.Stat(init); err == nil && st.Mode().IsRegular() {
					return init, dir, false
				}
			}
			namespace = true
		}
	}
	for _, suffix := range loaderSuffixes(key, listing) {
		file := key + suffix
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			file = strings.ToLower(file)
		}
		if real, ok := listing[file]; ok {
			path := filepath.Join(base, real)
			if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
				return path, "", false
			}
		}
	}
	return "", "", namespace
}

// Whether a zip archive on the path holds the module, whose origin then lies inside the archive
func zipHolds(archive, name string) bool {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return false
	}
	defer r.Close()
	for _, f := range r.File {
		for _, candidate := range []string{name + "/__init__.py", name + "/__init__.pyc", name + ".py", name + ".pyc"} {
			if f.Name == candidate {
				return true
			}
		}
	}
	return false
}

// Source path of a dotted module without importing it, empty for builtin, frozen, namespace, zipped or missing modules
func FindOrigin(path []string, module string) string {
	var parts []string
	for _, p := range strings.Split(module, ".") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 || metaPathModule(parts[0]) {
		return ""
	}
	origin, pkgDir := "", ""
	for _, entry := range path {
		if entry == "" {
			entry = "."
		}
		st, err := os.Stat(entry)
		if err != nil {
			continue
		}
		if !st.IsDir() {
			if zipHolds(entry, parts[0]) {
				return ""
			}
			continue
		}
		if found, dir, _ := findInDir(entry, parts[0]); found != "" {
			origin, pkgDir = found, dir
			break
		}
	}
	if origin == "" || len(parts) == 1 {
		return origin
	}
	if pkgDir == "" {
		return ""
	}
	search := []string{pkgDir}
	for i, part := range parts[1:] {
		found, next := "", ""
		for _, base := range search {
			init, mod := filepath.Join(base, part, "__init__.py"), filepath.Join(base, part+".py")
			if st, err := os.Stat(init); err == nil && st.Mode().IsRegular() {
				found, next = init, filepath.Join(base, part)
				break
			}
			if st, err := os.Stat(mod); err == nil && st.Mode().IsRegular() {
				found = mod
				break
			}
		}
		if found == "" {
			return ""
		}
		if i+2 == len(parts) || next == "" {
			return found
		}
		search = []string{next}
	}
	return ""
}

// First limit code points of a module source read as utf-8-sig with replacement, empty for non source origins
func ReadSource(origin string, limit int) string {
	if !strings.HasSuffix(origin, ".py") {
		return ""
	}
	raw, err := os.ReadFile(origin)
	if err != nil {
		return ""
	}
	text := strings.TrimPrefix(pystr.DecodeUTF8Replace(raw), "\uFEFF")
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit])
}
