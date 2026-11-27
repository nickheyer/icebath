package checkpoints

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	checkpointsconfig "github.com/nickheyer/icebath/internal/checkpoints/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/git"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Git step of a rewrite of the store's histories that must succeed, its failure stopping the prune
func (s *Store) must(st step, args ...string) (string, error) {
	ok, out, errs := s.git(s.base, st, args...)
	if !ok {
		return "", fmt.Errorf("checkpoint %s failed: %s", args[0], errs)
	}
	return out, nil
}

// Expires every reflog and collects the store's garbage, then puts back the folders a bare repository keeps and clears the mark of a
// collection left for later
func (s *Store) reclaim() error {
	if _, err := s.must(step{}, "reflog", "expire", "--expire=now", "--all"); err != nil {
		return err
	}
	if _, err := s.must(step{timeout: s.timeout * 3}, "gc", "--prune=now", "--quiet"); err != nil {
		return err
	}
	for _, dir := range []string{filepath.Join("refs", "heads"), "branches"} {
		if err := os.MkdirAll(filepath.Join(s.store(), dir), 0o700); err != nil {
			return err
		}
	}
	os.Remove(filepath.Join(s.store(), gcPending))
	return nil
}

// Whether a rewrite left its garbage collection for the next cleanup
func (s *Store) pending() bool { return fsx.Exists(filepath.Join(s.store(), gcPending)) }

// Leaves the mark of a garbage collection left for the next cleanup
func (s *Store) markPending() {
	path := filepath.Join(s.store(), gcPending)
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
		now := time.Now()
		os.Chtimes(path, now, now)
	}
}

// Snapshots of the ref, oldest first
func (s *Store) commits(name string) ([]string, error) {
	out, err := s.must(step{}, "rev-list", "--reverse", name)
	if err != nil {
		return nil, err
	}
	commits := git.Lines(out)
	if len(commits) == 0 {
		return nil, errors.New("checkpoint ref has no readable history: " + name)
	}
	return commits, nil
}

// Builds the kept snapshots again on one another with their trees, dates and reasons, then moves the ref only if it did not move
// meanwhile; the garbage collected now, or left for the next cleanup
func (s *Store) rewrite(name string, original, keep []string, collect bool) error {
	parent := ""
	for _, sha := range keep {
		metadata, err := s.must(step{}, "log", "-1", "--format=%T%x00%aI%x00%cI%x00%s", sha)
		if err != nil {
			return err
		}
		fields := strings.SplitN(metadata, "\x00", 4)
		if len(fields) != 4 {
			return errors.New("invalid checkpoint metadata: " + sha)
		}
		args := []string{"commit-tree", fields[0], "--no-gpg-sign", "-m", fields[3]}
		if parent != "" {
			args = append(args, "-p", parent)
		}
		if parent, err = s.must(step{env: map[string]string{"GIT_AUTHOR_DATE": fields[1], "GIT_COMMITTER_DATE": fields[2]}}, args...); err != nil {
			return err
		}
		if parent == "" {
			return errors.New("commit-tree returned no checkpoint identity")
		}
	}
	if parent == "" {
		return errors.New("cannot remove a project's last snapshot")
	}
	if _, err := s.must(step{}, "update-ref", name, parent, original[len(original)-1]); err != nil {
		return err
	}
	if collect {
		return s.reclaim()
	}
	s.markPending()
	return nil
}

// Keeps the newest snapshots of the ref within the count, leaving the garbage collection for later
func (s *Store) keepNewest(name string, count int) error {
	commits, err := s.commits(name)
	if err != nil {
		return err
	}
	if count = max(1, count); len(commits) > count {
		return s.rewrite(name, commits, commits[len(commits)-count:], false)
	}
	return nil
}

// Every project's ref
func (s *Store) refs() ([]string, error) {
	out, err := s.must(step{}, "for-each-ref", "--format=%(refname)", refPrefix)
	if err != nil {
		return nil, err
	}
	return git.Lines(out), nil
}

// Drops the oldest snapshot of every project once while the store is over the size, leaving the garbage collection for later
func (s *Store) dropRound(limit int64) (bool, error) {
	if limit <= 0 || size(s.store()) <= limit {
		return false, nil
	}
	names, err := s.refs()
	if err != nil {
		return false, err
	}
	changed := false
	for _, name := range names {
		commits, err := s.commits(name)
		if err != nil {
			return changed, err
		}
		if len(commits) > 1 {
			if err := s.rewrite(name, commits, commits[1:], false); err != nil {
				return changed, err
			}
			changed = true
		}
	}
	return changed, nil
}

