package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// Files that mark the root of a project
var projectMarkers = []string{
	"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "package.json", "tsconfig.json", "deno.json",
	"Cargo.toml", "go.mod", "pom.xml", "build.gradle", "build.gradle.kts", "Gemfile", "composer.json", "mix.exs",
	"pubspec.yaml", "CMakeLists.txt", "Makefile", "Dockerfile", "AGENTS.md", "CLAUDE.md", ".cursorrules",
}

// Project markers that are instructions for agents rather than manifests
var contextFileNames = []string{"AGENTS.md", "CLAUDE.md", ".cursorrules"}

var (
	lockfiles   = [][2]string{{"uv.lock", "uv"}, {"poetry.lock", "poetry"}, {"Pipfile.lock", "pipenv"}}
	jsLockfiles = [][2]string{{"pnpm-lock.yaml", "pnpm"}, {"bun.lockb", "bun"}, {"bun.lock", "bun"}, {"yarn.lock", "yarn"}, {"package-lock.json", "npm"}}
	// Package scripts and Makefile targets that check a project
	verifyTargets = []string{"test", "tests", "lint", "typecheck", "check", "build", "fmt", "format"}
)

const (
	// Directories above a folder a project marker is looked for in
	markerDepth = 6
	// Commands that check a project, at most
	verifyCommands = 8
	// Largest manifest read for facts
	factFileBytes = 256 * 1024
)

// What a project holds: its root, its manifests, package managers, the commands that check it and its instructions for agents
type Project struct {
	Root         string
	Manifests    []string
	Managers     []string
	Verify       []string
	ContextFiles []string
}

// Path made absolute with the links of the part of it that exists resolved
func resolved(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return abs
	}
	return filepath.Join(resolved(parent), filepath.Base(abs))
}

// Nearest folder at or a few above the folder holding a project marker, never the home or the shared temporary folder; empty when none
// does
func MarkerRoot(dir string) string {
	var skipped []string
	if home, err := os.UserHomeDir(); err == nil {
		skipped = append(skipped, resolved(home))
	}
	skipped = append(skipped, resolved(os.TempDir()))
	path := resolved(dir)
	for depth := 0; depth <= markerDepth; depth++ {
		if !slices.Contains(skipped, path) {
			for _, marker := range projectMarkers {
				if _, err := os.Stat(filepath.Join(path, marker)); err == nil {
					return path
				}
			}
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	return ""
}

// Root of the project the folder is in, and its git checkout alone: the checkout's top, else the nearest folder with a project marker;
// empty outside a project
func ProjectRoot(dir string) (root, git string) {
	if git = Worktree(resolved(dir)); git != "" {
		return git, git
	}
	return MarkerRoot(dir), ""
}

// Text of a small file, empty when it is missing or larger than a manifest is read
func small(path string) string {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > factFileBytes {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToValidUTF8(string(data), "�"), "\ufeff")
}

// What the project at the root holds
func ProjectAt(root string) Project {
	var verify []string
	if fsx.IsFile(filepath.Join(root, "scripts", "run_tests.sh")) {
		verify = append(verify, "scripts/run_tests.sh")
	}
	if fsx.IsFile(filepath.Join(root, "package.json")) {
		var manifest struct {
			Scripts map[string]any `json:"scripts"`
		}
		json.Unmarshal([]byte(small(filepath.Join(root, "package.json"))), &manifest)
		manager := "npm"
		for _, lockfile := range jsLockfiles {
			if fsx.IsFile(filepath.Join(root, lockfile[0])) {
				manager = lockfile[1]
				break
			}
		}
		for _, target := range verifyTargets {
			if _, ok := manifest.Scripts[target]; ok {
				verify = append(verify, manager+" run "+target)
			}
		}
	}
	if fsx.IsFile(filepath.Join(root, "pytest.ini")) || strings.Contains(small(filepath.Join(root, "pyproject.toml")), "[tool.pytest") {
		verify = append(verify, "pytest")
	}
	if makefile := small(filepath.Join(root, "Makefile")); makefile != "" {
		for _, target := range verifyTargets {
			if regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(target) + `\s*:`).MatchString(makefile) {
				verify = append(verify, "make "+target)
			}
		}
	}
	out := Project{Root: root, Manifests: []string{}, Managers: []string{}, Verify: []string{}, ContextFiles: []string{}}
	for _, marker := range projectMarkers {
		if !slices.Contains(contextFileNames, marker) && fsx.IsFile(filepath.Join(root, marker)) {
			out.Manifests = append(out.Manifests, marker)
		}
	}
	for _, lockfile := range append(slices.Clone(lockfiles), jsLockfiles...) {
		if fsx.IsFile(filepath.Join(root, lockfile[0])) && !slices.Contains(out.Managers, lockfile[1]) {
			out.Managers = append(out.Managers, lockfile[1])
		}
	}
	for _, command := range verify {
		if !slices.Contains(out.Verify, command) {
			out.Verify = append(out.Verify, command)
		}
	}
	out.Verify = out.Verify[:min(len(out.Verify), verifyCommands)]
	for _, name := range contextFileNames {
		if fsx.IsFile(filepath.Join(root, name)) {
			out.ContextFiles = append(out.ContextFiles, name)
		}
	}
	return out
}

// What the project the folder is in holds; false outside a project
func ProjectOf(dir string) (Project, bool) {
	root, _ := ProjectRoot(dir)
	if root == "" {
		return Project{}, false
	}
	return ProjectAt(root), true
}
