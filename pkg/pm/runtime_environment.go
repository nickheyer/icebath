//go:build !icebath_core && !icebath_noextras

package pm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Hash pinned package set of one extra of the embedded lock an environment is provisioned with
type RuntimeSet struct {
	extra string
}

// Set the relay environment holds, provisioned only when Relay plugins are switched on
var RelayRuntimeSet = &RuntimeSet{extra: relayExtra}

// Locked packages of the set, each with every wheel and source archive the lock admits
func (s *RuntimeSet) Requirements() ([]RuntimeRequirement, error) {
	entry, ok := extrasLock().Extras[s.extra]
	if !ok {
		return nil, UnknownExtraError(s.extra)
	}
	out := []RuntimeRequirement{}
	for _, p := range entry.Packages {
		r := RuntimeRequirement{Name: p.Name, Version: p.Version, Marker: p.Marker}
		for _, w := range p.Wheels {
			r.Files = append(r.Files, Wheel{URL: w.URL, SHA256: w.SHA256})
		}
		if p.Sdist != nil {
			r.Files = append(r.Files, Wheel{URL: p.Sdist.URL, SHA256: p.Sdist.SHA256})
		}
		out = append(out, r)
	}
	return out, nil
}

// Locked relay runtime set the binary embeds
func managedRuntimeRequirements() ([]RuntimeRequirement, error) {
	return RelayRuntimeSet.Requirements()
}

// Environment the relay runtime set lives in, inside the install state and apart from the plugin environment
func RelayVenv(ctx context.Context) string {
	return filepath.Join(InstallStateDir(ctx, RepoRoot()), "relay-venv")
}

