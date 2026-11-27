package checkpoints

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Snapshots the project, unless it is the root or the home folder, too big, or unchanged since its last snapshot: the snapshot its files
// are as of now, the one taken or the last when nothing changed since, empty when there is none. Never fails the call it comes before: a
// failure is logged
func (s *Store) Take(workdir, reason string) string {
	if !s.enabled {
		return ""
	}
	wd := normalize(workdir)
	if wd == filepath.Dir(wd) || wd == home() {
		slog.Debug("Checkpoint skipped: directory too broad", "directory", wd)
		return ""
	}
	release, err := lock(s.base)
	if err != nil {
		slog.Debug("Checkpoint failed (non-fatal)", "error", err)
		return ""
	}
	defer release()
	sha, err := s.take(wd, reason, true)
	if err != nil {
		slog.Debug("Checkpoint failed (non-fatal)", "error", err)
		return ""
	}
	return sha
}

// Takes one snapshot into the project's ref, the store lock held: the snapshot the files are as of, the one taken or the last when nothing
// changed, empty when there is none; an error for a failure of the disk
func (s *Store) take(wd, reason string, prune bool) (string, error) {
	failure, err := s.init()
	if err != nil {
		return "", err
	}
	if failure != "" {
		slog.Debug("Checkpoint store init failed: " + failure)
		return "", nil
	}
	s.touch(wd)
	if count(wd) > maxFiles {
		slog.Debug(fmt.Sprintf("Checkpoint skipped: >%d files in %s", maxFiles, wd))
		return "", nil
	}
	project := hash(wd)
	index, name := s.index(project), ref(project)
	if fsx.Exists(index) {
		if tip := s.tip(wd, name); tip != "" {
			s.git(wd, step{index: index, allowed: []int{128}}, "read-tree", tip)
		} else {
			os.Remove(index)
		}
	} else if err := os.MkdirAll(filepath.Dir(index), 0o700); err != nil {
		return "", err
	}
	if ok, _, errs := s.git(wd, step{timeout: s.timeout * 2, index: index}, append([]string{"add", "-A"}, s.outside(wd)...)...); !ok {
		slog.Debug("Checkpoint git-add failed: " + errs)
		return "", nil
	}
	s.dropOversize(wd, index)
	tip := s.tip(wd, name)
	if tip != "" {
		if same, _, _ := s.git(wd, step{index: index, allowed: []int{1}}, "diff-index", "--cached", "--quiet", tip); same {
			slog.Debug("Checkpoint skipped: no changes in " + wd)
			return tip, nil
		}
	} else if ok, listed, _ := s.git(wd, step{index: index}, "ls-files", "--cached"); ok && strings.TrimSpace(listed) == "" {
		slog.Debug("Checkpoint skipped: empty tree in " + wd)
		return "", nil
	}
	ok, tree, errs := s.git(wd, step{index: index}, "write-tree")
	if !ok || tree == "" {
		slog.Debug("Checkpoint write-tree failed: " + errs)
		return "", nil
	}
	if ok, listing, _ := s.git(wd, step{}, "ls-tree", "-r", "-z", tree); ok {
		if nested := gitlinks(listing); len(nested) > 0 {
			slog.Info("Checkpoint does not capture nested git repositories", "directory", wd, "repositories", strings.Join(nested, ", "))
			reason = strings.Join(strings.Fields(reason), " ") + uncaptured(nested)
		}
	}
	args := []string{"commit-tree", tree, "-m", reason, "--no-gpg-sign"}
	if tip != "" {
		args = []string{"commit-tree", tree, "-p", tip, "-m", reason, "--no-gpg-sign"}
	}
	ok, sha, errs := s.git(wd, step{index: index}, args...)
	if !ok || sha == "" {
		slog.Debug("Checkpoint commit-tree failed: " + errs)
		return "", nil
	}
	update := []string{"update-ref", name, sha}
	if tip != "" {
		update = append(update, tip)
	}
	if ok, _, errs := s.git(wd, step{}, update...); !ok {
		slog.Debug("Checkpoint update-ref failed: " + errs)
		return "", nil
	}
	slog.Debug("Checkpoint taken", "directory", wd, "reason", reason, "hash", short(sha))
	if prune {
		s.trim(wd, name)
	}
	return sha, nil
}

// First eight characters of a snapshot's hash
func short(sha string) string { return sha[:min(8, len(sha))] }

