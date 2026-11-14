package checkpoint

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type Checkpoint struct {
	Hash         string
	ShortHash    string
	Timestamp    string
	Reason       string
	FilesChanged int
	Insertions   int
	Deletions    int
	Workdir      string
}

type DiffResult struct {
	Stat     string
	Diff     string
	Baseline string
	Empty    bool
}

type RestoreResult struct {
	RestoredTo       string
	Reason           string
	Directory        string
	File             string
	Selective        bool
	RestoredFiles    []string
	SkippedUserEdits []string
	SkippedOversize  []string
	FailedDeletes    []string
}

var (
	shortstatFiles      = regexp.MustCompile(`(\d+) file`)
	shortstatInsertions = regexp.MustCompile(`(\d+) insertion`)
	shortstatDeletions  = regexp.MustCompile(`(\d+) deletion`)
)

func shortstatCount(re *regexp.Regexp, line string) (int, bool) {
	match := re.FindStringSubmatch(line)
	if match == nil {
		return 0, false
	}
	n, err := strconv.Atoi(match[1])
	return n, err == nil
}

func parseShortstat(line string, cp *Checkpoint) {
	if n, ok := shortstatCount(shortstatFiles, line); ok {
		cp.FilesChanged = n
	}
	if n, ok := shortstatCount(shortstatInsertions, line); ok {
		cp.Insertions = n
	}
	if n, ok := shortstatCount(shortstatDeletions, line); ok {
		cp.Deletions = n
	}
}

func (m *Manager) List(ctx context.Context, dir string) ([]Checkpoint, error) {
	abs, err := canonical(dir)
	if err != nil {
		return nil, err
	}
	l, err := m.layout(ctx)
	if err != nil {
		return nil, err
	}
	return m.list(ctx, l, abs)
}

func (m *Manager) list(ctx context.Context, l layout, abs string) ([]Checkpoint, error) {
	if !l.hasHead() {
		return nil, nil
	}
	hash, err := projectHash(abs)
	if err != nil {
		return nil, err
	}
	out, err := m.runGit(ctx, l.store, abs, []string{"log", refName(hash), "--format=%H|%h|%aI|%s", "-n", strconv.Itoa(m.maxSnapshots)}, gitOpts{})
	if exitedWith(err, 128, 129) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	var results []Checkpoint
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			continue
		}
		cp := Checkpoint{Hash: parts[0], ShortHash: parts[1], Timestamp: parts[2], Reason: parts[3]}
		stat, err := m.runGit(ctx, l.store, abs, []string{"diff", "--shortstat", parts[0] + "~1", parts[0]}, gitOpts{})
		if err != nil && !exitedWith(err, 128, 129) {
			return nil, err
		}
		if err == nil && stat != "" {
			parseShortstat(stat, &cp)
		}
		results = append(results, cp)
	}
	return results, nil
}

func (m *Manager) ListAll(ctx context.Context) ([]Checkpoint, error) {
	l, err := m.layout(ctx)
	if err != nil {
		return nil, err
	}
	if !l.hasHead() {
		return nil, nil
	}
	projects, err := listProjects(l)
	if err != nil {
		return nil, err
	}
	var results []Checkpoint
	for _, p := range projects {
		workdir := p.workdir()
		if workdir == "" {
			continue
		}
		abs, err := canonical(workdir)
		if err != nil {
			return nil, err
		}
		if info, err := os.Stat(abs); err != nil || !info.IsDir() {
			continue
		}
		entries, err := m.list(ctx, l, abs)
		if err != nil {
			return nil, err
		}
		for _, cp := range entries {
			cp.Workdir = workdir
			results = append(results, cp)
		}
	}
	slices.SortStableFunc(results, func(a, b Checkpoint) int { return cmp.Compare(b.Timestamp, a.Timestamp) })
	return results, nil
}

func (m *Manager) Diff(ctx context.Context, dir, hash string) (res DiffResult, err error) {
	l, err := m.layout(ctx)
	if err != nil {
		return DiffResult{}, err
	}
	lk, err := m.lock(l)
	if err != nil {
		return DiffResult{}, err
	}
	defer release(lk, &err)
	return m.diff(ctx, l, dir, hash)
}

func (m *Manager) checkpointExists(ctx context.Context, l layout, abs, hash string) error {
	if _, err := m.runGit(ctx, l.store, abs, []string{"cat-file", "-t", hash}, gitOpts{}); err != nil {
		if _, ok := exitCode(err); ok {
			return &NotFoundError{Hash: hash, Detail: err.Error()}
		}
		return err
	}
	return nil
}

