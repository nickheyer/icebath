//go:build windows

package cron

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/pm"
)

const processStillActive = 259

// Open and still running per its exit code
func scriptProcessLive(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	var code uint32
	return windows.GetExitCodeProcess(handle, &code) == nil && code == processStillActive
}

func killScriptProcess(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		p.Kill()
	}
}

func writeWindowsFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeWindowsVenv(t *testing.T, venv string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(venv, "Lib", "site-packages"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeWindowsFile(t, filepath.Join(venv, "Scripts", "python.exe"), "")
	return venv
}

func sitePackagesIn(overlay map[string]string) string {
	for _, entry := range strings.Split(overlay["PYTHONPATH"], string(os.PathListSeparator)) {
		if pypath.PathName(entry) == "site-packages" {
			return pypath.PathStr(entry)
		}
	}
	return ""
}

func jsonWindowsText(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestRunJobScriptWindowsUvVenvPythonScriptBypassesLauncher(t *testing.T) {
	s := scriptStore(t)
	script := writeScript(t, s, "probe.py", "print(\"ok\")\n")
	tmp := t.TempDir()
	venv := filepath.Join(tmp, "venv")
	sitePackages := filepath.Join(venv, "Lib", "site-packages")
	base := filepath.Join(tmp, "base")
	if err := os.MkdirAll(sitePackages, 0o700); err != nil {
		t.Fatal(err)
	}
	venvPython := writeWindowsFile(t, filepath.Join(venv, "Scripts", "python.exe"), "")
	basePython := writeWindowsFile(t, filepath.Join(base, "python.exe"), "")
	writeWindowsFile(t, filepath.Join(venv, "pyvenv.cfg"), "home = "+base+"\nuv = true\n")
	prevExe, prevStart := cronSysExecutable, cronStartScript
	t.Cleanup(func() { cronSysExecutable, cronStartScript = prevExe, prevStart })
	cronSysExecutable = func() (string, error) { return venvPython, nil }
	var argv []string
	var env map[string]string
	cronStartScript = func(got []string, dir string, gotEnv map[string]string) (*cronScriptProcess, error) {
		argv, env = got, gotEnv
		return startCronScript([]string{filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"), "/c", "echo ok"}, dir, gotEnv)
	}
	ok, out := runScript(t, s, "probe.py", nil)
	if !ok || out != "ok" {
		t.Fatalf("got %v %q", ok, out)
	}
	if len(argv) != 4 || argv[0] != basePython || argv[1] != "-c" || !strings.Contains(argv[2], "site.addsitedir") {
		t.Fatalf("argv %q", argv)
	}
	m := regexp.MustCompile(`site\.addsitedir\('([^']*)'\)`).FindStringSubmatch(argv[2])
	if m == nil || pypath.PathStr(m[1]) != pypath.PathStr(sitePackages) {
		t.Fatalf("addsitedir %q", argv[2])
	}
	resolved, err := pypath.Resolve(script)
	if err != nil || argv[3] != resolved {
		t.Fatalf("script %q %v", argv[3], err)
	}
	if flags := cronScriptSysProcAttr().CreationFlags; flags != 0x08000000|windows.CREATE_NEW_PROCESS_GROUP {
		t.Fatalf("creation flags %#x", flags)
	}
	if env["VIRTUAL_ENV"] != venv || !strings.Contains(env["PYTHONPATH"], sitePackages) {
		t.Fatalf("env %q %q", env["VIRTUAL_ENV"], env["PYTHONPATH"])
	}
}

func TestManagedInstallUsesTheCommittedGenerationNotTheInTreeVenv(t *testing.T) {
	tmp := t.TempDir()
	ctx := context.Background()
	repo := filepath.Join(tmp, "repo")
	stale := writeWindowsVenv(t, filepath.Join(repo, "venv"))
	t.Cleanup(pm.PatchRepoRoot(repo))
	t.Setenv(pm.EnvInstallRoot, "")
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	committed := writeWindowsVenv(t, filepath.Join(pm.InstallStateDir(ctx, repo), "environments", "gen", "venv"))
	writeWindowsFile(t, filepath.Join(committed, "pyvenv.cfg"), "home = C:\\Python\n")
	storeDir := filepath.Join(tmp, "store")
	store := writeWindowsFile(t, filepath.Join(storeDir, "python.exe"), "")
	if err := os.MkdirAll(filepath.Join(storeDir, "site"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeWindowsFile(t, filepath.Join(storeDir, "pm-runtime.json"), `{"python": "python.exe", "sitePackages": "site"}`)
	writeWindowsFile(t, filepath.Join(repo, "install-stamp.json"), `{"distribution": "docker", "pmRuntime": `+jsonWindowsText(t, storeDir)+`}`)
	writeWindowsFile(t, pm.EnvironmentRuntimeFactsPath(ctx, repo), `{"packages": {"venv": {"environment": `+jsonWindowsText(t, committed)+`}}}`)
	child := writeWindowsVenv(t, filepath.Join(tmp, "child"))
	selected, err := pm.SelectedVenv(ctx, repo)
	if err != nil || selected == stale {
		t.Fatalf("fixture selects %q %v", selected, err)
	}
	interpreter, overlay, err := windowsCronPythonInvocation(ctx, filepath.Join(child, "Scripts", "python.exe"))
	if err != nil {
		t.Fatal(err)
	}
	resolvedStore, err := pypath.Resolve(store)
	if err != nil || interpreter != resolvedStore {
		t.Fatalf("interpreter %q want %q %v", interpreter, resolvedStore, err)
	}
	site := sitePackagesIn(overlay)
	if site == "" {
		t.Fatal("the committed generation must supply a site-packages entry")
	}
	resolvedCommitted, err := pypath.Resolve(committed)
	if err != nil || site != pypath.PathJoin(resolvedCommitted, "Lib", "site-packages") {
		t.Fatalf("site %q %v", site, err)
	}
	if _, under := pypath.PathRelativeTo(site, stale); under {
		t.Fatalf("overlay %q sits under the in tree venv", site)
	}
}
