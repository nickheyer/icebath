package worktree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

const mergeCacheMax = 1000

var (
	remoteTrunkCandidates = []string{"origin/HEAD", "origin/main", "origin/master"}
	localTrunkCandidates  = []string{"main", "master"}
)

func (m *Manager) localTrunk(ctx context.Context, path string, timeout time.Duration) (string, bool, error) {
	for _, name := range localTrunkCandidates {
		out, ok, err := m.gitOut(ctx, path, timeout, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
		if err != nil {
			return "", false, err
		}
		if ok && out != "" {
			return name, true, nil
		}
	}
	porcelain, _, err := m.gitOut(ctx, path, timeout, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return "", false, err
	}
	first, _, _ := strings.Cut(porcelain, "\n\n")
	for _, line := range strings.Split(first, "\n") {
		if strings.HasPrefix(line, "branch refs/heads/") {
			name := strings.TrimSpace(strings.TrimPrefix(line, "branch refs/heads/"))
			return name, name != "", nil
		}
	}
	return "", false, nil
}

func (m *Manager) mergeBaseRef(ctx context.Context, path string, timeout time.Duration) (string, bool, error) {
	for _, cand := range remoteTrunkCandidates {
		out, ok, err := m.gitOut(ctx, path, timeout, nil, "rev-parse", "--verify", "--quiet", cand)
		if err != nil {
			return "", false, err
		}
		if ok && out != "" {
			return cand, true, nil
		}
	}
	refs, ok, err := m.gitOut(ctx, path, timeout, nil, "for-each-ref", "--format=%(refname)", "refs/remotes")
	if err != nil {
		return "", false, err
	}
	if ok && refs == "" {
		return m.localTrunk(ctx, path, timeout)
	}
	return "", false, nil
}

func (m *Manager) hasUnpushedCommits(ctx context.Context, path string, timeout time.Duration) (bool, error) {
	unpushed, err := m.unpushedProbe(ctx, path, timeout)
	if err != nil {
		if fatal(ctx, err) {
			return false, err
		}
		return true, nil
	}
	return unpushed, nil
}

func (m *Manager) unpushedProbe(ctx context.Context, path string, timeout time.Duration) (bool, error) {
	remoteRefs, ok, err := m.gitOut(ctx, path, timeout, nil, "for-each-ref", "--format=%(refname)", "refs/remotes")
	if err != nil {
		return true, err
	}
	if !ok {
		return true, nil
	}
	baseline := []string{"--remotes"}
	if remoteRefs == "" {
		trunk, found, err := m.localTrunk(ctx, path, timeout)
		if err != nil {
			return true, err
		}
		if !found {
			return true, nil
		}
		baseline = []string{trunk}
	}
	args := append([]string{"log", "--oneline", "HEAD", "--not"}, baseline...)
	out, ok, err := m.gitOut(ctx, path, timeout, nil, args...)
	if err != nil {
		return true, err
	}
	return !ok || out != "", nil
}

func purePosix(entry string) string {
	parts := strings.Split(filepath.ToSlash(entry), "/")
	kept := parts[:0]
	for i, p := range parts {
		if p == "" && i != 0 {
			continue
		}
		if p == "." {
			continue
		}
		kept = append(kept, p)
	}
	out := strings.Join(kept, "/")
	if out == "" {
		return "."
	}
	return out
}

func (m *Manager) includeSymlinkPaths(worktreePath, repoRoot string) (map[string]bool, error) {
	entries, err := worktreeIncludeEntries(repoRoot)
	if err != nil {
		return nil, err
	}
	paths := map[string]bool{}
	for _, entry := range entries {
		dst := filepath.Join(worktreePath, entry)
		info, err := os.Lstat(dst)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if resolvePath(dst) == resolvePath(filepath.Join(repoRoot, entry)) {
			paths[strings.TrimRight(purePosix(entry), "/")] = true
		}
	}
	return paths, nil
}

func (m *Manager) isDirty(ctx context.Context, worktreePath, repoRoot string, timeout time.Duration) (bool, error) {
	dirty, err := m.dirtyProbe(ctx, worktreePath, repoRoot, timeout)
	if err != nil {
		if fatal(ctx, err) {
			return false, err
		}
		return true, nil
	}
	return dirty, nil
}

func (m *Manager) dirtyProbe(ctx context.Context, worktreePath, repoRoot string, timeout time.Duration) (bool, error) {
	env, trusted, err := m.repoEnv(ctx, worktreePath)
	if err != nil {
		return true, err
	}
	if !trusted {
		return true, nil
	}
	r, err := m.run(ctx, "git", []string{"status", "--porcelain", "-z", "--untracked-files=normal"}, call{dir: worktreePath, env: env, timeout: timeout})
	if err != nil {
		return true, err
	}
	if !r.ok() {
		return true, nil
	}
	var entries []string
	for _, e := range strings.Split(lineText(r.stdout), "\x00") {
		if e != "" {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		edits, err := m.hiddenIndexEdits(ctx, worktreePath, nil)
		if err != nil {
			return true, err
		}
		return len(edits) > 0, nil
	}
	for _, e := range entries {
		if !strings.HasPrefix(e, "?? ") {
			return true, nil
		}
	}
	links, err := m.includeSymlinkPaths(worktreePath, repoRoot)
	if err != nil {
		return true, err
	}
	for _, e := range entries {
		if !links[strings.TrimRight(e[3:], "/")] {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) repoIsShallow(ctx context.Context, path string, timeout time.Duration) (bool, error) {
	out, ok, err := m.gitOut(ctx, path, timeout, nil, "rev-parse", "--is-shallow-repository")
	if err != nil {
		if fatal(ctx, err) {
			return false, err
		}
		return false, nil
	}
	return ok && out == "true", nil
}

func (m *Manager) deepenShallowRepo(ctx context.Context, repoRoot string, timeout time.Duration) (bool, error) {
	shallow, err := m.repoIsShallow(ctx, repoRoot, 5*time.Second)
	if err != nil {
		return false, err
	}
	if !shallow {
		return true, nil
	}
	done, err := m.unshallow(ctx, repoRoot, timeout)
	if err != nil {
		if fatal(ctx, err) {
			return false, err
		}
		slog.Debug("Deepening shallow repo failed (non-fatal)", "error", err)
		return false, nil
	}
	if !done {
		return false, nil
	}
	shallow, err = m.repoIsShallow(ctx, repoRoot, 5*time.Second)
	if err != nil {
		return false, err
	}
	if !shallow {
		slog.Info("Deepened shallow clone so worktree cleanup can verify push state", "repo", repoRoot)
	}
	return !shallow, nil
}

func (m *Manager) unshallow(ctx context.Context, repoRoot string, timeout time.Duration) (bool, error) {
	remotes, ok, err := m.gitOut(ctx, repoRoot, 10*time.Second, nil, "remote")
	if err != nil {
		return false, err
	}
	if !ok || remotes == "" {
		return false, nil
	}
	var names []string
	for _, r := range strings.Split(remotes, "\n") {
		if r = strings.TrimSpace(r); r != "" {
			names = append(names, r)
		}
	}
	remote := names[0]
	for _, n := range names {
		if n == "origin" {
			remote = "origin"
			break
		}
	}
	defer markUnmarkedPacksPromisor(repoRoot)
	env := m.noninteractiveEnv(ctx)
	for _, extra := range [][]string{{"--filter=blob:none"}, nil} {
		args := append([]string{"fetch", remote, "--unshallow"}, extra...)
		r, err := m.git(ctx, repoRoot, timeout, env, args...)
		if err != nil {
			var deadline *DeadlineError
			if errors.As(err, &deadline) {
				return false, nil
			}
			return false, err
		}
		if r.ok() {
			break
		}
		slog.Debug("git fetch --unshallow failed", "extra", strings.Join(extra, " "), "stderr", tail(r.errText(), 500))
	}
	return true, nil
}

func markUnmarkedPacksPromisor(repoRoot string) int {
	marked := 0
	packs, _ := filepath.Glob(filepath.Join(packDir(repoRoot), "pack-*.pack"))
	for _, pack := range packs {
		marker := strings.TrimSuffix(pack, ".pack") + ".promisor"
		f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if err != nil {
			if !os.IsExist(err) {
				slog.Warn("Could not mark pack as a partial-clone pack", "pack", pack, "error", err)
			}
			continue
		}
		f.Close()
		marked++
	}
	if marked > 0 {
		slog.Info("Marked packs as partial-clone packs", "count", marked, "repo", repoRoot)
	}
	return marked
}

func packDir(repoRoot string) string {
	gitDir := filepath.Join(repoRoot, ".git")
	if info, err := os.Stat(gitDir); err == nil && info.IsDir() {
		return filepath.Join(gitDir, "objects", "pack")
	}
	return filepath.Join(repoRoot, "objects", "pack")
}

type verdictCache struct {
	mu   sync.Mutex
	keys []string
	vals map[string]bool
}

func (c *verdictCache) get(key string) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.vals[key]
	return v, ok
}

func (c *verdictCache) set(key string, v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.vals[key]; !ok {
		c.keys = append(c.keys, key)
	}
	c.vals[key] = v
}

func (c *verdictCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.keys)
}

func (m *Manager) mergeCachePath(ctx context.Context) (string, error) {
	h, err := m.home(ctx)
	if err != nil {
		return "", err
	}
	return home.WorktreeMergeVerdicts.In(h), nil
}

func (m *Manager) loadMergeCache(ctx context.Context) (*verdictCache, error) {
	c := &verdictCache{vals: map[string]bool{}}
	path, err := m.mergeCachePath(ctx)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c, nil
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	keys, vals, ok := decodeVerdicts(data)
	if ok {
		c.keys, c.vals = keys, vals
	}
	return c, nil
}

func decodeVerdicts(data []byte) ([]string, map[string]bool, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, nil, false
	}
	var keys []string
	var vals map[string]bool
	found := false
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, nil, false
		}
		name, _ := t.(string)
		if name != "verdicts" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, nil, false
			}
			continue
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, nil, false
		}
		keys, vals, found = decodeOrderedBools(raw)
	}
	if _, err := dec.Token(); err != nil {
		return nil, nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil, false
	}
	return keys, vals, found
}

