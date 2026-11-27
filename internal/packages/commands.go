package packages

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// What pm install installs
type InstallArgs struct {
	Names   []string
	Extras  []string
	Without []string
	// Installs the tool closure and stops before the Python environments
	ToolsOnly bool
	// Trusts recorded digests rather than hashing every entry again
	TrustRecorded bool
	// Stages for another target, by name only
	Target string
}

// Installs packages, the required ones and the optional defaults when none are named, then the extras asked for; the exit status
func (h *Packages) Install(ctx context.Context, cfg *config.Config, a InstallArgs, r Reporter) (int, error) {
	ctx, receipt := beginReceipt(ctx, "install")
	code, err := h.install(ctx, cfg, a, r)
	receipt.finish(cfg, code)
	return code, err
}

func (h *Packages) install(ctx context.Context, cfg *config.Config, a InstallArgs, r Reporter) (int, error) {
	r = reporter(r)
	for i, name := range a.Names {
		a.Names[i] = canonicalDefault(name)
	}
	extras := strs.Unique(a.Extras)
	if message := h.installFlagError(a, extras); message != "" {
		r.Line("✗ " + message)
		return 1, nil
	}
	l := h.lock()
	names := a.Names
	bare := len(a.Names) == 0 && len(extras) == 0
	var err error
	if bare {
		if names, err = h.required(l.names()); err != nil {
			return 0, err
		}
	}
	if without := strs.Unique(a.Without); len(without) > 0 {
		packages, dropped, _ := h.splitWithout(without)
		if _, err := h.recordDeclined(packages, nil); err != nil {
			return 0, err
		}
		if _, err := h.recordDeclinedExtras(dropped, nil); err != nil {
			return 0, err
		}
		for _, extra := range dropped {
			if err := h.RemoveExtra(extra); err != nil {
				return 0, err
			}
			r.Line(fmt.Sprintf("✓ Removed %s. Later installs leave it out.", extra))
		}
	}
	var defaults []string
	if bare {
		if defaults, err = h.defaults(l.names()); err != nil {
			return 0, err
		}
	}
	failed, err := h.installNames(ctx, cfg, names, a.Target, !a.TrustRecorded, r)
	if err != nil {
		return 0, err
	}
	if failed != 0 {
		return 1, nil
	}
	if len(a.Names) > 0 && a.Target == "" {
		if _, err := h.recordDeclined(nil, a.Names); err != nil {
			return 0, err
		}
	}
	if a.Target == "" && (len(a.Names) == 0 || a.ToolsOnly) && len(extras) == 0 {
		if problems := h.drift(); len(problems) > 0 {
			verb, them := "are", "they are"
			if len(problems) == 1 {
				verb, them = "is", "it is"
			}
			r.Line(fmt.Sprintf("✗ %s %s missing or out of date, so %s not on your PATH. Run `icebath pm install` again.", joined(problems, "and"), verb, them))
			return 1, nil
		}
	}
	if a.ToolsOnly {
		return 0, h.installDefaults(ctx, cfg, defaults, !a.TrustRecorded, r)
	}
	failed = 0
	for _, extra := range extras {
		e, _ := h.extra(extra)
		r.Line(fmt.Sprintf("Installing the Python packages of %s for %s…", e.Name, e.Feature))
		if _, err := h.ProvisionExtra(ctx, cfg, e.Name, r); err != nil {
			if _, ok := errors.AsType[*InstallError](err); !ok {
				return 0, err
			}
			r.Line("✗ " + err.Error())
			failed++
			continue
		}
		r.Line("✓ Installed the extra " + e.Name + ".")
	}
	receiptOf(ctx).features(extras)
	if failed != 0 {
		return 1, nil
	}
	return 0, h.installDefaults(ctx, cfg, defaults, !a.TrustRecorded, r)
}

// First incompatible flag combination of pm install as the person reads it, empty when there is none
func (h *Packages) installFlagError(a InstallArgs, extras []string) string {
	names := len(a.Names) > 0
	if a.Target != "" {
		if !slices.Contains(Targets, a.Target) {
			return fmt.Sprintf("There is no platform %s. Use %s.", a.Target, joined(Targets, "or"))
		}
		if !names {
			return "--target needs the names of the programs to prepare."
		}
	}
	switch {
	case a.ToolsOnly && (len(extras) > 0 || a.Target != "" || names):
		return "--tools-only installs the default programs and stops, so it takes no names, --extra or --target."
	case a.TrustRecorded && (len(extras) > 0 || a.Target != "" || names):
		return "--trust-recorded installs the default programs and stops, so it takes no names, --extra or --target."
	case len(extras) > 0 && a.Target != "":
		return "--extra sets up the Python environments of this machine, so it cannot go with --target."
	}
	for _, extra := range extras {
		if _, ok := h.extra(extra); !ok {
			return h.unknownExtra(extra).Error()
		}
	}
	if len(a.Without) > 0 && (len(extras) > 0 || a.Target != "" || names) {
		return "--without changes which default programs are installed, so it takes no names, --extra or --target."
	}
	if _, _, unknown := h.splitWithout(a.Without); len(unknown) > 0 {
		return fmt.Sprintf("--without cannot leave out %s. It takes the optional default programs %s, or the extras %s.", joined(unknown, "and"),
			joined(h.defaultNames(), "and"), joined(h.extraNames(), "and"))
	}
	return ""
}

