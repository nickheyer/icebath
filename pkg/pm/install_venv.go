package pm

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// VenvState is the dependency environment state package a venv sync drives, the catalogue Venv or a test double
type VenvState interface {
	StatePackage
	ProjectRoot() string
	Stamp(ctx context.Context, extras []string, pluginDirs []MemberDir) (string, error)
	Prepare(ctx context.Context, extras []string, opts VenvApplyOptions) (*VenvGeneration, error)
}

var _ VenvState = (*Venv)(nil)

// Plugin state change a sync publishes under the dependency lock
type pluginChange interface {
	Publish(ctx context.Context, project string) error
}

type syncOutputKey struct{}

// Context whose sync output, the stdout a Python sync was redirected to, goes to w
func withSyncOutput(ctx context.Context, w io.Writer) context.Context {
	if w == nil {
		return ctx
	}
	return context.WithValue(ctx, syncOutputKey{}, w)
}

// Writer a dependency sync prints operator messages to, the process stdout unless redirected
func SyncOutput(ctx context.Context) io.Writer {
	if w, ok := ctx.Value(syncOutputKey{}).(io.Writer); ok {
		return w
	}
	return os.Stdout
}

// Registered venv state package as the sync engine drives it
func venvPackage() (VenvState, error) {
	p, err := GetPackage("venv")
	if err != nil {
		return nil, err
	}
	return asVenvState(p)
}

// Venv state view of a package, an AttributeError when it lacks the project root
func asVenvState(p Package) (VenvState, error) {
	v, ok := p.(VenvState)
	if !ok {
		return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'project_root'", reflectTypeName(p))}
	}
	return v, nil
}

