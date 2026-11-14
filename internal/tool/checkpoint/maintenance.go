package checkpoint

import (
	"context"
	jsonv1 "encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/proc"
)

const staleTmpPackAge = 10 * time.Minute

var tmpPackPrefixes = []string{"tmp_pack_", "tmp_idx_", "tmp_rev_", "tmp_mtimes_", ".tmp-"}

type PruneOptions struct {
	RetentionDays   int
	DeleteOrphans   bool
	MaxTotalSizeMB  int
	OrphanAllowlist map[string]bool
}

type PruneResult struct {
	Scanned       int
	DeletedOrphan int
	DeletedStale  int
	Errors        int
	BytesFreed    int64
}

type AutoPruneOptions struct {
	RetentionDays    int
	MinIntervalHours int
	DeleteOrphans    bool
	MaxTotalSizeMB   int
}

type AutoPruneResult struct {
	Skipped bool
	Result  PruneResult
}

type ProjectStatus struct {
	Hash      string
	Workdir   string
	Exists    bool
	CreatedAt *float64
	LastTouch *float64
	Commits   int
}

type PreV2Project struct {
	Path    string
	Workdir string
	Exists  bool
}

type LegacyArchive struct {
	Name      string
	SizeBytes int64
	Mtime     float64
}

type StoreStatus struct {
	Base            string
	StoreSizeBytes  int64
	LegacySizeBytes int64
	TotalSizeBytes  int64
	ProjectCount    int
	Projects        []ProjectStatus
	PreV2Projects   []PreV2Project
	LegacyArchives  []LegacyArchive
}

type ClearResult struct {
	BytesFreed int64
	Deleted    bool
}

type LegacyClearResult struct {
	BytesFreed int64
	Deleted    int
	Errors     int
}

func mtimeOf(info fs.FileInfo) float64 {
	return float64(info.ModTime().UnixNano()) / 1e9
}

func integerValue(v any) *big.Int {
	n, ok := v.(jsonv1.Number)
	if !ok || strings.ContainsAny(string(n), ".eE") {
		return nil
	}
	i, ok := new(big.Int).SetString(string(n), 10)
	if !ok {
		return nil
	}
	return i
}

func dirHasEntry(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(names) > 0, nil
}

func workdirGone(workdir string, dev, ino *big.Int, requireIdentity bool) bool {
	if workdir == "" {
		return false
	}
	if _, err := os.Stat(workdir); err == nil || (!errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR)) {
		return false
	}
	parent := filepath.Dir(workdir)
	if parent == filepath.Clean(workdir) {
		return false
	}
	if info, err := os.Stat(parent); err != nil || !info.IsDir() {
		return false
	}
	if dev != nil && ino != nil {
		resolved, err := filepath.EvalSymlinks(parent)
		if err != nil {
			return false
		}
		id, err := fileutil.LstatID(resolved)
		if err != nil {
			return false
		}
		if new(big.Int).SetUint64(id.Device).Cmp(dev) != 0 || new(big.Int).SetUint64(id.Inode).Cmp(ino) != 0 {
			return false
		}
	} else if requireIdentity {
		return false
	}
	has, err := dirHasEntry(parent)
	if err != nil {
		return false
	}
	return has || isMount(parent)
}

func (m *Manager) deleteRef(ctx context.Context, l layout, ref string) error {
	_, err := m.runGit(ctx, l.store, l.base, []string{"update-ref", "-d", ref}, gitOpts{})
	return err
}

func (m *Manager) Prune(ctx context.Context, o PruneOptions) (PruneResult, error) {
	l, err := m.layout(ctx)
	if err != nil {
		return PruneResult{}, err
	}
	return m.pruneStore(ctx, l, o)
}

