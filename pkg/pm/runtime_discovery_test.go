package pm

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestRuntimePMRuntimeDiscoversPluginsWithoutApplicationDependencies(t *testing.T) {
	uv, python := peUV(t), pePinnedPython(t)
	tmp := isolateHome(t)
	homeDir := home.Dir(context.Background())
	t.Setenv(EnvRuntimeDir, filepath.Join(homeDir, "tools"))
	peWrite(t, filepath.Join(homeDir, "config.yaml"), "plugins:\n  enabled: []\n")
	ctx := context.Background()
	runtimeRoot := filepath.Join(tmp, "runtime")
	opts := RuntimeOptions{Project: filepath.Join(peReferences, "pm"), Imports: PMRuntimeImports}
	prepared, err := PrepareRuntime(ctx, uv, python, runtimeRoot, opts)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := ReadHomeSelection(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	if plugins, _ := selection["plugins"].(map[string]any); plugins == nil || len(svList(plugins["enabled"])) != 0 {
		t.Fatalf("selection %v", selection)
	}
	if dirs, err := EnabledMemberDirs(ctx, MemberSelection{}); err != nil || len(dirs) != 0 {
		t.Fatalf("member dirs %v %v", dirs, err)
	}
	env := map[string]string{}
	for k, v := range pyEnviron() {
		if !strings.HasPrefix(k, "PYTHON") && !strings.HasPrefix(k, "UV_") {
			env[k] = v
		}
	}
	env[home.EnvHome], env[EnvRuntimeDir] = homeDir, filepath.Join(homeDir, "tools")
	code := "import importlib.util, json, sys\n" +
		"assert importlib.util.find_spec('openai') is None\n" +
		"assert importlib.util.find_spec('yaml') is None\n" +
		"print(json.dumps({'prefix': sys.prefix, 'yaml': importlib.util.find_spec('ruamel.yaml').origin}))\n"
	check := func(interpreter string) map[string]string {
		t.Helper()
		runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(runCtx, interpreter, "-I", "-B", "-c", code)
		cmd.Env = EnvList(env)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v %s", err, out)
		}
		var report map[string]string
		if err := json.Unmarshal(out, &report); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		return report
	}
	report := check(prepared)
	if rel, err := filepath.Rel(report["prefix"], report["yaml"]); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("ruamel.yaml outside the runtime %v", report)
	}
	opts.Offline = true
	if again, err := PrepareRuntime(ctx, uv, python, runtimeRoot, opts); err != nil || again != prepared {
		t.Fatalf("offline reuse %s %v", again, err)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(report["yaml"]), "main.py")); err != nil {
		t.Fatal(err)
	}
	repaired, err := PrepareRuntime(ctx, uv, python, runtimeRoot, opts)
	if err != nil || repaired == prepared {
		t.Fatalf("repair %s %v", repaired, err)
	}
	check(repaired)
}