// Names of pm install --without sorted into default packages, extras and names that are neither
func (h *Packages) splitWithout(names []string) (packages, extras, unknown []string) {
	defaults := h.defaultNames()
	for _, name := range names {
		if p := canonicalDefault(name); slices.Contains(defaults, p) {
			packages = append(packages, p)
		} else if e, ok := h.extra(name); ok {
			extras = append(extras, e.Name)
		} else {
			unknown = append(unknown, name)
		}
	}
	return packages, extras, unknown
}

// Installs each name under one hold of the store's lock, or stages it for another target; the count that failed
func (h *Packages) installNames(ctx context.Context, cfg *config.Config, names []string, target string, verify bool, r Reporter) (int, error) {
	if target != "" {
		var staged []string
		for _, name := range names {
			chain, err := h.walk([]string{name}, target)
			if _, unknown := errors.AsType[*UnknownError](err); unknown {
				staged = append(staged, name)
				continue
			}
			if err != nil {
				return 0, err
			}
			for _, p := range chain {
				if !slices.Contains(staged, p.Name) {
					staged = append(staged, p.Name)
				}
			}
		}
		names = staged
	}
	op := &operation{store: h.store(), checked: map[string]bool{}}
	defer op.close()
	failed := 0
	for _, name := range names {
		bar := newProgress(r, name)
		step := func(_, stage string, done, total int64, label string) { bar.report(stage, done, total, label) }
		var err error
		if target != "" {
			var p *Package
			if p, err = h.Package(name); err == nil {
				if err = op.lock(r); err == nil {
					var entry string
					if entry, err = h.realize(ctx, p, h.lock(), nil, op.store, target, realizing{
						step: func(stage string, done, total int64, label string) { step("", stage, done, total, label) }, held: true, report: r,
					}); err == nil {
						bar.finish()
						r.Line(fmt.Sprintf("✓ %s is ready for %s in %s.", name, target, filepath.Base(entry)))
					}
				}
			}
		} else if _, err = h.Ensure(ctx, cfg, name, Ensuring{Explicit: true, SkipVerify: !verify, Step: step, Report: r, op: op}); err == nil {
			bar.finish()
			r.Line("✓ Installed " + name + ".")
			receiptOf(ctx).step("install "+name, true, "")
		}
		if err == nil {
			continue
		}
		if _, ok := errors.AsType[*InstallError](err); !ok {
			if _, unknown := errors.AsType[*UnknownError](err); !unknown {
				return failed, err
			}
			err = Failure(name, err.Error(), "Run `icebath pm install` with the name of a program it knows.")
		}
		bar.finish()
		r.Line("✗ " + err.Error())
		receiptOf(ctx).step("install "+name, false, err.Error())
		failed++
	}
	return failed, nil
}

// Installs the optional defaults one at a time, a failure warning without failing the install
func (h *Packages) installDefaults(ctx context.Context, cfg *config.Config, names []string, verify bool, r Reporter) error {
	for _, name := range names {
		failed, err := h.installNames(ctx, cfg, []string{name}, "", verify, r)
		if err != nil {
			return err
		}
		if failed != 0 {
			message := fmt.Sprintf("⚠ %s is optional and was not installed. What needs it stays unavailable until `icebath pm install %s` works.", name, name)
			r.Line(message)
			receiptOf(ctx).warn(message)
		}
	}
	return nil
}

// Required packages not installed at their pins, never installing
func (h *Packages) drift() []string {
	l := h.lock()
	target := CurrentTarget()
	var problems []string
	for _, name := range l.names() {
		p, err := h.Package(name)
		if err != nil || p.Optional || p.Internal || p.Missing(target) != "" {
			continue
		}
		if loc, err := h.located(p, l, target, locating{}); err == nil && loc == nil {
			problems = append(problems, name)
		}
	}
	return problems
}

// Variables the installed packages of the names give their programs, every package's when none are named, never this process's
func (h *Packages) Env(names []string) map[string]string {
	if len(names) == 0 {
		names = h.lock().names()
	}
	return h.EnvFor(names, map[string]string{})
}

