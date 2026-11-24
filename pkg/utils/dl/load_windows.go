//go:build windows && (amd64 || arm64) && !faketime

package dlutil

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

// DLL opened by name or path, global having no meaning on Windows. A bare name is looked for only in the safe default directories (the executable's directory, the directories added with AddDLLDirectory, System32), never in the working directory or on PATH. A path loads that file, its dependencies searched in up to three passes, each only after the one before failed: (1) the DLL's own directory, the executable's directory, the added directories, System32; (2) the same with the System directory, the Windows directory and every absolute PATH directory added for that one call; (3) the classic LOAD_WITH_ALTERED_SEARCH_PATH order: the DLL's directory, System32, System, the Windows directory, the working directory, then PATH in order
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
		if handle, err = loadWithClassicDirs(name, safe); err != nil {
			handle, err = windows.LoadLibraryEx(name, 0, windows.LOAD_WITH_ALTERED_SEARCH_PATH)
		}
	}
	if err != nil {
		return nil, &LoadError{Name: name, Err: err}
	}
	return &Library{name: name, handle: uintptr(handle)}, nil
}

var classicDirsMu sync.Mutex

// DLL at path loaded with flags while the directories of the classic search other than the working directory are added as DLL directories, removed again before returning
func loadWithClassicDirs(path string, flags uintptr) (windows.Handle, error) {
	classicDirsMu.Lock()
	defer classicDirsMu.Unlock()
	var cookies []uintptr
	defer func() {
		for _, cookie := range cookies {
			_ = windows.RemoveDllDirectory(cookie)
		}
	}()
	for _, dir := range classicDirs() {
		p, err := windows.UTF16PtrFromString(dir)
		if err != nil {
			continue
		}
		if cookie, err := windows.AddDllDirectory(p); err == nil {
			cookies = append(cookies, cookie)
		}
	}
	return windows.LoadLibraryEx(path, 0, flags)
}

// The System directory, the Windows directory and the absolute PATH directories, each once; relative PATH entries are left out because they resolve against the working directory
func classicDirs() []string {
	var dirs []string
	if win, err := windows.GetWindowsDirectory(); err == nil {
		dirs = append(dirs, filepath.Join(win, "System"), win)
	}
	for _, dir := range envDirs("PATH") {
		if filepath.IsAbs(dir) {
			dirs = append(dirs, filepath.Clean(dir))
		}
	}
	seen := make(map[string]bool, len(dirs))
	out := dirs[:0]
	for _, dir := range dirs {
		if key := strings.ToLower(dir); !seen[key] {
			seen[key] = true
			out = append(out, dir)
		}
	}
	return out
}

// Address of a function the DLL exports, a SymbolError carrying the system error when it does not
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
	modules, err := processModules()
	if err != nil {
		return 0, &SymbolError{Symbol: name, Err: err}
	}
	for _, module := range modules {
		if addr, err := windows.GetProcAddress(module, name); err == nil && addr != 0 {
			return addr, nil
		}
	}
	return 0, &SymbolError{Symbol: name, Err: windows.ERROR_PROC_NOT_FOUND}
}

// Handles of every module loaded into the process
func processModules() ([]windows.Handle, error) {
	size := uint32(unsafe.Sizeof(windows.Handle(0)))
	modules := make([]windows.Handle, 256)
	for {
		var needed uint32
		if err := windows.EnumProcessModules(windows.CurrentProcess(), &modules[0], uint32(len(modules))*size, &needed); err != nil {
			return nil, err
		}
		n := int(needed / size)
		if n <= len(modules) {
			return modules[:n], nil
		}
		modules = make([]windows.Handle, n)
	}
}

// Binds a Go function variable to the C function at an address
func RegisterFunc(fptr any, addr uintptr) { purego.RegisterFunc(fptr, addr) }
