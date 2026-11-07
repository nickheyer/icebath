package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

var retentionOpaqueDirs = map[string]bool{
	"node_modules": true, ".venv": true, "venv": true, "__pycache__": true, ".pytest_cache": true, ".mypy_cache": true,
	".ruff_cache": true, ".tox": true, ".next": true, ".turbo": true, ".cache": true, ".gradle": true,
}

var retentionPerWorktreeRefs = []string{"refs/worktree/", "refs/bisect/", "refs/rewritten/"}

// Entries walked per tree before an embedded repository can no longer be ruled out
var retentionMaxEntries = 200_000

const retentionNamed = 5

// Keep actions a removal blocker reports
const (
	KeepUnarchivable   = "keep-unarchivable"
	KeepWorktreeRefs   = "keep-worktree-refs"
	KeepLargeUntracked = "keep-large-untracked"
	KeepSubmoduleWork  = "keep-submodule-work"
)

// Keep action and reason why a tree may not be removed
type RemovalBlock struct {
	Action string
	Reason string
}

func retentionEnv(wt string) (map[string]string, error) {
	env, err := archiveEnv(wt, "")
	if err != nil {
		return nil, err
	}
	return withoutOptionalLocks(env), nil
}

func retentionGit(args []string, wt string, env map[string]string) (string, error) {
	return archiveOK(args, wt, env, 120*time.Second, nil)
}

// Embedded repositories found in a tree, the walked entry count and whether the walk stopped at the cap
type TreeScan struct {
	Embedded  []string
	Entries   int
	Truncated bool
}

// Records nested git entries below an ignored directory without following symlinks
func retentionWalk(scan *TreeScan, wt, relDir string) {
	stack := []string{relDir}
	for len(stack) > 0 && !scan.Truncated {
		rel := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := os.ReadDir(filepath.Join(wt, rel))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			scan.Entries++
			if scan.Entries > retentionMaxEntries {
				scan.Truncated = true
				return
			}
			child := rel + "/" + entry.Name()
			if entry.Name() == ".git" {
				scan.Embedded = append(scan.Embedded, rel)
				continue
			}
			if entry.IsDir() && !retentionOpaqueDirs[entry.Name()] {
				stack = append(stack, child)
			}
		}
	}
}