// Python class name of a Go value, the pointed to type for pointers
func reflectTypeName(v any) string {
	t := reflect.TypeOf(v)
	if t == nil {
		return "NoneType"
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}

// Recorded venv fact of a facts file, nil for Python None and for a value that is not an object
func venvFact(f *Facts) map[string]any {
	fact, _ := f.Get("venv")
	return fact
}

// Recorded venv selection of this binary's own state, then of the store, then empty
func recordedVenvFact(ctx context.Context, runtimeFacts *Facts) (map[string]any, error) {
	if fact := venvFact(runtimeFacts); len(fact) > 0 {
		return fact, nil
	}
	facts, err := storeFacts(ctx)
	if err != nil {
		return nil, err
	}
	if fact := venvFact(facts); len(fact) > 0 {
		return fact, nil
	}
	return map[string]any{}, nil
}

// Reports a recorded selection whose stamp and selected environment match what the sync must reach
func runtimeStateMatches(ctx context.Context, fact map[string]any, stamp, projectRoot string) (bool, error) {
	if fact == nil || !jsonEquals(fact["stamp"], stamp) {
		return false, nil
	}
	root := projectRoot
	if root == "" {
		root = RepoRoot()
	}
	environment, err := SelectedVenv(ctx, root)
	if err != nil {
		if caughtAsOSRuntimeValue(err) {
			return false, nil
		}
		return false, err
	}
	if recorded, present := fact["environment"]; present && recorded != nil {
		text, ok := recorded.(string)
		if !ok || resolvePath(text) != environment {
			return false, nil
		}
	}
	return isRegularFile(filepath.Join(environment, "pyvenv.cfg")), nil
}

// Plugin dirs argument of the venv package for a member input, nil meaning config discovery
func memberInputs(ctx context.Context, plugins PluginInput) ([]MemberDir, error) {
	switch p := plugins.(type) {
	case nil:
		return nil, nil
	case *Candidates:
		members, err := candidateMembers(ctx, p.Dirs)
		if err != nil {
			return nil, err
		}
		if members == nil {
			members = []MemberDir{}
		}
		return members, nil
	case *Members:
		if p.Dirs == nil {
			return []MemberDir{}, nil
		}
		return p.Dirs, nil
	}
	return nil, &PyError{Class: "TypeError", Message: reflectTypeName(plugins) + " changes plugin state; only a sync may carry it"}
}

var extraSeparators = regexp.MustCompile(`[-_.]+`)

// PEP 685 name uv matches an extra by, separator runs folded to one dash and lowercased
func ExtraKey(name string) string {
	return pystr.Lower(extraSeparators.ReplaceAllString(name, "-"))
}

// Recorded extras the project still declares, the recorded spelling kept for uv
func stillDeclared(p VenvState, recorded []string) ([]string, error) {
	root := p.ProjectRoot()
	if !isRegularFile(filepath.Join(root, "pyproject.toml")) {
		return append([]string{}, recorded...), nil
	}
	declared, err := declaredExtras(root)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, extra := range declared {
		keys[ExtraKey(extra)] = true
	}
	out := []string{}
	for _, extra := range recorded {
		if keys[ExtraKey(extra)] {
			out = append(out, extra)
		}
	}
	return out, nil
}

// Recorded extras a sync carries forward, still declared and not dropped with install without
func CarriedExtras(ctx context.Context, p StatePackage, recorded []string) ([]string, error) {
	venv, err := asVenvState(p)
	if err != nil {
		return nil, err
	}
	refused, err := declinedExtras(ctx, venv.ProjectRoot())
	if err != nil {
		return nil, err
	}
	still, err := stillDeclared(venv, recorded)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, extra := range still {
		if !refused[ExtraKey(extra)] {
			out = append(out, extra)
		}
	}
	return out, nil
}

// Sorted union of string sets
func sortedUnion(lists ...[]string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, list := range lists {
		for _, item := range list {
			if !seen[item] {
				seen[item] = true
				out = append(out, item)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Validated recorded selection, an invalid recorded dependency state ValueError when it is malformed
func recordedSelection(value any) (map[string]any, []string, error) {
	invalid := &PyError{Class: "ValueError", Message: "invalid recorded dependency state"}
	fact, ok := value.(map[string]any)
	if !ok {
		return nil, nil, invalid
	}
	if stamp, isStr := fact["stamp"].(string); !isStr || stamp == "" {
		return nil, nil, invalid
	}
	list, isList := fact["extras"].([]any)
	if !isList {
		return nil, nil, invalid
	}
	extras := make([]string, 0, len(list))
	for _, item := range list {
		s, isStr := item.(string)
		if !isStr {
			return nil, nil, invalid
		}
		extras = append(extras, s)
	}
	return fact, extras, nil
}

// Probes the requested union of extras and members without changing recorded dependency state
func venvIsCurrent(ctx context.Context, extras []string, plugins PluginInput, projectRoot string) (bool, error) {
	root := RepoRoot()
	var venv VenvState
	var err error
	if projectRoot == "" {
		venv, err = venvPackage()
	} else {
		root = absoluteUnresolved(projectRoot)
		venv, err = asVenvState(NewVenv(root))
	}
	if err != nil {
		return false, err
	}
	runtimeFacts, err := NewFacts(EnvironmentRuntimeFactsPath(ctx, root), true)
	if err != nil {
		return false, err
	}
	value := runtimeFacts.packages["venv"]
	if value == nil {
		factsPath, err := FactsPath(ctx)
		if err != nil {
			return false, err
		}
		shipped, err := NewFacts(factsPath, true)
		if err != nil {
			return false, err
		}
		value = shipped.packages["venv"]
	}
	if value == nil {
		return false, nil
	}
	fact, recorded, err := recordedSelection(value)
	if err != nil {
		return false, err
	}
	carried, err := CarriedExtras(ctx, venv, recorded)
	if err != nil {
		return false, err
	}
	pluginDirs, err := memberInputs(ctx, plugins)
	if err != nil {
		return false, err
	}
	stamp, err := venv.Stamp(ctx, sortedUnion(carried, extras), pluginDirs)
	if err != nil {
		return false, err
	}
	return runtimeStateMatches(ctx, fact, stamp, root)
}

// Refuses extras the platform or a frozen bundle cannot carry and returns the shipped and frozen lists
func featurePolicy(ctx context.Context, extras []string, repair bool) (shipped, frozen []string, err error) {
	if len(extras) > 0 {
		var unsupported []string
		for _, extra := range extras {
			supported, err := extraSupported(ctx, extra, nil, func(string) bool { return false })
			if err != nil {
				return nil, nil, err
			}
			if !supported {
				unsupported = append(unsupported, extra)
			}
		}
		if len(unsupported) > 0 {
			return nil, nil, NewInstallError("venv", fmt.Sprintf("extras %s are not supported by this Python/platform", pyrepr.Repr(unsupported)),
				"choose a supported provider; no dependency environment was changed")
		}
	}
	shipped, err = readFeatures(ctx)
	if err != nil {
		return nil, nil, err
	}
	frozen = shipped
	if frozen != nil && !repair && LazyInstallsAllowedFor(ctx) {
		frozen = nil
	}
	if frozen != nil && len(extras) > 0 {
		allowed := map[string]bool{}
		for _, extra := range frozen {
			allowed[extra] = true
		}
		var outside []string
		for _, extra := range sortedUnion(extras) {
			if !allowed[extra] {
				outside = append(outside, extra)
			}
		}
		if len(outside) > 0 {
			return nil, nil, RefuseLazyFor(ctx, "venv", fmt.Sprintf(
				"extras %s are outside this bundle's frozen feature set (security.allow_lazy_installs is false)", pyrepr.Repr(outside)))
		}
	}
	return shipped, frozen, nil
}

// Holds the dependency lock, an impatient caller refusing instead of queueing behind a rebuild
func venvInstallLock(ctx context.Context, patient bool) (func(), error) {
	timeout := InstallLockTimeout
	if patient {
		timeout = WaitForever
	}
	held, release, err := venvRuntimeLock(ctx, RepoRoot(), timeout)
	if err != nil {
		return nil, err
	}
	if !held {
		release()
		refusal := NewInstallError("venv",
			fmt.Sprintf("another icebath process is installing dependencies (waited %.0fs)", InstallLockTimeout.Seconds()),
			"retry in a moment, or run `icebath pm install` to install explicitly")
		RecordRefusal(ctx, "install-busy", refusal.Error())
		return nil, refusal
	}
	return release, nil
}

// Snapshots a plugin state change under the held lock, nil for member only inputs
func publication(ctx context.Context, plugins PluginInput) (pluginChange, error) {
	switch p := plugins.(type) {
	case *Selection:
		change, err := newPluginSelection(ctx, copyJSONMap(p.Data))
		if err != nil {
			return nil, err
		}
		return change, nil
	case *StagedUpdate:
		change, err := newStagedPlugin(ctx, copyJSONMap(p.Data))
		if err != nil {
			return nil, err
		}
		return change, nil
	}
	return nil, nil
}

// Shallow copy of a JSON object the way dict copies a mapping
func copyJSONMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Members a plugin change implies for the venv package
func changeMembers(change pluginChange) []MemberDir {
	var members []MemberDir
	switch c := change.(type) {
	case *PluginSelection:
		members = c.Members
	case *StagedPlugin:
		members = c.Members
	}
	if members == nil {
		return []MemberDir{}
	}
	return members
}

// Recovers an interrupted publication and returns the failure that interrupted it
func recoverAfter(ctx context.Context, failure error) error {
	if err := recoverPublication(ctx, RepoRoot()); err != nil {
		return err
	}
	return failure
}

// Recovers an interrupted publication when a panic unwinds through it, then keeps unwinding
func recoverOnPanic(ctx context.Context) {
	if r := recover(); r != nil {
		if err := recoverPublication(ctx, RepoRoot()); err != nil {
			panic(err)
		}
		panic(r)
	}
}

// Receipt detail of a panic unwinding through a sync, its class and text
func panicDetail(r any) string {
	if err, ok := r.(error); ok {
		return errorClass(err) + ": " + err.Error()
	}
	return fmt.Sprintf("panic: %v", r)
}

// Publishes a disabled plugin's code without touching the dependency environment
func publishInactive(ctx context.Context, change *StagedPlugin) error {
	defer recoverOnPanic(ctx)
	err := change.Publish(ctx, RepoRoot())
	if err == nil {
		err = finishPublication(ctx, RepoRoot())
	}
	if err != nil {
		return recoverAfter(ctx, err)
	}
	RecordVenvRebuild(ctx, false, "inactive plugin")
	return nil
}

// Enabled extras, expected stamp and package inputs a sync must reach
func targetSelection(ctx context.Context, p VenvState, fact map[string]any, extras []string, inputs VenvApplyOptions, repair bool, shipped, frozen []string) ([]string, string, VenvApplyOptions, error) {
	if repair {
		if len(fact) > 0 {
			if _, _, err := recordedSelection(fact); err != nil {
				return nil, "", VenvApplyOptions{}, NewInstallError("venv", "recorded dependency selection is incomplete; refusing to change its graph", "")
			}
		}
		var enabled []string
		if raw, present := fact["extras"]; present {
			list, err := pyIterStrings(raw)
			if err != nil {
				return nil, "", VenvApplyOptions{}, err
			}
			enabled = list
		} else if frozen != nil {
			enabled = append([]string{}, frozen...)
		} else {
			enabled = []string{"all"}
		}
		stamp, _ := fact["stamp"].(string)
		if stamp == "" {
			computed, err := p.Stamp(ctx, enabled, []MemberDir{})
			if err != nil {
				return nil, "", VenvApplyOptions{}, err
			}
			stamp = computed
		}
		return enabled, stamp, VenvApplyOptions{Repair: true}, nil
	}
	recorded := shipped
	if raw, present := fact["extras"]; present {
		list, err := pyIterStrings(raw)
		if err != nil {
			return nil, "", VenvApplyOptions{}, err
		}
		recorded = list
	}
	carried, err := CarriedExtras(ctx, p, recorded)
	if err != nil {
		return nil, "", VenvApplyOptions{}, err
	}
	enabled := sortedUnion(carried, extras)
	stamp, err := p.Stamp(ctx, enabled, inputs.PluginDirs)
	if err != nil {
		return nil, "", VenvApplyOptions{}, err
	}
	return enabled, stamp, inputs, nil
}

// SelectionCommit is what a sync records, the extras, stamp and inputs it built, and how it was asked
type SelectionCommit struct {
	Enabled              []string
	Stamp                string
	Inputs               VenvApplyOptions
	Current              bool
	Repair               bool
	Explicit             bool
	SkipInvalidSecondary bool
}

// Builds unless current, publishes the plugin change, then records the selection, recovering on any failure
func commitSelection(ctx context.Context, p VenvState, facts *Facts, change pluginChange, c SelectionCommit) error {
	err := func() error {
		defer recoverOnPanic(ctx)
		generation := &VenvGeneration{}
		if !c.Current {
			opts := c.Inputs
			opts.Explicit, opts.SkipInvalidSecondary = c.Explicit, c.SkipInvalidSecondary
			built, err := p.Prepare(ctx, c.Enabled, opts)
			if err != nil {
				return err
			}
			if built != nil {
				generation = built
			}
		}
		if !c.Repair {
			stamp, err := p.Stamp(ctx, c.Enabled, c.Inputs.PluginDirs)
			if err != nil {
				return err
			}
			if stamp != c.Stamp {
				return &PyError{Class: "ValueError", Message: "Dependency inputs changed while preparing publication; retry."}
			}
		}
		if change != nil {
			if err := change.Publish(ctx, RepoRoot()); err != nil {
				return err
			}
		}
		if !c.Current {
			if generation.Environment != "" {
				if err := flushBeforeSelecting(); err != nil {
					return err
				}
			}
			if err := recordVenvState(facts, "venv", c.Stamp, c.Enabled, generation.Environment, generation.ResolvedLock); err != nil {
				return err
			}
		}
		if change != nil {
			if err := finishPublication(ctx, RepoRoot()); err != nil {
				return err
			}
		}
		reason := ""
		if c.Current {
			reason = "already in sync"
		}
		RecordVenvRebuild(ctx, !c.Current, reason)
		return nil
	}()
	if err != nil {
		return recoverAfter(ctx, err)
	}
	return nil
}

// Refuses a lazy install that would re-add an extra the user dropped
func refuseDeclined(ctx context.Context, p VenvState, extras []string, explicit bool) error {
	if len(extras) == 0 || explicit {
		return nil
	}
	refused, err := declinedExtras(ctx, p.ProjectRoot())
	if err != nil {
		return err
	}
	var dropped []string
	for _, extra := range extras {
		if refused[ExtraKey(extra)] {
			dropped = append(dropped, extra)
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	reasons := declinedReasons(ctx, p.ProjectRoot())
	var why []string
	for _, extra := range dropped {
		if reason, ok := reasons[ExtraKey(extra)]; ok {
			why = append(why, fmt.Sprintf("an update left %s out because %s", pyrepr.StrRepr(extra), reason))
		}
	}
	cause := strings.Join(why, "; ")
	if cause == "" {
		cause = fmt.Sprintf("extras %s were removed with `icebath pm install --without`", pyrepr.Repr(dropped))
	}
	hints := make([]string, len(dropped))
	for i, extra := range dropped {
		hints[i] = InstallHint(extra)
	}
	return NewInstallError("venv", cause, "run `"+strings.Join(hints, "`, `")+"` to enable them again")
}

// Extras minus the automatic entries the user has declined, read under the dependency lock
func withoutNewDeclines(ctx context.Context, p VenvState, extras, automatic []string) ([]string, error) {
	if len(extras) == 0 || len(automatic) == 0 {
		return extras, nil
	}
	refused, err := declinedExtras(ctx, p.ProjectRoot())
	if err != nil {
		return nil, err
	}
	derived := map[string]bool{}
	for _, extra := range automatic {
		derived[ExtraKey(extra)] = true
	}
	out := []string{}
	for _, extra := range extras {
		if key := ExtraKey(extra); !derived[key] || !refused[key] {
			out = append(out, extra)
		}
	}
	return out, nil
}

// Records install without for extras under the dependency lock so an in flight opt in cannot erase it
func DeclineExtras(ctx context.Context, names []string) error {
	if len(names) == 0 {
		return nil
	}
	release, err := dependencyLock(ctx, true)
	if err != nil {
		return err
	}
	defer release()
	_, err = recordDeclines(ctx, names, nil, nil, RepoRoot())
	return err
}

// Venv sync state fixed before the dependency lock is taken
type venvSync struct {
	pkg      VenvState
	plugins  PluginInput
	repair   bool
	explicit bool
	shipped  []string
	frozen   []string
	evict    bool
}

// Body of a venv sync under the held dependency lock
func (s *venvSync) locked(ctx context.Context, extras []string) error {
	change, err := publication(ctx, s.plugins)
	if err != nil {
		return err
	}
	if staged, ok := change.(*StagedPlugin); ok && !staged.Active {
		return publishInactive(ctx, staged)
	}
	if s.evict {
		facts, err := NewFacts(RuntimeFactsPath(ctx), false)
		if err != nil {
			return err
		}
		fact, err := recordedVenvFact(ctx, facts)
		if err != nil {
			return err
		}
		return syncEvicting(ctx, s.pkg, facts, fact, EvictingSync{Extras: extras, Shipped: s.shipped, Frozen: s.frozen, Explicit: s.explicit})
	}
	var inputs VenvApplyOptions
	if change != nil {
		inputs.PluginDirs = changeMembers(change)
	} else if inputs.PluginDirs, err = memberInputs(ctx, s.plugins); err != nil {
		return err
	}
	facts, err := NewFacts(RuntimeFactsPath(ctx), s.repair)
	if err != nil {
		return err
	}
	fact, err := recordedVenvFact(ctx, facts)
	if err != nil {
		return err
	}
	enabled, stamp, inputs, err := selectTarget(ctx, s.pkg, fact, extras, inputs, s.repair, s.shipped, s.frozen)
	if err != nil {
		return err
	}
	current := false
	if !s.repair {
		if current, err = runtimeStateMatches(ctx, fact, stamp, ""); err != nil {
			return err
		}
	}
	if !current && !s.repair && !s.explicit && !LazyInstallsAllowedFor(ctx) {
		what := "venv out of sync"
		if len(extras) > 0 {
			what = pyrepr.Repr(extras)
		}
		return RefuseLazyFor(ctx, "venv", what)
	}
	RecordFeatureList(ctx, enabled)
	return commitSelection(ctx, s.pkg, facts, change, SelectionCommit{
		Enabled: enabled, Stamp: stamp, Inputs: inputs, Current: current, Repair: s.repair, Explicit: s.explicit,
	})
}

// SyncVenvOptions select the extras, plugin source, repair and eviction policy and the output of a venv sync
type SyncVenvOptions struct {
	Extras                   []string
	Explicit                 bool
	Repair                   bool
	Automatic                []string
	Carried                  []string
	ProjectRoot              string
	EvictIncompatiblePlugins bool
	Plugins                  PluginInput
	Stdout                   io.Writer
}

// Reports every string of sub present in super
func subsetOf(sub, super []string) bool {
	for _, item := range sub {
		if !slices.Contains(super, item) {
			return false
		}
	}
	return true
}

// Makes the dependency environment match the lock and the enabled extras, always writing a receipt
func syncVenv(ctx context.Context, opts SyncVenvOptions) error {
	ctx = withSyncOutput(ctx, opts.Stdout)
	token := BeginReceipt(ctx, "sync")
	outcome := "failed"
	defer func() {
		code := 1
		if outcome == "ok" {
			code = 0
		}
		FinalizeReceipt(ctx, outcome, code, token)
	}()
	defer func() {
		if r := recover(); r != nil {
			RecordStep(ctx, "dependency-sync", false, panicDetail(r))
			panic(r)
		}
	}()
	if err := syncVenvBody(ctx, opts); err != nil {
		RecordStep(ctx, "dependency-sync", false, errorClass(err)+": "+err.Error())
		return err
	}
	outcome = "ok"
	return nil
}

// Validates a sync request, takes the dependency lock and reaches the selection, leaving out failing carried extras
func syncVenvBody(ctx context.Context, opts SyncVenvOptions) error {
	if opts.Repair && (opts.Extras != nil || opts.Plugins != nil) {
		return &PyError{Class: "ValueError", Message: "repair restores the recorded environment; it cannot change features or plugins"}
	}
	if opts.EvictIncompatiblePlugins && (opts.Repair || opts.Plugins != nil || !opts.Explicit) {
		return &PyError{Class: "ValueError", Message: "only an explicit sync of the discovered plugin selection may disable plugins"}
	}
	if len(opts.Carried) > 0 && (opts.Repair || opts.Plugins != nil || !subsetOf(opts.Carried, opts.Extras)) {
		return &PyError{Class: "ValueError", Message: "carried extras belong to a feature sync of the discovered selection"}
	}
	shipped, frozen, err := syncFeaturePolicy(ctx, opts.Extras, opts.Repair)
	if err != nil {
		return err
	}
	venv, err := venvPackage()
	if err != nil {
		return err
	}
	sync := &venvSync{pkg: venv, plugins: opts.Plugins, repair: opts.Repair, explicit: opts.Explicit,
		shipped: shipped, frozen: frozen, evict: opts.EvictIncompatiblePlugins}
	automatic := append(append([]string{}, opts.Automatic...), opts.Carried...)
	release, err := dependencyLock(ctx, opts.Explicit || opts.Repair)
	if err != nil {
		return err
	}
	defer release()
	extras, err := withoutNewDeclines(ctx, venv, opts.Extras, automatic)
	if err != nil {
		return err
	}
	if err := refuseDeclined(ctx, venv, extras, opts.Explicit); err != nil {
		return err
	}
	if err := recoverPublication(ctx, RepoRoot()); err != nil {
		return err
	}
	if failure := sync.locked(ctx, extras); failure != nil {
		if len(opts.Carried) == 0 || len(extras) == 0 || !isBuildVerdict(failure) {
			return failure
		}
		runtimeFacts, err := NewFacts(RuntimeFactsPath(ctx), false)
		if err != nil {
			return err
		}
		fact, err := recordedVenvFact(ctx, runtimeFacts)
		if err != nil {
			return err
		}
		failing, err := failingCarried(ctx, venv, failure, CarriedTrial{
			Extras: extras, Carried: opts.Carried, Fact: fact, Shipped: shipped, Frozen: frozen, Explicit: opts.Explicit,
		})
		if err != nil {
			return err
		}
		if err := refuseTransient(failing, failure); err != nil {
			return err
		}
		kept := []string{}
		for _, extra := range extras {
			if _, failed := failing[extra]; !failed {
				kept = append(kept, extra)
			}
		}
		extras = kept
		if err := sync.locked(ctx, extras); err != nil {
			return err
		}
		if err := recordLeftOut(ctx, venv, failing); err != nil {
			return err
		}
	}
	derived := map[string]bool{}
	for _, extra := range automatic {
		derived[ExtraKey(extra)] = true
	}
	var optedIn []string
	for _, extra := range extras {
		if !derived[ExtraKey(extra)] {
			optedIn = append(optedIn, extra)
		}
	}
	if len(optedIn) > 0 && opts.Explicit && !opts.Repair {
		if _, err := recordDeclines(ctx, nil, optedIn, nil, venv.ProjectRoot()); err != nil {
			return err
		}
	}
	return nil
}

// Records the committed venv selection in the runtime facts, replaced by crash tests to stop after the commit
var recordVenvState = (*Facts).RecordState

// Takes the per install dependency lock for a venv sync, replaced by tests that observe the wait
var venvRuntimeLock = RuntimeLock

// Dependency lock, sync selection, decline record and feature policy steps, replaced by tests that interleave syncs
var (
	dependencyLock    = venvInstallLock
	selectTarget      = targetSelection
	recordDeclines    = recordDeclinedExtras
	syncFeaturePolicy = featurePolicy
)
