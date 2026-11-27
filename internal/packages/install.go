package packages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	packagesconfig "github.com/nickheyer/icebath/internal/packages/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/proc"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Variable that turns installs on demand off, set by images and test harnesses
const DisableLazyEnv = "ICEBATH_DISABLE_LAZY_INSTALLS"

// A package found installed: the store holding its entry and the facts vouching for it
type location struct {
	store *store
	facts *facts
}

func (l *location) entry(name string) string {
	got, _ := l.facts.get(name)
	return l.store.entry(got.Entry)
}

// How a lookup judges what it finds
type locating struct {
	// Hash the entry again and run its checks
	verify bool
	// Settle for an entry of an earlier pin
	outdated bool
	// Leave a lost executable bit alone
	noHeal bool
}

// Store and facts holding the package at its current pin, at an earlier one when allowed, nil when it is not installed
func (h *Packages) located(p *Package, l *lockfile, target string, opts locating) (*location, error) {
	s := h.store()
	f, err := readFacts(s.root, false)
	if err != nil {
		return nil, err
	}
	got, ok := f.get(p.Name)
	if !ok || !f.installed(p.Name, s.root, nil, nil) {
		return nil, nil
	}
	entry := s.entry(got.Entry)
	if bin := p.binary(entry, target); bin != "" {
		if !fsx.IsFile(bin) {
			return nil, nil
		}
		if target == CurrentTarget() && !heal(bin, !opts.noHeal) {
			return nil, nil
		}
	}
	if opts.verify && !h.verified(p, got, s, target) {
		return nil, nil
	}
	loc := &location{store: s, facts: f}
	var version *string
	if v, ok := l.version(p.Name); ok {
		version = &v
	}
	if f.installed(p.Name, s.root, version, l.identity(p.Name, target)) {
		return loc, nil
	}
	if opts.outdated && got.Target == target && len(got.Artifacts) > 0 && got.Digest != "" {
		return loc, nil
	}
	return nil, nil
}

// Puts back a lost executable bit, false when the binary may not or cannot be made runnable
func heal(bin string, allowed bool) bool {
	if runtime.GOOS == "windows" || fsx.Executable(bin) {
		return true
	}
	st, err := os.Stat(bin)
	if !allowed || err != nil {
		return false
	}
	return os.Chmod(bin, st.Mode().Perm()|0o111) == nil && fsx.Executable(bin)
}

// Whether an entry still passes its checks and its tree matches the recorded digest
func (h *Packages) verified(p *Package, got fact, s *store, target string) bool {
	entry := s.entry(got.Entry)
	if h.verify(p, entry, target) != "" || got.Digest == "" {
		return false
	}
	digest, err := treeDigest(entry)
	return err == nil && digest == got.Digest
}

// A package found installed, as a feature asks for it
type Installed struct {
	// Its store entry
	Path    string
	Version string
	// Its binary, empty for a package without one
	Binary string
}

// The entry of an installed package at its current pin, an earlier one when allowed; nil when it is not installed, never installing
func (h *Packages) Installed(name string, outdated bool) (*Installed, error) {
	p, err := h.Package(name)
	if err != nil {
		return nil, err
	}
	if p.Internal {
		return nil, fmt.Errorf("%s is internal PM tooling, not an application package", name)
	}
	return h.installed(p, outdated)
}

func (h *Packages) installed(p *Package, outdated bool) (*Installed, error) {
	target := CurrentTarget()
	loc, err := h.located(p, h.lock(), target, locating{outdated: outdated})
	if err != nil || loc == nil {
		return nil, err
	}
	got, _ := loc.facts.get(p.Name)
	entry := loc.store.entry(got.Entry)
	return &Installed{Path: entry, Version: got.Version, Binary: p.binary(entry, target)}, nil
}

// Installed uv or uvx launcher, empty when uv is not installed
func (h *Packages) UVLauncher(name string) (string, error) {
	if name != "uv" && name != "uvx" {
		return "", fmt.Errorf("%s is not a uv launcher", name)
	}
	p, err := h.Package(UV)
	if err != nil {
		return "", err
	}
	got, err := h.installed(p, false)
	if err != nil || got == nil || got.Binary == "" {
		return "", err
	}
	launcher := filepath.Join(filepath.Dir(got.Binary), name+filepath.Ext(got.Binary))
	if !fsx.IsFile(launcher) {
		return "", nil
	}
	return launcher, nil
}

