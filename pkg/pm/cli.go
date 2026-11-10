package pm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// LockArgs are pm lock's arguments, a nil Name or Version when not given
type LockArgs struct {
	Name    *string
	Version *string
}

// InstallArgs are pm install's arguments, a nil Target or TestEnvironment when not given
type InstallArgs struct {
	Names           []string
	Extra           []string
	Without         []string
	ToolsOnly       bool
	TrustRecorded   bool
	TestEnvironment *string
	Target          *string
}

// EnvironmentArgs are pm environment's arguments
type EnvironmentArgs struct {
	Name        string
	Extra       []string
	Requirement []string
}

// BundleArgs are pm bundle's arguments, an empty Ref or Cache when not given
type BundleArgs struct {
	Out   string
	Ref   string
	Cache string
}

type cliStderrKey struct{}

// Context whose pm command errors print to w instead of the process stderr
func WithCLIStderr(ctx context.Context, w io.Writer) context.Context {
	return context.WithValue(ctx, cliStderrKey{}, w)
}

// Writer pm commands print their errors to
func cliStderr(ctx context.Context) io.Writer {
	if w, ok := ctx.Value(cliStderrKey{}).(io.Writer); ok {
		return w
	}
	return os.Stderr
}

// Argv after the executable reaching the hidden pm worker command
func pmWorkerArgs() []string { return []string{"pm", "worker"} }

// Relocks uv.lock with no name, otherwise pins a tool's artifacts in lock.json
func CmdLock(ctx context.Context, args LockArgs) (int, error) {
	if args.Name == nil {
		return relockProject(ctx, RepoRoot())
	}
	return ReuseIndexResponses(func() (int, error) { return pinTool(ctx, args) })
}

// Refreshes uv.lock from pyproject.toml when stale and prints the follow up steps
func relockProject(ctx context.Context, root string) (int, error) {
	out := SyncOutput(ctx)
	fmt.Fprintln(out, "→ Checking uv.lock…")
	err := pmCheckProjectLock(ctx, CheckProjectLockOptions{Source: root, Explicit: true, Quiet: true})
	var install *InstallError
	switch {
	case err == nil:
		fmt.Fprintln(out, "✓ uv.lock is already current with pyproject.toml; nothing written")
		return 0, nil
	case errors.As(err, &install):
		fmt.Fprintln(out, "uv.lock is out of date with pyproject.toml; relocking")
	default:
		return 0, err
	}
	before, err := lockedExtras(root)
	if err != nil {
		return 0, err
	}
	if err := pmLockProject(ctx, LockProjectOptions{Source: root, Explicit: true}); err != nil {
		if errors.As(err, &install) {
			fmt.Fprintf(out, "✗ uv.lock refresh failed: %s\n", err)
			return 1, nil
		}
		return 0, err
	}
	fmt.Fprintln(out, "✓ uv.lock updated from pyproject.toml")
	windows := runtime.GOOS == "windows"
	activate := "source ./activate"
	if windows {
		activate = `. .\activate.ps1`
	}
	fmt.Fprintf(out, "Next: re-source activation to sync the environments: %s\n", activate)
	optIn, err := newOptInExtras(root, before)
	if err != nil {
		return 0, err
	}
	if len(optIn) > 0 {
		names := strings.Join(append([]string{"all"}, optIn...), ",")
		flag := "--test-extras " + names
		if windows {
			flag = "-TestExtras '" + names + "'"
		}
		fmt.Fprintf(out, "New extras outside [all] (%s) need: %s %s\n", strings.Join(optIn, ", "), activate, flag)
	}
	fmt.Fprintln(out, "Then commit pyproject.toml and uv.lock together.")
	return 0, nil
}

