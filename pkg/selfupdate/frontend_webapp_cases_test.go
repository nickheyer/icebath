package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// Fixture vite whose build bundles index.html's entry, inlines static imports and splits dynamic ones into hashed chunks
var webappFixtureVite = map[string]string{
	"package.json": `{"name":"vite","version":"0.0.0-fixture","type":"module","bin":{"vite":"bin/vite.js"}}`,
	"dist/node/index.js": `import { existsSync } from 'node:fs'
import { join } from 'node:path'
export async function loadConfigFromFile(env, file, root) {
  const found = file ?? ['vite.config.ts', 'vite.config.js', 'vite.config.mjs'].map(name => join(root, name)).find(existsSync)
  return found ? { path: found, config: {} } : null
}
const plain = value => value && typeof value === 'object' && !Array.isArray(value)
export function mergeConfig(a, b) {
  const out = { ...a }
  for (const [key, value] of Object.entries(b)) out[key] = plain(value) && plain(out[key]) ? mergeConfig(out[key], value) : value
  return out
}
`,
	"bin/vite.js": `import { createHash } from 'node:crypto'
import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { pathToFileURL } from 'node:url'
const args = process.argv.slice(2)
const config = await (await import(pathToFileURL(args[args.indexOf('--config') + 1]).href)).default({ command: 'build', mode: 'production' })
const { root, build } = config
if (build.emptyOutDir) rmSync(build.outDir, { recursive: true, force: true })
mkdirSync(join(build.outDir, 'assets'), { recursive: true })
const manifest = {}
const emit = (file, base, text) => {
  const name = 'assets/' + base + '-' + createHash('sha256').update(text).digest('hex').slice(0, 8) + '.js'
  writeFileSync(join(build.outDir, name), text)
  manifest[relative(root, file).split('\\').join('/')] = { file: name }
  return name
}
const source = file => readFileSync(file.endsWith('.ts') ? file : file + '.ts', 'utf8')
const bundle = file => {
  let text = source(file)
  text = text.replace(/import\("([^"]+)"\)/g, (_, spec) => {
    const target = resolve(dirname(file), spec)
    return 'import("/' + emit(target + '.ts', 'lazy', bundle(target)) + '")'
  })
  return text.replace(/import \{[^}]*\} from "([^"]+)";?/g, (_, spec) => bundle(resolve(dirname(file), spec)))
}
const html = readFileSync(join(root, 'index.html'), 'utf8')
const entry = join(root, html.match(/src="\/([^"]+)"/)[1])
const main = emit(entry, 'index', bundle(entry.replace(/\.ts$/, '')))
writeFileSync(join(build.outDir, 'index.html'), html.replace(/src="\/[^"]+"/, 'src="/' + main + '"'))
if (build.manifest) writeFileSync(join(build.outDir, build.manifest), JSON.stringify(manifest))
`,
}

// Writes content at name under dir, creating its parents
func webappPut(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The checkout the webapp cases build, three linked workspaces in one lock
func webappSource(t *testing.T, source string) {
	t.Helper()
	webappPut(t, source, "package.json", `{"private":true,"type":"module","workspaces":["apps/*","web"]}`)
	webappPut(t, source, "package-lock.json", `{"lockfileVersion":3,"packages":{"":{"workspaces":["apps/*","web"]},"apps/desktop":{"name":"desktop"},`+
		`"apps/shared":{"name":"shared"},"web":{"name":"web"},"node_modules/desktop":{"link":true,"resolved":"apps/desktop"},`+
		`"node_modules/shared":{"link":true,"resolved":"apps/shared"},"node_modules/web":{"link":true,"resolved":"web"}}}`)
	webappPut(t, source, ".npmrc", "engine-strict=true\n")
	webappPut(t, source, "apps/desktop/package.json", `{"name":"desktop","type":"module"}`)
	webappPut(t, source, "apps/desktop/index.html", `<html><body><script type="module" src="/src/main.ts"></script></body></html>`)
	webappPut(t, source, "apps/desktop/src/main.ts", `import { answer } from "../../shared/src/value"; document.body.textContent = answer;`)
	webappPut(t, source, "apps/shared/package.json", `{"name":"shared"}`)
	webappPut(t, source, "apps/shared/src/value.ts", `export const answer: string = "browser renderer";`)
	webappPut(t, source, "web/package.json", `{"name":"web"}`)
}

// Installs the fixture vite into root's node_modules
func webappDependency(t *testing.T, root string) {
	t.Helper()
	for name, content := range webappFixtureVite {
		webappPut(t, filepath.Join(root, "node_modules", "vite"), name, content)
	}
}

// Build environment and node host of the cases, failing when node is absent
func webappEnv(t *testing.T) (map[string]string, nodeHost) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required on PATH")
	}
	host, err := frontendNodeHost(node)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"PATH": os.Getenv("PATH"), "HOME": t.TempDir(), "TMPDIR": t.TempDir()}, host
}

