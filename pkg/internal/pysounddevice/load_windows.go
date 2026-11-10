//go:build windows && !386 && !arm

package pysounddevice

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

type culong = uint32
type clong = int32

// Library paths found on PATH under the names sounddevice looks for, then the bundled name beside the executable
func openPortAudio() error {
	if libHandle != 0 {
		return nil
	}
	dirs := filepath.SplitList(os.Getenv("PATH"))
	for _, name := range []string{"portaudio", `bin\libportaudio-2.dll`} {
		for _, dir := range dirs {
			path := filepath.Join(dir, name)
			if !isFile(path) && filepath.Ext(path) != ".dll" {
				path += ".dll"
			}
			if !isFile(path) {
				continue
			}
			if handle, err := windows.LoadLibraryEx(path, 0, windows.LOAD_WITH_ALTERED_SEARCH_PATH); err == nil {
				return bindHandle(handle)
			}
		}
	}
	bundled := "libportaudio64bit.dll"
	if _, ok := os.LookupEnv("SD_ENABLE_ASIO"); ok {
		bundled = "libportaudio64bit-asio.dll"
	}
	if handle, err := windows.LoadLibraryEx(bundled, 0, windows.LOAD_LIBRARY_SEARCH_APPLICATION_DIR); err == nil {
		return bindHandle(handle)
	}
	return &LibraryError{Msg: "PortAudio library not found"}
}

func bindHandle(handle windows.Handle) error {
	return bindLibrary(uintptr(handle), func(name string) (uintptr, error) { return windows.GetProcAddress(handle, name) })
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Windows has no C stdio stderr to redirect, the restore does nothing
func ignoreStderr() func() { return func() {} }
