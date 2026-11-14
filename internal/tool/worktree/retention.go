package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const (
	KeepUnarchivable   = "keep-unarchivable"
	KeepWorktreeRefs   = "keep-worktree-refs"
	KeepLargeUntracked = "keep-large-untracked"
	KeepSubmoduleWork  = "keep-submodule-work"

	maxScanEntries   = 200_000
	namedOffenders   = 5
	retentionTimeout = 120 * time.Second
)

var (
	opaqueDirs = map[string]bool{
		"node_modules": true, ".venv": true, "venv": true, "__pycache__": true, ".pytest_cache": true,
		".mypy_cache": true, ".ruff_cache": true, ".tox": true, ".next": true, ".turbo": true, ".cache": true,
		".gradle": true,
	}
	perWorktreeRefs = []string{"refs/worktree/", "refs/bisect/", "refs/rewritten/"}
)

type treeScan struct {
	embedded  []string
	entries   int
	truncated bool
}

func (m *Manager) retentionEnv(ctx context.Context, wt string) (map[string]string, error) {
	env, err := m.archiveEnv(ctx, wt, "")
	if err != nil {
		return nil, err
	}
	return withOptionalLocksOff(env), nil
}

func (m *Manager) retentionGit(ctx context.Context, wt string, env map[string]string, args ...string) (string, error) {
	return m.archiveOK(ctx, wt, env, retentionTimeout, nil, args...)
}

func walkIgnored(scan *treeScan, wt, relDir string) {
	stack := []string{relDir}
	for len(stack) > 0 && !scan.truncated {
		rel := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		dir, err := os.Open(filepath.Join(wt, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		items, err := dir.ReadDir(-1)
		dir.Close()
		if err != nil {
			continue
		}
		for _, item := range items {
			scan.entries++
			if scan.entries > maxScanEntries {
				scan.truncated = true
				return
			}
			child := rel + "/" + item.Name()
			if item.Name() == ".git" {
				scan.embedded = append(scan.embedded, rel)
				continue
			}
			if item.IsDir() && !opaqueDirs[item.Name()] {
				stack = append(stack, child)
			}
		}
	}
}

func (m *Manager) ignoredEntries(ctx context.Context, wt string, env map[string]string) ([]string, error) {
	out, err := m.retentionGit(ctx, wt, env, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "--no-empty-directory", "-z")
	if err == nil {
		return splitNonEmpty(out), nil
	}
	var archiveErr *ArchiveError
	if !errors.As(err, &archiveErr) {
		return nil, err
	}
	out, err = m.retentionGit(ctx, wt, env, "status", "--porcelain", "-z", "--ignored=traditional", "--untracked-files=normal", "--no-renames")
	if err != nil {
		return nil, err
	}
	var ignored []string
	for _, e := range strings.Split(out, "\x00") {
		if strings.HasPrefix(e, "!! ") {
			ignored = append(ignored, e[3:])
		}
	}
	return ignored, nil
}

func splitNonEmpty(out string) []string {
	var items []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			items = append(items, p)
		}
	}
	return items
}

func (m *Manager) scanTree(ctx context.Context, wt string) (*treeScan, error) {
	env, err := m.retentionEnv(ctx, wt)
	if err != nil {
		return nil, err
	}
	scan := &treeScan{}
	untracked, err := m.retentionGit(ctx, wt, env, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	for _, p := range strings.Split(untracked, "\x00") {
		if strings.HasSuffix(p, "/") {
			scan.embedded = append(scan.embedded, strings.TrimRight(p, "/"))
		}
	}
	ignored, err := m.ignoredEntries(ctx, wt, env)
	if err != nil {
		return nil, err
	}
	for _, rel := range ignored {
		relClean := strings.TrimRight(rel, "/")
		path := filepath.Join(wt, filepath.FromSlash(relClean))
		last := relClean
		if i := strings.LastIndex(relClean, "/"); i >= 0 {
			last = relClean[i+1:]
		}
		if !strings.HasSuffix(rel, "/") || opaqueDirs[last] {
			continue
		}
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			scan.embedded = append(scan.embedded, relClean)
			continue
		}
		walkIgnored(scan, wt, relClean)
		if scan.truncated {
			break
		}
	}
	return scan, nil
}

func flaggedTag(tag string) bool {
	if tag == "S" {
		return true
	}
	cased := false
	for _, r := range tag {
		if unicode.IsUpper(r) || unicode.IsTitle(r) {
			return false
		}
		if unicode.IsLower(r) {
			cased = true
		}
	}
	return cased
}