func (m *Manager) pruneStore(ctx context.Context, l layout, o PruneOptions) (res PruneResult, err error) {
	if !exists(l.base) {
		return PruneResult{}, nil
	}
	lk, err := m.lock(l)
	if err != nil {
		return PruneResult{Errors: 1}, fmt.Errorf("Checkpoint maintenance stopped: %w", err)
	}
	defer release(lk, &err)
	return m.pruneLocked(ctx, l, o)
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (m *Manager) pruneLocked(ctx context.Context, l layout, o PruneOptions) (PruneResult, error) {
	var res PruneResult
	var errs []error
	fail := func(err error) (PruneResult, error) {
		res.Errors++
		return res, errors.Join(append(errs, err)...)
	}
	sizeBefore := dirSize(l.base)
	cutoff := 0.0
	if o.RetentionDays > 0 {
		cutoff = nowSeconds() - float64(o.RetentionDays)*86400
	}
	entries, err := os.ReadDir(l.base)
	if err != nil {
		return fail(fmt.Errorf("Checkpoint maintenance stopped: %w", err))
	}
	for _, e := range entries {
		child := filepath.Join(l.base, e.Name())
		info, err := os.Stat(child)
		if err != nil || !info.IsDir() || e.Name() == storeDirName || !strings.HasPrefix(e.Name(), legacyPrefix) {
			continue
		}
		if o.RetentionDays <= 0 || mtimeOf(info) >= cutoff {
			continue
		}
		size := dirSize(child)
		if err := fileutil.RemoveTree(child, true); err != nil {
			res.Errors++
			errs = append(errs, fmt.Errorf("Failed to delete legacy archive %s: %w", child, err))
			continue
		}
		res.BytesFreed += size
		res.DeletedStale++
	}
	repos, err := preV2Repos(l.base)
	if err != nil {
		return fail(fmt.Errorf("Checkpoint maintenance stopped: %w", err))
	}
	for _, repo := range repos {
		res.Scanned++
		reason := ""
		if o.DeleteOrphans && !repo.markerUnreadable &&
			(!repo.hasWorkdir || workdirGone(repo.workdir, nil, nil, false)) &&
			(o.OrphanAllowlist == nil || o.OrphanAllowlist[repo.path]) {
			reason = "orphan"
		}
		if reason == "" && o.RetentionDays > 0 {
			if newest := newestMtime(repo.path); newest > 0 && newest < cutoff {
				reason = "stale"
			}
		}
		if reason == "" {
			continue
		}
		size := dirSize(repo.path)
		if err := fileutil.RemoveTree(repo.path, true); err != nil {
			res.Errors++
			errs = append(errs, fmt.Errorf("Failed to prune checkpoint repo %s: %w", filepath.Base(repo.path), err))
			continue
		}
		res.BytesFreed += size
		if reason == "orphan" {
			res.DeletedOrphan++
		} else {
			res.DeletedStale++
		}
	}
	if l.hasHead() {
		if err := sweepTmpPacks(l.store); err != nil {
			errs = append(errs, err)
		}
		projects, err := listProjects(l)
		if err != nil {
			return fail(fmt.Errorf("Checkpoint maintenance stopped: %w", err))
		}
		for _, p := range projects {
			res.Scanned++
			workdir := p.workdir()
			reason := ""
			if o.DeleteOrphans &&
				(workdir == "" || workdirGone(workdir, integerValue(p.meta["workdir_parent_dev"]), integerValue(p.meta["workdir_parent_ino"]), true)) &&
				(o.OrphanAllowlist == nil || o.OrphanAllowlist[p.hash]) {
				reason = "orphan"
			} else if o.RetentionDays > 0 {
				lastTouch, err := numberValue(p.meta["last_touch"])
				if err != nil {
					return fail(fmt.Errorf("Checkpoint maintenance stopped: project %s last_touch: %w", p.hash, err))
				}
				if lastTouch > 0 && lastTouch < cutoff {
					reason = "stale"
				}
			}
			if reason == "" {
				continue
			}
			if err := m.deleteRef(ctx, l, refName(p.hash)); err != nil {
				return fail(fmt.Errorf("could not delete %s: %w", refName(p.hash), err))
			}
			for _, path := range []string{l.index(p.hash), l.meta(p.hash)} {
				if err := removeIfPresent(path); err != nil {
					res.Errors++
					errs = append(errs, err)
				}
			}
			if reason == "orphan" {
				res.DeletedOrphan++
			} else {
				res.DeletedStale++
			}
		}
		pr := pruner{m: m, store: l.store, cwd: l.base}
		if err := m.reclaimAndCap(ctx, pr, res.DeletedOrphan+res.DeletedStale > 0, int64(o.MaxTotalSizeMB)*mib); err != nil {
			res.Errors++
			errs = append(errs, err)
		}
	}
	res.BytesFreed = max(res.BytesFreed, sizeBefore-dirSize(l.base))
	return res, errors.Join(errs...)
}

func (m *Manager) reclaimAndCap(ctx context.Context, pr pruner, deleted bool, capBytes int64) error {
	if deleted || pr.gcPending() {
		if err := pr.reclaim(ctx); err != nil {
			return fmt.Errorf("Checkpoint pruning stopped: %w", err)
		}
	}
	fits, err := pr.enforceSize(ctx, capBytes)
	if err != nil {
		return fmt.Errorf("Checkpoint pruning stopped: %w", err)
	}
	if !fits {
		return errors.New("Checkpoint store remains over its size cap; minimum history retained")
	}
	return nil
}

func isGitProgram(name string) bool {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".exe")
	return name == "git" || strings.HasPrefix(name, "git-")
}

