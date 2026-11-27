package checkpoints

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// One snapshot of a project
type Entry struct {
	Hash   string
	Short  string
	When   time.Time
	Reason string
	// What it changed against the snapshot before it
	Files, Insertions, Deletions int
}

var (
	shortstatFiles      = regexp.MustCompile(`(\d+) file`)
	shortstatInsertions = regexp.MustCompile(`(\d+) insertion`)
	shortstatDeletions  = regexp.MustCompile(`(\d+) deletion`)
	commitHash          = regexp.MustCompile(`\A[0-9a-fA-F]{4,64}\n?\z`)
)

// Snapshots of the project in the folder, newest first, as many as are kept
func (s *Store) List(dir string) []Entry {
	wd := normalize(dir)
	if !s.made() {
		return nil
	}
	allowed := step{allowed: []int{128, 129}}
	ok, out, _ := s.git(wd, allowed, "log", ref(hash(wd)), "--format=%H|%h|%aI|%s", "-n", strconv.Itoa(s.snapshots))
	if !ok || out == "" {
		return nil
	}
	var entries []Entry
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			continue
		}
		when, _ := time.Parse(time.RFC3339, parts[2])
		entry := Entry{Hash: parts[0], Short: parts[1], When: when, Reason: parts[3]}
		if ok, stat, _ := s.git(wd, allowed, "diff", "--shortstat", parts[0]+"~1", parts[0]); ok && stat != "" {
			for _, field := range []struct {
				pattern *regexp.Regexp
				into    *int
			}{{shortstatFiles, &entry.Files}, {shortstatInsertions, &entry.Insertions}, {shortstatDeletions, &entry.Deletions}} {
				if m := field.pattern.FindStringSubmatch(stat); m != nil {
					*field.into, _ = strconv.Atoi(m[1])
				}
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

// Hash of the snapshot a reference names: its number in the listing, counted from one, or a hash as it is
func (s *Store) Resolve(dir, reference string) (string, error) {
	number, err := strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(reference), "_", ""))
	if err != nil {
		return reference, nil
	}
	entries := s.List(dir)
	if number < 1 || number > len(entries) {
		return "", fmt.Errorf("Invalid checkpoint number. Use 1-%d.", len(entries))
	}
	return entries[number-1].Hash, nil
}

// Why a snapshot hash can't be used, nil when it can
func checkHash(sha string) error {
	switch {
	case strings.TrimSpace(sha) == "":
		return errors.New("Empty commit hash")
	case strings.HasPrefix(sha, "-"):
		return fmt.Errorf("Invalid commit hash (must not start with '-'): %q", sha)
	case !commitHash.MatchString(sha):
		return fmt.Errorf("Invalid commit hash (expected 4-64 hex characters): %q", sha)
	}
	return nil
}

// Why a file to restore can't be, nil when it can: it must be relative and stay inside the project
func checkFile(file, wd string) error {
	switch {
	case strings.TrimSpace(file) == "":
		return errors.New("Empty file path")
	case filepath.IsAbs(file):
		return fmt.Errorf("File path must be relative, got absolute path: %q", file)
	}
	if rel, err := filepath.Rel(wd, resolve(wd+string(filepath.Separator)+file)); err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("File path escapes the working directory via traversal: %q", file)
	}
	return nil
}

// Changes between a snapshot and the files now
type Diff struct {
	Stat  string
	Patch string
	// Snapshot a session diff compares with, its oldest kept
	Baseline string
	// Nothing changed
	Empty bool
}

// What changed in the project since the snapshot, its files and its lines, the store lock held
func (s *Store) Diff(dir, sha string) (*Diff, error) {
	release, err := lock(s.base)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := checkHash(sha); err != nil {
		return nil, err
	}
	wd := normalize(dir)
	if !s.made() {
		return nil, errors.New("No checkpoints exist for this directory")
	}
	if ok, _, _ := s.git(wd, step{}, "cat-file", "-t", sha); !ok {
		return nil, fmt.Errorf("Checkpoint '%s' not found", sha)
	}
	project := hash(wd)
	index := s.index(project)
	s.git(wd, step{timeout: s.timeout * 2, index: index}, append([]string{"add", "-A"}, s.outside(wd)...)...)
	okStat, stat, _ := s.git(wd, step{index: index}, "diff", "--stat", sha, "--cached")
	okPatch, patch, _ := s.git(wd, step{index: index}, "diff", sha, "--cached", "--no-color")
	s.git(wd, step{index: index, allowed: []int{128}}, "read-tree", ref(project))
	if !okStat && !okPatch {
		return nil, errors.New("Could not generate diff")
	}
	diff := &Diff{}
	if okStat {
		diff.Stat = stat
	}
	if okPatch {
		diff.Patch = patch
	}
	diff.Empty = diff.Stat == "" && diff.Patch == ""
	return diff, nil
}