func (m *Manager) diff(ctx context.Context, l layout, dir, hash string) (DiffResult, error) {
	if err := validateHash(hash); err != nil {
		return DiffResult{}, err
	}
	abs, err := canonical(dir)
	if err != nil {
		return DiffResult{}, err
	}
	if !l.hasHead() {
		return DiffResult{}, ErrNoCheckpoints
	}
	if err := m.checkpointExists(ctx, l, abs, hash); err != nil {
		return DiffResult{}, err
	}
	phash, err := projectHash(abs)
	if err != nil {
		return DiffResult{}, err
	}
	index := l.index(phash)
	if _, err := m.runGit(ctx, l.store, abs, []string{"add", "-A"}, gitOpts{index: index, scale: 2}); err != nil {
		return DiffResult{}, err
	}
	stat, statErr := m.runGit(ctx, l.store, abs, []string{"diff", "--stat", hash, "--cached"}, gitOpts{index: index})
	body, diffErr := m.runGit(ctx, l.store, abs, []string{"diff", hash, "--cached", "--no-color"}, gitOpts{index: index})
	if _, err := m.runGit(ctx, l.store, abs, []string{"read-tree", refName(phash)}, gitOpts{index: index}); err != nil && !exitedWith(err, 128) {
		return DiffResult{}, err
	}
	if statErr != nil && diffErr != nil {
		return DiffResult{}, fmt.Errorf("Could not generate diff: %w", errors.Join(statErr, diffErr))
	}
	if statErr != nil {
		stat = ""
	}
	if diffErr != nil {
		body = ""
	}
	return DiffResult{Stat: stat, Diff: body}, nil
}

func (m *Manager) SessionDiff(ctx context.Context, dir string) (DiffResult, error) {
	checkpoints, err := m.List(ctx, dir)
	if err != nil {
		return DiffResult{}, err
	}
	if len(checkpoints) == 0 {
		return DiffResult{Empty: true}, nil
	}
	baseline := checkpoints[len(checkpoints)-1].Hash
	res, err := m.Diff(ctx, dir, baseline)
	if err != nil {
		return res, err
	}
	res.Baseline = baseline
	if res.Stat == "" && res.Diff == "" {
		res.Empty = true
	}
	return res, nil
}

type restorePlan struct {
	restore     []string
	skipped     []string
	ledgerEmpty bool
}

func (m *Manager) safePlan(ctx context.Context, l layout, abs, hash string) (restorePlan, error) {
	phash, err := projectHash(abs)
	if err != nil {
		return restorePlan{}, err
	}
	index := l.index(phash)
	if _, err := m.runGit(ctx, l.store, abs, []string{"add", "-A"}, gitOpts{index: index, scale: 2}); err != nil {
		return restorePlan{}, fmt.Errorf("Could not compute changed files: %w", err)
	}
	names, diffErr := m.runGit(ctx, l.store, abs, []string{"diff", "--name-only", "-z", hash, "--cached"}, gitOpts{index: index})
	if _, err := m.runGit(ctx, l.store, abs, []string{"read-tree", refName(phash)}, gitOpts{index: index}); err != nil && !exitedWith(err, 128) {
		return restorePlan{}, err
	}
	if diffErr != nil {
		return restorePlan{}, fmt.Errorf("Could not compute changed files: %w", diffErr)
	}
	key, err := m.ledgerKey(l, abs)
	if err != nil {
		return restorePlan{}, err
	}
	ledger := loadLedger(l, key)
	if len(ledger) == 0 {
		return restorePlan{ledgerEmpty: true}, nil
	}
	var plan restorePlan
	for _, rel := range strings.Split(names, "\x00") {
		if rel == "" {
			continue
		}
		path := filepath.Join(abs, filepath.FromSlash(rel))
		entry, _ := ledger[path].(map[string]any)
		recorded, has := entry["sha256"]
		if entry == nil || !has || recorded == nil {
			plan.skipped = append(plan.skipped, rel)
			continue
		}
		current, err := hashFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			plan.restore = append(plan.restore, rel)
		case err != nil:
			return restorePlan{}, err
		case current == recorded:
			plan.restore = append(plan.restore, rel)
		default:
			plan.skipped = append(plan.skipped, rel)
		}
	}
	return plan, nil
}

func (m *Manager) Restore(ctx context.Context, dir, hash, file string, safe bool) (res *RestoreResult, err error) {
	l, err := m.layout(ctx)
	if err != nil {
		return nil, err
	}
	lk, err := m.lock(l)
	if err != nil {
		return nil, err
	}
	defer release(lk, &err)
	return m.restore(ctx, l, dir, hash, file, safe)
}