// Checks every pinned package's install, checks and digest, then the extras and the uv cache, whose corrupt entries repair removes
func (h *Packages) Doctor(ctx context.Context, cfg *config.Config, repair bool, r Reporter) (int, error) {
	r = reporter(r)
	l := h.lock()
	target := CurrentTarget()
	s := h.store()
	bad := 0
	r.Line("Checking the programs icebath downloaded against the lock file.")
	for _, name := range l.names() {
		p, err := h.Package(name)
		if err != nil {
			return 0, err
		}
		if reason := p.Missing(target); reason != "" {
			r.Line(fmt.Sprintf("  %s is not available for %s: %s", name, target, sentence(reason, name)))
			continue
		}
		mark, weight, after := "✗", 1, " Run `icebath pm install "+name+"`."
		if p.Optional || p.Internal {
			mark, weight, after = " ", 0, " It is optional."
		}
		loc, err := h.located(p, l, target, locating{})
		if err != nil {
			return 0, err
		}
		if loc == nil {
			f, err := readFacts(s.root, false)
			if err != nil {
				return 0, err
			}
			state := "is not installed"
			if _, recorded := f.get(name); recorded {
				state = "is out of date"
			}
			r.Line(fmt.Sprintf("%s %s %s.%s", mark, name, state, after))
			bad += weight
			continue
		}
		got, _ := loc.facts.get(name)
		entry := loc.store.entry(got.Entry)
		if reason := h.verify(p, entry, target); reason != "" {
			r.Line(fmt.Sprintf("✗ %s is installed but did not pass its check: %s Run `icebath pm install %s` to install it again.", name,
				sentence(reason, name), name))
			bad++
			continue
		}
		if got.Digest != "" {
			digest, err := treeDigest(entry)
			if err != nil {
				return 0, err
			}
			if digest != got.Digest {
				r.Line(fmt.Sprintf("✗ The files of %s no longer match what was recorded when it was installed. Run `icebath pm install %s` to "+
					"install it again.", name, name))
				bad++
				continue
			}
		}
		r.Line(fmt.Sprintf("✓ %s %s", name, got.Version))
	}
	states, err := h.ExtraStates()
	if err != nil {
		return 0, err
	}
	stale := 0
	var absent []string
	for _, state := range states {
		switch {
		case !state.Provisioned:
			absent = append(absent, state.Name)
		case state.Current:
			r.Line(fmt.Sprintf("✓ The extra %s is installed for %s.", state.Name, state.Feature))
		default:
			r.Line(fmt.Sprintf("✗ The extra %s was installed from older pins. Run `%s`.", state.Name, installHint(state.Name)))
			stale++
		}
	}
	switch len(absent) {
	case 0:
	case 1:
		r.Line("  The extra " + absent[0] + " is not installed. It is optional.")
	default:
		r.Line("  The extras " + joined(absent, "and") + " are not installed. They are optional.")
	}
	cache, err := h.doctorCache(ctx, cfg, repair, r)
	if err != nil {
		return 0, err
	}
	if bad+stale+cache != 0 {
		return 1, nil
	}
	return 0, nil
}

// Reports corrupt uv cache entries an interrupted install left, removing them with repair
func (h *Packages) doctorCache(ctx context.Context, cfg *config.Config, repair bool, r Reporter) (int, error) {
	cache := h.uvCache()
	corrupt := slices.Sorted(maps.Keys(corruptCached(cache)))
	if len(corrupt) == 0 {
		r.Line("✓ The uv cache in " + cache + " has no damaged entries.")
		return 0, nil
	}
	if !repair {
		r.Line(fmt.Sprintf("✗ The uv cache in %s has damaged entries for %s. Run `icebath pm doctor --repair`, or let the next install that "+
			"meets one remove it.", cache, joined(corrupt, "and")))
		return 1, nil
	}
	e, err := h.pyenv(ctx, cfg, cache, nil, true, r)
	if err == nil {
		err = e.purge(ctx, corrupt)
	}
	if err != nil {
		if _, ok := errors.AsType[*InstallError](err); ok {
			r.Line("✗ The uv cache could not be cleaned. " + err.Error())
			return 1, nil
		}
		return 0, err
	}
	if left := slices.Sorted(maps.Keys(corruptCached(cache))); len(left) > 0 {
		r.Line(fmt.Sprintf("✗ The uv cache in %s still has damaged entries for %s after cleaning.", cache, joined(left, "and")))
		return 1, nil
	}
	r.Line(fmt.Sprintf("✓ Removed the damaged uv cache entries for %s. They download again when next used.", joined(corrupt, "and")))
	return 0, nil
}

// One dependency environment the package manager recorded: a provisioned extra or a side environment of the profile
type recorded struct {
	name   string
	root   string
	record selection
	extra  bool
}

// Every recorded dependency environment: the extras, then the profile's side environments
func (h *Packages) recordedEnvironments(cfg *config.Config) ([]recorded, error) {
	var out []recorded
	collect := func(dir string, extra bool) error {
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			root := filepath.Join(dir, entry.Name())
			if !entry.IsDir() {
				continue
			}
			record, err := selected(root)
			if err != nil {
				return err
			}
			if len(record) > 0 {
				out = append(out, recorded{name: entry.Name(), root: root, record: record, extra: extra})
			}
		}
		return nil
	}
	if err := collect(h.extrasRoot(), true); err != nil {
		return nil, err
	}
	return out, collect(environmentsDir(cfg), false)
}