// Whether the file is larger than a snapshot takes
func (s *Store) oversize(path string) bool {
	if s.fileMB <= 0 {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Size() > int64(s.fileMB)*1024*1024
}

// Takes every staged file larger than a snapshot takes out of the project's index, two hundred at a time
func (s *Store) dropOversize(wd, index string) {
	if s.fileMB <= 0 {
		return
	}
	ok, listed, _ := s.git(wd, step{index: index}, "ls-files", "--cached", "-z")
	if !ok || listed == "" {
		return
	}
	var large []string
	for _, rel := range strings.Split(listed, "\x00") {
		if rel != "" && s.oversize(filepath.Join(wd, rel)) {
			large = append(large, rel)
		}
	}
	if len(large) == 0 {
		return
	}
	slog.Debug(fmt.Sprintf("Checkpoint: dropping %d oversize file(s) (>%d MB) from index", len(large), s.fileMB))
	for chunk := range slices.Chunk(large, 200) {
		s.git(wd, step{index: index, allowed: []int{128}}, append([]string{"rm", "--cached", "--quiet", "--"}, chunk...)...)
	}
}

// Comparison key of a path, case folded where the system folds case
func key(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(filepath.Clean(path))
	}
	return filepath.Clean(path)
}

// Folder whose snapshots a path belongs to: the deepest project of the store holding it, else the nearest folder above it with a
// project marker below the home folder and its parents, else its own folder
func (s *Store) Project(path string) string {
	path = normalize(path)
	candidate := path
	if !fsx.IsDir(path) {
		candidate = filepath.Dir(path)
	}
	owner, depth := "", -1
	for _, meta := range s.projects() {
		if meta.Workdir == "" {
			continue
		}
		root := filepath.Clean(meta.Workdir)
		if rel, err := filepath.Rel(root, candidate); err != nil || !filepath.IsLocal(rel) {
			continue
		}
		if parts := len(strings.Split(root, string(filepath.Separator))); parts > depth {
			owner, depth = root, parts
		}
	}
	if owner != "" {
		return owner
	}
	broad := map[string]bool{}
	if dir := home(); dir != "" {
		for p := resolve(dir); ; p = filepath.Dir(p) {
			broad[key(p)] = true
			if filepath.Dir(p) == p {
				break
			}
		}
	}
	for check := candidate; check != filepath.Dir(check) && !broad[key(check)]; check = filepath.Dir(check) {
		for _, marker := range markers {
			if fsx.Exists(filepath.Join(check, marker)) {
				return check
			}
		}
	}
	return candidate
}

// Notes what the agent just left in a file, the file gone for one it deleted, so a safe restore tells the agent's writes from the
// edits the user made since. Never fails the call it comes after: a failure is logged
func (s *Store) Record(path string) {
	if !s.enabled {
		return
	}
	path = normalize(path)
	sum, _ := fsx.Digest(path)
	if sum == "" && fsx.Exists(path) {
		return
	}
	release, err := lock(s.base)
	if err != nil {
		slog.Debug("Recording an agent write failed", "path", path, "error", err)
		return
	}
	defer release()
	project := hash(s.Project(path))
	ledger := s.ledger(project)
	ledger[path] = write{SHA256: sum, TS: now()}
	s.keep(project, ledger)
}

// Notes every file of the project that changed since the snapshot as written by the agent, for a command that changed files it can't
// name, such as rm or mv in a shell: a safe restore then puts them back rather than keeping them as the user's edits
func (s *Store) RecordSince(workdir, sha string) {
	if !s.enabled || sha == "" {
		return
	}
	wd := normalize(workdir)
	release, err := lock(s.base)
	if err != nil {
		slog.Debug("Recording a command's changes failed", "directory", wd, "error", err)
		return
	}
	defer release()
	ledgers := map[string]map[string]write{}
	for _, rel := range s.changedFrom(wd, sha) {
		path := filepath.Join(wd, rel)
		sum, _ := fsx.Digest(path)
		if sum == "" && fsx.Exists(path) {
			continue
		}
		project := hash(s.Project(path))
		if ledgers[project] == nil {
			ledgers[project] = s.ledger(project)
		}
		ledgers[project][path] = write{SHA256: sum, TS: now()}
	}
	for project, ledger := range ledgers {
		s.keep(project, ledger)
	}
}

// Why snapshots taken on this machine don't serve the terminal backend, empty when they do: its files are a container's or another
// machine's
func Refusal(backend terminal.Backend) string {
	switch {
	case backend.Container:
		return "Checkpoints are not taken for terminal.backend=" + backend.Name + ": file paths belong to the container, not this host."
	case backend.Remote:
		return "Checkpoints are not taken for terminal.backend=" + backend.Name + ": file paths belong to the remote machine, not this host."
	}
	return ""
}