// Why there is no build of a package for this machine, empty when there is one
func (h *Packages) Missing(name string) (string, error) {
	p, err := h.Package(name)
	if err != nil {
		return "", err
	}
	return p.Missing(CurrentTarget()), nil
}

// Whether the profile lets features install what they need the first time they use it: ICEBATH_DISABLE_LAZY_INSTALLS turns that off for
// every profile
func (h *Packages) LazyAllowed(cfg *config.Config) bool {
	if config.Yes(cfg.Getenv(DisableLazyEnv)) {
		return false
	}
	return cfg.GetBool(packagesconfig.LazyInstalls)
}

// Refusal of an install on demand the settings forbid, recorded in the receipt being written
func refuseLazy(ctx context.Context, name, what string) *InstallError {
	err := Failure(name, "not installed and lazy installs are disabled: "+what, "enable "+packagesconfig.LazyInstalls+" or run `icebath pm install`")
	receiptOf(ctx).refuse("lazy-install", err.Error())
	return err
}

// Archive of a pin a cross target stage carries beside its tree
type stagePin struct {
	Target string   `json:"target"`
	SHA256 []string `json:"sha256"`
}

// How one pin is installed
type realizing struct {
	// Steps as they go: download with bytes, unpack and verify
	step func(stage string, done, total int64, label string)
	// Raw coverage of the download, for a feature drawing its own progress
	progress Progress
	// The store's install lock is held by the operation
	held   bool
	report Reporter
}

// Installs one pin: host installs record facts, a cross target stage (nil facts) marks its entry with its pin
func (h *Packages) realize(ctx context.Context, p *Package, l *lockfile, f *facts, s *store, target string, opts realizing) (string, error) {
	version, pinned := l.version(p.Name)
	if !pinned {
		return "", Failure(p.Name, "not in the lockfile", "add it with `icebath pm lock --bump`")
	}
	if reason := p.Missing(target); reason != "" {
		return "", Failure(p.Name, fmt.Sprintf("unavailable on %s: %s", target, reason), "none")
	}
	entryName := p.entryName(version, target)
	entry := s.entry(entryName)
	artifacts := l.artifacts(p.Name, target)
	pin := stagePin{Target: target}
	for _, a := range artifacts {
		pin.SHA256 = append(pin.SHA256, a.SHA256)
	}
	if !opts.held {
		release, err := s.lock(opts.report)
		if err != nil {
			return "", err
		}
		defer release()
	}
	if err := paused(ctx); err != nil {
		return "", err
	}
	var previous *fact
	if f != nil {
		if err := f.reload(true); err != nil {
			return "", err
		}
		if got, ok := f.get(p.Name); ok {
			previous = &got
		}
	}
	prefix := ".previous-"
	if f == nil {
		prefix += "stage-"
	}
	previousEntry := s.entry(prefix + entryName)
	if err := h.settle(p, s, entry, previousEntry, previous, target); err != nil {
		return "", err
	}
	s.reclaim()
	if h.current(p, l, f, s, entry, previous, version, pin, target) {
		if f != nil {
			if err := h.refreshEnv(p, f, s, entry, *previous, target); err != nil {
				return "", err
			}
		}
		s.dropDownloads(artifacts)
		return entry, nil
	}
	if len(artifacts) == 0 {
		return "", Failure(p.Name, fmt.Sprintf("no artifact for %s in the lockfile", target), "run `icebath pm lock --bump` for this package")
	}
	scratch, cleanup, err := s.scratch()
	if err != nil {
		return "", err
	}
	defer cleanup()
	err = func() error {
		build := &Build{Ctx: ctx, Version: version, Target: target, Archives: artifacts, host: h, report: reporter(opts.report)}
		staged, err := h.prepare(ctx, p, s, scratch, artifacts, build, opts)
		if err != nil {
			return err
		}
		if err := paused(ctx); err != nil {
			return err
		}
		if opts.step != nil {
			opts.step("verify", 0, 0, "")
		}
		reason, remedy := h.verify(p, staged, target), ""
		if reason != "" && p.Repair != nil {
			reason, remedy = p.Repair(ctx, h, build.report, staged, target, reason)
		}
		if reason != "" {
			return Failure(p.Name, "staged entry failed verification: "+reason, remedy)
		}
		if f == nil {
			if err := writeJSON(filepath.Join(staged, ".pm-stage-pin.json"), pin); err != nil {
				return err
			}
		}
		err = h.publishEntry(p, s, staged, entry, previousEntry, target, func() error {
			if f == nil {
				return nil
			}
			digest, err := treeDigest(entry)
			if err != nil {
				return err
			}
			return f.record(p.Name, fact{Entry: entryName, Version: version, Env: p.env(entry, target), Target: target, Artifacts: pin.SHA256,
				Digest: digest}, s.root)
		})
		if err != nil {
			return err
		}
		s.dropDownloads(artifacts)
		return nil
	}()
	if err != nil {
		if _, ok := errors.AsType[*InstallError](err); ok || IsDownload(err, Paused) {
			return "", err
		}
		return "", &InstallError{Package: p.Name, Cause: "install failed: " + err.Error(), Remedy: "retry, or run `icebath pm doctor`", Err: err}
	}
	if previous != nil && previous.Entry != "" {
		old := previous.Version + "/?"
		if len(previous.Artifacts) > 0 {
			old = previous.Version + "/" + short(previous.Artifacts[0])
		}
		fresh := version
		if len(artifacts) > 0 {
			fresh += "/" + short(artifacts[0].SHA256)
		}
		slog.Info(fmt.Sprintf("repair: %s re-realized %s -> %s", p.Name, old, fresh))
	}
	return entry, nil
}

