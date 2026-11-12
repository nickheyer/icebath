package selfupdate

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
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

const sbHermesScripts = "testdata/hermes/scripts/build"

func sbWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sbRun(t *testing.T, dir string, argv ...string) string {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", argv, err, out)
	}
	return string(out)
}

func sbEvents(t *testing.T, root string) []map[string]any {
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

func sbSteps(t *testing.T, root string) []string {
	out := []string{}
	for _, e := range sbEvents(t, root) {
		out = append(out, e["step"].(string))
	}
	return out
}

// Captures the desktop and product build output for one test
func sbCapture(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	prev := desktopOut
	desktopOut = &buf
	t.Cleanup(func() { desktopOut = prev })
	return &buf
}

// Replaces the migrations, feature deps and stage publication of an update with no ops
func sbQuietUpdateSteps(t *testing.T) {
	prev := productSteps
	productSteps.featureDeps = func(context.Context, string) error { return nil }
	productSteps.memoryMigrate = func(context.Context, func(string)) ([]string, error) { return nil, nil }
	productSteps.leftCoreMigrate = func(context.Context, func(string)) ([]string, error) { return nil, nil }
	productSteps.publishStage = func(string) {}
	t.Cleanup(func() { productSteps = prev })
}

// Isolated checkout whose node-deps, freshness and npm runs are real, PM acquisition standing in host node and npm
func sbSourceCheckout(t *testing.T) (string, *[]string) {
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
	sbWrite(t, filepath.Join(homeDir, "npmrc"), "fund=false\n")
	t.Setenv("ICEBATH_HOME", homeDir)
	t.Setenv("ICEBATH_RUNTIME_DIR", filepath.Join(tmp, "tools"))
	t.Setenv("ICEBATH_DISABLE_LAZY_INSTALLS", "0")
	t.Setenv("npm_config_cache", filepath.Join(tmp, "npm-cache"))
	t.Setenv("ESBUILD_BINARY_PATH", "/wrong/esbuild")
	t.Setenv("ICEBATH_PYTHON", "/wrong/python")
	t.Setenv("NPM_CONFIG_USERCONFIG", "")
	os.Unsetenv("NPM_CONFIG_USERCONFIG")
	acquired := &[]string{}
	t.Cleanup(SwapSourceToolsForTests(func(_ context.Context, name string, opts pm.EnsureOptions) (*pm.Runner, error) {
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
			env = sourceEnviron()
		}
		env["PATH"] = strings.Join([]string{filepath.Dir(npm), filepath.Dir(node), os.Getenv("PATH")}, string(os.PathListSeparator))
		return &pm.Runner{Name: name, Env: env}, nil
	}, func(context.Context) (string, bool) { return node, true }))
	provider.InstallPluginManagerHooks(provider.PluginManagerHooks{
		CatalogEntry: memory.CatalogSource,
		Install: func(ctx context.Context, name string, consent bool) (map[string]any, error) {
			return plugins.DashboardInstallPlugin(ctx, plugins.DashboardInstallOptions{Enable: true, CatalogName: name, AssumeDepsConsent: &consent})
		},
		LoadError:       func(plugin string) string { return provider.PluginLoadError(context.Background(), plugin) },
		DependencyHomes: func() ([]string, error) { return pm.DependencyHomes(context.Background()) },
	})
	root := filepath.Join(tmp, "source with spaces")
	workspaces := []string{"ui-tui", "web", "apps/desktop", "unrelated"}
	manifest, _ := json.Marshal(map[string]any{"name": "build-fixture", "private": true, "version": "1.0.0",
		"workspaces": workspaces, "scripts": map[string]any{"postinstall": "node log.mjs deps"}})
	sbWrite(t, filepath.Join(root, "package.json"), string(manifest))
	for _, ws := range workspaces {
		scripts := map[string]any{}
		if ws == "apps/desktop" {
			scripts = map[string]any{"build": "node ../../scripts/build/build-desktop.mjs", "builder": "node ../../scripts/build/package-desktop.mjs"}
		}
		raw, _ := json.Marshal(map[string]any{"name": strings.ReplaceAll(ws, "/", "-"), "version": "1.0.0", "scripts": scripts})
		sbWrite(t, filepath.Join(root, ws, "package.json"), string(raw))
	}
	sbWrite(t, filepath.Join(root, "log.mjs"), "import { appendFileSync } from 'node:fs';\n"+
		"appendFileSync('events.jsonl', JSON.stringify({step: process.argv[2], "+
		"python: process.env.ICEBATH_PYTHON, ci: process.env.CI, "+
		"esbuild: process.env.ESBUILD_BINARY_PATH, "+
		"npmrc: process.env.NPM_CONFIG_USERCONFIG}) + '\\n');\n")
	sbRun(t, root, npm, "install", "--package-lock-only", "--ignore-scripts", "--offline", "--no-audit", "--no-fund")
	for _, name := range []string{"freshness.mjs", "frontend-common.mjs"} {
		raw, err := os.ReadFile(filepath.Join(sbHermesScripts, name))
		if err != nil {
			t.Fatal(err)
		}
		sbWrite(t, filepath.Join(root, "scripts", "build", name), string(raw))
	}
	sbWrite(t, filepath.Join(root, ".gitignore"), "node_modules/\n**/dist/\n")
	return root, acquired
}

