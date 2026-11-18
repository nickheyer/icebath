//go:build !icebath_core && !icebath_noextras

package pm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Ceiling on one extra's install, long enough for large wheels and a source build on a slow machine
const extraInstallTimeout = 90 * time.Minute

// Directory of the install state holding one environment per provisioned extra
func ExtrasRoot(ctx context.Context) string {
	return filepath.Join(InstallStateDir(ctx, RepoRoot()), "extras")
}

// Environment root of one canonical extra
func extraRoot(ctx context.Context, canonical string) string {
	return filepath.Join(ExtrasRoot(ctx), canonical)
}

// Network remedy naming the command that retries
func retryCommandRemedy(command string) string {
	return "check that this machine reaches the address in the error, through its proxy when it uses one, then run `" + command + "` again"
}

// Install error of a failed provisioning keeping a specific remedy and naming the retry command in place of a generic one
func provisioningFailure(name string, install *InstallError, command string) *InstallError {
	remedy := install.Remedy
	if remedy == RetryDownloadRemedy || remedy == NewInstallError("", "", "").Remedy {
		remedy = retryCommandRemedy(command)
	}
	return &InstallError{Package: name, Cause: install.Cause, Remedy: remedy, Err: install.Err}
}

// Lock document of packages merged from several extras, refusing two extras that pin one package differently
func mergedPylock(names []string, target string) ([]byte, error) {
	short, _, err := extrasPythonVersion()
	if err != nil {
		return nil, err
	}
	seen := map[string]LockedPackage{}
	owner := map[string]string{}
	var merged []LockedPackage
	for _, name := range names {
		canonical, entry, ok := ResolveExtra(name)
		if !ok {
			return nil, UnknownExtraError(name)
		}
		packages, err := entry.packagesFor(target)
		if err != nil {
			return nil, err
		}
		for _, p := range packages {
			key := CanonicalizeName(p.Name)
			if prior, ok := seen[key]; ok {
				if prior.Version != p.Version {
					return nil, NewInstallError(canonical, fmt.Sprintf("%s pins %s %s but %s pins %s", canonical, p.Name, p.Version, owner[key], prior.Version),
						"build these extras in separate environments")
				}
				continue
			}
			seen[key], owner[key] = p, canonical
			merged = append(merged, p)
		}
	}
	return pylockDocument(short, merged), nil
}

// Build requirements of several extras' source builds on a target, refusing two extras that pin one build requirement differently
func mergedBuildDeps(names []string, target string) ([]LockedPackage, error) {
	seen := map[string]LockedPackage{}
	owner := map[string]string{}
	var merged []LockedPackage
	for _, name := range names {
		canonical, entry, ok := ResolveExtra(name)
		if !ok {
			return nil, UnknownExtraError(name)
		}
		deps, err := entry.buildDepsFor(target)
		if err != nil {
			return nil, err
		}
		for _, p := range deps {
			key := CanonicalizeName(p.Name)
			if prior, ok := seen[key]; ok {
				if prior.Version != p.Version {
					return nil, NewInstallError(canonical, fmt.Sprintf("%s builds with %s %s but %s builds with %s", canonical, p.Name, p.Version, owner[key], prior.Version),
						"build these extras in separate environments")
				}
				continue
			}
			seen[key], owner[key] = p, canonical
			merged = append(merged, p)
		}
	}
	return merged, nil
}

// Generation inputs of pinned packages and their build requirements, the build digest present only when something builds
func pinnedInputs(pins []byte, buildDeps []LockedPackage) map[string]any {
	inputs := map[string]any{"pins": pinsDigest(pins)}
	if len(buildDeps) > 0 {
		inputs["build"] = pinsDigest(buildConstraintsText(buildDeps))
	}
	return inputs
}

