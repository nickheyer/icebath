package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/errchain"
)

var hintFilenames = []string{"AGENTS.override.md", "AGENTS.md", "agents.md", "CLAUDE.md", "claude.md", ".cursorrules"}

const (
	maxHintChars    = 32_000
	maxAncestorWalk = 5
)

var (
	hintPathArgKeys = []string{"path", "file_path", "workdir"}
	hintNavCommands = setOf("cd", "pushd")
	hintShellOps    = setOf("&&", "||", "|", ";", "&", ";;", "|&", "(", ")")
	containerHomes  = setOf(filepath.Clean("/home"), filepath.Clean("/root"), filepath.Clean("/workspace"))
)

// Followed stat behind every hint is_file, is_dir and exists check, swapped by tests to raise
var hintPathStat = pathutil.StatIfExists

// Hex sha256 of the utf-8 content
func hintDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// Resolved target of a hint file inside workingDir and off the read deny list, false when it must not load
func resolvedHintTarget(env *Env, hintPath, workingDir string) (string, bool) {
	resolved, err := pathutil.Resolve(hintPath)
	if err != nil {
		return "", false
	}
	if _, inside := pathutil.RelativeTo(resolved, workingDir); !inside {
		return "", false
	}
	if ReadBlockError(env, resolved) != "" {
		return "", false
	}
	return resolved, true
}

// Path and stripped content of the first readable hint file of directory in priority order
func firstHintFile(env *Env, directory string) (string, string, bool) {
	if safeWorkerEnabled() {
		return "", "", false
	}
	for _, filename := range hintFilenames {
		candidate := pathutil.Join(directory, filename)
		fi, err := hintPathStat(candidate)
		if err != nil || fi == nil || !fi.Mode().IsRegular() {
			continue
		}
		target, ok := resolvedHintTarget(env, candidate, directory)
		if !ok {
			continue
		}
		content, err := readTextFile(target)
		if err != nil {
			continue
		}
		return candidate, strings.TrimSpace(content), true
	}
	return "", "", false
}

// Operands of cd or pushd starting a shell segment, cd dash and a bare cd yielding nothing
func navTargets(cmd string) []string {
	tokens, err := shellutil.SplitOperators(cmd)
	if err != nil {
		return nil
	}
	var targets []string
	segmentStart := true
	for idx, token := range tokens {
		if hintShellOps[token] {
			segmentStart = true
			continue
		}
		if segmentStart && hintNavCommands[token] {
			for _, t := range tokens[idx+1:] {
				if hintShellOps[t] || !strings.HasPrefix(t, "-") {
					if t != "" && !hintShellOps[t] {
						targets = append(targets, t)
					}
					break
				}
			}
		}
		segmentStart = false
	}
	return targets
}

// Whether workingDir is a home or container home the session fell back to rather than a project
func isHomeLikeWorkingDir(workingDir string) (bool, error) {
	h, err := pathutil.Home()
	if err != nil {
		return false, err
	}
	if workingDir == h || containerHomes[workingDir] || containerHomes[filepath.Dir(filepath.Clean(workingDir))] {
		return true, nil
	}
	_, parts := pathutil.Parts(workingDir)
	for _, part := range parts {
		if part == ".home" || part == "host-home" || part == "host_home" {
			return true, nil
		}
	}
	return false, nil
}

// Directories the agent visits through tool calls, each first visit loading its project hint file
type SubdirectoryHintTracker struct {
	mu               sync.Mutex
	env              *Env
	enabled          bool
	workingDir       string
	homeIsWorkingDir bool
	loadedDirs       map[string]bool
	loadedDigests    map[string]bool
}

// Tracker anchored at workingDir or the process cwd, its deny list and read timeout from the home of env
func NewSubdirectoryHintTrackerFor(env *Env, workingDir string, enabled bool) *SubdirectoryHintTracker {
	if workingDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			panic(err)
		}
		workingDir = cwd
	}
	resolved, err := pathutil.Resolve(workingDir)
	if err != nil {
		panic(err)
	}
	homeLike, err := isHomeLikeWorkingDir(resolved)
	if err != nil {
		panic(err)
	}
	t := &SubdirectoryHintTracker{env: env, enabled: enabled, workingDir: resolved, homeIsWorkingDir: homeLike}
	t.anchor()
	return t
}