// Checkout whose product scripts record their step and stamp a receipt
func sbSourceProducts(t *testing.T) (string, *[]string) {
	root, acquired := sbSourceCheckout(t)
	sbWrite(t, filepath.Join(root, "product.mjs"),
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
	for _, row := range [][3]string{{"generate-icons.mjs", "icons", "never-rendered-at-install"}} {
		relative := "../"
		if strings.HasPrefix(row[0], "build/") {
			relative = "../../"
		}
		sbWrite(t, filepath.Join(root, "scripts", row[0]), "import { build } from '"+relative+"product.mjs'; build('"+row[1]+"', '"+row[2]+"');\n")
	}
	sbWrite(t, filepath.Join(root, "scripts/build/package-desktop.mjs"), "import { relative } from 'node:path';\n"+
		"import { build } from '../../product.mjs';\n"+
		"const flag = '-c.directories.output=';\n"+
		"const staging = process.argv.find(arg => arg.startsWith(flag)).slice(flag.length);\n"+
		"build('desktop', relative('../..', staging) + '/linux-unpacked/icebath');\n")
	sbWrite(t, filepath.Join(root, "scripts/build/build-desktop.mjs"), "if (!process.argv.includes('--icons')) await import('../generate-icons.mjs');\n")
	sbWrite(t, filepath.Join(root, "web/public/favicon.ico"), "committed icon")
	node, _ := exec.LookPath("node")
	prev := compileSourceWeb
	compileSourceWeb = func(_ context.Context, _ *frontendStep, source, _, out string, env map[string]string) error {
		events, err := os.OpenFile(filepath.Join(source, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, err = events.WriteString(`{"step":"web"}` + "\n")
		events.Close()
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(source, "fail-web")); err == nil {
			return sbWebFailure
		}
		if _, err := os.Stat(filepath.Join(source, "web/public/favicon.ico")); err != nil {
			return errors.New("icons missing")
		}
		sbWrite(t, filepath.Join(out, "index.html"), "web")
		host, err := frontendNodeHost(node)
		if err != nil {
			return err
		}
		inputs, err := frontendBuildInputs(source, "web", map[string]string{}, env)
		if err != nil {
			return err
		}
		return frontendRecordProduct(host, source, "web", out, inputs, env, nil, false)
	}
	t.Cleanup(func() { compileSourceWeb = prev })
	prevTUI := compileSourceTUI
	compileSourceTUI = func(_ context.Context, _ *frontendStep, source string, env map[string]string) (string, error) {
		entry := filepath.Join(source, "ui-tui", "dist", "entry.js")
		events, err := os.OpenFile(filepath.Join(source, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return "", err
		}
		_, err = events.WriteString(`{"step":"tui"}` + "\n")
		events.Close()
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(filepath.Join(source, "fail-tui")); err == nil {
			return "", sbTUIFailure
		}
		sbWrite(t, entry, "tui")
		host, err := frontendNodeHost(node)
		if err != nil {
			return "", err
		}
		inputs, err := frontendBuildInputs(source, "tui", map[string]string{}, env)
		if err != nil {
			return "", err
		}
		return entry, frontendRecordProduct(host, source, "tui", filepath.Dir(entry), inputs, env, nil, false)
	}
	t.Cleanup(func() { compileSourceTUI = prevTUI })
	return root, acquired
}

// Failure the fixture TUI compiler reports when the checkout asks it to fail
var sbTUIFailure = errors.New("fixture tui failure")

// Failure the fixture dashboard compiler reports when the checkout asks it to fail
var sbWebFailure = errors.New("fixture web failure")

func sbNeedsLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("platforms linux")
	}
}

func sbNeedsPosix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
}

