package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Ref namespace holding worktree archives
const ArchivePrefix = "refs/icebath/archive/"

const archiveSubject = "icebath worktree archive: "

var archiveIdentity = [][2]string{
	{"GIT_AUTHOR_NAME", "Icebath worktree archive"}, {"GIT_AUTHOR_EMAIL", "icebath-archive@localhost"},
	{"GIT_COMMITTER_NAME", "Icebath worktree archive"}, {"GIT_COMMITTER_EMAIL", "icebath-archive@localhost"},
}

var archiveRawConfig = [][2]string{{"core.autocrlf", "false"}, {"core.safecrlf", "false"}, {"core.eol", "lf"}}

// Snapshot or restore failure after which callers must keep the tree
type ArchiveError struct{ Msg string }

func (e *ArchiveError) Error() string { return e.Msg }

// One archive ref with its metadata and the staged digest of the index it captured
type Archive struct {
	Ref     string
	Name    string
	Sha     string
	Created float64
	Meta    *pyjson.Object
	Staged  string
}

// Hardened git env with the archive identity and raw content config, optionally on a temporary index
func archiveEnv(cwd, index string) (map[string]string, error) {
	env := noninteractiveRepoGitEnv(cwd, nil)
	if env == nil {
		return nil, &ArchiveError{"git filter discovery failed"}
	}
	for _, kv := range archiveIdentity {
		env[kv[0]] = kv[1]
	}
	start, err := strconv.Atoi(env["GIT_CONFIG_COUNT"])
	if err != nil {
		return nil, err
	}
	for offset, kv := range archiveRawConfig {
		env[fmt.Sprintf("GIT_CONFIG_KEY_%d", start+offset)] = kv[0]
		env[fmt.Sprintf("GIT_CONFIG_VALUE_%d", start+offset)] = kv[1]
	}
	env["GIT_CONFIG_COUNT"] = strconv.Itoa(start + len(archiveRawConfig))
	if index != "" {
		env["GIT_INDEX_FILE"] = index
	}
	return env, nil
}

// Copy of env with optional locks off so read only probes never write the index
func withoutOptionalLocks(env map[string]string) map[string]string {
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		out[k] = v
	}
	out["GIT_OPTIONAL_LOCKS"] = "0"
	return out
}

// Runs git in cwd under env, stdin from input or the null device, a zero timeout means five minutes
func archiveRun(args []string, cwd string, env map[string]string, timeout time.Duration, input []byte) (*pysubprocess.Completed, error) {
	if timeout == 0 {
		timeout = 300 * time.Second
	}
	return pysubprocess.RunWith(context.Background(), append([]string{"git"}, args...),
		pysubprocess.Options{Dir: cwd, Env: mapToEnviron(env), Input: input, Timeout: timeout})
}

// Last three hundred characters of stripped stderr as the archive failure text
func archiveStderrTail(stderr []byte) string {
	s := pystr.Strip(pystr.SurrogateEscapeDecode(stderr))
	return pystr.Slice(s, -300, pystr.Len(s))
}

// Stdout of a git command, an ArchiveError naming the subcommand when it exits non zero
func archiveOK(args []string, cwd string, env map[string]string, timeout time.Duration, input []byte) (string, error) {
	r, err := archiveRun(args, cwd, env, timeout, input)
	if err != nil {
		return "", err
	}
	if r.Returncode != 0 {
		return "", &ArchiveError{fmt.Sprintf("git %s failed: %s", args[0], archiveStderrTail(r.Stderr))}
	}
	return string(r.Stdout), nil
}

// Runs a converting command with attributes from the empty tree, retrying without on git older than 2.40
func archiveRawAttrs(args []string, cwd string, env map[string]string, emptyTree string) (string, error) {
	r, err := archiveRun(append([]string{"--attr-source", emptyTree}, args...), cwd, env, 0, nil)
	if err != nil {
		return "", err
	}
	if r.Returncode != 0 && strings.Contains(string(r.Stderr), "attr-source") {
		if r, err = archiveRun(args, cwd, env, 0, nil); err != nil {
			return "", err
		}
	}
	if r.Returncode != 0 {
		return "", &ArchiveError{fmt.Sprintf("git %s failed: %s", args[0], archiveStderrTail(r.Stderr))}
	}
	return string(r.Stdout), nil
}

type indexEntry struct{ mode, sha, path string }

