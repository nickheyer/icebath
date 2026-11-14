package worktree

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

const (
	registryLockWait = 5 * time.Second
	tuiRecordMax     = 500
)

func readJSONObject(path string) map[string]json.RawMessage {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]json.RawMessage{}
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		return map[string]json.RawMessage{}
	}
	return top
}

func repoExists(repo string) bool {
	_, err := os.Stat(filepath.Join(repo, ".git"))
	return err == nil
}

func readRepos(path string) []string {
	raw, ok := readJSONObject(path)["repos"]
	if !ok {
		return nil
	}
	var list []any
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	var repos []string
	for _, item := range list {
		if s, ok := item.(string); ok && s != "" {
			repos = append(repos, s)
		}
	}
	return repos
}

func (m *Manager) registryPath(ctx context.Context) (string, error) {
	h, err := m.home(ctx)
	if err != nil {
		return "", err
	}
	return home.WorktreeRepos.In(h), nil
}

func (m *Manager) RegisteredRepos(ctx context.Context) ([]string, error) {
	path, err := m.registryPath(ctx)
	if err != nil {
		return nil, err
	}
	var repos []string
	for _, r := range readRepos(path) {
		if repoExists(r) {
			repos = append(repos, r)
		}
	}
	return repos, nil
}

func lockRegistry(ctx context.Context, path, message string) (*fileutil.Lock, error) {
	lctx, cancel := context.WithTimeout(ctx, registryLockWait)
	defer cancel()
	return fileutil.OpenLock(lctx, path+".lock", fileutil.Range(0, 1), fileutil.TimeoutMessage(message))
}

func (m *Manager) RecordRepo(ctx context.Context, repoRoot string) error {
	if repoRoot == "" {
		return nil
	}
	repo := resolvePath(repoRoot)
	path, err := m.registryPath(ctx)
	if err != nil {
		return err
	}
	lock, err := lockRegistry(ctx, path, "Timed out waiting for the worktree registry lock")
	if err != nil {
		return err
	}
	defer lock.Release()
	current := readRepos(path)
	var kept []string
	seen := map[string]bool{}
	for _, r := range current {
		if seen[r] {
			continue
		}
		seen[r] = true
		if repoExists(r) {
			kept = append(kept, r)
		}
	}
	if slices.Contains(kept, repo) && slices.Equal(kept, current) {
		return nil
	}
	if !slices.Contains(kept, repo) {
		kept = append(kept, repo)
	}
	body, err := encodeIndented(struct {
		Version int      `json:"version"`
		Repos   []string `json:"repos"`
	}{1, kept}, "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteText(ctx, path, body, fileutil.KeepPerm(), fileutil.KeepOwner())
}

func (m *Manager) recordQuietly(ctx context.Context, repoRoot string) error {
	if err := m.RecordRepo(ctx, repoRoot); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Debug("worktree registry update failed", "repo", repoRoot, "error", err)
	}
	return nil
}

func treeKey(path string) string {
	return normCase(resolvePath(path))
}

type tuiEntry struct {
	key string
	raw json.RawMessage
}

func (m *Manager) tuiRecordPath(ctx context.Context) (string, error) {
	h, err := m.home(ctx)
	if err != nil {
		return "", err
	}
	return home.TUIWorktrees.In(h), nil
}

func readTUITrees(path string) ([]tuiEntry, error) {
	raw, ok := readJSONObject(path)["trees"]
	if !ok {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, nil
	}
	var entries []tuiEntry
	index := map[string]int{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := t.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		if i, ok := index[key]; ok {
			entries[i].raw = value
			continue
		}
		index[key] = len(entries)
		entries = append(entries, tuiEntry{key: key, raw: value})
	}
	return entries, nil
}

func entryFields(raw json.RawMessage) (map[string]any, bool) {
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil || v == nil {
		return nil, false
	}
	return v, true
}

func fieldString(v map[string]any, key string) string {
	switch x := v[key].(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if !x {
			return ""
		}
		return "True"
	default:
		return fmt.Sprint(x)
	}
}

func (m *Manager) RecordTUITree(ctx context.Context, t *Tree) error {
	path, err := m.tuiRecordPath(ctx)
	if err != nil {
		return err
	}
	lock, err := lockRegistry(ctx, path, "Timed out waiting for the TUI worktree record lock")
	if err != nil {
		return err
	}
	defer lock.Release()
	entries, err := readTUITrees(path)
	if err != nil {
		entries = nil
	}
	var kept []tuiEntry
	for _, e := range entries {
		v, ok := entryFields(e.raw)
		if !ok || !repoExists(fieldString(v, "repo_root")) {
			continue
		}
		kept = append(kept, e)
	}
	value, err := json.Marshal(struct {
		Path     string `json:"path"`
		Branch   string `json:"branch"`
		RepoRoot string `json:"repo_root"`
	}{t.Path, t.Branch, t.RepoRoot})
	if err != nil {
		return err
	}
	key := treeKey(t.Path)
	replaced := false
	for i := range kept {
		if kept[i].key == key {
			kept[i].raw, replaced = value, true
		}
	}
	if !replaced {
		kept = append(kept, tuiEntry{key: key, raw: value})
	}
	if len(kept) > tuiRecordMax {
		kept = kept[len(kept)-tuiRecordMax:]
	}
	var compact bytes.Buffer
	compact.WriteString(`{"version":1,"trees":{`)
	for i, e := range kept {
		if i > 0 {
			compact.WriteByte(',')
		}
		compact.WriteString(jsonString(e.key))
		compact.WriteByte(':')
		if err := json.Compact(&compact, e.raw); err != nil {
			return err
		}
	}
	compact.WriteString("}}")
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, compact.Bytes(), "", "  "); err != nil {
		return err
	}
	return fileutil.WriteText(ctx, path, pretty.String(), fileutil.KeepPerm(), fileutil.KeepOwner())
}

