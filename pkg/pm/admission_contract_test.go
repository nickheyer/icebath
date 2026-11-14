package pm_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
)

func acAdmit(home string, enabled ...string) error {
	set := map[string]bool{}
	for _, name := range enabled {
		set[name] = true
	}
	return plugins.AdmitPluginSetChange(context.Background(), set, map[string]bool{},
		plugins.AdmitOptions{ActivePluginsDir: filepath.Join(home, "plugins")})
}

func acEnabled(t *testing.T, home string) []string {
	t.Helper()
	cfg := pm.SvConfig(t, filepath.Join(home, "config.yaml"))
	section, _ := cfg["plugins"].(map[string]any)
	out := []string{}
	list, _ := section["enabled"].([]any)
	for _, v := range list {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

func acRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func acTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	if err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err == nil && p != root {
			out = append(out, p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func acPython(t *testing.T, python string, args ...string) string {
	t.Helper()
	out, err := exec.Command(python, args...).Output()
	if err != nil {
		t.Fatalf("%s %v: %v", python, args, err)
	}
	return strings.TrimSpace(string(out))
}

func TestConflictingCandidateRefusedUnenabledAndUnimported(t *testing.T) {
	tmp, home := pm.SvAdmissionEnv(t)
	core := filepath.Join(tmp, "core")
	plugA, plugB := pm.SvConflictMembers(t, home)
	pm.SvWriteEnabled(t, home, []string{}, "plug-a")
	if err := acAdmit(home); err != nil {
		t.Fatal(err)
	}
	working := pm.SvSelected(t, core)
	configBefore := acRead(t, filepath.Join(home, "config.yaml"))
	treeBefore := map[string][]string{plugA: acTree(t, plugA), plugB: acTree(t, plugB)}
	marker := filepath.Join(home, "plugins", "mnemosyne-wrapper", "mnemosyne-wrapper.json")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("{\"wrapper\":true}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(tmp, "external-sidecar")
	if out, err := exec.Command(pm.PeUV(t), "venv", "--python", pm.PeHostPython(t), sidecar).CombinedOutput(); err != nil {
		t.Fatalf("uv venv: %v %s", err, out)
	}
	sidecarPython := filepath.Join(sidecar, "bin", "python")
	if runtime.GOOS == "windows" {
		sidecarPython = filepath.Join(sidecar, "Scripts", "python.exe")
	}
	before := acRead(t, marker)

	err := acAdmit(home, "plug-a", "plug-b")
	var refused *plugins.AdmissionRefused
	if !errors.As(err, &refused) {
		t.Fatalf("want AdmissionRefused, got %v", err)
	}
	message := err.Error()
	if !strings.Contains(message, "plug-b") && !strings.Contains(message, "plug-a") {
		t.Fatalf("refusal must carry the plugin identity, got: %s", message)
	}
	if lower := strings.ToLower(message); !strings.Contains(lower, "sharedlib") && !strings.Contains(lower, "conflict") {
		t.Fatalf("refusal must carry the resolver's reason, got: %s", message)
	}
	if got := acEnabled(t, home); len(got) != 0 {
		t.Fatalf("a refused candidate must stay unenabled, enabled %v", got)
	}
	if string(acRead(t, filepath.Join(home, "config.yaml"))) != string(configBefore) {
		t.Fatal("config changed by a refused admission")
	}
	if memory, _ := pm.SvConfig(t, filepath.Join(home, "config.yaml"))["memory"].(map[string]any); memory["provider"] != "plug-a" {
		t.Fatalf("memory section %v", memory)
	}
	if pm.SvSelected(t, core) != working {
		t.Fatal("failed resolution moved the selected environment")
	}
	for plug, listing := range treeBefore {
		if !reflect.DeepEqual(acTree(t, plug), listing) {
			t.Fatalf("failed resolution mutated plugin tree %s", plug)
		}
	}
	raw, err := pm.LatestReceiptBytes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(raw, &receipt); err != nil || receipt["outcome"] != "failed" {
		t.Fatalf("receipt %s %v", raw, err)
	}
	if !strings.Contains(string(raw), "plug-b") && !strings.Contains(string(raw), "sharedlib") {
		t.Fatalf("receipt must carry the conflict identity or reason: %s", raw)
	}

	if err := acAdmit(home, "plug-a"); err != nil {
		t.Fatal(err)
	}
	if got := acEnabled(t, home); !reflect.DeepEqual(got, []string{"plug-a"}) {
		t.Fatalf("enabled after retry %v", got)
	}
	if string(acRead(t, marker)) != string(before) {
		t.Fatal("wrapper marker changed")
	}
	if prefix := acPython(t, sidecarPython, "-c", "import sys; print(sys.prefix)"); prefix != sidecar {
		t.Fatalf("sidecar prefix %s", prefix)
	}
	selected := pm.SvSelected(t, core)
	if fi, err := os.Stat(selected); err != nil || !fi.IsDir() || selected == sidecar {
		t.Fatalf("selected %s %v", selected, err)
	}
	project := filepath.Join(plugA, "pyproject.toml")
	text := strings.Replace(string(acRead(t, project)), "sharedlib==1.0.0", "sharedlib>=1,<2", 1)
	if err := os.WriteFile(project, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := acAdmit(home, "plug-a"); err != nil {
		t.Fatal(err)
	}
	if pm.SvSelected(t, core) == selected {
		t.Fatal("a declared range did not rebuild the managed environment")
	}
	if string(acRead(t, marker)) != string(before) {
		t.Fatal("wrapper marker changed by the rebuild")
	}
	acPython(t, sidecarPython, "-c", "import sys; assert sys.prefix != sys.base_prefix")
}

func TestPluginWhosePyprojectInstallsNothingKeepsTheEnvironment(t *testing.T) {
	tmp, home := pm.SvAdmissionEnv(t)
	core := filepath.Join(tmp, "core")
	if err := pm.SyncVenvInProcess(context.Background(), pm.SyncVenvOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	selected := pm.SvSelected(t, core)
	recorded := pm.SvVenvFact(t)
	plugin := filepath.Join(home, "plugins", "importers")
	for name, body := range map[string]string{"plugin.yaml": "name: importers\n", "__init__.py": "VERSION = 1\n",
		"pyproject.toml": "[project]\nname=\"importers\"\nversion=\"1\"\nrequires-python=\">=3.11\"\ndependencies=[]\n"} {
		if err := os.MkdirAll(plugin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(plugin, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := acAdmit(home, "importers"); err != nil {
		t.Fatal(err)
	}
	if got := acEnabled(t, home); !reflect.DeepEqual(got, []string{"importers"}) {
		t.Fatalf("enabled %v", got)
	}
	if pm.SvSelected(t, core) != selected || !reflect.DeepEqual(pm.SvVenvFact(t), recorded) {
		t.Fatal("a plugin that installs nothing moved the environment")
	}
	if err := os.WriteFile(filepath.Join(plugin, "__init__.py"), []byte("VERSION = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !pm.SvCurrent(t, core) {
		t.Fatal("a file change of a plugin that installs nothing made the venv stale")
	}
	real := filepath.Join(home, "plugins", "real")
	pm.SvInstallingMember(t, real, "real", filepath.Join(tmp, "wheels"), "")
	if err := os.WriteFile(filepath.Join(real, "plugin.yaml"), []byte("name: real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := acAdmit(home, "importers", "real"); err != nil {
		t.Fatal(err)
	}
	lock, _ := pm.SvVenvFact(t)["resolved_lock"].(string)
	var doc struct {
		Tool struct {
			UV struct {
				Workspace struct {
					Members []string `toml:"members"`
				} `toml:"workspace"`
			} `toml:"uv"`
		} `toml:"tool"`
	}
	if err := toml.Unmarshal(acRead(t, filepath.Join(filepath.Dir(lock), "pyproject.toml")), &doc); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, m := range doc.Tool.UV.Workspace.Members {
		names = append(names, strings.Split(filepath.Base(m), "-")[0])
	}
	if !reflect.DeepEqual(names, []string{"real"}) || pm.SvSelected(t, core) == selected || !pm.SvCurrent(t, core) {
		t.Fatalf("workspace members %v", names)
	}
}

func TestOnlyEnablingAPluginThatRequiresANewerIcebathIsRefused(t *testing.T) {
	for _, enabledBefore := range [][]string{{}, {"needs-newer"}} {
		t.Run(map[bool]string{true: "enabling-it", false: "enabled-before"}[len(enabledBefore) == 0], func(t *testing.T) {
			_, home := pm.SvAdmissionEnv(t)
			pm.WsHost(t, "1.0.0")
			for name, declaration := range map[string]string{"needs-newer": "pip_dependencies: ['icebath-agent>=99']\n", "other": ""} {
				dir := filepath.Join(home, "plugins", name)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte("name: "+name+"\n"+declaration), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			pm.SvWriteEnabled(t, home, enabledBefore, "")
			before := acRead(t, filepath.Join(home, "config.yaml"))
			if len(enabledBefore) == 0 {
				err := acAdmit(home, "needs-newer")
				var refused *plugins.AdmissionRefused
				if !errors.As(err, &refused) || !regexp.MustCompile(`needs-newer.*requires icebath-agent>=99, running 1\.0\.0`).MatchString(err.Error()) {
					t.Fatalf("want the newer icebath refusal, got %v", err)
				}
				if string(acRead(t, filepath.Join(home, "config.yaml"))) != string(before) {
					t.Fatal("config changed by a refused admission")
				}
				return
			}
			if err := acAdmit(home, "needs-newer", "other"); err != nil {
				t.Fatal(err)
			}
			if got := acEnabled(t, home); !reflect.DeepEqual(got, []string{"needs-newer", "other"}) {
				t.Fatalf("enabled %v", got)
			}
		})
	}
}
