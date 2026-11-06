package pm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
)

const brRealHelperEnv = "ICEBATH_TEST_BN_REAL_STAGE"

func TestAgentFixedLayoutManifestMakesTheTreeAResolvablePayload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix launchers")
	}
	tmp := t.TempDir()
	out, data := baInputsFixture(t, tmp)
	if err := pyfs.CopyTree(data["code"].(string), out, false, true, nil); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(out, "pyproject.toml")
	writeFile(t, project, append(readFile(t, project), []byte("icebath=\"entry:main\"\n")...), 0o666)
	outside := filepath.Join(tmp, "usr-local-bin-python3")
	if err := os.Symlink(data["python"].(string), outside); err != nil {
		t.Fatal(err)
	}
	venvPython := filepath.Join(out, "venv/bin/python")
	os.MkdirAll(filepath.Dir(venvPython), 0o777)
	if err := os.Symlink(outside, venvPython); err != nil {
		t.Fatal(err)
	}
	data["placement"], data["repo"], data["code"], data["project"], data["bin_dir"], data["python"] = "fixed", ".", out, project, "libexec", venvPython
	if err := baBuild(t, data, out, tmp); err != nil {
		t.Fatal(err)
	}
	if store := bpReadJSON(t, filepath.Join(out, "manifest.json"))["runtime"].(map[string]any)["storePython"]; store != "venv/bin/python" {
		t.Fatalf("storePython %v", store)
	}
	ctx := context.Background()
	venv, err := PayloadVenv(out)
	if err != nil || venv != filepath.Join(out, "venv") {
		t.Fatalf("payload venv %q %v", venv, err)
	}
	if committed, err := CommittedVenv(ctx, out); err != nil || committed != filepath.Join(out, "venv") {
		t.Fatalf("committed venv %q %v", committed, err)
	}
	if store, err := EnvironmentStoreRoot(ctx, out, false); err != nil || store != filepath.Join(out, "tools") {
		t.Fatalf("store root %q %v", store, err)
	}
	if commands, err := PayloadCommandDir(out); err != nil || commands != filepath.Join(out, "libexec") {
		t.Fatalf("command dir %q %v", commands, err)
	}
	command, err := VenvCommand(ctx, out, filepath.Join(out, "venv"), nil)
	if err != nil || command[0] != resolvePath(filepath.Join(out, "tools/python/bin/python3")) {
		t.Fatalf("venv command %v %v", command, err)
	}
	stdout, stderr, _ := baRun(t, append(command, "-c", "import dependency; print(dependency.VALUE)"), tmp,
		[]string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(tmp, "home")})
	if strings.TrimSpace(stdout) != "prepared" {
		t.Fatalf("dependency %q %s", stdout, stderr)
	}
	manifest := bpReadJSON(t, filepath.Join(out, "manifest.json"))
	manifest["repo"] = "elsewhere"
	encoded, _ := json.Marshal(manifest)
	writeFile(t, filepath.Join(out, "manifest.json"), encoded, 0o666)
	if venv, err := PayloadVenv(out); err != nil || venv != "" {
		t.Fatalf("foreign manifest claimed the tree %q %v", venv, err)
	}
}

func TestAgentPreparedEnvironmentPythonImportsAppFromUnrelatedScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix launchers")
	}
	tmp := t.TempDir()
	out, data := baInputsFixture(t, tmp)
	environment := filepath.Join(out, ".venv")
	if output, err := exec.Command(sttHostPython(t), "-m", "venv", "--without-pip", environment).CombinedOutput(); err != nil {
		t.Fatalf("venv %v %s", err, output)
	}
	python := filepath.Join(environment, "bin/python")
	purelib, err := exec.Command(python, "-I", "-c", "import sysconfig; print(sysconfig.get_paths()['purelib'])").Output()
	if err != nil {
		t.Fatal(err)
	}
	site := strings.TrimSpace(string(purelib))
	writeFile(t, filepath.Join(site, "dependency.py"), []byte("VALUE='prepared'"), 0o666)
	data["placement"], data["environment"], data["python"], data["site_packages"] = "fixed", environment, python, site
	if err := baBuild(t, data, out, tmp); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(tmp, "external-probe.py")
	writeFile(t, script, []byte("import entry\nfrom importlib.metadata import version\nprint(version('assembly-fixture'))\n"), 0o666)
	stdout, stderr, status := baRun(t, []string{python, "-I", script}, tmp,
		[]string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(tmp, "home"), "PYTHONDONTWRITEBYTECODE=1"})
	if status != 0 || strings.TrimSpace(stdout) != "1.2.3" {
		t.Fatalf("probe %d %q %s", status, stdout, stderr)
	}
}

