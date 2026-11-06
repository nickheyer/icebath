package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Temporary process home with the plugin host child running on the system python
func doctorTestHome(t *testing.T) (context.Context, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required to run the plugin host child: %v", err)
	}
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	installHostEnvHooks()
	previous := hostInterpreter
	hostInterpreter = func(context.Context) (string, error) { return python, nil }
	t.Cleanup(func() { hostInterpreter = previous })
	return homeCtx(h), h
}

func doctorPluginDir(t *testing.T, dir, manifest, init string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "plugin.yaml"), manifest)
	if init != "" {
		writeFile(t, filepath.Join(dir, "__init__.py"), init)
	}
	return dir
}

func resolvedPath(p string) string {
	r, _ := pypath.Resolve(p)
	return r
}

func runDoctor(t *testing.T, ctx context.Context, target string) *PluginDoctorReport {
	t.Helper()
	report, err := doctorPlugin(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func findingText(r *PluginDoctorReport) string {
	var parts []string
	for _, f := range r.Findings {
		parts = append(parts, f.Message)
	}
	return strings.Join(parts, "\n")
}

// Records every staging copy source, failing the test once a copy runs away
func trackDoctorCopies(t *testing.T, fail error) *[]string {
	t.Helper()
	var copied []string
	previous := doctorCopyTree
	doctorCopyTree = func(src, dst string, ignore pyfs.IgnoreErrFunc) error {
		copied = append(copied, src)
		if fail != nil {
			return fail
		}
		if len(copied) > 20 {
			return errors.New("runaway staging copy")
		}
		return previous(src, dst, ignore)
	}
	t.Cleanup(func() { doctorCopyTree = previous })
	return &copied
}

const doctorNoopInit = "def register(ctx):\n    pass\n"

func TestDoctorUsesRegistrationToRejectBadHookAndCallbackSignature(t *testing.T) {
	ctx, _ := doctorTestHome(t)
	plugin := doctorPluginDir(t, filepath.Join(t.TempDir(), "bad-plugin"),
		"name: bad-plugin\nversion: 0.1.0\ndescription: broken contract\nprovides_hooks:\n  - typo_hook\n  - pre_tool_call\n",
		"def callback(tool_name):\n    return None\n\ndef register(ctx):\n    ctx.register_hook('typo_hook', callback)\n    ctx.register_hook('pre_tool_call', callback)\n")
	report := runDoctor(t, ctx, plugin)
	messages := findingText(report)
	if report.OK() || !strings.Contains(messages, "unknown hook 'typo_hook'") || !strings.Contains(messages, "must accept **kwargs") {
		t.Fatal(report.FormatText())
	}
}

func TestDoctorAcceptsManifestDefaultsFromRuntimeParser(t *testing.T) {
	ctx, _ := doctorTestHome(t)
	plugin := doctorPluginDir(t, filepath.Join(t.TempDir(), "minimal"), "name: minimal\n", doctorNoopInit)
	report := runDoctor(t, ctx, plugin)
	if !report.OK() || report.Manifest == nil || report.Manifest.Kind != "standalone" {
		t.Fatal(report.FormatText())
	}
}

func TestDoctorRestoresGlobalToolPolicyAndModuleState(t *testing.T) {
	ctx, h := doctorTestHome(t)
	plugin := doctorPluginDir(t, filepath.Join(t.TempDir(), "cleanup-plugin"),
		"name: cleanup-plugin\nprovides_tools: [cleanup_plugin_ping]\n",
		"import json\n\ndef ping(args, **kwargs):\n    return json.dumps({'ok': True})\n\ndef register(ctx):\n"+
			"    ctx.register_tool(name='cleanup_plugin_ping', toolset='cleanup', schema={'name': 'cleanup_plugin_ping', 'description': 'test', 'parameters': {'type': 'object'}}, handler=ping)\n")
	module := NewManager(h).policyModuleName(&Manifest{Name: "cleanup-plugin", Key: "cleanup-plugin", Source: "user", Path: plugin})
	policyBefore := tools.Default.SnapshotPluginOverridePolicy(module, h)
	report := runDoctor(t, ctx, plugin)
	if !report.OK() || !reflect.DeepEqual(report.RegisteredTools, []string{"cleanup_plugin_ping"}) {
		t.Fatal(report.FormatText())
	}
	for _, scope := range []string{"", h} {
		if def := tools.Default.Lookup(ctx, scope, "cleanup_plugin_ping"); def != nil {
			t.Fatalf("tool left registered in scope %q", scope)
		}
	}
	if tools.Default.SnapshotPluginOverridePolicy(module, h) != policyBefore {
		t.Fatal("override policy changed")
	}
}

func TestDoctorBlocksLiveNetwork(t *testing.T) {
	ctx, _ := doctorTestHome(t)
	plugin := doctorPluginDir(t, filepath.Join(t.TempDir(), "network-plugin"), "name: network-plugin\n",
		"import socket\n\ndef register(ctx):\n    socket.create_connection(('example.com', 443))\n")
	report := runDoctor(t, ctx, plugin)
	if report.OK() || !strings.Contains(report.FormatText(), "network access is disabled while Plugin Doctor runs") {
		t.Fatal(report.FormatText())
	}
}

func TestDoctorDefaultTargetDoesNotCopyCwd(t *testing.T) {
	ctx, _ := doctorTestHome(t)
	workdir := filepath.Join(t.TempDir(), "workdir")
	if err := os.MkdirAll(filepath.Join(workdir, "big"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(workdir, "big", "payload.bin"), strings.Repeat("x", 1024))
	t.Chdir(workdir)
	copied := trackDoctorCopies(t, nil)
	if report := runDoctor(t, ctx, ""); report.OK() || len(*copied) != 0 {
		t.Fatalf("%v %s", *copied, report.FormatText())
	}
}

func TestDoctorDoesNotCopyHomeWithAPluginCheckout(t *testing.T) {
	ctx, _ := doctorTestHome(t)
	fakeHome := filepath.Join(t.TempDir(), "fake-home")
	if err := os.MkdirAll(filepath.Join(fakeHome, ".icebath", "cache", "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	plugin := doctorPluginDir(t, filepath.Join(fakeHome, "plugin-checkout"), "name: checkout\n", "")
	t.Chdir(fakeHome)
	copied := trackDoctorCopies(t, errors.New("copytree must not run"))
	if got, err := ResolvePluginPath(ctx, plugin); err != nil || got != resolvedPath(plugin) {
		t.Fatalf("%q %v", got, err)
	}
	report := runDoctor(t, ctx, ".")
	if len(*copied) != 0 || report.OK() || !strings.Contains(report.FormatText(), "holds no plugin manifest") {
		t.Fatalf("%v %s", *copied, report.FormatText())
	}
}

func TestResolveAcceptsCategoryLayout(t *testing.T) {
	ctx, _ := doctorTestHome(t)
	category := filepath.Join(t.TempDir(), "image_gen")
	doctorPluginDir(t, filepath.Join(category, "openai"), "name: openai\n", "")
	if got, err := ResolvePluginPath(ctx, category); err != nil || got != resolvedPath(category) {
		t.Fatalf("%q %v", got, err)
	}
}

func TestDoctorStagesOnlyThePluginOfACategory(t *testing.T) {
	ctx, _ := doctorTestHome(t)
	category := filepath.Join(t.TempDir(), "tools")
	plugin := doctorPluginDir(t, filepath.Join(category, "minimal"), "name: minimal\n", doctorNoopInit)
	writeFile(t, filepath.Join(category, "disk-image.iso"), strings.Repeat("x", 1024))
	for _, skipped := range []string{"__data__", ".git", ".claude-plugin"} {
		if err := os.Mkdir(filepath.Join(category, skipped), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copied := trackDoctorCopies(t, nil)
	report := runDoctor(t, ctx, category)
	if !report.OK() || !reflect.DeepEqual(*copied, []string{resolvedPath(plugin)}) {
		t.Fatalf("%v %s", *copied, report.FormatText())
	}
	if report.Manifest == nil || report.Manifest.Key != "tools/minimal" {
		t.Fatalf("manifest %+v", report.Manifest)
	}
}

func TestDoctorRejectsCategoryOfSeveralPluginsBeforeCopying(t *testing.T) {
	ctx, _ := doctorTestHome(t)
	category := filepath.Join(t.TempDir(), "web")
	for _, name := range []string{"a", "b"} {
		doctorPluginDir(t, filepath.Join(category, name), "name: "+name+"\n", "")
	}
	copied := trackDoctorCopies(t, nil)
	report := runDoctor(t, ctx, category)
	if len(*copied) != 0 || report.OK() || !strings.Contains(report.FormatText(), "holds 2 plugins (a, b)") {
		t.Fatalf("%v %s", *copied, report.FormatText())
	}
}

func TestDoctorNamesPluginDirsOfATargetThatIsNotACategory(t *testing.T) {
	ctx, _ := doctorTestHome(t)
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	doctorPluginDir(t, filepath.Join(repo, "myplugin"), "name: x\n", "")
	if err := os.MkdirAll(filepath.Join(repo, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, ".claude-plugin", "plugin.json"), "name: x\n")
	t.Chdir(repo)
	report := runDoctor(t, ctx, ".")
	if report.OK() || !strings.Contains(report.FormatText(), "plugin directory inside it: "+pypath.Join(".", "myplugin")+".") {
		t.Fatal(report.FormatText())
	}
}

func TestResolvePrefersInstalledIDOverUnrelatedLocalDir(t *testing.T) {
	doctorTestHome(t)
	icebathHome := filepath.Join(t.TempDir(), "icebath-home")
	installed := doctorPluginDir(t, filepath.Join(icebathHome, "plugins", "sample"), "name: sample\n", "")
	workdir := filepath.Join(t.TempDir(), "workdir")
	if err := os.MkdirAll(filepath.Join(workdir, "sample"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workdir)
	if got, err := ResolvePluginPath(homeCtx(icebathHome), "sample"); err != nil || got != resolvedPath(installed) {
		t.Fatalf("%q %v", got, err)
	}
}

func TestDoctorRemovesTempHomeWhenStagingCopyFails(t *testing.T) {
	ctx, _ := doctorTestHome(t)
	plugin := doctorPluginDir(t, filepath.Join(t.TempDir(), "sample"), "name: sample\n", doctorNoopInit)
	scratch := filepath.Join(t.TempDir(), "scratch-tmp")
	if err := os.Mkdir(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", scratch)
	trackDoctorCopies(t, &os.PathError{Op: "copy", Path: plugin, Err: syscall.ENOSPC})
	err := doctorRuntime(ctx, plugin, func(*doctorHost) error {
		t.Fatal("staging fails before the body runs")
		return nil
	})
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("err %v", err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(scratch, "icebath-plugin-doctor-*")); len(leftovers) != 0 {
		t.Fatalf("stranded doctor temp dirs: %v", leftovers)
	}
}

func TestDoctorNeverCopiesAPluginIntoItsOwnTempDir(t *testing.T) {
	for _, layout := range []string{"direct", "category"} {
		for _, route := range []string{"tmpdir-inside", "link-to-tmpdir", "harmless-link"} {
			t.Run(layout+"/"+route, func(t *testing.T) {
				ctx, _ := doctorTestHome(t)
				tmp := t.TempDir()
				target := filepath.Join(tmp, "target")
				plugin := target
				if layout == "category" {
					plugin = filepath.Join(target, "inner")
				}
				doctorPluginDir(t, plugin, "name: inner\n", doctorNoopInit)
				scratch := filepath.Join(tmp, "scratch")
				if route == "tmpdir-inside" {
					scratch = filepath.Join(plugin, "scratch")
				}
				if err := os.Mkdir(scratch, 0o755); err != nil {
					t.Fatal(err)
				}
				alias := filepath.Join(tmp, "tmp-alias")
				if err := os.Symlink(scratch, alias); err != nil {
					t.Fatal(err)
				}
				t.Setenv("TMPDIR", alias)
				switch route {
				case "link-to-tmpdir":
					if err := os.Symlink(scratch, filepath.Join(plugin, "assets")); err != nil {
						t.Fatal(err)
					}
				case "harmless-link":
					if err := os.Mkdir(filepath.Join(tmp, "shared"), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Join(tmp, "shared"), filepath.Join(plugin, "assets")); err != nil {
						t.Fatal(err)
					}
				}
				trackDoctorCopies(t, nil)
				report := runDoctor(t, ctx, target)
				refused := strings.Contains(findingText(report), "would recurse into itself")
				if refused != (route != "harmless-link") || (route == "harmless-link" && !report.OK()) {
					t.Fatal(report.FormatText())
				}
				if left, _ := os.ReadDir(scratch); len(left) != 0 {
					t.Fatalf("scratch not empty: %v", left)
				}
			})
		}
	}
}

func hostedProfileKeys() map[string]bool {
	hostedProfilesMu.Lock()
	defer hostedProfilesMu.Unlock()
	out := map[string]bool{}
	for k := range hostedProfiles {
		out[k] = true
	}
	return out
}

const acmeProviderInit = "from providers import register_provider\nfrom providers.base import ProviderProfile\n" +
	"register_provider(ProviderProfile(name='%s', auth_type='external_process',\n                                  process_command='acme'%s))\n"

func TestDoctorLoadsModelProviderPluginsThroughProviderDiscovery(t *testing.T) {
	ctx, _ := doctorTestHome(t)
	plugin := doctorPluginDir(t, filepath.Join(t.TempDir(), "acme-provider"), "name: acme-provider\nkind: model-provider\n",
		strings.Replace(strings.Replace(acmeProviderInit, "%s", "acme-doctor-probe", 1), "%s", ", aliases=('acme-alias',)", 1))
	before := hostedProfileKeys()
	report := runDoctor(t, ctx, plugin)
	if !report.OK() || !reflect.DeepEqual(report.RegisteredProviders, []string{"acme-doctor-probe"}) ||
		!strings.Contains(report.FormatText(), "provider(s): acme-doctor-probe") {
		t.Fatal(report.FormatText())
	}
	if !reflect.DeepEqual(hostedProfileKeys(), before) {
		t.Fatal("hosted profile registry changed")
	}
	writeFile(t, filepath.Join(plugin, "__init__.py"), "x = 1\n")
	report = runDoctor(t, ctx, plugin)
	if report.OK() || !strings.Contains(findingText(report), "registered no ProviderProfile") {
		t.Fatal(report.FormatText())
	}
}

func TestDoctorLoadsAModelProviderPluginFromItsCategory(t *testing.T) {
	ctx, _ := doctorTestHome(t)
	plugin := doctorPluginDir(t, filepath.Join(t.TempDir(), "model-providers", "acme-provider"), "name: acme-provider\nkind: model-provider\n",
		strings.Replace(strings.Replace(acmeProviderInit, "%s", "acme-category-probe", 1), "%s", "", 1))
	before := hostedProfileKeys()
	report := runDoctor(t, ctx, filepath.Dir(plugin))
	if !report.OK() || !reflect.DeepEqual(report.RegisteredProviders, []string{"acme-category-probe"}) {
		t.Fatal(report.FormatText())
	}
	if !reflect.DeepEqual(hostedProfileKeys(), before) {
		t.Fatal("hosted profile registry changed")
	}
}

func TestDoctorRunsOnUnloadDuringTeardown(t *testing.T) {
	ctx, _ := doctorTestHome(t)
	marker := filepath.Join(t.TempDir(), "unloaded.marker")
	t.Setenv("DOCTOR_UNLOAD_MARKER", marker)
	plugin := doctorPluginDir(t, filepath.Join(t.TempDir(), "lifecycle-plugin"), "name: lifecycle-plugin\n",
		"import os\nfrom pathlib import Path\n\ndef register(ctx):\n    marker = os.environ['DOCTOR_UNLOAD_MARKER']\n    ctx.on_unload(lambda: Path(marker).write_text('unloaded'))\n")
	report := runDoctor(t, ctx, plugin)
	if !report.OK() {
		t.Fatal(report.FormatText())
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "unloaded" {
		t.Fatalf("on_unload must run before the temp home is removed: %q %v", got, err)
	}
}

func desktopUniPlugin(t *testing.T, root string) string {
	t.Helper()
	plugin := doctorPluginDir(t, filepath.Join(root, "plugins", "uni"), "name: uni\nversion: 0.1.0\n", doctorNoopInit)
	if err := os.MkdirAll(filepath.Join(plugin, "desktop"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(plugin, "desktop", "plugin.js"), "export default { id: 'uni' } // v1\n")
	return plugin
}

func staleWarnings(r *PluginDoctorReport) []string {
	var out []string
	for _, f := range r.Findings {
		if strings.Contains(f.Message, "stale copy") {
			out = append(out, f.Message)
		}
	}
	return out
}

func TestDoctorWarnsWhenTheDesktopCopyIsStale(t *testing.T) {
	ctx, h := doctorTestHome(t)
	plugin := desktopUniPlugin(t, h)
	copyDir := filepath.Join(h, "desktop-plugins", "uni")
	if err := os.MkdirAll(copyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	js, _ := os.ReadFile(filepath.Join(plugin, "desktop", "plugin.js"))
	writeFile(t, filepath.Join(copyDir, "plugin.js"), string(js))
	marker, _ := json.Marshal(map[string]any{"package": "uni", "source": filepath.Join(plugin, "desktop"), "sourceMtimeMs": 1})
	writeFile(t, filepath.Join(copyDir, ".icebath-package.json"), string(marker))
	if got := staleWarnings(runDoctor(t, ctx, plugin)); len(got) != 0 {
		t.Fatal(got)
	}
	writeFile(t, filepath.Join(plugin, "desktop", "plugin.js"), "export default { id: 'uni' } // v2\n")
	report := runDoctor(t, ctx, plugin)
	warnings := staleWarnings(report)
	if len(warnings) != 1 || !strings.Contains(warnings[0], copyDir) || !strings.Contains(warnings[0], "Rescan") || !report.OK() {
		t.Fatal(report.FormatText())
	}
}

func TestDoctorIsSilentWithoutADesktopCopy(t *testing.T) {
	ctx, h := doctorTestHome(t)
	if got := staleWarnings(runDoctor(t, ctx, desktopUniPlugin(t, h))); len(got) != 0 {
		t.Fatal(got)
	}
}