func gitProcessRunning() bool {
	table, err := proc.ReadTable()
	if err != nil {
		slog.Debug("git process probe failed; assuming git may be running", "error", err)
		return true
	}
	for _, e := range table {
		name, err := proc.Name(e.PID)
		if errors.Is(err, proc.ErrNotFound) {
			continue
		}
		if err != nil {
			slog.Debug("git process scan failed; assuming git may be running", "error", err)
			return true
		}
		if isGitProgram(name) {
			return true
		}
	}
	return false
}

func sweepTmpPacks(repo string) error {
	root := repo
	if info, err := os.Stat(filepath.Join(repo, ".git")); err == nil && info.IsDir() {
		root = filepath.Join(repo, ".git")
	}
	packDir := filepath.Join(root, "objects", "pack")
	if info, err := os.Stat(packDir); err != nil || !info.IsDir() {
		return nil
	}
	if gitProcessRunning() {
		slog.Debug("git process running; skipping tmp-pack sweep")
		return nil
	}
	var candidates []string
	for _, group := range []struct {
		dir      string
		prefixes []string
	}{
		{packDir, tmpPackPrefixes},
		{filepath.Join(root, "objects", "info", "commit-graphs"), []string{"tmp_graph_"}},
	} {
		entries, err := os.ReadDir(group.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			for _, prefix := range group.prefixes {
				if strings.HasPrefix(e.Name(), prefix) {
					candidates = append(candidates, filepath.Join(group.dir, e.Name()))
					break
				}
			}
		}
	}
	cutoff := time.Now().Add(-staleTmpPackAge)
	var errs []error
	for _, path := range candidates {
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("Could not clear %s: %w", path, err))
			continue
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		makeRemovable(path)
		if err := os.Remove(path); err != nil {
			errs = append(errs, fmt.Errorf("Could not clear %s: %w", path, err))
			continue
		}
		slog.Info("Removed aborted-write git debris", "path", path, "bytes", info.Size())
	}
	return errors.Join(errs...)
}

func (m *Manager) MaybeAutoPrune(ctx context.Context, o AutoPruneOptions) (AutoPruneResult, error) {
	l, err := m.layout(ctx)
	if err != nil {
		return AutoPruneResult{}, err
	}
	if !exists(l.base) {
		return AutoPruneResult{}, nil
	}
	now := nowSeconds()
	if text, err := fileutil.ReadText(l.marker); err == nil {
		if last, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil && now-last < float64(o.MinIntervalHours)*3600 {
			return AutoPruneResult{Skipped: true}, nil
		}
	}
	if err := fileutil.WriteText(ctx, l.marker, strconv.FormatFloat(now, 'f', -1, 64)); err != nil {
		slog.Debug("Could not write checkpoint prune marker", "error", err)
	}
	res, err := m.pruneStore(ctx, l, PruneOptions{RetentionDays: o.RetentionDays, DeleteOrphans: o.DeleteOrphans, MaxTotalSizeMB: o.MaxTotalSizeMB})
	if total := res.DeletedOrphan + res.DeletedStale; total > 0 {
		slog.Info(fmt.Sprintf("checkpoint auto-maintenance: pruned %d entry(ies) (%d orphan, %d stale), reclaimed %.1f MB",
			total, res.DeletedOrphan, res.DeletedStale, float64(res.BytesFreed)/mib))
	}
	return AutoPruneResult{Result: res}, err
}

func (m *Manager) AutoPrune(ctx context.Context, cfg config.CheckpointsConfig) (AutoPruneResult, error) {
	if !cfg.AutoPrune {
		return AutoPruneResult{Skipped: true}, nil
	}
	return m.MaybeAutoPrune(ctx, AutoPruneOptions{
		RetentionDays:    cfg.RetentionDays,
		MinIntervalHours: cfg.MinIntervalHours,
		DeleteOrphans:    false,
		MaxTotalSizeMB:   cfg.MaxTotalSizeMB,
	})
}

func formatBytes(n int64) string {
	size := float64(n)
	if size < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	for _, unit := range []string{"KB", "MB", "GB"} {
		size /= 1024
		if size < 1024 {
			return fmt.Sprintf("%.1f %s", size, unit)
		}
	}
	return fmt.Sprintf("%.1f TB", size/1024)
}

func (m *Manager) FootprintNotice(ctx context.Context, cfg config.CheckpointsConfig) (string, error) {
	if !cfg.Enabled || cfg.MaxTotalSizeMB <= 0 {
		return "", nil
	}
	l, err := m.layout(ctx)
	if err != nil {
		return "", err
	}
	size := dirSize(l.base)
	if size < int64(cfg.MaxTotalSizeMB)*mib {
		return "", nil
	}
	count := 0
	if l.hasHead() {
		projects, err := listProjects(l)
		if err != nil {
			return "", err
		}
		count = len(projects)
	}
	return fmt.Sprintf("Filesystem checkpoints (/rollback) are on: %s across %d project(s), above the %d MB cap (one snapshot per project is "+
		"always kept). Not using /rollback? `icebath config set checkpoints.enabled false` then "+
		"`icebath checkpoints clear`; or lower `checkpoints.retention_days`.", formatBytes(size), count, cfg.MaxTotalSizeMB), nil
}

