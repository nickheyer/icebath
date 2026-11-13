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

// Embedded hash pinned package set a selected environment is provisioned with on first use
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
	reqs, err := s.Requirements()
	if err != nil {
		return nil, err
	}
	target, err := CurrentTarget()
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, r := range reqs {
		if !r.ServesTarget(target) {
			continue
		}
		b.WriteString(r.Name + "==" + r.Version + " ; " + r.Marker)
		for _, f := range r.Files {
			b.WriteString(" \\\n    --hash=sha256:" + f.SHA256)
		}
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}

// Options of one runtime provisioning, explicit for an install the user asked for, the tool runtime when Set is nil
type RuntimeEnvironmentOptions struct {
	Explicit bool
	Output   io.Writer
	Set      *RuntimeSet
}

// Selected environment made to hold the runtime set, created with the managed interpreter when missing
func EnsureRuntimeEnvironment(ctx context.Context, opts RuntimeEnvironmentOptions) (string, error) {
	set := opts.Set
	if set == nil {
		set = ToolRuntime
	}
	venv, err := SelectedVenv(ctx, RepoRoot())
	if err != nil {
		return "", err
	}
	if !opts.Explicit && !LazyInstallsAllowedFor(ctx) {
		return "", set.refuse(ctx)
	}
	env, err := ManagedEnvironment(ctx, venv, ManagedEnvironmentOptions{Explicit: opts.Explicit, Output: opts.Output})
	if err != nil {
		return "", err
	}
	if !isRegularFile(env.Executable()) {
		if err := env.Create(ctx); err != nil {
			return "", err
		}
	}
	requirements, err := set.requirementsText()
	if err != nil {
		return "", err
	}
	return venv, env.installPinned(ctx, requirements)
}

// Installs a hash pinned requirement set exactly as locked, without resolving dependencies again
func (e *PythonEnvironment) installPinned(ctx context.Context, requirements []byte) error {
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