func (m *Manager) recordedTreeFor(ctx context.Context, cwd string) (*Tree, error) {
	path, err := m.tuiRecordPath(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := readTUITrees(path)
	if err != nil {
		return nil, nil
	}
	byKey := map[string]json.RawMessage{}
	for _, e := range entries {
		byKey[e.key] = e.raw
	}
	candidate := cwd
	for {
		raw, ok := byKey[treeKey(candidate)]
		if ok {
			if v, isMap := entryFields(raw); isMap {
				repo := fieldString(v, "repo_root")
				tree := fieldString(v, "path")
				branch := fieldString(v, "branch")
				if branch != "" && repoExists(repo) && treeKey(filepath.Dir(tree)) == treeKey(filepath.Join(repo, ".worktrees")) && treeKey(tree) == treeKey(candidate) {
					return &Tree{Path: tree, Branch: branch, RepoRoot: repo}, nil
				}
				return nil, nil
			}
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return nil, nil
		}
		candidate = parent
	}
}

func (m *Manager) RecreateTUITree(ctx context.Context, cwd, sessionID string) (string, error) {
	if cwd == "" {
		return "", nil
	}
	if info, err := os.Stat(cwd); err == nil && info.IsDir() {
		return "", nil
	}
	entry, err := m.recordedTreeFor(ctx, cwd)
	if err != nil || entry == nil {
		return "", err
	}
	if _, err := os.Lstat(entry.Path); err == nil {
		return "", nil
	}
	env, trusted, err := m.repoEnv(ctx, entry.RepoRoot)
	if err != nil {
		return "", err
	}
	if !trusted {
		return "", nil
	}
	h, err := m.home(ctx)
	if err != nil {
		return "", err
	}
	if err := m.gitQuiet(ctx, entry.RepoRoot, 15*time.Second, env, "worktree prune before recreate failed", "worktree", "prune"); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(entry.Path), 0o777); err != nil {
		slog.Warn("could not recreate worktree", "path", entry.Path, "error", err)
		return "", nil
	}
	_, exists, err := m.gitOut(ctx, entry.RepoRoot, 10*time.Second, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+entry.Branch)
	if err != nil {
		if fatal(ctx, err) {
			return "", err
		}
		slog.Warn("could not recreate worktree", "path", entry.Path, "error", err)
		return "", nil
	}
	args := []string{"worktree", "add", entry.Path, entry.Branch}
	if !exists {
		args = []string{"worktree", "add", entry.Path, "-b", entry.Branch, "HEAD"}
	}
	r, err := m.git(ctx, entry.RepoRoot, 120*time.Second, env, args...)
	if err != nil {
		if fatal(ctx, err) {
			return "", err
		}
		slog.Warn("could not recreate worktree", "path", entry.Path, "error", err)
		return "", nil
	}
	if !r.ok() {
		slog.Warn("could not recreate worktree", "path", entry.Path, "stderr", r.errText())
		return "", nil
	}
	m.copyIncludes(entry.RepoRoot, entry.Path)
	if _, err := m.RetainForSession(ctx, entry.RepoRoot, entry.Path, sessionID, home.StateDB.In(h)); err != nil {
		if fatal(ctx, err) {
			return "", err
		}
		slog.Warn("could not recreate worktree", "path", entry.Path, "error", err)
		return "", nil
	}
	return entry.Path, nil
}

func (m *Manager) RetainForSession(ctx context.Context, repoRoot, worktreePath, sessionID, db string) (bool, error) {
	if err := m.gitQuiet(ctx, repoRoot, 10*time.Second, nil, "worktree unlock before retain failed", "worktree", "unlock", worktreePath); err != nil {
		return false, err
	}
	r, err := m.git(ctx, repoRoot, 10*time.Second, nil, "worktree", "lock", "--reason", "icebath session="+sessionID+" db="+db, worktreePath)
	if err != nil {
		if fatal(ctx, err) {
			return false, err
		}
		slog.Debug("worktree retain lock failed", "path", worktreePath, "error", err)
		return false, nil
	}
	return r.ok(), nil
}
