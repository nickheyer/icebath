package pm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func baStageHostPython(t *testing.T, python string) string {
	t.Helper()
	host := sttHostPython(t)
	out, err := exec.Command(host, "-I", "-c", "import sys, pathlib; print(pathlib.Path(sys._base_executable).resolve()); print(sys.base_prefix)").Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	os.MkdirAll(filepath.Dir(python), 0o777)
	if err := pyfs.Copy2(lines[0], python); err != nil {
		t.Fatal(err)
	}
	candidates, _ := filepath.Glob(filepath.Join(lines[1], "lib", "python3.*"))
	for _, stdlib := range candidates {
		if !isRegularFile(filepath.Join(stdlib, "os.py")) {
			continue
		}
		skip := map[string]bool{"site-packages": true, "test": true, "__pycache__": true, "idlelib": true, "tkinter": true}
		if err := pyfs.CopyTree(stdlib, filepath.Join(filepath.Dir(filepath.Dir(python)), "lib", filepath.Base(stdlib)), true, false, func(dir string, names []string) map[string]bool {
			ignored := map[string]bool{}
			for _, name := range names {
				ignored[name] = skip[name]
			}
			return ignored
		}); err != nil {
			t.Fatal(err)
		}
		break
	}
	return python
}

func baInputsFixture(t *testing.T, tmp string) (string, map[string]any) {
	t.Helper()
	source := filepath.Join(tmp, "prepared source")
	writeFile(t, filepath.Join(source, "pyproject.toml"), []byte("[project]\nname=\"assembly-fixture\"\nversion=\"1.2.3\"\n[project.scripts]\nprobe=\"entry:main\"\n"), 0o666)
	writeFile(t, filepath.Join(source, "entry.py"), []byte("import json, os, sys\nfrom pathlib import Path\nimport dependency\nfrom importlib.metadata import version\ndef main():\n print(json.dumps([sys.argv[1:], dependency.VALUE, version('assembly-fixture'), (Path(__file__).parent/'skills/data').read_text()]))\n return 7\n"), 0o666)
	writeFile(t, filepath.Join(source, "skills/data"), []byte("resource"), 0o666)
	out := filepath.Join(tmp, "payload")
	python := baStageHostPython(t, filepath.Join(out, "tools/python/bin/python3"))
	site := filepath.Join(out, "venv/lib/python3.14/site-packages")
	writeFile(t, filepath.Join(site, "dependency.py"), []byte("VALUE = 'prepared'\n"), 0o666)
	writeFile(t, filepath.Join(site, "entry.py"), []byte("raise RuntimeError('dependency shadowed source')\n"), 0o666)
	pmRuntime := filepath.Join(out, "pm-runtime")
	os.MkdirAll(filepath.Join(pmRuntime, "lib/site-packages"), 0o777)
	writeFile(t, filepath.Join(pmRuntime, "pm-runtime.json"), []byte(`{"python": "../tools/python/bin/python3", "sitePackages": "lib/site-packages"}`), 0o666)
	tui, web := filepath.Join(tmp, "tui"), filepath.Join(tmp, "web")
	writeFile(t, filepath.Join(tui, "dist/entry.js"), []byte("export const built = true;"), 0o666)
	writeFile(t, filepath.Join(tui, "package.json"), []byte(`{"type":"module"}`), 0o666)
	writeFile(t, filepath.Join(web, "index.html"), []byte("built web"), 0o666)
	return out, map[string]any{
		"project": filepath.Join(source, "pyproject.toml"), "code": source, "repo": "app", "target": "linux-x64",
		"placement": "contained", "python": python, "site_packages": site, "environment": filepath.Join(out, "venv"),
		"tools": filepath.Join(out, "tools"), "pm_runtime": pmRuntime, "resources": map[string]any{"skills": filepath.Join(source, "skills")},
		"frontends": map[string]any{"tui": tui, "web": web}, "ref": "fixture-revision",
	}
}

func baWithWebapp(t *testing.T, data map[string]any, tmp string) {
	t.Helper()
	webapp := filepath.Join(tmp, "webapp")
	writeFile(t, filepath.Join(webapp, "index.html"), []byte("built webapp"), 0o666)
	data["frontends"].(map[string]any)["webapp"] = webapp
}

func baBuild(t *testing.T, data map[string]any, out, tmp string) error {
	t.Helper()
	document := filepath.Join(tmp, "inputs.json")
	encoded, _ := json.Marshal(data)
	writeFile(t, document, encoded, 0o666)
	return AgentMain(context.Background(), document, out)
}

func baTreeBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			files[path] = string(readFile(t, path))
		}
		return nil
	})
	return files
}

func baRun(t *testing.T, argv []string, dir string, env []string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = dir, env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.String(), stderr.String(), exitErr.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), 0
}

func TestAgentContainedAssemblyRunsAfterMoveAndPreservesPreparedState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix launchers")
	}
	tmp := t.TempDir()
	out, data := baInputsFixture(t, tmp)
	baWithWebapp(t, data, tmp)
	writeFile(t, filepath.Join(out, "provider-state"), []byte("retain"), 0o666)
	code := data["code"].(string)
	before := baTreeBytes(t, code)
	if err := baBuild(t, data, out, tmp); err != nil {
		t.Fatal(err)
	}
	if string(readFile(t, filepath.Join(out, "provider-state"))) != "retain" {
		t.Fatal("provider state lost")
	}
	manifest := bpReadJSON(t, filepath.Join(out, "manifest.json"))
	if manifest["ref"] != "fixture-revision" {
		t.Fatalf("ref %v", manifest["ref"])
	}
	want := map[string]any{"repoDir": "app", "toolsDir": "tools", "storePython": "tools/python/bin/python3",
		"sitePackages": "venv/lib/python3.14/site-packages", "commands": map[string]any{"probe": "bin/probe"}}
	if !reflect.DeepEqual(manifest["runtime"], want) {
		t.Fatalf("runtime %v", manifest["runtime"])
	}
	if !isRegularFile(filepath.Join(out, "app/cmd/icebath/tui_dist/entry.js")) || !isRegularFile(filepath.Join(out, "app/cmd/icebath/web_dist/index.html")) {
		t.Fatal("frontends not planted")
	}
	if bpReadJSON(t, filepath.Join(out, "app/cmd/icebath/tui_dist/package.json"))["type"] != "module" {
		t.Fatal("tui package.json not planted")
	}
	if string(readFile(t, filepath.Join(out, "app/apps/desktop/dist-webapp/index.html"))) != "built webapp" {
		t.Fatal("webapp not planted")
	}
	moved := filepath.Join(tmp, "relocated payload")
	if err := os.Rename(out, moved); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tmp, "entry.py"), []byte("raise RuntimeError('cwd shadowed source')"), 0o666)
	stdout, stderr, status := baRun(t, []string{filepath.Join(moved, "bin/probe"), "two words", "$(no)", ""}, tmp,
		[]string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(tmp, "home"), "PYTHONPATH=/foreign", "PYTHONHOME=/foreign"})
	if status != 7 {
		t.Fatalf("status %d: %s", status, stderr)
	}
	var got []any
	json.Unmarshal([]byte(stdout), &got)
	if !reflect.DeepEqual(got, []any{[]any{"two words", "$(no)", ""}, "prepared", "1.2.3", "resource"}) {
		t.Fatalf("probe %s", stdout)
	}
	if !reflect.DeepEqual(baTreeBytes(t, code), before) {
		t.Fatal("prepared source changed")
	}
	filepath.Walk(moved, func(path string, info os.FileInfo, err error) error {
		if filepath.Base(path) == "__pycache__" {
			t.Fatalf("bytecode cache written into the payload at %s", path)
		}
		return nil
	})
	if !isRegularFile(filepath.Join(moved, "pm-runtime/pm-runtime.json")) || pathExists(filepath.Join(tmp, "home/.icebath")) {
		t.Fatal("payload state wrong")
	}
}