func TestTermuxLaunchersForwardArgumentsAndExportPayloadEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix launchers")
	}
	tmp := t.TempDir()
	host := sttHostPython(t)
	payload := filepath.Join(tmp, "payload with spaces")
	os.MkdirAll(filepath.Join(payload, "venv/bin"), 0o777)
	if err := os.Symlink(host, filepath.Join(payload, "venv/bin/python")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(payload, "app/capture_entry.py"), []byte("import json, os, sys\ndef main():\n    print(json.dumps({'argv': sys.argv[1:], 'env': {k: os.environ.get(k) for k in ['ICEBATH_NODE', 'ICEBATH_PYTHON', 'ICEBATH_RUNTIME_DIR', 'PYTHONPATH', 'PYTHONHOME', 'PYTHONPYCACHEPREFIX']}}))\n"), 0o666)
	writeFile(t, filepath.Join(tmp, "json.py"), []byte("raise RuntimeError('cwd shadowed stdlib')\n"), 0o666)
	entries := NewOrderedObject()
	for _, name := range []string{"icebath", "icebath-agent", "icebath-acp"} {
		entries.Set(name, "capture_entry:main")
	}
	if _, err := writeLaunchers(context.Background(), payload, entries, "venv/bin/python", "app", "venv/lib/python3.14/site-packages", "linux-arm64-bionic", "bin"); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(tmp, "prefix/bin")
	os.MkdirAll(binDir, 0o777)
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range entries.Keys {
		link := filepath.Join(binDir, name)
		relative, _ := filepath.Rel(binDir, filepath.Join(payload, "bin", name))
		if err := os.Symlink(relative, link); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, status := baRun(t, []string{shell, link, "one two", "$(not-executed)", ""}, tmp,
			append(os.Environ(), "PYTHONHOME=/does-not-exist", "PYTHONPATH=/foreign"))
		if status != 0 {
			t.Fatalf("%s exit %d %s", name, status, stderr)
		}
		var got struct {
			Argv []string           `json:"argv"`
			Env  map[string]*string `json:"env"`
		}
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Argv, []string{"one two", "$(not-executed)", ""}) {
			t.Fatalf("argv %q", got.Argv)
		}
		env := got.Env
		if env["ICEBATH_PYTHON"] == nil || resolvePath(*env["ICEBATH_PYTHON"]) != resolvePath(host) {
			t.Fatalf("python %v", env["ICEBATH_PYTHON"])
		}
		if env["ICEBATH_NODE"] == nil || *env["ICEBATH_NODE"] != filepath.Join(payload, "tools/node/data/data/com.termux/files/usr/bin/node") {
			t.Fatalf("node %v", env["ICEBATH_NODE"])
		}
		if env["ICEBATH_RUNTIME_DIR"] == nil || *env["ICEBATH_RUNTIME_DIR"] != filepath.Join(payload, "tools") {
			t.Fatalf("runtime dir %v", env["ICEBATH_RUNTIME_DIR"])
		}
		if env["PYTHONPATH"] == nil || strings.Split(*env["PYTHONPATH"], string(os.PathListSeparator))[0] != filepath.Join(payload, "app") {
			t.Fatalf("pythonpath %v", env["PYTHONPATH"])
		}
		if env["PYTHONHOME"] != nil {
			t.Fatalf("pythonhome %q", *env["PYTHONHOME"])
		}
		if prefix := env["PYTHONPYCACHEPREFIX"]; prefix == nil || *prefix == payload || strings.HasPrefix(*prefix, payload+string(filepath.Separator)) {
			t.Fatalf("pycache prefix %v", prefix)
		}
	}
}

// Execs the rendered wrapper into a namespace under the host interpreter and runs the asserting body
func dwRun(t *testing.T, tmp, body string, extra ...string) {
	t.Helper()
	text, err := renderWrapper("stubmod.entry:main", "../repo", "../venv/Lib/site-packages")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "launcher_wrapper.py")
	writeFile(t, wrapper, []byte(text), 0o666)
	driver := "import os, sys\nns = {'__name__': 'launcher_wrapper_under_test'}\n" +
		"exec(compile(open(sys.argv[1], encoding='utf-8').read(), sys.argv[1], 'exec'), ns)\ntmp = sys.argv[2]\n" + body
	_, stderr, status := baRun(t, append([]string{sttHostPython(t), "-I", "-B", "-c", driver, wrapper, tmp}, extra...), tmp, os.Environ())
	if status != 0 {
		t.Fatalf("wrapper assertion failed %d\n%s", status, stderr)
	}
}