func TestTheWebappReceiptCertifiesItsCheckoutInputsNotTheWorkspaceItCompiledIn(t *testing.T) {
	base := t.TempDir()
	source, workspace := filepath.Join(base, "source"), filepath.Join(base, "workspace")
	out := filepath.Join(source, "apps", "desktop", "dist-webapp")
	webappSource(t, source)
	webappDependency(t, workspace)
	env, host := webappEnv(t)
	ctx := context.Background()
	build := func() error {
		if err := frontendMirrorWebappInputs(source, workspace); err != nil {
			return err
		}
		return frontendBuildWebapp(ctx, newFrontendStep("webapp"), source, workspace, out, env)
	}
	current := func() bool { return frontendProductCurrent(host, source, "webapp", out, nil, env) }
	if err := build(); err != nil {
		t.Fatal(err)
	}
	if !current() {
		t.Fatal("fresh build is not current")
	}
	if index, _ := os.ReadFile(filepath.Join(out, "index.html")); !strings.Contains(string(index), "<script") {
		t.Fatalf("index %q", index)
	}
	for _, name := range []string{"web/src/unrelated.ts", "pyproject.toml", "assets/icon.svg", "apps/desktop/dist/index.html", "scripts/build/tui.mjs"} {
		webappPut(t, source, name, "not a Webapp input")
		if !current() {
			t.Fatalf("%s rebuilt the renderer", name)
		}
	}
	lock, _ := os.ReadFile(filepath.Join(source, "package-lock.json"))
	for _, edit := range [][2]string{
		{"apps/shared/src/value.ts", `export const answer: string = "edited shared";`},
		{"apps/desktop/src/main.ts", `document.body.textContent = "edited desktop";`},
		{"package-lock.json", strings.Replace(string(lock), `"lockfileVersion":3`, `"lockfileVersion":3,"requires":true`, 1)},
		{".npmrc", "engine-strict=false\n"},
		{"pm/lock.json", "{}"},
		{"scripts/build/webapp.mjs", "// compiler changed"},
	} {
		webappPut(t, source, edit[0], edit[1])
		if current() {
			t.Fatalf("%s left the renderer current", edit[0])
		}
		if err := build(); err != nil {
			t.Fatal(err)
		}
		if !current() {
			t.Fatalf("not current after rebuilding for %s", edit[0])
		}
	}
	webappPut(t, workspace, "apps/desktop/src/main.ts", `document.body.textContent = "stale mirror";`)
	if err := frontendBuildWebapp(ctx, newFrontendStep("webapp"), source, workspace, out, env); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("stale mirror published: %v", err)
	}
	if !current() {
		t.Fatal("refused build disturbed the current output")
	}
	if err := os.WriteFile(filepath.Join(out, "index.html"), []byte("damaged output"), 0o644); err != nil {
		t.Fatal(err)
	}
	if current() {
		t.Fatal("damaged output is current")
	}
}

