package worktrees

import (
	"errors"
	"fmt"
	"github.com/nickheyer/icebath/pkg/strs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// Folders a scan for repositories nested in a tree doesn't walk into
var opaque = map[string]bool{
	"node_modules": true, ".venv": true, "venv": true, "__pycache__": true, ".pytest_cache": true, ".mypy_cache": true,
	".ruff_cache": true, ".tox": true, ".next": true, ".turbo": true, ".cache": true, ".gradle": true,
}

// Refs a worktree keeps of its own, which no branch may hold
var ownRefs = []string{"refs/worktree/", "refs/bisect/", "refs/rewritten/"}

// Entries walked in one tree before a repository nested in it can no longer be ruled out
const scanEntries = 200_000

// Paths a reason names before it counts the rest
const named = 5

// How a removal blocker keeps a tree
const (
	KeepUnarchivable   = "keep-unarchivable"
	KeepWorktreeRefs   = "keep-worktree-refs"
	KeepLargeUntracked = "keep-large-untracked"
	KeepSubmoduleWork  = "keep-submodule-work"
)

// Why a tree may not be removed and how it is kept
type RemovalBlock struct {
	Action string
	Reason string
}

// Environment of the read only runs that check a tree
func checkEnv(tree string) (map[string]string, error) {
	env, err := archiveEnv(tree, "")
	if err != nil {
		return nil, err
	}
	return lockless(env), nil
}

func checkGit(tree string, env map[string]string, args ...string) (string, error) {
	return archiveOK(tree, env, 2*time.Minute, nil, args...)
}

// Repositories nested in a tree, how many entries the scan walked, and whether it stopped at its limit
type treeScan struct {
	embedded  []string
	entries   int
	truncated bool
}

// Notes the repositories nested below an ignored folder, links never followed
func (s *treeScan) walk(tree, start string) {
	stack := []string{start}
	for len(stack) > 0 && !s.truncated {
		rel := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := os.ReadDir(filepath.Join(tree, rel))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if s.entries++; s.entries > scanEntries {
				s.truncated = true
				return
			}
			if entry.Name() == ".git" {
				s.embedded = append(s.embedded, rel)
				continue
			}
			if entry.IsDir() && !opaque[entry.Name()] {
				stack = append(stack, rel+"/"+entry.Name())
			}
		}
	}
}

