//go:build (darwin || freebsd || linux || netbsd) && !386 && !arm && !faketime

package pysounddevice

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"github.com/ebitengine/purego"
)

type culong = uint64
type clong = int64

// Library paths tried in order, sonames first on ELF systems, the dyld search path and Homebrew on macOS
func libraryCandidates() []string {
	if runtime.GOOS != "darwin" {
		out := []string{"libportaudio.so.2", "libportaudio.so"}
		if prefix := os.Getenv("PREFIX"); strings.Contains(prefix, "com.termux") {
			out = append(out, filepath.Join(prefix, "lib", "libportaudio.so"))
		}
		return out
	}
	var dirs []string
	for _, name := range []string{"DYLD_LIBRARY_PATH", "DYLD_FALLBACK_LIBRARY_PATH"} {
		for _, dir := range filepath.SplitList(os.Getenv(name)) {
			if dir != "" {
				dirs = append(dirs, dir)
			}
		}
	}
	if os.Getenv("DYLD_FALLBACK_LIBRARY_PATH") == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dirs = append(dirs, filepath.Join(home, "lib"))
		}
		dirs = append(dirs, "/usr/local/lib", "/lib", "/usr/lib")
	}
	dirs = append(dirs, "/opt/homebrew/lib", "/opt/local/lib")
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	var out []string
	for _, dir := range dirs {
		for _, name := range []string{"libportaudio.dylib", "portaudio.dylib", "libportaudio.2.dylib"} {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out
}

// Loads the first PortAudio library the candidates yield and binds it
func openPortAudio() error {
	if libHandle != 0 {
		return nil
	}
	for _, path := range libraryCandidates() {
		handle, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			continue
		}
		return bindLibrary(handle, func(name string) (uintptr, error) { return purego.Dlsym(handle, name) })
	}
	return &LibraryError{Msg: "PortAudio library not found"}
}

// Points the C stdio stderr at the null device until the returned restore runs
func ignoreStderr() func() {
	noop := func() {}
	fopenAddr, err := purego.Dlsym(purego.RTLD_DEFAULT, "fopen")
	if err != nil {
		return noop
	}
	fcloseAddr, err := purego.Dlsym(purego.RTLD_DEFAULT, "fclose")
	if err != nil {
		return noop
	}
	slotAddr, err := purego.Dlsym(purego.RTLD_DEFAULT, "stderr")
	if err != nil {
		if slotAddr, err = purego.Dlsym(purego.RTLD_DEFAULT, "__stderrp"); err != nil {
			return noop
		}
	}
	var fopen func(path, mode string) uintptr
	var fclose func(file uintptr) int32
	purego.RegisterFunc(&fopen, fopenAddr)
	purego.RegisterFunc(&fclose, fcloseAddr)
	devnull := fopen(os.DevNull, "w")
	if devnull == 0 {
		return noop
	}
	slot := *(*unsafe.Pointer)(unsafe.Pointer(&slotAddr))
	old := *(*uintptr)(slot)
	*(*uintptr)(slot) = devnull
	return func() {
		*(*uintptr)(slot) = old
		fclose(devnull)
	}
}
