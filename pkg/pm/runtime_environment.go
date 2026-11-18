package pm

import (
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Tool runtime package of the hermes core sync, nemo-relay, hash pinned
//
//go:embed runtime_requirements.json
var runtimeRequirementsJSON []byte

// One locked package of the runtime set with its marker and every artifact hermes's lock admits
type RuntimeRequirement struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Marker  string   `json:"marker"`
	Files   []Wheel  `json:"files"`
	Targets []string `json:"targets,omitempty"`
}

// Whether the package installs on target, a wheel only package listing the targets its wheels serve
func (r RuntimeRequirement) ServesTarget(target string) bool {
	if len(r.Targets) == 0 {
		return true
	}
	for _, t := range r.Targets {
		if t == target {
			return true
		}
	}
	return false
}

// Embedded hash pinned package set a selected environment is provisioned with
type RuntimeSet struct {
	pinned []byte
	refuse func(ctx context.Context) *InstallError
}

// Package the nemo_relay binding loads
var ToolRuntime = &RuntimeSet{pinned: runtimeRequirementsJSON, refuse: func(ctx context.Context) *InstallError {
	return RefuseLazyFor(ctx, "venv", "the Python tool runtime environment")
}}

// Locked packages of the set
func (s *RuntimeSet) Requirements() ([]RuntimeRequirement, error) {
	var out []RuntimeRequirement
	err := json.Unmarshal(s.pinned, &out)
	return out, err
}

// Locked tool runtime set the binary embeds
func RuntimeRequirements() ([]RuntimeRequirement, error) {
	return ToolRuntime.Requirements()
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

// Whether venv has its interpreter and a dist-info of every set package this host needs, pinned versions when pinned
func (s *RuntimeSet) installedIn(venv string, pinned bool) (bool, error) {
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
				(!pinned || name == stem+r.Version+".dist-info") {
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

// Options of one runtime provisioning, explicit for an install the user asked for, else joined through the detached child
type RuntimeEnvironmentOptions struct {
	Explicit bool
	Output   io.Writer
}

// Runtime environment a caller needed that is not provisioned, with the refusal, deferral or failure that left it so
type RuntimeUnprovisioned struct {
	Venv string
	Err  error
}

func (e *RuntimeUnprovisioned) Error() string {
	return "the runtime environment " + e.Venv + " is not provisioned: " + e.Err.Error()
}

func (e *RuntimeUnprovisioned) Unwrap() error { return e.Err }

// Selected environment made to hold the runtime set, in this process when explicit, else by the single detached provisioning
func EnsureRuntimeEnvironment(ctx context.Context, opts RuntimeEnvironmentOptions) (string, error) {
	venv, err := SelectedVenv(ctx, RepoRoot())
	if err != nil {
		return "", err
	}
	if !opts.Explicit {
		return awaitRuntimeEnvironment(ctx, venv)
	}
	requirements, err := ToolRuntime.requirementsText()
	if err != nil {
		return "", err
	}
	gate := newProvisionGate(venv, requirements)
	release, err := gate.enter(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	if err := provisionUnderGate(ctx, gate, venv, opts, requirements); err != nil {
		return "", err
	}
	return venv, nil
}

// Selected environment and whether it holds the runtime set with no provisioning changing it or left unfinished
func RuntimeEnvironmentReady(ctx context.Context) (string, bool, error) {
	root := RepoRoot()
	venv, err := SelectedVenv(ctx, root)
	if err != nil {
		return "", false, err
	}
	requirements, err := ToolRuntime.requirementsText()
	if err != nil {
		return "", false, err
	}
	ready, err := runtimeSetReady(ctx, root, venv, newProvisionGate(venv, requirements))
	return venv, ready, err
}

// Whether venv holds the set, pinned in the tree's runtime only environment, any version where a project owns it
func runtimeSetReady(ctx context.Context, root, venv string, gate provisionGate) (bool, error) {
	if gate.unfinished() {
		return false, nil
	}
	owned, err := runtimeSetOwned(ctx, root)
	if err != nil {
		return false, err
	}
	return ToolRuntime.installedIn(venv, owned)
}

// Whether the selected environment of root is the runtime only tree's own, which holds exactly the pinned set
func runtimeSetOwned(ctx context.Context, root string) (bool, error) {
	if !runtimeOnlyProject(root) {
		return false, nil
	}
	recorded, err := recordedVenv(ctx, root)
	if err != nil || recorded != "" {
		return false, err
	}
	payload, err := PayloadVenv(root)
	return payload == "", err
}

// Installs the set holding the gate, a run an earlier one left unfinished reinstalling it, success clearing marker and record
func provisionUnderGate(ctx context.Context, gate provisionGate, venv string, opts RuntimeEnvironmentOptions, requirements []byte) error {
	reinstall := gate.unfinished()
	if err := gate.begin(); err != nil {
		return err
	}
	if err := provisionRuntimeSet(ctx, venv, opts, requirements, reinstall); err != nil {
		return err
	}
	return gate.finish()
}

// Builds the environment on the pinned toolchain when missing and installs the pinned requirement bytes into it
func provisionRuntimeSet(ctx context.Context, venv string, opts RuntimeEnvironmentOptions, requirements []byte, reinstall bool) error {
	toolchainCtx, cache, err := runtimeToolchainContext(ctx, venv)
	if err != nil {
		return err
	}
	env, err := ManagedEnvironment(toolchainCtx, venv, ManagedEnvironmentOptions{Explicit: opts.Explicit, Output: opts.Output, Cache: cache})
	if err != nil {
		return err
	}
	if !isRegularFile(env.Executable()) {
		if err := env.Create(ctx); err != nil {
			return err
		}
	}
	return env.installPinned(ctx, requirements, reinstall)
}

// Store beside the environment with its own facts, holding the interpreter and uv the environment is built on
func RuntimeSetStore(venv string) string {
	return venv + "-runtime"
}

// Toolchain context and uv cache of a runtime set environment, the default store when it holds the toolchain
func runtimeToolchainContext(ctx context.Context, venv string) (context.Context, string, error) {
	_, _, found, err := uvToolchain(ctx, false, false)
	if err != nil {
		return nil, "", err
	}
	if found {
		return ctx, "", nil
	}
	store := RuntimeSetStore(venv)
	return WithRuntimeStore(ctx, store), filepath.Join(store, "uv-cache"), nil
}

// Installs a hash pinned requirement set exactly as locked without resolving dependencies again, replacing installed copies when reinstall
func (e *PythonEnvironment) installPinned(ctx context.Context, requirements []byte, reinstall bool) error {
	temporary, err := os.MkdirTemp("", "pm-runtime-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	file := filepath.Join(temporary, "requirements.txt")
	if err := os.WriteFile(file, requirements, 0o666); err != nil {
		return err
	}
	command := []string{"pip", "install", "--no-config", "--python", e.Executable(), "--require-hashes", "--no-deps", "--requirements", file}
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

// Whether the tree has no project of its own, a binary install whose environment holds only the runtime set
func runtimeOnlyProject(root string) bool {
	return !isRegularFile(filepath.Join(root, "pyproject.toml"))
}