// Collects the garbage, then drops the oldest snapshot of each project in turn, measuring again after each, for at most twenty rounds:
// whether the store fits the size
func (s *Store) fit(limit int64) (bool, error) {
	if limit <= 0 || size(s.store()) <= limit {
		return true, nil
	}
	if err := s.reclaim(); err != nil {
		return false, err
	}
	if size(s.store()) <= limit {
		return true, nil
	}
	names, err := s.refs()
	if err != nil {
		return false, err
	}
	for range 20 {
		changed := false
		for _, name := range names {
			commits, err := s.commits(name)
			if err != nil {
				return false, err
			}
			if len(commits) <= 1 {
				continue
			}
			if err := s.rewrite(name, commits, commits[1:], true); err != nil {
				return false, err
			}
			changed = true
			if size(s.store()) <= limit {
				return true, nil
			}
		}
		if !changed {
			break
		}
	}
	return false, nil
}

// Keeps the project's snapshots within the count, and drops one round of the oldest when the store is over its size, after a snapshot
func (s *Store) trim(wd, name string) {
	if err := s.keepNewest(name, s.snapshots); err != nil {
		slog.Warn("Checkpoint pruning stopped", "error", err)
		return
	}
	dropped, err := s.dropRound(int64(s.totalMB) * 1024 * 1024)
	if err != nil {
		slog.Warn("Checkpoint pruning stopped", "error", err)
		return
	}
	if dropped {
		slog.Info(fmt.Sprintf("Checkpoint store exceeded %d MB — dropped the oldest snapshot per project; space is reclaimed by the next prune",
			s.totalMB))
	}
}

// What a cleanup took
type PruneOptions struct {
	// Projects not snapshotted for this long go, none for zero
	Retention time.Duration
	// Projects whose folder is gone go
	Orphans bool
	// Megabytes the store is brought within, none for zero
	MaxTotalMB int
	// Projects, by hash, whose folder being gone lets them go; nil lets every one
	Allow map[string]bool
}

// What a cleanup scanned, deleted and freed
type Pruned struct {
	Scanned int
	Orphans int
	Stale   int
	Errors  int
	Freed   int64
}

// Deletes the projects whose folder is gone and those not snapshotted for the retention, old archives, then collects the store's
// garbage and brings it within its size, under the store lock
func (s *Store) Prune(o PruneOptions) (Pruned, error) {
	var pruned Pruned
	if !fsx.Exists(s.base) {
		return pruned, nil
	}
	release, err := lock(s.base)
	if err != nil {
		pruned.Errors++
		slog.Warn("Checkpoint maintenance stopped", "error", err)
		return pruned, nil
	}
	defer release()
	if err := s.prune(o, &pruned); err != nil {
		pruned.Errors++
		slog.Warn("Checkpoint maintenance stopped", "error", err)
	}
	return pruned, nil
}

func (s *Store) prune(o PruneOptions, pruned *Pruned) error {
	before := size(s.base)
	cutoff := time.Now().Add(-o.Retention)
	entries, err := os.ReadDir(s.base)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(s.base, entry.Name())
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || !strings.HasPrefix(entry.Name(), legacyPrefix) || o.Retention <= 0 || !info.ModTime().Before(cutoff) {
			continue
		}
		freed := size(path)
		if err := removeTree(path); err != nil {
			pruned.Errors++
			slog.Warn("Failed to delete legacy archive "+path, "error", err)
			continue
		}
		pruned.Freed += freed
		pruned.Stale++
	}
	if !s.made() {
		pruned.Freed = max(pruned.Freed, before-size(s.base))
		return nil
	}
	git.SweepTemp(s.store(), git.StaleAge)
	deleted := false
	for _, meta := range s.projects() {
		pruned.Scanned++
		orphan := o.Orphans && (meta.Workdir == "" || gone(meta.Workdir, meta.ParentDev, meta.ParentIno))
		reason := ""
		switch {
		case orphan && (o.Allow == nil || o.Allow[meta.Hash]):
			reason = "orphan"
		case o.Retention > 0 && meta.Touched > 0 && meta.Touched < float64(cutoff.UnixNano())/1e9:
			reason = "stale"
		default:
			continue
		}
		if ok, _, _ := s.git(s.base, step{allowed: []int{128}}, "update-ref", "-d", ref(meta.Hash)); !ok {
			pruned.Errors++
			return nil
		}
		os.Remove(s.index(meta.Hash))
		os.Remove(s.meta(meta.Hash))
		os.Remove(s.ledgerPath(meta.Hash))
		deleted = true
		if reason == "orphan" {
			pruned.Orphans++
		} else {
			pruned.Stale++
		}
	}
	if err := s.settle(deleted, int64(o.MaxTotalMB)*1024*1024, pruned); err != nil {
		pruned.Errors++
		slog.Warn("Checkpoint pruning stopped", "error", err)
	}
	pruned.Freed = max(pruned.Freed, before-size(s.base))
	return nil
}

