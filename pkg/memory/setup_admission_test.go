package memory

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// User cache directory read before any test moves HOME
var admissionCacheRoot, _ = os.UserCacheDir()

var (
	admissionUVOnce sync.Once
	admissionUVPath string
	admissionUVErr  error
)

// Real uv from PATH, else the lockfile's pinned uv unpacked once into the user cache the pm suite shares
func admissionUV(t *testing.T) string {
	t.Helper()
	admissionUVOnce.Do(func() { admissionUVPath, admissionUVErr = admissionFetchUV() })
	if admissionUVErr != nil {
		t.Fatalf("admission tests need real uv: %v", admissionUVErr)
	}
	return admissionUVPath
}

// Downloads, verifies and unpacks the pinned uv for this host the way the pm suite fetches it
func admissionFetchUV() (string, error) {
	if found, err := exec.LookPath("uv"); err == nil {
		return found, nil
	}
	name := "uv"
	if runtime.GOOS == "windows" {
		name = "uv.exe"
	}
	target, err := pm.CurrentTarget()
	if err != nil {
		return "", err
	}
	artifacts := pm.NewLockfile(pm.LockfilePath()).Artifacts("uv", target)
	if len(artifacts) == 0 || admissionCacheRoot == "" {
		return "", &fs.PathError{Op: "pin", Path: "uv " + target, Err: fs.ErrNotExist}
	}
	url, sha := pyrepr.Str(artifacts[0]["url"]), pyrepr.Str(artifacts[0]["sha256"])
	root := filepath.Join(admissionCacheRoot, "icebath-pm-tests", "uv-"+sha[:16])
	find := func() string {
		found := ""
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && d.Name() == name && found == "" {
				found = path
			}
			return nil
		})
		return found
	}
	if binary := find(); binary != "" {
		return binary, nil
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != sha {
		return "", &fs.PathError{Op: "sha256", Path: url, Err: fs.ErrInvalid}
	}
	staging := root + ".partial"
	os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o777); err != nil {
		return "", err
	}
	archive := filepath.Join(staging, filepath.Base(url))
	if err := os.WriteFile(archive, data, 0o666); err != nil {
		return "", err
	}
	if err := pm.Extract(archive, filepath.Join(staging, "tree")); err != nil {
		return "", err
	}
	os.Remove(archive)
	if err := os.Rename(staging, root); err != nil {
		return "", err
	}
	if binary := find(); binary != "" {
		return binary, nil
	}
	return "", &fs.PathError{Op: "find", Path: root, Err: fs.ErrNotExist}
}

// Pure Python wheel whose bytes depend only on its name and version
func admissionWheel(t *testing.T, dir, name, version string) {
	t.Helper()
	metadata := name + "-" + version + ".dist-info"
	paths := []string{name + "/__init__.py", metadata + "/METADATA", metadata + "/WHEEL"}
	entries := map[string]string{
		paths[0]: "__version__ = " + pyrepr.StrRepr(version) + "\n",
		paths[1]: "Metadata-Version: 2.1\nName: " + name + "\nVersion: " + version + "\n",
		paths[2]: "Wheel-Version: 1.0\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
	}
	var record strings.Builder
	for _, p := range paths {
		record.WriteString(p + ",,\n")
	}
	paths = append(paths, metadata+"/RECORD")
	entries[metadata+"/RECORD"] = record.String()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range paths {
		header := &zip.FileHeader{Name: p, Method: zip.Deflate, Modified: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)}
		header.SetMode(0o644)
		w, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, entries[p])
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	admissionWrite(t, filepath.Join(dir, name+"-"+version+"-py3-none-any.whl"), buf.String())
}

func admissionWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func admissionRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Isolated home and a uv locked core project over local wheels that real PM admissions run against
func admissionWorld(t *testing.T, wheels ...string) (tmp, core, h string) {
	t.Helper()
	uv := admissionUV(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("admission tests need a host Python")
	}
	tmp = t.TempDir()
	core, h = filepath.Join(tmp, "core"), filepath.Join(tmp, "home")
	wheelDir := filepath.Join(tmp, "wheels")
	for _, name := range wheels {
		admissionWheel(t, wheelDir, name, "1.0")
	}
	links, _ := json.Marshal(filepath.ToSlash(wheelDir))
	admissionWrite(t, filepath.Join(core, "pyproject.toml"), "[project]\nname=\"core\"\nversion=\"1\"\nrequires-python=\">=3.14\"\n"+
		"[tool.uv]\npackage=false\nno-index=true\nfind-links=["+string(links)+"]\n")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ICEBATH_MANAGED_MODE", pm.EnvInstallRoot, "VIRTUAL_ENV", pm.EnvDisableLazyInstalls, "PYTHONPATH"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	for key, value := range map[string]string{"HOME": tmp, "USERPROFILE": tmp, home.EnvHome: h, pm.EnvRuntimeDir: filepath.Join(tmp, "tools"),
		"UV_CACHE_DIR": filepath.Join(tmp, "cache"), "XDG_CONFIG_HOME": filepath.Join(tmp, "config"), "XDG_CONFIG_DIRS": filepath.Join(tmp, "config"),
		"UV_OFFLINE": "1", "UV_PYTHON_DOWNLOADS": "never"} {
		t.Setenv(key, value)
	}
	t.Cleanup(home.PatchUserHome(tmp))
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	lock := exec.Command(uv, "lock", "--python", python)
	lock.Dir = core
	if out, err := lock.CombinedOutput(); err != nil {
		t.Fatalf("uv lock: %v\n%s", err, out)
	}
	t.Cleanup(pm.OverrideCoreRepo(core))
	t.Cleanup(pm.OverrideUVToolchain(uv, python))
	return tmp, core, h
}

// Runs the selected environment's interpreter isolated on an import line and returns its stripped output
func admissionImports(t *testing.T, core, cwd, code string) string {
	t.Helper()
	venv, err := pm.SelectedVenv(context.Background(), core)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(pm.VenvPython(venv), "-I", "-c", code)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", code, err, out)
	}
	return strings.TrimSpace(string(out))
}

func admissionSelected(t *testing.T, core string) string {
	t.Helper()
	venv, err := pm.SelectedVenv(context.Background(), core)
	if err != nil {
		t.Fatal(err)
	}
	return venv
}

type postSetupRecorder struct {
	regProvider
	t     *testing.T
	home  string
	calls []map[string]any
}

func (p *postSetupRecorder) PostSetup(_ context.Context, h string, cfg map[string]any) error {
	if h != p.home {
		p.t.Errorf("post setup ran for %s, want %s", h, p.home)
	}
	p.calls = append(p.calls, cfg)
	return nil
}

func TestSetupRequiresDependenciesAndKeepsTheExistingUnion(t *testing.T) {
	for _, picker := range []bool{false, true} {
		t.Run(map[bool]string{false: "named", true: "picker"}[picker], func(t *testing.T) { admissionSetupCase(t, picker) })
	}
}

