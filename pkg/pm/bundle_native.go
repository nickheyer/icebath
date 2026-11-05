package pm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// StageNativeArgs are the isolated native staging child's arguments, empty TUI or Web when not given
type StageNativeArgs struct {
	Out    string
	Ref    string
	Source string
	TUI    string
	Web    string
}

// Argv starting this binary's hidden isolated native staging command, swapped by tests for a re-executed test binary
var stageNativeArgv = func() ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []string{exe, "pm", "stage-native"}, nil
}

// Seams the bundle staging reads, replaced by tests the way Python patches the native module
var (
	bundleLockfile = currentLockfile
	bundleTarget   = CurrentTarget
)

// Lockfile packages a payload ships, the public ones plus uv, always including python
func bundlePackageNames() ([]string, error) {
	var names []string
	for _, name := range bundleLockfile().Names() {
		p, err := GetPackage(name)
		if err != nil {
			return nil, err
		}
		if !p.Base().Internal || name == "uv" {
			names = append(names, name)
		}
	}
	if !slices.Contains(names, "python") {
		names = append(names, "python")
	}
	return names, nil
}

// Staged entry binaries whose machine type is not this target and the package does not run emulated
func archGuard(storeDir string) ([]string, error) {
	facts, err := NewFacts(filepath.Join(storeDir, "facts.json"), false)
	if err != nil {
		return nil, err
	}
	target, err := bundleTarget()
	if err != nil {
		return nil, err
	}
	var problems []string
	for _, name := range bundleLockfile().Names() {
		p, err := GetPackage(name)
		if err != nil {
			return nil, err
		}
		fact, ok := facts.Get(name)
		if !ok || fact == nil {
			continue
		}
		entry, ok := fact["entry"]
		if !ok {
			continue
		}
		binary := p.Binary(filepath.Join(storeDir, fmt.Sprint(entry)), target)
		if binary == "" {
			continue
		}
		if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
			continue
		}
		if match, known := MachineMatchesBinary(binary, target); known && !match && !slices.Contains(p.Base().EmulatedArchTargets, target) {
			problems = append(problems, fmt.Sprintf("%s: %s is not a %s binary", name, filepath.Base(binary), target))
		}
	}
	return problems, nil
}