func TestDesktopWrapperLauncherDirResolvesFromArgv0(t *testing.T) {
	dwRun(t, t.TempDir(), "assert ns['launcher_dir'](os.path.join(tmp, 'bin', 'icebath.exe')) == os.path.join(tmp, 'bin')\n")
}

func TestDesktopWrapperPayloadSysPathsOrderRepoFirst(t *testing.T) {
	dwRun(t, t.TempDir(), "here = os.path.join(tmp, 'bin')\n"+
		"assert ns['payload_sys_paths'](here) == [os.path.join(here, '..', 'repo'), os.path.join(here, '..', 'venv', 'Lib', 'site-packages')]\n")
}

func TestDesktopWrapperPayloadSysPathsAcceptsForwardSlashRelsOnAnyPlatform(t *testing.T) {
	dwRun(t, t.TempDir(), "for entry in ns['payload_sys_paths'](os.path.join(tmp, 'bin')):\n    assert os.path.isabs(os.path.normpath(entry))\n")
}

func TestDesktopWrapperConfigurePrependsRepoThenSitePackages(t *testing.T) {
	dwRun(t, t.TempDir(), "here = os.path.join(tmp, 'bin')\nentries = ns['configure'](here, environ={})\nassert sys.path[:2] == entries\n"+
		"assert sys.path[0] == os.path.join(here, '..', 'repo')\nassert sys.path[1] == os.path.join(here, '..', 'venv', 'Lib', 'site-packages')\n")
}

func TestDesktopWrapperConfigureProcessesSitePthFiles(t *testing.T) {
	dwRun(t, t.TempDir(), "here = os.path.join(tmp, 'bin')\nsite = os.path.join(here, '..', 'venv', 'Lib', 'site-packages')\nos.makedirs(site)\n"+
		"added = os.path.join(tmp, 'pth-added')\nos.mkdir(added)\nopen(os.path.join(site, 'load-bearing.pth'), 'w', encoding='utf-8').write(added + '\\n')\n"+
		"ns['configure'](here, environ={})\nassert sys.path[:2] == [os.path.join(here, '..', 'repo'), site], sys.path\nassert sys.path.index(added) > 1\n")
}

func TestDesktopWrapperConfigureDropsInheritedPythonpathAndPythonhome(t *testing.T) {
	dwRun(t, t.TempDir(), "environ = {'PYTHONPATH': '/foreign/install', 'PYTHONHOME': '/foreign/python'}\nns['configure'](tmp, environ=environ)\n"+
		"assert 'PYTHONPATH' not in environ\nassert 'PYTHONHOME' not in environ\n")
}

func TestDesktopWrapperConfigureDefaultsPycachePrefixToLocalappdata(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("platforms windows")
	}
	dwRun(t, t.TempDir(), "environ = {'LOCALAPPDATA': os.path.join(tmp, 'lad')}\nns['configure'](tmp, environ=environ)\n"+
		"expected = os.path.join(tmp, 'lad', 'icebath', 'pycache')\nassert environ['PYTHONPYCACHEPREFIX'] == expected\nassert str(sys.pycache_prefix) == expected\n")
}

func TestDesktopWrapperConfigureDefaultsPycachePrefixToHomeCache(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
	dwRun(t, t.TempDir(), "environ = {'HOME': os.path.join(tmp, 'home')}\nns['configure'](tmp, environ=environ)\n"+
		"expected = os.path.join(tmp, 'home', '.cache', 'icebath-pycache')\nassert environ['PYTHONPYCACHEPREFIX'] == expected\nassert str(sys.pycache_prefix) == expected\n")
}

func TestDesktopWrapperConfigureKeepsAUserSetPycachePrefix(t *testing.T) {
	dwRun(t, t.TempDir(), "environ = {'LOCALAPPDATA': os.path.join(tmp, 'lad'), 'PYTHONPYCACHEPREFIX': os.path.join(tmp, 'mine')}\noriginal = sys.pycache_prefix\n"+
		"ns['configure'](tmp, environ=environ)\nassert environ['PYTHONPYCACHEPREFIX'] == os.path.join(tmp, 'mine')\nassert sys.pycache_prefix == original\n")
}

func TestDesktopWrapperConfigureWithoutLocalappdataLeavesPycacheAlone(t *testing.T) {
	dwRun(t, t.TempDir(), "environ = {}\noriginal = sys.pycache_prefix\nns['configure'](tmp, environ=environ)\n"+
		"assert 'PYTHONPYCACHEPREFIX' not in environ\nassert sys.pycache_prefix == original\n")
}

