package worktree

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	ArchivePrefix   = "refs/icebath/archive/"
	archiveSubject  = "icebath worktree archive: "
	submoduleReason = "submodule "
	archiveTimeout  = 300 * time.Second
)

var refUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func archiveIdentity() []configPair {
	return []configPair{
		{"GIT_AUTHOR_NAME", "icebath worktree archive"},
		{"GIT_AUTHOR_EMAIL", "icebath-archive@localhost"},
		{"GIT_COMMITTER_NAME", "icebath worktree archive"},
		{"GIT_COMMITTER_EMAIL", "icebath-archive@localhost"},
	}
}

func rawConfig() []configPair {
	return []configPair{{"core.autocrlf", "false"}, {"core.safecrlf", "false"}, {"core.eol", "lf"}}
}

type Skipped struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

type Archive struct {
	Ref     string         `json:"ref"`
	Name    string         `json:"name"`
	SHA     string         `json:"sha"`
	Created float64        `json:"created"`
	Meta    map[string]any `json:"meta"`
	Staged  string         `json:"staged"`
}

func (a *Archive) metaString(key string) string {
	s, _ := a.Meta[key].(string)
	return s
}

func (a *Archive) Branch() string { return a.metaString("branch") }

func (a *Archive) Reason() string { return a.metaString("reason") }

func (a *Archive) Skipped() []Skipped {
	list, _ := a.Meta["skipped_untracked"].([]any)
	var out []Skipped
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		path, _ := entry["path"].(string)
		var size int64
		switch v := entry["bytes"].(type) {
		case json.Number:
			size, _ = v.Int64()
		case float64:
			size = int64(v)
		}
		out = append(out, Skipped{Path: path, Bytes: size})
	}
	return out
}

func (m *Manager) archiveEnv(ctx context.Context, cwd, index string) (map[string]string, error) {
	env, trusted, err := m.repoEnv(ctx, cwd)
	if err != nil {
		return nil, err
	}
	if !trusted {
		return nil, &ArchiveError{Msg: filterDiscoveryText}
	}
	for _, p := range archiveIdentity() {
		env[envKey(p.key)] = p.value
	}
	appendConfig(env, rawConfig())
	if index != "" {
		env[envKey("GIT_INDEX_FILE")] = index
	}
	return env, nil
}

func withOptionalLocksOff(env map[string]string) map[string]string {
	out := cloneEnv(env)
	out[envKey("GIT_OPTIONAL_LOCKS")] = "0"
	return out
}

func (m *Manager) archiveRun(ctx context.Context, cwd string, env map[string]string, timeout time.Duration, input []byte, args ...string) (*result, error) {
	return m.run(ctx, "git", args, call{dir: cwd, env: env, timeout: timeout, input: input})
}

func archiveFailure(sub string, r *result) error {
	return &ArchiveError{Msg: fmt.Sprintf("git %s failed: %s", sub, tail(r.errText(), 300))}
}

func (m *Manager) archiveOK(ctx context.Context, cwd string, env map[string]string, timeout time.Duration, input []byte, args ...string) (string, error) {
	r, err := m.archiveRun(ctx, cwd, env, timeout, input, args...)
	if err != nil {
		return "", err
	}
	if !r.ok() {
		return "", archiveFailure(args[0], r)
	}
	return string(r.stdout), nil
}

func (m *Manager) rawAttrs(ctx context.Context, cwd string, env map[string]string, emptyTree string, args ...string) error {
	r, err := m.archiveRun(ctx, cwd, env, archiveTimeout, nil, append([]string{"--attr-source", emptyTree}, args...)...)
	if err != nil {
		return err
	}
	if !r.ok() && strings.Contains(string(r.stderr), "attr-source") {
		r, err = m.archiveRun(ctx, cwd, env, archiveTimeout, nil, args...)
		if err != nil {
			return err
		}
	}
	if !r.ok() {
		return archiveFailure(args[0], r)
	}
	return nil
}

type indexEntry struct {
	mode string
	sha  string
	path string
}

func (m *Manager) regularEntries(ctx context.Context, cwd string, env map[string]string) ([]indexEntry, error) {
	out, err := m.archiveOK(ctx, cwd, env, archiveTimeout, nil, "ls-files", "-s", "-z")
	if err != nil {
		return nil, err
	}
	var entries []indexEntry
	for _, record := range strings.Split(out, "\x00") {
		meta, path, _ := strings.Cut(record, "\t")
		parts := strings.Fields(meta)
		if path != "" && len(parts) == 3 && (parts[0] == "100644" || parts[0] == "100755") {
			entries = append(entries, indexEntry{mode: parts[0], sha: parts[1], path: path})
		}
	}
	return entries, nil
}