func (m *Manager) hiddenIndexEdits(ctx context.Context, wt string, env map[string]string) ([]string, error) {
	if env == nil {
		var err error
		if env, err = m.retentionEnv(ctx, wt); err != nil {
			return nil, err
		}
	}
	out, err := m.retentionGit(ctx, wt, env, "ls-files", "-v", "-s", "-z")
	if err != nil {
		return nil, err
	}
	type flagged struct{ rel, mode, sha string }
	var deleted []string
	var check []flagged
	for _, record := range strings.Split(out, "\x00") {
		meta, rel, _ := strings.Cut(record, "\t")
		parts := strings.Fields(meta)
		if rel == "" || len(parts) != 4 || !flaggedTag(parts[0]) {
			continue
		}
		path := filepath.Join(wt, rel)
		if _, err := os.Lstat(path); err != nil {
			if parts[0] != "S" {
				deleted = append(deleted, rel)
			}
			continue
		}
		check = append(check, flagged{rel: rel, mode: parts[1], sha: parts[2]})
	}
	edits := deleted
	for _, f := range check {
		path := filepath.Join(wt, f.rel)
		info, err := os.Lstat(path)
		if err != nil {
			edits = append(edits, f.rel)
			continue
		}
		isLink := info.Mode()&os.ModeSymlink != 0
		var data []byte
		known := false
		if f.mode == "120000" {
			if isLink {
				target, err := os.Readlink(path)
				if err != nil {
					return nil, err
				}
				data, known = []byte(target), true
			}
		} else if !isLink && info.Mode().IsRegular() {
			if data, err = os.ReadFile(path); err != nil {
				return nil, err
			}
			known = true
		}
		if !known {
			edits = append(edits, f.rel)
			continue
		}
		rawSHA, err := m.hashObject(ctx, wt, env, data, f.rel, true)
		if err != nil {
			return nil, err
		}
		if rawSHA == f.sha {
			continue
		}
		filtered, err := m.hashObject(ctx, wt, env, data, f.rel, false)
		if err != nil {
			return nil, err
		}
		if filtered != f.sha {
			edits = append(edits, f.rel)
		}
	}
	return edits, nil
}

func (m *Manager) hashObject(ctx context.Context, wt string, env map[string]string, data []byte, rel string, raw bool) (string, error) {
	args := []string{"hash-object", "--stdin"}
	if raw {
		args = append(args, "--no-filters")
	} else {
		args = append(args, "--path="+rel)
	}
	if data == nil {
		data = []byte{}
	}
	r, err := m.run(ctx, "git", args, call{dir: wt, env: env, timeout: 60 * time.Second, input: data})
	if err != nil {
		return "", err
	}
	if !r.ok() {
		return "", nil
	}
	return strings.TrimSpace(string(r.stdout)), nil
}

func (m *Manager) privateWorktreeRefs(ctx context.Context, wt string, env map[string]string) ([]string, error) {
	if env == nil {
		var err error
		if env, err = m.retentionEnv(ctx, wt); err != nil {
			return nil, err
		}
	}
	args := append([]string{"for-each-ref", "--format=%(refname)%00%(objectname)"}, perWorktreeRefs...)
	out, err := m.retentionGit(ctx, wt, env, args...)
	if err != nil {
		return nil, err
	}
	var private []string
	for _, line := range strings.Split(lineText([]byte(out)), "\n") {
		name, sha, _ := strings.Cut(line, "\x00")
		if sha == "" {
			continue
		}
		reach, err := m.retentionGit(ctx, wt, env, "rev-list", "--max-count=1", sha, "--not", "--branches", "--tags", "--remotes", "--glob="+ArchivePrefix+"*")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(reach) != "" {
			private = append(private, name)
		}
	}
	return private, nil
}

func namedList(paths []string, more bool) string {
	shown := paths
	if len(shown) > namedOffenders {
		shown = shown[:namedOffenders]
	}
	extra := len(paths) - namedOffenders
	suffix := ""
	if extra > 0 {
		suffix = fmt.Sprintf(" (+%d more)", extra)
	} else if more {
		suffix = " (+more)"
	}
	return strings.Join(shown, ", ") + suffix
}

func (m *Manager) removalBlocker(ctx context.Context, wt string, skipped []Skipped, gitlinks bool) (*Blocker, error) {
	b, err := m.removalProbe(ctx, wt, skipped, gitlinks)
	if err != nil {
		if fatal(ctx, err) {
			return nil, err
		}
		return &Blocker{Action: KeepUnarchivable, Reason: "could not verify the tree is safe to remove: " + err.Error()}, nil
	}
	return b, nil
}

func (m *Manager) removalProbe(ctx context.Context, wt string, skipped []Skipped, gitlinks bool) (*Blocker, error) {
	why, err := m.submoduleWork(ctx, wt, gitlinks)
	if err != nil {
		return nil, err
	}
	if why != "" {
		return &Blocker{Action: KeepSubmoduleWork, Reason: why}, nil
	}
	scan, err := m.scanTree(ctx, wt)
	if err != nil {
		return nil, err
	}
	if len(scan.embedded) > 0 {
		return &Blocker{Action: KeepUnarchivable, Reason: "embedded repository " + scan.embedded[0] + " cannot be archived"}, nil
	}
	if scan.truncated {
		return &Blocker{Action: KeepUnarchivable, Reason: fmt.Sprintf("over %d ignored entries; cannot rule out an embedded repository", maxScanEntries)}, nil
	}
	refs, err := m.privateWorktreeRefs(ctx, wt, nil)
	if err != nil {
		return nil, err
	}
	if len(refs) > 0 {
		return &Blocker{Action: KeepWorktreeRefs, Reason: "per-worktree ref " + namedList(refs, false) + " holds commits no branch has"}, nil
	}
	if len(skipped) > 0 {
		paths := make([]string, 0, len(skipped))
		for _, s := range skipped {
			paths = append(paths, s.Path)
		}
		return &Blocker{Action: KeepLargeUntracked, Reason: "untracked file(s) over the archive size cap: " + namedList(paths, false)}, nil
	}
	return nil, nil
}

func (m *Manager) RemovalBlocked(ctx context.Context, path string) (*Blocker, error) {
	return m.removalBlocker(ctx, path, nil, false)
}

func (m *Manager) HiddenIndexEdits(ctx context.Context, path string) ([]string, error) {
	return m.hiddenIndexEdits(ctx, path, nil)
}