func (m *Manager) inspectSelection(ctx context.Context, l layout, abs, hash, file string) ([]string, error) {
	scratch, err := os.MkdirTemp(l.store, "restore-inspect-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	index := filepath.Join(scratch, "index")
	if _, err := m.runGit(ctx, l.store, abs, []string{"read-tree", hash}, gitOpts{index: index}); err != nil {
		return nil, fmt.Errorf("Could not inspect restore selection: %w", err)
	}
	out, err := m.runGit(ctx, l.store, abs, []string{"ls-files", "--stage", "--error-unmatch", "-z", "--", file}, gitOpts{index: index})
	if err != nil {
		return nil, fmt.Errorf("Could not inspect restore selection: %w", err)
	}
	return strings.Split(out, "\x00"), nil
}

func (m *Manager) restore(ctx context.Context, l layout, dir, hash, file string, safe bool) (*RestoreResult, error) {
	if err := validateHash(hash); err != nil {
		return nil, err
	}
	abs, err := canonical(dir)
	if err != nil {
		return nil, err
	}
	if file != "" {
		if err := validateFilePath(file, abs); err != nil {
			return nil, err
		}
	}
	if !l.hasHead() {
		return nil, ErrNoCheckpoints
	}
	if err := m.checkpointExists(ctx, l, abs, hash); err != nil {
		return nil, err
	}
	listing, err := m.runGit(ctx, l.store, abs, []string{"ls-tree", "-r", "-z", hash}, gitOpts{})
	if err != nil {
		return nil, fmt.Errorf("Could not inspect checkpoint: %w", err)
	}
	var selected []string
	haveSelection := false
	if nested := gitlinkPaths(listing); len(nested) > 0 {
		blocked := nested
		if file != "" {
			joined, _ := joinRelative(abs, file)
			rel, ok := relativeTo(filepath.Clean(joined), abs)
			if !ok {
				rel = file
			}
			requested := partsOf(rel)
			blocked = nil
			for _, repo := range nested {
				repoParts := posixParts(repo)
				if hasPrefixParts(requested, repoParts) || hasPrefixParts(repoParts, requested) {
					blocked = append(blocked, repo)
				}
			}
			if len(blocked) == 0 {
				records, err := m.inspectSelection(ctx, l, abs, hash, file)
				if err != nil {
					return nil, err
				}
				for _, record := range records {
					if _, path, ok := strings.Cut(record, "\t"); ok {
						if strings.HasPrefix(record, "160000 ") {
							blocked = append(blocked, path)
						}
						selected = append(selected, path)
					}
				}
				haveSelection = true
				if len(selected) == 0 {
					return nil, &InputError{msg: "Restore selection matched no files in checkpoint: " + file}
				}
			}
		}
		if len(blocked) > 0 {
			return nil, &NestedReposError{Paths: blocked}
		}
	}
	short := hash[:min(8, len(hash))]
	var plan restorePlan
	selective := false
	if safe && file == "" {
		plan, err = m.safePlan(ctx, l, abs, hash)
		if err != nil {
			return nil, err
		}
		if !plan.ledgerEmpty {
			selective = true
			if len(plan.restore) == 0 {
				return &RestoreResult{
					RestoredTo:       short,
					Reason:           "nothing to restore (all changed files were user-edited)",
					Directory:        abs,
					Selective:        true,
					RestoredFiles:    []string{},
					SkippedUserEdits: append([]string{}, plan.skipped...),
					SkippedOversize:  []string{},
				}, nil
			}
		}
	}
	if _, err := m.take(ctx, l, abs, fmt.Sprintf("pre-rollback snapshot (restoring to %s)", short), false); err != nil {
		slog.Warn("pre-rollback snapshot was not taken", "dir", abs, "error", err)
	}
	phash, err := projectHash(abs)
	if err != nil {
		return nil, err
	}
	index := l.index(phash)
	var keptOversize, failedDeletes []string
	var checkoutErr error
	switch {
	case selective:
		var targets, deletes []string
		for _, rel := range plan.restore {
			_, err := m.runGit(ctx, l.store, abs, []string{"cat-file", "-e", hash + ":" + rel}, gitOpts{})
			switch {
			case err == nil:
				targets = append(targets, rel)
			case !exitedWith(err, 1, 128):
				return nil, err
			case m.exceedsSizeCap(filepath.Join(abs, filepath.FromSlash(rel))):
				keptOversize = append(keptOversize, rel)
			default:
				deletes = append(deletes, rel)
			}
		}
		for _, rel := range deletes {
			target := filepath.Join(abs, filepath.FromSlash(rel))
			info, err := os.Lstat(target)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err == nil && !info.Mode().IsRegular() && info.Mode()&fs.ModeSymlink == 0 {
				continue
			}
			if err == nil {
				err = os.Remove(target)
			}
			if err != nil {
				slog.Warn("Safe restore: could not remove file", "path", rel, "error", err)
				failedDeletes = append(failedDeletes, rel)
			}
		}
		if len(targets) > 0 {
			_, checkoutErr = m.runGit(ctx, l.store, abs, append([]string{"checkout", hash, "--"}, targets...), gitOpts{index: index, scale: 2})
		}
	case haveSelection:
		checkoutErr = m.checkoutSelection(ctx, l, abs, hash, index, selected)
	default:
		target := file
		if target == "" {
			target = "."
		}
		_, checkoutErr = m.runGit(ctx, l.store, abs, []string{"checkout", hash, "--", target}, gitOpts{index: index, scale: 2})
	}
	if checkoutErr != nil {
		return nil, fmt.Errorf("Restore failed: %w", checkoutErr)
	}
	reason, err := m.runGit(ctx, l.store, abs, []string{"log", "--format=%s", "-1", hash}, gitOpts{})
	if err != nil {
		reason = "unknown"
	}
	res := &RestoreResult{RestoredTo: short, Reason: reason, Directory: abs, File: file}
	if selective {
		res.Selective = true
		res.RestoredFiles = []string{}
		for _, rel := range plan.restore {
			if !slices.Contains(keptOversize, rel) && !slices.Contains(failedDeletes, rel) {
				res.RestoredFiles = append(res.RestoredFiles, rel)
			}
		}
		res.SkippedUserEdits = append([]string{}, plan.skipped...)
		res.SkippedOversize = append([]string{}, keptOversize...)
		res.FailedDeletes = failedDeletes
	}
	m.budget(ctx, l.store, abs, refName(phash))
	return res, nil
}

func (m *Manager) checkoutSelection(ctx context.Context, l layout, abs, hash, index string, selected []string) error {
	scratch, err := os.MkdirTemp(l.store, "restore-select-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	var spec strings.Builder
	for _, p := range selected {
		spec.WriteString(p)
		spec.WriteByte(0)
	}
	specFile := filepath.Join(scratch, "pathspec")
	if err := os.WriteFile(specFile, []byte(spec.String()), 0o600); err != nil {
		return err
	}
	_, err = m.runGit(ctx, l.store, abs, []string{"checkout", hash, "--pathspec-from-file=" + specFile, "--pathspec-file-nul"},
		gitOpts{index: index, scale: 2, extra: map[string]string{"GIT_LITERAL_PATHSPECS": "1"}})
	return err
}

func FormatList(checkpoints []Checkpoint, directory string) string {
	if len(checkpoints) == 0 {
		return "No checkpoints found for " + directory
	}
	lines := []string{"📸 Checkpoints for " + directory + ":\n"}
	for i, cp := range checkpoints {
		ts := cp.Timestamp
		if strings.Contains(ts, "T") {
			clock := strings.Split(ts, "T")[1]
			clock = strings.Split(clock, "+")[0]
			clock = strings.Split(clock, "-")[0]
			clock = string([]rune(clock)[:min(5, len([]rune(clock)))])
			ts = strings.Split(cp.Timestamp, "T")[0] + " " + clock
		}
		stat := ""
		if cp.FilesChanged != 0 {
			plural := "s"
			if cp.FilesChanged == 1 {
				plural = ""
			}
			stat = fmt.Sprintf("  (%d file%s, +%d/-%d)", cp.FilesChanged, plural, cp.Insertions, cp.Deletions)
		}
		if cp.Workdir != "" && directory == "all directories" {
			name := filepath.Base(cp.Workdir)
			if name == "." || name == "" || os.IsPathSeparator(name[len(name)-1]) {
				name = cp.Workdir
			}
			lines = append(lines, fmt.Sprintf("  %d. %s  %s  [%s]  %s%s", i+1, cp.ShortHash, ts, name, cp.Reason, stat))
		} else {
			lines = append(lines, fmt.Sprintf("  %d. %s  %s  %s%s", i+1, cp.ShortHash, ts, cp.Reason, stat))
		}
	}
	lines = append(lines,
		"\n  /rollback <N>             restore to checkpoint N",
		"  /rollback diff <N>        preview changes since checkpoint N",
		"  /rollback <N> <file>      restore a single file from checkpoint N")
	return strings.Join(lines, "\n")
}
