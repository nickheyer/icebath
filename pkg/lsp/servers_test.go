package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func swapWhich(t *testing.T, fn func(names ...string) string) {
	t.Helper()
	prev := which
	which = fn
	t.Cleanup(func() { which = prev })
}

func TestBladeTemplatesRouteToLaravelLSPAndPlainPHPStaysOnIntelephense(t *testing.T) {
	rt, _, _ := testRuntime(t)
	blade := "/app/resources/views/Home.BLADE.php"
	if s := FindServerForFile(blade, nil); s.ServerID != "laravel-lsp" || LanguageIDFor(blade, nil) != "blade" {
		t.Fatalf("blade routed to %s", s.ServerID)
	}
	if s := FindServerForFile("/app/app/Models/User.php", nil); s.ServerID != "intelephense" || LanguageIDFor("/app/app/Models/User.php", nil) != "php" {
		t.Fatal("php routing")
	}
	srv := FindServerForFile(blade, nil)
	dir := t.TempDir()
	swapWhich(t, func(...string) string { return "" })
	if spec, err := srv.BuildSpawn(dir, ServerContext{WorkspaceRoot: dir, InstallStrategy: "manual", Runtime: rt}); err != nil || spec != nil {
		t.Fatalf("missing binary must skip, got %v %v", spec, err)
	}
	swapWhich(t, func(...string) string { return "/usr/local/bin/laravel-lsp" })
	spec, err := srv.BuildSpawn(dir, ServerContext{WorkspaceRoot: dir, InstallStrategy: "manual", Runtime: rt})
	must(t, err)
	if !reflect.DeepEqual(spec.Command, []string{"/usr/local/bin/laravel-lsp", "lsp"}) {
		t.Fatalf("command %v", spec.Command)
	}
}

func TestCustomServerPrecedesBuiltinsAndCarriesLanguageID(t *testing.T) {
	rt, h, _ := testRuntime(t)
	mock := mockBinary(t)
	cfg := map[string]any{
		"pyright": map[string]any{"disabled": true},
		"blade":   map[string]any{"command": []any{mock}, "extensions": []any{".PHP"}, "language_id": "blade"},
		"broken":  map[string]any{"command": "not-a-list", "extensions": []any{".zzz"}},
	}
	custom := CustomServers(rt, cfg)
	if len(custom) != 1 || custom[0].ServerID != "blade" {
		t.Fatalf("custom %v", custom)
	}
	if len(h.matching("lsp.servers.broken", "ignored")) != 1 {
		t.Fatal("malformed entry must warn")
	}
	registry := append(custom, Servers...)
	if FindServerForFile("/w/index.php", registry).ServerID != "blade" || FindServerForFile("/w/index.php", nil).ServerID != "intelephense" {
		t.Fatal("routing")
	}
	if LanguageIDFor("/w/index.php", custom[0]) != "blade" || LanguageIDFor("/w/index.php", nil) != "php" {
		t.Fatal("language id")
	}
	spec, err := custom[0].BuildSpawn("/w", ServerContext{WorkspaceRoot: "/w", InstallStrategy: "manual", Runtime: rt})
	must(t, err)
	if spec == nil || !reflect.DeepEqual(spec.Command, []string{mock}) {
		t.Fatalf("spec %v", spec)
	}
}

func makeFakeBundle(t *testing.T, root string) string {
	bundle := filepath.Join(root, "PowerShellEditorServices")
	writeFile(t, filepath.Join(bundle, "PowerShellEditorServices", "Start-EditorServices.ps1"), "# fake")
	return bundle
}

func TestPowershellExtensionsRouteToPSES(t *testing.T) {
	for _, f := range []string{"script.ps1", "module.psm1", "manifest.psd1"} {
		if s := FindServerForFile(f, nil); s == nil || s.ServerID != "powershell" {
			t.Fatalf("%s routed to %v", f, s)
		}
	}
}

func TestPowershellSpawnBuildsCommandWithBundleViaEnv(t *testing.T) {
	rt, _, _ := testRuntime(t)
	swapWhich(t, func(...string) string { return "/usr/bin/pwsh" })
	tmp := t.TempDir()
	bundle := makeFakeBundle(t, tmp)
	t.Setenv("PSES_BUNDLE_PATH", bundle)
	spec, err := spawnPowershellES(tmp, ServerContext{WorkspaceRoot: tmp, InstallStrategy: "manual", Runtime: rt})
	must(t, err)
	last := spec.Command[len(spec.Command)-1]
	if spec.Command[0] != "/usr/bin/pwsh" || !strings.Contains(last, "-Stdio") || !strings.Contains(last, "Start-EditorServices.ps1") ||
		!strings.Contains(last, bundle) || !slices.Contains(spec.Command, "-NonInteractive") || !slices.Contains(spec.Command, "-NoProfile") {
		t.Fatalf("command %v", spec.Command)
	}
}

