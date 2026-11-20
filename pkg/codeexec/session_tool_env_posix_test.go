//go:build linux || darwin

package codeexec

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

func TestExecuteCodeRunsEachSessionsOwnInterpreter(t *testing.T) {
	env := kernelEnv(t, map[string]any{"mode": "project"})
	rt := hostRuntime(t)
	base := t.TempDir()
	roots := map[string]string{}
	for _, name := range []string{"alpha", "beta", "daemon"} {
		roots[name] = filepath.Join(base, name)
		if out, err := exec.Command(rt.Python, "-m", "venv", "--without-pip", "--copies", roots[name]).CombinedOutput(); err != nil {
			t.Fatalf("venv %s: %v %s", name, err, out)
		}
	}
	t.Setenv("VIRTUAL_ENV", roots["daemon"])
	probe := "import json, sys; print(json.dumps([sys.executable, sys.prefix]))"
	child := func(client map[string]string) (string, string) {
		t.Helper()
		bindSessionClient(t, client, true)
		result := decodeResult(t, executeCode(t.Context(), env, probe, "child-env-test", []string{"read_file"}, true))
		wantStatus(t, result, "success")
		var pair []string
		if err := json.Unmarshal([]byte(outputOf(result)), &pair); err != nil || len(pair) != 2 {
			t.Fatalf("probe output %v err %v", result, err)
		}
		return pair[0], pair[1]
	}
	resolved := func(p string) string {
		r, err := pypath.Realpath(p, pypath.Lax)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, name := range []string{"alpha", "beta"} {
		exe, prefix := child(map[string]string{"PATH": filepath.Join(roots[name], "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
			"VIRTUAL_ENV": roots[name]})
		if exe != filepath.Join(roots[name], "bin", "python") || resolved(prefix) != resolved(roots[name]) {
			t.Fatalf("%s ran %s with prefix %s", name, exe, prefix)
		}
	}
	if _, prefix := child(map[string]string{"PATH": os.Getenv("PATH")}); resolved(prefix) != resolved(rt.Prefix) {
		t.Fatalf("venvless client ran under prefix %s, want %s", prefix, rt.Prefix)
	}
	if _, prefix := child(nil); resolved(prefix) != resolved(roots["daemon"]) {
		t.Fatalf("unbound session ran under prefix %s", prefix)
	}
}