func TestAWebappRebuildKeepsThePreviousGenerationForTabsThatBootedItNeverAnOlderOne(t *testing.T) {
	base := t.TempDir()
	source, workspace := filepath.Join(base, "source"), filepath.Join(base, "workspace")
	out := filepath.Join(source, "apps", "desktop", "dist-webapp")
	webappSource(t, source)
	webappDependency(t, workspace)
	env, host := webappEnv(t)
	chunk := func(marker string) bool {
		entries, _ := os.ReadDir(filepath.Join(out, "assets"))
		for _, e := range entries {
			if data, _ := os.ReadFile(filepath.Join(out, "assets", e.Name())); strings.Contains(string(data), marker) {
				return true
			}
		}
		return false
	}
	for _, generation := range []string{"gen-one", "gen-two", "gen-three"} {
		webappPut(t, source, "apps/desktop/src/lazy.ts", "export const text = '"+generation+"-marker'")
		webappPut(t, source, "apps/desktop/src/main.ts", `import("./lazy").then(m => { document.body.textContent = m.text })`)
		if err := frontendMirrorWebappInputs(source, workspace); err != nil {
			t.Fatal(err)
		}
		if err := frontendBuildWebapp(context.Background(), newFrontendStep("webapp"), source, workspace, out, env); err != nil {
			t.Fatal(err)
		}
		if !frontendProductCurrent(host, source, "webapp", out, nil, env) {
			t.Fatalf("%s not current", generation)
		}
		if generation == "gen-two" {
			webappPut(t, out, frontendStaleMarker, "icebath update could not rebuild this bundle\n")
			if frontendProductCurrent(host, source, "webapp", out, nil, env) {
				t.Fatal("stale marker did not force the rebuild")
			}
		}
	}
	if !chunk("gen-three-marker") || !chunk("gen-two-marker") || chunk("gen-one-marker") {
		t.Fatalf("generations three %v two %v one %v", chunk("gen-three-marker"), chunk("gen-two-marker"), chunk("gen-one-marker"))
	}
	previous, next, outside := t.TempDir(), t.TempDir(), t.TempDir()
	webappPut(t, outside, "secret.txt", "outside")
	if err := os.Symlink(outside, filepath.Join(previous, "linked")); err != nil {
		t.Fatal(err)
	}
	for _, f := range [][2]string{{"index.html", "old"}, {"assets/ok.js", "old chunk"}, {"assets/same.js", "old same"}, {"x/kept.js", "old"}} {
		webappPut(t, previous, f[0], f[1])
	}
	for _, f := range [][2]string{{"index.html", "new"}, {"assets/same.js", "new same"}, {"assets/only-new.js", "new"}, {"x", "new file"}} {
		webappPut(t, next, f[0], f[1])
	}
	manifest, _ := json.Marshal(map[string]any{"a": map[string]any{"file": "assets/ok.js", "css": []string{"../escape.css", "/abs.css"},
		"assets": []string{"index.html", "assets/same.js", "assets/only-new.js", "linked/secret.txt", "bad\x00name.js", "assets/ok.js/child", "x/kept.js"}}})
	webappPut(t, previous, frontendWebappManifest, string(manifest))
	record := func(dir string) {
		inputs, err := frontendBuildInputs(base, "webapp", nil, env)
		if err != nil {
			t.Fatal(err)
		}
		if err := frontendRecordProduct(host, base, "webapp", dir, inputs, env, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	record(previous)
	if kept := frontendKeepPreviousGeneration(previous, next); !slices.Equal(kept, []string{"assets/ok.js"}) {
		t.Fatalf("kept %v", kept)
	}
	for name, want := range map[string]string{"index.html": "new", "assets/same.js": "new same", "assets/only-new.js": "new", "x": "new file"} {
		if data, _ := os.ReadFile(filepath.Join(next, name)); string(data) != want {
			t.Fatalf("%s = %q", name, data)
		}
	}
	if _, err := os.Lstat(filepath.Join(next, "linked")); err == nil {
		t.Fatal("a symlinked entry was kept")
	}
	record(next)
	if !frontendOutputIntact(next, "webapp") {
		t.Fatal("recorded next generation is not intact")
	}
	os.Remove(filepath.Join(next, "assets", "ok.js"))
	webappPut(t, previous, "assets/ok.js", "damaged")
	if kept := frontendKeepPreviousGeneration(previous, next); len(kept) != 0 {
		t.Fatalf("damaged previous generation kept %v", kept)
	}
}

func TestWebappBuildsInstallPrivatelyAndNeverDisturbTheNativeDesktopInstallation(t *testing.T) {
	base := t.TempDir()
	source, workspace := filepath.Join(base, "source"), filepath.Join(base, "workspace")
	webappSource(t, source)
	webappPut(t, source, "node_modules/electron/dist/electron", "native install")
	webappPut(t, source, "apps/desktop/dist/index.html", "native renderer")
	webappPut(t, source, "apps/desktop/src/removed-later.ts", "export {}")
	env, _ := webappEnv(t)
	saved := frontendWebappPrepare
	t.Cleanup(func() { frontendWebappPrepare = saved })
	var installs []bool
	frontendWebappPrepare = func(_ context.Context, _ *frontendStep, ws string, install bool, prepEnv map[string]string) error {
		installs = append(installs, install)
		if ws != workspace || prepEnv["npm_config_ignore_scripts"] != "true" {
			t.Fatalf("prepare %s %v", ws, prepEnv)
		}
		if _, err := os.Stat(filepath.Join(workspace, "web", "package.json")); err != nil {
			t.Fatal("npm ci needs every locked workspace manifest")
		}
		if _, err := os.Stat(filepath.Join(workspace, "node_modules", "vite")); err != nil {
			webappDependency(t, workspace)
		}
		return nil
	}
	if err := frontendBuildSourceWebapp(context.Background(), newFrontendStep("webapp"), source, workspace, false, env); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(source, "apps", "desktop", "src", "removed-later.ts"))
	if err := frontendBuildSourceWebapp(context.Background(), newFrontendStep("webapp"), source, workspace, true, env); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(installs, []bool{false, true}) {
		t.Fatalf("installs %v", installs)
	}
	if _, err := os.Stat(filepath.Join(workspace, "apps", "desktop", "src", "removed-later.ts")); err == nil {
		t.Fatal("removed input survived in the workspace")
	}
	if _, err := os.Stat(filepath.Join(workspace, "node_modules", "vite", "package.json")); err != nil {
		t.Fatal("workspace dependencies removed")
	}
	for name, want := range map[string]string{"node_modules/electron/dist/electron": "native install", "apps/desktop/dist/index.html": "native renderer"} {
		if data, _ := os.ReadFile(filepath.Join(source, filepath.FromSlash(name))); string(data) != want {
			t.Fatalf("%s = %q", name, data)
		}
	}
	if _, err := os.Stat(filepath.Join(source, "apps", "desktop", "dist-webapp", frontendReceiptName)); err != nil {
		t.Fatal("no receipt published")
	}
}

// Previous and next generations with the previous one recorded under a receipt of root
func webappGenerations(t *testing.T, root string, previousFiles, nextFiles map[string]string, manifest any) (string, string) {
	t.Helper()
	previous, next := filepath.Join(root, "previous"), filepath.Join(root, "next")
	for name, content := range previousFiles {
		webappPut(t, previous, name, content)
	}
	for name, content := range nextFiles {
		webappPut(t, next, name, content)
	}
	raw, _ := json.Marshal(manifest)
	webappPut(t, previous, frontendWebappManifest, string(raw))
	env, host := webappEnv(t)
	inputs, err := frontendBuildInputs(root, "webapp", nil, env)
	if err != nil {
		t.Fatal(err)
	}
	if err := frontendRecordProduct(host, root, "webapp", previous, inputs, env, nil, false); err != nil {
		t.Fatal(err)
	}
	return previous, next
}

// Faults the kept chunk copies from the named sources, writing the given partial bytes first
func webappFaultCopies(t *testing.T, faults map[string][2]string) {
	t.Helper()
	saved := frontendWebappCopy
	t.Cleanup(func() { frontendWebappCopy = saved })
	frontendWebappCopy = func(from, to string) error {
		if fault, ok := faults[from]; ok {
			if fault[1] != "" {
				os.WriteFile(to, []byte(fault[1]), 0o644)
			}
			code := map[string]syscall.Errno{"ENOSPC": syscall.ENOSPC, "EEXIST": syscall.EEXIST}[fault[0]]
			return &os.PathError{Op: "copyfile", Path: from, Err: code}
		}
		return saved(from, to)
	}
}

// Captures the webapp warnings of one case
func webappWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	saved := frontendWebappWarn
	var buf bytes.Buffer
	frontendWebappWarn = &buf
	t.Cleanup(func() { frontendWebappWarn = saved })
	return &buf
}