// Rebuilds every recorded dependency environment that no longer runs, from the inputs it was built with
func (h *Packages) Repair(ctx context.Context, cfg *config.Config, r Reporter) (int, error) {
	ctx, receipt := beginReceipt(ctx, "repair")
	code, err := h.repair(ctx, cfg, reporter(r))
	receipt.finish(cfg, code)
	return code, err
}

func (h *Packages) repair(ctx context.Context, cfg *config.Config, r Reporter) (int, error) {
	environments, err := h.recordedEnvironments(cfg)
	if err != nil {
		return 0, err
	}
	if len(environments) == 0 {
		r.Line("No Python environment has been built yet, so there is nothing to repair. Run `icebath pm install --extra NAME` to build one.")
		return 1, nil
	}
	rebuilt, failed := 0, 0
	for _, env := range environments {
		venv := filepath.Join(env.root, env.record["generation"].(string), "venv")
		if healthy(venv) {
			continue
		}
		r.Line("Building " + env.name + " again…")
		if err := os.Remove(filepath.Join(env.root, "active.json")); err != nil {
			return 0, err
		}
		var err error
		switch {
		case env.extra:
			_, err = h.ProvisionExtra(ctx, cfg, env.name, r)
		case env.record["extras"] != nil:
			extras := strings.Split(fmt.Sprint(env.record["extras"]), ",")
			_, err = h.ExtrasEnvironment(ctx, cfg, env.name, extras, r)
		default:
			var requirements []string
			if listed, ok := env.record["requirements"].([]any); ok {
				for _, req := range listed {
					requirements = append(requirements, fmt.Sprint(req))
				}
			}
			_, err = h.Environment(ctx, cfg, EnvironmentOptions{Name: env.name, Requirements: requirements, Explicit: true, Report: r})
		}
		if err != nil {
			if _, ok := errors.AsType[*InstallError](err); !ok {
				return 0, err
			}
			r.Line("✗ " + err.Error())
			receiptOf(ctx).rebuilt(false, err.Error())
			failed++
			continue
		}
		r.Line("✓ Built " + env.name + " again.")
		receiptOf(ctx).rebuilt(true, env.name)
		rebuilt++
	}
	switch {
	case failed > 0:
		return 1, nil
	case rebuilt == 0:
		r.Line("✓ Every Python environment works, so nothing needed repair.")
		return 0, nil
	}
	r.Line("Restart icebath to use the repaired Python environments.")
	return 0, nil
}

// What pm environment builds
type EnvironmentArgs struct {
	Name         string
	Extras       []string
	Requirements []string
}

// Makes a named side environment of the profile current and returns its interpreter, every step told to the reporter
func (h *Packages) SideEnvironment(ctx context.Context, cfg *config.Config, a EnvironmentArgs, r Reporter) (string, int, error) {
	ctx, receipt := beginReceipt(ctx, "environment")
	python, code, err := h.sideEnvironment(ctx, cfg, a, reporter(r))
	receipt.finish(cfg, code)
	return python, code, err
}

func (h *Packages) sideEnvironment(ctx context.Context, cfg *config.Config, a EnvironmentArgs, r Reporter) (string, int, error) {
	if (len(a.Extras) > 0) == (len(a.Requirements) > 0) {
		r.Line("✗ Give --extra or --requirement, but not both.")
		return "", 2, nil
	}
	var python string
	var err error
	if len(a.Extras) > 0 {
		python, err = h.ExtrasEnvironment(ctx, cfg, a.Name, a.Extras, r)
	} else {
		python, err = h.Environment(ctx, cfg, EnvironmentOptions{Name: a.Name, Requirements: a.Requirements, Explicit: true, Report: r})
	}
	if err != nil {
		_, install := errors.AsType[*InstallError](err)
		if _, refused := errors.AsType[invalid](err); install || refused {
			r.Line("✗ " + err.Error())
			return "", 1, nil
		}
		return "", 0, err
	}
	receiptOf(ctx).step("environment "+a.Name, true, python)
	return python, 0, nil
}

