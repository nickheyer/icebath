package pm

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Git for Windows bash under the store's git entry, Windows bundles only
func stagedBash(ctx context.Context) (string, error) {
	if runtime.GOOS != "windows" {
		return "", nil
	}
	factsPath, err := FactsPath(ctx)
	if err != nil {
		return "", err
	}
	if !isRegularFile(factsPath) {
		return "", nil
	}
	facts, err := NewFacts(factsPath, false)
	if err != nil {
		return "", nil
	}
	fact, ok := facts.Get("git")
	if !ok || fact == nil {
		return "", nil
	}
	entryName, isString := fact["entry"].(string)
	if !isString {
		return "", nil
	}
	storeRoot, err := StoreRoot(ctx)
	if err != nil {
		return "", nil
	}
	entry := filepath.Join(storeRoot, entryName)
	for _, candidate := range []string{filepath.Join(entry, "usr", "bin", "bash.exe"), filepath.Join(entry, "bin", "bash.exe")} {
		if isRegularFile(candidate) {
			return candidate, nil
		}
	}
	return "", nil
}

// PATH directories whose bash.exe is the WSL launcher or an MSIX alias, never a shell
var windowsBashLauncherDirs = []string{"system32", "windowsapps"}

// Ordered bash.exe candidates for a Windows host, the override and Git roots first and a launcher PATH bash never
func WindowsBashCandidates(onPath string, env map[string]string) []string {
	programFiles, ok := env["ProgramFiles"]
	if !ok {
		programFiles = `C:\Program Files`
	}
	var candidates []string
	if override := env["ICEBATH_GIT_BASH_PATH"]; override != "" {
		candidates = append(candidates, override)
	}
	roots := []string{ntJoin(programFiles, "Git")}
	if x86 := env["ProgramFiles(x86)"]; x86 != "" {
		roots = append(roots, ntJoin(x86, "Git"))
	}
	if local := env["LOCALAPPDATA"]; local != "" {
		roots = append(roots, ntJoin(local, "icebath", "git"), ntJoin(local, "Programs", "Git"))
	}
	for _, root := range roots {
		candidates = append(candidates, ntJoin(root, "bin", "bash.exe"), ntJoin(root, "usr", "bin", "bash.exe"))
	}
	if onPath != "" {
		norm := strings.ToLower(ntNormpath(onPath))
		launcher := false
		for _, dir := range windowsBashLauncherDirs {
			if strings.Contains(norm, dir) {
				launcher = true
			}
		}
		if !launcher {
			candidates = append(candidates, onPath)
		}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// Normalizes a Windows path the way ntpath normpath does
func ntNormpath(path string) string {
	path = strings.ReplaceAll(path, "/", `\`)
	drive, root, tail := ntSplitRoot(path)
	prefix := drive + root
	var comps []string
	for _, comp := range strings.Split(tail, `\`) {
		switch {
		case comp == "" || comp == ".":
		case comp == "..":
			switch {
			case len(comps) > 0 && comps[len(comps)-1] != "..":
				comps = comps[:len(comps)-1]
			case len(comps) > 0 || root == "":
				comps = append(comps, comp)
			}
		default:
			comps = append(comps, comp)
		}
	}
	if prefix == "" && len(comps) == 0 {
		comps = append(comps, ".")
	}
	return prefix + strings.Join(comps, `\`)
}

// Reports whether a bash candidate actually starts, a broken install or a launcher failing here
func bashStarts(candidate string) bool {
	result, err := runProcess(processSpec{
		Argv: []string{candidate, "-c", "exit 0"}, NullStdin: true, NoWindow: true, Timeout: 5 * time.Second,
	})
	return err == nil && result.Code == 0
}

// Bash binary commands run with, the store's git bash first, empty when none is available
func Bash(ctx context.Context) (string, error) {
	staged, err := stagedBash(ctx)
	if err != nil {
		return "", err
	}
	if staged != "" && bashStarts(staged) {
		return staged, nil
	}
	onPath := whichOn("bash", nil)
	if runtime.GOOS == "windows" {
		for _, c := range WindowsBashCandidates(onPath, environMap()) {
			if isRegularFile(c) && bashStarts(c) {
				return c, nil
			}
		}
		return "", nil
	}
	if onPath != "" {
		return onPath, nil
	}
	for _, candidate := range []string{"/usr/bin/bash", "/bin/bash", os.Getenv("SHELL"), "/bin/sh"} {
		if candidate != "" && isRegularFile(candidate) && canExecute(candidate) {
			return candidate, nil
		}
	}
	return "", nil
}

// Search path shutil which falls back to when PATH is unset, the confstr CS_PATH value
func defaultExecPath() string {
	switch runtime.GOOS {
	case "windows":
		return `.;C:\bin`
	case "darwin", "freebsd", "netbsd", "openbsd", "dragonfly":
		return "/usr/bin:/bin:/usr/sbin:/sbin"
	}
	return "/bin:/usr/bin"
}
