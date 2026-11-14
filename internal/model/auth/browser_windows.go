package auth

import (
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func browserSession(*exec.Cmd) {}

func platformBrowsers(env func(string) string) []browserCandidate {
	out := []browserCandidate{{key: "windows-default", mode: launchWindowsDefault}}
	programFilesX86 := env("PROGRAMFILES(x86)")
	if programFilesX86 == "" {
		programFilesX86 = `C:\Program Files (x86)`
	}
	programFiles := env("PROGRAMFILES")
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	edge64 := filepath.Join(programFilesX86, `Microsoft\Edge\Application\msedge.exe`)
	edge32 := filepath.Join(programFiles, `Microsoft\Edge\Application\msedge.exe`)
	for _, name := range []string{"firefox", "seamonkey", "mozilla", "chrome", "opera", edge64, edge32} {
		if onPath(name) {
			out = append(out, namedBrowser(name, launchBackground))
		}
	}
	if onPath("MicrosoftEdge.exe") {
		out = append(out, browserCandidate{key: "microsoft-edge", name: "MicrosoftEdge.exe", argv: []string{"MicrosoftEdge.exe", "%s"},
			remoteArgs: []string{"%s"}, mode: launchRemote})
	}
	return out
}

func openNative(target string) bool {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return false
	}
	file, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return false
	}
	return windows.ShellExecute(0, verb, file, nil, nil, 1) == nil
}