// Removes store entries, archives and partials nothing refers to, and generations of Python environments superseded a day ago or more
func (h *Packages) Collect(ctx context.Context, cfg *config.Config, r Reporter) (int, error) {
	r = reporter(r)
	s := h.store()
	removed, kept := 0, 0
	if fsx.IsDir(s.root) || fsx.IsDir(s.partials) {
		release, err := s.lock(r)
		if err != nil {
			return 0, err
		}
		f, err := readFacts(s.root, false)
		if err == nil {
			err = f.forget(func(name string) bool {
				_, declared := h.Package(name)
				_, gone := retired[name]
				return declared == nil && !gone
			})
		}
		if err != nil {
			release()
			return 0, err
		}
		keep := f.entries()
		kept = len(keep)
		if err := collectPartials(s.partials); err != nil {
			release()
			return 0, err
		}
		removed = s.reclaim()
		entries, _ := os.ReadDir(s.root)
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || (strings.HasPrefix(name, ".") && !strings.HasPrefix(name, ".staging-")) || keep[name] {
				continue
			}
			r.Line("Removing " + name + "…")
			_ = removeTree(s.entry(name))
			removed++
		}
		release()
	}
	generations := 0
	for _, dir := range []string{h.extrasRoot(), environmentsDir(cfg)} {
		roots, _ := os.ReadDir(dir)
		for _, root := range roots {
			n, err := collectGenerations(filepath.Join(dir, root.Name()), 24*time.Hour)
			if err != nil {
				return 0, err
			}
			generations += n
		}
	}
	r.Line(fmt.Sprintf("✓ Removed %s and %s, and kept %s.", strs.Count(removed, "unused download"), strs.Count(generations, "old Python environment"),
		strs.Count(kept, "installed program")))
	return 0, nil
}