// Ignored paths with whole ignored directories collapsed, through status when ls-files hits its internal error
func retentionIgnoredEntries(wt string, env map[string]string) ([]string, error) {
	out, err := retentionGit([]string{"ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "--no-empty-directory", "-z"}, wt, env)
	var ae *ArchiveError
	if err == nil {
		var paths []string
		for _, p := range strings.Split(out, "\x00") {
			if p != "" {
				paths = append(paths, p)
			}
		}
		return paths, nil
	}
	if !errors.As(err, &ae) {
		return nil, err
	}
	out, err = retentionGit([]string{"status", "--porcelain", "-z", "--ignored=traditional", "--untracked-files=normal", "--no-renames"}, wt, env)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range strings.Split(out, "\x00") {
		if strings.HasPrefix(e, "!! ") {
			paths = append(paths, e[3:])
		}
	}
	return paths, nil
}

// Embedded repositories of the tree, ignored or not, walking collapsed ignored directories within the entry cap
func ScanTree(wtPath string) (*TreeScan, error) {
	env, err := retentionEnv(wtPath)
	if err != nil {
		return nil, err
	}
	scan := &TreeScan{}
	untracked, err := retentionGit([]string{"ls-files", "--others", "--exclude-standard", "-z"}, wtPath, env)
	if err != nil {
		return nil, err
	}
	for _, p := range strings.Split(untracked, "\x00") {
		if strings.HasSuffix(p, "/") {
			scan.Embedded = append(scan.Embedded, strings.TrimRight(p, "/"))
		}
	}
	ignored, err := retentionIgnoredEntries(wtPath, env)
	if err != nil {
		return nil, err
	}
	for _, rel := range ignored {
		relClean := strings.TrimRight(rel, "/")
		p := filepath.Join(wtPath, relClean)
		last := relClean[strings.LastIndex(relClean, "/")+1:]
		if !strings.HasSuffix(rel, "/") || isSymlink(p) || retentionOpaqueDirs[last] {
			continue
		}
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			scan.Embedded = append(scan.Embedded, relClean)
			continue
		}
		retentionWalk(scan, wtPath, relClean)
		if scan.Truncated {
			break
		}
	}
	return scan, nil
}

func isSymlink(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

// Skip worktree and assume unchanged paths whose disk state differs from the index, a missing sparse file is no edit
func HiddenIndexEdits(wtPath string, env map[string]string) ([]string, error) {
	if env == nil {
		var err error
		if env, err = retentionEnv(wtPath); err != nil {
			return nil, err
		}
	}
	out, err := retentionGit([]string{"ls-files", "-v", "-s", "-z"}, wtPath, env)
	if err != nil {
		return nil, err
	}
	type flaggedEntry struct{ rel, mode, sha string }
	var edits []string
	var present []flaggedEntry
	for _, record := range strings.Split(out, "\x00") {
		meta, rel, _ := strings.Cut(record, "\t")
		parts := pystr.SplitWhitespace(meta, -1)
		if rel == "" || len(parts) != 4 || !(parts[0] == "S" || pyIsLower(parts[0])) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(wtPath, rel)); err != nil {
			if parts[0] != "S" {
				edits = append(edits, rel)
			}
			continue
		}
		present = append(present, flaggedEntry{rel, parts[1], parts[2]})
	}
	for _, f := range present {
		p := filepath.Join(wtPath, f.rel)
		var data []byte
		ok := false
		if f.mode == "120000" {
			if isSymlink(p) {
				link, err := os.Readlink(p)
				if err != nil {
					return nil, err
				}
				data, ok = []byte(link), true
			}
		} else if st, err := os.Stat(p); !isSymlink(p) && err == nil && st.Mode().IsRegular() {
			if data, err = os.ReadFile(p); err != nil {
				return nil, err
			}
			ok = true
		}
		if !ok {
			edits = append(edits, f.rel)
			continue
		}
		if retentionHash(wtPath, env, data, f.rel, true) != f.sha && retentionHash(wtPath, env, data, f.rel, false) != f.sha {
			edits = append(edits, f.rel)
		}
	}
	return edits, nil
}

// Whether the text has a cased character and every cased character is lowercase, as str.islower
func pyIsLower(s string) bool {
	cased := false
	for _, r := range s {
		if unicode.IsUpper(r) || unicode.IsTitle(r) {
			return false
		}
		if unicode.IsLower(r) {
			cased = true
		}
	}
	return cased
}

func retentionHash(wt string, env map[string]string, data []byte, rel string, raw bool) string {
	args := []string{"git", "hash-object", "--stdin"}
	if raw {
		args = append(args, "--no-filters")
	} else {
		args = append(args, "--path="+rel)
	}
	r, err := pysubprocess.RunWith(context.Background(), args, pysubprocess.Options{Dir: wt, Env: mapToEnviron(env), Input: data, Timeout: 60 * time.Second})
	if err != nil || r.Returncode != 0 {
		return ""
	}
	return pystr.Strip(strings.ToValidUTF8(string(r.Stdout), "�"))
}

// Per worktree refs holding commits no branch, tag, remote or archive reaches
func PrivateWorktreeRefs(wtPath string, env map[string]string) ([]string, error) {
	if env == nil {
		var err error
		if env, err = retentionEnv(wtPath); err != nil {
			return nil, err
		}
	}
	out, err := retentionGit(append([]string{"for-each-ref", "--format=%(refname)%00%(objectname)"}, retentionPerWorktreeRefs...), wtPath, env)
	if err != nil {
		return nil, err
	}
	var private []string
	for _, line := range pystr.Splitlines(out, false) {
		name, sha, _ := strings.Cut(line, "\x00")
		if sha == "" {
			continue
		}
		reach, err := retentionGit([]string{"rev-list", "--max-count=1", sha, "--not", "--branches", "--tags", "--remotes", "--glob=" + ArchivePrefix + "*"}, wtPath, env)
		if err != nil {
			return nil, err
		}
		if pystr.Strip(reach) != "" {
			private = append(private, name)
		}
	}
	return private, nil
}

func retentionNamedList(paths []string, more bool) string {
	tail := ""
	if extra := len(paths) - retentionNamed; extra > 0 {
		tail = fmt.Sprintf(" (+%d more)", extra)
	} else if more {
		tail = " (+more)"
	}
	return strings.Join(paths[:min(len(paths), retentionNamed)], ", ") + tail
}

// Keep action and reason when removing the tree would lose something, any failure to verify blocks
func RemovalBlocker(wtPath string, skipped []SkippedUntracked, gitlinks bool) *RemovalBlock {
	unverifiable := func(err error) *RemovalBlock {
		return &RemovalBlock{KeepUnarchivable, "could not verify the tree is safe to remove: " + err.Error()}
	}
	why, err := SubmoduleWork(wtPath, gitlinks)
	if err != nil {
		return unverifiable(err)
	}
	if why != "" {
		return &RemovalBlock{KeepSubmoduleWork, why}
	}
	scan, err := ScanTree(wtPath)
	if err != nil {
		return unverifiable(err)
	}
	if len(scan.Embedded) > 0 {
		return &RemovalBlock{KeepUnarchivable, fmt.Sprintf("embedded repository %s cannot be archived", scan.Embedded[0])}
	}
	if scan.Truncated {
		return &RemovalBlock{KeepUnarchivable, fmt.Sprintf("over %d ignored entries; cannot rule out an embedded repository", retentionMaxEntries)}
	}
	refs, err := PrivateWorktreeRefs(wtPath, nil)
	if err != nil {
		return unverifiable(err)
	}
	if len(refs) > 0 {
		return &RemovalBlock{KeepWorktreeRefs, fmt.Sprintf("per-worktree ref %s holds commits no branch has", retentionNamedList(refs, false))}
	}
	if len(skipped) > 0 {
		var names []string
		for _, s := range skipped {
			names = append(names, s.Path)
		}
		return &RemovalBlock{KeepLargeUntracked, "untracked file(s) over the archive size cap: " + retentionNamedList(names, false)}
	}
	return nil
}