func TestAgentReferencedInstalledCodeUsesDerivedCommandMapWithoutCopying(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix links")
	}
	tmp := t.TempDir()
	_, data := baInputsFixture(t, tmp)
	baWithWebapp(t, data, tmp)
	out := filepath.Join(tmp, "nix output")
	source := data["code"].(string)
	writeFile(t, filepath.Join(source, "entry.py"), []byte("import json, os\nfrom pathlib import Path\ndef main():\n print(json.dumps([Path(os.environ['ICEBATH_BUNDLED_SKILLS'],'data').read_text(),os.environ['ICEBATH_NODE']]))\n"), 0o666)
	bindir := filepath.Join(tmp, "installed env/bin")
	command := filepath.Join(bindir, "probe")
	writeFile(t, command, []byte("#!"+sttHostPython(t)+"\nimport sys\nsys.path.insert(0, "+pyrepr.StrRepr(source)+")\nfrom entry import main\nmain()\n"), 0o755)
	data["placement"], data["repo"], data["command_dir"] = "references", "share/icebath-agent", bindir
	data["env"] = map[string]any{"ICEBATH_NODE": "/prepared/node"}
	before := baTreeBytes(t, source)
	if err := baBuild(t, data, out, tmp); err != nil {
		t.Fatal(err)
	}
	mapping := bpReadJSON(t, filepath.Join(out, "command-map.json"))
	if want := map[string]any{"probe": map[string]any{"source": command, "destination": "bin/probe", "entry": "entry:main"}}; !reflect.DeepEqual(mapping["commands"], want) {
		t.Fatalf("commands %v", mapping["commands"])
	}
	env := mapping["env"].(map[string]any)
	frontends := data["frontends"].(map[string]any)
	for key, name := range map[string]string{"ICEBATH_TUI_DIR": "tui", "ICEBATH_WEB_DIST": "web", "ICEBATH_WEBAPP_DIST": "webapp"} {
		if resolvePath(env[key].(string)) != frontends[name] {
			t.Fatalf("%s -> %v", key, env[key])
		}
	}
	if env["ICEBATH_INSTALL_ROOT"] != filepath.Join(out, "share/icebath-agent") {
		t.Fatalf("install root %v", env["ICEBATH_INSTALL_ROOT"])
	}
	for _, link := range []string{"share/icebath-agent/skills", "ui-tui", "share/icebath-agent/web_dist", "share/icebath-agent/webapp_dist"} {
		if info, err := os.Lstat(filepath.Join(out, link)); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s not linked", link)
		}
	}
	if pathExists(filepath.Join(out, "bin/probe")) || pathExists(filepath.Join(out, "share/icebath-agent/entry.py")) {
		t.Fatal("referenced code was copied or wrapped")
	}
	runEnv := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(tmp, "home"), "PYTHONDONTWRITEBYTECODE=1"}
	for _, key := range []string{"ICEBATH_NODE", "ICEBATH_PYTHON", "ICEBATH_INSTALL_ROOT", "ICEBATH_BUNDLED_SKILLS", "ICEBATH_TUI_DIR", "ICEBATH_WEB_DIST", "ICEBATH_WEBAPP_DIST"} {
		runEnv = append(runEnv, key+"="+env[key].(string))
	}
	stdout, stderr, status := baRun(t, []string{command}, tmp, runEnv)
	var got []any
	json.Unmarshal([]byte(stdout), &got)
	if status != 0 || !reflect.DeepEqual(got, []any{"resource", "/prepared/node"}) {
		t.Fatalf("probe %d %s %s", status, stdout, stderr)
	}
	if !reflect.DeepEqual(baTreeBytes(t, source), before) {
		t.Fatal("source changed")
	}
}

func TestAgentFixedRootKeepsPrivilegeShimAndResolvesVenvCommandSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix launchers")
	}
	tmp := t.TempDir()
	out, data := baInputsFixture(t, tmp)
	if err := pyfs.CopyTree(data["code"].(string), out, false, true, nil); err != nil {
		t.Fatal(err)
	}
	data["placement"], data["repo"], data["code"], data["project"], data["bin_dir"], data["python"] = "fixed", ".", out, filepath.Join(out, "pyproject.toml"), "libexec", sttHostPython(t)
	writeFile(t, filepath.Join(out, "bin/probe"), []byte("privilege shim"), 0o666)
	stale := filepath.Join(out, "cmd/icebath/web_dist/stale")
	writeFile(t, stale, []byte("old surface"), 0o666)
	if err := baBuild(t, data, out, tmp); err != nil {
		t.Fatal(err)
	}
	if string(readFile(t, filepath.Join(out, "bin/probe"))) != "privilege shim" || pathExists(stale) {
		t.Fatal("shim replaced or stale surface kept")
	}
	link := filepath.Join(out, "venv/bin/probe")
	os.MkdirAll(filepath.Dir(link), 0o777)
	os.Symlink("../../libexec/probe", link)
	stdout, stderr, status := baRun(t, []string{link, "ok"}, tmp, []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(tmp, "home")})
	var got []any
	json.Unmarshal([]byte(stdout), &got)
	if status != 7 || len(got) == 0 || !reflect.DeepEqual(got[0], []any{"ok"}) {
		t.Fatalf("probe %d %s %s", status, stdout, stderr)
	}
}