func decodeOrderedBools(raw json.RawMessage) ([]string, map[string]bool, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, nil, false
	}
	var keys []string
	vals := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, nil, false
		}
		key, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, false
		}
		var b bool
		s := string(bytes.TrimSpace(v))
		if s != "true" && s != "false" {
			if _, ok := vals[key]; ok {
				delete(vals, key)
				for i, k := range keys {
					if k == key {
						keys = append(keys[:i], keys[i+1:]...)
						break
					}
				}
			}
			continue
		}
		b = s == "true"
		if _, ok := vals[key]; !ok {
			keys = append(keys, key)
		}
		vals[key] = b
	}
	return keys, vals, true
}

func jsonString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return strconv.Quote(s)
	}
	return strings.TrimRight(buf.String(), "\n")
}

func (m *Manager) saveMergeCache(ctx context.Context, c *verdictCache) error {
	c.mu.Lock()
	keys := c.keys
	if len(keys) > mergeCacheMax {
		keys = keys[len(keys)-mergeCacheMax:]
	}
	var b strings.Builder
	b.WriteString(`{"version": 1, "verdicts": {`)
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s: %t", jsonString(k), c.vals[k])
	}
	b.WriteString("}}")
	c.mu.Unlock()
	path, err := m.mergeCachePath(ctx)
	if err != nil {
		return err
	}
	if err := m.homes.Mkdir(ctx, filepath.Dir(path)); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Debug("Could not persist worktree merge cache", "error", err)
		return nil
	}
	if err := fileutil.WriteText(ctx, path, b.String(), fileutil.KeepPerm(), fileutil.KeepOwner()); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Debug("Could not persist worktree merge cache", "error", err)
	}
	return nil
}

