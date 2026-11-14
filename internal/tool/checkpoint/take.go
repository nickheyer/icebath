package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

var projectMarkers = []string{".git", "pyproject.toml", "package.json", "Cargo.toml", "go.mod", "Makefile", "pom.xml", ".hg", "Gemfile"}

func (m *Manager) lock(l layout) (*fileutil.Lock, error) {
	resolved, err := canonical(l.base)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(l.base), 0o777); err != nil {
		return nil, err
	}
	lk, err := fileutil.TryOpenLock(home.CheckpointLock.In(filepath.Dir(resolved)), fileutil.LockPerm(0o600), fileutil.Range(0, 1))
	if errors.Is(err, fileutil.ErrLocked) {
		return nil, &BusyError{Base: l.base}
	}
	if err != nil {
		return nil, err
	}
	return lk, nil
}

func release(lk *fileutil.Lock, err *error) {
	if rerr := lk.Release(); rerr != nil && *err == nil {
		*err = rerr
	}
}

func (m *Manager) EnsureCheckpoint(ctx context.Context, dir, reason string) (taken bool, err error) {
	if !m.enabled {
		return false, nil
	}
	if reason == "" {
		reason = "auto"
	}
	abs, err := canonical(dir)
	if err != nil {
		return false, err
	}
	userHome, err := home.UserHome()
	if err != nil {
		return false, err
	}
	if abs == "/" || abs == userHome {
		slog.Debug("Checkpoint skipped: directory too broad", "dir", abs)
		return false, nil
	}
	m.mu.Lock()
	if m.turn[abs] {
		m.mu.Unlock()
		return false, nil
	}
	m.turn[abs] = true
	m.mu.Unlock()
	l, err := m.layout(ctx)
	if err != nil {
		return false, err
	}
	lk, err := m.lock(l)
	if err != nil {
		return false, err
	}
	defer release(lk, &err)
	return m.take(ctx, l, abs, reason, true)
}

func (m *Manager) refTip(ctx context.Context, store, workdir, ref string) (string, error) {
	out, err := m.runGit(ctx, store, workdir, []string{"rev-parse", "--verify", ref + "^{commit}"}, gitOpts{})
	if exitedWith(err, 128) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return out, nil
}

func (m *Manager) take(ctx context.Context, l layout, workdir, reason string, prune bool) (bool, error) {
	if err := m.initStore(ctx, l); err != nil {
		return false, err
	}
	hash, err := projectHash(workdir)
	if err != nil {
		return false, err
	}
	if err := touchProject(ctx, l, workdir, hash); err != nil {
		return false, err
	}
	if dirFileCount(workdir) > maxFiles {
		return false, &TooManyFilesError{Dir: workdir, Limit: maxFiles}
	}
	index, ref := l.index(hash), refName(hash)
	if exists(index) {
		tip, err := m.refTip(ctx, l.store, workdir, ref)
		if err != nil {
			return false, err
		}
		if tip != "" {
			if _, err := m.runGit(ctx, l.store, workdir, []string{"read-tree", tip}, gitOpts{index: index}); err != nil && !exitedWith(err, 128) {
				return false, err
			}
		} else if err := os.Remove(index); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	} else if err := os.MkdirAll(filepath.Dir(index), 0o777); err != nil {
		return false, err
	}
	if _, err := m.runGit(ctx, l.store, workdir, []string{"add", "-A"}, gitOpts{index: index, scale: 2}); err != nil {
		return false, fmt.Errorf("Checkpoint git-add failed: %w", err)
	}
	if m.maxFileSizeMB > 0 {
		if err := m.dropOversize(ctx, l.store, workdir, index); err != nil {
			return false, err
		}
	}
	tip, err := m.refTip(ctx, l.store, workdir, ref)
	if err != nil {
		return false, err
	}
	if tip != "" {
		_, err := m.runGit(ctx, l.store, workdir, []string{"diff-index", "--cached", "--quiet", tip}, gitOpts{index: index})
		if err == nil {
			slog.Debug("Checkpoint skipped: no changes", "dir", workdir)
			return false, nil
		}
		if !exitedWith(err, 1) {
			return false, err
		}
	} else {
		out, err := m.runGit(ctx, l.store, workdir, []string{"ls-files", "--cached"}, gitOpts{index: index})
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(out) == "" {
			slog.Debug("Checkpoint skipped: empty tree", "dir", workdir)
			return false, nil
		}
	}
	tree, err := m.runGit(ctx, l.store, workdir, []string{"write-tree"}, gitOpts{index: index})
	if err != nil {
		return false, fmt.Errorf("Checkpoint write-tree failed: %w", err)
	}
	if tree == "" {
		return false, errors.New("Checkpoint write-tree failed: no tree written")
	}
	listing, err := m.runGit(ctx, l.store, workdir, []string{"ls-tree", "-r", "-z", tree}, gitOpts{})
	if err != nil {
		return false, err
	}
	if nested := gitlinkPaths(listing); len(nested) > 0 {
		slog.Info("Checkpoint does not capture nested git repositories", "dir", workdir, "repos", strings.Join(nested, ", "))
		reason = singleLine(reason) + uncapturedNote(nested)
	}
	args := []string{"commit-tree", tree, "-m", reason, "--no-gpg-sign"}
	if tip != "" {
		args = []string{"commit-tree", tree, "-p", tip, "-m", reason, "--no-gpg-sign"}
	}
	sha, err := m.runGit(ctx, l.store, workdir, args, gitOpts{index: index})
	if err != nil {
		return false, fmt.Errorf("Checkpoint commit-tree failed: %w", err)
	}
	if sha == "" {
		return false, errors.New("Checkpoint commit-tree failed: no commit written")
	}
	update := []string{"update-ref", ref, sha}
	if tip != "" {
		update = append(update, tip)
	}
	if _, err := m.runGit(ctx, l.store, workdir, update, gitOpts{}); err != nil {
		return false, fmt.Errorf("Checkpoint update-ref failed: %w", err)
	}
	slog.Debug("Checkpoint taken", "dir", workdir, "commit", sha[:min(8, len(sha))])
	if prune {
		m.budget(ctx, l.store, workdir, ref)
	}
	return true, nil
}

