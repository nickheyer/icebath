package plugins

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Pure python wheel whose bytes depend only on its name and version
func pubWheel(t *testing.T, dir, name, version string) {
	t.Helper()
	info := name + "-" + version + ".dist-info"
	files := [][2]string{{name + "/helper.py", "__version__ = " + pyrepr.StrRepr(version) + "\n"},
		{info + "/METADATA", "Metadata-Version: 2.1\nName: " + name + "\nVersion: " + version + "\n"},
		{info + "/WHEEL", "Wheel-Version: 1.0\nRoot-Is-Purelib: true\nTag: py3-none-any\n"}}
	var record strings.Builder
	for _, f := range files {
		record.WriteString(f[0] + ",,\n")
	}
	files = append(files, [2]string{info + "/RECORD", record.String() + info + "/RECORD,,\n"})
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		header := &zip.FileHeader{Name: f[0], Method: zip.Deflate, Modified: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)}
		header.SetMode(0o644)
		w, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, f[1])
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	vtWrite(t, filepath.Join(dir, name+"-"+version+"-py3-none-any.whl"), buf.String())
}

// Publication world with an offline proof wheel index, a core locking plugin-core-dep and the install service doubles
func pubDepsWorld(t *testing.T) (root, h, core string) {
	t.Helper()
	uv, python := pubUV(t), pubPython(t)
	root, h = pubWorld(t)
	core = filepath.Join(root, "core")
	wheels := filepath.Join(root, "wheels")
	pubWheel(t, wheels, "plugin_core_dep", "1.0")
	pubWheel(t, wheels, "plugin_proof_other", "1.0")
	for _, pin := range []string{"1.0", "2.0"} {
		pubWheel(t, wheels, "plugin_proof_dep", pin)
	}
	vtWrite(t, filepath.Join(core, "pyproject.toml"), "[project]\nname=\"plugin-proof-core\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n"+
		"dependencies=[\"plugin-core-dep==1.0\"]\n[tool.uv]\npackage=false\nno-index=true\nfind-links=[\""+filepath.ToSlash(wheels)+"\"]\n")
	lock := exec.Command(uv, "lock", "--python", python)
	lock.Dir = core
	if out, err := lock.CombinedOutput(); err != nil {
		t.Fatalf("uv lock: %v\n%s", err, out)
	}
	scanSaved := scanOnInstallEnabled
	t.Cleanup(func() { scanOnInstallEnabled = scanSaved })
	scanOnInstallEnabled = func(string) bool { return false }
	txOAuthSurface(t)
	next := *services.Load()
	next.DefinesMemoryProvider = func(string) bool { return false }
	next.RequiresAuthNotice = func(map[string]any) string { return "" }
	next.DeclaresModelProviderKind = func(string) bool { return false }
	next.PluginOAuthKey = func(string) string { return "" }
	next.ForgetPluginGrants = func(string) error { return nil }
	next.OAuthAdmissionProblems = func(map[string]any, string) []string { return nil }
	next.OAuthAdmissionWarnings = func(map[string]any, string) []string { return nil }
	next.GatewayRunningPid = func(string, bool) (int, bool, error) { return 0, false, nil }
	next.InstalledGatewayServiceKind = func(context.Context) (string, bool, error) { return "", false, nil }
	next.RefreshPluginSessions = func(context.Context, string, string, string) error { return nil }
	InstallServices(&next)
	return root, h, core
}

// Git origin of plugin-worker-proof at one pin, importing plugin_proof_dep, and its HEAD sha
func pubOrigin(t *testing.T, root, pin string) (string, string) {
	t.Helper()
	repo := filepath.Join(root, "plugin-worker-proof-origin")
	c6Git(t, root, "init", "-qb", "main", repo)
	vtWrite(t, filepath.Join(repo, "plugin.yaml"), "name: plugin-worker-proof\npython_dependencies:\n- plugin-proof-dep=="+pin+"\nversion: '"+pin+"'\n")
	vtWrite(t, filepath.Join(repo, "helper.py"), "import plugin_proof_dep as plugin_proof_dep\nVERSION = "+pyrepr.StrRepr(pin)+
		"\ndef register(ctx):\n    return (VERSION, plugin_proof_dep.__version__)\n")
	return repo, c6Commit(t, repo, "fixture "+pin)
}

// Runs the installed plugin under the selected venv, proving both proof wheels import from it
func pubImports(t *testing.T, ctx context.Context, root, h, core, pin string) {
	t.Helper()
	selected, err := pm.SelectedVenv(ctx, core)
	if err != nil {
		t.Fatal(err)
	}
	q := func(s string) string { return pyrepr.StrRepr(s) }
	code := "import importlib.util, pathlib; import plugin_core_dep, plugin_proof_dep; " +
		"assert pathlib.Path(plugin_core_dep.__file__).is_relative_to(" + q(selected) + "); " +
		"assert pathlib.Path(plugin_proof_dep.__file__).is_relative_to(" + q(selected) + "); " +
		"spec = importlib.util.spec_from_file_location('loaded_plugin', " + q(filepath.Join(h, "plugins", "plugin-worker-proof", "helper.py")) + "); " +
		"plugin = importlib.util.module_from_spec(spec); spec.loader.exec_module(plugin); " +
		"assert plugin.register(None) == (" + q(pin) + ", " + q(pin) + ")"
	cmd := exec.Command(filepath.Join(selected, "bin", "python"), "-I", "-B", "-c", code)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("imports: %v\n%s", err, out)
	}
}