// Extras the current uv.lock resolves for the root project, empty when either file is unreadable
func lockedExtras(root string) (map[string]bool, error) {
	readTOML := func(name string) (*OrderedObject, bool) {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || !utf8.Valid(data) {
			return nil, false
		}
		doc, err := tomlLoads(string(data))
		return doc, err == nil
	}
	lock, ok := readTOML("uv.lock")
	if !ok {
		return map[string]bool{}, nil
	}
	pyproject, ok := readTOML("pyproject.toml")
	if !ok {
		return map[string]bool{}, nil
	}
	project, err := tomlIndex(pyproject, "project")
	var name any
	if err == nil {
		name, err = tomlIndex(project, "name")
	}
	if err != nil {
		if errorClass(err) == "KeyError" {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	packages, _ := tomlGet(lock, "package", []any{})
	rows, err := tomlIter(packages)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		got, err := tomlGet(row, "name", nil)
		if err != nil {
			return nil, err
		}
		if !tomlEqual(got, name) {
			continue
		}
		metadata, err := tomlGet(row, "metadata", NewOrderedObject())
		if err != nil {
			return nil, err
		}
		provides, err := tomlGet(metadata, "provides-extras", []any{})
		if err != nil {
			return nil, err
		}
		items, err := tomlIter(provides)
		if err != nil {
			return nil, err
		}
		extras := map[string]bool{}
		for _, item := range items {
			if err := tomlUnhashable(item); err != nil {
				return nil, err
			}
			if extra, ok := item.(string); ok {
				extras[extra] = true
			}
		}
		return extras, nil
	}
	return map[string]bool{}, nil
}

// Extras this relock introduced that the default all closure does not reach
func newOptInExtras(root string, before map[string]bool) ([]string, error) {
	droppable, err := DroppableExtras(root)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, extra := range droppable {
		if !before[extra] && !seen[extra] {
			seen[extra] = true
			out = append(out, extra)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Resolves and hashes every target's archives for a tool version and writes the pin
func pinTool(ctx context.Context, args LockArgs) (int, error) {
	out := SyncOutput(ctx)
	lockfile := updateLockfile()
	name, version := *args.Name, *args.Version
	p, err := GetPackage(name)
	if err != nil {
		return 0, err
	}
	pin := func(url string) (map[string]any, error) {
		fmt.Fprintf(out, "    %s\n", url)
		digest, err := p.KnownSHA256(version, url)
		if err != nil {
			return nil, err
		}
		if digest == "" {
			if digest, err = updateHashURL(url); err != nil {
				return nil, err
			}
		}
		fmt.Fprintf(out, "      sha256 %s\n", digest)
		return map[string]any{"url": url, "sha256": digest}, nil
	}
	pinAll := func(urls []string) (any, error) {
		pinned := []any{}
		for _, url := range urls {
			entry, err := pin(url)
			if err != nil {
				return nil, err
			}
			pinned = append(pinned, entry)
		}
		if len(pinned) == 1 {
			return pinned[0], nil
		}
		return pinned, nil
	}
	var targets []string
	urls := map[string][]string{}
	distinct := map[string]bool{}
	for _, target := range AllTargets {
		if p.MissingReason(target) != "" {
			continue
		}
		list, err := p.FetchURLs(version, target)
		if err != nil {
			return 0, err
		}
		targets = append(targets, target)
		urls[target] = list
		distinct[fmt.Sprintf("%q", list)] = true
	}
	artifacts := map[string]any{}
	if len(distinct) == 1 {
		fmt.Fprintln(out, "  any:")
		pinned, err := pinAll(urls[targets[0]])
		if err != nil {
			return 0, err
		}
		artifacts["any"] = pinned
	} else {
		for _, target := range targets {
			fmt.Fprintf(out, "  %s:\n", target)
			pinned, err := pinAll(urls[target])
			if err != nil {
				return 0, err
			}
			artifacts[target] = pinned
		}
	}
	lockfile.SetPin(name, version, artifacts)
	if err := lockfile.Save(); err != nil {
		return 0, err
	}
	fmt.Fprintf(out, "pinned %s %s (%d targets)\n", name, version, len(artifacts))
	return regenerateBootstrapPins(ctx, lockfile)
}

// Bytes as MiB with one decimal
func fmtBytes(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024)) }

// Seams the progress renderer reads, replaced by tests the way Python patches the cli module
var (
	enableVT         = enableVTProcessing
	streamIsTerminal = func(w io.Writer) bool {
		f, ok := w.(*os.File)
		return ok && f != nil && fileIsTTY(f)
	}
	stderrStream   = func() io.Writer { return os.Stderr }
	progressStream = defaultProgressStream
)

// Stream for in place progress, stdout first then stderr, nil off a terminal
func defaultProgressStream(ctx context.Context) io.Writer {
	for _, stream := range []io.Writer{SyncOutput(ctx), stderrStream()} {
		if streamIsTerminal(stream) && enableVT(stream) {
			return stream
		}
	}
	return nil
}

// Reports whether stdout or stderr is a terminal that renders progress
func interactive(ctx context.Context) bool { return progressStream(ctx) != nil }

// Per package progress redrawn in place on a terminal and printed as throttled lines off one
type liveProgress struct {
	name     string
	stream   io.Writer
	out      io.Writer
	last     int64
	lastTime time.Time
}

// Progress reporter for one package's install
func newLiveProgress(ctx context.Context, name string) *liveProgress {
	return &liveProgress{name: name, stream: progressStream(ctx), out: SyncOutput(ctx)}
}

// Redraws the progress line on a terminal or prints it as its own line
func (p *liveProgress) write(line string) {
	if p.stream != nil {
		fmt.Fprint(p.stream, "\r\x1b[2K"+line)
		return
	}
	fmt.Fprintln(p.out, line)
}

// Returns the cursor to a clean line so the caller's status glyph starts its own row
func (p *liveProgress) finish() {
	if p.stream != nil {
		fmt.Fprint(p.stream, "\r\x1b[2K")
	}
}

// Renders an unpack phase line or a download percentage throttled to ten per second or 4 MiB steps
func (p *liveProgress) report(stage string, done, total int64, label string) error {
	if stage == "unpack" {
		p.last = 0
		p.lastTime = time.Time{}
		suffix := ""
		if label != "" {
			suffix = " " + label
		}
		p.write(fmt.Sprintf("  %s: unpacking%s", p.name, suffix))
		return nil
	}
	if total <= 0 {
		return nil
	}
	if done < total {
		if p.stream != nil {
			now := time.Now()
			if !p.lastTime.IsZero() && now.Sub(p.lastTime) < 100*time.Millisecond {
				return nil
			}
			p.lastTime = now
		} else if done-p.last < 4*1024*1024 {
			return nil
		}
	}
	p.last = done
	p.write(fmt.Sprintf("  %s: %5.1f%%  %s / %s", p.name, float64(done)/float64(total)*100, fmtBytes(done), fmtBytes(total)))
	return nil
}

// Install steps of the install command, replaced by tests the way Python patches the cli and install modules
var (
	cliInstallNames    = installNamesFor
	cliInstallDefaults = installDefaults
	installActivate    = Activate
	installSyncVenv    func(ctx context.Context, opts SyncVenvOptions) error
)

// Binds the venv sync step at init because ensure reaches it through the same seam
func init() { installSyncVenv = syncVenv }

// Facades of the pm package the commands call, replaced by tests the way Python patches the pm module
var (
	pmCheckProjectLock  = CheckProjectLock
	pmLockProject       = LockProject
	pmEnsureEnvironment = EnsureEnvironment
)

// InstallCommandSteps are the tool install, PATH activation and venv sync steps the install command runs
type InstallCommandSteps struct {
	InstallNames func(ctx context.Context, names []string, target *string, verify bool) (int, error)
	Activate     func(ctx context.Context, allowIncomplete bool) ([]string, error)
	SyncVenv     func(ctx context.Context, opts SyncVenvOptions) error
}

// Replaces the install command steps until the returned restore runs
func SwapInstallCommandSteps(steps InstallCommandSteps) func() {
	names, activate, sync := cliInstallNames, installActivate, installSyncVenv
	cliInstallNames, installActivate, installSyncVenv = steps.InstallNames, steps.Activate, steps.SyncVenv
	return func() { cliInstallNames, installActivate, installSyncVenv = names, activate, sync }
}

// uncaughtError carries an exception the int returning installNames cannot return up to Main
type uncaughtError struct{ err error }

// Installs or cross stages names and returns the failure count, other errors unwinding to Main
func installNames(ctx context.Context, names []string, target string, verify bool) int {
	var cross *string
	if target != "" {
		cross = &target
	}
	failed, err := installNamesFor(ctx, names, cross, verify)
	if err != nil {
		panic(uncaughtError{err})
	}
	return failed
}

// Installs names under one install operation, or stages them for a cross target, counting install failures
func installNamesFor(ctx context.Context, names []string, target *string, verify bool) (int, error) {
	out := SyncOutput(ctx)
	failed := 0
	operation := newInstallOperation(ctx)
	defer operation.Close()
	for _, name := range names {
		progress := newLiveProgress(ctx, name)
		var err error
		if target != nil {
			var entry string
			if entry, err = stageOnly(ctx, name, *target, StageOnlyOptions{}); err == nil {
				fmt.Fprintf(out, "✓ %s (staged for %s: %s)\n", name, *target, filepath.Base(entry))
			}
		} else {
			_, err = ensure(ctx, name, EnsureOptions{Explicit: true, SkipVerify: !verify, Progress: progress.report, operation: operation})
			if err == nil && name == "python" {
				err = publishLaunchers(ctx, RepoRoot(), false)
			}
			if err == nil {
				progress.finish()
				fmt.Fprintf(out, "✓ %s\n", name)
			}
		}
		if err != nil {
			var install *InstallError
			if !errors.As(err, &install) {
				return failed, err
			}
			progress.finish()
			fmt.Fprintf(out, "✗ %s\n", err)
			failed++
		}
	}
	return failed, nil
}

// First incompatible install flag combination as its user facing message, empty when none
func installFlagError(args InstallArgs, extras []string, crossTarget string, toolsOnly, trustRecorded bool, testEnvironment *string) (string, error) {
	names := len(args.Names) > 0
	if crossTarget != "" {
		if !containsString(AllTargets, crossTarget) {
			return fmt.Sprintf("unknown target %s; known: %s", pyrepr.StrRepr(crossTarget), strings.Join(AllTargets, ", ")), nil
		}
		if !names {
			return "--target requires explicit package names", nil
		}
	}
	if testEnvironment != nil && (len(extras) > 0 || crossTarget != "" || names || toolsOnly) {
		return "--test-environment builds beside the default closure; it does not take names, --extra, --target, or --tools-only", nil
	}
	if toolsOnly && (len(extras) > 0 || crossTarget != "" || names) {
		return "--tools-only installs the tool closure and then stops; it does not take names, --extra, or --target", nil
	}
	if trustRecorded && (len(extras) > 0 || crossTarget != "" || names) {
		return "--trust-recorded installs the default closure and then stops; it does not take names, --extra, or --target", nil
	}
	if len(extras) > 0 && crossTarget != "" {
		return "--extra syncs this install's venv and cannot combine with --target", nil
	}
	if len(args.Without) > 0 && (len(extras) > 0 || crossTarget != "" || names) {
		return "--without shapes the default closure; it does not take names, --extra, or --target", nil
	}
	if len(args.Without) > 0 {
		_, _, unknown, err := splitWithout(args.Without)
		if err != nil {
			return "", err
		}
		if len(unknown) > 0 {
			droppable, err := DroppableExtras(RepoRoot())
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("--without accepts only optional default packages (%s) or extras outside [all] (%s); not %s",
				strings.Join(DefaultPackageNames(), ", "), strings.Join(droppable, ", "), strings.Join(unknown, ", ")), nil
		}
	}
	return "", nil
}

// Sorts without names into default packages, droppable extras and unknown names
func splitWithout(names []string) (packages, extras, unknown []string, err error) {
	droppable, err := DroppableExtras(RepoRoot())
	if err != nil {
		return nil, nil, nil, err
	}
	defaults := DefaultPackageNames()
	for _, name := range names {
		switch p := CanonicalDefaultName(name); {
		case containsString(defaults, p):
			packages = append(packages, p)
		case containsString(droppable, name):
			extras = append(extras, name)
		default:
			unknown = append(unknown, name)
		}
	}
	return packages, extras, unknown, nil
}

// Installs the optional defaults one by one, a failure warning without failing the install
func installDefaults(ctx context.Context, names []string, verify bool) error {
	for _, name := range names {
		failed, err := cliInstallNames(ctx, []string{name}, nil, verify)
		if err != nil {
			return err
		}
		if failed != 0 {
			fmt.Fprintf(SyncOutput(ctx), "⚠ optional %s was not installed; its tools stay unavailable until `icebath pm install %s` succeeds\n", name, name)
		}
	}
	return nil
}

// Extras this install's sync requests and the legacy ones it carries before PM recorded a selection
func firstSelection(ctx context.Context, extras []string) (requested, carried []string, err error) {
	requested = extras
	if len(requested) == 0 {
		requested = []string{"all"}
	}
	if isRegularFile(EnvironmentRuntimeFactsPath(ctx, RepoRoot())) {
		return requested, []string{}, nil
	}
	legacy, err := LegacySelection(ctx, RepoRoot())
	if err != nil {
		return nil, nil, err
	}
	if len(legacy) == 1 && legacy[0] == "all" {
		return requested, []string{}, nil
	}
	union := map[string]bool{}
	for _, extra := range append(append([]string{}, legacy...), extras...) {
		union[extra] = true
	}
	asked := map[string]bool{}
	for _, extra := range requested {
		asked[extra] = true
	}
	selection, left := []string{}, []string{}
	for extra := range union {
		selection = append(selection, extra)
	}
	for _, extra := range legacy {
		if !asked[extra] && !containsString(left, extra) {
			left = append(left, extra)
		}
	}
	sort.Strings(selection)
	sort.Strings(left)
	return selection, left, nil
}

// Syncs the venv and builds the side test environment, returning the failure count
func installPythonEnvironments(ctx context.Context, extras []string, sync bool, testEnvironment *string) (int, error) {
	out := SyncOutput(ctx)
	failed := 0
	var install *InstallError
	if sync {
		selection, carried, err := firstSelection(ctx, extras)
		if err == nil {
			opts := SyncVenvOptions{Extras: selection, Explicit: true}
			if len(carried) > 0 {
				opts.Automatic = carried
			}
			err = installSyncVenv(ctx, opts)
		}
		switch {
		case err == nil:
			suffix := ""
			if len(extras) > 0 {
				suffix = " +" + strings.Join(extras, " +")
			}
			fmt.Fprintf(out, "✓ venv%s\n", suffix)
		case errors.As(err, &install):
			fmt.Fprintf(out, "✗ %s\n", err)
			failed++
		default:
			return failed, err
		}
	}
	if testEnvironment != nil && failed == 0 {
		err := pmCheckProjectLock(ctx, CheckProjectLockOptions{Source: RepoRoot(), Explicit: true})
		if err == nil {
			_, err = EnsureTestenv(ctx, RepoRoot(), ParseTestExtras(*testEnvironment))
		}
		switch {
		case err == nil:
			fmt.Fprintln(out, "✓ test environment")
		case errors.As(err, &install):
			fmt.Fprintf(out, "✗ %s\n", err)
			failed++
		default:
			return failed, err
		}
	}
	return failed, nil
}

// Installs the tool closure then the venv, the optional defaults last, recording activation inputs for a full closure
func CmdInstall(ctx context.Context, args InstallArgs) (int, error) {
	out := SyncOutput(ctx)
	canonical := make([]string, 0, len(args.Names))
	for _, name := range args.Names {
		canonical = append(canonical, CanonicalDefaultName(name))
	}
	args.Names = canonical
	crossTarget := ""
	if args.Target != nil {
		crossTarget = *args.Target
	}
	extras := dedupeStrings(args.Extra)
	errText, err := installFlagError(args, extras, crossTarget, args.ToolsOnly, args.TrustRecorded, args.TestEnvironment)
	if err != nil {
		return 0, err
	}
	if errText != "" {
		fmt.Fprintf(out, "✗ %s\n", errText)
		return 1, nil
	}
	lockfile := updateLockfile()
	names := args.Names
	if len(args.Names) == 0 && len(extras) == 0 {
		if names, err = SourceInstallPackages(lockfile.Names()); err != nil {
			return 0, err
		}
	}
	if without := dedupeStrings(args.Without); len(without) > 0 {
		packages, droppedExtras, _, err := splitWithout(without)
		if err != nil {
			return 0, err
		}
		if _, err := RecordDeclined(ctx, packages, nil, ""); err != nil {
			return 0, err
		}
		if err := DeclineExtras(ctx, droppedExtras); err != nil {
			return 0, err
		}
	}
	var defaults []string
	if len(args.Names) == 0 && len(extras) == 0 {
		if defaults, err = DefaultPackages(ctx, lockfile.Names(), "", nil); err != nil {
			return 0, err
		}
	}
	fullClosure := !(len(args.Names) > 0 || args.ToolsOnly || crossTarget != "")
	inputMtimes := map[string]int64{}
	if fullClosure {
		if inputMtimes, err = ActivationInputMtimes(RepoRoot()); err != nil {
			return 0, err
		}
	}
	toolNames := names
	if len(args.Names) == 0 {
		if toolNames, err = ToolRoots(names); err != nil {
			return 0, err
		}
	}
	failedTools, err := cliInstallNames(ctx, toolNames, args.Target, !args.TrustRecorded)
	if err != nil {
		return 0, err
	}
	if failedTools != 0 {
		return 1, nil
	}
	if len(args.Names) > 0 && crossTarget == "" {
		if _, err := RecordDeclined(ctx, nil, args.Names, ""); err != nil {
			return 0, err
		}
	}
	if crossTarget == "" && (len(args.Names) == 0 || args.ToolsOnly) {
		problems, err := installActivate(ctx, true)
		if err != nil {
			return 0, err
		}
		if len(problems) > 0 {
			fmt.Fprintf(out, "✗ tools not on PATH before venv sync: %s\n", strings.Join(problems, "; "))
			return 1, nil
		}
	}
	if args.ToolsOnly {
		return 0, cliInstallDefaults(ctx, defaults, !args.TrustRecorded)
	}
	failed, err := installPythonEnvironments(ctx, extras, len(extras) > 0 || len(args.Names) == 0, args.TestEnvironment)
	if err != nil {
		return 0, err
	}
	if failed == 0 {
		if err := cliInstallDefaults(ctx, defaults, !args.TrustRecorded); err != nil {
			return 0, err
		}
	}
	if fullClosure && failed == 0 {
		if err := RecordActivationInputs(ActivationInputsDir(ctx, RepoRoot()), inputMtimes, RepoRoot(), args.TestEnvironment != nil); err != nil {
			return 0, err
		}
	}
	if failed != 0 {
		return 1, nil
	}
	return 0, nil
}

// Prints the composed package exports of names, never the caller's environment
func CmdEnv(ctx context.Context, names []string) (int, error) {
	if len(names) == 0 {
		names = updateLockfile().Names()
	}
	env, err := envFor(ctx, names, map[string]string{})
	if err != nil {
		return 0, err
	}
	data, err := dumpsJSON(env, true)
	if err != nil {
		return 0, err
	}
	fmt.Fprintln(SyncOutput(ctx), string(data))
	return 0, nil
}

// Checks every locked package's installed state, verification and digest, then the uv cache
func CmdDoctor(ctx context.Context, repair bool) (int, error) {
	out := SyncOutput(ctx)
	lockfile := updateLockfile()
	if _, err := storeFacts(ctx); err != nil {
		return 0, err
	}
	if _, err := defaultStore(ctx); err != nil {
		return 0, err
	}
	target, err := CurrentTarget()
	if err != nil {
		return 0, err
	}
	bad := 0
	for _, name := range lockfile.Names() {
		p, err := GetPackage(name)
		if err != nil {
			return 0, err
		}
		if reason := p.MissingReason(target); reason != "" {
			fmt.Fprintf(out, "- %s: n/a on %s (%s)\n", name, target, reason)
			continue
		}
		location, err := InstalledLocation(ctx, p, lockfile, target, LocationOptions{})
		if err != nil {
			return 0, err
		}
		if location == nil {
			facts, err := storeFacts(ctx)
			if err != nil {
				return 0, err
			}
			store, err := defaultStore(ctx)
			if err != nil {
				return 0, err
			}
			location = &Location{Facts: facts, Store: store}
		}
		fact, recorded := location.Facts.Get(name)
		base := p.Base()
		mark, weight := "✗", 1
		if base.Optional || base.Internal {
			mark, weight = "?", 0
		}
		identity := pinIdentity(lockfile, name, target)
		if recorded && identity != nil {
			_, hasTarget := fact["target"]
			_, hasArtifacts := fact["artifacts"]
			if !hasTarget || !hasArtifacts {
				fmt.Fprintf(out, "%s %s: legacy fact: no recorded identity, run `icebath pm install`\n", mark, name)
				bad += weight
				continue
			}
		}
		var version *string
		if v, ok := lockfile.Version(name); ok {
			version = &v
		}
		if !location.Facts.Installed(name, version, location.Store.Root, identity) {
			state := "outdated"
			if !recorded {
				state = "not installed"
			}
			fmt.Fprintf(out, "%s %s: %s\n", mark, name, state)
			bad += weight
			continue
		}
		entry := location.Store.Entry(pyrepr.Str(fact["entry"]))
		if reason := p.Verify(entry, target); reason != "" {
			fmt.Fprintf(out, "✗ %s: installed but failed verification: %s\n", name, reason)
			bad++
			continue
		}
		if digest, ok := fact["digest"]; ok && digest != nil {
			text := pyrepr.Str(digest)
			matches, err := DigestMatches(entry, &text)
			if err != nil {
				return 0, err
			}
			if !matches {
				fmt.Fprintf(out, "✗ %s: realized bytes do not match recorded digest\n", name)
				bad++
				continue
			}
		}
		fmt.Fprintf(out, "✓ %s %s\n", name, pyrepr.Str(fact["version"]))
	}
	cache, err := doctorUVCache(ctx, repair)
	if err != nil {
		return 0, err
	}
	if bad+cache != 0 {
		return 1, nil
	}
	return 0, nil
}

// Reports corrupt uv cache entries an interrupted install left, removing them with repair
func doctorUVCache(ctx context.Context, repair bool) (int, error) {
	out := SyncOutput(ctx)
	cache := UvCachePath()
	corrupt := sortedKeys(CorruptCachedPackages(cache))
	if len(corrupt) == 0 {
		fmt.Fprintf(out, "✓ uv cache: no corrupt entries in %s\n", cache)
		return 0, nil
	}
	if !repair {
		fmt.Fprintf(out, "✗ uv cache: corrupt entries for %s in %s; run `icebath pm doctor --repair` (installs also remove them when they hit one)\n",
			strings.Join(corrupt, ", "), cache)
		return 1, nil
	}
	env, err := ManagedEnvironment(ctx, cache, ManagedEnvironmentOptions{Cache: cache})
	if err == nil {
		err = env.PurgeCachedPackages(ctx, corrupt)
	}
	if err != nil {
		var install *InstallError
		if errors.As(err, &install) {
			fmt.Fprintf(out, "✗ uv cache: %s\n", err)
			return 1, nil
		}
		return 0, err
	}
	if left := sortedKeys(CorruptCachedPackages(cache)); len(left) > 0 {
		fmt.Fprintf(out, "✗ uv cache: still corrupt after cleaning: %s in %s\n", strings.Join(left, ", "), cache)
		return 1, nil
	}
	fmt.Fprintf(out, "✓ uv cache: removed corrupt entries for %s; they are fetched again on next use\n", strings.Join(corrupt, ", "))
	return 0, nil
}

// Removes every unreferenced store entry, fetch cache and expired partial, returning removed and kept counts
func gcStore(ctx context.Context, store *Store, facts *Facts) (int, int, error) {
	partials := PartialsRoot()
	if !isDirPath(store.Root) && !isDirPath(partials) {
		return 0, 0, nil
	}
	release, err := store.InstallLock()
	if err != nil {
		return 0, 0, err
	}
	defer release()
	retired := sortedKeys(func() map[string]bool {
		names := map[string]bool{}
		for name := range RetiredDefaults {
			names[name] = true
		}
		return names
	}())
	if err := facts.Forget(retired); err != nil {
		return 0, 0, err
	}
	if err := facts.Reload(); err != nil {
		return 0, 0, err
	}
	keep := facts.EntriesInUse()
	if err := CollectPartials(partials, GCGraceSeconds); err != nil {
		return 0, 0, err
	}
	removed := reclaimSetAside(store)
	items, err := os.ReadDir(store.Root)
	if err != nil {
		return 0, 0, err
	}
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name())
	}
	sortPathNames(names)
	out := SyncOutput(ctx)
	for _, name := range names {
		item := filepath.Join(store.Root, name)
		if !isDirPath(item) {
			continue
		}
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(name, ".staging-") {
			continue
		}
		if keep[name] {
			continue
		}
		fmt.Fprintf(out, "removing %s\n", name)
		_ = RemoveTree(item)
		removed++
	}
	return removed, len(keep), nil
}