func isRegularNotLink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func (m *Manager) rehashRaw(ctx context.Context, wt string, tenv map[string]string) error {
	all, err := m.regularEntries(ctx, wt, tenv)
	if err != nil {
		return err
	}
	var entries []indexEntry
	for _, e := range all {
		if isRegularNotLink(filepath.Join(wt, e.path)) {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return nil
	}
	var batch []string
	for _, e := range entries {
		if !strings.Contains(e.path, "\n") && !strings.HasPrefix(e.path, `"`) {
			batch = append(batch, e.path)
		}
	}
	byPath := map[string]string{}
	if len(batch) > 0 {
		var data strings.Builder
		for _, p := range batch {
			data.WriteString(p + "\n")
		}
		out, err := m.archiveOK(ctx, wt, tenv, archiveTimeout, []byte(data.String()), "hash-object", "-w", "--no-filters", "--stdin-paths")
		if err != nil {
			return err
		}
		shas := strings.Fields(out)
		for i, p := range batch {
			if i >= len(shas) {
				break
			}
			byPath[p] = shas[i]
		}
	}
	for _, e := range entries {
		if _, ok := byPath[e.path]; ok {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(wt, e.path))
		if err != nil {
			return err
		}
		out, err := m.archiveOK(ctx, wt, tenv, archiveTimeout, raw, "hash-object", "-w", "--no-filters", "--stdin")
		if err != nil {
			return err
		}
		byPath[e.path] = strings.TrimSpace(out)
	}
	var info bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&info, "%s %s\t%s\x00", e.mode, byPath[e.path], e.path)
	}
	_, err = m.archiveOK(ctx, wt, tenv, archiveTimeout, info.Bytes(), "update-index", "-z", "--index-info")
	return err
}

func (m *Manager) writeRaw(ctx context.Context, dest string, env map[string]string) error {
	entries, err := m.regularEntries(ctx, dest, env)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	var req bytes.Buffer
	for _, e := range entries {
		req.WriteString(e.sha + "\n")
	}
	r, err := m.archiveRun(ctx, dest, env, archiveTimeout, req.Bytes(), "cat-file", "--batch")
	if err != nil {
		return err
	}
	if !r.ok() {
		return archiveFailure("cat-file", r)
	}
	out := r.stdout
	pos := 0
	for _, e := range entries {
		nl := bytes.IndexByte(out[pos:], '\n')
		if nl < 0 {
			return &ArchiveError{Msg: "git cat-file returned a truncated batch for " + e.path}
		}
		header := strings.Fields(string(out[pos : pos+nl]))
		if len(header) < 3 {
			return &ArchiveError{Msg: "git cat-file could not read object " + e.sha + " for " + e.path}
		}
		size, err := strconv.Atoi(header[2])
		if err != nil || pos+nl+1+size > len(out) {
			return &ArchiveError{Msg: "git cat-file returned a malformed object for " + e.path}
		}
		body := out[pos+nl+1 : pos+nl+1+size]
		pos = pos + nl + 1 + size + 1
		target := filepath.Join(dest, e.path)
		if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(target); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
			return err
		}
		var mode os.FileMode
		hadMode := false
		if info, err := os.Stat(target); err == nil {
			mode, hadMode = info.Mode().Perm(), true
		}
		if err := os.WriteFile(target, body, 0o666); err != nil {
			return err
		}
		if hadMode {
			if err := os.Chmod(target, mode); err != nil {
				return err
			}
		}
	}
	return nil
}

func refSafe(name string) string {
	safe := strings.Trim(refUnsafe.ReplaceAllString(name, "-"), "-.")
	if safe == "" {
		safe = "tree"
	}
	return strings.TrimSuffix(strings.ReplaceAll(safe, "..", "-"), ".lock")
}