func TestDesktopWrapperRenderedWrapperDispatchesToTheEntryModule(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "repo/icebath_cli/__init__.py"), nil, 0o666)
	writeFile(t, filepath.Join(tmp, "repo/icebath_bootstrap.py"), []byte("READY = True\n"), 0o666)
	writeFile(t, filepath.Join(tmp, "repo/icebath_cli/main.py"), []byte("import sys\nassert sys.modules['icebath_bootstrap'].READY\ndef main():\n"+
		"    assert sys.argv[0].endswith('icebath.exe'), sys.argv[0]\n    assert sys.argv[1:] == ['--version']\n    import stubdep\n    return 7\n"), 0o666)
	writeFile(t, filepath.Join(tmp, "venv/Lib/site-packages/stubdep.py"), []byte("X = 1\n"), 0o666)
	text := launcherWrapperTemplate
	for placeholder, value := range map[string]string{"__ICEBATH_ENTRY_MODULE__": "icebath_cli.main", "__ICEBATH_ENTRY_FUNC__": "main",
		"__ICEBATH_REPO_REL__": "../repo", "__ICEBATH_SITE_REL__": "../venv/Lib/site-packages"} {
		text = strings.ReplaceAll(text, placeholder, value)
	}
	wrapper := filepath.Join(tmp, "bin/icebath.exe")
	writeFile(t, wrapper, []byte(text), 0o666)
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key != "PYTHONPATH" && key != "PYTHONHOME" && key != "PYTHONPYCACHEPREFIX" && key != "HOME" {
			env = append(env, entry)
		}
	}
	env = append(env, "LOCALAPPDATA="+filepath.Join(tmp, "lad"), "HOME="+filepath.Join(tmp, "home"))
	_, stderr, status := baRun(t, []string{sttHostPython(t), wrapper, "--version"}, tmp, env)
	if status != 7 {
		t.Fatalf("exit %d %s", status, stderr)
	}
}

func TestBNStageNativeRealHelper(t *testing.T) {
	if os.Getenv(brRealHelperEnv) == "" {
		return
	}
	args := StageNativeArgs{}
	argv := os.Args[slices.Index(os.Args, "--")+1:]
	for i := 0; i+1 < len(argv); i += 2 {
		switch argv[i] {
		case "--out":
			args.Out = argv[i+1]
		case "--ref":
			args.Ref = argv[i+1]
		case "--source":
			args.Source = argv[i+1]
		}
	}
	code, err := StageNativeMain(context.Background(), args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(code)
}

func TestBNNativeDispatchIsolatesProcessStateOnRealChildFailure(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "user-home"))
	t.Setenv(EnvRuntimeDir, filepath.Join(tmp, "user-tools"))
	t.Setenv(brRealHelperEnv, "1")
	previous := stageNativeArgv
	stageNativeArgv = func() ([]string, error) {
		return []string{os.Args[0], "-test.run=^TestBNStageNativeRealHelper$", "-test.count=1", "--"}, nil
	}
	t.Cleanup(func() { stageNativeArgv = previous })
	before := os.Environ()
	out := filepath.Join(tmp, "output")
	ctx := withSyncOutput(context.Background(), &bytes.Buffer{})
	code, err := stageNative(ctx, BundleArgs{Out: out, Ref: "missing-build-test-ref", Cache: filepath.Join(tmp, "cache")})
	if err != nil || code == 0 {
		t.Fatalf("status %d err %v", code, err)
	}
	if !slices.Equal(before, os.Environ()) {
		t.Fatal("parent environment changed")
	}
	if pathExists(filepath.Join(tmp, "user-home")) || pathExists(filepath.Join(tmp, "user-tools")) {
		t.Fatal("user state created")
	}
	if pathExists(filepath.Join(out, "manifest.json")) {
		t.Fatal("manifest published")
	}
	if work, _ := filepath.Glob(filepath.Join(out, ".build-*")); len(work) != 0 {
		t.Fatalf("work dirs left %v", work)
	}
}

func TestBNNativePreparationRefusesSymlinkedOutputBeforeWriting(t *testing.T) {
	tmp := t.TempDir()
	output := filepath.Join(tmp, "payload")
	outside := filepath.Join(tmp, "outside")
	os.MkdirAll(output, 0o777)
	writeFile(t, filepath.Join(outside, "keep"), []byte("untouched"), 0o666)
	if err := os.Symlink(outside, filepath.Join(output, "icebath-agent")); err != nil {
		t.Fatal(err)
	}
	_, err := PrepareNative(context.Background(), output, "HEAD", RepoRoot(), filepath.Join(tmp, "cache"), "", nil)
	var pyErr *PyError
	if !errors.As(err, &pyErr) || pyErr.Class != "ValueError" || !regexp.MustCompile("symlink|escaped").MatchString(pyErr.Message) {
		t.Fatalf("err %v", err)
	}
	if string(readFile(t, filepath.Join(outside, "keep"))) != "untouched" || pathExists(preparedPath(output)) {
		t.Fatal("preparation wrote through the symlink")
	}
}