func TestAChunkThatCannotBeCopiedIsReportedWithItsPathAndTheRestAreStillKept(t *testing.T) {
	root := t.TempDir()
	warn := webappWarnings(t)
	previous, next := webappGenerations(t, root, map[string]string{"index.html": "old", "assets/full.js": "old full", "assets/ok.js": "old ok"},
		map[string]string{"index.html": "new"}, map[string]any{"a": map[string]any{"file": "assets/full.js", "assets": []string{"assets/ok.js"}}})
	full := filepath.Join(previous, "assets", "full.js")
	webappFaultCopies(t, map[string][2]string{full: {"ENOSPC", "partial"}})
	if kept := frontendKeepPreviousGeneration(previous, next); !slices.Equal(kept, []string{"assets/ok.js"}) {
		t.Fatalf("kept %v", kept)
	}
	lines := strings.Split(strings.TrimSuffix(warn.String(), "\n"), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], full) || !strings.Contains(lines[0], "ENOSPC") {
		t.Fatalf("warnings %q", warn.String())
	}
	if _, err := os.Stat(filepath.Join(next, "assets", "full.js")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the failed copy's partial file was published")
	}
	if data, _ := os.ReadFile(filepath.Join(next, "assets", "ok.js")); string(data) != "old ok" {
		t.Fatalf("ok.js %q", data)
	}
}

