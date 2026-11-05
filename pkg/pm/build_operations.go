package pm

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// CheckProjectLockOptions are check_project_lock's keywords, Quiet capturing uv's output
type CheckProjectLockOptions struct {
	Source   string
	Python   string
	Cache    string
	Env      map[string]string
	Offline  bool
	Explicit bool
	Quiet    bool
}

// Refuses a missing or stale lock without rewriting the source or creating a venv
func checkProjectLock(ctx context.Context, opts CheckProjectLockOptions) error {
	if !opts.Offline {
		if err := requireInstallAllowed(ctx, opts.Explicit); err != nil {
			return err
		}
	}
	source := absoluteUnresolved(opts.Source)
	if !isRegularFile(filepath.Join(source, "pyproject.toml")) {
		return NewInstallError("venv", "project manifest is missing: "+source, "")
	}
	var output io.Writer = os.Stderr
	if opts.Quiet {
		output = nil
	}
	environment, err := ManagedEnvironment(ctx, filepath.Join(source, ".venv"), ManagedEnvironmentOptions{Python: opts.Python,
		Cache: opts.Cache, Env: opts.Env, Offline: opts.Offline, Explicit: opts.Explicit, Output: output})
	if err != nil {
		return err
	}
	return environment.CheckLock(ctx, source)
}

// ExportRequirementsOptions are export_requirements's keywords
type ExportRequirementsOptions struct {
	Source   string
	Out      string
	Extras   []string
	Python   string
	Cache    string
	Env      map[string]string
	Explicit bool
}

// Exports locked runtime requirements with markers and direct URL pins and returns the written file
func exportRequirements(ctx context.Context, opts ExportRequirementsOptions) (string, error) {
	if err := requireInstallAllowed(ctx, opts.Explicit); err != nil {
		return "", err
	}
	source, out := absoluteUnresolved(opts.Source), absoluteUnresolved(opts.Out)
	if !isRegularFile(filepath.Join(source, "pyproject.toml")) {
		return "", NewInstallError("venv", "project manifest is missing: "+source, "")
	}
	if lock := filepath.Join(source, "uv.lock"); !isRegularFile(lock) {
		return "", NewInstallError("venv", "frozen export requires a lock: "+lock, "")
	}
	environment, err := ManagedEnvironment(ctx, filepath.Join(source, ".venv"), ManagedEnvironmentOptions{Python: opts.Python,
		Cache: opts.Cache, Env: opts.Env, Explicit: opts.Explicit, Output: os.Stderr})
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o777); err != nil {
		return "", err
	}
	if err := environment.ExportRequirements(ctx, source, out, opts.Extras, 0); err != nil {
		return "", err
	}
	return out, nil
}

// BuildRequirementsEnvironmentOptions are build_requirements_environment's keywords, empty paths for None
type BuildRequirementsEnvironmentOptions struct {
	Requirements []string
	Out          string
	Python       string
	Cache        string
	Env          map[string]string
	Wheelhouse   string
	Offline      bool
	Sealed       bool
	Explicit     bool
	Lock         string
}

// Creates and checks a fresh environment from requirement lines, a wheelhouse disabling indexes and source builds
func buildRequirementsEnvironment(ctx context.Context, opts BuildRequirementsEnvironmentOptions) (string, error) {
	if err := requireInstallAllowed(ctx, opts.Explicit); err != nil {
		return "", err
	}
	requirements := []string{}
	if len(opts.Requirements) > 0 {
		var err error
		if requirements, err = normalizedRequirements(opts.Requirements); err != nil {
			return "", err
		}
	}
	out := absoluteUnresolved(opts.Out)
	lock, wheelhouse := "", ""
	if opts.Lock != "" {
		lock = absoluteUnresolved(opts.Lock)
	}
	if opts.Wheelhouse != "" {
		wheelhouse = absoluteUnresolved(opts.Wheelhouse)
		if !isDirectory(wheelhouse) {
			return "", NewInstallError("venv", "wheelhouse directory is missing: "+wheelhouse, "")
		}
	}
	if existsOrLink(out) {
		return "", &DestinationExistsError{Path: out}
	}
	environment, err := ManagedEnvironment(ctx, out, ManagedEnvironmentOptions{Python: opts.Python, Cache: opts.Cache, Env: opts.Env,
		Offline: opts.Offline, Explicit: opts.Explicit, Output: os.Stderr})
	if err != nil {
		return "", err
	}
	err = freshBuild(ctx, environment, opts.Sealed, lock, func() error {
		return environment.InstallRequirements(ctx, requirements, wheelhouse, lock)
	})
	if err != nil {
		return "", err
	}
	return environment.Executable(), nil
}

