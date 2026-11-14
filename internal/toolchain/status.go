package toolchain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Condition of one catalog entry on this machine
type State string

// Every condition a listing reports
const (
	StateInstalled   State = "installed"
	StateOutdated    State = "outdated"
	StateMissing     State = "missing"
	StateBroken      State = "broken"
	StateUnavailable State = "unavailable"
)

// One catalog entry as listings and the doctor show it
type Status struct {
	Name string
	Kind Kind
	// Version the lock pins
	Pinned string
	// Version on disk, empty when nothing is installed
	Installed string
	State     State
	// True for an entry the product cannot run without
	Required bool
	// True for an entry a default install carries
	Default bool
	// Why the entry is unavailable or broken, empty otherwise
	Reason string
	// Install directory, empty when nothing is installed
	Dir string
	// Bytes of the install directory
	Size int64
}

// One directory an uninstall of the product deletes
type Location struct {
	Path string
	Size int64
	// True for a directory every profile of the install shares
	Shared bool
}

// Pinned version of a catalog entry
func (s *Store) Version(name string) (string, error) {
	if _, err := s.recipe(name); err != nil {
		return "", err
	}
	version, _ := s.lock.version(name)
	return version, nil
}

// Catalog names in sorted order
func (s *Store) Names() []string { return append([]string(nil), s.order...) }

// Why name cannot be installed on this machine, empty when the lock pins a build for it
func (s *Store) Unavailable(ctx context.Context, name string) (string, error) {
	r, err := s.recipe(name)
	if err != nil {
		return "", err
	}
	host, err := s.hostTarget(ctx)
	if err != nil {
		return "", err
	}
	if reason := r.missing(host.target); reason != "" {
		return reason, nil
	}
	if len(s.lock.artifacts(name, host)) == 0 {
		var unavailable *UnavailableError
		if err := s.noArtifact(r, host); errors.As(err, &unavailable) {
			return unavailable.Reason, nil
		}
		return name + " is not pinned for " + string(host.target), nil
	}
	return "", nil
}