func TestAMissingPreviousBuildRejectedEntriesAndANewFileStandingInTheWayStayQuiet(t *testing.T) {
	root := t.TempDir()
	warn := webappWarnings(t)
	if kept := frontendKeepPreviousGeneration(filepath.Join(root, "never-built"), filepath.Join(root, "next")); len(kept) != 0 {
		t.Fatalf("kept %v", kept)
	}
	previous, next := webappGenerations(t, root, map[string]string{"index.html": "old", "assets/raced.js": "old raced", "x/kept.js": "old"},
		map[string]string{"index.html": "new", "x": "new file"},
		map[string]any{"a": map[string]any{"file": "assets/raced.js", "css": []string{"../escape.css", "/abs.css"},
			"assets": []string{"assets/gone.js", "bad\x00name.js", "x/kept.js", "index.html"}}})
	webappFaultCopies(t, map[string][2]string{filepath.Join(previous, "assets", "raced.js"): {"EEXIST", "new raced"}})
	if kept := frontendKeepPreviousGeneration(previous, next); len(kept) != 0 {
		t.Fatalf("kept %v", kept)
	}
	if warn.Len() != 0 {
		t.Fatalf("warnings %q", warn.String())
	}
	if data, _ := os.ReadFile(filepath.Join(next, "assets", "raced.js")); string(data) != "new raced" {
		t.Fatalf("raced %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(next, "x")); string(data) != "new file" {
		t.Fatalf("x %q", data)
	}
}
