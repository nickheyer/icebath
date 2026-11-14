package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Interpreter the capability probe runs on, the selected environment python else the first one on PATH
func probePython(ctx context.Context) (string, error) {
	if python, err := pm.ProjectPython(ctx, pm.RepoRoot()); err == nil && isFile(python) {
		return python, nil
	}
	for _, name := range []string{"python3", "python"} {
		if found, err := exec.LookPath(name); err == nil {
			return found, nil
		}
	}
	return "", errors.New("no Python interpreter on PATH to run the capability probe")
}

// Kind and plugin context method names the probe script reads
func probeOptions(manifest map[string]any) map[string]any {
	kind := ""
	if config.PyTruthy(manifest["kind"]) {
		kind = config.PyStr(manifest["kind"])
	}
	return map[string]any{"kind": kind, "context_methods": pluginContextMethods()}
}

// Runs the recording probe in a scratch subprocess, the recorded registrations or the reason it gave none
func runCapabilityProbe(ctx context.Context, pluginDir string, manifest map[string]any) (map[string]any, string) {
	scratch, err := os.MkdirTemp("", "icebath-validate-")
	if err != nil {
		return nil, "capability probe produced no result: " + pyrepr.ExceptionMessage(err)
	}
	defer os.RemoveAll(scratch)
	python, err := probePython(ctx)
	if err != nil {
		return nil, "capability probe produced no result: " + err.Error()
	}
	options, err := json.Marshal(probeOptions(manifest))
	if err != nil {
		return nil, "capability probe produced no result: " + err.Error()
	}
	env := map[string]string{}
	for _, entry := range os.Environ() {
		if key, value, ok := strings.Cut(entry, "="); ok {
			env[key] = value
		}
	}
	env["ICEBATH_HOME"] = scratch
	env["PYTHONPATH"] = pm.RepoRoot()
	if existing := os.Getenv("PYTHONPATH"); existing != "" {
		env["PYTHONPATH"] += string(os.PathListSeparator) + existing
	}
	runCtx, cancel := context.WithTimeout(ctx, probeTimeout*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, python, "-P", "-c", probeScript, pluginDir, probeSentinel, string(options))
	cmd.Env = envList(env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return nil, fmt.Sprintf("capability probe timed out after %ds", probeTimeout)
	}
	var exit *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exit) {
		return nil, "capability probe produced no result: " + pyrepr.ExceptionMessage(runErr)
	}
	var payload map[string]any
	for _, line := range strings.Split(strings.ReplaceAll(strings.ToValidUTF8(stdout.String(), "�"), "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(line, probeSentinel) {
			continue
		}
		var decoded any
		payload = nil
		if json.Unmarshal([]byte(line[len(probeSentinel):]), &decoded) == nil {
			payload, _ = decoded.(map[string]any)
		}
	}
	if payload == nil {
		detail := fmt.Sprintf("capability probe produced no result (exit %d)", exitCode(cmd.ProcessState))
		if text := strings.TrimSpace(strings.ToValidUTF8(stderr.String(), "�")); text != "" {
			detail += ": " + text
		}
		return nil, detail
	}
	if reason, failed := payload["error"]; failed {
		return nil, config.PyStr(reason)
	}
	return payload, ""
}

