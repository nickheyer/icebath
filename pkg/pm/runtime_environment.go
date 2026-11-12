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

// Tool runtime packages of the hermes core sync, nemo-relay and the browser-harness closure, hash pinned from its lock
//
//go:embed runtime_requirements.json
var runtimeRequirementsJSON []byte

// One locked package of the runtime set with its marker and every artifact hermes's lock admits
type RuntimeRequirement struct {
	Name    string  `json:"name"`
	Version string  `json:"version"`
	Marker  string  `json:"marker"`
	Files   []Wheel `json:"files"`
}

// Locked runtime set the binary embeds
func RuntimeRequirements() ([]RuntimeRequirement, error) {
	var out []RuntimeRequirement
	err := json.Unmarshal(runtimeRequirementsJSON, &out)
	return out, err
}

// Requirements file pinning each package by version, marker and the hashes of its artifacts
func runtimeRequirementsText() ([]byte, error) {
	reqs, err := RuntimeRequirements()
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

// Options of one tool runtime provisioning, explicit for an install the user asked for
type RuntimeEnvironmentOptions struct {
	Explicit bool
	Output   io.Writer
}

// Selected environment made to hold the runtime set, created with the managed interpreter when missing
func EnsureRuntimeEnvironment(ctx context.Context, opts RuntimeEnvironmentOptions) (string, error) {
	venv, err := SelectedVenv(ctx, RepoRoot())
	if err != nil {
		return "", err
	}
	if !opts.Explicit && !LazyInstallsAllowedFor(ctx) {
		return "", RefuseLazyFor(ctx, "venv", "the Python tool runtime environment")
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
	requirements, err := runtimeRequirementsText()
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
