package app

import (
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
	"time"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate"
)

const tniHermesScripts = "../../references/projects/hermes-agent/scripts/build"

// The desktop build of these cases runs its node steps under the custody launcher this binary serves
func init() { selfupdate.DispatchCustodyLauncher() }

func tniWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func tniRun(t *testing.T, dir string, argv ...string) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", argv, err, out)
	}
}

func tniEvents(t *testing.T, root string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "events.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return []map[string]any{}
	}
	if err != nil {
		t.Fatal(err)
	}
	out := []map[string]any{}
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	return out
}

func tniSteps(t *testing.T, root string) []string {
	out := []string{}
	for _, e := range tniEvents(t, root) {
		out = append(out, e["step"].(string))
	}
	return out
}

// Isolated checkout whose node-deps, freshness and npm runs are real, PM acquisition standing in the host node and npm
func tniSourceCheckout(t *testing.T) (string, *[]string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("source-build integration requires node and npm")
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Fatal("source-build integration requires node and npm")
	}
	tmp := t.TempDir()
	homeDir := filepath.Join(tmp, "home")
	tniWrite(t, filepath.Join(homeDir, "npmrc"), "fund=false\n")
	t.Setenv("ICEBATH_HOME", homeDir)
	t.Setenv("ICEBATH_RUNTIME_DIR", filepath.Join(tmp, "tools"))
	t.Setenv("ICEBATH_DISABLE_LAZY_INSTALLS", "0")
	t.Setenv("npm_config_cache", filepath.Join(tmp, "npm-cache"))
	t.Setenv("ESBUILD_BINARY_PATH", "/wrong/esbuild")
	t.Setenv("ICEBATH_PYTHON", "/wrong/python")
	t.Setenv("NPM_CONFIG_USERCONFIG", "")
	os.Unsetenv("NPM_CONFIG_USERCONFIG")
	acquired := &[]string{}
	restore := selfupdate.SwapSourceToolsForTests(func(_ context.Context, name string, opts pm.EnsureOptions) (*pm.Runner, error) {
		*acquired = append(*acquired, name)
		if name != "npm" {
			t.Fatalf("acquired %q", name)
		}
		env := map[string]string{}
		if opts.BaseEnv != nil {
			for k, v := range opts.BaseEnv {
				env[k] = v
			}
		} else {
			for _, kv := range os.Environ() {
				k, v, _ := strings.Cut(kv, "=")
				env[k] = v
			}
		}
		env["PATH"] = strings.Join([]string{filepath.Dir(npm), filepath.Dir(node), os.Getenv("PATH")}, string(os.PathListSeparator))
		return &pm.Runner{Name: name, Env: env}, nil
	}, func(context.Context) (string, bool) { return node, true })
	t.Cleanup(restore)
	root := filepath.Join(tmp, "source with spaces")
	workspaces := []string{"ui-tui", "web", "apps/desktop", "unrelated"}
	manifest, _ := json.Marshal(map[string]any{"name": "build-fixture", "private": true, "version": "1.0.0",
		"workspaces": workspaces, "scripts": map[string]any{"postinstall": "node log.mjs deps"}})
	tniWrite(t, filepath.Join(root, "package.json"), string(manifest))
	for _, ws := range workspaces {
		scripts := map[string]any{}
		if ws == "apps/desktop" {
			scripts = map[string]any{"build": "node ../../scripts/build/build-desktop.mjs", "builder": "node ../../scripts/build/package-desktop.mjs"}
		}
		raw, _ := json.Marshal(map[string]any{"name": strings.ReplaceAll(ws, "/", "-"), "version": "1.0.0", "scripts": scripts})
		tniWrite(t, filepath.Join(root, ws, "package.json"), string(raw))
	}
	tniWrite(t, filepath.Join(root, "log.mjs"), "import { appendFileSync } from 'node:fs';\n"+
		"appendFileSync('events.jsonl', JSON.stringify({step: process.argv[2], "+
		"python: process.env.ICEBATH_PYTHON, ci: process.env.CI, "+
		"esbuild: process.env.ESBUILD_BINARY_PATH, "+
		"npmrc: process.env.NPM_CONFIG_USERCONFIG}) + '\\n');\n")
	tniRun(t, root, npm, "install", "--package-lock-only", "--ignore-scripts", "--offline", "--no-audit", "--no-fund")
	for _, name := range []string{"node-deps.mjs", "electron-runtime.mjs", "freshness.mjs", "frontend-common.mjs"} {
		raw, err := os.ReadFile(filepath.Join(tniHermesScripts, name))
		if err != nil {
			t.Fatal(err)
		}
		tniWrite(t, filepath.Join(root, "scripts", "build", name), string(raw))
	}
	tniWrite(t, filepath.Join(root, ".gitignore"), "node_modules/\n**/dist/\n")
	return root, acquired
}