func gitlinkPaths(listing string) []string {
	var out []string
	for _, record := range strings.Split(listing, "\x00") {
		if strings.HasPrefix(record, "160000 commit ") {
			if _, path, ok := strings.Cut(record, "\t"); ok {
				out = append(out, path)
			}
		}
	}
	return out
}

func uncapturedNote(nested []string) string {
	shown := strings.Join(nested[:min(3, len(nested))], ", ")
	more := ""
	if len(nested) > 3 {
		more = fmt.Sprintf(" (+%d)", len(nested)-3)
	}
	return " [nested git repos not captured: " + shown + more + "]"
}

func isLineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
		return true
	}
	return false
}

func singleLine(text string) string {
	var lines []string
	for _, line := range strings.FieldsFunc(text, isLineBreak) {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, " ")
}

func (m *Manager) exceedsSizeCap(path string) bool {
	limit := int64(m.maxFileSizeMB) * mib
	if limit <= 0 {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Size() > limit
}

func (m *Manager) dropOversize(ctx context.Context, store, workdir, index string) error {
	out, err := m.runGit(ctx, store, workdir, []string{"ls-files", "--cached", "-z"}, gitOpts{index: index})
	if err != nil {
		return err
	}
	var oversize []string
	for _, rel := range strings.Split(out, "\x00") {
		if rel != "" && m.exceedsSizeCap(filepath.Join(workdir, filepath.FromSlash(rel))) {
			oversize = append(oversize, rel)
		}
	}
	if len(oversize) == 0 {
		return nil
	}
	slog.Debug("Checkpoint: dropping oversize files from index", "count", len(oversize), "max_mb", m.maxFileSizeMB)
	for i := 0; i < len(oversize); i += 200 {
		args := append([]string{"rm", "--cached", "--quiet", "--"}, oversize[i:min(i+200, len(oversize))]...)
		if _, err := m.runGit(ctx, store, workdir, args, gitOpts{index: index}); err != nil && !exitedWith(err, 128) {
			return err
		}
	}
	return nil
}

func (m *Manager) RecordAgentWrite(ctx context.Context, file string) (err error) {
	if !m.enabled {
		return nil
	}
	path, err := canonical(file)
	if err != nil {
		return err
	}
	digest, err := hashFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	l, err := m.layout(ctx)
	if err != nil {
		return err
	}
	lk, err := m.lock(l)
	if err != nil {
		return err
	}
	defer release(lk, &err)
	key, err := m.ledgerKey(l, path)
	if err != nil {
		return err
	}
	ledger := loadLedger(l, key)
	ledger[path] = map[string]any{"sha256": digest, "ts": nowSeconds()}
	return saveLedger(ctx, l, key, ledger)
}

func (m *Manager) ledgerKey(l layout, path string) (string, error) {
	dir, err := workingDirFor(l, path)
	if err != nil {
		return "", err
	}
	return projectHash(dir)
}

func (m *Manager) WorkingDirFor(ctx context.Context, file string) (string, error) {
	l, err := m.layout(ctx)
	if err != nil {
		return "", err
	}
	return workingDirFor(l, file)
}

func samePath(a, b string) bool {
	rel, ok := relativeTo(a, b)
	return ok && rel == "."
}

func workingDirFor(l layout, file string) (string, error) {
	path, err := canonical(file)
	if err != nil {
		return "", err
	}
	candidate := path
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		candidate = filepath.Dir(path)
	}
	projects, err := listProjects(l)
	if err != nil {
		return "", err
	}
	best, depth := "", -1
	for _, p := range projects {
		root := p.workdir()
		if root == "" {
			continue
		}
		if _, ok := relativeTo(candidate, root); ok {
			if d := len(partsOf(root)); d > depth {
				best, depth = filepath.Clean(root), d
			}
		}
	}
	if best != "" {
		return best, nil
	}
	userHome, err := home.UserHome()
	if err != nil {
		return "", err
	}
	resolvedHome, err := canonical(userHome)
	if err != nil {
		return "", err
	}
	broad := []string{resolvedHome}
	for p := resolvedHome; filepath.Dir(p) != p; {
		p = filepath.Dir(p)
		broad = append(broad, p)
	}
	isBroad := func(p string) bool {
		for _, b := range broad {
			if samePath(p, b) {
				return true
			}
		}
		return false
	}
	for check := candidate; filepath.Dir(check) != check && !isBroad(check); check = filepath.Dir(check) {
		for _, marker := range projectMarkers {
			if exists(filepath.Join(check, marker)) {
				return check, nil
			}
		}
	}
	return candidate, nil
}