// Collects the garbage after deletions or a rewrite that left it for later, then brings the store within the size, counting a size it
// can't reach as an error
func (s *Store) settle(deleted bool, limit int64, pruned *Pruned) error {
	if deleted || s.pending() {
		if err := s.reclaim(); err != nil {
			return err
		}
	}
	fits, err := s.fit(limit)
	if err != nil {
		return err
	}
	if !fits {
		pruned.Errors++
		slog.Warn("Checkpoint store remains over its size cap; minimum history retained")
	}
	return nil
}

// Whether the folder was deleted beyond doubt: missing from a parent folder that is there, the same one it was when recorded when that
// is known, and holds something or is a mount point, so a volume not mounted now never counts as the folder gone
func gone(workdir string, dev, ino uint64) bool {
	path := filepath.Clean(workdir)
	if _, err := os.Stat(path); err == nil || !missing(err) {
		return false
	}
	parent := filepath.Dir(path)
	if parent == path || !fsx.IsDir(parent) {
		return false
	}
	if dev == 0 || ino == 0 {
		return false
	}
	if d, i, err := identity(parent); err != nil || d != dev || i != ino {
		return false
	}
	f, err := os.Open(parent)
	if err != nil {
		return false
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err == nil {
		return true
	} else if !errors.Is(err, io.EOF) {
		return false
	}
	return mount(parent)
}

// Prunes at most once in the interval the settings set, claiming it in the marker before it runs, never deleting a project whose folder
// is gone: whether it ran, and what it took
func AutoPrune(cfg *config.Config) (bool, Pruned, error) {
	if !cfg.GetBool(checkpointsconfig.AutoPrune) {
		return false, Pruned{}, nil
	}
	s := Open(cfg)
	if !fsx.Exists(s.base) {
		return true, Pruned{}, nil
	}
	marker := filepath.Join(s.base, pruneMarker)
	interval := time.Duration(cfg.GetInt(checkpointsconfig.MinInterval)) * time.Hour
	if raw, err := os.ReadFile(marker); err == nil {
		if last, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(string(raw), "\ufeff")), 64); err == nil &&
			now()-last < interval.Seconds() {
			return false, Pruned{}, nil
		}
	}
	if err := os.WriteFile(marker, []byte(strconv.FormatFloat(now(), 'f', -1, 64)), 0o600); err != nil {
		slog.Debug("Could not write checkpoint prune marker", "error", err)
	}
	pruned, err := s.Prune(PruneOptions{Retention: time.Duration(cfg.GetInt(checkpointsconfig.Retention)) * 24 * time.Hour,
		MaxTotalMB: cfg.GetInt(checkpointsconfig.MaxTotalSize)})
	if err != nil {
		slog.Warn("checkpoint auto-maintenance failed", "error", err)
		return true, pruned, err
	}
	if total := pruned.Orphans + pruned.Stale; total > 0 {
		slog.Info(fmt.Sprintf("checkpoint auto-maintenance: pruned %d entry(ies) (%d orphan, %d stale), reclaimed %.1f MB", total,
			pruned.Orphans, pruned.Stale, float64(pruned.Freed)/(1024*1024)))
	}
	return true, pruned, nil
}

