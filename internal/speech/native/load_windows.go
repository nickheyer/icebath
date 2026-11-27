//go:build windows && (amd64 || arm64) && !faketime

package native

import (
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/windows"
)

// Whether this build can load shared libraries
const Supported = true

// DLL opened by name, looked for in the safe default directories only, or by path, its dependencies searched beside it and the safe
// directories first, then with the classic directories added, then in the classic altered search order
func Open(name string, global bool) (*Library, error) {
	if name == "" {
		return nil, &LoadError{Err: ErrNotFound}
	}
	if !strings.ContainsAny(name, `/\`) {
		handle, err := windows.LoadLibraryEx(name, 0, windows.LOAD_LIBRARY_SEARCH_DEFAULT_DIRS)
		if err != nil {
			return nil, &LoadError{Name: name, Err: err}
		}
		return &Library{name: name, handle: uintptr(handle)}, nil
	}
	if abs, err := filepath.Abs(name); err == nil {
		name = abs
	}
	safe := uintptr(windows.LOAD_LIBRARY_SEARCH_DEFAULT_DIRS | windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR)
	handle, err := windows.LoadLibraryEx(name, 0, safe)
	if err != nil && isFile(name) {
		if handle, err = withClassicDirs(name, safe); err != nil {
			handle, err = windows.LoadLibraryEx(name, 0, windows.LOAD_WITH_ALTERED_SEARCH_PATH)
		}
	}
	if err != nil {
		return nil, &LoadError{Name: name, Err: err}
	}
	return &Library{name: name, handle: uintptr(handle)}, nil
}

var classicMu sync.Mutex

// DLL loaded while the classic search directories but the working one are added, removed again after
func withClassicDirs(path string, flags uintptr) (windows.Handle, error) {
	classicMu.Lock()
	defer classicMu.Unlock()
	var cookies []uintptr
	defer func() {
		for _, cookie := range cookies {
			windows.RemoveDllDirectory(cookie)
		}
	}()
	var dirs []string
	if win, err := windows.GetWindowsDirectory(); err == nil {
		dirs = append(dirs, filepath.Join(win, "System"), win)
	}
	for _, dir := range envDirs("PATH") {
		if filepath.IsAbs(dir) {
			dirs = append(dirs, filepath.Clean(dir))
		}
	}
	seen := map[string]bool{}
	for _, dir := range dirs {
		if seen[strings.ToLower(dir)] {
			continue
		}
		seen[strings.ToLower(dir)] = true
		if p, err := windows.UTF16PtrFromString(dir); err == nil {
			if cookie, err := windows.AddDllDirectory(p); err == nil {
				cookies = append(cookies, cookie)
			}
		}
	}
	return windows.LoadLibraryEx(path, 0, flags)
}

func (l *Library) Symbol(name string) (uintptr, error) {
	if l == nil || l.handle == 0 {
		return 0, &SymbolError{Library: l.label(), Symbol: name, Err: errNotOpen}
	}
	addr, err := windows.GetProcAddress(windows.Handle(l.handle), name)
	if err == nil && addr == 0 {
		err = windows.ERROR_PROC_NOT_FOUND
	}
	if err != nil {
		return 0, &SymbolError{Library: l.name, Symbol: name, Err: err}
	}
	return addr, nil
}

// Address of a function in any module already loaded into the process
func Lookup(name string) (uintptr, error) {
	size := uint32(unsafe.Sizeof(windows.Handle(0)))
	modules := make([]windows.Handle, 256)
	for {
		var needed uint32
		if err := windows.EnumProcessModules(windows.CurrentProcess(), &modules[0], uint32(len(modules))*size, &needed); err != nil {
			return 0, &SymbolError{Symbol: name, Err: err}
		}
		if n := int(needed / size); n > len(modules) {
			modules = make([]windows.Handle, n)
			continue
		} else {
			modules = modules[:n]
		}
		break
	}
	for _, module := range modules {
		if addr, err := windows.GetProcAddress(module, name); err == nil && addr != 0 {
			return addr, nil
		}
	}
	return 0, &SymbolError{Symbol: name, Err: windows.ERROR_PROC_NOT_FOUND}
}

func RegisterFunc(fptr any, addr uintptr) { purego.RegisterFunc(fptr, addr) }