func (m *Manager) commitsAllMergedUpstream(ctx context.Context, path string, timeout time.Duration, maxAhead int, cache *verdictCache) (bool, error) {
	merged, err := m.mergedProbe(ctx, path, timeout, maxAhead, cache)
	if err != nil {
		if fatal(ctx, err) {
			return false, err
		}
		return false, nil
	}
	return merged, nil
}

func (m *Manager) mergedProbe(ctx context.Context, path string, timeout time.Duration, maxAhead int, cache *verdictCache) (bool, error) {
	base, found, err := m.mergeBaseRef(ctx, path, timeout)
	if err != nil || !found {
		return false, err
	}
	cacheKey := ""
	if cache != nil {
		revs, _, err := m.gitOut(ctx, path, timeout, nil, "rev-parse", base+"^{commit}", "HEAD^{commit}")
		if err != nil {
			return false, err
		}
		shas := strings.Fields(revs)
		if len(shas) == 2 {
			cacheKey = fmt.Sprintf("%s..%s:%d", shas[0], shas[1], maxAhead)
			if v, ok := cache.get(cacheKey); ok {
				return v, nil
			}
		}
	}
	memo := func(v bool) bool {
		if cache != nil && cacheKey != "" {
			cache.set(cacheKey, v)
		}
		return v
	}
	ahead, ok, err := m.gitOut(ctx, path, timeout, nil, "rev-list", "--count", base+"..HEAD")
	if err != nil || !ok {
		return false, err
	}
	if ahead == "" {
		ahead = "0"
	}
	count, err := strconv.Atoi(ahead)
	if err != nil {
		return false, nil
	}
	if count == 0 {
		return memo(true), nil
	}
	if count > maxAhead {
		return memo(false), nil
	}
	cherry, err := m.git(ctx, path, timeout, nil, "cherry", base, "HEAD")
	if err != nil {
		return false, err
	}
	if !cherry.ok() {
		return false, nil
	}
	var lines []string
	for _, ln := range strings.Split(cherry.text(), "\n") {
		if strings.TrimSpace(ln) != "" {
			lines = append(lines, ln)
		}
	}
	all := len(lines) > 0
	for _, ln := range lines {
		if !strings.HasPrefix(ln, "-") {
			all = false
			break
		}
	}
	return memo(all), nil
}