// Copies a uv cache with links kept as links, leaving out sdist source trees a revision pointer selects
func stageUVCache(source, destination string) error {
	ignore := func(directory string, names []string) map[string]bool {
		rel, err := filepath.Rel(source, directory)
		if err != nil || rel == "." {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if !strings.HasPrefix(parts[0], "sdists-v") || !slices.Contains(names, "src") {
			return nil
		}
		if info, err := os.Stat(filepath.Join(directory, "src")); err != nil || !info.IsDir() {
			return nil
		}
		for _, pointer := range []string{"revision.http", "revision.rev"} {
			if info, err := os.Stat(filepath.Join(filepath.Dir(directory), pointer)); err == nil && info.Mode().IsRegular() {
				return map[string]bool{"src": true}
			}
		}
		return nil
	}
	return copyTreeLike(source, destination, true, ignore)
}

// Publishes the PM dependency runtime under root from the repo's PM project and seals it
func stagePMRuntime(ctx context.Context, root, python, repo string, offline bool, cache string) error {
	destination := filepath.Join(root, "pm-runtime")
	if _, err := os.Lstat(destination); err == nil {
		if err := os.RemoveAll(destination); err != nil {
			return err
		}
	}
	if _, err := StageManagerRuntime(ctx, StageManagerRuntimeOptions{Python: python, Destination: destination,
		Project: filepath.Join(repo, "pkg", "pm"), Offline: offline, Cache: cache}); err != nil {
		return err
	}
	return sealPMRuntime(ctx, root, python)
}

// Runs the native staging child with home and PM state isolated but the build cache kept
func stageNative(ctx context.Context, args BundleArgs) (int, error) {
	out := resolvePath(args.Out)
	if err := os.MkdirAll(out, 0o777); err != nil {
		return 0, err
	}
	if err := os.Remove(filepath.Join(out, "manifest.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	root := RepoRoot()
	cache := args.Cache
	if cache == "" {
		cache = os.Getenv("UV_CACHE_DIR")
	}
	if cache == "" {
		dir, err := UvCacheDir(ctx)
		if err != nil {
			return 0, err
		}
		cache = dir
	}
	cache = resolvePath(cache)
	baseEnv := environMap()
	target, err := CurrentTarget()
	if err != nil {
		return 0, err
	}
	if target == "win32-arm64" {
		if baseEnv, err = PrepareWindowsEnvironment(root, filepath.Join(filepath.Dir(out), ".build-deps"), baseEnv); err != nil {
			return 0, err
		}
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return 0, err
	}
	for _, pair := range [][2]string{{"CARGO_HOME", ".cargo"}, {"RUSTUP_HOME", ".rustup"}} {
		if _, ok := baseEnv[pair[0]]; !ok {
			baseEnv[pair[0]] = filepath.Join(userHome, pair[1])
		}
	}
	work, err := os.MkdirTemp(out, ".build-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(work)
	env := map[string]string{}
	for key, value := range baseEnv {
		env[key] = value
	}
	for key, value := range map[string]string{
		"HOME": work, "USERPROFILE": work,
		"ICEBATH_HOME":            filepath.Join(work, ".icebath"),
		EnvRuntimeDir:             filepath.Join(out, "tools"),
		"ICEBATH_PYTHON_SRC_ROOT": root,
		"XDG_CACHE_HOME":          filepath.Join(work, "cache"),
		"XDG_CONFIG_HOME":         filepath.Join(work, "config"),
		"UV_CACHE_DIR":            cache,
	} {
		env[key] = value
	}
	argv, err := stageNativeArgv()
	if err != nil {
		return 0, err
	}
	ref := args.Ref
	if ref == "" {
		ref = "HEAD"
	}
	argv = append(argv, "--out", out, "--ref", ref, "--source", root)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = root, EnvList(env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, SyncOutput(ctx), cliStderr(ctx)
	return exitStatus(cmd.Run())
}

// Runs one named staging phase, printing its start and on success its duration
func phase(ctx context.Context, label string, body func() error) error {
	out := SyncOutput(ctx)
	fmt.Fprintf(out, "  %s…\n", label)
	started := time.Now()
	if err := body(); err != nil {
		return err
	}
	fmt.Fprintf(out, "  ✓ %s (%.1fs)\n", label, time.Since(started).Seconds())
	return nil
}

// Removes store entries outside the facts of the names' closure, leaving dot entries alone
func pruneStagedStore(storeDir string, names []string) error {
	facts, err := NewFacts(filepath.Join(storeDir, "facts.json"), true)
	if err != nil {
		return err
	}
	closure, err := Walk(names)
	if err != nil {
		return err
	}
	retained := map[string]bool{}
	for _, p := range closure {
		retained[p.Base().Name] = true
	}
	if err := facts.Retain(retained); err != nil {
		return err
	}
	keep := facts.EntriesInUse()
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(storeDir, entry.Name())
		if info, err := os.Stat(path); err == nil && info.IsDir() && !strings.HasPrefix(entry.Name(), ".") && !keep[entry.Name()] {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		}
	}
	return nil
}

// Prepares the payload dependencies under out holding its preparation lock, returning the prepared record path
func PrepareNative(ctx context.Context, out, ref, source, cache, tools string, env map[string]string) (string, error) {
	out, err := filepath.Abs(out)
	if err != nil {
		return "", err
	}
	if out != resolvePath(out) {
		return "", &PyError{Class: "ValueError", Message: "symlinked native output"}
	}
	if err := os.MkdirAll(out, 0o777); err != nil {
		return "", err
	}
	release, err := preparationLock(out)
	if err != nil {
		return "", err
	}
	defer release()
	for _, stale := range []string{preparedPath(out), filepath.Join(out, "manifest.json")} {
		if err := os.Remove(stale); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	cache, err = filepath.Abs(cache)
	if err != nil {
		return "", err
	}
	return prepareNative(ctx, out, ref, resolvePath(source), cache, tools, env)
}

// Snapshots the repo, stages tools, venv, runtime and uv cache, verifies architectures and publishes the inputs
func prepareNative(ctx context.Context, out, ref, source, cache, tools string, env map[string]string) (string, error) {
	stdout := SyncOutput(ctx)
	storeDir := filepath.Join(out, "tools")
	if err := os.MkdirAll(storeDir, 0o777); err != nil {
		return "", err
	}
	repoDir := filepath.Join(out, "icebath-agent")
	revision, err := revParseCommit(ctx, source, ref)
	if err != nil {
		return "", err
	}
	if err := phase(ctx, fmt.Sprintf("staging repo snapshot (%s)", ref), func() error {
		return bundleSnapshot(ctx, source, revision, repoDir, inertSnapshotDirs)
	}); err != nil {
		return "", err
	}
	staged, err := os.ReadFile(filepath.Join(repoDir, "pkg", "pm", "lock.json"))
	if err != nil {
		return "", err
	}
	builder, err := os.ReadFile(LockfilePath())
	if err != nil {
		return "", err
	}
	if !bytes.Equal(staged, builder) {
		return "", &PyError{Class: "ValueError", Message: "selected revision's PM lock differs from the builder; use a checkout at that revision"}
	}
	buildEnv := env
	if buildEnv == nil {
		buildEnv = environMap()
	}
	target, err := CurrentTarget()
	if err != nil {
		return "", err
	}
	candidates, err := bundlePackageNames()
	if err != nil {
		return "", err
	}
	var names []string
	for _, name := range candidates {
		p, err := GetPackage(name)
		if err != nil {
			return "", err
		}
		if p.MissingReason(target) == "" {
			names = append(names, name)
		}
	}
	prepared := storeDir
	if tools != "" {
		prepared = tools
	}
	if err := phase(ctx, fmt.Sprintf("preparing %d pinned tools", len(names)), func() error {
		_, err := PrepareTools(ctx, PrepareToolsOptions{Names: names, Out: prepared, Target: target})
		return err
	}); err != nil {
		return "", err
	}
	if tools != "" {
		if err := phase(ctx, "copying verified tools into the payload store", func() error {
			_, err := StageTools(ctx, StageToolsOptions{Names: names, SourceStore: tools, Out: storeDir, Target: target})
			return err
		}); err != nil {
			return "", err
		}
	}
	if err := phase(ctx, "pruning the staged tool store", func() error { return pruneStagedStore(storeDir, names) }); err != nil {
		return "", err
	}
	facts, err := NewFacts(filepath.Join(storeDir, "facts.json"), false)
	if err != nil {
		return "", err
	}
	pythonFact, ok := facts.Get("python")
	if !ok || pythonFact == nil {
		return "", NewInstallError("venv", "no staged interpreter to build on", "")
	}
	pythonPackage, err := GetPackage("python")
	if err != nil {
		return "", err
	}
	pythonBin := pythonPackage.Binary(filepath.Join(storeDir, fmt.Sprint(pythonFact["entry"])), target)
	if pythonBin == "" {
		return "", &PyError{Class: "FileNotFoundError", Message: "staged Python executable is missing"}
	}
	if err := phase(ctx, "pm-runtime (independent locked dependencies)", func() error {
		return stagePMRuntime(ctx, out, pythonBin, repoDir, false, cache)
	}); err != nil {
		return "", err
	}
	venvDir := filepath.Join(out, "venv")
	if _, err := os.Lstat(venvDir); err == nil {
		if err := os.RemoveAll(venvDir); err != nil {
			return "", err
		}
	}
	childEnv := map[string]string{}
	for key, value := range buildEnv {
		childEnv[key] = value
	}
	if _, err := BuildEnvironment(ctx, BuildEnvironmentOptions{Source: repoDir, Python: pythonBin, Out: venvDir,
		Env: childEnv, Cache: cache, AllExtras: true, Frozen: true, Sealed: true, Explicit: true, Timeout: 2 * time.Hour}); err != nil {
		return "", err
	}
	fmt.Fprintln(stdout, "✓ venv (all extras, on the staged interpreter)")
	features, err := InstalledExtras(ctx, repoDir, venvDir, pythonBin)
	if err != nil {
		return "", err
	}
	if _, err := WriteFeatures(ctx, features, out); err != nil {
		return "", err
	}
	stamp, err := newVenv(repoDir).Stamp(ctx, features, []MemberDir{})
	if err != nil {
		return "", err
	}
	if err := facts.RecordState("venv", stamp, features, "", ""); err != nil {
		return "", err
	}
	fmt.Fprintf(stdout, "✓ enabled-features.json (%d extras recorded)\n", len(features))
	if err := stageBundleUVCache(stdout, out, cache, repoDir); err != nil {
		return "", err
	}
	if strings.HasPrefix(target, "win32-") {
		dropped, err := dropMSIXReservedNames(out)
		if err != nil {
			return "", err
		}
		for _, tree := range dropped {
			fmt.Fprintf(stdout, "✓ msix: dropped %s (holds a package-reserved name, never read at runtime)\n", filepath.ToSlash(tree))
		}
	}
	bad, err := archGuard(storeDir)
	if err != nil {
		return "", err
	}
	for _, line := range bad {
		fmt.Fprintf(stdout, "✗ arch: %s\n", line)
	}
	if len(bad) > 0 {
		return "", NewInstallError("tools", "native architecture verification failed", "")
	}
	recorded := NewOrderedObject()
	for _, name := range names {
		if fact, ok := facts.Get(name); ok && len(fact) > 0 {
			if entry, ok := fact["entry"]; ok {
				recorded.Set(name, entry)
			}
		}
	}
	if err := recordTools(out, LockfilePath(), target, recorded); err != nil {
		return "", err
	}
	site, err := dependencySite(venvDir, fmt.Sprint(pythonFact["version"]), target)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(site, repoDir)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(site, "icebath-agent.pth"), []byte(filepath.ToSlash(rel)+"\n"), 0o644); err != nil {
		return "", err
	}
	if err := relativizeLinks(out); err != nil {
		return "", err
	}
	baked, err := bakeBytecode(ctx, out, pythonBin)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(stdout, "✓ baked bytecode (%d modules, unchecked-hash, owner-writable caches)\n", baked["modules"])
	resources := map[string]string{}
	for _, name := range resourceEnvNames() {
		resources[name] = filepath.Join(repoDir, name)
	}
	inputs := AgentInputs{
		Project: filepath.Join(repoDir, "pyproject.toml"), Code: repoDir, Repo: "icebath-agent",
		Placement: "contained", Target: target, Python: pythonBin,
		SitePackages: site, Environment: venvDir,
		Tools: storeDir, PMRuntime: filepath.Join(out, "pm-runtime"), Ref: ref,
		Resources: resources, Features: filepath.Join(out, "enabled-features.json"),
	}
	return publishPrepared(ctx, out, source, revision, inputs)
}

// Commit a ref names in the source checkout, failing like check_output on a nonzero git exit
func revParseCommit(ctx context.Context, source, ref string) (string, error) {
	argv := []string{"git", "rev-parse", "--verify", ref + "^{commit}"}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Stderr = source, os.Stderr
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", &PyError{Class: "CalledProcessError", Message: fmt.Sprintf("Command %s returned non-zero exit status %d.", pyrepr.Repr(argv), pyReturnCode(exitErr))}
		}
		return "", err
	}
	if err := strictUTF8(output); err != nil {
		return "", &PyError{Class: "UnicodeDecodeError", Message: err.Error()}
	}
	return pystr.Strip(string(output)), nil
}

// Replaces the payload uv cache with the lock scoped copy of the build cache
func stageBundleUVCache(stdout io.Writer, out, cache, repoDir string) error {
	payloadCache := filepath.Join(out, "uv-cache")
	if _, err := os.Lstat(payloadCache); err == nil {
		os.RemoveAll(payloadCache)
	}
	if info, err := os.Stat(cache); err != nil || !info.IsDir() {
		return NewInstallError("uv-cache", "runtime dependency cache is missing", "")
	}
	fmt.Fprintf(stdout, "  uv-cache: copying %s → payload...\n", cache)
	if err := stageUVCache(cache, payloadCache); err != nil {
		return err
	}
	pruned, err := PruneUVCacheToLock(payloadCache, repoDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "✓ uv-cache (lock-scoped: pruned %d stale entries; offline rebuilds resolve from shipped wheels)\n", pruned)
	return nil
}

// Assembles the payload from a verified preparation record and the given frontend products
func FinishNative(ctx context.Context, prepared string, frontends map[string]string) (int, error) {
	prepared, err := filepath.Abs(prepared)
	if err != nil {
		return 0, err
	}
	info, statErr := os.Stat(prepared)
	if !strings.HasSuffix(filepath.Base(prepared), ".prepared.json") || statErr != nil || !info.Mode().IsRegular() {
		return 0, &PyError{Class: "ValueError", Message: "native preparation is missing or invalid; run preparation again"}
	}
	out := strings.TrimSuffix(prepared, ".prepared.json")
	if err := func() error {
		release, err := preparationLock(out)
		if err != nil {
			return err
		}
		defer release()
		if err := os.Remove(filepath.Join(out, "manifest.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		inputs, err := loadPrepared(prepared)
		if err != nil {
			return err
		}
		inputs.Frontends = map[string]string{}
		for name, path := range frontends {
			if inputs.Frontends[name], err = filepath.Abs(path); err != nil {
				return err
			}
		}
		fmt.Fprintln(SyncOutput(ctx), "assembling verified payload")
		return assembleAgent(ctx, inputs, out)
	}(); err != nil {
		return 0, err
	}
	fmt.Fprintf(SyncOutput(ctx), "✓ manifest (%s)\n", filepath.Join(out, "manifest.json"))
	return 0, nil
}

// Isolated staging child that prepares and assembles the payload, an install or probe failure exiting 1
func StageNativeMain(ctx context.Context, args StageNativeArgs) (int, error) {
	frontends := map[string]string{}
	if args.TUI != "" {
		frontends["tui"] = args.TUI
	}
	if args.Web != "" {
		frontends["web"] = args.Web
	}
	ref := args.Ref
	if ref == "" {
		ref = "HEAD"
	}
	source := args.Source
	if source == "" {
		source = RepoRoot()
	}
	cache, ok := os.LookupEnv("UV_CACHE_DIR")
	if !ok {
		return 0, &PyError{Class: "KeyError", Message: pyrepr.StrRepr("UV_CACHE_DIR")}
	}
	code, err := func() (int, error) {
		prepared, err := PrepareNative(ctx, args.Out, ref, source, cache, "", nil)
		if err != nil {
			return 0, err
		}
		return FinishNative(ctx, prepared, frontends)
	}()
	var install *InstallError
	var probe *FeatureProbeError
	if errors.As(err, &install) || errors.As(err, &probe) {
		fmt.Fprintf(SyncOutput(ctx), "✗ %s\n", err)
		return 1, nil
	}
	return code, err
}