func TestAutomaticBuildPreservesPmAdmissionIntent(t *testing.T) {
	var intent []bool
	t.Cleanup(SwapSourceToolsForTests(func(_ context.Context, name string, opts pm.EnsureOptions) (*pm.Runner, error) {
		intent = append(intent, opts.Explicit)
		return &pm.Runner{Name: name, Env: opts.BaseEnv}, nil
	}, sourceNodeBinary))
	if _, err := SourceBuildEnv(context.Background(), nil, false, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(intent, []bool{false}) {
		t.Fatalf("intent %v", intent)
	}
}

func TestInstalledNpmDoesNotAuthorizeMissingWorkspaceDependencies(t *testing.T) {
	root, _ := sbSourceCheckout(t)
	ctx := context.Background()
	env, err := SourceBuildEnv(ctx, nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_DISABLE_LAZY_INSTALLS", "1")
	if err := PrepareSourceDependencies(ctx, root, []string{"ui-tui", "web"}, env, false); err == nil ||
		err.Error() != "Workspace dependencies are stale or missing and lazy installs are disabled; run an explicit build/update" {
		t.Fatalf("err %v", err)
	}
	if len(sbEvents(t, root)) != 0 {
		t.Fatalf("events %v", sbEvents(t, root))
	}
	if err := PrepareSourceDependencies(ctx, root, []string{"ui-tui", "web"}, env, true); err != nil {
		t.Fatal(err)
	}
	events := sbEvents(t, root)
	if err := PrepareSourceDependencies(ctx, root, []string{"ui-tui", "web"}, env, false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sbEvents(t, root), events) {
		t.Fatalf("events %v want %v", sbEvents(t, root), events)
	}
}

func TestPreparationReusesOnlyTheExactCompletedWorkspaceUnion(t *testing.T) {
	sbNeedsPosix(t)
	root, acquired := sbSourceCheckout(t)
	ctx := context.Background()
	before, _ := os.ReadFile(filepath.Join(root, "package-lock.json"))
	env, err := SourceBuildEnv(ctx, nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareSourceDependencies(ctx, root, []string{"ui-tui", "web"}, env, false); err != nil {
		t.Fatal(err)
	}
	first := sbEvents(t, root)
	if !reflect.DeepEqual(sbSteps(t, root), []string{"deps"}) || first[0]["python"] != env["ICEBATH_PYTHON"] || first[0]["ci"] != "1" {
		t.Fatalf("first %v", first)
	}
	if _, ok := first[0]["esbuild"]; ok {
		t.Fatalf("esbuild leaked %v", first[0])
	}
	if first[0]["npmrc"] != filepath.Join(os.Getenv("ICEBATH_HOME"), "npmrc") {
		t.Fatalf("npmrc %v", first[0]["npmrc"])
	}
	exists := func(rel string) bool { _, err := os.Stat(filepath.Join(root, rel)); return err == nil }
	if !exists("node_modules/ui-tui") || !exists("node_modules/web") || exists("node_modules/apps-desktop") || exists("node_modules/unrelated") {
		t.Fatal("wrong workspace union installed")
	}
	if err := PrepareSourceDependencies(ctx, root, []string{"ui-tui", "web"}, env, false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sbEvents(t, root), first) {
		t.Fatal("an identical union was prepared again")
	}
	if err := PrepareSourceDependencies(ctx, root, []string{"ui-tui", "web", "apps/desktop"}, env, false); err != nil {
		t.Fatal(err)
	}
	if len(sbEvents(t, root)) != 2 || !exists("node_modules/apps-desktop") || exists("node_modules/unrelated") {
		t.Fatalf("events %v", sbEvents(t, root))
	}
	if after, _ := os.ReadFile(filepath.Join(root, "package-lock.json")); !bytes.Equal(after, before) {
		t.Fatal("lock rewritten")
	}
	if !reflect.DeepEqual(*acquired, []string{"npm"}) {
		t.Fatalf("acquired %v", *acquired)
	}
	sbWrite(t, filepath.Join(root, "package-lock.json"), "not json")
	if err := PrepareSourceDependencies(ctx, root, []string{"ui-tui", "web"}, env, false); err == nil {
		t.Fatal("an unparsable lock was accepted")
	}
	if raw, _ := os.ReadFile(filepath.Join(root, "package-lock.json")); string(raw) != "not json" || len(sbEvents(t, root)) != 2 {
		t.Fatal("a lock failure fell back to npm install")
	}
	if !reflect.DeepEqual(*acquired, []string{"npm"}) {
		t.Fatalf("acquired %v", *acquired)
	}
	restore := SwapSourceToolsForTests(func(context.Context, string, pm.EnsureOptions) (*pm.Runner, error) {
		return nil, pm.NewInstallError("npm", "unavailable", "")
	}, sourceNodeBinary)
	_, err = SourceBuildEnv(ctx, nil, false, true)
	restore()
	var install *pm.InstallError
	if !errors.As(err, &install) || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("err %v", err)
	}
	if len(sbEvents(t, root)) != 2 {
		t.Fatal("events changed")
	}
}

func TestUpdateBuildsSelectedProductsAfterOneUnionPreparation(t *testing.T) {
	sbNeedsLinux(t)
	for _, desktop := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-desktop", true: "desktop"}[desktop], func(t *testing.T) {
			root, acquired := sbSourceProducts(t)
			sbCapture(t)
			app := filepath.Join(root, "apps/desktop/release/linux-unpacked/icebath")
			sbWrite(t, app, "previous app")
			if err := BuildUpdateProducts(context.Background(), root, desktop); err != nil {
				t.Fatal(err)
			}
			want := []string{"deps", "tui", "web"}
			if desktop {
				want = append(want, "desktop")
			}
			if got := sbSteps(t, root); !reflect.DeepEqual(got, want) {
				t.Fatalf("steps %v", got)
			}
			if !reflect.DeepEqual(*acquired, []string{"npm"}) {
				t.Fatalf("acquired %v", *acquired)
			}
			read := func(rel string) string { raw, _ := os.ReadFile(filepath.Join(root, rel)); return string(raw) }
			if read("ui-tui/dist/entry.js") != "tui" || read("cmd/icebath/web_dist/index.html") != "web" {
				t.Fatal("products not built")
			}
			if !SourceProductCurrent(context.Background(), root, "web", WebDistDir(root)) {
				t.Fatal("web UI build still needed")
			}
			if _, err := os.Stat(filepath.Join(root, "node_modules/apps-desktop")); (err == nil) != desktop {
				t.Fatal("desktop workspace union wrong")
			}
			if _, err := os.Stat(filepath.Join(root, "node_modules/unrelated")); err == nil {
				t.Fatal("unrelated workspace installed")
			}
			wantApp := "previous app"
			if desktop {
				wantApp = "desktop"
			}
			if got, _ := os.ReadFile(app); string(got) != wantApp {
				t.Fatalf("app %q", got)
			}
			if staged, _ := filepath.Glob(filepath.Join(root, "apps/desktop/.staging-*")); len(staged) != 0 {
				t.Fatalf("staging left %v", staged)
			}
		})
	}
}