// Everything that changed in the project since its oldest kept snapshot, empty before the first
func (s *Store) SessionDiff(dir string) (*Diff, error) {
	entries := s.List(dir)
	if len(entries) == 0 {
		return &Diff{Empty: true}, nil
	}
	baseline := entries[len(entries)-1].Hash
	diff, err := s.Diff(dir, baseline)
	if err != nil {
		return nil, err
	}
	diff.Baseline = baseline
	return diff, nil
}

// What a restore did
type Restored struct {
	// Short hash of the snapshot and why it was taken
	To     string
	Reason string
	// Project, and the file asked for alone
	Directory string
	File      string
	// The restore went by what the agent wrote, keeping the files the user changed since
	Safe bool
	// Files a safe restore put back, the ones the user changed since that it kept, the ones too large for snapshots it left, and those
	// it could not delete
	Files         []string
	Edits         []string
	Oversize      []string
	FailedDeletes []string
}

// Puts the project, or one file of it, back as the snapshot has it, the store lock held. A safe restore of the whole project puts back
// only the files the agent wrote, keeping those the user changed since. A snapshot of the files as they are is taken first
func (s *Store) Restore(dir, sha, file string, safe bool) (*Restored, error) {
	release, err := lock(s.base)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := checkHash(sha); err != nil {
		return nil, err
	}
	wd := normalize(dir)
	if file != "" {
		if err := checkFile(file, wd); err != nil {
			return nil, err
		}
	}
	if !s.made() {
		return nil, errors.New("No checkpoints exist for this directory")
	}
	if ok, _, _ := s.git(wd, step{}, "cat-file", "-t", sha); !ok {
		return nil, fmt.Errorf("Checkpoint '%s' not found", sha)
	}
	ok, tree, errs := s.git(wd, step{}, "ls-tree", "-r", "-z", sha)
	if !ok {
		return nil, errors.New("Could not inspect checkpoint: " + errs)
	}
	var selected []string
	narrowed := false
	if nested := gitlinks(tree); len(nested) > 0 {
		blocked, picked, err := s.selection(wd, sha, file, nested)
		if err != nil {
			return nil, err
		}
		if len(blocked) > 0 {
			return nil, fmt.Errorf("Checkpoint contains nested git repositories that were not captured (%s); rollback was not performed",
				strings.Join(blocked, ", "))
		}
		selected, narrowed = picked, picked != nil
	}
	restored := &Restored{To: short(sha), Directory: wd, File: file}
	var plan []string
	if safe && file == "" {
		restore, kept, known, err := s.plan(wd, sha)
		if err != nil {
			return nil, err
		}
		if known {
			restored.Safe, restored.Edits, plan = true, kept, restore
			if len(plan) == 0 {
				restored.Reason = "nothing to restore (all changed files were user-edited)"
				return restored, nil
			}
		}
	}
	if _, err := s.take(wd, "pre-rollback snapshot (restoring to "+short(sha)+")", false); err != nil {
		return nil, err
	}
	index := s.index(hash(wd))
	slow := step{timeout: s.timeout * 2, index: index}
	switch {
	case restored.Safe:
		var checkout []string
		for _, rel := range plan {
			inSnapshot, _, _ := s.git(wd, step{allowed: []int{1, 128}}, "cat-file", "-e", sha+":"+rel)
			switch {
			case inSnapshot:
				checkout = append(checkout, rel)
			case s.oversize(filepath.Join(wd, rel)):
				restored.Oversize = append(restored.Oversize, rel)
			default:
				target := filepath.Join(wd, rel)
				info, err := os.Lstat(target)
				if err != nil || !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
					continue
				}
				if err := os.Remove(target); err != nil {
					slog.Warn("Safe restore: could not remove "+rel, "error", err)
					restored.FailedDeletes = append(restored.FailedDeletes, rel)
				}
			}
		}
		ok, errs = true, ""
		if len(checkout) > 0 {
			ok, _, errs = s.git(wd, slow, append([]string{"checkout", sha, "--"}, checkout...)...)
		}
	case narrowed:
		if ok, errs, err = s.checkoutSelected(wd, sha, index, selected); err != nil {
			return nil, err
		}
	default:
		ok, _, errs = s.git(wd, slow, "checkout", sha, "--", cmp.Or(file, "."))
	}
	if !ok {
		return nil, errors.New("Restore failed: " + errs)
	}
	restored.Reason = "unknown"
	if ok, reason, _ := s.git(wd, step{}, "log", "--format=%s", "-1", sha); ok {
		restored.Reason = reason
	}
	if restored.Safe {
		for _, rel := range plan {
			if !slices.Contains(restored.Oversize, rel) && !slices.Contains(restored.FailedDeletes, rel) {
				restored.Files = append(restored.Files, rel)
			}
		}
	}
	s.trim(wd, ref(hash(wd)))
	return restored, nil
}

