package worktrees

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/git"
)

// Ref namespace holding the archives of removed trees
const ArchivePrefix = "refs/icebath/archive/"

// Subject of an archive's commit, before the tree's name
const archiveSubject = "icebath worktree archive: "

// Who an archive's commits are by
var archiveIdentity = map[string]string{
	"GIT_AUTHOR_NAME": "icebath worktree archive", "GIT_AUTHOR_EMAIL": "icebath-archive@localhost",
	"GIT_COMMITTER_NAME": "icebath worktree archive", "GIT_COMMITTER_EMAIL": "icebath-archive@localhost",
}

// Settings that keep the bytes an archive takes and gives back exactly as they are on disk
var rawContent = [][2]string{{"core.autocrlf", "false"}, {"core.safecrlf", "false"}, {"core.eol", "lf"}}

// Failure to snapshot or restore a tree, after which it must be kept
type ArchiveError struct{ Message string }

func (e *ArchiveError) Error() string { return e.Message }

// Untracked file left out of an archive for being larger than it takes
type Skipped struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// What an archive's commit message keeps of the tree
type ArchiveMeta struct {
	Version  int    `json:"version"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	RepoRoot string `json:"repo_root"`
	// Branch it had checked out, empty when detached
	Branch      string    `json:"branch"`
	Head        string    `json:"head"`
	IndexCommit string    `json:"index_commit"`
	Created     int64     `json:"created"`
	Reason      string    `json:"reason"`
	Skipped     []Skipped `json:"skipped_untracked"`
}

// One archive of a removed tree
type Archive struct {
	Ref     string
	Name    string
	Sha     string
	Created time.Time
	Meta    ArchiveMeta
	// Digest of the staged entries of the index it captured, set as it is made
	Staged string
}

// Environment of an archive's git runs: hardened for the repository, with the archive's identity and settings that keep bytes raw, on a
// temporary index when one is given
func archiveEnv(dir, index string) (map[string]string, error) {
	env := repoEnv(dir)
	if env == nil {
		return nil, &ArchiveError{FilterDiscoveryFailed}
	}
	for key, value := range archiveIdentity {
		env[key] = value
	}
	git.Configure(env, rawContent...)
	if index != "" {
		env["GIT_INDEX_FILE"] = index
	}
	return env, nil
}

// Copy of the environment whose read only runs never write the index
func lockless(env map[string]string) map[string]string {
	out := make(map[string]string, len(env)+1)
	for key, value := range env {
		out[key] = value
	}
	out["GIT_OPTIONAL_LOCKS"] = "0"
	return out
}

// Runs git for an archive, five minutes at most when no timeout is given
func archiveRun(dir string, env map[string]string, timeout time.Duration, input []byte, args ...string) (*git.Result, error) {
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	return git.Run(context.Background(), dir, env, timeout, input, args...)
}

// Last part of the errors of a git run, as an archive failure says it
func tail(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if runes := []rune(stderr); len(runes) > 300 {
		return string(runes[len(runes)-300:])
	}
	return stderr
}

// Output of a git run for an archive that must succeed, an ArchiveError naming the command when it doesn't
func archiveOK(dir string, env map[string]string, timeout time.Duration, input []byte, args ...string) (string, error) {
	result, err := archiveRun(dir, env, timeout, input, args...)
	if err != nil {
		return "", err
	}
	if result.Code != 0 {
		return "", &ArchiveError{fmt.Sprintf("`git %s` failed: %s", args[0], tail(result.Stderr))}
	}
	return result.Stdout, nil
}

// Runs a converting command with attributes read from the empty tree, so no filter or line end conversion applies; again without on a
// git older than 2.40 that has no --attr-source
func rawAttrs(dir string, env map[string]string, empty string, args ...string) (string, error) {
	result, err := archiveRun(dir, env, 0, nil, append([]string{"--attr-source", empty}, args...)...)
	if err != nil {
		return "", err
	}
	if result.Code != 0 && strings.Contains(result.Stderr, "attr-source") {
		if result, err = archiveRun(dir, env, 0, nil, args...); err != nil {
			return "", err
		}
	}
	if result.Code != 0 {
		return "", &ArchiveError{fmt.Sprintf("`git %s` failed: %s", args[0], tail(result.Stderr))}
	}
	return result.Stdout, nil
}

// One regular file of an index
type indexEntry struct{ mode, sha, path string }

// Regular files of the index the environment points at
func regularEntries(dir string, env map[string]string) ([]indexEntry, error) {
	out, err := archiveOK(dir, env, 0, nil, "ls-files", "-s", "-z")
	if err != nil {
		return nil, err
	}
	var entries []indexEntry
	for _, record := range strings.Split(out, "\x00") {
		meta, path, _ := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if path != "" && len(fields) == 3 && (fields[0] == "100644" || fields[0] == "100755") {
			entries = append(entries, indexEntry{fields[0], fields[1], path})
		}
	}
	return entries, nil
}

// Gives every regular file of the temporary index the exact bytes it has on disk, no filter or conversion applied
func rehashRaw(tree string, env map[string]string) error {
	all, err := regularEntries(tree, env)
	if err != nil {
		return err
	}
	var entries []indexEntry
	for _, entry := range all {
		if info, err := os.Lstat(filepath.Join(tree, entry.path)); err == nil && info.Mode().IsRegular() {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return nil
	}
	var batch []string
	for _, entry := range entries {
		if !strings.Contains(entry.path, "\n") && !strings.HasPrefix(entry.path, `"`) {
			batch = append(batch, entry.path)
		}
	}
	shas := map[string]string{}
	if len(batch) > 0 {
		out, err := archiveOK(tree, env, 0, []byte(strings.Join(batch, "\n")+"\n"), "hash-object", "-w", "--no-filters", "--stdin-paths")
		if err != nil {
			return err
		}
		for i, sha := range strings.Fields(out) {
			if i < len(batch) {
				shas[batch[i]] = sha
			}
		}
	}
	for _, entry := range entries {
		if _, ok := shas[entry.path]; ok {
			continue
		}
		data, err := os.ReadFile(filepath.Join(tree, entry.path))
		if err != nil {
			return err
		}
		out, err := archiveOK(tree, env, 0, data, "hash-object", "-w", "--no-filters", "--stdin")
		if err != nil {
			return err
		}
		shas[entry.path] = strings.TrimSpace(out)
	}
	var info bytes.Buffer
	for _, entry := range entries {
		fmt.Fprintf(&info, "%s %s\t%s\x00", entry.mode, shas[entry.path], entry.path)
	}
	_, err = archiveOK(tree, env, 0, info.Bytes(), "update-index", "-z", "--index-info")
	return err
}