// Removes the generations of a root that are not selected and were made longer ago than minAge, under the root's lock
func collectGenerations(root string, minAge time.Duration) (int, error) {
	if !fsx.IsDir(root) {
		return 0, nil
	}
	release, err := fsx.Lock(context.Background(), filepath.Join(root, ".install.lock"), 10*time.Second)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer release()
	record, err := selected(root)
	if err != nil {
		return 0, err
	}
	entries, _ := os.ReadDir(root)
	removed := 0
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 || !generationName.MatchString(entry.Name()) ||
			entry.Name() == record["generation"] || time.Since(info.ModTime()) < minAge {
			continue
		}
		if err := removeTree(filepath.Join(root, entry.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// The newest receipt of the profile as written, or why there is none
func (h *Packages) Status(cfg *config.Config) (string, bool) {
	data, ok := h.latestReceipt(cfg)
	if !ok {
		return "No pm command has run yet, so there is no record to show. Run one, such as `icebath pm install`, first.", false
	}
	return strings.TrimRight(string(data), "\n"), true
}

// What pm update checks and changes
type UpdateArgs struct {
	Names      []string
	Check      bool
	Target     string
	SkipTarget []string
	// Also refreshes the Python environments: side environments resolved again to the newest their requirements allow, extras of
	// older pins provisioned again
	UV bool
	// Also refreshes the Node dependency trees the package manager installed
	NPM bool
	// Repins the Termux archives the pool retired
	Termux bool
}

// Resolves the newest versions of the packages and pins them, or reports what would change when checking
func (h *Packages) Update(ctx context.Context, cfg *config.Config, a UpdateArgs, r Reporter) (int, error) {
	ctx, receipt := beginReceipt(ctx, "update")
	code, err := h.update(ctx, cfg, a, reporter(r))
	receipt.finish(cfg, code)
	return code, err
}

func (h *Packages) update(ctx context.Context, cfg *config.Config, a UpdateArgs, r Reporter) (int, error) {
	if a.Termux {
		var ignored []string
		for _, flag := range []struct {
			name  string
			given bool
		}{{"names", len(a.Names) > 0}, {"--target", a.Target != ""}, {"--uv", a.UV}, {"--npm", a.NPM}} {
			if flag.given {
				ignored = append(ignored, flag.name)
			}
		}
		if len(ignored) > 0 {
			r.Line("⚠ --termux repairs only the pins of the Termux pool, so it ignores " + joined(ignored, "and") + ".")
		}
		return h.termux(ctx, cfg, a.Check, r)
	}
	l := h.lock()
	names := a.Names
	if len(names) == 0 {
		for _, name := range l.names() {
			p, err := h.Package(name)
			if err != nil {
				return 0, err
			}
			if !p.Internal || name == UV {
				names = append(names, name)
			}
		}
	}
	target := a.Target
	if target != "" && !a.Check {
		r.Line("⚠ --target works only with --check, so it is ignored. The lock file pins every platform anyway.")
		target = ""
	}
	var decisions []*decision
	var failures []string
	for _, name := range names {
		p, err := h.Package(name)
		if err != nil {
			return 0, err
		}
		var targets []string
		for _, t := range Targets {
			if p.Missing(t) == "" && !slices.Contains(a.SkipTarget, t) && (target == "" || t == target) {
				targets = append(targets, t)
			}
		}
		if len(targets) == 0 {
			continue
		}
		var locked *string
		if v, ok := l.version(name); ok {
			locked = &v
		}
		d, err := resolvePackage(cfg, p, targets, locked, l.pins[name].Artifacts)
		if err != nil {
			d = &decision{name: name, locked: locked, style: p.style(), reason: "could not find its newest version: " + err.Error(), artifacts: map[string][]string{}}
			failures = append(failures, name)
		}
		decisions = append(decisions, d)
	}
	if len(decisions) == 0 {
		r.Line("There is nothing to check, because none of these programs is built for the platforms asked for.")
		return 0, nil
	}
	width := 0
	for _, d := range decisions {
		width = max(width, utf8.RuneCountInString(d.name))
	}
	var changed []*decision
	for _, d := range decisions {
		printDecision(r, d, width)
		if d.changed() {
			changed = append(changed, d)
		}
	}
	if len(failures) > 0 {
		r.Line(fmt.Sprintf("✗ The newest version of %s could not be found, so nothing was changed.", joined(failures, "and")))
		return 1, nil
	}
	if a.Check {
		if a.UV {
			r.Line("With --uv, every Python environment would move to the newest packages its requirements allow.")
		}
		if a.NPM {
			r.Line("With --npm, `npm update` would run in every Node package folder.")
		}
		if len(changed) > 0 {
			return 1, nil
		}
		return 0, nil
	}
	if code, err := h.applyPins(ctx, cfg, changed, l, r); err != nil || code != 0 {
		return code, err
	}
	if a.UV {
		if code, err := h.refreshPython(ctx, cfg, r); err != nil || code != 0 {
			return code, err
		}
	}
	if a.NPM {
		return h.refreshNode(ctx, cfg, r), nil
	}
	return 0, nil
}

// Prints one decision of pm update: the name, then what it is pinned at and what it would move to
func printDecision(r Reporter, d *decision, width int) {
	name := d.name + strings.Repeat(" ", max(0, width-utf8.RuneCountInString(d.name)))
	pinned := func(v *string) string {
		if v == nil || *v == "" {
			return "Not pinned"
		}
		return *v
	}
	switch {
	case d.version == nil:
		r.Line(fmt.Sprintf("%s  %s", name, sentence(cmp.Or(d.reason, "up to date"), d.name)))
	case !d.changed():
		r.Line(fmt.Sprintf("%s  %s, up to date", name, pinned(d.locked)))
	case d.locked != nil && *d.version == *d.locked && len(d.artifacts) > 0:
		r.Line(fmt.Sprintf("%s  %s, with newer archives for %s", name, *d.version, joined(slices.Sorted(maps.Keys(d.artifacts)), "and")))
	default:
		per := ""
		distinct := map[string]bool{}
		for _, tv := range d.perTarget {
			distinct[tv.version] = true
		}
		if len(distinct) > 1 {
			pairs := slices.Clone(d.perTarget)
			slices.SortFunc(pairs, func(a, b targetVersion) int { return strings.Compare(a.target, b.target) })
			parts := make([]string, len(pairs))
			for i, tv := range pairs {
				parts[i] = tv.version + " for " + tv.target
			}
			per = ", by platform " + strings.Join(parts, ", ")
		}
		r.Line(fmt.Sprintf("%s  %s → %s%s", name, pinned(d.locked), *d.version, per))
	}
}

// Pins every changed package, one failing pin reported without holding back the rest, then installs what was pinned
func (h *Packages) applyPins(ctx context.Context, cfg *config.Config, changed []*decision, l *lockfile, r Reporter) (int, error) {
	if len(changed) == 0 {
		r.Line("✓ Everything is up to date, so nothing changed.")
		return 0, nil
	}
	var pinned, failedNames []string
	failed := map[string]string{}
	for _, d := range changed {
		p, err := h.Package(d.name)
		if err != nil {
			return 0, err
		}
		artifacts, err := pinArtifacts(ctx, p, d, l.pins[d.name].Artifacts)
		if err != nil {
			detail := err.Error()
			if he, ok := errors.AsType[*HTTPError](err); ok && he.URL != "" {
				detail = fmt.Sprintf("%s, from %s", err, he.URL)
			}
			r.Line(fmt.Sprintf("✗ %s could not be pinned: %s", d.name, sentence(detail, d.name)))
			if _, seen := failed[d.name]; !seen {
				failedNames = append(failedNames, d.name)
			}
			failed[d.name] = detail
			continue
		}
		l.set(d.name, *d.version, artifacts)
		pinned = append(pinned, d.name)
		if d.locked != nil && *d.locked != "" {
			r.Line(fmt.Sprintf("✓ Pinned %s at %s instead of %s.", d.name, *d.version, *d.locked))
		} else {
			r.Line(fmt.Sprintf("✓ Pinned %s at %s.", d.name, *d.version))
		}
	}
	code := 1
	if len(pinned) > 0 {
		if err := l.save(); err != nil {
			return 0, err
		}
		failedInstalls, err := h.installNames(ctx, cfg, pinned, "", true, r)
		if err != nil {
			return 0, err
		}
		code = min(failedInstalls, 1)
	} else {
		r.Line("✗ No program could be pinned, so the lock file is unchanged.")
	}
	if len(failed) == 0 {
		return code, nil
	}
	r.Line(fmt.Sprintf("✗ Pinned %s, and %s failed: %s.", strs.Count(len(pinned), "program"), strs.Count(len(failed), "program"), joined(failedNames, "and")))
	for _, name := range failedNames {
		receiptOf(ctx).step("pin "+name, false, failed[name])
	}
	return 1, nil
}

// Resolves every side environment of the profile again to the newest its requirements allow, and provisions again the extras of older pins
func (h *Packages) refreshPython(ctx context.Context, cfg *config.Config, r Reporter) (int, error) {
	environments, err := h.recordedEnvironments(cfg)
	if err != nil {
		return 0, err
	}
	states, err := h.ExtraStates()
	if err != nil {
		return 0, err
	}
	failed := 0
	for _, env := range environments {
		var err error
		switch {
		case env.extra:
			if i := slices.IndexFunc(states, func(s ExtraState) bool { return s.Name == env.name }); i >= 0 && states[i].Current {
				continue
			}
			r.Line(fmt.Sprintf("Installing %s again from the pins of this version…", env.name))
			_, err = h.ProvisionExtra(ctx, cfg, env.name, r)
		case env.record["extras"] != nil:
			r.Line(fmt.Sprintf("Checking %s against the pins of its extras…", env.name))
			_, err = h.ExtrasEnvironment(ctx, cfg, env.name, strings.Split(fmt.Sprint(env.record["extras"]), ","), r)
		default:
			var requirements []string
			if listed, ok := env.record["requirements"].([]any); ok {
				for _, req := range listed {
					requirements = append(requirements, fmt.Sprint(req))
				}
			}
			r.Line(fmt.Sprintf("Moving %s to the newest packages its requirements allow…", env.name))
			err = h.upgrade(ctx, cfg, env.name, env.root, requirements, r)
		}
		if err != nil {
			if _, ok := errors.AsType[*InstallError](err); !ok {
				return 0, err
			}
			r.Line("✗ " + env.name + " could not be brought up to date. " + err.Error())
			failed++
			continue
		}
		r.Line("✓ " + env.name + " is up to date.")
	}
	if failed > 0 {
		return 1, nil
	}
	r.Line("✓ Every Python environment is up to date.")
	return 0, nil
}

// Builds a side environment again with every pin moved to the newest its requirements allow, selecting it once it checks out
func (h *Packages) upgrade(ctx context.Context, cfg *config.Config, name, root string, requirements []string, r Reporter) error {
	release, err := fsx.Lock(ctx, filepath.Join(root, ".install.lock"), -1)
	if err != nil {
		return err
	}
	defer release()
	_, python, _, err := h.toolchain(ctx, cfg, true, true, r)
	if err != nil {
		return err
	}
	generation := filepath.Join(root, "gen-"+strs.Random(16))
	if err := os.Mkdir(generation, 0o777); err != nil {
		return err
	}
	deps, _ := json.Marshal(requirements)
	manifest := "[project]\nname = \"icebath-side-environment\"\nversion = \"0\"\nrequires-python = \">=3.11\"\ndependencies = " + string(deps) +
		"\n[tool.uv]\npackage = false\n"
	if err := os.WriteFile(filepath.Join(generation, "pyproject.toml"), []byte(manifest), 0o666); err != nil {
		removeQuiet(generation)
		return err
	}
	if _, err := h.buildProject(ctx, cfg, generation, resolved(python), true, 0, true, r); err != nil {
		removeQuiet(generation)
		return err
	}
	record, err := selected(root)
	if err != nil {
		removeQuiet(generation)
		return err
	}
	record["generation"] = filepath.Base(generation)
	return writeJSON(filepath.Join(root, "active.json"), record)
}

// Runs npm update in every Node dependency tree the package manager installed, with its own npm and Node
func (h *Packages) refreshNode(ctx context.Context, cfg *config.Config, r Reporter) int {
	trees := slices.DeleteFunc(h.trees(), func(tree string) bool { return !fsx.IsFile(filepath.Join(tree, "package.json")) })
	if len(trees) == 0 {
		r.Line("There are no Node package folders to update.")
		return 0
	}
	runner, err := h.Ensure(ctx, cfg, NPM, Ensuring{Explicit: true, Report: r})
	npm := ""
	if err == nil {
		npm = Which("npm", runner.Env[pathKey(runner.Env)])
	}
	if npm == "" {
		r.Line("✗ npm or Node is not installed. Run `icebath pm install`.")
		return 1
	}
	runner.Env = npmEnv(filepath.Join(h.storeRoot(), ".npm-cache"), runner.Env)
	code := 0
	for _, tree := range trees {
		if message := npmRun(ctx, runner, npm, tree, "update"); message != "" {
			r.Line(fmt.Sprintf("✗ The Node packages in %s could not be updated:\n%s", tree, message))
			code = 1
			continue
		}
		r.Line("✓ Updated " + tree + ".")
	}
	return code
}

// Repins the Termux archives the pool retired under the pins, a check reporting without writing
func (h *Packages) termux(ctx context.Context, cfg *config.Config, check bool, r Reporter) (int, error) {
	l := h.lock()
	pool, err := termuxIndex(cfg)
	if err != nil {
		return 0, err
	}
	stale := retiredPins(l, pool)
	if len(stale) == 0 {
		rows := 0
		for _, name := range l.names() {
			for _, archives := range l.pins[name].Artifacts {
				for _, a := range archives {
					if termuxPackage(a.URL) != "" {
						rows++
					}
				}
			}
		}
		if rows == 0 {
			r.Line("✓ No archive is pinned to the Termux pool.")
		} else {
			r.Line(fmt.Sprintf("✓ The Termux pool still serves the %s pinned to it.", strs.Count(rows, "archive")))
		}
		return 0, nil
	}
	width, unresolved := 0, 0
	for _, entry := range stale {
		width = max(width, utf8.RuneCountInString(entry.name+"@"+entry.target))
		if entry.replacement == nil {
			unresolved++
		}
	}
	for _, entry := range stale {
		label := entry.name + "@" + entry.target
		label += strings.Repeat(" ", max(0, width-utf8.RuneCountInString(label)))
		if entry.replacement == nil {
			r.Line(fmt.Sprintf("%s  %s, which the pool no longer carries", label, entry.version))
		} else {
			r.Line(fmt.Sprintf("%s  %s → %s", label, entry.version, entry.replacement.version))
		}
	}
	if check {
		return 1, nil
	}
	applied := 0
	for _, entry := range stale {
		if entry.replacement == nil {
			continue
		}
		served, err := hashURL(ctx, entry.replacement.url)
		if err != nil {
			return 0, err
		}
		if served != entry.replacement.sha256 {
			return 0, fmt.Errorf("The Termux pool serves %s with the SHA-256 %s, but its index says %s.", termuxPackage(entry.url), served, entry.replacement.sha256)
		}
		pin := l.pins[entry.name]
		artifacts := maps.Clone(pin.Artifacts)
		rows := slices.Clone(artifacts[entry.target])
		for i, a := range rows {
			if termuxPackage(a.URL) == termuxPackage(entry.url) {
				rows[i] = Artifact{URL: entry.replacement.url, SHA256: entry.replacement.sha256}
			}
		}
		artifacts[entry.target] = rows
		l.set(entry.name, pin.Version, artifacts)
		applied++
	}
	if applied > 0 {
		if err := l.save(); err != nil {
			return 0, err
		}
		r.Line(fmt.Sprintf("✓ Pinned %s again. Run `icebath pm install` on the Termux device to use them.", strs.Count(applied, "Termux archive")))
	}
	if unresolved > 0 {
		return 1, nil
	}
	return 0, nil
}

// What pm lock does: pin a package to a version, or with neither resolve the extras again
type LockArgs struct {
	Name    *string
	Version *string
}

// The pm lock usage refusal, empty when the arguments go together
func (a LockArgs) Usage() string {
	if (a.Name == nil) != (a.Version == nil) {
		return "--bump NAME and VERSION go together. Give both to pin one program, or neither to work out the Python extras again."
	}
	return ""
}

// Pins a package's archives for a version in the user's lock, or with no package resolves every extra again
func (h *Packages) Lock(ctx context.Context, cfg *config.Config, a LockArgs, r Reporter) (int, error) {
	ctx, receipt := beginReceipt(ctx, "lock")
	code, err := h.lockCommand(ctx, cfg, a, reporter(r))
	receipt.finish(cfg, code)
	return code, err
}

func (h *Packages) lockCommand(ctx context.Context, cfg *config.Config, a LockArgs, r Reporter) (int, error) {
	if a.Name == nil {
		return h.relock(ctx, cfg, r)
	}
	p, err := h.Package(*a.Name)
	if err != nil {
		return 0, err
	}
	artifacts, err := remembering(func() (map[string]Archives, error) { return h.pinVersion(ctx, p, *a.Version, r) })
	if err != nil {
		return 0, err
	}
	l := h.lock()
	l.set(p.Name, *a.Version, artifacts)
	if err := l.save(); err != nil {
		return 0, err
	}
	where := strs.Count(len(artifacts), "platform")
	if _, every := artifacts["any"]; every && len(artifacts) == 1 {
		where = "every platform"
	}
	r.Line(fmt.Sprintf("✓ Pinned %s at %s for %s.", p.Name, *a.Version, where))
	return 0, nil
}

// Exit status and message of a failure that left a pm command: an install failure, or the state folder this user cannot write
func (h *Packages) Failed(err error) (string, bool) {
	if ie, ok := errors.AsType[*InstallError](err); ok {
		return "✗ " + ie.Error(), true
	}
	if pe, ok := errors.AsType[*fs.PathError](err); ok && errors.Is(err, fs.ErrPermission) {
		denied := resolved(pe.Path)
		if fsx.Within(denied, resolved(h.stateDir())) || fsx.Within(denied, resolved(h.storeRoot())) {
			return fmt.Sprintf("✗ You cannot write to %s, where the package manager keeps its state. Run icebath as the user who installed it, or give "+
				"yourself write access.", denied), true
		}
	}
	return "", false
}