// Files changed since the snapshot split into those the agent wrote last, to put back, and those the user changed after it, to keep;
// false when the agent wrote nothing that was noted, so every file goes back
func (s *Store) plan(wd, sha string) (restore, kept []string, known bool, err error) {
	project := hash(wd)
	index := s.index(project)
	s.git(wd, step{timeout: s.timeout * 2, index: index}, append([]string{"add", "-A"}, s.outside(wd)...)...)
	ok, names, errs := s.git(wd, step{index: index}, "diff", "--name-only", "-z", sha, "--cached")
	s.git(wd, step{index: index, allowed: []int{128}}, "read-tree", ref(project))
	if !ok {
		return nil, nil, false, errors.New("Could not compute changed files: " + errs)
	}
	ledger := s.ledger(hash(s.Project(wd)))
	if len(ledger) == 0 {
		return nil, nil, false, nil
	}
	restore, kept = []string{}, []string{}
	for _, rel := range strings.Split(names, "\x00") {
		if rel == "" {
			continue
		}
		path := filepath.Join(wd, rel)
		written, noted := ledger[path]
		current, _ := fsx.Digest(path)
		if noted && (current == "" || current == written.SHA256) {
			restore = append(restore, rel)
		} else {
			kept = append(kept, rel)
		}
	}
	return restore, kept, true, nil
}

// Paths of the nested repositories a snapshot holds only as a commit reference, from its ls-tree -r -z listing
func gitlinks(listing string) []string {
	var paths []string
	for _, record := range strings.Split(listing, "\x00") {
		if !strings.HasPrefix(record, "160000 commit ") {
			continue
		}
		if _, path, ok := strings.Cut(record, "\t"); ok {
			paths = append(paths, path)
		}
	}
	return paths
}

// Note a snapshot's reason ends with when it could not capture the files of nested repositories
func uncaptured(nested []string) string {
	more := ""
	if len(nested) > 3 {
		more = fmt.Sprintf(" (+%d)", len(nested)-3)
	}
	return " [nested git repos not captured: " + strings.Join(nested[:min(3, len(nested))], ", ") + more + "]"
}