func TestUpdateRecompilesOnlyProductsWhoseInputsChanged(t *testing.T) {
	sbNeedsLinux(t)
	root, _ := sbSourceProducts(t)
	sbCapture(t)
	ctx := context.Background()
	products := func() []string {
		out := []string{}
		for _, s := range sbSteps(t, root) {
			if s != "deps" {
				out = append(out, s)
			}
		}
		return out
	}
	if err := BuildUpdateProducts(ctx, root, true); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(root, "events.jsonl"))
	if err := BuildUpdateProducts(ctx, root, true); err != nil {
		t.Fatal(err)
	}
	if got := products(); !reflect.DeepEqual(got, []string{"desktop"}) {
		t.Fatalf("products %v", got)
	}
	os.Remove(filepath.Join(root, "events.jsonl"))
	sbWrite(t, filepath.Join(root, "web/src/changed.ts"), "export {}\n")
	if err := BuildUpdateProducts(ctx, root, false); err != nil {
		t.Fatal(err)
	}
	if got := products(); !reflect.DeepEqual(got, []string{"web"}) {
		t.Fatalf("products %v", got)
	}
}

func TestUpdateFailureRaisesWithoutRetriesOrReplacingLiveApp(t *testing.T) {
	sbNeedsLinux(t)
	for _, step := range []string{"tui", "web", "desktop"} {
		t.Run(step, func(t *testing.T) {
			root, acquired := sbSourceProducts(t)
			sbCapture(t)
			app := filepath.Join(root, "apps/desktop/release/linux-unpacked/icebath")
			sbWrite(t, app, "previous app")
			sbWrite(t, filepath.Join(root, "fail-"+step), "")
			err := BuildUpdateProducts(context.Background(), root, true)
			var failure *ProductBuildError
			if !errors.As(err, &failure) || len(failure.Failures) != 1 {
				t.Fatalf("err %v", err)
			}
			var called *pm.CalledProcessError
			fixture := map[string]error{"web": sbWebFailure, "tui": sbTUIFailure}[step]
			if fixture != nil && !errors.Is(failure.Failures[0].Err, fixture) || fixture == nil && !errors.As(failure.Failures[0].Err, &called) {
				t.Fatalf("failure %v", failure.Failures[0].Err)
			}
			if got := sbSteps(t, root); !reflect.DeepEqual(got, []string{"deps", "tui", "web", "desktop"}) {
				t.Fatalf("steps %v", got)
			}
			if !reflect.DeepEqual(*acquired, []string{"npm"}) {
				t.Fatalf("acquired %v", *acquired)
			}
			if staged, _ := filepath.Glob(filepath.Join(root, "apps/desktop/.staging-*")); len(staged) != 0 {
				t.Fatalf("staging left %v", staged)
			}
			if step == "desktop" {
				if got, _ := os.ReadFile(app); string(got) != "previous app" {
					t.Fatalf("app %q", got)
				}
				if _, err := os.Stat(filepath.Join(os.Getenv("ICEBATH_HOME"), "desktop-build-stamp.json")); err == nil {
					t.Fatal("failed build stamped")
				}
			}
		})
	}
}