// Byte count as B, KB, MB, GB or TB, one decimal above bytes
func Size(n int64) string {
	value := float64(n)
	if value < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	for _, unit := range []string{"KB", "MB", "GB"} {
		if value /= 1024; value < 1024 {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return fmt.Sprintf("%.1f TB", value/1024)
}

// Note naming the way out when snapshots are on and the store is at or above its size, empty otherwise
func Footprint(cfg *config.Config) string {
	if !cfg.GetBool(checkpointsconfig.Enabled) {
		return ""
	}
	limit := cfg.GetInt(checkpointsconfig.MaxTotalSize)
	if limit <= 0 {
		return ""
	}
	s := Open(cfg)
	taken := size(s.base)
	if taken < int64(limit)*1024*1024 {
		return ""
	}
	return fmt.Sprintf("Filesystem checkpoints (/rollback) are on: %s across %d project(s), above the %d MB cap (one snapshot per project is "+
		"always kept). Not using /rollback? Turn off File checkpoints in /settings under Tools then run `icebath checkpoints clear`; or "+
		"lower Snapshot retention.", Size(taken), len(s.projects()), limit)
}

// One project of the store as its status shows it
type ProjectStatus struct {
	Hash    string
	Workdir string
	// Its folder is there now
	Exists  bool
	Created time.Time
	Touched time.Time
	Commits int
}

// One archive of what the folder held before the store was made
type Archive struct {
	Name     string
	Bytes    int64
	Modified time.Time
}

// What the profile's checkpoints take: the store, its projects and the archives
type Status struct {
	Base string
	// The checkpoint folder is there
	Exists      bool
	StoreBytes  int64
	LegacyBytes int64
	TotalBytes  int64
	Projects    []ProjectStatus
	Archives    []Archive
}

func stamp(seconds float64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(seconds*1e9))
}

// Sizes of the store and the archives, and every project with its count of snapshots
func (s *Store) Status() Status {
	status := Status{Base: s.base, Exists: fsx.Exists(s.base)}
	if !status.Exists {
		return status
	}
	if fsx.Exists(s.store()) {
		status.StoreBytes = size(s.store())
		if s.made() {
			for _, meta := range s.projects() {
				commits := 0
				if ok, out, _ := s.git(s.base, step{allowed: []int{128}}, "rev-list", "--count", ref(meta.Hash)); ok {
					commits, _ = strconv.Atoi(out)
				}
				status.Projects = append(status.Projects, ProjectStatus{Hash: meta.Hash, Workdir: meta.Workdir,
					Exists: meta.Workdir != "" && fsx.Exists(meta.Workdir), Created: stamp(meta.Created), Touched: stamp(meta.Touched), Commits: commits})
			}
		}
	}
	entries, _ := os.ReadDir(s.base)
	for _, entry := range entries {
		path := filepath.Join(s.base, entry.Name())
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || !strings.HasPrefix(entry.Name(), legacyPrefix) {
			continue
		}
		bytes := size(path)
		status.LegacyBytes += bytes
		status.Archives = append(status.Archives, Archive{Name: entry.Name(), Bytes: bytes, Modified: info.ModTime()})
	}
	status.TotalBytes = size(s.base)
	return status
}

// Deletes the whole checkpoint folder, store and archives, under the store lock: the bytes it freed
func (s *Store) Clear() (int64, error) {
	if !fsx.Exists(s.base) {
		return 0, nil
	}
	freed := size(s.base)
	release, err := lock(s.base)
	if err != nil {
		return 0, err
	}
	defer release()
	if err := removeTree(s.base); err != nil {
		slog.Warn("Could not clear checkpoint base "+s.base, "error", err)
		return 0, err
	}
	return freed, nil
}

// What clearing the archives did
type Cleared struct {
	Deleted int
	Freed   int64
	Errors  int
}

// Deletes every archive of what the folder held before the store was made, under the store lock
func (s *Store) ClearLegacy() Cleared {
	var cleared Cleared
	if !fsx.Exists(s.base) {
		return cleared
	}
	release, err := lock(s.base)
	if err != nil {
		cleared.Errors++
		slog.Warn("Could not clear legacy archives", "error", err)
		return cleared
	}
	defer release()
	entries, err := os.ReadDir(s.base)
	if err != nil {
		cleared.Errors++
		slog.Warn("Could not clear legacy archives", "error", err)
		return cleared
	}
	for _, entry := range entries {
		path := filepath.Join(s.base, entry.Name())
		if !strings.HasPrefix(entry.Name(), legacyPrefix) || !fsx.IsDir(path) {
			continue
		}
		freed := size(path)
		if err := removeTree(path); err != nil {
			cleared.Errors++
			slog.Warn("Could not delete legacy archive "+path, "error", err)
			continue
		}
		cleared.Freed += freed
		cleared.Deleted++
	}
	return cleared
}