// String items of a recorded registration list
func recordedNames(recorded map[string]any, kind string) []string {
	out := []string{}
	for _, item := range toList(recorded[kind]) {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Probes actual registrations and compares them with the declared capabilities, nil when the probe gave none
func checkCapabilities(ctx context.Context, report *ValidationReport, manifest map[string]any, pluginDir string) map[string]any {
	if !isFile(filepath.Join(pluginDir, "__init__.py")) {
		report.warn("no __init__.py — capability probe skipped (manifest-only plugin)")
		report.add("capability probe", true, "skipped (no __init__.py)")
		return nil
	}
	recorded, reason := runCapabilityProbe(ctx, pluginDir, manifest)
	if recorded == nil {
		report.add("capability probe", false, reason)
		return nil
	}
	if providers := recordedNames(recorded, "providers"); len(providers) > 0 {
		report.add("capability probe", true, "import registered provider(s): "+strings.Join(providers, ", "))
	} else {
		report.add("capability probe", true, "register() ran in isolation")
	}
	for _, pair := range [][2]string{{"tools", "provides_tools"}, {"hooks", "provides_hooks"}, {"middleware", "provides_middleware"}} {
		kind, manifestKey := pair[0], pair[1]
		declared := toSet(declaredList(manifest, manifestKey))
		actual := toSet(recordedNames(recorded, kind))
		var undeclared, unregistered []string
		for name := range actual {
			if !declared[name] {
				undeclared = append(undeclared, name)
			}
		}
		for name := range declared {
			if !actual[name] {
				unregistered = append(unregistered, name)
			}
		}
		sort.Strings(undeclared)
		sort.Strings(unregistered)
		if len(undeclared) > 0 {
			report.add("declared "+kind, false, fmt.Sprintf("undeclared %s registered (not in %s): %s", kind, manifestKey, strings.Join(undeclared, ", ")))
		} else {
			report.add("declared "+kind, true, "matches registrations")
		}
		if len(unregistered) > 0 {
			report.warn(fmt.Sprintf("%s declares %s but register() did not register them", manifestKey, strings.Join(unregistered, ", ")))
		}
	}
	return recorded
}

// Fails tool names that collide with built in tools, declared ones and those the probe saw registered
func checkBuiltinCollisionsWith(report *ValidationReport, manifest map[string]any, recorded map[string]any) {
	candidates := toSet(declaredList(manifest, "provides_tools"))
	for _, name := range recordedNames(recorded, "tools") {
		candidates[name] = true
	}
	if len(candidates) == 0 {
		report.add("built-in tool collisions", true, "no tools to check")
		return
	}
	collisions := builtinToolCollisions(candidates)
	if len(collisions) > 0 {
		report.add("built-in tool collisions", false, "tool name(s) collide with built-in tools: "+strings.Join(collisions, ", "))
		return
	}
	report.add("built-in tool collisions", true, "no collisions")
}

// Reports trusted inbound platforms the probe saw, failing one that ships with core
func checkTrustedInbound(report *ValidationReport, recorded map[string]any) error {
	names := recordedNames(recorded, "trusted_inbound")
	if len(names) == 0 {
		return nil
	}
	registry, err := needService("Platforms", func(s *Services) PlatformRegistry { return s.Platforms })
	if err != nil {
		return err
	}
	for _, name := range names {
		if registry.CoreShipsPlatform(name) {
			report.add("trusted inbound", false, fmt.Sprintf("platform '%s' ships with core; trusted_inbound is refused for it", name))
		} else {
			report.add("trusted inbound", true, fmt.Sprintf("platform '%s' is trusted_inbound: its events skip user allowlists and pairing", name))
		}
	}
	return nil
}

// Checks that declared Python dependencies are well formed requirements the installer accepts
func checkPythonDependencies(report *ValidationReport, pluginDir string) {
	declaration, err := pm.ReadPythonDeclaration(pluginDir)
	var urls, installable []string
	if err == nil && declaration.External() {
		report.add("python dependencies", true, "external runtime (plugin manages its own)")
		return
	}
	if err == nil {
		urls, err = pm.UnsupportedRequirements(declaration.Requirements)
	}
	if err == nil {
		installable, err = declaration.InstallRequirements()
	}
	if err != nil {
		report.add("python dependencies", false, "declaration invalid: "+pyrepr.ExceptionMessage(err))
		return
	}
	if len(urls) > 0 {
		report.warn("python dependencies: direct URL requirement(s) are not managed by PM; " +
			"use a plugin-owned external runtime: " + strings.Join(urls, ", "))
	}
	detail := "none declared"
	if len(declaration.Requirements) > 0 {
		source := "manifest"
		if declaration.Pyproject != "" {
			source = "pyproject"
		}
		detail = fmt.Sprintf("%d requirements from %s", len(installable), source)
	}
	report.add("python dependencies", true, detail)
}