// Sweeps the writable store, dependency and PM runtime generations, and orphaned install state dirs
func CmdGC(ctx context.Context) (int, error) {
	root, err := WritableStoreRoot(ctx)
	if err != nil {
		return 0, err
	}
	store := NewStore(root)
	shared, err := defaultStore(ctx)
	if err != nil {
		return 0, err
	}
	var facts *Facts
	if samePath(store.Root, shared.Root) {
		facts, err = storeFacts(ctx)
	} else {
		facts, err = NewFacts(filepath.Join(store.Root, "facts.json"), false)
	}
	if err != nil {
		return 0, err
	}
	removed, kept, err := gcStore(ctx, store, facts)
	if err != nil {
		return 0, err
	}
	generations, err := collectGenerations(ctx, RepoRoot(), 86400*time.Second)
	if err != nil {
		return 0, err
	}
	runtimes, err := CollectRuntimeGenerations(filepath.Join(InstallStateDir(ctx, RepoRoot()), "pm-runtime"))
	if err != nil {
		return 0, err
	}
	orphans, err := CollectOrphanInstallStates(InstallsRoot(ctx))
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(SyncOutput(ctx), "gc: removed %d, kept %d; removed %d dependency generations, %d PM runtime generations, %d install state dir(s) of deleted checkouts\n",
		removed, kept, len(generations), len(runtimes), len(orphans))
	return 0, nil
}