// Condition of every catalog entry, deep rehashing each install and running its smoke check
func (s *Store) Status(ctx context.Context, deep bool) ([]Status, error) {
	host, err := s.hostTarget(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]Status, 0, len(s.order))
	for _, name := range s.order {
		r := s.recipes[name]
		pinned, _ := s.lock.version(name)
		row := Status{Name: name, Kind: r.kind, Pinned: pinned, Required: r.required, Default: r.standard, State: StateMissing}
		if reason, err := s.Unavailable(ctx, name); err != nil {
			return nil, err
		} else if reason != "" {
			row.State, row.Reason = StateUnavailable, reason
			rows = append(rows, row)
			continue
		}
		if r.inHome() {
			if inst, err := s.homeInstalled(ctx, r, host); err == nil {
				row.State, row.Installed, row.Dir = StateInstalled, inst.Version, inst.Dir
				row.Size = s.homeSize(r, inst.Dir, host)
				if deep {
					if reason := s.homeBroken(r, inst.Dir, host); reason != "" {
						row.State, row.Reason = StateBroken, reason
					}
				}
			} else if !errors.Is(err, ErrNotInstalled) {
				return nil, err
			}
			rows = append(rows, row)
			continue
		}
		loc, err := s.locate(ctx, r, host, false, true)
		if err != nil {
			return nil, err
		}
		if loc == nil {
			if legacy := s.legacyReceipt(ctx, name); legacy {
				row.Reason = "legacy fact: no recorded identity, run `icebath pm install`"
			}
			rows = append(rows, row)
			continue
		}
		row.Installed, row.Dir = loc.fact.Version, filepath.Join(loc.root, loc.fact.Entry)
		row.Size = treeSize(row.Dir)
		row.State = StateInstalled
		if !loc.current {
			row.State = StateOutdated
		}
		if deep {
			switch reason := s.verifyEntry(ctx, r, row.Dir, host.target, host.target); {
			case reason != "":
				row.State, row.Reason = StateBroken, "installed but failed verification: "+reason
			case loc.fact.Digest != "" && !digestMatches(row.Dir, loc.fact.Digest):
				row.State, row.Reason = StateBroken, "realized bytes do not match recorded digest"
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// Reports a receipt written before installs recorded their target and digests
func (s *Store) legacyReceipt(ctx context.Context, name string) bool {
	roots, err := s.searchRoots(ctx)
	if err != nil {
		return false
	}
	for _, root := range roots {
		if row, ok := loadFacts(filepath.Join(root, factsName)).get(name); ok && (row.Target == "" || row.Artifacts == nil) {
			return true
		}
	}
	return false
}

// Bytes of the files a home placed entry owns below dir
func (s *Store) homeSize(r *recipe, dir string, host hostTarget) int64 {
	if !r.raw {
		return treeSize(filepath.Join(dir, strings.SplitN(r.present, "/", 2)[0]))
	}
	var total int64
	for _, a := range s.lock.artifacts(r.name, host) {
		if fi, err := os.Stat(filepath.Join(dir, a.fileName())); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// Why the pinned plain files of a raw home entry no longer hash to their pins, empty when they do
func (s *Store) homeBroken(r *recipe, dir string, host hostTarget) string {
	if !r.raw {
		return ""
	}
	for _, a := range s.lock.artifacts(r.name, host) {
		got, err := sha256File(filepath.Join(dir, a.fileName()))
		if err != nil {
			return a.fileName() + " is unreadable: " + err.Error()
		}
		if got != a.SHA256 {
			return a.fileName() + " does not match its pinned digest"
		}
	}
	return ""
}

// Required entries that are absent or behind their pin, by name, empty when the store is healthy
func (s *Store) Drift(ctx context.Context) (map[string]string, error) {
	host, err := s.hostTarget(ctx)
	if err != nil {
		return nil, err
	}
	problems := map[string]string{}
	roots, err := s.searchRoots(ctx)
	if err != nil {
		return nil, err
	}
	touched := false
	for _, root := range roots {
		if _, err := os.Stat(filepath.Join(root, factsName)); err == nil {
			touched = true
		}
	}
	if !touched {
		return problems, nil
	}
	for _, name := range s.order {
		r := s.recipes[name]
		if !r.required || r.inHome() || r.missing(host.target) != "" {
			continue
		}
		loc, err := s.locate(ctx, r, host, false, false)
		if err != nil {
			return nil, err
		}
		if loc == nil {
			problems[name] = "not installed or outdated"
		}
	}
	return problems, nil
}

// Reinstalls every recorded install of the writable store whose files or digest no longer hold, returning their names
func (s *Store) Repair(ctx context.Context, progress func(Progress)) ([]string, error) {
	host, err := s.hostTarget(ctx)
	if err != nil {
		return nil, err
	}
	_, writable, err := s.roots(ctx)
	if err != nil {
		return nil, err
	}
	f := loadFacts(filepath.Join(writable, factsName))
	var repaired []string
	var failures []error
	for _, name := range f.names() {
		r := s.recipes[name]
		row, ok := f.get(name)
		if r == nil || !ok || s.entryVerified(ctx, r, row, writable, host) {
			continue
		}
		if _, err := s.Ensure(ctx, name, EnsureOptions{Explicit: true, Progress: progress}); err != nil {
			failures = append(failures, err)
			continue
		}
		repaired = append(repaired, name)
	}
	return repaired, errors.Join(failures...)
}

// Removes the install of name and its receipt from the writable store, or its files from the profile home
func (s *Store) Remove(ctx context.Context, name string) error {
	r, err := s.recipe(name)
	if err != nil {
		return err
	}
	host, err := s.hostTarget(ctx)
	if err != nil {
		return err
	}
	if r.inHome() {
		dir, err := s.homeDirOf(ctx, r)
		if err != nil {
			return err
		}
		if !r.raw {
			return removeAll(filepath.Join(dir, strings.SplitN(r.present, "/", 2)[0]))
		}
		var failures []error
		for _, a := range s.lock.artifacts(name, host) {
			if err := os.Remove(filepath.Join(dir, a.fileName())); err != nil && !os.IsNotExist(err) {
				failures = append(failures, err)
			}
		}
		return errors.Join(failures...)
	}
	_, writable, err := s.roots(ctx)
	if err != nil {
		return err
	}
	if _, err := os.Stat(writable); err != nil {
		return nil
	}
	l, err := s.lockRoot(ctx, writable, nil)
	if err != nil {
		return err
	}
	defer l.Release()
	f := &facts{path: filepath.Join(writable, factsName)}
	if err := f.reload(); err != nil {
		return err
	}
	row, ok := f.get(name)
	if !ok {
		return nil
	}
	if err := f.forget(ctx, name); err != nil {
		return err
	}
	discardEntry(ctx, writable, row.Entry)
	return nil
}

// Sweeps the writable store of everything no receipt references and expired partial downloads, returning removed and kept counts
func (s *Store) Collect(ctx context.Context) (removed, kept int, err error) {
	_, writable, err := s.roots(ctx)
	if err != nil {
		return 0, 0, err
	}
	partials, err := s.partialsRoot(ctx)
	if err != nil {
		return 0, 0, err
	}
	if _, statErr := os.Stat(writable); statErr != nil {
		collectPartials(partials, partialGrace)
		return 0, 0, nil
	}
	l, err := s.lockRoot(ctx, writable, nil)
	if err != nil {
		return 0, 0, err
	}
	defer l.Release()
	f := &facts{path: filepath.Join(writable, factsName)}
	if err := f.reload(); err != nil {
		return 0, 0, err
	}
	var retired []string
	for _, name := range f.names() {
		if s.recipes[name] == nil {
			retired = append(retired, name)
		}
	}
	if err := f.forget(ctx, retired...); err != nil {
		return 0, 0, err
	}
	keep := f.entriesInUse()
	collectPartials(partials, partialGrace)
	removed += reclaimSetAside(ctx, writable)
	entries, err := os.ReadDir(writable)
	if err != nil {
		return removed, len(keep), err
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || keep[name] || (strings.HasPrefix(name, ".") && !strings.HasPrefix(name, stagingPrefix)) {
			continue
		}
		_ = fileutil.RemoveTree(filepath.Join(writable, name), true)
		removed++
	}
	return removed, len(keep), nil
}

// Every directory this package writes for the profile of ctx and the install it belongs to
func (s *Store) Footprint(ctx context.Context) ([]Location, error) {
	primary, writable, err := s.roots(ctx)
	if err != nil {
		return nil, err
	}
	partials, err := s.partialsRoot(ctx)
	if err != nil {
		return nil, err
	}
	var out []Location
	seen := map[string]bool{}
	add := func(path string, shared bool) {
		if seen[path] {
			return
		}
		seen[path] = true
		if _, err := os.Lstat(path); err == nil {
			out = append(out, Location{Path: path, Size: treeSize(path), Shared: shared})
		}
	}
	if !sealedStore(primary) {
		add(primary, true)
	}
	add(writable, true)
	add(partials, true)
	for _, name := range s.order {
		if r := s.recipes[name]; r.inHome() {
			if dir, err := s.homeDirOf(ctx, r); err == nil {
				add(dir, false)
			}
		}
	}
	return out, nil
}