func TestAgentSourceMetadataKeepsDeclaredRequirementsExtrasAndEntrypoints(t *testing.T) {
	tmp := t.TempDir()
	out, data := baInputsFixture(t, tmp)
	writeFile(t, data["project"].(string), []byte("[project]\nname=\"assembly-fixture\"\nversion=\"1.2.3\"\nrequires-python=\">=3.11\"\n"+
		"dependencies=[\"dependency>=1,<2; python_version >= '3.11'\"]\n[project.optional-dependencies]\nextra=[\"optional>=2; sys_platform == 'linux'\"]\n"+
		"[project.scripts]\nprobe=\"entry:main\"\n"), 0o666)
	if err := baBuild(t, data, out, tmp); err != nil {
		t.Fatal(err)
	}
	info := filepath.Join(out, "app/assembly_fixture-1.2.3.dist-info")
	metadata := "Metadata-Version: 2.3\nName: assembly-fixture\nVersion: 1.2.3\nRequires-Python: >=3.11\n" +
		"Requires-Dist: dependency>=1,<2; python_version >= '3.11'\nProvides-Extra: extra\n" +
		"Requires-Dist: optional>=2; (sys_platform == 'linux') and extra == 'extra'\n"
	if got := string(readFile(t, filepath.Join(info, "METADATA"))); got != metadata {
		t.Fatalf("METADATA %q", got)
	}
	if got := string(readFile(t, filepath.Join(info, "entry_points.txt"))); got != "[console_scripts]\nprobe = entry:main\n" {
		t.Fatalf("entry points %q", got)
	}
}

func TestAgentFailedInputsCannotLeaveACompletionClaim(t *testing.T) {
	for _, fault := range []string{"missing-pm", "missing-resource", "missing-tui", "missing-web", "unknown-field", "bad-repo", "bad-target"} {
		t.Run(fault, func(t *testing.T) {
			tmp := t.TempDir()
			out, data := baInputsFixture(t, tmp)
			writeFile(t, filepath.Join(out, "manifest.json"), []byte(`{"runtime": {"commands": {}}}`), 0o666)
			switch fault {
			case "missing-pm":
				os.Remove(filepath.Join(data["pm_runtime"].(string), "pm-runtime.json"))
			case "missing-resource":
				data["resources"].(map[string]any)["skills"] = filepath.Join(tmp, "missing skills")
			case "missing-tui":
				os.Remove(filepath.Join(data["frontends"].(map[string]any)["tui"].(string), "dist/entry.js"))
			case "missing-web":
				os.Remove(filepath.Join(data["frontends"].(map[string]any)["web"].(string), "index.html"))
			case "unknown-field":
				data["typo"] = true
			case "bad-repo":
				data["repo"] = "../escape"
			default:
				data["target"] = "linux-typo"
			}
			if err := baBuild(t, data, out, tmp); err == nil {
				t.Fatal("faulty inputs assembled")
			}
			if pathExists(filepath.Join(out, "manifest.json")) || pathExists(filepath.Join(out, "bin/probe")) {
				t.Fatal("completion claim left behind")
			}
		})
	}
}

func TestAgentIncrementalCopyDropsRemovedSourceWithoutDeletingProviderFiles(t *testing.T) {
	tmp := t.TempDir()
	out, data := baInputsFixture(t, tmp)
	source := data["code"].(string)
	writeFile(t, filepath.Join(source, "obsolete.py"), []byte("old"), 0o666)
	if err := baBuild(t, data, out, tmp); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(out, "provider-file")
	writeFile(t, sentinel, []byte("retain"), 0o666)
	os.Remove(filepath.Join(source, "obsolete.py"))
	writeFile(t, filepath.Join(out, "app/cmd/icebath/web_dist/stale"), []byte("old"), 0o666)
	if err := baBuild(t, data, out, tmp); err != nil {
		t.Fatal(err)
	}
	if pathExists(filepath.Join(out, "app/obsolete.py")) || pathExists(filepath.Join(out, "app/cmd/icebath/web_dist/stale")) {
		t.Fatal("removed source or stale surface kept")
	}
	tui := data["frontends"].(map[string]any)["tui"].(string)
	if !bytes.Equal(readFile(t, filepath.Join(out, "app/cmd/icebath/tui_dist/entry.js")), readFile(t, filepath.Join(tui, "dist/entry.js"))) {
		t.Fatal("tui product differs")
	}
	if string(readFile(t, sentinel)) != "retain" {
		t.Fatal("provider file removed")
	}
}

