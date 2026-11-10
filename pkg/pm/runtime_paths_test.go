package pm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Writes value as JSON text at path
func rpWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, data, 0o666)
}

// Fails unless the store of repo resolves to want
func rpWantStore(t *testing.T, repo, want string) string {
	t.Helper()
	got, err := EnvironmentStoreRoot(context.Background(), repo, true)
	if err != nil || got != want {
		t.Fatalf("store %q %v, want %q", got, err, want)
	}
	return got
}

func TestStoreResolutionFollowsRelocatedPayload(t *testing.T) {
	isolateHome(t)
	tmp := resolvePath(t.TempDir())
	payload := filepath.Join(tmp, "agent-payload")
	repo := sttMkdir(t, filepath.Join(payload, "icebath-agent"))
	rpWriteJSON(t, filepath.Join(repo, "install-stamp.json"), map[string]any{"payload": "bundled", "runtime": map[string]any{
		"repoDir": "icebath-agent", "toolsDir": "tools",
	}})
	rpWriteJSON(t, filepath.Join(payload, "manifest.json"), map[string]any{"schema": 1, "repo": "icebath-agent",
		"venv": "venv", "store": "tools", "runtime": map[string]any{"toolsDir": "tools"}})
	tools := sttMkdir(t, filepath.Join(payload, "tools"))
	rpWriteJSON(t, filepath.Join(tools, "facts.json"), map[string]any{"schema": 1, "packages": map[string]any{"node": map[string]any{"entry": "node-test"}}})
	sttMkdir(t, filepath.Join(tools, "node-test"))

	destination := payload
	for _, dest := range []string{payload, filepath.Join(tmp, "relocated payload")} {
		if dest != payload {
			if err := os.Rename(payload, dest); err != nil {
				t.Fatal(err)
			}
		}
		destination = dest
		repo = filepath.Join(destination, "icebath-agent")
		resolved := rpWantStore(t, repo, filepath.Join(destination, "tools"))
		data, err := os.ReadFile(filepath.Join(resolved, "facts.json"))
		if err != nil {
			t.Fatal(err)
		}
		var installed struct {
			Packages map[string]struct{ Entry string } `json:"packages"`
		}
		if err := json.Unmarshal(data, &installed); err != nil {
			t.Fatal(err)
		}
		if st, err := os.Stat(filepath.Join(resolved, installed.Packages["node"].Entry)); err != nil || !st.IsDir() {
			t.Fatalf("the recorded entry must be a directory of the resolved store, %v", err)
		}
	}

	override := filepath.Join(tmp, "stage-tools")
	t.Setenv(EnvRuntimeDir, override)
	rpWantStore(t, repo, override)
	os.Unsetenv(EnvRuntimeDir)
	rpWriteJSON(t, filepath.Join(repo, "install-stamp.json"), map[string]any{"runtimeDir": override})
	rpWriteJSON(t, filepath.Join(destination, "manifest.json"), map[string]any{"repo": "other-repo"})
	rpWantStore(t, repo, override)
	if err := os.Remove(filepath.Join(repo, "install-stamp.json")); err != nil {
		t.Fatal(err)
	}
	rpWantStore(t, repo, filepath.Join(home.Root(), "tools"))
}

func TestPayloadStoreCannotEscapePayload(t *testing.T) {
	for _, escape := range []string{"relative", "absolute", "symlink"} {
		t.Run(escape, func(t *testing.T) {
			isolateHome(t)
			tmp := resolvePath(t.TempDir())
			payload := filepath.Join(tmp, "agent-payload")
			repo := sttMkdir(t, filepath.Join(payload, "icebath-agent"))
			outside := sttMkdir(t, filepath.Join(tmp, "outside"))
			values := map[string]string{"relative": "../outside", "absolute": outside, "symlink": "tools"}
			if escape == "symlink" {
				if err := os.Symlink(outside, filepath.Join(payload, "tools")); err != nil {
					t.Fatal(err)
				}
			}
			rpWriteJSON(t, filepath.Join(payload, "manifest.json"), map[string]any{"repo": "icebath-agent", "store": values[escape]})
			_, err := EnvironmentStoreRoot(context.Background(), repo, true)
			var pyErr *PyError
			if !errors.As(err, &pyErr) || pyErr.Class != "RuntimeError" || !strings.Contains(pyErr.Message, "payload store escapes its root") {
				t.Fatalf("want the RuntimeError naming the escape, got %v", err)
			}
			t.Setenv(EnvRuntimeDir, outside)
			rpWantStore(t, repo, outside)
		})
	}
}

// Fails unless the venv holds the wanted interpreter version
func rpWantVersion(t *testing.T, venv, major, minor string) {
	t.Helper()
	gotMajor, gotMinor, ok, err := VenvPythonVersion(venv)
	if err != nil || !ok || gotMajor != major || gotMinor != minor {
		t.Fatalf("version (%q, %q) ok=%v err=%v, want (%s, %s)", gotMajor, gotMinor, ok, err, major, minor)
	}
}

func TestSitePackagesFollowsTheVenvPythonNotTheCaller(t *testing.T) {
	venv := filepath.Join(t.TempDir(), "venv")
	tree := sttMkdir(t, filepath.Join(venv, "lib", "python3.9", "site-packages"))
	writeFile(t, filepath.Join(venv, "pyvenv.cfg"), []byte("home = /usr/bin\nversion = 3.9.20\n"), 0o666)

	rpWantVersion(t, venv, "3", "9")
	if runtime.GOOS != "windows" {
		site, err := SitePackages(venv)
		if err != nil || site != tree {
			t.Fatalf("site packages %q %v, want %q", site, err, tree)
		}
		if st, err := os.Stat(site); err != nil || !st.IsDir() {
			t.Fatalf("the composed path must be the real tree of the venv, %v", err)
		}
	}
}

func TestVenvPythonVersionReadsUvVersionInfo(t *testing.T) {
	venv := sttMkdir(t, filepath.Join(t.TempDir(), "venv"))
	writeFile(t, filepath.Join(venv, "pyvenv.cfg"), []byte("home = C:\\Python311\nversion_info = 3.11.9\n"), 0o666)

	rpWantVersion(t, venv, "3", "11")
}

func TestVenvPythonVersionFallsBackToTheLibDirectory(t *testing.T) {
	venv := filepath.Join(t.TempDir(), "venv")
	sttMkdir(t, filepath.Join(venv, "lib", "python3.12", "site-packages"))

	rpWantVersion(t, venv, "3", "12")
}

func TestVenvPythonVersionIsNoneWithoutEvidence(t *testing.T) {
	major, minor, ok, err := VenvPythonVersion(filepath.Join(t.TempDir(), "absent"))
	if err != nil || ok || major != "" || minor != "" {
		t.Fatalf("an absent venv holds no version, got (%q, %q) ok=%v err=%v", major, minor, ok, err)
	}
}