// Fetches every artifact of the build requirements serving this target into dir, each checked against its pinned digest
func fetchBuildDeps(buildDeps []LockedPackage, dir string, out io.Writer) error {
	for _, p := range buildDeps {
		files := append([]LockedFile{}, p.Wheels...)
		if len(files) == 0 && p.Sdist != nil {
			files = append(files, *p.Sdist)
		}
		for _, f := range files {
			name := filepath.Base(strings.SplitN(f.URL, "?", 2)[0])
			if err := fetchPinnedFile(f, filepath.Join(dir, name)); err != nil {
				remedy := RetryDownloadRemedy
				if IsDownloadKind(err, DownloadHashFailed) {
					remedy = "the file that arrived is not the one the lock pins, so nothing was built with it; check what proxy or mirror sits between this machine and the address, then run the same command again"
				}
				return NewInstallError(p.Name, fmt.Sprintf("build requirement %s %s: %s", p.Name, p.Version, err), remedy)
			}
			fmt.Fprintf(out, "  ✓ build requirement %s %s\n", p.Name, p.Version)
		}
	}
	return nil
}

// Downloads one locked file to path, refusing bytes whose digest differs from the pin
func fetchPinnedFile(f LockedFile, path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = RetryNetwork(func() (bool, error) {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return false, err
		}
		if err := file.Truncate(0); err != nil {
			return false, err
		}
		resp, err := openURL(f.URL, nil, 600*time.Second)
		if err != nil {
			return false, err
		}
		defer resp.Body.Close()
		h := sha256.New()
		if _, err := io.Copy(io.MultiWriter(file, h), resp.Body); err != nil {
			return false, err
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
			return false, downloadError(DownloadHashFailed, "%s has sha256 %s, the lock pins %s", f.URL, got, f.SHA256)
		}
		return true, nil
	}, RetryOptions{})
	return err
}

// Short digest of lock bytes an environment records to know when its pins change
func pinsDigest(pins []byte) string {
	sum := sha256.Sum256(pins)
	return hex.EncodeToString(sum[:])
}