func TestUnbuiltDesktopIsNamedOnOneWholeLine(t *testing.T) {
	sbNeedsLinux(t)
	for _, failure := range []string{"npm-unavailable", "desktop-after-long-feature-failure"} {
		t.Run(failure, func(t *testing.T) {
			root, _ := sbSourceProducts(t)
			out := sbCapture(t)
			expected := ""
			if failure == "npm-unavailable" {
				t.Cleanup(SwapSourceToolsForTests(func(context.Context, string, pm.EnsureOptions) (*pm.Runner, error) {
					return nil, pm.NewInstallError("npm", "unavailable", "")
				}, sourceNodeBinary))
				expected = "Desktop app build owed: Node dependencies failed"
			} else {
				prev := productSteps.featureDeps
				productSteps.featureDeps = func(context.Context, string) error {
					return errors.New("pip install failed: " + strings.Repeat("x", 600))
				}
				t.Cleanup(func() { productSteps.featureDeps = prev })
				sbWrite(t, filepath.Join(root, "fail-desktop"), "")
				expected = "Desktop app build owed: desktop app build failed"
			}
			var pbe *ProductBuildError
			if err := BuildUpdateProducts(context.Background(), root, true); !errors.As(err, &pbe) {
				t.Fatalf("err %v", err)
			}
			var lines []string
			for _, line := range strings.Split(out.String(), "\n") {
				if line = strings.TrimSpace(line); strings.HasPrefix(line, "Desktop app build owed:") {
					lines = append(lines, line)
				}
			}
			if !reflect.DeepEqual(lines, []string{expected}) {
				t.Fatalf("lines %v", lines)
			}
		})
	}
}