// Mode, blob and path of every regular file entry of the index env points at
func archiveRegularEntries(cwd string, env map[string]string) ([]indexEntry, error) {
	out, err := archiveOK([]string{"ls-files", "-s", "-z"}, cwd, env, 0, nil)
	if err != nil {
		return nil, err
	}
	var entries []indexEntry
	for _, record := range strings.Split(out, "\x00") {
		meta, path, _ := strings.Cut(record, "\t")
		parts := pystr.SplitWhitespace(meta, -1)
		if path != "" && len(parts) == 3 && (parts[0] == "100644" || parts[0] == "100755") {
			entries = append(entries, indexEntry{parts[0], parts[1], path})
		}
	}
	return entries, nil
}

// Replaces every regular file blob in the temporary index with the exact bytes on disk
func archiveRehashRaw(wt string, tenv map[string]string) error {
	all, err := archiveRegularEntries(wt, tenv)
	if err != nil {
		return err
	}
	var entries []indexEntry
	for _, e := range all {
		p := filepath.Join(wt, e.path)
		if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() {
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
		var data bytes.Buffer
		for _, p := range batch {
			data.WriteString(p + "\n")
		}
		out, err := archiveOK([]string{"hash-object", "-w", "--no-filters", "--stdin-paths"}, wt, tenv, 0, data.Bytes())
		if err != nil {
			return err
		}
		shas := strings.Fields(out)
		for i := 0; i < len(batch) && i < len(shas); i++ {
			byPath[batch[i]] = shas[i]
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
		out, err := archiveOK([]string{"hash-object", "-w", "--no-filters", "--stdin"}, wt, tenv, 0, raw)
		if err != nil {
			return err
		}
		byPath[e.path] = pystr.Strip(out)
	}
	var info bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&info, "%s %s\t%s\x00", e.mode, byPath[e.path], e.path)
	}
	_, err = archiveOK([]string{"update-index", "-z", "--index-info"}, wt, tenv, 0, info.Bytes())
	return err
}