// Requirements file pinning each package of the set by version, marker and the hashes of its artifacts
func (s *RuntimeSet) requirementsText() ([]byte, error) {
	reqs, err := s.served()
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, r := range reqs {
		b.WriteString(r.Name + "==" + r.Version + " ; " + r.Marker)
		for _, f := range r.Files {
			b.WriteString(" \\\n    --hash=sha256:" + f.SHA256)
		}
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}

// Build constraints pinning by hash what a source build of the set may install on this host
func (s *RuntimeSet) buildConstraints() ([]byte, error) {
	entry, ok := extrasLock().Extras[s.extra]
	if !ok {
		return nil, UnknownExtraError(s.extra)
	}
	target, err := CurrentTarget()
	if err != nil {
		return nil, err
	}
	deps, err := entry.buildDepsFor(target)
	if err != nil {
		return nil, err
	}
	return buildConstraintsText(deps), nil
}

// Whether venv has its interpreter and a dist-info of the pinned version of every set package this host needs
func (s *RuntimeSet) installedIn(venv string) (bool, error) {
	if !isRegularFile(VenvPython(venv)) {
		return false, nil
	}
	reqs, err := s.Requirements()
	if err != nil {
		return false, err
	}
	target, err := CurrentTarget()
	if err != nil {
		return false, err
	}
	site, err := SitePackages(venv)
	if err != nil {
		return false, err
	}
	entries, err := os.ReadDir(site)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	for _, r := range reqs {
		if !r.ServesTarget(target) {
			continue
		}
		stem := strings.ReplaceAll(CanonicalizeName(r.Name), "-", "_") + "-"
		found := false
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() && strings.HasPrefix(name, stem) && strings.HasSuffix(name, ".dist-info") &&
				name == stem+r.Version+".dist-info" {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

// Relay environment made to hold the pinned set in this process, the step the user switching Relay on runs
func managedEnsureRuntimeEnvironment(ctx context.Context, opts RuntimeEnvironmentOptions) (string, error) {
	target, err := CurrentTarget()
	if err != nil {
		return "", err
	}
	if _, ok := NemoRelayWheel(target); !ok {
		return "", NewInstallError("nemo-relay", "NeMo Relay publishes no wheel for "+target, "use Relay plugins on a platform NeMo Relay publishes wheels for")
	}
	venv := RelayVenv(ctx)
	requirements, err := RelayRuntimeSet.requirementsText()
	if err != nil {
		return "", err
	}
	constraints, err := RelayRuntimeSet.buildConstraints()
	if err != nil {
		return "", err
	}
	gate := newProvisionGate(venv)
	release, err := gate.enter(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	if err := provisionUnderGate(ctx, gate, venv, opts, requirements, constraints); err != nil {
		var install *InstallError
		if errors.As(err, &install) {
			return "", NewInstallError("relay runtime", install.Cause, "check that this machine reaches the address in the error, through its proxy when it uses one, then run `icebath pm install --extra nemo-relay` again")
		}
		return "", err
	}
	return venv, nil
}

// Relay environment and whether it holds the pinned set with no provisioning changing it or left unfinished
func managedRuntimeEnvironmentReady(ctx context.Context) (string, bool, error) {
	venv := RelayVenv(ctx)
	if newProvisionGate(venv).unfinished() {
		return venv, false, nil
	}
	ready, err := RelayRuntimeSet.installedIn(venv)
	return venv, ready, err
}

// Installs the set holding the gate, a run an earlier one left unfinished reinstalling it, success clearing the marker
func provisionUnderGate(ctx context.Context, gate provisionGate, venv string, opts RuntimeEnvironmentOptions, requirements, constraints []byte) error {
	reinstall := gate.unfinished()
	if err := gate.begin(); err != nil {
		return err
	}
	if err := provisionRuntimeSet(ctx, venv, opts, requirements, constraints, reinstall); err != nil {
		return err
	}
	return gate.finish()
}

// Builds the environment on the pinned toolchain when missing and installs the pinned requirement bytes into it
func provisionRuntimeSet(ctx context.Context, venv string, opts RuntimeEnvironmentOptions, requirements, constraints []byte, reinstall bool) error {
	toolchainCtx, cache, found, err := runtimeToolchainContext(ctx, venv)
	if err != nil {
		return err
	}
	if !found {
		if err := ensureRuntimeToolchain(toolchainCtx, opts.Output); err != nil {
			return err
		}
	}
	env, err := ManagedEnvironment(toolchainCtx, venv, ManagedEnvironmentOptions{Explicit: true, Output: opts.Output, Cache: cache})
	if err != nil {
		return err
	}
	if !isRegularFile(env.Executable()) {
		if err := env.Create(ctx); err != nil {
			return err
		}
	}
	return env.installPinned(ctx, requirements, constraints, reinstall)
}

// Store beside the environment with its own facts, holding the interpreter and uv the environment is built on
func RuntimeSetStore(venv string) string {
	return venv + "-runtime"
}

// Toolchain context and uv cache of a runtime set environment, the default store when it holds the toolchain
func runtimeToolchainContext(ctx context.Context, venv string) (context.Context, string, bool, error) {
	_, _, found, err := uvToolchain(ctx, false, false)
	if err != nil {
		return nil, "", false, err
	}
	if found {
		return ctx, "", true, nil
	}
	store := RuntimeSetStore(venv)
	return WithRuntimeStore(ctx, store), filepath.Join(store, "uv-cache"), false, nil
}

// Installs the interpreter then uv into the store of ctx, each download's progress and its done row on out
func ensureRuntimeToolchain(ctx context.Context, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	var stream io.Writer
	if streamIsTerminal(out) && enableVT(out) {
		stream = out
	}
	for _, name := range []string{"python", "uv"} {
		progress := &liveProgress{name: name, stream: stream, out: out}
		fetched := false
		_, err := ensure(ctx, name, EnsureOptions{Explicit: true, Progress: func(stage string, done, total int64, label string) error {
			fetched = true
			return progress.report(stage, done, total, label)
		}})
		progress.finish()
		if err != nil {
			return err
		}
		if fetched {
			fmt.Fprintf(out, "  ✓ %s\n", name)
		}
	}
	return nil
}

// Installs a hash pinned requirement set exactly as locked without resolving again, any source build held to the hash pinned constraints
func (e *PythonEnvironment) installPinned(ctx context.Context, requirements, constraints []byte, reinstall bool) error {
	temporary, err := os.MkdirTemp("", "pm-runtime-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	file := filepath.Join(temporary, "requirements.txt")
	if err := os.WriteFile(file, requirements, 0o666); err != nil {
		return err
	}
	build := filepath.Join(temporary, "build-constraints.txt")
	if err := os.WriteFile(build, constraints, 0o666); err != nil {
		return err
	}
	command := []string{"pip", "install", "--no-config", "--python", e.Executable(), "--require-hashes", "--no-deps", "--requirements", file,
		"--preview-features", "build-dependency-hashes", "--require-build-hashes"}
	if len(constraints) > 0 {
		command = append(command, "--build-constraints", build)
	}
	if reinstall {
		command = append(command, "--reinstall")
	}
	result, err := e.withCacheRecovery(ctx, "pip", func() (uvResult, error) {
		return e.run(ctx, command, temporary, defaultUVTimeout)
	})
	if err != nil {
		return err
	}
	if result.Code != 0 {
		return ClassifyUVFailure("pip", result.Code, result.diagnostic())
	}
	return nil
}