func TestDesktopDependencyFailureStillBuildsTheTuiAndWebUi(t *testing.T) {
	sbNeedsLinux(t)
	root, _ := sbSourceProducts(t)
	out := sbCapture(t)
	manifest := filepath.Join(root, "apps/desktop/package.json")
	var doc map[string]any
	raw, _ := os.ReadFile(manifest)
	_ = json.Unmarshal(raw, &doc)
	doc["scripts"] = map[string]any{"postinstall": "exit 1"}
	raw, _ = json.Marshal(doc)
	sbWrite(t, manifest, string(raw))
	var failure *ProductBuildError
	if err := BuildUpdateProducts(context.Background(), root, true); !errors.As(err, &failure) {
		t.Fatalf("err %v", err)
	}
	names := []string{}
	for _, f := range failure.Failures {
		names = append(names, f.Name)
	}
	if !reflect.DeepEqual(names, []string{"desktop dependencies"}) {
		t.Fatalf("failures %v", names)
	}
	if got := sbSteps(t, root); !reflect.DeepEqual(got, []string{"deps", "tui", "web"}) {
		t.Fatalf("steps %v", got)
	}
	if _, err := os.Stat(filepath.Join(root, "node_modules/apps-desktop")); err == nil {
		t.Fatal("desktop workspace installed")
	}
	if !strings.Contains(out.String(), "Desktop app build owed: desktop dependencies failed") || !strings.Contains(out.String(), "icebath uninstall --gui") {
		t.Fatalf("out %s", out.String())
	}
}

func TestPackagedDesktopIsReusedOnlyWhileItNamesHead(t *testing.T) {
	sbNeedsPosix(t)
	root := filepath.Join(t.TempDir(), "checkout")
	_ = os.MkdirAll(root, 0o755)
	git := []string{"git", "-C", root, "-c", "user.name=t", "-c", "user.email=t@example.invalid"}
	sbRun(t, root, "git", "-C", root, "init", "-q")
	sbRun(t, root, append(git, "commit", "-q", "--allow-empty", "-m", "one")...)
	desktopDir := filepath.Join(root, "apps/desktop")
	exe := filepath.Join(desktopDir, "release/app/Icebath")
	prevExe := desktopPackagedExecutable
	desktopPackagedExecutable = func(string) (string, bool) { return exe, true }
	t.Cleanup(func() { desktopPackagedExecutable = prevExe })
	resources := packagedResourcesDir(desktopDir)
	_ = os.MkdirAll(filepath.Join(resources, "app.asar.unpacked/dist"), 0o755)
	receiptCurrent := true
	prevNeeded := desktopBuildNeeded
	desktopBuildNeeded = func(context.Context, string, string, bool) bool { return !receiptCurrent }
	t.Cleanup(func() { desktopBuildNeeded = prevNeeded })
	head := func() string { return strings.TrimSpace(sbRun(t, root, "git", "-C", root, "rev-parse", "HEAD")) }
	reused := func() bool { return packagedDesktopCurrentForHead(context.Background(), desktopDir, root) }
	if reused() {
		t.Fatal("reused without a baked stamp")
	}
	raw, _ := json.Marshal(map[string]any{"commit": head()})
	sbWrite(t, filepath.Join(resources, "install-stamp.json"), string(raw))
	if !reused() {
		t.Fatal("not reused while naming HEAD")
	}
	receiptCurrent = false
	if reused() {
		t.Fatal("reused with a stale receipt")
	}
	receiptCurrent = true
	sbRun(t, root, append(git, "commit", "-q", "--allow-empty", "-m", "two")...)
	if reused() {
		t.Fatal("reused after HEAD moved")
	}
}