func TestPowershellBundlePathInitOverrideNotLeaked(t *testing.T) {
	rt, _, _ := testRuntime(t)
	swapWhich(t, func(...string) string { return "/usr/bin/pwsh" })
	t.Setenv("PSES_BUNDLE_PATH", "")
	tmp := t.TempDir()
	bundle := makeFakeBundle(t, tmp)
	spec, err := spawnPowershellES(tmp, ServerContext{WorkspaceRoot: tmp, InstallStrategy: "manual", Runtime: rt,
		InitOverrides: map[string]map[string]any{"powershell": {"bundlePath": bundle, "foo": "bar"}}})
	must(t, err)
	if _, leaked := spec.InitializationOptions["bundlePath"]; leaked || spec.InitializationOptions["foo"] != "bar" {
		t.Fatalf("init %v", spec.InitializationOptions)
	}
}

// Staged vue launcher and node_modules tree under the runtime home
func stageVue(t *testing.T, rt *Runtime, version string, jsSDK bool) {
	nodeModules := filepath.Join(rt.Home, "lsp", "node_modules")
	pkg := filepath.Join(nodeModules, "@vue", "language-server")
	b, _ := json.Marshal(map[string]any{"version": version})
	writeFile(t, filepath.Join(pkg, "package.json"), string(b))
	staged := writeFile(t, filepath.Join(rt.Home, "lsp", "bin", "vue-language-server"), "#!/usr/bin/env node\n")
	must(t, os.Chmod(staged, 0o755))
	if jsSDK {
		writeFile(t, filepath.Join(nodeModules, "typescript", "lib", "typescript.js"), "")
	}
}

func TestVueSpawnsOnlyASelfHostingServerWithAJSTypescriptSDK(t *testing.T) {
	swapWhich(t, func(...string) string { return "" })
	for _, tc := range []struct {
		version string
		jsSDK   bool
		spawns  bool
	}{{"2.2.12", true, true}, {"3.3.11", true, false}, {"2.2.12", false, false}} {
		rt, _, _ := testRuntime(t)
		stageVue(t, rt, tc.version, tc.jsSDK)
		root := filepath.Join(t.TempDir(), "project")
		must(t, os.MkdirAll(root, 0o755))
		spec, err := FindServerForFile("App.vue", nil).BuildSpawn(root, ServerContext{WorkspaceRoot: root, InstallStrategy: "manual", Runtime: rt})
		must(t, err)
		if !tc.spawns {
			if spec != nil {
				t.Fatalf("%v: spawned %v", tc, spec.Command)
			}
			continue
		}
		if spec == nil || !reflect.DeepEqual(spec.Command[1:], []string{"--stdio"}) ||
			!reflect.DeepEqual(spec.InitializationOptions["vue"], map[string]any{"hybridMode": false}) {
			t.Fatalf("%v: spec %+v", tc, spec)
		}
		tsdk := spec.InitializationOptions["typescript"].(map[string]any)["tsdk"].(string)
		if !isFile(filepath.Join(tsdk, "typescript.js")) {
			t.Fatalf("tsdk %q", tsdk)
		}
	}
}

func TestJSToolchainRecipesPinAJavascriptTypescriptSDK(t *testing.T) {
	major := func(spec, name string) int {
		if !strings.HasPrefix(spec, name+"@") {
			t.Fatalf("unpinned %s", spec)
		}
		head, _, _ := strings.Cut(spec[len(name)+1:], ".")
		n, err := pyInt(head)
		must(t, err)
		return int(n)
	}
	vue, tsls := InstallRecipes["@vue/language-server"], InstallRecipes["typescript-language-server"]
	if major(vue.Pkg, "@vue/language-server") >= 3 {
		t.Fatal("vue must stay below the tunnel only 3.x line")
	}
	for _, r := range []Recipe{vue, tsls} {
		var sdk []string
		for _, p := range r.ExtraPkgs {
			if strings.Split(p, "@")[0] == "typescript" {
				sdk = append(sdk, p)
			}
		}
		if len(sdk) == 0 || major(sdk[0], "typescript") >= 7 {
			t.Fatalf("recipe %+v", r)
		}
	}
}

