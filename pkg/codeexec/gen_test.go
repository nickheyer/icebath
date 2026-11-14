package codeexec

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

var fileRPCCalls = []struct {
	name string
	args map[string]any
}{
	{"web_search", map[string]any{"query": "fixture", "limit": 3}},
	{"web_extract", map[string]any{"urls": []any{"https://example.test"}, "char_limit": 3000}},
	{"read_file", map[string]any{"path": "reference", "offset": 2, "limit": 4}},
	{"write_file", map[string]any{"path": "output", "content": "café", "cross_profile": false}},
	{"search_files", map[string]any{"pattern": "x", "target": "files", "path": ".", "file_glob": "*.py",
		"limit": 4, "offset": 2, "output_mode": "count", "context": 3, "order": "modified"}},
	{"patch", map[string]any{"path": "output", "old_string": "old", "new_string": "new", "replace_all": true,
		"mode": "replace", "patch": nil, "cross_profile": false}},
	{"terminal", map[string]any{"command": "echo fixture", "timeout": 3, "workdir": "/tmp"}},
}

const fileRPCClient = `
import concurrent.futures, json, sys
calls = json.loads(sys.argv[2])
ns = {}
exec(open(sys.argv[1], encoding="utf-8").read(), ns)
for name, args in calls:
    got = ns[name](**args)
    assert got == {"name": name, "args": args}, (name, got)
with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
    results = list(pool.map(lambda i: ns["terminal"](f"tag-{i}", 3, "/tmp"), range(8)))
assert [r["args"]["command"] for r in results] == [f"tag-{i}" for i in range(8)], results
assert "not available" in ns["_call"]("unauthorized-tool", {})["error"]
assert "limit reached" in ns["terminal"]("over-budget")["error"]
print("OK")
`

func TestGeneratedFileRPCKwargsCorrelationAndAuthority(t *testing.T) {
	python := hostRuntime(t).Python
	env := testEnv(t)
	tmp := t.TempDir()
	rpc := filepath.Join(tmp, "rpc with spaces")
	if err := os.MkdirAll(rpc, 0o700); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(tmp, "empty_tools.py")
	os.WriteFile(empty, []byte(generateToolsModule(nil, "file")), 0o600)
	probe := exec.Command(python, "-c", "import json,sys; ns={}; exec(open(sys.argv[1]).read(), ns); print(json.dumps([ns['_RPC_DIR'], 'terminal' in ns]))", empty)
	probe.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "ICEBATH_RPC_DIR=") }), "TMPDIR="+tmp)
	out, err := probe.Output()
	if err != nil {
		t.Fatal(err)
	}
	var got []any
	json.Unmarshal(out, &got)
	if got[0] != filepath.Join(tmp, "icebath_rpc") || got[1] != false {
		t.Fatalf("empty module %v", got)
	}
	names := make([]string, len(fileRPCCalls))
	pairs := make([][]any, len(fileRPCCalls))
	for i, c := range fileRPCCalls {
		names[i] = c.name
		pairs[i] = []any{c.name, c.args}
		def := tools.Default.Get(c.name)
		if def == nil {
			t.Fatalf("%s is not registered", c.name)
		}
		params, _ := def.Schema["parameters"].(map[string]any)
		props, _ := params["properties"].(map[string]any)
		for key := range props {
			blocked := c.name == "terminal" && slices.Contains(terminalBlockedParams, key)
			if _, ok := c.args[key]; !ok && !blocked {
				t.Fatalf("%s wrapper omits schema key %s", c.name, key)
			}
		}
	}
	module := filepath.Join(tmp, "icebath_tools.py")
	os.WriteFile(module, []byte(generateToolsModule(names, "file")), 0o600)
	var mu sync.Mutex
	var seen []recordedCall
	budget := newRPCBudget(names, len(fileRPCCalls)+8)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		rpcPollLoop(t.Context(), env, newShellEnv(t, rpc), rpc, "owner", budget, stop, "right-token", echoDispatch(&mu, &seen))
		close(done)
	}()
	defer func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("poller never stopped")
		}
	}()
	callsJSON, _ := json.Marshal(pairs)
	client := exec.Command(python, "-c", fileRPCClient, module, string(callsJSON))
	client.Env = append(os.Environ(), "ICEBATH_RPC_DIR="+rpc, "ICEBATH_RPC_TOKEN=right-token")
	out, err = client.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "OK" {
		t.Fatalf("client %v %s", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, c := range fileRPCCalls {
		want, _ := json.Marshal(c.args)
		have, _ := json.Marshal(seen[i].args)
		var a, b map[string]any
		json.Unmarshal(want, &a)
		json.Unmarshal(have, &b)
		if seen[i].name != c.name || !reflect.DeepEqual(a, b) {
			t.Fatalf("call %d %v want %s %v", i, seen[i], c.name, c.args)
		}
	}
	if budget.callCount() != len(fileRPCCalls)+8 || len(seen) != budget.callCount() || len(budget.entries()) != budget.callCount() {
		t.Fatalf("counter %d seen %d log %d", budget.callCount(), len(seen), len(budget.entries()))
	}
}

func TestSandboxFailureHints(t *testing.T) {
	cases := []struct{ stderr, want string }{
		{"", ""},
		{"ImportError: cannot import name 'json_parse' from 'icebath_tools'", "Import helpers with `from icebath_tools import json_parse`."},
		{"ImportError: cannot import name 'browser' from 'icebath_tools'", "'browser' is not available inside the execute_code sandbox. Importable tools here: read_file, terminal."},
		{"NameError: name 'retry' is not defined", "Import retry before calling it: from icebath_tools import retry"},
		{"ModuleNotFoundError: No module named 'pandas.io'", "'pandas.io' is not installed in the sandbox interpreter."},
		{"AttributeError: 'str' object has no attribute 'get'", "Tool functions in the sandbox return DICTS"},
		{strings.Repeat("x", 4000) + "NameError: name 'retry' is not defined", ""},
	}
	for _, c := range cases {
		got := sandboxFailureHint(c.stderr, []string{"terminal", "read_file", "browser"})
		if c.want == "" && got != "" || !strings.HasPrefix(got, c.want) {
			t.Fatalf("%q -> %q", c.stderr, got)
		}
	}
}

func TestUnavailableToolImportListsAvailable(t *testing.T) {
	err := "Traceback (most recent call last):\n  File \"script.py\", line 1\n" +
		"ImportError: cannot import name 'browser_navigate' from 'icebath_tools'"
	h := sandboxFailureHint(err, []string{"terminal", "read_file"})
	if !strings.Contains(h, "browser_navigate") || !strings.Contains(h, "read_file") || !strings.Contains(h, "terminal") {
		t.Fatalf("hint %q", h)
	}
}

func TestUnknownAndEmptyFailureNoHint(t *testing.T) {
	if sandboxFailureHint("ZeroDivisionError: division by zero", nil) != "" || sandboxFailureHint("", nil) != "" {
		t.Fatal("hint for an unknown or empty failure")
	}
}