// Prints the latest pm receipt in its recorded key order
func CmdStatus(ctx context.Context) (int, error) {
	out := SyncOutput(ctx)
	data, ok := latestReceiptDocument(ctx)
	if !ok || data == nil {
		fmt.Fprintln(out, "no pm sync receipt yet (no venv operation has run)")
		return 0, nil
	}
	text, err := dumpsJSON(data, true)
	if err != nil {
		return 0, err
	}
	fmt.Fprintln(out, string(text))
	return 0, nil
}

// Newest receipt document with its key order, false when absent, unreadable or not JSON
func latestReceiptDocument(ctx context.Context) (any, bool) {
	point := filepath.Join(receiptDir(ctx), "latest.json")
	if !isRegularFile(point) {
		return nil, false
	}
	data, err := os.ReadFile(point)
	if err != nil || !utf8.Valid(data) {
		return nil, false
	}
	doc, err := loadOrderedJSON(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	if err != nil {
		return nil, false
	}
	return doc, true
}

// Rebuilds the recorded dependency environment without changing its graph
func CmdRepair(ctx context.Context) (int, error) {
	recovered, err := recoverIfNeeded(ctx, RepoRoot(), nil, true)
	if err != nil {
		return 0, err
	}
	if !recovered {
		return 1, nil
	}
	fmt.Fprintln(SyncOutput(ctx), "Restart icebath to use the repaired dependency environment.")
	return 0, nil
}

// Pinned requirements of declared extras with the project's self references expanded
func extraRequirements(project string, extras []string) ([]string, error) {
	path := filepath.Join(project, "pyproject.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := strictUTF8(data); err != nil {
		return nil, &PyError{Class: "ValueError", Message: err.Error()}
	}
	declared, err := tomlLoads(universalNewlines(string(data)))
	if err != nil {
		return nil, err
	}
	projectTable, err := tomlIndex(declared, "project")
	if err != nil {
		return nil, err
	}
	nameValue, err := tomlIndex(projectTable, "name")
	if err != nil {
		return nil, err
	}
	table, err := tomlGet(projectTable, "optional-dependencies", NewOrderedObject())
	if err != nil {
		return nil, err
	}
	var requirements []string
	var own *regexp.Regexp
	pending, seen := append([]string{}, extras...), map[string]bool{}
	for len(pending) > 0 {
		extra := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[extra] {
			continue
		}
		found, err := tomlContains(table, extra)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, &PyError{Class: "ValueError", Message: fmt.Sprintf("%s is not an extra declared in %s", pyrepr.StrRepr(extra), path)}
		}
		seen[extra] = true
		listed, err := tomlIndex(table, extra)
		if err != nil {
			return nil, err
		}
		rows, err := tomlIter(listed)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if own == nil {
				name, ok := nameValue.(string)
				if !ok {
					return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("decoding to str: need a bytes-like object, %s found", tomlTypeName(nameValue))}
				}
				own = regexp.MustCompile(`\A(?:` + regexp.QuoteMeta(name) + `\[([^\]]+)\])\z`)
			}
			requirement, ok := row.(string)
			if !ok {
				return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'replace'", tomlTypeName(row))}
			}
			if m := own.FindStringSubmatch(strings.ReplaceAll(requirement, " ", "")); m != nil {
				pending = append(pending, strings.Split(m[1], ",")...)
			} else {
				requirements = append(requirements, requirement)
			}
		}
	}
	return requirements, nil
}