// Installs a pylock document exactly as locked, source builds taking their requirements only from the hash checked locked files
func (e *PythonEnvironment) installLocked(ctx context.Context, pins []byte, buildDeps []LockedPackage, out io.Writer) error {
	temporary, err := os.MkdirTemp("", "pm-extra-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	file := filepath.Join(temporary, "pylock.toml")
	if err := os.WriteFile(file, pins, 0o666); err != nil {
		return err
	}
	build := filepath.Join(temporary, "build-constraints.txt")
	if err := os.WriteFile(build, buildConstraintsText(buildDeps), 0o666); err != nil {
		return err
	}
	links := filepath.Join(temporary, "build-requirements")
	if err := os.Mkdir(links, 0o777); err != nil {
		return err
	}
	if err := fetchBuildDeps(buildDeps, links, out); err != nil {
		return err
	}
	command := []string{"pip", "install", "--no-config", "--python", e.Executable(), "--preview-features", "pylock,build-dependency-hashes",
		"--requirements", file, "--require-build-hashes", "--no-index", "--find-links", links}
	if len(buildDeps) > 0 {
		command = append(command, "--build-constraints", build)
	}
	result, err := e.withCacheRecovery(ctx, "pip", func() (uvResult, error) {
		return e.run(ctx, command, temporary, extraInstallTimeout)
	})
	if err != nil {
		return err
	}
	if result.Code != 0 {
		return ClassifyUVFailure("pip", result.Code, result.diagnostic())
	}
	return nil
}

// Installs the pm tools a source build calls with progress, returning the environment that puts them on PATH
func extraBuildEnvironment(ctx context.Context, tools []string, out io.Writer) (map[string]string, error) {
	var stream io.Writer
	if streamIsTerminal(out) && enableVT(out) {
		stream = out
	}
	var env map[string]string
	for _, name := range tools {
		if systemBuildTool(name) {
			continue
		}
		progress := &liveProgress{name: name, stream: stream, out: out}
		fetched := false
		runner, err := ensure(ctx, name, EnsureOptions{Explicit: true, Progress: func(stage string, done, total int64, label string) error {
			fetched = true
			return progress.report(stage, done, total, label)
		}})
		progress.finish()
		if err != nil {
			return nil, err
		}
		if fetched {
			fmt.Fprintf(out, "  ✓ %s\n", name)
		}
		if env == nil {
			env = map[string]string{}
			for k, v := range runner.Env {
				env[k] = v
			}
			continue
		}
		known := filepath.SplitList(env["PATH"])
		for _, dir := range filepath.SplitList(runner.Env["PATH"]) {
			if !containsString(known, dir) {
				env["PATH"] = dir + string(os.PathListSeparator) + env["PATH"]
			}
		}
	}
	return env, nil
}

// Builds or reuses a generation of root holding exactly the given pins built with exactly the given build constraints
func ensurePinnedGeneration(ctx context.Context, name, root string, pins []byte, buildDeps []LockedPackage, record map[string]any, tools []string, out io.Writer) (string, error) {
	toolchainCtx, cache, found, err := runtimeToolchainContext(ctx, ExtrasRoot(ctx))
	if err != nil {
		return "", err
	}
	if !found {
		if err := ensureRuntimeToolchain(toolchainCtx, out); err != nil {
			return "", err
		}
	}
	inputs, selection := pinnedInputs(pins, buildDeps), pinnedInputs(pins, buildDeps)
	for k, v := range record {
		inputs[k], selection[k] = v, v
	}
	build := func(generation, basePython string) (string, error) {
		buildEnv, err := extraBuildEnvironment(ctx, tools, out)
		if err != nil {
			return "", err
		}
		env, err := ManagedEnvironment(toolchainCtx, filepath.Join(generation, "venv"), ManagedEnvironmentOptions{
			Python: basePython, Cache: cache, Env: buildEnv, Explicit: true, Output: out})
		if err != nil {
			return "", err
		}
		if err := env.Create(ctx); err != nil {
			return "", err
		}
		if err := env.installLocked(ctx, pins, buildDeps, out); err != nil {
			return "", err
		}
		return env.Executable(), nil
	}
	return ensureGeneration(toolchainCtx, name, root, inputs, build, selection, true, "")
}

// Provisions an extra in this process with progress on the output, the step a user switching its feature on runs
func managedEnsureExtra(ctx context.Context, name string, opts ExtraOptions) (string, error) {
	canonical, entry, ok := ResolveExtra(name)
	if !ok {
		return "", UnknownExtraError(name)
	}
	out := opts.Output
	if out == nil {
		out = io.Discard
	}
	python, err := provisionExtra(ctx, canonical, entry, out)
	if err != nil {
		return "", err
	}
	if _, err := recordDeclinedExtras(ctx, nil, []string{canonical}, nil, RepoRoot()); err != nil {
		return "", err
	}
	return python, nil
}

// Relay runtime or pinned generation of one extra, the preflight refusing before anything downloads
func provisionExtra(ctx context.Context, canonical string, entry *ExtraEntry, out io.Writer) (string, error) {
	if canonical == relayExtra {
		venv, err := EnsureRuntimeEnvironment(ctx, RuntimeEnvironmentOptions{Output: out})
		if err != nil {
			return "", err
		}
		return VenvPython(venv), nil
	}
	target, err := CurrentTarget()
	if err != nil {
		return "", err
	}
	if err := extraPreflight(canonical, entry, target, out); err != nil {
		return "", err
	}
	pins, err := mergedPylock([]string{canonical}, target)
	if err != nil {
		return "", err
	}
	buildDeps, err := mergedBuildDeps([]string{canonical}, target)
	if err != nil {
		return "", err
	}
	python, err := ensurePinnedGeneration(ctx, canonical, extraRoot(ctx, canonical), pins, buildDeps, map[string]any{"extra": canonical}, entry.Tools, out)
	if err != nil {
		var install *InstallError
		if errors.As(err, &install) {
			return "", provisioningFailure(canonical, install, InstallHint(canonical))
		}
		return "", err
	}
	return python, nil
}

// Side environment NAME holding the pinned packages of the named extras, the pm environment extra mode of a binary install
func EnsureExtrasEnvironment(ctx context.Context, name string, extras []string, out io.Writer) (string, error) {
	root, err := environmentRoot(ctx, name, "")
	if err != nil {
		return "", err
	}
	target, err := CurrentTarget()
	if err != nil {
		return "", err
	}
	var canonical, tools []string
	for _, extra := range extras {
		key, entry, ok := ResolveExtra(extra)
		if !ok {
			return "", UnknownExtraError(extra)
		}
		if err := extraPreflight(key, entry, target, out); err != nil {
			return "", err
		}
		if !containsString(canonical, key) {
			canonical = append(canonical, key)
		}
		for _, tool := range entry.Tools {
			if !containsString(tools, tool) {
				tools = append(tools, tool)
			}
		}
	}
	pins, err := mergedPylock(canonical, target)
	if err != nil {
		return "", err
	}
	buildDeps, err := mergedBuildDeps(canonical, target)
	if err != nil {
		return "", err
	}
	python, err := ensurePinnedGeneration(ctx, name, root, pins, buildDeps, map[string]any{"extras": strings.Join(canonical, ",")}, tools, out)
	if err != nil {
		var install *InstallError
		if errors.As(err, &install) {
			return "", provisioningFailure(name, install, "icebath pm environment "+name+" --extra "+strings.Join(canonical, " --extra "))
		}
		return "", err
	}
	return python, nil
}

// Canonical name of the relay extra, whose environment the relay runtime owns
const relayExtra = "nemo-relay"

// Interpreter of a provisioned extra, false when the extra is unknown or not provisioned
func ExtraInterpreter(ctx context.Context, name string) (string, bool) {
	canonical, _, ok := ResolveExtra(name)
	if !ok {
		return "", false
	}
	if canonical == relayExtra {
		python := VenvPython(RelayVenv(ctx))
		return python, isRegularFile(python)
	}
	python, found, err := environmentPython(ctx, canonical, extraRoot(ctx, canonical))
	return python, err == nil && found
}

// Isolated interpreter command of a provisioned extra, an install error naming the command to run when it is missing
func managedExtraPythonCommand(ctx context.Context, name string) ([]string, error) {
	python, ok := ExtraInterpreter(ctx, name)
	if !ok {
		canonical, _, known := ResolveExtra(name)
		if !known {
			return nil, UnknownExtraError(name)
		}
		return nil, NewInstallError(canonical, "the "+canonical+" extra is not provisioned", "run `"+InstallHint(canonical)+"`")
	}
	return []string{python, "-I"}, nil
}

// Site packages of a provisioned extra, false when it is not provisioned
func ExtraSitePackages(ctx context.Context, name string) (string, bool) {
	python, ok := ExtraInterpreter(ctx, name)
	if !ok {
		return "", false
	}
	site, err := SitePackages(filepath.Dir(filepath.Dir(python)))
	if err != nil {
		return "", false
	}
	return site, true
}

// Whether an extra's environment is provisioned on this install
func ExtraProvisioned(ctx context.Context, name string) bool {
	_, ok := ExtraInterpreter(ctx, name)
	return ok
}

// Provisioning state of one extra, whether its environment exists and holds the pins this binary carries
type ExtraState struct {
	Name        string
	Feature     string
	Provisioned bool
	Current     bool
	Unsupported string
}

// State of every extra this binary carries on this install
func ExtraStates(ctx context.Context) ([]ExtraState, error) {
	target, err := CurrentTarget()
	if err != nil {
		return nil, err
	}
	var out []ExtraState
	for _, name := range KnownExtras() {
		entry := extrasLock().Extras[name]
		state := ExtraState{Name: name, Feature: entry.Feature, Unsupported: entry.Unsupported[target]}
		if name == relayExtra {
			_, ready, err := RuntimeEnvironmentReady(ctx)
			if err != nil {
				return nil, err
			}
			state.Provisioned, state.Current = ExtraProvisioned(ctx, name), ready
			out = append(out, state)
			continue
		}
		state.Provisioned = ExtraProvisioned(ctx, name)
		if state.Provisioned && state.Unsupported == "" {
			pins, err := mergedPylock([]string{name}, target)
			if err != nil {
				return nil, err
			}
			buildDeps, err := mergedBuildDeps([]string{name}, target)
			if err != nil {
				return nil, err
			}
			selected, err := environmentSelection(extraRoot(ctx, name))
			if err != nil {
				return nil, err
			}
			want := pinnedInputs(pins, buildDeps)
			state.Current = selected["pins"] == want["pins"] && selected["build"] == want["build"]
		}
		out = append(out, state)
	}
	return out, nil
}

// Deletes an extra's environment so nothing of it stays on disk
func RemoveExtra(ctx context.Context, name string) error {
	canonical, _, ok := ResolveExtra(name)
	if !ok {
		return UnknownExtraError(name)
	}
	if canonical == relayExtra {
		if err := os.RemoveAll(RelayVenv(ctx)); err != nil {
			return err
		}
		return nil
	}
	return os.RemoveAll(extraRoot(ctx, canonical))
}