// Overwrites every regular file of the index with its blob bytes, no smudge and no eol conversion
func archiveWriteRaw(dest string, env map[string]string) error {
	entries, err := archiveRegularEntries(dest, env)
	if err != nil || len(entries) == 0 {
		return err
	}
	var req bytes.Buffer
	for _, e := range entries {
		req.WriteString(e.sha + "\n")
	}
	outS, err := archiveOK([]string{"cat-file", "--batch"}, dest, env, 0, req.Bytes())
	if err != nil {
		return err
	}
	out := []byte(outS)
	pos := 0
	for _, e := range entries {
		rel := bytes.IndexByte(out[pos:], '\n')
		if rel < 0 {
			return errors.New("substring not found")
		}
		headerEnd := pos + rel
		fields := strings.Fields(string(out[pos:headerEnd]))
		if len(fields) < 3 {
			return errors.New("list index out of range")
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil {
			return err
		}
		body := out[headerEnd+1 : min(len(out), headerEnd+1+size)]
		pos = headerEnd + 1 + size + 1
		target := filepath.Join(dest, e.path)
		if st, err := os.Lstat(target); err == nil && st.Mode()&fs.ModeSymlink != 0 {
			if err := os.Remove(target); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
			return err
		}
		st, statErr := os.Stat(target)
		if err := os.WriteFile(target, body, 0o666); err != nil {
			return err
		}
		if statErr == nil {
			if err := os.Chmod(target, st.Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}

var refUnsafeRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// A tree name as one valid ref component
func RefSafe(name string) string {
	safe := strings.Trim(refUnsafeRe.ReplaceAllString(name, "-"), "-.")
	if safe == "" {
		safe = "tree"
	}
	return strings.TrimSuffix(strings.ReplaceAll(safe, "..", "-"), ".lock")
}

func archiveUntracked(wt string, env map[string]string) ([]string, error) {
	out, err := archiveOK([]string{"ls-files", "--others", "--exclude-standard", "-z"}, wt, env, 0, nil)
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

// Reason prefix of every submodule verdict
const SubmoduleReason = "submodule "

// Why a submodule keeps the tree, empty when nothing does, gitlinks refuses any gitlink at all
func SubmoduleWork(wtPath string, gitlinks bool) (string, error) {
	base, err := archiveEnv(wtPath, "")
	if err != nil {
		return "", err
	}
	env := withoutOptionalLocks(base)
	gitDir, err := archiveOK([]string{"rev-parse", "--absolute-git-dir"}, wtPath, env, 0, nil)
	if err != nil {
		return "", err
	}
	modules := filepath.Join(pystr.Strip(gitDir), "modules")
	if ents, err := os.ReadDir(modules); err == nil && len(ents) > 0 {
		return fmt.Sprintf("%srepository in %s would be deleted with the tree", SubmoduleReason, modules), nil
	}
	staged, err := archiveOK([]string{"ls-files", "--stage", "-z"}, wtPath, env, 0, nil)
	if err != nil {
		return "", err
	}
	for _, e := range strings.Split(staged, "\x00") {
		if !strings.HasPrefix(e, "160000 ") || !strings.Contains(e, "\t") {
			continue
		}
		_, p, _ := strings.Cut(e, "\t")
		if _, err := os.Stat(filepath.Join(wtPath, p, ".git")); gitlinks || err == nil {
			return fmt.Sprintf("%s%s cannot be archived (gitlink only)", SubmoduleReason, p), nil
		}
	}
	return "", nil
}

// Digest of the semantic staged entries, unchanged by a stat only refresh and moved by any add or rm
func StagedState(wtPath string) (string, error) {
	env, err := archiveEnv(wtPath, "")
	if err != nil {
		return "", err
	}
	return archiveDigestIndex(wtPath, env)
}

func archiveDigestIndex(wt string, env map[string]string) (string, error) {
	out, err := archiveOK([]string{"ls-files", "--stage", "-z"}, wt, withoutOptionalLocks(env), 0, nil)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(out))
	return hex.EncodeToString(sum[:]), nil
}

// Keep action and reason when archiving and removing the tree would lose something, nil when it is safe
func ArchivePreflight(wtPath string, maxUntrackedMB float64) *RemovalBlock {
	env, err := archiveEnv(wtPath, "")
	var skipped []SkippedUntracked
	if err == nil {
		var untracked []string
		if untracked, err = archiveUntracked(wtPath, env); err == nil {
			skipped = archiveSplitOversized(wtPath, untracked, maxUntrackedMB)
		}
	}
	if err != nil {
		return &RemovalBlock{KeepUnarchivable, err.Error()}
	}
	return RemovalBlocker(wtPath, skipped, true)
}

// Untracked file left out of an archive for exceeding the size cap
type SkippedUntracked struct {
	Path  string
	Bytes int64
}

func archiveSplitOversized(wt string, untracked []string, maxMB float64) []SkippedUntracked {
	if maxMB <= 0 {
		return nil
	}
	limit := maxMB * 1024 * 1024
	var skipped []SkippedUntracked
	for _, rel := range untracked {
		st, err := os.Lstat(filepath.Join(wt, rel))
		if err != nil {
			continue
		}
		if float64(st.Size()) > limit {
			skipped = append(skipped, SkippedUntracked{rel, st.Size()})
		}
	}
	return skipped
}

// Clock used for archive stamps and expiry, swapped by tests
var archiveNow = time.Now

func archiveCreateRef(repoRoot, name, sha string, env map[string]string) (string, error) {
	base := ArchivePrefix + RefSafe(name)
	stamp := archiveNow().Format("20060102T150405")
	refs := []string{base, base + "--" + stamp}
	for i := 2; i < 50; i++ {
		refs = append(refs, fmt.Sprintf("%s--%s-%d", base, stamp, i))
	}
	for _, ref := range refs {
		r, err := archiveRun([]string{"update-ref", "-m", "icebath worktree archive", ref, sha, ""}, repoRoot, env, 0, nil)
		if err != nil {
			return "", err
		}
		if r.Returncode == 0 {
			return ref, nil
		}
	}
	return "", &ArchiveError{"could not create an archive ref for " + name}
}

// Archives the full working state of the tree through a temporary index, never touching its real index or files
func SnapshotWorktree(repoRoot, wtPath, reason string, maxUntrackedMB float64) (*Archive, error) {
	env, err := archiveEnv(wtPath, "")
	if err != nil {
		return nil, err
	}
	unsafe, err := SubmoduleWork(wtPath, true)
	if err != nil {
		return nil, err
	}
	if unsafe != "" {
		return nil, &ArchiveError{unsafe}
	}
	head, err := archiveOK([]string{"rev-parse", "--verify", "HEAD^{commit}"}, wtPath, env, 0, nil)
	if err != nil {
		return nil, err
	}
	head = pystr.Strip(head)
	br, err := archiveRun([]string{"symbolic-ref", "--short", "-q", "HEAD"}, wtPath, env, 0, nil)
	if err != nil {
		return nil, err
	}
	var branch any
	if b := pystr.Strip(string(br.Stdout)); b != "" {
		branch = b
	}
	gitDirOut, err := archiveOK([]string{"rev-parse", "--absolute-git-dir"}, wtPath, env, 0, nil)
	if err != nil {
		return nil, err
	}
	gitDir := pystr.Strip(gitDirOut)
	untracked, err := archiveUntracked(wtPath, env)
	if err != nil {
		return nil, err
	}
	for _, p := range untracked {
		if strings.HasSuffix(p, "/") {
			return nil, &ArchiveError{fmt.Sprintf("untracked embedded repository %s cannot be archived", strings.TrimRight(p, "/"))}
		}
	}
	skipped := archiveSplitOversized(wtPath, untracked, maxUntrackedMB)
	token := fmt.Sprintf("icebath-archive-%d-%s", os.Getpid(), strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
	tmpIndex, pathspec := filepath.Join(gitDir, token+".index"), filepath.Join(gitDir, token+".pathspec")
	name := filepath.Base(wtPath)
	var meta *pyjson.Object
	var staged, sha string
	err = func() error {
		defer func() {
			for _, leftover := range []string{tmpIndex, tmpIndex + ".lock", pathspec} {
				_ = os.Remove(leftover)
			}
		}()
		tenv, err := archiveEnv(wtPath, tmpIndex)
		if err != nil {
			return err
		}
		if st, err := os.Stat(filepath.Join(gitDir, "index")); err == nil && st.Mode().IsRegular() {
			data, err := os.ReadFile(filepath.Join(gitDir, "index"))
			if err != nil {
				return err
			}
			if err := os.WriteFile(tmpIndex, data, 0o666); err != nil {
				return err
			}
		} else if _, err := archiveOK([]string{"read-tree", head}, wtPath, tenv, 0, nil); err != nil {
			return err
		}
		if staged, err = archiveDigestIndex(wtPath, tenv); err != nil {
			return err
		}
		indexTree, err := archiveOK([]string{"write-tree"}, wtPath, tenv, 0, nil)
		if err != nil {
			return err
		}
		emptyTree, err := archiveOK([]string{"mktree"}, wtPath, env, 0, []byte{})
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
		if _, err := archiveRawAttrs([]string{"add", "-A", "--pathspec-from-file", pathspec, "--pathspec-file-nul"}, wtPath, tenv, pystr.Strip(emptyTree)); err != nil {
			return err
		}
		if err := archiveRehashRaw(wtPath, tenv); err != nil {
			return err
		}
		workTree, err := archiveOK([]string{"write-tree"}, wtPath, tenv, 0, nil)
		if err != nil {
			return err
		}
		indexCommit, err := archiveOK([]string{"commit-tree", pystr.Strip(indexTree), "-p", head, "-m", "index of " + name}, wtPath, env, 0, nil)
		if err != nil {
			return err
		}
		indexCommit = pystr.Strip(indexCommit)
		skippedList := []any{}
		for _, s := range skipped {
			o := pyjson.NewObject()
			o.Set("path", s.Path)
			o.Set("bytes", s.Bytes)
			skippedList = append(skippedList, o)
		}
		meta = pyjson.NewObject()
		for _, kv := range []struct {
			k string
			v any
		}{{"version", int64(1)}, {"name", name}, {"path", wtPath}, {"repo_root", repoRoot}, {"branch", branch},
			{"head", head}, {"index_commit", indexCommit}, {"created", archiveNow().Unix()}, {"reason", reason},
			{"skipped_untracked", skippedList}} {
			meta.Set(kv.k, kv.v)
		}
		body, err := pyjson.Dumps(meta, pyjson.Indent(1), pyjson.EnsureASCII(false))
		if err != nil {
			return err
		}
		message := archiveSubject + name + "\n\n" + body + "\n"
		out, err := archiveOK([]string{"commit-tree", pystr.Strip(workTree), "-p", head, "-p", indexCommit}, wtPath, env, 0, []byte(message))
		sha = pystr.Strip(out)
		return err
	}()
	if err != nil {
		return nil, err
	}
	ref, err := archiveCreateRef(repoRoot, name, sha, env)
	if err != nil {
		return nil, err
	}
	toolsLogger(nil).Info(fmt.Sprintf("Archived worktree %s -> %s (%s)", wtPath, ref, reason))
	created, _ := meta.Get("created")
	return &Archive{Ref: ref, Name: ref[len(ArchivePrefix):], Sha: sha, Created: float64(created.(int64)), Meta: meta, Staged: staged}, nil
}

func archiveParseMeta(body string) *pyjson.Object {
	start := strings.Index(body, "{")
	if start < 0 {
		return pyjson.NewObject()
	}
	v, err := pyjson.Loads(body[start:])
	if o, ok := v.(*pyjson.Object); err == nil && ok {
		return o
	}
	return pyjson.NewObject()
}

// Every archive of the repository newest first, empty when git fails, a malformed created stamp raises
func ListArchives(repoRoot string) ([]Archive, error) {
	env, err := archiveEnv(repoRoot, "")
	if err != nil {
		return nil, nil
	}
	out, err := archiveOK([]string{"for-each-ref", "--sort=-creatordate",
		"--format=%(refname)%00%(objectname)%00%(creatordate:unix)%00%(contents:body)%00", ArchivePrefix}, repoRoot, env, 60*time.Second, nil)
	if err != nil {
		return nil, nil
	}
	var archives []Archive
	for _, record := range strings.Split(out, "\x00\n") {
		parts := strings.Split(record, "\x00")
		if len(parts) < 4 || !strings.HasPrefix(parts[0], ArchivePrefix) {
			continue
		}
		meta := archiveParseMeta(parts[3])
		var created any = int64(0)
		if c, _ := meta.Get("created"); pyTruthy(c) {
			created = c
		} else if parts[2] != "" {
			created = parts[2]
		}
		f, err := pyint.Float(created)
		if err != nil {
			return nil, err
		}
		archives = append(archives, Archive{Ref: parts[0], Name: parts[0][len(ArchivePrefix):], Sha: parts[1], Created: f, Meta: meta})
	}
	return archives, nil
}

// Exact archive name or ref first, else the newest archive of the tree with that name
func FindArchive(repoRoot, name string) (*Archive, error) {
	archives, err := ListArchives(repoRoot)
	if err != nil {
		return nil, err
	}
	wanted := strings.TrimPrefix(name, ArchivePrefix)
	for i := range archives {
		if archives[i].Name == wanted {
			return &archives[i], nil
		}
	}
	for i := range archives {
		if n, _ := archives[i].Meta.Get("name"); n == name || archives[i].Name == RefSafe(name) {
			return &archives[i], nil
		}
	}
	return nil, nil
}

// Deletes archive refs older than the retention window, zero keeps them forever, dry run only lists them
func PruneExpiredArchives(repoRoot string, retentionDays float64, now float64, dryRun bool) ([]string, error) {
	if retentionDays <= 0 {
		return nil, nil
	}
	if now == 0 {
		now = float64(archiveNow().UnixNano()) / 1e9
	}
	cutoff := now - retentionDays*86400
	all, err := ListArchives(repoRoot)
	if err != nil {
		return nil, err
	}
	var expired []Archive
	for _, a := range all {
		if a.Created != 0 && a.Created < cutoff {
			expired = append(expired, a)
		}
	}
	if dryRun || len(expired) == 0 {
		refs := []string{}
		for _, a := range expired {
			refs = append(refs, a.Ref)
		}
		return refs, nil
	}
	env, err := archiveEnv(repoRoot, "")
	if err != nil {
		return nil, err
	}
	deleted := []string{}
	for _, a := range expired {
		r, err := archiveRun([]string{"update-ref", "-d", a.Ref, a.Sha}, repoRoot, env, 0, nil)
		if err != nil {
			return nil, err
		}
		if r.Returncode == 0 {
			deleted = append(deleted, a.Ref)
		}
	}
	return deleted, nil
}

func archiveBranchTip(repoRoot, branch string, env map[string]string) (string, bool, error) {
	r, err := archiveRun([]string{"rev-parse", "--verify", "-q", "refs/heads/" + branch + "^{commit}"}, repoRoot, env, 0, nil)
	if err != nil || r.Returncode != 0 {
		return "", false, err
	}
	return pystr.Strip(string(r.Stdout)), true, nil
}

func archiveCheckedOutBranches(repoRoot string, env map[string]string) (map[string]bool, error) {
	r, err := archiveRun([]string{"worktree", "list", "--porcelain"}, repoRoot, env, 0, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, line := range pystr.Splitlines(string(r.Stdout), false) {
		if strings.HasPrefix(line, "branch refs/heads/") {
			out[line[len("branch refs/heads/"):]] = true
		}
	}
	return out, nil
}

// Worktree add argv restoring the branch when that is safe, with whether the result is detached
func archiveAddArgs(repoRoot, dest, branch, head string, env map[string]string) ([]string, bool, error) {
	base := []string{"worktree", "add", "--no-checkout"}
	if branch != "" {
		tip, ok, err := archiveBranchTip(repoRoot, branch, env)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return append(base, "-b", branch, dest, head), false, nil
		}
		if tip == head {
			out, err := archiveCheckedOutBranches(repoRoot, env)
			if err != nil {
				return nil, false, err
			}
			if !out[branch] {
				return append(base, dest, branch), false, nil
			}
		}
	}
	return append(base, "--detach", dest, head), true, nil
}

// Result of recreating an archived tree
type RestoredArchive struct {
	Path             string
	Branch           any
	Detached         bool
	Head             string
	Ref              string
	SkippedUntracked any
}

// Installed by pkg/cron at init over RecordWorktreeRepo of the active home, panics when unassigned
var WorktreeRecordRepo func(repoRoot string)

// Recreates the archived tree at its original path or target with its exact files and index
func RestoreArchive(repoRoot, name, target string) (*RestoredArchive, error) {
	arc, err := FindArchive(repoRoot, name)
	if err != nil {
		return nil, err
	}
	if arc == nil {
		return nil, &ArchiveError{"no archive named " + name}
	}
	metaStr := func(key string) string {
		v, _ := arc.Meta.Get(key)
		if s, ok := v.(string); ok {
			return s
		}
		return ""
	}
	head := metaStr("head")
	if head == "" {
		head = arc.Sha + "^1"
	}
	indexCommit := metaStr("index_commit")
	if indexCommit == "" {
		indexCommit = arc.Sha + "^2"
	}
	dest := target
	if dest == "" {
		dest = metaStr("path")
	}
	if dest == "" {
		dest = filepath.Join(repoRoot, ".worktrees", arc.Name)
	}
	if dest, err = filepath.Abs(dest); err != nil {
		return nil, err
	}
	if st, err := os.Stat(dest); err == nil {
		ents, _ := os.ReadDir(dest)
		if !st.IsDir() || len(ents) > 0 {
			return nil, &ArchiveError{dest + " already exists; pass a different path"}
		}
	}
	env, err := archiveEnv(repoRoot, "")
	if err != nil {
		return nil, err
	}
	branchV, _ := arc.Meta.Get("branch")
	args, detached, err := archiveAddArgs(repoRoot, dest, metaStr("branch"), head, env)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
		return nil, err
	}
	if _, err := archiveOK(args, repoRoot, env, 0, nil); err != nil {
		return nil, err
	}
	wenv, err := archiveEnv(dest, "")
	if err != nil {
		return nil, err
	}
	emptyTree, err := archiveOK([]string{"mktree"}, dest, wenv, 0, []byte{})
	if err != nil {
		return nil, err
	}
	if _, err := archiveOK([]string{"read-tree", arc.Sha + "^{tree}"}, dest, wenv, 0, nil); err != nil {
		return nil, err
	}
	if _, err := archiveRawAttrs([]string{"checkout-index", "-a", "-f"}, dest, wenv, pystr.Strip(emptyTree)); err != nil {
		return nil, err
	}
	if err := archiveWriteRaw(dest, wenv); err != nil {
		return nil, err
	}
	if _, err := archiveOK([]string{"read-tree", indexCommit + "^{tree}"}, dest, wenv, 0, nil); err != nil {
		return nil, err
	}
	if _, err := archiveRun([]string{"update-index", "-q", "--refresh"}, dest, wenv, 0, nil); err != nil {
		return nil, err
	}
	WorktreeRecordRepo(repoRoot)
	skipped, _ := arc.Meta.Get("skipped_untracked")
	if !pyTruthy(skipped) {
		skipped = []any{}
	}
	return &RestoredArchive{Path: dest, Branch: branchV, Detached: detached, Head: head, Ref: arc.Ref, SkippedUntracked: skipped}, nil
}