// Ignored paths of the tree with whole ignored folders as one, through status when ls-files fails on them
func ignored(tree string, env map[string]string) ([]string, error) {
	out, err := checkGit(tree, env, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "--no-empty-directory", "-z")
	if err == nil {
		var paths []string
		for _, path := range strings.Split(out, "\x00") {
			if path != "" {
				paths = append(paths, path)
			}
		}
		return paths, nil
	}
	if _, ok := errors.AsType[*ArchiveError](err); !ok {
		return nil, err
	}
	if out, err = checkGit(tree, env, "status", "--porcelain", "-z", "--ignored=traditional", "--untracked-files=normal", "--no-renames"); err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range strings.Split(out, "\x00") {
		if path, ok := strings.CutPrefix(entry, "!! "); ok {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// Repositories nested in the tree, ignored or not, the ignored folders walked within the limit of entries
func scanTree(tree string) (*treeScan, error) {
	env, err := checkEnv(tree)
	if err != nil {
		return nil, err
	}
	s := &treeScan{}
	others, err := checkGit(tree, env, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	for _, path := range strings.Split(others, "\x00") {
		if strings.HasSuffix(path, "/") {
			s.embedded = append(s.embedded, strings.TrimRight(path, "/"))
		}
	}
	paths, err := ignored(tree, env)
	if err != nil {
		return nil, err
	}
	for _, rel := range paths {
		clean := strings.TrimRight(rel, "/")
		path := filepath.Join(tree, clean)
		info, err := os.Lstat(path)
		if !strings.HasSuffix(rel, "/") || err == nil && info.Mode()&os.ModeSymlink != 0 || opaque[clean[strings.LastIndex(clean, "/")+1:]] {
			continue
		}
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			s.embedded = append(s.embedded, clean)
			continue
		}
		if s.walk(tree, clean); s.truncated {
			break
		}
	}
	return s, nil
}

// Whether the text has a cased letter and every cased letter is lower case
func lower(s string) bool {
	cased := false
	for _, r := range s {
		if unicode.IsUpper(r) || unicode.IsTitle(r) {
			return false
		}
		cased = cased || unicode.IsLower(r)
	}
	return cased
}

// Blob hash git gives the data as the file of the path, raw or through its filters
func blobHash(tree string, env map[string]string, data []byte, rel string, raw bool) string {
	args := []string{"hash-object", "--stdin", "--path=" + rel}
	if raw {
		args = []string{"hash-object", "--stdin", "--no-filters"}
	}
	result, err := archiveRun(tree, env, time.Minute, data, args...)
	if err != nil || result.Code != 0 {
		return ""
	}
	return strings.TrimSpace(result.Stdout)
}

// Files git was told to overlook, skip-worktree and assume-unchanged, that differ on disk from the index; a sparse file not checked out
// is no edit
func hiddenEdits(tree string, env map[string]string) ([]string, error) {
	if env == nil {
		var err error
		if env, err = checkEnv(tree); err != nil {
			return nil, err
		}
	}
	out, err := checkGit(tree, env, "ls-files", "-v", "-s", "-z")
	if err != nil {
		return nil, err
	}
	type flagged struct{ rel, mode, sha string }
	var edits []string
	var present []flagged
	for _, record := range strings.Split(out, "\x00") {
		meta, rel, _ := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if rel == "" || len(fields) != 4 || fields[0] != "S" && !lower(fields[0]) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(tree, rel)); err != nil {
			if fields[0] != "S" {
				edits = append(edits, rel)
			}
			continue
		}
		present = append(present, flagged{rel, fields[1], fields[2]})
	}
	for _, f := range present {
		path := filepath.Join(tree, f.rel)
		info, err := os.Lstat(path)
		link := err == nil && info.Mode()&os.ModeSymlink != 0
		var data []byte
		switch {
		case f.mode == "120000" && link:
			target, err := os.Readlink(path)
			if err != nil {
				return nil, err
			}
			data = []byte(target)
		case f.mode != "120000" && !link && err == nil && info.Mode().IsRegular():
			if data, err = os.ReadFile(path); err != nil {
				return nil, err
			}
		default:
			edits = append(edits, f.rel)
			continue
		}
		if blobHash(tree, env, data, f.rel, true) != f.sha && blobHash(tree, env, data, f.rel, false) != f.sha {
			edits = append(edits, f.rel)
		}
	}
	return edits, nil
}

// Refs of the worktree's own that hold commits no branch, tag, remote or archive reaches
func privateRefs(tree string) ([]string, error) {
	env, err := checkEnv(tree)
	if err != nil {
		return nil, err
	}
	out, err := checkGit(tree, env, append([]string{"for-each-ref", "--format=%(refname)%00%(objectname)"}, ownRefs...)...)
	if err != nil {
		return nil, err
	}
	var private []string
	for line := range strings.Lines(out) {
		name, sha, _ := strings.Cut(strings.TrimRight(line, "\r\n"), "\x00")
		if sha == "" {
			continue
		}
		reached, err := checkGit(tree, env, "rev-list", "--max-count=1", sha, "--not", "--branches", "--tags", "--remotes", "--glob="+ArchivePrefix+"*")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(reached) != "" {
			private = append(private, name)
		}
	}
	return private, nil
}

// Paths a reason names, the rest counted
func listed(paths []string) string {
	rest := ""
	if extra := len(paths) - named; extra > 0 {
		rest = fmt.Sprintf(" and %s more", strs.Number(float64(extra)))
	}
	return strings.Join(paths[:min(len(paths), named)], ", ") + rest
}

// Why removing the tree would lose something no branch or archive holds: a submodule's work, a nested repository, a ref of its own, or
// untracked files too large for its archive; nil when it is safe. Anything that can't be checked keeps the tree
func Blocker(tree string, skipped []Skipped, gitlinks bool) *RemovalBlock {
	unsure := func(err error) *RemovalBlock {
		return &RemovalBlock{KeepUnarchivable, "icebath could not check that removing it is safe: " + strings.TrimRight(err.Error(), ".")}
	}
	why, err := SubmoduleWork(tree, gitlinks)
	if err != nil {
		return unsure(err)
	}
	if why != "" {
		return &RemovalBlock{KeepSubmoduleWork, why}
	}
	s, err := scanTree(tree)
	if err != nil {
		return unsure(err)
	}
	if len(s.embedded) > 0 {
		return &RemovalBlock{KeepUnarchivable, fmt.Sprintf("it holds the repository %s, which cannot be archived", s.embedded[0])}
	}
	if s.truncated {
		return &RemovalBlock{KeepUnarchivable, fmt.Sprintf("it has over %s ignored entries, so a repository inside it cannot be ruled out", strs.Number(scanEntries))}
	}
	refs, err := privateRefs(tree)
	if err != nil {
		return unsure(err)
	}
	if len(refs) > 0 {
		return &RemovalBlock{KeepWorktreeRefs, fmt.Sprintf("its own ref %s holds commits that no branch has", listed(refs))}
	}
	if len(skipped) > 0 {
		var paths []string
		for _, s := range skipped {
			paths = append(paths, s.Path)
		}
		return &RemovalBlock{KeepLargeUntracked, "untracked files larger than an archive takes: " + listed(paths)}
	}
	return nil
}

// Why removing a clean tree would lose something no branch holds, nil when it is safe
func Blocked(tree string) *RemovalBlock { return Blocker(tree, nil, false) }