// Fetches, unpacks, merges and stages a pin's archives, the person told of each step with the archive it is on
func (h *Packages) prepare(ctx context.Context, p *Package, s *store, scratch string, artifacts Archives, build *Build, opts realizing) (string, error) {
	label := func(i int) string {
		if len(artifacts) > 1 {
			return fmt.Sprintf("%d/%d", i+1, len(artifacts))
		}
		return ""
	}
	tick := func(done, total int64, coverage []Coverage) error {
		if opts.step != nil && len(coverage) > 0 {
			active := coverage[len(coverage)-1].Key
			if i := slices.IndexFunc(artifacts, func(a Artifact) bool { return a.URL == active }); i >= 0 {
				opts.step("download", done, total, label(i))
			}
		}
		if opts.progress != nil {
			return opts.progress(done, total, coverage)
		}
		return nil
	}
	staged := filepath.Join(scratch, "tree")
	archives, err := s.fetch(ctx, artifacts, scratch, tick)
	if err != nil {
		return "", err
	}
	for i, archive := range archives {
		if err := paused(ctx); err != nil {
			return "", err
		}
		if opts.step != nil {
			opts.step("unpack", 0, 0, label(i))
		}
		dest := staged
		if i > 0 {
			dest = filepath.Join(scratch, fmt.Sprintf("extra-%d", i))
		}
		build.Dir = dest
		switch {
		case p.Unpack != nil && !p.deb(build.Target):
			err = p.Unpack(build, archive)
		default:
			err = Extract(archive, dest)
		}
		if err != nil {
			return "", err
		}
		if i > 0 {
			if err := Merge(dest, staged); err != nil {
				return "", err
			}
		}
	}
	build.Dir = staged
	switch {
	case p.deb(build.Target):
	case p.Stage != nil:
		err = p.Stage(build)
	case !p.Nested:
		err = Flatten(staged)
	}
	return staged, err
}

// Whether an entry already realizes the pin: through the host's facts, or a cross target stage through its pin marker
func (h *Packages) current(p *Package, l *lockfile, f *facts, s *store, entry string, previous *fact, version string, pin stagePin, target string) bool {
	if f != nil {
		return previous != nil && f.installed(p.Name, s.root, &version, l.identity(p.Name, target)) && h.verified(p, *previous, s, target)
	}
	data, err := os.ReadFile(filepath.Join(entry, ".pm-stage-pin.json"))
	if err != nil {
		return false
	}
	var marked stagePin
	return json.Unmarshal(data, &marked) == nil && marked.Target == pin.Target && slices.Equal(marked.SHA256, pin.SHA256) && h.verify(p, entry, target) == ""
}

// Records an entry's variables again when the declaration's changed, its digest still vouching for its bytes
func (h *Packages) refreshEnv(p *Package, f *facts, s *store, entry string, previous fact, target string) error {
	env := p.env(entry, target)
	if f.env(p.Name, s.root).equal(env) {
		return nil
	}
	previous.Env = env
	return f.record(p.Name, previous, s.root)
}