func (m *Manager) currentBranch(ctx context.Context, path string, timeout time.Duration) (string, bool, error) {
	branch, ok, err := m.gitOut(ctx, path, timeout, nil, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", false, err
	}
	if !ok || branch == "" || branch == "HEAD" {
		return "", false, nil
	}
	return branch, true, nil
}

func (m *Manager) branchPRMerged(ctx context.Context, path string, timeout time.Duration, cache *verdictCache) (bool, error) {
	merged, err := m.prMergedProbe(ctx, path, timeout, cache)
	if err != nil {
		if fatal(ctx, err) {
			return false, err
		}
		return false, nil
	}
	return merged, nil
}

func (m *Manager) prMergedProbe(ctx context.Context, path string, timeout time.Duration, cache *verdictCache) (bool, error) {
	branch, ok, err := m.currentBranch(ctx, path, timeout)
	if err != nil || !ok {
		return false, err
	}
	cacheKey := ""
	if cache != nil {
		sha, _, err := m.gitOut(ctx, path, timeout, nil, "rev-parse", "HEAD")
		if err != nil {
			return false, err
		}
		if sha != "" {
			cacheKey = "pr-merged:" + branch + ":" + sha
			if v, ok := cache.get(cacheKey); ok && v {
				return true, nil
			}
		}
	}
	r, err := m.run(ctx, "gh", []string{"pr", "list", "--head", branch, "--state", "merged", "--json", "number", "--limit", "1"}, call{dir: path, timeout: timeout})
	if err != nil {
		return false, err
	}
	if !r.ok() {
		return false, nil
	}
	body := strings.TrimSpace(r.text())
	if body == "" {
		body = "[]"
	}
	var prs any
	if err := json.Unmarshal([]byte(body), &prs); err != nil {
		return false, nil
	}
	list, isList := prs.([]any)
	merged := isList && len(list) > 0
	if merged && cache != nil && cacheKey != "" {
		cache.set(cacheKey, true)
	}
	return merged, nil
}

func (m *Manager) fetchRemoteHeads(ctx context.Context, repoRoot string, timeout time.Duration) (map[string]string, bool, error) {
	r, err := m.git(ctx, repoRoot, timeout, m.noninteractiveEnv(ctx), "ls-remote", "--heads", "origin")
	if err != nil {
		if fatal(ctx, err) {
			return nil, false, err
		}
		return nil, false, nil
	}
	if !r.ok() {
		return nil, false, nil
	}
	heads := map[string]string{}
	for _, line := range strings.Split(r.text(), "\n") {
		sha, ref, found := strings.Cut(line, "\t")
		if !found || !strings.HasPrefix(ref, "refs/heads/") {
			continue
		}
		heads[strings.TrimSpace(strings.TrimPrefix(ref, "refs/heads/"))] = strings.TrimSpace(sha)
	}
	return heads, true, nil
}

func (m *Manager) IsDirty(ctx context.Context, worktreePath, repoRoot string) (bool, error) {
	return m.isDirty(ctx, worktreePath, repoRoot, 10*time.Second)
}

func (m *Manager) HasUnpushedCommits(ctx context.Context, worktreePath string) (bool, error) {
	return m.hasUnpushedCommits(ctx, worktreePath, 10*time.Second)
}

func (m *Manager) RepoIsShallow(ctx context.Context, repoRoot string) (bool, error) {
	return m.repoIsShallow(ctx, repoRoot, 5*time.Second)
}

func (m *Manager) ReleaseLSP(ctx context.Context, worktreePath string) error {
	return m.releaseLSP(ctx, worktreePath)
}

func (m *Manager) branchPushedExact(ctx context.Context, path string, heads map[string]string, timeout time.Duration) (bool, error) {
	if len(heads) == 0 {
		return false, nil
	}
	branch, ok, err := m.currentBranch(ctx, path, timeout)
	if err != nil {
		if fatal(ctx, err) {
			return false, err
		}
		return false, nil
	}
	if !ok {
		return false, nil
	}
	remoteSHA := heads[branch]
	if remoteSHA == "" {
		return false, nil
	}
	head, _, err := m.gitOut(ctx, path, timeout, nil, "rev-parse", "HEAD")
	if err != nil {
		if fatal(ctx, err) {
			return false, err
		}
		return false, nil
	}
	return head == remoteSHA, nil
}
