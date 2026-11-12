package files

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// Exists and readdir the Windows git candidate scan reads
type GitCandidateFS struct {
	Exists  func(string) bool
	ReadDir func(string) ([]string, error)
}

// Windows locations the git scan reads
type WindowsGitEnv struct {
	LocalAppData    string
	ProgramFiles    string
	ProgramFilesX86 string
}

var numericCollator = struct {
	sync.Mutex
	c *collate.Collator
}{c: collate.New(language.Und, collate.Numeric)}

// Entries with the prefix sorted newest first by numeric locale compare, each joined with rel when it exists
func versionedBinaries(root, prefix, rel string, fsys GitCandidateFS) []string {
	entries, err := fsys.ReadDir(root)
	if err != nil {
		return nil
	}
	var matched []string
	for _, e := range entries {
		if strings.HasPrefix(e, prefix) {
			matched = append(matched, e)
		}
	}
	numericCollator.Lock()
	sort.SliceStable(matched, func(i, j int) bool { return numericCollator.c.CompareString(matched[j], matched[i]) < 0 })
	numericCollator.Unlock()
	var out []string
	for _, e := range matched {
		if p := filepath.Join(root, e, rel); fsys.Exists(p) {
			out = append(out, p)
		}
	}
	return out
}

// Git bundled by UGit, newest app version first
func UgitGitBinaries(localAppData string, fsys GitCandidateFS) []string {
	return versionedBinaries(filepath.Join(localAppData, "UGit"), "app-", filepath.Join("resources", "app", "git", "cmd", "git.exe"), fsys)
}

// Git installed by the package manager under the icebath tools folder, newest first
func ManagedGitBinaries(localAppData string, fsys GitCandidateFS) []string {
	return versionedBinaries(filepath.Join(localAppData, "icebath", "tools"), "git-", filepath.Join("cmd", "git.exe"), fsys)
}

// Fixed Windows git candidates in preference order, bundled PortableGit, UGit, then Git for Windows
func WindowsGitCandidates(env WindowsGitEnv, fsys GitCandidateFS) []string {
	var c []string
	if env.LocalAppData != "" {
		c = append(c, ManagedGitBinaries(env.LocalAppData, fsys)...)
		c = append(c, filepath.Join(env.LocalAppData, "icebath", "git", "cmd", "git.exe"),
			filepath.Join(env.LocalAppData, "icebath", "git", "bin", "git.exe"))
		c = append(c, UgitGitBinaries(env.LocalAppData, fsys)...)
	}
	c = append(c, filepath.Join(env.ProgramFiles, "Git", "cmd", "git.exe"), filepath.Join(env.ProgramFilesX86, "Git", "cmd", "git.exe"))
	if env.LocalAppData != "" {
		c = append(c, filepath.Join(env.LocalAppData, "Programs", "Git", "cmd", "git.exe"))
	}
	return c
}

// First candidate that exists and runs, else the first that exists, else empty
func SelectRunnableBinary(candidates []string, exists func(string) bool, runs func(string) bool) string {
	var existing []string
	for _, c := range candidates {
		if exists(c) {
			existing = append(existing, c)
		}
	}
	for _, c := range existing {
		if runs(c) {
			return c
		}
	}
	if len(existing) > 0 {
		return existing[0]
	}
	return ""
}

// True when the binary answers --version within five seconds
func binaryRuns(candidate string) bool {
	cmd := exec.Command(candidate, "--version")
	hideConsole(cmd)
	if cmd.Start() != nil {
		return false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		<-done
		return false
	}
}

func pathEntries() []string {
	var out []string
	for _, e := range filepath.SplitList(os.Getenv("PATH")) {
		if e != "" {
			out = append(out, e)
		}
	}
	return out
}

// Every PATH entry holding the command as a regular file
func findPathCandidates(command string) []string {
	var out []string
	for _, e := range pathEntries() {
		if c := filepath.Join(e, command); fileExists(c) {
			out = append(out, c)
		}
	}
	return out
}

// Command on PATH, PATHEXT extensions tried before the bare name on Windows
func findOnPath(command string) string {
	exts := []string{""}
	if hostOS == "windows" {
		pathext := os.Getenv("PATHEXT")
		if pathext == "" {
			pathext = ".COM;.EXE;.BAT;.CMD"
		}
		exts = nil
		for _, e := range strings.Split(pathext, ";") {
			if e != "" {
				exts = append(exts, e)
			}
		}
		exts = append(exts, "")
	}
	for _, e := range pathEntries() {
		for _, ext := range exts {
			if c := filepath.Join(e, command+ext); fileExists(c) {
				return c
			}
		}
	}
	return ""
}

var gitBinaryCache struct {
	sync.Mutex
	path string
}

// Git the desktop runs, a probed PATH hit on POSIX and the fixed install locations first on Windows, cached
func ResolveGitBinary() string {
	gitBinaryCache.Lock()
	defer gitBinaryCache.Unlock()
	if gitBinaryCache.path != "" {
		return gitBinaryCache.path
	}
	if hostOS != "windows" {
		selected := SelectRunnableBinary(findPathCandidates("git"), fileExists, binaryRuns)
		if selected == "" {
			selected = "git"
		}
		gitBinaryCache.path = selected
		return selected
	}
	programFiles, programFilesX86 := os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	if programFilesX86 == "" {
		programFilesX86 = `C:\Program Files (x86)`
	}
	fsys := GitCandidateFS{Exists: fileExists, ReadDir: func(dir string) ([]string, error) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		return names, nil
	}}
	for _, c := range WindowsGitCandidates(WindowsGitEnv{LocalAppData: os.Getenv("LOCALAPPDATA"), ProgramFiles: programFiles, ProgramFilesX86: programFilesX86}, fsys) {
		if fileExists(c) {
			gitBinaryCache.path = c
			return c
		}
	}
	if found := findOnPath("git"); found != "" {
		gitBinaryCache.path = found
		return found
	}
	gitBinaryCache.path = "git"
	return "git"
}
