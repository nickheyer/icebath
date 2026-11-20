package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
	"gopkg.in/yaml.v3"
)

var vtBaseManifest = map[string]any{"name": "fixture-plugin", "version": "1.0.0", "description": "A fixture plugin."}

// Manifest of the base fixture with the given keys overridden
func vtManifest(over map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range vtBaseManifest {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// Plugin directory with a plugin.yaml and an __init__.py, named after the manifest
func vtPlugin(t *testing.T, root string, manifest map[string]any, initPy string) string {
	t.Helper()
	name, _ := manifest["name"].(string)
	if name == "" {
		name = "fixture-plugin"
	}
	dir := filepath.Join(root, name)
	raw, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	vtWrite(t, filepath.Join(dir, "plugin.yaml"), string(raw))
	if initPy == "" {
		initPy = "def register(ctx):\n    pass\n"
	}
	vtWrite(t, filepath.Join(dir, "__init__.py"), initPy)
	return dir
}

// Every native check, the source audits included
func vtNativeChecks(t *testing.T, dir string) *ValidationReport {
	t.Helper()
	report, err := validatePluginDir(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func vtHasCheck(r *ValidationReport, name string, ok bool) bool {
	for _, c := range r.Checks {
		if c.Name == name && c.OK == ok {
			return true
		}
	}
	return false
}

func vtPortable(t *testing.T, root string, servers, declarations map[string]any) string {
	t.Helper()
	manifest, _ := json.Marshal(map[string]any{"$schema": PluginSchemaV1, "name": "example-plugin",
		"extensions": map[string]any{PortableExtension: map[string]any{"servers": declarations}}})
	mcp, _ := json.Marshal(map[string]any{"$schema": MCPSchemaV1, "mcpServers": servers})
	vtWrite(t, filepath.Join(root, "plugin.json"), string(manifest))
	vtWrite(t, filepath.Join(root, "mcp.json"), string(mcp))
	return root
}

func TestPortableValidationFailsOrphanAndReportsAvailability(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var asked []AgentPluginServerDeclaration
	InstallServices(&Services{PortableServerAvailability: func(_ context.Context, decl AgentPluginServerDeclaration) (PortableAvailability, error) {
		asked = append(asked, decl)
		return PortableAvailability{State: "missing_app"}, nil
	}})
	defer InstallServices(nil)
	tmp := t.TempDir()
	orphan := vtPortable(t, filepath.Join(tmp, "orphan"), map[string]any{}, map[string]any{"worker": map[string]any{"requires": map[string]any{"app": false}}})
	report, err := validatePluginDir(context.Background(), orphan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() || !strings.Contains(strings.Join(report.Failures(), "\n"), "no matching mcp.json server") {
		t.Fatalf("orphan declaration must fail: %v", report.Failures())
	}
	declared := vtPortable(t, filepath.Join(tmp, "declared"), map[string]any{"worker": map[string]any{"type": "stdio", "command": "python"}},
		map[string]any{"worker": map[string]any{"app": map[string]any{"darwin": map[string]any{"presence": "executable", "location": filepath.Join(tmp, "example-app")}},
			"requires": map[string]any{"app": true}}})
	report, err = validatePluginDir(context.Background(), declared, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range report.Checks {
		found = found || (c.Name == "server availability: worker" && c.OK && c.Detail == "missing_app")
	}
	if !found || len(asked) != 1 {
		t.Fatalf("availability not reported: %+v asked=%d", report.Checks, len(asked))
	}
}

func TestConfigSchemaAdmitsEveryTypeTheLoaderAccepts(t *testing.T) {
	schema := map[string]any{}
	for typ := range ConfigSchemaTypes {
		schema["k_"+typ] = map[string]any{"type": typ}
	}
	schema["api_key"] = map[string]any{"type": "secret", "env": "FIXTURE_API_KEY", "description": "token"}
	report := vtNativeChecks(t, vtPlugin(t, t.TempDir(), vtManifest(map[string]any{"config_schema": schema}), ""))
	found := false
	for _, c := range report.Checks {
		found = found || c == (ValidationCheck{"config schema", true, "shape valid"})
	}
	if !found {
		t.Fatalf("every loader type must pass: %v", report.Failures())
	}
	bad := vtNativeChecks(t, vtPlugin(t, filepath.Join(t.TempDir(), "bad"), vtManifest(map[string]any{"config_schema": map[string]any{"x": map[string]any{"type": "mapping"}}}), ""))
	if !vtHasCheck(bad, "config schema", false) || vtHasCheck(bad, "config schema", true) {
		t.Fatalf("unknown type must fail: %+v", bad.Checks)
	}
	ordered := &ValidationReport{}
	raw := []byte("config_schema:\n  zeta: 1\n  alpha: {required: maybe}\n")
	checkConfigSpec(ordered, map[string]any{"zeta": 1, "alpha": map[string]any{"required": "maybe"}}, yamlMappingKeyOrder(raw, "config_schema"))
	want := "config_schema.zeta: must be a mapping (e.g. {type: str}); config_schema.alpha: required must be a boolean"
	if ordered.Checks[0].Detail != want {
		t.Fatalf("problems must follow the manifest order: %q", ordered.Checks[0].Detail)
	}
}

func TestValidateReportJSONKeyOrder(t *testing.T) {
	r := &ValidationReport{}
	r.add("manifest", true, "plugin.yaml parses")
	r.warn("w")
	raw, err := json.Marshal(r.ToDict())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ok":true,"checks":[{"name":"manifest","ok":true,"detail":"plugin.yaml parses"}],"warnings":["w"]}`
	if string(raw) != want {
		t.Fatalf("%s", raw)
	}
}

// Stands in a synced dependency environment for validate with install deps, every seam restored after the test
func vtSyncedEnvironment(t *testing.T, sync func(context.Context, pm.SyncVenvOptions) error, python, venv string, env []string) *[][]string {
	t.Helper()
	savedSync, savedPython, savedVenv, savedCommand, savedEnv := syncVenv, projectPython, selectedVenv, venvCommand, activationEnvironment
	t.Cleanup(func() {
		syncVenv, projectPython, selectedVenv, venvCommand, activationEnvironment = savedSync, savedPython, savedVenv, savedCommand, savedEnv
	})
	commands := &[][]string{}
	syncVenv = sync
	projectPython = func(context.Context, string) (string, error) { return python, nil }
	selectedVenv = func(context.Context, string) (string, error) { return venv, nil }
	venvCommand = func(_ context.Context, _, gotVenv string, options []string) ([]string, error) {
		*commands = append(*commands, append([]string{gotVenv}, options...))
		return append([]string{python}, options...), nil
	}
	activationEnvironment = func(context.Context, string) ([]string, error) { return env, nil }
	return commands
}

// Checks of a JSON validation report by name, a CommandExit accepted the way SystemExit is
func vtJSONChecks(t *testing.T, out []byte, err error) map[string]map[string]any {
	t.Helper()
	var exit *CommandExit
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	var doc struct {
		Checks []map[string]any `json:"checks"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("report %q: %v", out, err)
	}
	checks := map[string]map[string]any{}
	for _, c := range doc.Checks {
		checks[c["name"].(string)] = c
	}
	return checks
}

// Process environment with PYTHONPATH replaced by the given entries
func vtEnvironWithPythonPath(parts ...string) []string {
	env := []string{"PYTHONPATH=" + strings.Join(parts, string(os.PathListSeparator))}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "PYTHONPATH=") {
			env = append(env, entry)
		}
	}
	return env
}

func TestInstallDepsProbeImportsFromTheSyncedEnvironment(t *testing.T) {
	tmp := t.TempDir()
	deps := filepath.Join(tmp, "synced-site-packages")
	vtWrite(t, filepath.Join(deps, "probe_only_dep.py"), "VALUE = 1\n")
	plugin := vtPlugin(t, tmp, map[string]any{"name": "needs-dep", "version": "1.0.0", "description": "d"},
		"import probe_only_dep\n\ndef register(ctx):\n    pass\n")
	python := pubPython(t)
	vtSyncedEnvironment(t, func(context.Context, pm.SyncVenvOptions) error { return nil }, python,
		filepath.Join(tmp, "synced-venv"), vtEnvironWithPythonPath(pm.RepoRoot(), deps))
	var out, errOut bytes.Buffer
	err := CmdValidate(context.Background(), plugin, true, true, &out, &errOut)
	checks := vtJSONChecks(t, out.Bytes(), err)
	if probe := checks["capability probe"]; probe == nil || probe["ok"] != true {
		t.Fatalf("capability probe %v", probe)
	}
}

// A failed sync prints the preparation failure to stderr and validation still runs on this install's interpreter
func TestInstallDepsSyncFailureStillValidates(t *testing.T) {
	tmp := t.TempDir()
	plugin := vtPlugin(t, tmp, vtBaseManifest, "")
	var synced []pm.SyncVenvOptions
	commands := vtSyncedEnvironment(t, func(_ context.Context, opts pm.SyncVenvOptions) error {
		synced = append(synced, opts)
		return &pm.PyError{Class: "RuntimeError", Message: "resolver said no"}
	}, pubPython(t), filepath.Join(tmp, "synced-venv"), os.Environ())
	var out, errOut bytes.Buffer
	err := CmdValidate(context.Background(), plugin, true, true, &out, &errOut)
	checks := vtJSONChecks(t, out.Bytes(), err)
	if errOut.String() != "dependency preparation failed: resolver said no\n" {
		t.Fatalf("stderr %q", errOut.String())
	}
	if probe := checks["capability probe"]; probe == nil || probe["ok"] != true || len(*commands) != 0 {
		t.Fatalf("capability probe %v, venv commands %v", probe, *commands)
	}
	if len(synced) != 1 || synced[0].Stdout != &out || !reflect.DeepEqual(synced[0].Plugins, &pm.Candidates{Dirs: []string{plugin}}) {
		t.Fatalf("sync calls %+v", synced)
	}
}

// A synced install whose selected interpreter is missing probes this install's interpreter without a venv command
func TestInstallDepsWithoutACommittedInterpreterProbesInProcessEnvironment(t *testing.T) {
	tmp := t.TempDir()
	plugin := vtPlugin(t, tmp, vtBaseManifest, "")
	commands := vtSyncedEnvironment(t, func(context.Context, pm.SyncVenvOptions) error { return nil },
		filepath.Join(tmp, "no-venv", "bin", "python"), filepath.Join(tmp, "no-venv"), nil)
	var out, errOut bytes.Buffer
	err := CmdValidate(context.Background(), plugin, true, true, &out, &errOut)
	checks := vtJSONChecks(t, out.Bytes(), err)
	if probe := checks["capability probe"]; probe == nil || probe["ok"] != true || len(*commands) != 0 || errOut.Len() != 0 {
		t.Fatalf("capability probe %v, venv commands %v, stderr %q", probe, *commands, errOut.String())
	}
}

// An interrupted sync escapes validation the way KeyboardInterrupt escapes except Exception
func TestInstallDepsInterruptedSyncEscapes(t *testing.T) {
	tmp := t.TempDir()
	plugin := vtPlugin(t, tmp, vtBaseManifest, "")
	vtSyncedEnvironment(t, func(context.Context, pm.SyncVenvOptions) error { return ErrUserInterrupt },
		pubPython(t), filepath.Join(tmp, "synced-venv"), os.Environ())
	var out, errOut bytes.Buffer
	if err := CmdValidate(context.Background(), plugin, true, true, &out, &errOut); !errors.Is(err, ErrUserInterrupt) || out.Len() != 0 || errOut.Len() != 0 {
		t.Fatalf("err %v, stdout %q, stderr %q", err, out.String(), errOut.String())
	}
}

// Writes a file and its missing parent directories
func vtWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Detail of every failed check, its name when the detail is empty
func (r *ValidationReport) Failures() []string {
	out := []string{}
	for _, c := range r.Checks {
		if !c.OK {
			if c.Detail != "" {
				out = append(out, c.Detail)
			} else {
				out = append(out, c.Name)
			}
		}
	}
	return out
}