// Publishes a staged tree, the entry it replaces kept to roll back to until the facts are committed
func (h *Packages) publishEntry(p *Package, s *store, staged, entry, previousEntry, target string, commit func() error) error {
	keptLive := false
	if fsx.Lexists(entry) {
		if err := fsx.Retry(func() error { return os.Rename(entry, previousEntry) }); err != nil {
			live, lerr := treeDigest(entry)
			fresh, serr := treeDigest(staged)
			if h.verify(p, entry, target) != "" || lerr != nil || serr != nil || live != fresh {
				return err
			}
			slog.Warn(fmt.Sprintf("%s entry is in use but already matches the staged bytes; keeping it live", p.Name))
			keptLive = true
		}
	}
	err := func() error {
		if !keptLive {
			if err := s.publish(staged, filepath.Base(entry)); err != nil {
				return err
			}
		}
		if reason := h.verify(p, entry, target); reason != "" {
			return Failure(p.Name, "published entry failed verification: "+reason, "")
		}
		return commit()
	}()
	if err != nil {
		if fsx.IsDir(previousEntry) {
			if rerr := h.restore(s, entry, previousEntry); rerr != nil {
				return rerr
			}
		}
		return err
	}
	if fsx.IsDir(previousEntry) {
		s.discard(filepath.Base(previousEntry))
	}
	return nil
}

// Puts the previous tree back, both kept until the rename lands
func (h *Packages) restore(s *store, entry, previousEntry string) error {
	displaced := s.entry(".displaced-" + strs.Random(16))
	had := fsx.Lexists(entry)
	if had {
		if err := fsx.Retry(func() error { return os.Rename(entry, displaced) }); err != nil {
			return err
		}
	}
	if err := fsx.Retry(func() error { return os.Rename(previousEntry, entry) }); err != nil {
		if had {
			if rerr := fsx.Retry(func() error { return os.Rename(displaced, entry) }); rerr != nil {
				return rerr
			}
		}
		return err
	}
	if had {
		s.discard(filepath.Base(displaced))
	}
	return nil
}

// Finishes or undoes a publication an interrupted install left behind
func (h *Packages) settle(p *Package, s *store, entry, previousEntry string, previous *fact, target string) error {
	if !fsx.IsDir(previousEntry) {
		return nil
	}
	if previous != nil && previous.Entry == filepath.Base(entry) && h.verified(p, *previous, s, target) {
		s.discard(filepath.Base(previousEntry))
		return nil
	}
	return h.restore(s, entry, previousEntry)
}

// Releases the cached archives of a pin once its entry is published
func (s *store) dropDownloads(artifacts Archives) {
	for _, a := range artifacts {
		s.discard("fetch-" + a.SHA256)
	}
}

// One command's hold on the store's install lock and what it already checked under it
type operation struct {
	store   *store
	release func()
	checked map[string]bool
}

func (o *operation) lock(r Reporter) error {
	if o.release != nil {
		return nil
	}
	release, err := o.store.lock(r)
	if err != nil {
		return err
	}
	o.release = release
	return nil
}

func (o *operation) close() {
	clear(o.checked)
	if o.release != nil {
		o.release()
		o.release = nil
	}
}

// How ensure installs
type Ensuring struct {
	// Base environment of the runner, this process's when nil
	Base map[string]string
	// A person asked for it, so the lazy install setting does not apply and the entries are checked again
	Explicit bool
	// Trusts recorded digests instead of hashing entries again
	SkipVerify bool
	// Steps of each package's install as they go
	Step func(name, stage string, done, total int64, label string)
	// Download coverage, for a feature drawing its own progress
	Progress Progress
	// Where lock waits and build notes go
	Report Reporter
	op     *operation
}

// A package installed with what it needs, and the environment its programs run in
type Runner struct {
	Name string
	Env  map[string]string
}