// Parts of a relative path, empty and . parts left out
func parts(path string) []string {
	separators := "/"
	if runtime.GOOS == "windows" {
		separators = `/\`
	}
	var out []string
	for _, part := range strings.FieldsFunc(path, func(r rune) bool { return strings.ContainsRune(separators, r) }) {
		if part != "." {
			out = append(out, part)
		}
	}
	return out
}

// Whether one list of parts starts the other
func overlap(a, b []string) bool {
	n := min(len(a), len(b))
	return slices.Equal(a[:n], b[:n])
}

// Nested repositories a restore of the file, or of everything, would wrongly empty, and the entries of the snapshot the file names, read
// through a throwaway index; nil entries for a restore of everything
func (s *Store) selection(wd, sha, file string, nested []string) (blocked, selected []string, err error) {
	if file == "" {
		return nested, nil, nil
	}
	requested := parts(file)
	if rel, err := filepath.Rel(wd, filepath.Clean(filepath.Join(wd, file))); err == nil && filepath.IsLocal(rel) {
		requested = parts(rel)
	}
	for _, repo := range nested {
		if overlap(requested, parts(filepath.ToSlash(repo))) {
			blocked = append(blocked, repo)
		}
	}
	if len(blocked) > 0 {
		return blocked, nil, nil
	}
	scratch, err := os.MkdirTemp(s.store(), "restore-inspect-")
	if err != nil {
		return nil, nil, err
	}
	defer removeTree(scratch)
	inspect := step{index: filepath.Join(scratch, "index")}
	ok, _, errs := s.git(wd, inspect, "read-tree", sha)
	listed := ""
	if ok {
		ok, listed, errs = s.git(wd, inspect, "ls-files", "--stage", "--error-unmatch", "-z", "--", file)
	}
	if !ok {
		return nil, nil, errors.New("Could not inspect restore selection: " + errs)
	}
	selected = []string{}
	for _, record := range strings.Split(listed, "\x00") {
		_, path, ok := strings.Cut(record, "\t")
		if !ok {
			continue
		}
		if strings.HasPrefix(record, "160000 ") {
			blocked = append(blocked, path)
		}
		selected = append(selected, path)
	}
	if len(selected) == 0 && len(blocked) == 0 {
		return nil, nil, errors.New("Restore selection matched no files in checkpoint: " + file)
	}
	return blocked, selected, nil
}

// Checks out exactly the entries selected, named in a NUL separated file of literal pathspecs
func (s *Store) checkoutSelected(wd, sha, index string, selected []string) (bool, string, error) {
	scratch, err := os.MkdirTemp(s.store(), "restore-select-")
	if err != nil {
		return false, "", err
	}
	defer removeTree(scratch)
	spec := filepath.Join(scratch, "pathspec")
	if err := os.WriteFile(spec, []byte(strings.Join(selected, "\x00")+"\x00"), 0o600); err != nil {
		return false, "", err
	}
	ok, _, errs := s.git(wd, step{timeout: s.timeout * 2, index: index, env: map[string]string{"GIT_LITERAL_PATHSPECS": "1"}},
		"checkout", sha, "--pathspec-from-file="+spec, "--pathspec-file-nul")
	return ok, errs, nil
}

// Tree of the newest kept snapshot taken at or before the time, the oldest kept when all are later; empty when none was taken from
// that time on
func (s *Store) spanBase(wd string, since time.Time) string {
	ok, out, _ := s.git(wd, step{allowed: []int{128, 129}}, "log", ref(hash(wd)), "--format=%T|%at", "-n", strconv.Itoa(s.snapshots))
	if !ok {
		return ""
	}
	type row struct {
		tree string
		at   int64
	}
	var rows []row
	for _, line := range strings.Split(out, "\n") {
		tree, stamp, _ := strings.Cut(line, "|")
		at, err := strconv.ParseInt(stamp, 10, 64)
		if tree != "" && err == nil {
			rows = append(rows, row{tree, at})
		}
	}
	start := float64(since.UnixNano()) / 1e9
	if !slices.ContainsFunc(rows, func(r row) bool { return float64(r.at) >= math.Floor(start) }) {
		return ""
	}
	for _, r := range rows {
		if float64(r.at) <= start {
			return r.tree
		}
	}
	return rows[len(rows)-1].tree
}

// Files of the project that differ from the snapshot a span starting at the time began from: changed and deleted ones and new ones, read
// through a throwaway index so nothing of the store is written; none when snapshots are off, none was taken in the span or git fails
func (s *Store) Changed(dir string, since time.Time) []string {
	if !s.enabled || since.IsZero() {
		return nil
	}
	wd := normalize(dir)
	if !s.made() {
		return nil
	}
	tree := s.spanBase(wd, since)
	if tree == "" {
		return nil
	}
	return s.changedFrom(wd, tree)
}

// Files of the project that differ from the tree, sorted
func (s *Store) changedFrom(wd, tree string) []string {
	scratch, err := os.MkdirTemp(s.store(), "undo-hint-")
	if err != nil {
		return nil
	}
	defer os.RemoveAll(scratch)
	index := step{index: filepath.Join(scratch, "index")}
	if ok, _, _ := s.git(wd, index, "read-tree", tree); !ok {
		return nil
	}
	outside := s.outside(wd)
	okChanged, changed, _ := s.git(wd, index, append([]string{"diff", "--name-only", "-z"}, outside...)...)
	okNew, added, _ := s.git(wd, index, append([]string{"ls-files", "--others", "--exclude-standard", "-z"}, outside...)...)
	if !okChanged || !okNew {
		return nil
	}
	var paths []string
	for _, path := range strings.Split(changed+"\x00"+added, "\x00") {
		if path != "" && !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	return paths
}