func TestUpdateRebuildsTheWebappBundleAHomeServes(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			tmp := t.TempDir()
			root := filepath.Join(tmp, "checkout")
			sbWrite(t, filepath.Join(root, "apps/desktop/vite.config.ts"), "")
			homes := []string{filepath.Join(tmp, "default"), filepath.Join(tmp, "profiles", "work")}
			for _, h := range homes {
				_ = os.MkdirAll(h, 0o755)
			}
			sbWrite(t, filepath.Join(homes[1], "config.yaml"), "webapp:\n  enabled: "+map[bool]string{false: "false", true: "true"}[enabled]+"\n")
			sbQuietUpdateSteps(t)
			sbCapture(t)
			productSteps.dependencyHomes = func(context.Context) ([]string, error) { return homes, nil }
			type build struct {
				root                       string
				force, skipBuild, explicit bool
			}
			var builds []build
			prev := PrepareWebappRenderer
			PrepareWebappRenderer = func(_ context.Context, projectRoot string, force, skipBuild, explicit bool) (string, error) {
				builds = append(builds, build{projectRoot, force, skipBuild, explicit})
				return "", nil
			}
			t.Cleanup(func() { PrepareWebappRenderer = prev })
			if err := BuildUpdateProducts(context.Background(), root, false); err != nil {
				t.Fatal(err)
			}
			var want []build
			if enabled {
				want = []build{{root, false, false, true}}
			}
			if !reflect.DeepEqual(builds, want) {
				t.Fatalf("builds %v", builds)
			}
		})
	}
}

func TestAFailedWebappRebuildInUpdateMarksTheServedBundleOutOfDate(t *testing.T) {
	for _, built := range []bool{true, false} {
		t.Run(map[bool]string{false: "unbuilt", true: "built"}[built], func(t *testing.T) {
			tmp := t.TempDir()
			root := filepath.Join(tmp, "checkout")
			sbWrite(t, filepath.Join(root, "apps/desktop/vite.config.ts"), "")
			dist := WebappDistDir(root)
			_ = os.MkdirAll(dist, 0o755)
			if built {
				sbWrite(t, filepath.Join(dist, "index.html"), "<!doctype html>")
			}
			h := filepath.Join(tmp, "default")
			sbWrite(t, filepath.Join(h, "config.yaml"), "webapp:\n  enabled: true\n")
			sbQuietUpdateSteps(t)
			out := sbCapture(t)
			productSteps.dependencyHomes = func(context.Context) ([]string, error) { return []string{h}, nil }
			prev := PrepareWebappRenderer
			PrepareWebappRenderer = func(context.Context, string, bool, bool, bool) (string, error) {
				return "", &WebappBuildError{"Browser-hosted Desktop build failed: npm ci exited 1"}
			}
			t.Cleanup(func() { PrepareWebappRenderer = prev })
			listing := func() []string {
				var all []string
				_ = filepath.Walk(root, func(p string, _ os.FileInfo, _ error) error { all = append(all, p); return nil })
				slices.Sort(all)
				return all
			}
			before := listing()
			err := BuildUpdateProducts(context.Background(), root, false)
			var failure *ProductBuildError
			if !errors.As(err, &failure) || !strings.Contains(err.Error(), "Webapp build") {
				t.Fatalf("err %v", err)
			}
			reason := gateway.BundleMissingReason(dist)
			if !strings.HasPrefix(reason, gateway.WebappNotBuilt) || !strings.Contains(reason, "icebath webapp") {
				t.Fatalf("reason %q", reason)
			}
			if built {
				if raw, _ := os.ReadFile(filepath.Join(dist, "index.html")); string(raw) != "<!doctype html>" {
					t.Fatalf("index %q", raw)
				}
				if !strings.Contains(out.String(), "icebath webapp") {
					t.Fatalf("out %s", out.String())
				}
			} else if !reflect.DeepEqual(listing(), before) {
				t.Fatal("an unbuilt bundle was marked")
			}
		})
	}
}