// Checkout whose product scripts record their step and stamp a receipt
func tniSourceProducts(t *testing.T) (string, *[]string) {
	root, acquired := tniSourceCheckout(t)
	tniWrite(t, filepath.Join(root, "product.mjs"),
		"import { appendFileSync, existsSync, mkdirSync, writeFileSync } from 'node:fs';\n"+
			"import { dirname, join } from 'node:path';\n"+
			"import { fileURLToPath } from 'node:url';\n"+
			"import { recordProduct, buildInputs } from './scripts/build/freshness.mjs';\n"+
			"const root = dirname(fileURLToPath(import.meta.url));\n"+
			"export function build(step, output) {\n"+
			"  appendFileSync(join(root, 'events.jsonl'), JSON.stringify({step}) + '\\n');\n"+
			"  if (existsSync(join(root, 'fail-' + step))) throw new Error('fixture ' + step + ' failure');\n"+
			"  if (step === 'web' && !existsSync(join(root, 'web/public/favicon.ico'))) throw new Error('icons missing');\n"+
			"  const path = join(root, output); mkdirSync(dirname(path), { recursive: true });\n"+
			"  writeFileSync(path, step);\n"+
			"  if (step !== 'icons') {\n"+
			"    let out = dirname(path);\n"+
			"    if (step === 'desktop') { out = join(out, 'resources/app.asar.unpacked/dist');\n"+
			"      mkdirSync(out, {recursive: true}); writeFileSync(join(out, 'index.html'), 'renderer'); }\n"+
			"    recordProduct({source: root, product: step, out, inputs: buildInputs(root, step)});\n"+
			"  }\n"+
			"}\n")
	for _, row := range [][3]string{{"generate-icons.mjs", "icons", "never-rendered-at-install"},
		{"build/tui.mjs", "tui", "ui-tui/dist/entry.js"}, {"build/web.mjs", "web", "cmd/icebath/web_dist/index.html"}} {
		relative := "../"
		if strings.HasPrefix(row[0], "build/") {
			relative = "../../"
		}
		tniWrite(t, filepath.Join(root, "scripts", row[0]), "import { build } from '"+relative+"product.mjs'; build('"+row[1]+"', '"+row[2]+"');\n")
	}
	tniWrite(t, filepath.Join(root, "scripts/build/package-desktop.mjs"), "import { relative } from 'node:path';\n"+
		"import { build } from '../../product.mjs';\n"+
		"const flag = '-c.directories.output=';\n"+
		"const staging = process.argv.find(arg => arg.startsWith(flag)).slice(flag.length);\n"+
		"build('desktop', relative('../..', staging) + '/linux-unpacked/icebath');\n")
	tniWrite(t, filepath.Join(root, "scripts/build/build-desktop.mjs"), "if (!process.argv.includes('--icons')) await import('../generate-icons.mjs');\n")
	tniWrite(t, filepath.Join(root, "web/public/favicon.ico"), "committed icon")
	return root, acquired
}