func admissionSetupCase(t *testing.T, picker bool) {
	ctx := context.Background()
	tmp, core, h := admissionWorld(t, "existing_dep", "provider_dep", "modern_dep", "other_dep")
	plugins := filepath.Join(h, "plugins")
	incumbent, candidate := filepath.Join(plugins, "incumbent"), filepath.Join(plugins, "candidate")
	for _, dir := range []string{incumbent, candidate} {
		admissionWrite(t, filepath.Join(dir, "__init__.py"), "# MemoryProvider fixture\n")
	}
	admissionWrite(t, filepath.Join(incumbent, "plugin.yaml"), "name: incumbent\npython_dependencies: [\"existing_dep==1.0\"]\n")
	candidateManifest := filepath.Join(candidate, "plugin.yaml")
	admissionWrite(t, candidateManifest, "name: candidate\npip_dependencies: [\"provider_dep==1.0\", \"provider_dep==2.0\"]\n")
	cfgPath := filepath.Join(h, "config.yaml")
	admissionWrite(t, cfgPath, "memory:\n  provider: incumbent\n  setting: keep\nplugins:\n  enabled:\n  - incumbent\n")
	otherHome := filepath.Join(h, "profiles", "other")
	admissionWrite(t, filepath.Join(otherHome, "plugins", "other-plugin", "plugin.yaml"), "name: other-plugin\npython_dependencies: [\"other_dep==1.0\"]\n")
	otherConfig := filepath.Join(otherHome, "config.yaml")
	admissionWrite(t, otherConfig, "plugins:\n  enabled: [other-plugin]\n")
	if err := pm.SyncVenv(ctx, pm.SyncVenvOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	original := admissionSelected(t, core)
	files := []string{cfgPath, otherConfig, pm.RuntimeFactsPath(ctx)}
	before := map[string]string{}
	for _, f := range files {
		before[f] = admissionRead(t, f)
	}
	unchanged := func(stage string) {
		t.Helper()
		for _, f := range files {
			if admissionRead(t, f) != before[f] {
				t.Fatalf("%s changed %s", stage, f)
			}
		}
	}
	recorder := &postSetupRecorder{t: t, home: h}
	out := &bytes.Buffer{}
	m := NewMemorySetup(&tools.Env{Home: h, Values: &sync.Map{}}, &cliui.Console{In: strings.NewReader(""), Out: out}, out)
	offered := []setupProvider{{"candidate", "local", recorder}}
	m.availableProviders = func(context.Context) []setupProvider { return offered }
	m.cursesSelect = func(context.Context, string, [][2]string, int, int) (int, error) { return 0, nil }
	setup := func() int {
		t.Helper()
		name := "candidate"
		if picker {
			name = ""
		}
		code, err := m.Run(ctx, "setup", name)
		if err != nil {
			t.Fatalf("setup: %v\n%s", err, out.String())
		}
		return code
	}
	if code := setup(); code != 1 || len(recorder.calls) != 0 {
		t.Fatalf("conflicting candidate: exit %d post setup %d\n%s", code, len(recorder.calls), out.String())
	}
	unchanged("refused candidate")
	if admissionSelected(t, core) != original {
		t.Fatal("refused candidate selected a new environment")
	}
	for _, malformed := range []string{"name: [broken", "name: candidate\npython_dependencies: 9\n"} {
		admissionWrite(t, candidateManifest, malformed)
		if code := setup(); code != 1 || len(recorder.calls) != 0 {
			t.Fatalf("%q: exit %d\n%s", malformed, code, out.String())
		}
		unchanged("malformed manifest")
	}
	admissionWrite(t, candidateManifest, "name: candidate\npip_dependencies: [\"provider_dep==1.0\"]\n")
	offered = []setupProvider{{"candidate", "local", &regProvider{name: "candidate"}}}
	if code := setup(); code != 0 {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	var saved map[string]any
	if err := yaml.Unmarshal([]byte(admissionRead(t, cfgPath)), &saved); err != nil {
		t.Fatal(err)
	}
	if mem, _ := saved["memory"].(map[string]any); mem["provider"] != "candidate" {
		t.Fatalf("memory block %v", saved["memory"])
	}
	if got := admissionImports(t, core, tmp, `import existing_dep, provider_dep, other_dep; print("both")`); got != "both" {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(candidate, "pyproject.toml")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("candidate pyproject written: %v", err)
	}
	if admissionRead(t, otherConfig) != before[otherConfig] {
		t.Fatal("other profile config changed")
	}
	modern := filepath.Join(plugins, "modern")
	admissionWrite(t, filepath.Join(modern, "__init__.py"), "# MemoryProvider fixture\n")
	admissionWrite(t, filepath.Join(modern, "plugin.yaml"), "name: modern\n")
	admissionWrite(t, filepath.Join(modern, "pyproject.toml"), "[project]\nname=\"modern-provider\"\nversion=\"1\"\nrequires-python=\">=3.14\"\n"+
		"dependencies=[\"modern_dep==1.0\"]\n[tool.uv]\npackage=false\n")
	offered = []setupProvider{{"modern", "local", &regProvider{name: "modern"}}}
	if err := m.CmdSetupProvider(ctx, "modern"); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := admissionImports(t, core, tmp, `import existing_dep, provider_dep, modern_dep, other_dep; print("all")`); got != "all" {
		t.Fatal(got)
	}
	if admissionRead(t, otherConfig) != before[otherConfig] {
		t.Fatal("other profile config changed")
	}
}

// Every path under a tree, the listing a passive readiness probe must leave alone
func admissionTree(t *testing.T, root string) string {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		paths = append(paths, path)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return strings.Join(paths, "\n")
}

func TestSetupAdmitsRealProviderUnionAndKeepsSelectionOnFailure(t *testing.T) {
	for _, declaration := range []string{"pyproject", "python_dependencies", "pip_dependencies"} {
		t.Run(declaration, func(t *testing.T) { admissionCLICase(t, declaration) })
	}
}

func admissionCLICase(t *testing.T, declaration string) {
	ctx := context.Background()
	tmp, core, h := admissionWorld(t, "existing_dep", "provider_dep")
	incumbent, candidate := filepath.Join(h, "plugins", "incumbent"), filepath.Join(h, "plugins", "candidate")
	admissionWrite(t, filepath.Join(incumbent, "plugin.yaml"), "name: incumbent\npython_dependencies: [\"existing_dep==1.0\"]\n")
	declare := func(requirements ...string) {
		listed, _ := json.Marshal(requirements)
		if declaration == "pyproject" {
			admissionWrite(t, filepath.Join(candidate, "plugin.yaml"), "name: candidate\n")
			admissionWrite(t, filepath.Join(candidate, "pyproject.toml"), "[project]\nname=\"candidate\"\nversion=\"1\"\nrequires-python=\">=3.14\"\n"+
				"dependencies="+strings.ReplaceAll(string(listed), ",", ", ")+"\n[tool.uv]\npackage=false\n")
			return
		}
		admissionWrite(t, filepath.Join(candidate, "plugin.yaml"), "name: candidate\n"+declaration+": "+strings.ReplaceAll(string(listed), ",", ", ")+"\n")
	}
	declare("provider_dep==1.0", "provider_dep==2.0")
	cfgPath := filepath.Join(h, "config.yaml")
	admissionWrite(t, cfgPath, "plugins:\n  enabled: [incumbent]\n")
	previousFind := setupFindProviderDir
	setupFindProviderDir = func(context.Context, *tools.Env, string) string { return candidate }
	t.Cleanup(func() { setupFindProviderDir = previousFind })
	admissionWrite(t, filepath.Join(tmp, "provider_dep.py"), "VALUE=\"ambient, not admitted\"\n")
	t.Setenv("PYTHONPATH", tmp)
	if ambient, err := exec.Command("python3", "-c", "import provider_dep; print(provider_dep.VALUE)").Output(); err != nil ||
		strings.TrimSpace(string(ambient)) != "ambient, not admitted" {
		t.Fatalf("ambient module not importable: %v %s", err, ambient)
	}
	env := &tools.Env{Home: h, Values: &sync.Map{}}
	out := &bytes.Buffer{}
	m := NewMemorySetup(env, &cliui.Console{In: strings.NewReader(""), Out: out}, out)
	prepare := func() error { return m.installDeps(ctx, "candidate") }
	if err := pm.LockProject(ctx, pm.LockProjectOptions{Source: core, Explicit: true, Offline: true}); err != nil {
		t.Fatal(err)
	}
	if err := pm.SyncVenv(ctx, pm.SyncVenvOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	original := admissionSelected(t, core)
	originalConfig := admissionRead(t, cfgPath)
	current := func() bool {
		t.Helper()
		_, opts, err := MemoryProviderDependencyInputs(ctx, env, "candidate")
		if err != nil || opts == nil {
			t.Fatalf("candidate declaration not read: %v", err)
		}
		ok, err := pm.VenvIsCurrent(ctx, pm.VenvIsCurrentOptions{Extras: opts.Extras, Plugins: opts.Plugins})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	listing := admissionTree(t, h)
	if current() {
		t.Fatal("conflicting candidate reported current")
	}
	if admissionTree(t, h) != listing {
		t.Fatal("listing must not prepare dependencies")
	}
	var installErr *pm.InstallError
	if err := prepare(); !errors.As(err, &installErr) {
		t.Fatalf("conflicting candidate: %v\n%s", err, out.String())
	}
	if admissionSelected(t, core) != original || admissionRead(t, cfgPath) != originalConfig {
		t.Fatal("refused candidate changed the selection or config")
	}
	declare("provider_dep==1.0")
	if err := prepare(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := admissionImports(t, core, tmp, `import existing_dep, provider_dep; print('both')`); got != "both" {
		t.Fatal(got)
	}
	if admissionRead(t, cfgPath) != originalConfig {
		t.Fatal("preparation changed config.yaml")
	}
	selected := admissionSelected(t, core)
	if err := prepare(); err != nil || admissionSelected(t, core) != selected {
		t.Fatalf("a current union produced another generation: %v", err)
	}
	if !current() {
		t.Fatal("prepared candidate not current")
	}
	declare("provider_dep==2.0")
	if current() || admissionSelected(t, core) != selected {
		t.Fatal("redeclared candidate still current or reselected")
	}
}
