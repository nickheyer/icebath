package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate"
	"github.com/nickheyer/icebath/pkg/tools"
)

// User state one data only removal deletes and the runtime paths it keeps for one home
type dataRemovalPlan struct {
	Home   string
	Remove []string
	Keep   []string
}

// ValueError a data plan raises for a target it must never erase
type dataPlanError struct{ msg string }

func (e *dataPlanError) Error() string { return e.msg }

// Removal of a whole tree, swapped by the failed removal case like shutil.rmtree
var dataRmtree = pyshutil.Rmtree

// Whether two paths are equal the way pathlib compares them on this host
func pathEq(a, b string) bool {
	rel, ok := pypath.PathRelativeTo(a, b)
	return ok && rel == "."
}

// Whether p equals root or lies under it the way PurePath.is_relative_to answers
func pathUnder(p, root string) bool {
	_, ok := pypath.PathRelativeTo(p, root)
	return ok
}

// Orders paths by their normalized parts the way pathlib sorts them
func pathCompare(a, b string) int {
	key := func(p string) []string {
		anchor, parts := pypath.PathParts(p)
		out := append([]string{anchor}, parts...)
		if runtime.GOOS == "windows" {
			for i := range out {
				out[i] = strings.ToLower(strings.ReplaceAll(out[i], "/", `\`))
			}
		}
		return out
	}
	return slices.Compare(key(a), key(b))
}

// Lists what a data wipe of the home removes and keeps, never the code, runtime stores or sibling profiles
func planDataRemoval(ctx context.Context, h, project string, userdata *string) (*dataRemovalPlan, error) {
	h, err := pypath.Resolve(h)
	if err != nil {
		return nil, err
	}
	anchor, _ := pypath.PathParts(h)
	userHome, err := pypath.PathHome()
	if err != nil {
		return nil, err
	}
	userHomeResolved, err := pypath.Resolve(userHome)
	if err != nil {
		return nil, err
	}
	if pathEq(h, anchor) || pathEq(h, userHomeResolved) {
		return nil, &dataPlanError{"refusing to erase an entire filesystem or user home: " + h}
	}
	machine, err := pypath.Resolve(home.RootOf(h))
	if err != nil {
		return nil, err
	}
	projectResolved, err := pypath.Resolve(project)
	if err != nil {
		return nil, err
	}
	venv, err := pm.BaseVenv(project)
	if err != nil {
		return nil, err
	}
	store, err := pm.EnvironmentStoreRoot(ctx, project, true)
	if err != nil {
		return nil, err
	}
	protected := []string{projectResolved}
	for _, p := range []string{venv, store, pm.InstallsRoot(ctx)} {
		resolved, err := pypath.Resolve(p)
		if err != nil {
			return nil, err
		}
		protected = append(protected, resolved)
	}
	protected = append(protected, filepath.Join(machine, "tools"), filepath.Join(machine, "bin"), filepath.Join(machine, "cache"),
		filepath.Join(machine, tools.LedgerFilename), filepath.Join(machine, tools.LedgerFilename+".lock"),
		filepath.Join(h, "gateway.lock"), filepath.Join(h, ".backup.lock"), filepath.Join(h, "runtime", "active_sessions.lock"),
		filepath.Join(h, "cron", ".tick.lock"), tools.CheckpointStoreLockPath(filepath.Join(h, "checkpoints")))
	if pathEq(h, machine) {
		protected = append(protected, filepath.Join(machine, "profiles"))
	}
	if selfupdate.IsBundledPayload(project) {
		layout, err := resolveBundleLayout(project, "")
		if err != nil {
			var notBundled *notBundledApp
			if errors.As(err, &notBundled) {
				return nil, &dataPlanError{notBundled.Error()}
			}
			return nil, err
		}
		protected = append(protected, layout.AppRoot)
	}
	canonical := filepath.Join(h, "icebath-agent")
	if st, err := os.Stat(filepath.Join(canonical, "hermes_cli", "__init__.py")); err == nil && st.Mode().IsRegular() {
		protected = append(protected, canonical)
	}
	var remove, keep []string
	keepAdd := func(p string) {
		for _, k := range keep {
			if pathEq(k, p) {
				return
			}
		}
		keep = append(keep, p)
	}
	var visit func(p string) error
	visit = func(p string) error {
		st, err := os.Lstat(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		resolved, err := pypath.Resolve(p)
		if err != nil {
			return err
		}
		for _, root := range protected {
			if pathEq(p, root) || pathEq(resolved, root) || pathUnder(p, root) {
				keepAdd(p)
				return nil
			}
		}
		if st.Mode()&fs.ModeSymlink != 0 || pm.IsJunction(p) {
			remove = append(remove, p)
			return nil
		}
		for _, root := range protected {
			if pathUnder(root, p) {
				if !st.IsDir() {
					keepAdd(p)
					return nil
				}
				children, err := sortedChildren(p)
				if err != nil {
					return err
				}
				for _, child := range children {
					if err := visit(child); err != nil {
						return err
					}
				}
				return nil
			}
		}
		remove = append(remove, p)
		return nil
	}
	if st, err := os.Stat(h); err == nil && st.IsDir() {
		children, err := sortedChildren(h)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			if err := visit(child); err != nil {
				return nil, err
			}
		}
	}
	if !pathEq(h, machine) {
		userdata = nil
	}
	if userdata != nil {
		resolvedUserdata, err := pypath.Resolve(*userdata)
		if err != nil {
			return nil, err
		}
		if !pathUnder(resolvedUserdata, h) {
			if pathEq(resolvedUserdata, userHomeResolved) || pathUnder(h, resolvedUserdata) {
				return nil, &dataPlanError{"desktop data directory must not contain the Icebath or user home: " + *userdata}
			}
			parent, err := pypath.Resolve(pypath.PathParent(*userdata))
			if err != nil {
				return nil, err
			}
			if err := visit(filepath.Join(parent, pypath.PathName(*userdata))); err != nil {
				return nil, err
			}
		}
	}
	slices.SortFunc(keep, pathCompare)
	return &dataRemovalPlan{Home: h, Remove: remove, Keep: keep}, nil
}

// Entries of a directory sorted the way sorted(Path.iterdir()) orders them
func sortedChildren(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, filepath.Join(dir, e.Name()))
	}
	slices.SortFunc(out, pathCompare)
	return out, nil
}

// One planned path a data removal could not delete and the reason
type dataRemovalFailure struct {
	Path   string
	Reason string
}

// str of an exception a data removal step raised, OSError text for filesystem failures
func dataErrText(err error) string {
	var planErr *dataPlanError
	if errors.As(err, &planErr) {
		return planErr.msg
	}
	if pypath.IsOSError(err) {
		return pypath.OSErrorText(err)
	}
	return pyrepr.ExceptionMessage(err)
}

// Deletes each planned path without following a link swapped in after confirmation, collecting survivors
func removeData(plan *dataRemovalPlan) ([]string, []dataRemovalFailure) {
	var removed []string
	var failed []dataRemovalFailure
	for _, p := range plan.Remove {
		err := func() error {
			parent := pypath.PathParent(p)
			resolvedParent, err := pypath.Resolve(parent)
			if err != nil {
				return err
			}
			if !pathEq(resolvedParent, parent) {
				return &dataPlanError{"parent directory changed after confirmation; refusing to follow a link"}
			}
			st, err := os.Lstat(p)
			if err != nil {
				return err
			}
			switch {
			case st.Mode()&fs.ModeSymlink != 0 || !st.IsDir():
				return pypath.Unlink(p)
			case pm.IsJunction(p):
				return os.Remove(p)
			}
			return dataRmtree(p)
		}()
		if err == nil {
			removed = append(removed, p)
			continue
		}
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		failed = append(failed, dataRemovalFailure{p, dataErrText(err)})
	}
	return removed, failed
}