// Makes a named side environment current and prints only its interpreter on stdout
func CmdEnvironment(ctx context.Context, args EnvironmentArgs) (int, error) {
	stderr := cliStderr(ctx)
	if (len(args.Extra) > 0) == (len(args.Requirement) > 0) {
		fmt.Fprintln(stderr, "icebath pm environment: pass --extra or --requirement (not both)")
		return 2, nil
	}
	restore, err := redirectStdout()
	if err != nil {
		return 0, err
	}
	python, err := func() (string, error) {
		defer restore()
		quiet := withSyncOutput(ctx, stderrStream())
		requirements := args.Requirement
		if len(args.Extra) > 0 {
			var err error
			if requirements, err = extraRequirements(RepoRoot(), args.Extra); err != nil {
				return "", err
			}
		}
		return pmEnsureEnvironment(quiet, EnsureEnvironmentOptions{Name: args.Name, Requirements: requirements, Explicit: true})
	}()
	if err != nil {
		var install *InstallError
		var toml *TOMLDecodeError
		if errors.As(err, &install) || errors.As(err, &toml) || errorClass(err) == "ValueError" {
			fmt.Fprintf(stderr, "icebath pm environment: %s\n", err)
			return 1, nil
		}
		return 0, err
	}
	fmt.Fprintln(SyncOutput(ctx), python)
	return 0, nil
}