// Command run with the runner's environment, a bare name found on the runner's PATH
func (r *Runner) Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	if !strings.ContainsAny(name, `/\`) {
		if found := Which(name, r.Env[pathKey(r.Env)]); found != "" {
			name = found
		}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = envList(r.Env)
	proc.HideWindow(cmd)
	return cmd
}

// Makes a package and what it needs match their pins, installing what is missing when allowed, and returns its runner
func (h *Packages) Ensure(ctx context.Context, cfg *config.Config, name string, opts Ensuring) (*Runner, error) {
	target := CurrentTarget()
	chain, err := h.walk([]string{name}, target)
	if err != nil {
		return nil, err
	}
	l := h.lock()
	s := h.store()
	checked := map[string]bool{}
	if opts.op != nil {
		checked = opts.op.checked
		if err := opts.op.lock(opts.Report); err != nil {
			return nil, err
		}
	}
	var missing []*Package
	for _, p := range chain {
		key := p.Name + "\x00" + target
		if v, ok := l.version(p.Name); ok {
			key += "\x00" + v
		}
		if checked[key] {
			continue
		}
		loc, err := h.located(p, l, target, locating{verify: opts.Explicit && !opts.SkipVerify})
		if err != nil {
			return nil, err
		}
		if loc == nil {
			missing = append(missing, p)
			continue
		}
		got, _ := loc.facts.get(p.Name)
		if err := h.refreshFound(p, loc, got, target, opts.op != nil, opts.Report); err != nil {
			return nil, err
		}
		checked[key] = true
	}
	if len(missing) > 0 && !opts.Explicit && !h.LazyAllowed(cfg) {
		names := make([]string, len(missing))
		for i, p := range missing {
			names[i] = p.Name
		}
		return nil, refuseLazy(ctx, name, strings.Join(names, ", "))
	}
	if len(missing) > 0 {
		f, err := readFacts(s.root, false)
		if err != nil {
			return nil, err
		}
		for _, p := range missing {
			clear(checked)
			var step func(stage string, done, total int64, label string)
			if opts.Step != nil {
				step = func(stage string, done, total int64, label string) { opts.Step(p.Name, stage, done, total, label) }
			}
			if _, err := h.realize(ctx, p, l, f, s, target, realizing{step: step, progress: opts.Progress, held: opts.op != nil,
				report: opts.Report}); err != nil {
				return nil, err
			}
		}
	}
	return &Runner{Name: name, Env: h.EnvFor([]string{name}, opts.Base)}, nil
}

// Records a found entry's variables again when the declaration's changed
func (h *Packages) refreshFound(p *Package, loc *location, got fact, target string, held bool, r Reporter) error {
	entry := loc.store.entry(got.Entry)
	if loc.facts.env(p.Name, loc.store.root).equal(p.env(entry, target)) {
		return nil
	}
	if !held {
		release, err := loc.store.lock(r)
		if err != nil {
			return err
		}
		defer release()
	}
	if err := loc.facts.reload(true); err != nil {
		return err
	}
	if previous, ok := loc.facts.get(p.Name); ok && previous.Entry == got.Entry {
		return h.refreshEnv(p, loc.facts, loc.store, entry, previous, target)
	}
	return nil
}

// Environment of installed packages and what they need over base, this process's when nil; never installs, skipping what is missing
func (h *Packages) EnvFor(names []string, base map[string]string) map[string]string {
	if base == nil {
		base = environ()
	}
	return compose(h.diffs(names), base)
}

// Recorded variables of the installed packages of names and their dependencies, dependencies first
func (h *Packages) diffs(names []string) []Env {
	target := CurrentTarget()
	l := h.lock()
	var diffs []Env
	for _, name := range names {
		chain, err := h.walk([]string{name}, target)
		if err != nil {
			continue
		}
		for _, p := range chain {
			if p.Internal {
				continue
			}
			if loc, err := h.located(p, l, target, locating{}); err == nil && loc != nil {
				diffs = append(diffs, loc.facts.env(p.Name, loc.store.root))
			}
		}
	}
	return diffs
}

// Variables over base, later ones winning, every PATH folder put first with the last package's first
func compose(diffs []Env, base map[string]string) map[string]string {
	env := make(map[string]string, len(base))
	for k, v := range base {
		env[k] = v
	}
	var dirs []string
	for i := len(diffs) - 1; i >= 0; i-- {
		for _, dir := range diffs[i].Path {
			if !slices.Contains(dirs, dir) {
				dirs = append(dirs, dir)
			}
		}
	}
	for _, diff := range diffs {
		for k, v := range diff.Vars {
			env[k] = v
		}
	}
	if len(dirs) > 0 {
		key := pathKey(env)
		value := strings.Join(dirs, string(os.PathListSeparator))
		if existing := env[key]; existing != "" {
			value += string(os.PathListSeparator) + existing
		}
		env[key] = value
	}
	return env
}

// Key of PATH in an environment, as Windows spells it there
func pathKey(env map[string]string) string {
	if runtime.GOOS == "windows" {
		for k := range env {
			if strings.EqualFold(k, "PATH") {
				return k
			}
		}
	}
	return "PATH"
}

// PATH folders of every installed package that goes on PATH and whose dependencies are all installed, dependents first
func (h *Packages) storeDirs() []string {
	target := CurrentTarget()
	l := h.lock()
	names := l.names()
	order := slices.Clone(names)
	for _, name := range names {
		chain, err := h.walk([]string{name}, target)
		if err != nil {
			continue
		}
		at := slices.Index(order, name)
		lowest := at
		for _, dep := range chain {
			if i := slices.Index(order, dep.Name); dep.Name != name && i >= 0 && i < lowest {
				lowest = i
			}
		}
		if lowest < at {
			order = slices.Insert(slices.Delete(order, at, at+1), lowest, name)
		}
	}
	found := map[string]*location{}
	locate := func(p *Package) *location {
		if loc, ok := found[p.Name]; ok {
			return loc
		}
		loc, _ := h.located(p, l, target, locating{})
		found[p.Name] = loc
		return loc
	}
	var dirs []string
	for _, name := range order {
		p, err := h.Package(name)
		if err != nil || p.Internal || !p.onPath() || p.Missing(target) != "" {
			continue
		}
		loc := locate(p)
		if loc == nil {
			continue
		}
		chain, err := h.walk([]string{name}, target)
		if err != nil || slices.ContainsFunc(chain, func(dep *Package) bool { return dep.Missing(target) == "" && locate(dep) == nil }) {
			continue
		}
		for _, dir := range loc.facts.env(name, loc.store.root).Path {
			if dir != "" && !slices.Contains(dirs, dir) {
				dirs = append(dirs, dir)
			}
		}
	}
	return dirs
}

// PATH for the user's own commands: the folders of the installed packages behind the user's entries, so a tool the user installed wins,
// and the package manager's Python just before the Windows Store alias folder whose python only opens the Store
func (h *Packages) Path(path string) string {
	store := h.storeDirs()
	if len(store) == 0 {
		return path
	}
	fold := func(dir string) string {
		if runtime.GOOS == "windows" {
			return strings.ToLower(filepath.Clean(dir))
		}
		return dir
	}
	ours := map[string]bool{}
	for _, dir := range store {
		ours[fold(dir)] = true
	}
	var kept []string
	for _, entry := range filepath.SplitList(path) {
		if entry != "" && !ours[fold(entry)] {
			kept = append(kept, entry)
		}
	}
	python := map[string]bool{}
	for _, diff := range h.diffs([]string{Python}) {
		for _, dir := range diff.Path {
			python[fold(dir)] = true
		}
	}
	cut := len(kept)
	if i := slices.IndexFunc(kept, storeAlias); i >= 0 {
		cut = i
	}
	var early, late []string
	for _, dir := range store {
		if python[fold(dir)] {
			early = append(early, dir)
		} else {
			late = append(late, dir)
		}
	}
	ordered := slices.Concat(kept[:cut], early, kept[cut:], late)
	var unique []string
	for _, dir := range ordered {
		if !slices.Contains(unique, dir) {
			unique = append(unique, dir)
		}
	}
	return strings.Join(unique, string(os.PathListSeparator))
}

// The Windows app alias folder whose python.exe only opens the Store, a Store Python shipping python3.N.exe being real
func storeAlias(dir string) bool {
	clean := filepath.Clean(dir)
	if !strings.EqualFold(filepath.Base(clean), "WindowsApps") || !strings.EqualFold(filepath.Base(filepath.Dir(clean)), "Microsoft") {
		return false
	}
	found, _ := filepath.Glob(filepath.Join(clean, "python3.*.exe"))
	return len(found) == 0
}

// Bash of the package manager's Git for Windows, the shell commands run with on Windows; empty elsewhere or when it is not installed
func (h *Packages) GitBash() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	p, err := h.Package(Git)
	if err != nil {
		return ""
	}
	got, err := h.installed(p, true)
	if err != nil || got == nil {
		return ""
	}
	for _, candidate := range []string{filepath.Join(got.Path, "usr", "bin", "bash.exe"), filepath.Join(got.Path, "bin", "bash.exe")} {
		if fsx.IsFile(candidate) {
			if r, err := run(context.Background(), command{argv: []string{candidate, "-c", "exit 0"}, timeout: 5 * time.Second}); err == nil && r.code == 0 {
				return candidate
			}
		}
	}
	return ""
}

// First twelve characters of a digest
func short(digest string) string {
	return digest[:min(12, len(digest))]
}