func (m *Manager) untracked(ctx context.Context, wt string, env map[string]string) ([]string, error) {
	out, err := m.archiveOK(ctx, wt, env, archiveTimeout, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

func (m *Manager) submoduleWork(ctx context.Context, wt string, gitlinks bool) (string, error) {
	base, err := m.archiveEnv(ctx, wt, "")
	if err != nil {
		return "", err
	}
	env := withOptionalLocksOff(base)
	gitDir, err := m.archiveOK(ctx, wt, env, archiveTimeout, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	modules := filepath.Join(strings.TrimSpace(gitDir), "modules")
	if info, err := os.Stat(modules); err == nil && info.IsDir() {
		items, err := os.ReadDir(modules)
		if err != nil {
			return "", err
		}
		if len(items) > 0 {
			return submoduleReason + "repository in " + modules + " would be deleted with the tree", nil
		}
	}
	staged, err := m.archiveOK(ctx, wt, env, archiveTimeout, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return "", err
	}
	for _, e := range strings.Split(staged, "\x00") {
		if !strings.HasPrefix(e, "160000 ") {
			continue
		}
		_, p, found := strings.Cut(e, "\t")
		if !found {
			continue
		}
		if gitlinks {
			return submoduleReason + p + " cannot be archived (gitlink only)", nil
		}
		if _, err := os.Stat(filepath.Join(wt, p, ".git")); err == nil {
			return submoduleReason + p + " cannot be archived (gitlink only)", nil
		}
	}
	return "", nil
}

func (m *Manager) stagedState(ctx context.Context, wt string) (string, error) {
	env, err := m.archiveEnv(ctx, wt, "")
	if err != nil {
		return "", err
	}
	return m.digestIndex(ctx, wt, env)
}

func (m *Manager) digestIndex(ctx context.Context, wt string, env map[string]string) (string, error) {
	out, err := m.archiveOK(ctx, wt, withOptionalLocksOff(env), archiveTimeout, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(out))
	return hex.EncodeToString(sum[:]), nil
}

type Blocker struct {
	Action string
	Reason string
}

func (m *Manager) archivePreflight(ctx context.Context, wt string, maxUntrackedMB float64) (*Blocker, error) {
	env, err := m.archiveEnv(ctx, wt, "")
	if err == nil {
		var paths []string
		paths, err = m.untracked(ctx, wt, env)
		if err == nil {
			skipped := splitOversized(wt, paths, maxUntrackedMB)
			return m.removalBlocker(ctx, wt, skipped, true)
		}
	}
	if fatal(ctx, err) {
		return nil, err
	}
	return &Blocker{Action: KeepUnarchivable, Reason: err.Error()}, nil
}

func splitOversized(wt string, untracked []string, maxMB float64) []Skipped {
	if maxMB <= 0 {
		return nil
	}
	limit := maxMB * 1024 * 1024
	var skipped []Skipped
	for _, rel := range untracked {
		info, err := os.Lstat(filepath.Join(wt, rel))
		if err != nil {
			continue
		}
		if float64(info.Size()) > limit {
			skipped = append(skipped, Skipped{Path: rel, Bytes: info.Size()})
		}
	}
	return skipped
}

func (m *Manager) createRef(ctx context.Context, repoRoot, name, sha string, env map[string]string) (string, error) {
	base := ArchivePrefix + refSafe(name)
	stamp := time.Now().Format("20060102T150405")
	refs := []string{base, base + "--" + stamp}
	for i := 2; i < 50; i++ {
		refs = append(refs, fmt.Sprintf("%s--%s-%d", base, stamp, i))
	}
	for _, ref := range refs {
		r, err := m.archiveRun(ctx, repoRoot, env, archiveTimeout, nil, "update-ref", "-m", "icebath worktree archive", ref, sha, "")
		if err != nil {
			return "", err
		}
		if r.ok() {
			return ref, nil
		}
	}
	return "", &ArchiveError{Msg: "could not create an archive ref for " + name}
}

type archiveMeta struct {
	Version          int       `json:"version"`
	Name             string    `json:"name"`
	Path             string    `json:"path"`
	RepoRoot         string    `json:"repo_root"`
	Branch           *string   `json:"branch"`
	Head             string    `json:"head"`
	IndexCommit      string    `json:"index_commit"`
	Created          int64     `json:"created"`
	Reason           string    `json:"reason"`
	SkippedUntracked []Skipped `json:"skipped_untracked"`
}

func randomHex(n int) (string, error) {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b)[:n], nil
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !isNotExist(err) {
		return err
	}
	return nil
}

func (m *Manager) snapshot(ctx context.Context, repoRoot, wtPath, reason string, maxUntrackedMB float64) (arc *Archive, err error) {
	wt := wtPath
	env, err := m.archiveEnv(ctx, wt, "")
	if err != nil {
		return nil, err
	}
	unsafe, err := m.submoduleWork(ctx, wt, true)
	if err != nil {
		return nil, err
	}
	if unsafe != "" {
		return nil, &ArchiveError{Msg: unsafe}
	}
	headOut, err := m.archiveOK(ctx, wt, env, archiveTimeout, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, err
	}
	head := strings.TrimSpace(headOut)
	sym, err := m.archiveRun(ctx, wt, env, archiveTimeout, nil, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		return nil, err
	}
	var branch *string
	if b := strings.TrimSpace(lineText(sym.stdout)); b != "" {
		branch = &b
	}
	gitDirOut, err := m.archiveOK(ctx, wt, env, archiveTimeout, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	gitDir := strings.TrimSpace(gitDirOut)
	untracked, err := m.untracked(ctx, wt, env)
	if err != nil {
		return nil, err
	}
	for _, p := range untracked {
		if strings.HasSuffix(p, "/") {
			return nil, &ArchiveError{Msg: "untracked embedded repository " + strings.TrimRight(p, "/") + " cannot be archived"}
		}
	}
	skipped := splitOversized(wt, untracked, maxUntrackedMB)
	if skipped == nil {
		skipped = []Skipped{}
	}
	suffix, err := randomHex(8)
	if err != nil {
		return nil, err
	}
	token := fmt.Sprintf("icebath-archive-%d-%s", os.Getpid(), suffix)
	tmpIndex := filepath.Join(gitDir, token+".index")
	pathspec := filepath.Join(gitDir, token+".pathspec")
	var meta archiveMeta
	var sha, staged string
	err = func() (ferr error) {
		defer func() {
			for _, leftover := range []string{tmpIndex, tmpIndex + ".lock", pathspec} {
				if rerr := removeIfPresent(leftover); rerr != nil && ferr == nil {
					ferr = rerr
				}
			}
		}()
		tenv, err := m.archiveEnv(ctx, wt, tmpIndex)
		if err != nil {
			return err
		}
		realIndex := filepath.Join(gitDir, "index")
		if info, statErr := os.Stat(realIndex); statErr == nil && info.Mode().IsRegular() {
			data, err := os.ReadFile(realIndex)
			if err != nil {
				return err
			}
			if err := os.WriteFile(tmpIndex, data, 0o666); err != nil {
				return err
			}
		} else if _, err := m.archiveOK(ctx, wt, tenv, archiveTimeout, nil, "read-tree", head); err != nil {
			return err
		}
		if staged, err = m.digestIndex(ctx, wt, tenv); err != nil {
			return err
		}
		indexTreeOut, err := m.archiveOK(ctx, wt, tenv, archiveTimeout, nil, "write-tree")
		if err != nil {
			return err
		}
		emptyTreeOut, err := m.archiveOK(ctx, wt, env, archiveTimeout, []byte{}, "mktree")
		if err != nil {
			return err
		}
		specs := []string{":(top)"}
		for _, s := range skipped {
			specs = append(specs, ":(top,exclude,literal)"+s.Path)
		}
		if err := os.WriteFile(pathspec, []byte(strings.Join(specs, "\x00")+"\x00"), 0o666); err != nil {
			return err
		}
		if err := m.rawAttrs(ctx, wt, tenv, strings.TrimSpace(emptyTreeOut), "add", "-A", "--pathspec-from-file", pathspec, "--pathspec-file-nul"); err != nil {
			return err
		}
		if err := m.rehashRaw(ctx, wt, tenv); err != nil {
			return err
		}
		workTreeOut, err := m.archiveOK(ctx, wt, tenv, archiveTimeout, nil, "write-tree")
		if err != nil {
			return err
		}
		name := filepath.Base(wt)
		indexCommitOut, err := m.archiveOK(ctx, wt, env, archiveTimeout, nil, "commit-tree", strings.TrimSpace(indexTreeOut), "-p", head, "-m", "index of "+name)
		if err != nil {
			return err
		}
		meta = archiveMeta{
			Version: 1, Name: name, Path: wt, RepoRoot: repoRoot, Branch: branch, Head: head,
			IndexCommit: strings.TrimSpace(indexCommitOut), Created: time.Now().Unix(), Reason: reason,
			SkippedUntracked: skipped,
		}
		body, err := encodeIndented(meta, " ")
		if err != nil {
			return err
		}
		message := archiveSubject + name + "\n\n" + body + "\n"
		shaOut, err := m.archiveOK(ctx, wt, env, archiveTimeout, []byte(message), "commit-tree", strings.TrimSpace(workTreeOut), "-p", head, "-p", meta.IndexCommit)
		if err != nil {
			return err
		}
		sha = strings.TrimSpace(shaOut)
		return nil
	}()
	if err != nil {
		return nil, err
	}
	ref, err := m.createRef(ctx, repoRoot, filepath.Base(wt), sha, env)
	if err != nil {
		return nil, err
	}
	slog.Info("Archived worktree", "path", wt, "ref", ref, "reason", reason)
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	return &Archive{Ref: ref, Name: strings.TrimPrefix(ref, ArchivePrefix), SHA: sha, Created: float64(meta.Created), Meta: decodeMetaMap(metaJSON), Staged: staged}, nil
}

func encodeIndented(v any, indent string) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

func decodeMetaMap(data []byte) map[string]any {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var meta map[string]any
	if err := dec.Decode(&meta); err != nil || meta == nil {
		return map[string]any{}
	}
	if dec.More() {
		return map[string]any{}
	}
	return meta
}

func parseMeta(body string) map[string]any {
	start := strings.Index(body, "{")
	if start < 0 {
		return map[string]any{}
	}
	return decodeMetaMap([]byte(body[start:]))
}

func metaCreated(meta map[string]any, fallback string) (float64, error) {
	switch v := meta["created"].(type) {
	case json.Number:
		if f, err := v.Float64(); err == nil && f != 0 {
			return f, nil
		}
	case string:
		if v != "" {
			return strconv.ParseFloat(strings.TrimSpace(v), 64)
		}
	case bool:
		if v {
			return 1, nil
		}
	}
	if fallback == "" {
		return 0, nil
	}
	return strconv.ParseFloat(strings.TrimSpace(fallback), 64)
}

func (m *Manager) ListArchives(ctx context.Context, repoRoot string) ([]*Archive, error) {
	env, err := m.archiveEnv(ctx, repoRoot, "")
	if err != nil {
		return nil, err
	}
	out, err := m.archiveOK(ctx, repoRoot, env, 60*time.Second, nil, "for-each-ref", "--sort=-creatordate",
		"--format=%(refname)%00%(objectname)%00%(creatordate:unix)%00%(contents:body)%00", ArchivePrefix)
	if err != nil {
		return nil, err
	}
	var archives []*Archive
	for _, record := range strings.Split(lineText([]byte(out)), "\x00\n") {
		parts := strings.Split(record, "\x00")
		if len(parts) < 4 || !strings.HasPrefix(parts[0], ArchivePrefix) {
			continue
		}
		meta := parseMeta(parts[3])
		created, err := metaCreated(meta, parts[2])
		if err != nil {
			return nil, err
		}
		archives = append(archives, &Archive{Ref: parts[0], Name: strings.TrimPrefix(parts[0], ArchivePrefix), SHA: parts[1], Created: created, Meta: meta})
	}
	return archives, nil
}

func (m *Manager) findArchive(ctx context.Context, repoRoot, name string) (*Archive, error) {
	archives, err := m.ListArchives(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	wanted := strings.TrimPrefix(name, ArchivePrefix)
	for _, a := range archives {
		if a.Name == wanted {
			return a, nil
		}
	}
	safe := refSafe(name)
	for _, a := range archives {
		if a.metaString("name") == name || a.Name == safe {
			return a, nil
		}
	}
	return nil, nil
}

func (m *Manager) PruneExpiredArchives(ctx context.Context, repoRoot string, retentionDays float64, dryRun bool) ([]string, error) {
	if retentionDays <= 0 {
		return nil, nil
	}
	cutoff := float64(time.Now().UnixNano())/1e9 - retentionDays*86400
	archives, err := m.ListArchives(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	var expired []*Archive
	for _, a := range archives {
		if a.Created != 0 && a.Created < cutoff {
			expired = append(expired, a)
		}
	}
	if dryRun || len(expired) == 0 {
		refs := make([]string, 0, len(expired))
		for _, a := range expired {
			refs = append(refs, a.Ref)
		}
		return refs, nil
	}
	env, err := m.archiveEnv(ctx, repoRoot, "")
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, a := range expired {
		r, err := m.archiveRun(ctx, repoRoot, env, archiveTimeout, nil, "update-ref", "-d", a.Ref, a.SHA)
		if err != nil {
			return deleted, err
		}
		if r.ok() {
			deleted = append(deleted, a.Ref)
		}
	}
	return deleted, nil
}

func (m *Manager) branchTip(ctx context.Context, repoRoot, branch string, env map[string]string) (string, bool, error) {
	r, err := m.archiveRun(ctx, repoRoot, env, archiveTimeout, nil, "rev-parse", "--verify", "-q", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", false, err
	}
	if !r.ok() {
		return "", false, nil
	}
	return r.out(), true, nil
}

func (m *Manager) checkedOutBranches(ctx context.Context, repoRoot string, env map[string]string) (map[string]bool, error) {
	r, err := m.archiveRun(ctx, repoRoot, env, archiveTimeout, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, line := range strings.Split(r.text(), "\n") {
		if strings.HasPrefix(line, "branch refs/heads/") {
			out[strings.TrimPrefix(line, "branch refs/heads/")] = true
		}
	}
	return out, nil
}

func (m *Manager) addArgs(ctx context.Context, repoRoot, dest, branch, head string, env map[string]string) ([]string, bool, error) {
	base := []string{"worktree", "add", "--no-checkout"}
	if branch != "" {
		tip, found, err := m.branchTip(ctx, repoRoot, branch, env)
		if err != nil {
			return nil, false, err
		}
		if !found {
			return append(base, "-b", branch, dest, head), false, nil
		}
		if tip == head {
			checked, err := m.checkedOutBranches(ctx, repoRoot, env)
			if err != nil {
				return nil, false, err
			}
			if !checked[branch] {
				return append(base, dest, branch), false, nil
			}
		}
	}
	return append(base, "--detach", dest, head), true, nil
}

type Restored struct {
	Path     string    `json:"path"`
	Branch   string    `json:"branch"`
	Detached bool      `json:"detached"`
	Head     string    `json:"head"`
	Ref      string    `json:"ref"`
	Skipped  []Skipped `json:"skipped_untracked"`
}

func (m *Manager) Restore(ctx context.Context, repoRoot, name, target string) (*Restored, error) {
	arc, err := m.findArchive(ctx, repoRoot, name)
	if err != nil {
		return nil, err
	}
	if arc == nil {
		return nil, &ArchiveError{Msg: "no archive named " + name}
	}
	head := arc.metaString("head")
	if head == "" {
		head = arc.SHA + "^1"
	}
	indexCommit := arc.metaString("index_commit")
	if indexCommit == "" {
		indexCommit = arc.SHA + "^2"
	}
	dest := target
	if dest == "" {
		dest = arc.metaString("path")
	}
	if dest == "" {
		dest = filepath.Join(repoRoot, ".worktrees", arc.Name)
	}
	dest, err = filepath.Abs(dest)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(dest); err == nil {
		occupied := !info.IsDir()
		if !occupied {
			items, err := os.ReadDir(dest)
			if err != nil {
				return nil, err
			}
			occupied = len(items) > 0
		}
		if occupied {
			return nil, &ArchiveError{Msg: dest + " already exists; pass a different path"}
		}
	}
	env, err := m.archiveEnv(ctx, repoRoot, "")
	if err != nil {
		return nil, err
	}
	branch := arc.Branch()
	args, detached, err := m.addArgs(ctx, repoRoot, dest, branch, head, env)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
		return nil, err
	}
	if _, err := m.archiveOK(ctx, repoRoot, env, archiveTimeout, nil, args...); err != nil {
		return nil, err
	}
	wenv, err := m.archiveEnv(ctx, dest, "")
	if err != nil {
		return nil, err
	}
	emptyTree, err := m.archiveOK(ctx, dest, wenv, archiveTimeout, []byte{}, "mktree")
	if err != nil {
		return nil, err
	}
	if _, err := m.archiveOK(ctx, dest, wenv, archiveTimeout, nil, "read-tree", arc.SHA+"^{tree}"); err != nil {
		return nil, err
	}
	if err := m.rawAttrs(ctx, dest, wenv, strings.TrimSpace(emptyTree), "checkout-index", "-a", "-f"); err != nil {
		return nil, err
	}
	if err := m.writeRaw(ctx, dest, wenv); err != nil {
		return nil, err
	}
	if _, err := m.archiveOK(ctx, dest, wenv, archiveTimeout, nil, "read-tree", indexCommit+"^{tree}"); err != nil {
		return nil, err
	}
	if _, err := m.archiveRun(ctx, dest, wenv, archiveTimeout, nil, "update-index", "-q", "--refresh"); err != nil {
		return nil, err
	}
	if err := m.recordQuietly(ctx, repoRoot); err != nil {
		return nil, err
	}
	skipped := arc.Skipped()
	if skipped == nil {
		skipped = []Skipped{}
	}
	return &Restored{Path: dest, Branch: branch, Detached: detached, Head: head, Ref: arc.Ref, Skipped: skipped}, nil
}