// Writes every regular file of the index with the exact bytes of its blob, no filter or line end conversion applied
func writeRaw(dest string, env map[string]string) error {
	entries, err := regularEntries(dest, env)
	if err != nil || len(entries) == 0 {
		return err
	}
	var request bytes.Buffer
	for _, entry := range entries {
		request.WriteString(entry.sha + "\n")
	}
	text, err := archiveOK(dest, env, 0, request.Bytes(), "cat-file", "--batch")
	if err != nil {
		return err
	}
	out := []byte(text)
	pos := 0
	for _, entry := range entries {
		end := bytes.IndexByte(out[pos:], '\n')
		if end < 0 {
			return errors.New("A part of the git archive has no header.")
		}
		header := strings.Fields(string(out[pos : pos+end]))
		if len(header) < 3 {
			return errors.New("A header of the git archive has fewer than three fields.")
		}
		size, err := strconv.Atoi(header[2])
		if err != nil {
			return err
		}
		start := pos + end + 1
		body := out[start:min(len(out), start+size)]
		pos = start + size + 1
		target := filepath.Join(dest, entry.path)
		if info, err := os.Lstat(target); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			if err := os.Remove(target); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
			return err
		}
		info, statErr := os.Stat(target)
		if err := os.WriteFile(target, body, 0o666); err != nil {
			return err
		}
		if statErr == nil {
			if err := os.Chmod(target, info.Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}

var refUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// A tree name made one valid part of a ref
func RefSafe(name string) string {
	safe := strings.Trim(refUnsafe.ReplaceAllString(name, "-"), "-.")
	if safe == "" {
		safe = "tree"
	}
	return strings.TrimSuffix(strings.ReplaceAll(safe, "..", "-"), ".lock")
}

// Untracked files of the tree, ignored ones left out
func untracked(tree string, env map[string]string) ([]string, error) {
	out, err := archiveOK(tree, env, 0, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, path := range strings.Split(out, "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// Start of every reason a submodule keeps a tree
const SubmoduleReason = "the submodule "

// Why a submodule keeps the tree: a repository of submodules that removing the tree deletes, or a submodule whose work only its commit
// reference holds, any gitlink at all when gitlinks is set; empty when none does
func SubmoduleWork(tree string, gitlinks bool) (string, error) {
	env, err := archiveEnv(tree, "")
	if err != nil {
		return "", err
	}
	env = lockless(env)
	gitDir, err := archiveOK(tree, env, 0, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	modules := filepath.Join(strings.TrimSpace(gitDir), "modules")
	if entries, err := os.ReadDir(modules); err == nil && len(entries) > 0 {
		return fmt.Sprintf("%srepositories in %s would be deleted with it", SubmoduleReason, modules), nil
	}
	staged, err := archiveOK(tree, env, 0, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return "", err
	}
	for _, entry := range strings.Split(staged, "\x00") {
		_, path, ok := strings.Cut(entry, "\t")
		if !strings.HasPrefix(entry, "160000 ") || !ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(tree, path, ".git")); gitlinks || err == nil {
			return fmt.Sprintf("%s%s cannot be archived, because only its commit is recorded", SubmoduleReason, path), nil
		}
	}
	return "", nil
}

// Digest of what the tree has staged, unchanged by a refresh of file times and moved by any add or remove
func StagedState(tree string) (string, error) {
	env, err := archiveEnv(tree, "")
	if err != nil {
		return "", err
	}
	return digestIndex(tree, env)
}

func digestIndex(tree string, env map[string]string) (string, error) {
	out, err := archiveOK(tree, lockless(env), 0, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(out))
	return hex.EncodeToString(sum[:]), nil
}

// Untracked files larger than the size in MB an archive takes, none when it takes any
func oversized(tree string, paths []string, maxMB float64) []Skipped {
	if maxMB <= 0 {
		return nil
	}
	limit := maxMB * 1024 * 1024
	var skipped []Skipped
	for _, rel := range paths {
		if info, err := os.Lstat(filepath.Join(tree, rel)); err == nil && float64(info.Size()) > limit {
			skipped = append(skipped, Skipped{rel, info.Size()})
		}
	}
	return skipped
}

// Why archiving and removing the tree would lose something, nil when it is safe
func Preflight(tree string, maxMB float64) *RemovalBlock {
	env, err := archiveEnv(tree, "")
	var skipped []Skipped
	if err == nil {
		var paths []string
		if paths, err = untracked(tree, env); err == nil {
			skipped = oversized(tree, paths, maxMB)
		}
	}
	if err != nil {
		return &RemovalBlock{KeepUnarchivable, err.Error()}
	}
	return Blocker(tree, skipped, true)
}

// Makes the archive's ref: the tree's name, else stamped with the time, else numbered
func createRef(repo, name, sha string, env map[string]string) (string, error) {
	base := ArchivePrefix + RefSafe(name)
	stamp := time.Now().Format("20060102T150405")
	refs := []string{base, base + "--" + stamp}
	for i := 2; i < 50; i++ {
		refs = append(refs, fmt.Sprintf("%s--%s-%d", base, stamp, i))
	}
	for _, ref := range refs {
		result, err := archiveRun(repo, env, 0, nil, "update-ref", "-m", "icebath worktree archive", ref, sha, "")
		if err != nil {
			return "", err
		}
		if result.Code == 0 {
			return ref, nil
		}
	}
	return "", &ArchiveError{"No archive ref could be made for " + name + "."}
}

// Archives everything of the tree, tracked, staged and untracked, through a temporary index, never touching its own index or files:
// a commit of its files whose parents are its HEAD and a commit of its index, under a ref of its name
func Snapshot(repo, tree, reason string, maxMB float64) (*Archive, error) {
	env, err := archiveEnv(tree, "")
	if err != nil {
		return nil, err
	}
	unsafe, err := SubmoduleWork(tree, true)
	if err != nil {
		return nil, err
	}
	if unsafe != "" {
		return nil, &ArchiveError{unsafe}
	}
	head, err := archiveOK(tree, env, 0, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, err
	}
	head = strings.TrimSpace(head)
	symbolic, err := archiveRun(tree, env, 0, nil, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		return nil, err
	}
	branch := strings.TrimSpace(symbolic.Stdout)
	gitDir, err := archiveOK(tree, env, 0, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	gitDir = strings.TrimSpace(gitDir)
	paths, err := untracked(tree, env)
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "/") {
			return nil, &ArchiveError{fmt.Sprintf("The untracked repository %s inside the worktree cannot be archived.", strings.TrimRight(path, "/"))}
		}
	}
	skipped := oversized(tree, paths, maxMB)
	token := fmt.Sprintf("icebath-archive-%d-%s", os.Getpid(), strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
	index, pathspec := filepath.Join(gitDir, token+".index"), filepath.Join(gitDir, token+".pathspec")
	defer func() {
		for _, leftover := range []string{index, index + ".lock", pathspec} {
			os.Remove(leftover)
		}
	}()
	name := filepath.Base(tree)
	temp, err := archiveEnv(tree, index)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(filepath.Join(gitDir, "index")); err == nil && info.Mode().IsRegular() {
		data, err := os.ReadFile(filepath.Join(gitDir, "index"))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(index, data, 0o666); err != nil {
			return nil, err
		}
	} else if _, err := archiveOK(tree, temp, 0, nil, "read-tree", head); err != nil {
		return nil, err
	}
	staged, err := digestIndex(tree, temp)
	if err != nil {
		return nil, err
	}
	indexTree, err := archiveOK(tree, temp, 0, nil, "write-tree")
	if err != nil {
		return nil, err
	}
	empty, err := archiveOK(tree, env, 0, []byte{}, "mktree")
	if err != nil {
		return nil, err
	}
	specs := []string{":(top)"}
	for _, s := range skipped {
		specs = append(specs, ":(top,exclude,literal)"+s.Path)
	}
	if err := os.WriteFile(pathspec, []byte(strings.Join(specs, "\x00")+"\x00"), 0o666); err != nil {
		return nil, err
	}
	if _, err := rawAttrs(tree, temp, strings.TrimSpace(empty), "add", "-A", "--pathspec-from-file", pathspec, "--pathspec-file-nul"); err != nil {
		return nil, err
	}
	if err := rehashRaw(tree, temp); err != nil {
		return nil, err
	}
	workTree, err := archiveOK(tree, temp, 0, nil, "write-tree")
	if err != nil {
		return nil, err
	}
	indexCommit, err := archiveOK(tree, env, 0, nil, "commit-tree", strings.TrimSpace(indexTree), "-p", head, "-m", "index of "+name)
	if err != nil {
		return nil, err
	}
	meta := ArchiveMeta{Version: 1, Name: name, Path: tree, RepoRoot: repo, Branch: branch, Head: head, IndexCommit: strings.TrimSpace(indexCommit),
		Created: time.Now().Unix(), Reason: reason, Skipped: skipped}
	if meta.Skipped == nil {
		meta.Skipped = []Skipped{}
	}
	body, err := json.MarshalIndent(meta, "", " ")
	if err != nil {
		return nil, err
	}
	message := archiveSubject + name + "\n\n" + string(body) + "\n"
	sha, err := archiveOK(tree, env, 0, []byte(message), "commit-tree", strings.TrimSpace(workTree), "-p", head, "-p", meta.IndexCommit)
	if err != nil {
		return nil, err
	}
	sha = strings.TrimSpace(sha)
	ref, err := createRef(repo, name, sha, env)
	if err != nil {
		return nil, err
	}
	slog.Info("Archived worktree", "tree", tree, "ref", ref, "reason", reason)
	return &Archive{Ref: ref, Name: strings.TrimPrefix(ref, ArchivePrefix), Sha: sha, Created: time.Unix(meta.Created, 0), Meta: meta, Staged: staged}, nil
}

// Every archive of the repository, newest first; none when git can't list them
func Archives(repo string) []Archive {
	env, err := archiveEnv(repo, "")
	if err != nil {
		return nil
	}
	out, err := archiveOK(repo, env, time.Minute, nil, "for-each-ref", "--sort=-creatordate",
		"--format=%(refname)%00%(objectname)%00%(creatordate:unix)%00%(contents:body)%00", ArchivePrefix)
	if err != nil {
		return nil
	}
	var archives []Archive
	for _, record := range strings.Split(out, "\x00\n") {
		fields := strings.Split(record, "\x00")
		if len(fields) < 4 || !strings.HasPrefix(fields[0], ArchivePrefix) {
			continue
		}
		var meta ArchiveMeta
		if start := strings.Index(fields[3], "{"); start >= 0 {
			json.Unmarshal([]byte(fields[3][start:]), &meta)
		}
		created := meta.Created
		if created == 0 {
			created, _ = strconv.ParseInt(fields[2], 10, 64)
		}
		archive := Archive{Ref: fields[0], Name: strings.TrimPrefix(fields[0], ArchivePrefix), Sha: fields[1], Meta: meta}
		if created > 0 {
			archive.Created = time.Unix(created, 0)
		}
		archives = append(archives, archive)
	}
	return archives
}

// Archive of the name or ref, else the newest archive of a tree of that name; nil when there is none
func Find(repo, name string) *Archive {
	archives := Archives(repo)
	wanted := strings.TrimPrefix(name, ArchivePrefix)
	for i := range archives {
		if archives[i].Name == wanted {
			return &archives[i]
		}
	}
	for i := range archives {
		if archives[i].Meta.Name == name || archives[i].Name == RefSafe(name) {
			return &archives[i]
		}
	}
	return nil
}

// Deletes the archives older than the retention, none for zero, or only names them for a dry run: their refs
func Expire(repo string, retentionDays float64, dryRun bool) ([]string, error) {
	if retentionDays <= 0 {
		return nil, nil
	}
	cutoff := time.Now().Add(-time.Duration(retentionDays * 24 * float64(time.Hour)))
	var expired []Archive
	for _, archive := range Archives(repo) {
		if !archive.Created.IsZero() && archive.Created.Before(cutoff) {
			expired = append(expired, archive)
		}
	}
	refs := []string{}
	if dryRun || len(expired) == 0 {
		for _, archive := range expired {
			refs = append(refs, archive.Ref)
		}
		return refs, nil
	}
	env, err := archiveEnv(repo, "")
	if err != nil {
		return nil, err
	}
	for _, archive := range expired {
		result, err := archiveRun(repo, env, 0, nil, "update-ref", "-d", archive.Ref, archive.Sha)
		if err != nil {
			return nil, err
		}
		if result.Code == 0 {
			refs = append(refs, archive.Ref)
		}
	}
	return refs, nil
}

// Arguments of the worktree add of a restore: the archived branch when it can be had again, else a detached HEAD, and whether it is
// detached
func addArgs(repo, dest, branch, head string, env map[string]string) ([]string, bool, error) {
	base := []string{"worktree", "add", "--no-checkout"}
	if branch != "" {
		tip, err := archiveRun(repo, env, 0, nil, "rev-parse", "--verify", "-q", "refs/heads/"+branch+"^{commit}")
		if err != nil {
			return nil, false, err
		}
		if tip.Code != 0 {
			return append(base, "-b", branch, dest, head), false, nil
		}
		if strings.TrimSpace(tip.Stdout) == head {
			listing, err := archiveRun(repo, env, 0, nil, "worktree", "list", "--porcelain")
			if err != nil {
				return nil, false, err
			}
			checkedOut := false
			for line := range strings.Lines(listing.Stdout) {
				if strings.TrimRight(line, "\r\n") == "branch refs/heads/"+branch {
					checkedOut = true
				}
			}
			if !checkedOut {
				return append(base, dest, branch), false, nil
			}
		}
	}
	return append(base, "--detach", dest, head), true, nil
}

// What a restore made
type Restored struct {
	Path string
	// Branch it is on, or that moved on or is checked out elsewhere when it is detached
	Branch   string
	Detached bool
	Head     string
	Ref      string
	Skipped  []Skipped
}

// Makes the archived tree again at its first path or the target, with its exact files and index
func (w *Worktrees) Restore(ctx context.Context, repo, name, target string) (*Restored, error) {
	archive := Find(repo, name)
	if archive == nil {
		return nil, &ArchiveError{"There is no worktree archive named " + name + ". Run `icebath worktree list` to see the archives."}
	}
	head := cmp.Or(archive.Meta.Head, archive.Sha+"^1")
	indexCommit := cmp.Or(archive.Meta.IndexCommit, archive.Sha+"^2")
	dest := cmp.Or(target, cmp.Or(archive.Meta.Path, filepath.Join(repo, ".worktrees", archive.Name)))
	dest, err := filepath.Abs(dest)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(dest); err == nil {
		entries, _ := os.ReadDir(dest)
		if !info.IsDir() || len(entries) > 0 {
			return nil, &ArchiveError{dest + " already exists. Give another folder with --path."}
		}
	}
	env, err := archiveEnv(repo, "")
	if err != nil {
		return nil, err
	}
	args, detached, err := addArgs(repo, dest, archive.Meta.Branch, head, env)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
		return nil, err
	}
	if _, err := archiveOK(repo, env, 0, nil, args...); err != nil {
		return nil, err
	}
	tree, err := archiveEnv(dest, "")
	if err != nil {
		return nil, err
	}
	empty, err := archiveOK(dest, tree, 0, []byte{}, "mktree")
	if err != nil {
		return nil, err
	}
	if _, err := archiveOK(dest, tree, 0, nil, "read-tree", archive.Sha+"^{tree}"); err != nil {
		return nil, err
	}
	if _, err := rawAttrs(dest, tree, strings.TrimSpace(empty), "checkout-index", "-a", "-f"); err != nil {
		return nil, err
	}
	if err := writeRaw(dest, tree); err != nil {
		return nil, err
	}
	if _, err := archiveOK(dest, tree, 0, nil, "read-tree", indexCommit+"^{tree}"); err != nil {
		return nil, err
	}
	if _, err := archiveRun(dest, tree, 0, nil, "update-index", "-q", "--refresh"); err != nil {
		return nil, err
	}
	w.Hooks().Recorded(ctx, repo)
	return &Restored{Path: dest, Branch: archive.Meta.Branch, Detached: detached, Head: head, Ref: archive.Ref, Skipped: archive.Meta.Skipped}, nil
}