// Fresh loaded sets for the working dir, seeded with the digest of its own hint file
func (t *SubdirectoryHintTracker) anchor() {
	t.loadedDirs = map[string]bool{t.workingDir: true}
	t.loadedDigests = map[string]bool{}
	if _, content, ok := firstHintFile(t.env, t.workingDir); ok && content != "" {
		t.loadedDigests[hintDigest(content)] = true
	}
}

// Anchors hint discovery at a real project directory the session adopted, a home or invalid path ignored
func (t *SubdirectoryHintTracker) RebindWorkingDir(workingDir string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	expanded, err := pathutil.ExpandUserPath(workingDir)
	if err != nil {
		return
	}
	newDir, err := pathutil.Resolve(expanded)
	if err != nil {
		return
	}
	fi, err := hintPathStat(newDir)
	if err != nil || fi == nil || !fi.IsDir() {
		return
	}
	if homeLike, err := isHomeLikeWorkingDir(newDir); err != nil || homeLike {
		return
	}
	if newDir == t.workingDir {
		t.homeIsWorkingDir = false
		return
	}
	t.workingDir = newDir
	t.homeIsWorkingDir = false
	t.anchor()
}

// WorkingDir is the directory the tracker treats as the session root
func (t *SubdirectoryHintTracker) WorkingDir() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.workingDir
}