func tniStampProduct(t *testing.T, root, product, out string) {
	node, _ := exec.LookPath("node")
	script := "file://" + filepath.ToSlash(filepath.Join(root, "scripts/build/freshness.mjs"))
	quoted, _ := json.Marshal(script)
	tniRun(t, root, node, "--input-type=module", "-e", "import {recordProduct, buildInputs} from "+string(quoted)+";"+
		"const [source, product, out] = process.argv.slice(1);"+
		"recordProduct({source, product, out, inputs: buildInputs(source, product)});", root, product, out)
}

func tniTouchTUIEntry(t *testing.T, root string) {
	tniWrite(t, filepath.Join(root, "dist", "entry.js"), "console.log('tui')")
}

// Launch fixture over the product checkout with no bundled TUI and the host node standing in
func tniTUISource(t *testing.T) (string, *[]string) {
	root, acquired := tniSourceProducts(t)
	for _, name := range []string{"ICEBATH_TUI_DIR", "ICEBATH_TUI_FORCE_BUILD", "TERMUX_VERSION"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	t.Setenv("PREFIX", "/usr")
	node, _ := exec.LookPath("node")
	t.Setenv("ICEBATH_NODE", node)
	t.Setenv("ICEBATH_PYTHON_SRC_ROOT", "")
	tniBundledTUI(t, "")
	return root, acquired
}

// Makes the bundled TUI lookup answer the given entry
func tniBundledTUI(t *testing.T, entry string) {
	prev := tuiBundledEntry
	tuiBundledEntry = func() string { return entry }
	t.Cleanup(func() { tuiBundledEntry = prev })
}

func tniArgv(t *testing.T, tuiDir string, dev bool) ([]string, string, error) {
	var out, errOut strings.Builder
	return makeTUIArgvTo(&out, &errOut)(context.Background(), tuiDir, dev)
}

func TestNeedRebuildWhenTuiBundleMissing(t *testing.T) {
	tmp := t.TempDir()
	tniWrite(t, filepath.Join(tmp, "src", "entry.tsx"), "console.log('src')")
	if !tuiNeedRebuild(context.Background(), tmp) {
		t.Fatal("a missing bundle must rebuild")
	}
}

func TestUnreceiptedBundleIsStaleEvenWhenNewer(t *testing.T) {
	tmp := t.TempDir()
	tniTouchTUIEntry(t, tmp)
	src := filepath.Join(tmp, "src", "entry.tsx")
	tniWrite(t, src, "console.log('src')")
	_ = os.Chtimes(src, time.Unix(100, 0), time.Unix(100, 0))
	_ = os.Chtimes(filepath.Join(tmp, "dist", "entry.js"), time.Unix(200, 0), time.Unix(200, 0))
	if !tuiNeedRebuild(context.Background(), tmp) {
		t.Fatal("an unreceipted bundle must rebuild")
	}
}

func TestSourceLaunchPreparesBaseUnionButCompilesOnlyTui(t *testing.T) {
	root, acquired := tniTUISource(t)
	argv, cwd, err := tniArgv(t, filepath.Join(root, "ui-tui"), false)
	if err != nil {
		t.Fatal(err)
	}
	if cwd != filepath.Join(root, "ui-tui") || argv[len(argv)-1] != filepath.Join(cwd, "dist", "entry.js") {
		t.Fatalf("argv %v cwd %s", argv, cwd)
	}
	if raw, _ := os.ReadFile(filepath.Join(cwd, "dist", "entry.js")); string(raw) != "tui" {
		t.Fatalf("entry %q", raw)
	}
	if got := tniSteps(t, root); !reflect.DeepEqual(got, []string{"deps", "tui"}) {
		t.Fatalf("steps %v", got)
	}
	if !reflect.DeepEqual(*acquired, []string{"npm"}) {
		t.Fatalf("acquired %v", *acquired)
	}
	for name, want := range map[string]bool{"ui-tui": true, "web": true, "apps-desktop": false} {
		if _, err := os.Stat(filepath.Join(root, "node_modules", name)); (err == nil) != want {
			t.Fatalf("node_modules/%s present=%v", name, err == nil)
		}
	}
}

func TestSourceCompileFailureStopsLaunchWithoutReinstall(t *testing.T) {
	root, acquired := tniTUISource(t)
	tniWrite(t, filepath.Join(root, "fail-tui"), "")
	_, _, err := tniArgv(t, filepath.Join(root, "ui-tui"), false)
	var called *pm.CalledProcessError
	if !errors.As(err, &called) {
		t.Fatalf("err %v", err)
	}
	if got := tniSteps(t, root); !reflect.DeepEqual(got, []string{"deps", "tui"}) {
		t.Fatalf("steps %v", got)
	}
	if !reflect.DeepEqual(*acquired, []string{"npm"}) {
		t.Fatalf("acquired %v", *acquired)
	}
}

func TestFreshBundleDoesNotPrepareOrCompile(t *testing.T) {
	for _, termux := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "termux"}[termux], func(t *testing.T) {
			root, acquired := tniTUISource(t)
			tniTouchTUIEntry(t, filepath.Join(root, "ui-tui"))
			tniStampProduct(t, root, "tui", filepath.Join(root, "ui-tui", "dist"))
			if termux {
				t.Setenv("TERMUX_VERSION", "test")
			}
			argv, cwd, err := tniArgv(t, filepath.Join(root, "ui-tui"), false)
			if err != nil {
				t.Fatal(err)
			}
			if argv[len(argv)-1] != filepath.Join(cwd, "dist", "entry.js") {
				t.Fatalf("argv %v", argv)
			}
			if len(tniEvents(t, root)) != 0 || len(*acquired) != 0 {
				t.Fatalf("events %v acquired %v", tniEvents(t, root), *acquired)
			}
		})
	}
}