// Stages a payload of repo, store, facts and relocatable venv into the output dir
func CmdBundle(ctx context.Context, args BundleArgs) (int, error) {
	return stageNative(ctx, args)
}

// CmdUpdateArgs runs pm update, the driver living in update.go
func CmdUpdateArgs(ctx context.Context, args UpdateArgs) (int, error) {
	return CmdUpdate(ctx, args, SyncOutput(ctx))
}

// LockUsageError is the pm lock usage refusal for a bump name without a version or the reverse
func LockUsageError(args LockArgs) string {
	if (args.Name == nil) != (args.Version == nil) {
		return "--bump NAME and VERSION go together; run with neither to relock uv.lock"
	}
	return ""
}

// Runs one pm command, reporting install errors and install state permission failures with status 1
func Main(ctx context.Context, command func(context.Context) (int, error)) (code int, err error) {
	defer func() {
		if r := recover(); r != nil {
			uncaught, ok := r.(uncaughtError)
			if !ok {
				panic(r)
			}
			code, err = mainFailure(ctx, uncaught.err)
		}
	}()
	code, err = command(ctx)
	if err != nil {
		return mainFailure(ctx, err)
	}
	return code, nil
}

// Exit status and surviving error for a failure that left a pm command
func mainFailure(ctx context.Context, err error) (int, error) {
	var exit *SystemExit
	if errors.As(err, &exit) {
		return exit.Code, nil
	}
	var install *InstallError
	if errors.As(err, &install) {
		fmt.Fprintf(cliStderr(ctx), "✗ %s\n", err)
		return 1, nil
	}
	if errors.Is(err, fs.ErrPermission) {
		if message := InstallStatePermissionMessage(ctx, RepoRoot(), err); message != "" {
			fmt.Fprintf(cliStderr(ctx), "✗ %s\n", message)
			return 1, nil
		}
	}
	return 1, err
}

// Strings in first occurrence order without repeats
func dedupeStrings(items []string) []string {
	out := []string{}
	for _, item := range items {
		if !containsString(out, item) {
			out = append(out, item)
		}
	}
	return out
}

// Reports whether path names a directory, following links
func isDirPath(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// Sorts sibling names the way Python sorts their paths, by code point and lowercased on Windows
func sortPathNames(names []string) {
	sort.SliceStable(names, func(i, j int) bool {
		if runtime.GOOS == "windows" {
			return pystr.Lower(names[i]) < pystr.Lower(names[j])
		}
		return tarNameLess(names[i], names[j])
	})
}

// Reports whether two paths are equal as Python Path objects compare, lowercased on Windows
func samePath(a, b string) bool {
	a, b = pypath.PurePathStr(a), pypath.PurePathStr(b)
	if runtime.GOOS == "windows" {
		return pystr.Lower(a) == pystr.Lower(b)
	}
	return a == b
}