// Refuses a closure that names application state instead of tools
func toolClosure(names []string, refusal string) ([]Package, error) {
	packages, err := Walk(names)
	if err != nil {
		return nil, err
	}
	for _, p := range packages {
		if _, state := p.(StatePackage); state {
			return nil, &PyError{Class: "ValueError", Message: refusal}
		}
	}
	return packages, nil
}

// PrepareToolsOptions are prepare_tools's arguments
type PrepareToolsOptions struct {
	Names  []string
	Out    string
	Target string
}

// Realizes an explicit tool closure in a build owned store instead of live state
func prepareTools(ctx context.Context, opts PrepareToolsOptions) (string, error) {
	packages, err := toolClosure(opts.Names, "prepare_tools accepts tools, not application state")
	if err != nil {
		return "", err
	}
	out := resolvePath(opts.Out)
	store, lock := NewStore(out), currentLockfile()
	release, err := store.InstallLock()
	if err != nil {
		return "", err
	}
	defer release()
	facts, err := NewFacts(filepath.Join(out, "facts.json"), true)
	if err != nil {
		return "", err
	}
	for _, p := range packages {
		if _, err := InstallPackage(ctx, p, lock, facts, store, opts.Target, InstallPackageOptions{LockHeld: true}); err != nil {
			return "", err
		}
	}
	return out, nil
}