func TestPrebuiltBundleLaunchDoesNotTouchMissingSource(t *testing.T) {
	tmp := t.TempDir()
	bundled := filepath.Join(tmp, "bundle", "entry.js")
	tniWrite(t, bundled, "console.log('prebuilt')")
	t.Setenv("ICEBATH_TUI_DIR", "")
	os.Unsetenv("ICEBATH_TUI_DIR")
	node, _ := exec.LookPath("node")
	t.Setenv("ICEBATH_NODE", node)
	tniBundledTUI(t, bundled)
	argv, cwd, err := tniArgv(t, filepath.Join(tmp, "missing-source"), false)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "prebuilt" {
		t.Fatalf("out %q err %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "missing-source")); err == nil {
		t.Fatal("missing source was created")
	}
}

func TestDevLaunchBuildsInkBeforeRunningSource(t *testing.T) {
	for _, localTsx := range []bool{false, true} {
		t.Run(map[bool]string{false: "npm-start", true: "local-tsx"}[localTsx], func(t *testing.T) {
			root, acquired := tniTUISource(t)
			ink := filepath.Join(root, "ui-tui", "packages", "hermes-ink")
			tniWrite(t, filepath.Join(ink, "package.json"), `{"name": "fixture-ink", "version": "1.0.0", "scripts": {"build": "node build.mjs"}}`)
			tniWrite(t, filepath.Join(ink, "build.mjs"), "import { writeFileSync } from 'node:fs'; writeFileSync('built', 'ink');")
			var manifest map[string]any
			raw, _ := os.ReadFile(filepath.Join(root, "package.json"))
			_ = json.Unmarshal(raw, &manifest)
			manifest["workspaces"] = append(manifest["workspaces"].([]any), "ui-tui/packages/hermes-ink")
			raw, _ = json.Marshal(manifest)
			tniWrite(t, filepath.Join(root, "package.json"), string(raw))
			npm, _ := exec.LookPath("npm")
			tniRun(t, root, npm, "install", "--package-lock-only", "--ignore-scripts", "--offline")
			tsx := filepath.Join(root, "ui-tui", "node_modules", ".bin", "tsx")
			if localTsx {
				hook := filepath.Join(root, "log.mjs")
				prev, _ := os.ReadFile(hook)
				tniWrite(t, hook, string(prev)+"\nimport { mkdirSync, writeFileSync } from 'node:fs'; "+
					"mkdirSync('ui-tui/node_modules/.bin', {recursive: true}); "+
					"writeFileSync('ui-tui/node_modules/.bin/tsx', 'fixture tsx');\n")
			}
			argv, cwd, err := tniArgv(t, filepath.Join(root, "ui-tui"), true)
			if err != nil {
				t.Fatal(err)
			}
			if built, _ := os.ReadFile(filepath.Join(ink, "built")); string(built) != "ink" {
				t.Fatalf("ink built %q", built)
			}
			want := []string{npm, "start"}
			if localTsx {
				want = []string{tsx, "src/entry.tsx"}
			}
			if !reflect.DeepEqual(argv, want) || cwd != filepath.Join(root, "ui-tui") {
				t.Fatalf("argv %v cwd %s", argv, cwd)
			}
			if !reflect.DeepEqual(*acquired, []string{"npm"}) || !reflect.DeepEqual(tniSteps(t, root), []string{"deps"}) {
				t.Fatalf("acquired %v steps %v", *acquired, tniSteps(t, root))
			}
		})
	}
}

func TestRuntimeNodeComesFromPmWithoutLegacyRepair(t *testing.T) {
	node, _ := exec.LookPath("node")
	managed := filepath.Join(t.TempDir(), "bin", "node")
	_ = os.MkdirAll(filepath.Dir(managed), 0o755)
	if err := os.Symlink(node, managed); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_NODE", "")
	os.Unsetenv("ICEBATH_NODE")
	acquired := []string{}
	prev := tuiPMEnsurePath
	tuiPMEnsurePath = func(_ context.Context, name string) (string, error) {
		acquired = append(acquired, name)
		return filepath.Dir(managed), nil
	}
	t.Cleanup(func() { tuiPMEnsurePath = prev })
	got, err := tuiNodeBin(context.Background(), "node", &strings.Builder{})
	if err != nil || got != managed || !reflect.DeepEqual(acquired, []string{"node"}) {
		t.Fatalf("got %q err %v acquired %v", got, err, acquired)
	}
}

func TestTuiRebuildPreservesThePreparedDesktopAndWebUnion(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("platforms linux")
	}
	root, acquired := tniTUISource(t)
	if err := selfupdate.BuildUpdateProducts(context.Background(), root, true); err != nil {
		t.Fatal(err)
	}
	before := tniEvents(t, root)
	if _, _, err := tniArgv(t, filepath.Join(root, "ui-tui"), false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tniEvents(t, root), before) {
		t.Fatal("fresh launch must not prepare or rebuild")
	}
	tniWrite(t, filepath.Join(root, "ui-tui", "src", "entry.tsx"), "changed tui source")
	_ = os.Chtimes(filepath.Join(root, "ui-tui", "dist", "entry.js"), time.Unix(1, 0), time.Unix(1, 0))
	if _, _, err := tniArgv(t, filepath.Join(root, "ui-tui"), false); err != nil {
		t.Fatal(err)
	}
	if want := append(before, map[string]any{"step": "tui"}); !reflect.DeepEqual(tniEvents(t, root), want) {
		t.Fatalf("events %v", tniEvents(t, root))
	}
	if !reflect.DeepEqual(*acquired, []string{"npm", "npm"}) {
		t.Fatalf("acquired %v", *acquired)
	}
	for _, rel := range []string{"node_modules/web", "node_modules/apps-desktop"} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Fatalf("%s missing", rel)
		}
	}
}
