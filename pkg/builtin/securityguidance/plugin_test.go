package securityguidance

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
)

var bg = context.Background()

func names(fs []finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.name)
	}
	return out
}

func transform(t *testing.T, tool string, args map[string]any, result string) any {
	t.Helper()
	out, err := onTransformToolResult(bg, map[string]any{"tool_name": tool, "args": args, "result": result})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEveryRuleHasRequiredFieldsAndUniqueNames(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range SecurityPatterns {
		if r.RuleName == "" || r.Reminder == "" || (r.Substrings == nil && r.Regex == "" && r.PathCheck == nil) || seen[r.RuleName] {
			t.Fatalf("rule %+v", r.RuleName)
		}
		seen[r.RuleName] = true
	}
	if len(SecurityPatterns) != 25 || len(compiled) != 25 {
		t.Fatalf("%d rules, %d compiled", len(SecurityPatterns), len(compiled))
	}
}

func TestScanContent(t *testing.T) {
	cases := []struct {
		path, content, rule string
		want                bool
	}{
		{"/tmp/foo.py", "import pickle\nx = pickle.load(open('p.pkl', 'rb'))\n", "pickle_deserialization", true},
		{"/tmp/foo.py", "model.eval()\nout = model(x)\n", "eval_injection", false},
		{"/tmp/foo.py", "result = eval(user_input)\n", "eval_injection", true},
		{"/tmp/foo.py", "subprocess.run('ls ' + path, shell=True)\n", "python_subprocess_shell", true},
		{"/tmp/foo.tsx", "<div dangerouslySetInnerHTML={{__html: x}} />", "react_dangerously_set_html", true},
		{".github/workflows/test.yml", "name: CI\non: pull_request", "github_actions_workflow", true},
		{"/tmp/foo.py", "name: CI", "github_actions_workflow", false},
		{"/tmp/a.js", "const cp = require('child_process'); exec(`ls ${x}`)", "child_process_exec", true},
		{"/tmp/a.py", "obj.exec(x)", "child_process_exec", false},
		{"/tmp/a.md", "eval(x)", "eval_injection", false},
		{"/tmp/a.py", "yaml.load(f, Loader=yaml.SafeLoader)", "unsafe_yaml_load", false},
		{"/tmp/a.py", "yaml.load(f)", "unsafe_yaml_load", true},
		{"/tmp/a.py", "torch.load(p, weights_only=True)", "torch_unsafe_load", false},
		{"/tmp/a.py", "torch.load(p)", "torch_unsafe_load", true},
		{"/tmp/a.html", `<script src="https://cdn.example/x.js"></script>`, "script_src_without_sri", true},
		{"/tmp/a.html", `<script integrity="sha384-x" src="https://cdn.example/x.js"></script>`, "script_src_without_sri", false},
		{"/tmp/a.go", `exec.Command("sh", "-c", cmd)`, "go_exec_shell_injection", true},
		{"/tmp/a.js", `cipher = "aes-128-ecb"`, "aes_ecb_mode", true},
		{"/tmp/a.py", "requests.get(u, verify=False)", "tls_verification_disabled", true},
		{"/tmp/a.py", "np.load(p, allow_pickle=True)", "pickle_wrapper_load", true},
		{"/tmp/a.py", "from os import system", "os_system_injection", true},
	}
	for _, c := range cases {
		if got := slices.Contains(names(scanContent(c.path, c.content)), c.rule); got != c.want {
			t.Fatalf("%s %q %s: %v", c.path, c.content, c.rule, got)
		}
	}
	if scanContent("/tmp/foo.py", "") != nil {
		t.Fatal("empty content")
	}
	if scanContent("/tmp/foo.py", strings.Repeat("x", 1024*1024)+"\npickle.load(open('p.pkl', 'rb'))\n") != nil {
		t.Fatal("huge content scanned")
	}
}

func TestTransformToolResultHook(t *testing.T) {
	out := transform(t, "write_file", map[string]any{"path": "/tmp/foo.py", "content": "import pickle\nx = pickle.loads(b)\n"}, `{"success": true, "bytes_written": 30}`)
	s, _ := out.(string)
	if !strings.Contains(s, "Security guidance") || !strings.Contains(s, "pickle_deserialization") || !strings.HasPrefix(s, `{"success": true`) {
		t.Fatalf("%q", s)
	}
	if !strings.Contains(s, "\n\n\n---\n⚠️ Security guidance — 1 pattern matched (pickle_deserialization)\n\n⚠️ Security Warning: Loading pickle data") ||
		!strings.HasSuffix(s, "\n\nPattern matches can be false positives. If the construct is safe in this context, briefly document why in a code comment and continue. Otherwise, fix the code before moving on.") {
		t.Fatalf("block layout %q", s)
	}
	if transform(t, "write_file", map[string]any{"path": "/tmp/foo.py", "content": "import json\nx = json.loads(b)\n"}, `{"success": true}`) != nil {
		t.Fatal("clean content decorated")
	}
	if s, _ := transform(t, "patch", map[string]any{"path": "/tmp/foo.py", "old_string": "x = 1", "new_string": "x = eval(user_input)"}, `{"success": true}`).(string); !strings.Contains(s, "eval_injection") {
		t.Fatalf("patch %q", s)
	}
	if s, _ := transform(t, "skill_manage", map[string]any{"file_path": "scripts/a.py", "file_content": "eval(x)\npickle.load(f)\n"}, "ok").(string); !strings.Contains(s, "2 patterns matched (eval_injection, pickle_deserialization)") {
		t.Fatalf("skill_manage %q", s)
	}
	if transform(t, "terminal", map[string]any{"command": "echo pickle.load"}, `{"output": "pickle.load"}`) != nil {
		t.Fatal("untargeted tool decorated")
	}
	if transform(t, "write_file", map[string]any{"path": "/tmp/foo.py", "content": "pickle.load(f)\n"}, `{"error": "denied", "path": "x"}`) != nil {
		t.Fatal("error result decorated")
	}
	t.Setenv("SECURITY_GUIDANCE_DISABLE", "1")
	if transform(t, "write_file", map[string]any{"path": "/tmp/foo.py", "content": "pickle.load(f)\n"}, `{"ok": true}`) != nil {
		t.Fatal("kill switch ignored")
	}
}

func TestBlockModeMakesTransformQuietAndPreToolCallRefuses(t *testing.T) {
	t.Setenv("SECURITY_GUIDANCE_BLOCK", "1")
	args := map[string]any{"path": "/tmp/foo.py", "content": "pickle.load(f)\n"}
	if transform(t, "write_file", args, `{"ok": true}`) != nil {
		t.Fatal("transform decorated in block mode")
	}
	out, err := onPreToolCall(bg, map[string]any{"tool_name": "write_file", "args": args})
	m, _ := out.(map[string]any)
	msg, _ := m["message"].(string)
	if err != nil || m["action"] != "block" || !strings.Contains(msg, "pickle_deserialization") || !strings.Contains(msg, "SECURITY_GUIDANCE_BLOCK") {
		t.Fatalf("%v %v", out, err)
	}
	t.Setenv("SECURITY_GUIDANCE_BLOCK", "no")
	if out, _ := onPreToolCall(bg, map[string]any{"tool_name": "write_file", "args": args}); out != nil {
		t.Fatal("blocked outside block mode")
	}
}

func TestManifestDeclaresRegisteredHooks(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	m := plugins.NewManager(h)
	m.DiscoverAndLoad(bg, true)
	for _, r := range m.ListPlugins() {
		if r["key"] == "security-guidance" {
			if r["source"] != "bundled" || r["enabled"] != false {
				t.Fatalf("%v", r)
			}
			return
		}
	}
	t.Fatal("security-guidance not discovered")
}

func TestRegisterHooksMatchManifest(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := writeEnabled(h); err != nil {
		t.Fatal(err)
	}
	m := plugins.NewManager(h)
	m.DiscoverAndLoad(bg, true)
	for _, hook := range (Plugin{}).Manifest().ProvidesHooks {
		if !m.HasHook(hook) {
			t.Fatalf("hook %s not registered", hook)
		}
	}
	for _, r := range m.ListPlugins() {
		if r["key"] == "security-guidance" && r["hooks"] != len((Plugin{}).Manifest().ProvidesHooks) {
			t.Fatalf("%v", r)
		}
	}
}

// Enables security-guidance in the home config
func writeEnabled(h string) error {
	return os.WriteFile(filepath.Join(h, "config.yaml"), []byte("plugins:\n  enabled:\n  - security-guidance\n"), 0o644)
}