// Formatted hint text of directories this call visits for the first time, false when there is none
func (t *SubdirectoryHintTracker) CheckToolCall(toolName string, toolArgs map[string]any) (string, bool) {
	if !t.enabled || safeWorkerEnabled() {
		return "", false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.homeIsWorkingDir {
		return "", false
	}
	var hints []string
	for _, d := range t.extractDirectories(toolName, toolArgs) {
		if h, ok := t.loadHintsForDirectory(d); ok {
			hints = append(hints, h)
		}
	}
	if len(hints) == 0 {
		return "", false
	}
	return "\n\n" + strings.Join(hints, "\n\n"), true
}

// Candidate directories of the path arguments and of the terminal command, first seen first
func (t *SubdirectoryHintTracker) extractDirectories(toolName string, args map[string]any) []string {
	var candidates []string
	seen := map[string]bool{}
	for _, key := range hintPathArgKeys {
		if val, ok := args[key].(string); ok && strings.TrimSpace(val) != "" {
			t.addPathCandidate(val, &candidates, seen)
		}
	}
	if toolName == "terminal" {
		cmd, present := args["command"]
		if !present {
			cmd = ""
		}
		if s, ok := cmd.(string); ok {
			t.extractPathsFromCommand(s, &candidates, seen)
		}
	}
	return candidates
}

// Adds the directory of rawPath and its unloaded ancestors up to the walk bound
func (t *SubdirectoryHintTracker) addPathCandidate(rawPath string, candidates *[]string, seen map[string]bool) {
	p, err := pathutil.ExpandUserPath(rawPath)
	if err != nil {
		return
	}
	if !filepath.IsAbs(p) {
		p = pathutil.Join(t.workingDir, p)
	}
	if p, err = pathutil.Resolve(p); err != nil {
		return
	}
	if pathutil.Suffix(p) != "" {
		p = filepath.Dir(filepath.Clean(p))
	} else {
		fi, err := hintPathStat(p)
		if err != nil {
			return
		}
		if fi != nil && fi.Mode().IsRegular() {
			p = filepath.Dir(filepath.Clean(p))
		}
	}
	for range maxAncestorWalk {
		if t.loadedDirs[p] {
			break
		}
		if t.isValidSubdir(p) && !seen[p] {
			seen[p] = true
			*candidates = append(*candidates, p)
		}
		parent := filepath.Dir(filepath.Clean(p))
		if parent == p {
			break
		}
		p = parent
	}
}

// Adds navigation operands and path like tokens of a shell command, flags and urls excluded
func (t *SubdirectoryHintTracker) extractPathsFromCommand(cmd string, candidates *[]string, seen map[string]bool) {
	tokens, err := shellutil.Split(cmd)
	if err != nil {
		tokens = strings.Fields(cmd)
	}
	for _, target := range navTargets(cmd) {
		t.addPathCandidate(target, candidates, seen)
	}
	for _, token := range tokens {
		if strings.HasPrefix(token, "-") || strings.HasPrefix(token, "http://") || strings.HasPrefix(token, "https://") ||
			strings.HasPrefix(token, "git@") || (!strings.Contains(token, "/") && !strings.Contains(token, ".")) {
			continue
		}
		t.addPathCandidate(token, candidates, seen)
	}
}

// Whether path sits inside the working dir tree
func (t *SubdirectoryHintTracker) withinWorkingDir(path string) bool {
	_, inside := pathutil.RelativeTo(path, t.workingDir)
	return inside
}

// Unloaded directory inside the working dir tree with no excluded copy directory below the working dir
func (t *SubdirectoryHintTracker) isValidSubdir(path string) bool {
	fi, err := hintPathStat(path)
	if err != nil || fi == nil || !fi.IsDir() {
		return false
	}
	return !t.loadedDirs[path] && t.withinWorkingDir(path) && !t.isExcluded(path)
}

// Whether a segment below the working dir names a dependency, cache, build or backup copy directory
func (t *SubdirectoryHintTracker) isExcluded(path string) bool {
	rel, ok := pathutil.RelativeTo(path, t.workingDir)
	if !ok {
		return true
	}
	if rel == "." {
		return false
	}
	for _, part := range strings.Split(rel, "/") {
		if SearchPruneDirNames[part] {
			return true
		}
	}
	return false
}

// First hint file of directory, scanned and capped, formatted with its display path
func (t *SubdirectoryHintTracker) loadHintsForDirectory(directory string) (string, bool) {
	t.loadedDirs[directory] = true
	if t.homeIsWorkingDir || !t.withinWorkingDir(directory) {
		pkgLog().Debug(fmt.Sprintf("Skipping hint files in %s — outside working_dir %s", directory, t.workingDir))
		return "", false
	}
	for _, filename := range hintFilenames {
		hintPath := pathutil.Join(directory, filename)
		fi, err := hintPathStat(hintPath)
		if err != nil || fi == nil || !fi.Mode().IsRegular() {
			continue
		}
		target, ok := resolvedHintTarget(t.env, hintPath, t.workingDir)
		if !ok {
			continue
		}
		text, _, err := ReadTextWithTimeout(t.env, target, nil)
		if err != nil {
			pkgLog().Debug(fmt.Sprintf("Could not read %s: %s", hintPath, errchain.OSText(err)))
			continue
		}
		content := strings.TrimSpace(text)
		if content == "" {
			continue
		}
		digest := hintDigest(content)
		if t.loadedDigests[digest] {
			pkgLog().Debug(fmt.Sprintf("Skipping duplicate hint content at %s (digest %s)", hintPath, digest[:12]))
			return "", false
		}
		t.loadedDigests[digest] = true
		content = ScanContextContent(content, filename, false)
		relPath := t.displayPath(hintPath)
		limit := maxHintChars
		content = TruncateContent(context.Background(), t.env, content, filename,
			TruncateOptions{MaxChars: &limit, ReadPath: relPath, NoQueue: true})
		pkgLog().Debug(fmt.Sprintf("Loaded subdirectory hints from %s: %s", directory, anyutil.Quote([]any{relPath})))
		return fmt.Sprintf("[Subdirectory context discovered: %s]\n%s", relPath, content), true
	}
	return "", false
}

// Working dir relative path, else a tilde relative posix path, else the absolute path
func (t *SubdirectoryHintTracker) displayPath(hintPath string) string {
	if rel, ok := pathutil.RelativeTo(hintPath, t.workingDir); ok {
		return filepath.FromSlash(rel)
	}
	h, err := pathutil.Home()
	if err == nil {
		if rel, ok := pathutil.RelativeTo(hintPath, h); ok {
			return "~/" + rel
		}
	}
	return hintPath
}