func TestAgentSiteLayoutUsesExplicitTargetVersionNotBuilderPython(t *testing.T) {
	tmp := t.TempDir()
	if site, _ := dependencySite(tmp, "3.99.1+vendor", "linux-x64"); site != filepath.Join(tmp, "lib/python3.99/site-packages") {
		t.Fatalf("linux site %s", site)
	}
	if site, _ := dependencySite(tmp, "3.99.1", "win32-arm64"); site != filepath.Join(tmp, "Lib", "site-packages") {
		t.Fatalf("windows site %s", site)
	}
}

func TestAgentNativeFinishConsumesRealMinimalPreparationWithoutBootstrap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix launchers")
	}
	tmp := t.TempDir()
	out := filepath.Join(tmp, "payload")
	code := filepath.Join(out, "icebath-agent")
	writeFile(t, filepath.Join(code, "pyproject.toml"), []byte("[project]\nname=\"native-fixture\"\nversion=\"1.0\"\n[project.scripts]\nprobe=\"entry:main\"\n"), 0o666)
	writeFile(t, filepath.Join(code, "entry.py"), []byte("def main():\n print(\"native fixture\")\n return 0\n"), 0o666)
	resources := map[string]string{}
	for _, name := range resourceEnvNames() {
		os.MkdirAll(filepath.Join(code, name), 0o777)
		resources[name] = filepath.Join(code, name)
	}
	python := baStageHostPython(t, filepath.Join(out, "tools/python/bin/python3"))
	site := filepath.Join(out, "venv/lib/site-packages")
	runtimeDir := filepath.Join(out, "pm-runtime")
	os.MkdirAll(site, 0o777)
	os.MkdirAll(filepath.Join(runtimeDir, "lib/site-packages"), 0o777)
	writeFile(t, filepath.Join(runtimeDir, "pm-runtime.json"), []byte(`{"python": "../tools/python/bin/python3", "sitePackages": "lib/site-packages"}`), 0o666)
	features := filepath.Join(out, "enabled-features.json")
	writeFile(t, features, []byte(`{"extras":[]}`), 0o666)
	os.MkdirAll(filepath.Join(out, "uv-cache"), 0o777)
	target, err := CurrentTarget()
	if err != nil {
		t.Fatal(err)
	}
	ref := "fixture"
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "private"))
	ctx := withSyncOutput(context.Background(), &bytes.Buffer{})
	prepared, err := publishPrepared(ctx, out, RepoRoot(), strings.Repeat("a", 40), AgentInputs{
		Project: filepath.Join(code, "pyproject.toml"), Code: code, Repo: "icebath-agent", Placement: "contained",
		Target: target, Python: python, SitePackages: site, Environment: filepath.Join(out, "venv"),
		Tools: filepath.Join(out, "tools"), PMRuntime: runtimeDir, Features: features, Ref: &ref, Resources: resources,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := FinishNative(ctx, prepared, nil); err != nil || code != 0 {
		t.Fatalf("finish %d %v", code, err)
	}
	commands := bpReadJSON(t, filepath.Join(out, "manifest.json"))["runtime"].(map[string]any)["commands"]
	if !reflect.DeepEqual(commands, map[string]any{"probe": "bin/probe"}) {
		t.Fatalf("commands %v", commands)
	}
	if pathExists(filepath.Join(code, "cmd/icebath/tui_dist")) || pathExists(filepath.Join(code, "cmd/icebath/web_dist")) {
		t.Fatal("frontends planted without products")
	}
	stdout, stderr, status := baRun(t, []string{filepath.Join(out, "bin/probe")}, tmp, os.Environ())
	if status != 0 || strings.TrimSpace(stdout) != "native fixture" {
		t.Fatalf("probe %d %s %s", status, stdout, stderr)
	}
	if pathExists(filepath.Join(tmp, "private")) {
		t.Fatal("finish touched the private home")
	}
	writeFile(t, filepath.Join(site, "changed.py"), []byte("changed = True"), 0o666)
	if _, err := FinishNative(ctx, prepared, nil); err == nil || !strings.Contains(err.Error(), "prepar") {
		t.Fatalf("changed preparation accepted: %v", err)
	}
	if pathExists(filepath.Join(out, "manifest.json")) {
		t.Fatal("manifest left after a refused finish")
	}
}
