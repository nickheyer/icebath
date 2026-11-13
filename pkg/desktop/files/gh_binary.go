package files

import (
	"os"
	"path/filepath"
	"sync"
)

var ghBinaryCache struct {
	sync.Mutex
	path string
}

// GitHub CLI the review pane runs, the usual install folders probed before PATH, cached
func ResolveGhBinary() string {
	ghBinaryCache.Lock()
	defer ghBinaryCache.Unlock()
	if ghBinaryCache.path != "" {
		return ghBinaryCache.path
	}
	var candidates []string
	if hostOS == "windows" {
		programFiles := os.Getenv("ProgramFiles")
		if programFiles == "" {
			programFiles = `C:\Program Files`
		}
		candidates = append(candidates, filepath.Join(programFiles, "GitHub CLI", "gh.exe"))
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			candidates = append(candidates, filepath.Join(local, "Microsoft", "WinGet", "Links", "gh.exe"))
		}
	} else {
		home, _ := os.UserHomeDir()
		candidates = append(candidates, "/opt/homebrew/bin/gh", "/usr/local/bin/gh", "/usr/bin/gh", filepath.Join(home, ".local", "bin", "gh"))
		candidates = append(candidates, findPathCandidates("gh")...)
	}
	selected := SelectRunnableBinary(candidates, fileExists, binaryRuns)
	if selected == "" {
		selected = findOnPath("gh")
	}
	if selected == "" {
		selected = "gh"
	}
	ghBinaryCache.path = selected
	return selected
}