func TestRuntimeStageOfflineWheelhouseRuntimeSurvivesSealingAndMove(t *testing.T) {
	tmp := peIsolatedBuilder(t)
	wheelhouse, versions := peRuntimeWheelhouse(t)
	root := filepath.Join(tmp, "payload")
	python := baStageHostPython(t, filepath.Join(root, "tools/python/bin/python"))
	peToolchain(t, peUV(t), python)
	ctx := context.Background()
	executable, err := StageManagerRuntime(ctx, StageManagerRuntimeOptions{Python: python, Destination: filepath.Join(root, "pm-runtime"),
		Project: peRuntimeProject(t), Wheelhouse: wheelhouse, Offline: true})
	if err != nil || !isRegularFile(executable) {
		t.Fatalf("executable %q %v", executable, err)
	}
	markerPath := filepath.Join(root, "pm-runtime/pm-runtime.json")
	if st, err := os.Stat(markerPath); err != nil || st.Mode().Perm() != 0o600 || !isRegularFile(filepath.Join(root, "pm-runtime/.lock")) {
		t.Fatalf("staged marker %v %v", st, err)
	}
	if err := sealPMRuntime(ctx, root, python); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(markerPath); err != nil || st.Mode().Perm() != 0o644 || pathExists(filepath.Join(root, "pm-runtime/.lock")) {
		t.Fatalf("sealed marker %v %v", st, err)
	}
	private := filepath.Join(tmp, "mutable/selected.json")
	if err := writeLockDocument(private, map[string]any{"runtime": "private"}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(private); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("private write %v %v", st, err)
	}
	moved := filepath.Join(tmp, "installed elsewhere")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(moved, "pm-runtime")
	var marker struct {
		Python       string `json:"python"`
		SitePackages string `json:"sitePackages"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(readFile(t, filepath.Join(runtimeDir, "pm-runtime.json")), []byte("\ufeff")), &marker); err != nil {
		t.Fatal(err)
	}
	probe := "import importlib.metadata, importlib.util, json, sys\nsys.path.insert(0, sys.argv[1])\nfrom packaging.utils import canonicalize_name\n" +
		"from ruamel.yaml import YAML\nimport packaging, tomli_w\nassert YAML(typ='safe').load('isolated: true') == {'isolated': True}\n" +
		"assert importlib.util.find_spec('openai') is None\nassert importlib.util.find_spec('yaml') is None\n" +
		"print(json.dumps({canonicalize_name(d.metadata['Name']): d.version for d in importlib.metadata.distributions(path=[sys.argv[1]])}))"
	env, err := RuntimeEnvironment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	site := filepath.Join(runtimeDir, marker.SitePackages)
	got := map[string]string{}
	if out := peRun(t, []string{filepath.Join(runtimeDir, marker.Python), "-I", "-S", "-B", "-c", probe, site}, tmp, env); json.Unmarshal([]byte(out), &got) != nil || !reflect.DeepEqual(got, versions) {
		t.Fatalf("distributions %s want %v", out, versions)
	}
	repo := filepath.Join(moved, "icebath-agent")
	writeFile(t, filepath.Join(moved, "manifest.json"), []byte(`{"repo":"icebath-agent"}`), 0o666)
	script := filepath.Join(repo, "probe.py")
	writeFile(t, script, []byte("import sys,json; print(json.dumps(sys.path))"), 0o666)
	previous := repoRoot
	repoRoot = func() string { return repo }
	command, err := RuntimeCommand(ctx, script, nil, RuntimeOptions{})
	repoRoot = previous
	if err != nil {
		t.Fatal(err)
	}
	var entries []string
	if err := json.Unmarshal([]byte(peRun(t, command, tmp, env)), &entries); err != nil {
		t.Fatal(err)
	}
	recorded := resolvePath(site)
	if !slices.Contains(entries, recorded) {
		t.Fatalf("site %s missing from %v", recorded, entries)
	}
	for _, entry := range entries {
		if name := filepath.Base(entry); (name == "site-packages" || name == "dist-packages") && resolvePath(entry) != recorded {
			t.Fatalf("foreign site %s", entry)
		}
	}
}
