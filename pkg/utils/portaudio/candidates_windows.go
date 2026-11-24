//go:build windows

package portaudioutil

import (
	"os"
	"path/filepath"
	"strings"
)

// PortAudio DLL paths in the order they are tried: portaudio and a conda bin\libportaudio-2.dll in each PATH directory, then the 64-bit build shipped beside the executable, its ASIO variant when SD_ENABLE_ASIO is set
func libraryCandidates() []string {
	dirs := filepath.SplitList(os.Getenv("PATH"))
	out := make([]string, 0, 2*len(dirs)+1)
	for _, name := range []string{"portaudio", `bin\libportaudio-2.dll`} {
		for _, dir := range dirs {
			if dir == "" {
				continue
			}
			path := filepath.Join(dir, name)
			if !isFile(path) && !strings.EqualFold(filepath.Ext(path), ".dll") {
				path += ".dll"
			}
			out = append(out, path)
		}
	}
	bundled := "libportaudio64bit.dll"
	if _, ok := os.LookupEnv("SD_ENABLE_ASIO"); ok {
		bundled = "libportaudio64bit-asio.dll"
	}
	if exe, err := os.Executable(); err == nil {
		out = append(out, filepath.Join(filepath.Dir(exe), bundled))
	}
	return out
}