func TestPyrightUsesTheProjectInterpreterBeforeIcebath(t *testing.T) {
	rt, _, _ := testRuntime(t)
	tmp := t.TempDir()
	project := filepath.Join(tmp, "project")
	must(t, os.MkdirAll(project, 0o755))
	t.Setenv("VIRTUAL_ENV", "")
	pmPython := writeFile(t, filepath.Join(tmp, "payload", "python", "bin", "python3"), "")
	rt.InstalledBinary = func(name string) (string, bool) {
		if name == "python" && isFile(pmPython) {
			return pmPython, true
		}
		return "", false
	}
	self, _ := os.Executable()
	ctx := ServerContext{WorkspaceRoot: project, InstallStrategy: "off", BinaryOverrides: map[string][]string{"pyright": {self}},
		Trusted: true, Runtime: rt}
	server := FindServerForFile(filepath.Join(project, "app.py"), nil)
	pythonPath := func() any {
		spec, err := server.BuildSpawn(project, ctx)
		must(t, err)
		py, _ := spec.InitializationOptions["python"].(map[string]any)
		return py["pythonPath"]
	}
	if got := pythonPath(); got != pmPython {
		t.Fatalf("managed python fallback %v", got)
	}
	must(t, os.Remove(pmPython))
	if got := pythonPath(); got != nil {
		t.Fatalf("no interpreter at all must leave pythonPath out, got %v", got)
	}
	projectPython := writeFile(t, filepath.Join(project, ".venv", "bin", "python"), "")
	if got := pythonPath(); got != projectPython {
		t.Fatalf("project venv %v", got)
	}
	explicit := filepath.Join(tmp, "explicit-environment")
	explicitPython := writeFile(t, filepath.Join(explicit, "bin", "python"), "")
	t.Setenv("VIRTUAL_ENV", explicit)
	if got := pythonPath(); got != explicitPython {
		t.Fatalf("operator VIRTUAL_ENV must win, got %v", got)
	}
	ctx.Trusted = false
	t.Setenv("VIRTUAL_ENV", "")
	if got := pythonPath(); got != nil {
		t.Fatalf("an untrusted workspace never gets its own venv, got %v", got)
	}
}

// npm's Windows layout, a POSIX shim under the bare name and a runnable cmd beside it
func npmShimPair(t *testing.T, binDir, name string) string {
	shim := writeFile(t, filepath.Join(binDir, name), "#!/bin/sh\nexec node \"$0.js\" \"$@\"\n")
	cmd := writeFile(t, filepath.Join(binDir, name+".cmd"), "@echo off\r\necho wrapper-ran\r\n")
	must(t, os.Chmod(shim, 0o755))
	must(t, os.Chmod(cmd, 0o755))
	return cmd
}

func TestWindowsResolutionPicksCmdWrapperFromNpmBinOverPosixShim(t *testing.T) {
	rt, _, _ := testRuntime(t)
	npmBin := filepath.Join(rt.Home, "lsp", "node_modules", ".bin")
	cmd := npmShimPair(t, npmBin, "tool")
	got, err := existingBinary(rt, "tool", true)
	must(t, err)
	if got != cmd {
		t.Fatalf("windows resolution %q", got)
	}
	if got, _ := existingBinary(rt, "tool", false); got != "" {
		t.Fatalf("POSIX resolution never looks in npm's bin dir, got %q", got)
	}
	pyrightCmd := npmShimPair(t, npmBin, "pyright")
	langserverCmd := npmShimPair(t, npmBin, "pyright-langserver")
	spec, err := spawnPyright(rt.Home, ServerContext{WorkspaceRoot: rt.Home, BinaryOverrides: map[string][]string{"pyright": {pyrightCmd}}, Runtime: rt})
	must(t, err)
	if spec == nil || spec.Command[0] != langserverCmd {
		t.Fatalf("spec %+v", spec)
	}
}

func TestFileExtOrBasenameAndSplitext(t *testing.T) {
	cases := map[string]string{"/a/Dockerfile": "Dockerfile", "/a/b.PY": ".py", "/a/.bashrc": ".bashrc", "/a/x.tar.gz": ".gz",
		"/a/v.Blade.PHP": ".blade.php", "/a/..x": "..x"}
	for in, want := range cases {
		if got := fileExtOrBasename(in); got != want {
			t.Errorf("%s: %q want %q", in, got, want)
		}
	}
	if !StaticRegistryHandles(".tsx") || StaticRegistryHandles(".zzz") {
		t.Fatal("static registry")
	}
}