func (m *Manager) Status(ctx context.Context) (StoreStatus, error) {
	l, err := m.layout(ctx)
	if err != nil {
		return StoreStatus{}, err
	}
	out := StoreStatus{Base: l.base, Projects: []ProjectStatus{}, PreV2Projects: []PreV2Project{}, LegacyArchives: []LegacyArchive{}}
	if !exists(l.base) {
		return out, nil
	}
	if exists(l.store) {
		out.StoreSizeBytes = dirSize(l.store)
		if l.hasHead() {
			projects, err := listProjects(l)
			if err != nil {
				return out, err
			}
			for _, p := range projects {
				count, err := m.runGit(ctx, l.store, l.base, []string{"rev-list", "--count", refName(p.hash)}, gitOpts{})
				if err != nil && !exitedWith(err, 128) {
					return out, err
				}
				commits := 0
				if err == nil {
					if n, perr := strconv.Atoi(count); perr == nil {
						commits = n
					}
				}
				workdir := p.workdir()
				out.Projects = append(out.Projects, ProjectStatus{
					Hash:      p.hash,
					Workdir:   workdir,
					Exists:    workdir != "" && exists(workdir),
					CreatedAt: optionalNumber(p.meta["created_at"]),
					LastTouch: optionalNumber(p.meta["last_touch"]),
					Commits:   commits,
				})
			}
		}
	}
	out.ProjectCount = len(out.Projects)
	repos, err := preV2Repos(l.base)
	if err != nil {
		return out, err
	}
	for _, r := range repos {
		out.PreV2Projects = append(out.PreV2Projects, PreV2Project{Path: r.path, Workdir: r.workdir, Exists: r.exists})
	}
	entries, err := os.ReadDir(l.base)
	if err != nil {
		return out, err
	}
	for _, e := range entries {
		child := filepath.Join(l.base, e.Name())
		info, err := os.Stat(child)
		if err != nil || !info.IsDir() || !strings.HasPrefix(e.Name(), legacyPrefix) {
			continue
		}
		size := dirSize(child)
		out.LegacySizeBytes += size
		out.LegacyArchives = append(out.LegacyArchives, LegacyArchive{Name: e.Name(), SizeBytes: size, Mtime: mtimeOf(info)})
	}
	out.TotalSizeBytes = dirSize(l.base)
	return out, nil
}

func (m *Manager) ClearAll(ctx context.Context) (res ClearResult, err error) {
	l, err := m.layout(ctx)
	if err != nil {
		return ClearResult{}, err
	}
	if !exists(l.base) {
		return ClearResult{}, nil
	}
	size := dirSize(l.base)
	lk, err := m.lock(l)
	if err != nil {
		return ClearResult{}, fmt.Errorf("Could not clear checkpoint base %s: %w", l.base, err)
	}
	defer release(lk, &err)
	if err := fileutil.RemoveTree(l.base, true); err != nil {
		return ClearResult{}, fmt.Errorf("Could not clear checkpoint base %s: %w", l.base, err)
	}
	return ClearResult{BytesFreed: size, Deleted: true}, nil
}

func (m *Manager) ClearLegacy(ctx context.Context) (res LegacyClearResult, err error) {
	l, err := m.layout(ctx)
	if err != nil {
		return LegacyClearResult{}, err
	}
	if !exists(l.base) {
		return LegacyClearResult{}, nil
	}
	lk, err := m.lock(l)
	if err != nil {
		return LegacyClearResult{Errors: 1}, fmt.Errorf("Could not clear legacy archives: %w", err)
	}
	defer release(lk, &err)
	entries, err := os.ReadDir(l.base)
	if err != nil {
		return LegacyClearResult{Errors: 1}, fmt.Errorf("Could not clear legacy archives: %w", err)
	}
	var errs []error
	for _, e := range entries {
		child := filepath.Join(l.base, e.Name())
		info, err := os.Stat(child)
		if err != nil || !info.IsDir() || !strings.HasPrefix(e.Name(), legacyPrefix) {
			continue
		}
		size := dirSize(child)
		if err := fileutil.RemoveTree(child, true); err != nil {
			res.Errors++
			errs = append(errs, fmt.Errorf("Could not delete legacy archive %s: %w", child, err))
			continue
		}
		res.BytesFreed += size
		res.Deleted++
	}
	return res, errors.Join(errs...)
}