// Refuses a tool entry holding a link that is absolute, dangling or escapes the entry
func checkCopyLinks(p Package, entry string) error {
	var walk func(directory string) error
	walk = func(directory string) error {
		children, err := os.ReadDir(directory)
		if err != nil {
			return nil
		}
		var dirs, files []string
		for _, child := range children {
			path := filepath.Join(directory, child.Name())
			if st, err := os.Stat(path); err == nil && st.IsDir() {
				dirs = append(dirs, path)
			} else {
				files = append(files, path)
			}
		}
		for _, path := range append(append([]string{}, dirs...), files...) {
			st, err := os.Lstat(path)
			if err != nil || (st.Mode()&os.ModeSymlink == 0 && !IsJunction(path)) {
				continue
			}
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_, statErr := os.Stat(path)
			if filepath.IsAbs(target) || statErr != nil || !isRelativeTo(resolvePath(path), entry) {
				return NewInstallError(p.Base().Name, "tool copy link escapes entry: "+Native(path), "")
			}
		}
		for _, dir := range dirs {
			if st, err := os.Lstat(dir); err == nil && st.Mode()&os.ModeSymlink == 0 && !IsJunction(dir) {
				if err := walk(dir); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(entry)
}

// VerifiedTools is an admitted tool closure, its entries in ordinary spelling and their recorded env diffs
type VerifiedTools struct {
	Entries map[string]InstalledPackage
	envs    []EnvDiff
}

// Environment the admitted closure composes over a base
func (v *VerifiedTools) Environment(base map[string]string) map[string]string {
	copied := map[string]string{}
	for k, val := range base {
		copied[k] = val
	}
	return ComposeEnv(v.envs, copied)
}

// Reads a pinned tool closure from a store without writes, acquisition or running any binary
func ReadVerifiedTools(names []string, sourceStore, target string, lock *Lockfile) (*VerifiedTools, error) {
	packages, err := toolClosure(names, "verified_tools accepts tool packages, not application state")
	if err != nil {
		return nil, err
	}
	sourceStore = resolvePath(sourceStore)
	if !isRegularFile(filepath.Join(sourceStore, "facts.json")) {
		return nil, NewInstallError("tools", "source facts missing: "+sourceStore, "")
	}
	sourceStore = LongRoot(sourceStore)
	facts, err := NewFacts(filepath.Join(sourceStore, "facts.json"), true)
	if err != nil {
		return nil, err
	}
	if lock == nil {
		lock = currentLockfile()
	}
	selection := &VerifiedTools{Entries: map[string]InstalledPackage{}}
	for _, p := range packages {
		if pin, ok := p.(PinOnlyPackage); ok && pin.PinOnly() {
			continue
		}
		name := p.Base().Name
		version, pinned := lock.Version(name)
		identity := pinIdentity(lock, name, target)
		if !pinned || version == "" || identity == nil {
			return nil, NewInstallError(name, "tool source failed verification: missing pin", "")
		}
		fact, _ := facts.Get(name)
		expected := p.StoreEntry(version, target)
		entry := filepath.Join(sourceStore, expected)
		failed := NewInstallError(name, "tool source failed verification", "run preparation again")
		recorded, isString := fact["entry"].(string)
		if len(fact) == 0 || !isString || recorded != expected || !isDirectory(entry) || !isRelativeTo(resolvePath(entry), sourceStore) ||
			!facts.Installed(name, &version, sourceStore, identity) {
			return nil, failed
		}
		matches, err := DigestMatches(entry, factDigest(fact))
		if err != nil {
			return nil, err
		}
		if !matches {
			return nil, failed
		}
		if err := checkCopyLinks(p, entry); err != nil {
			return nil, err
		}
		binary := p.Binary(entry, target)
		env, err := PackageEnv(p, entry, target)
		if err != nil {
			return nil, err
		}
		if (binary != "" && (!isRegularFile(binary) || !isRelativeTo(resolvePath(binary), entry))) || !envDiffEqual(facts.EnvFor(name, sourceStore), env) {
			return nil, NewInstallError(name, "tool source failed verification: binary or environment", "")
		}
		installed := InstalledPackage{Path: Native(entry), Version: version}
		if binary != "" {
			installed.Binary = Native(binary)
		}
		selection.Entries[name] = installed
		selection.envs = append(selection.envs, env)
	}
	return selection, nil
}

// StageToolsOptions are stage_tools's arguments
type StageToolsOptions struct {
	Names       []string
	SourceStore string
	Out         string
	Target      string
}

// Copies a current verified tool closure into a disjoint store without acquisition or live selection
func stageTools(ctx context.Context, opts StageToolsOptions) (string, error) {
	sourceStore, out := resolvePath(opts.SourceStore), resolvePath(opts.Out)
	if isRelativeTo(sourceStore, out) || isRelativeTo(out, sourceStore) {
		return "", &PyError{Class: "ValueError", Message: "tool stores must be disjoint"}
	}
	if !isRegularFile(filepath.Join(sourceStore, "facts.json")) {
		return "", NewInstallError("tools", "source facts missing: "+sourceStore, "")
	}
	packages, err := toolClosure(opts.Names, "stage_tools accepts tool packages, not application state")
	if err != nil {
		return "", err
	}
	source, destination := NewStore(sourceStore), NewStore(out)
	lock := currentLockfile()
	stores := []*Store{source, destination}
	sort.SliceStable(stores, func(i, j int) bool { return stores[i].Root < stores[j].Root })
	for _, store := range stores {
		release, err := store.InstallLock()
		if err != nil {
			return "", err
		}
		defer release()
	}
	selection, err := ReadVerifiedTools(opts.Names, sourceStore, opts.Target, lock)
	if err != nil {
		return "", err
	}
	facts, err := NewFacts(filepath.Join(sourceStore, "facts.json"), true)
	if err != nil {
		return "", err
	}
	for _, p := range packages {
		name := p.Base().Name
		if _, selected := selection.Entries[name]; !selected {
			continue
		}
		fact, _ := facts.Get(name)
		if !entryVerified(p, fact, source, opts.Target) {
			return "", NewInstallError(name, "tool copy source failed verification", "run preparation again")
		}
	}
	staged, err := NewFacts(filepath.Join(out, "facts.json"), true)
	if err != nil {
		return "", err
	}
	for _, p := range packages {
		started := time.Now()
		if _, err := InstallPackage(ctx, p, lock, staged, destination, opts.Target, InstallPackageOptions{
			CopyFrom: &Location{Facts: facts, Store: source}, LockHeld: true, FreshCopy: true}); err != nil {
			return "", err
		}
		fmt.Printf("    copied %s (%.1fs)\n", p.Base().Name, time.Since(started).Seconds())
	}
	return out, nil
}

// PruneCacheOptions are prune_cache's arguments
type PruneCacheOptions struct {
	Cache string
	CI    bool
}

// Prunes unused uv cache entries, CI mode also discarding downloaded wheels
func pruneCache(ctx context.Context, opts PruneCacheOptions) error {
	cache := absoluteUnresolved(opts.Cache)
	environment, err := ManagedEnvironment(ctx, cache, ManagedEnvironmentOptions{Cache: cache, NoRealize: true, Output: os.Stderr})
	if err != nil {
		return err
	}
	return environment.PruneCache(ctx, opts.CI)
}
