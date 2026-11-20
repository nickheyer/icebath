package plugins

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/skills"
)

var guardBaseFiles = map[string]string{
	"plugin.yaml": "name: test-plugin\nmanifest_version: 1\n",
	"__init__.py": "def register(ctx):\n    ctx.register_tool('hello', lambda: 'hi')\n",
	"README.md":   "# Test plugin\n\nA simple test plugin.\n",
}

const (
	guardEngine    = "def make_execution_decision(**kw):\n    return kw.get('token') is not None\n\n\n"
	guardTokenLine = "token=\"USR-session123-abc123def4567890\"\n"
)

func mkGuardPlugin(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	plugin := filepath.Join(dir, "test-plugin")
	for rel, content := range files {
		p := filepath.Join(plugin, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return plugin
}

func guardFilesWith(extra map[string]string) map[string]string {
	files := map[string]string{}
	for k, v := range guardBaseFiles {
		files[k] = v
	}
	for k, v := range extra {
		files[k] = v
	}
	return files
}

func firstFinding(t *testing.T, r skills.ScanResult, pid string) skills.Finding {
	t.Helper()
	for _, f := range r.Findings {
		if f.PatternID == pid {
			return f
		}
	}
	t.Fatalf("no %s finding in %+v", pid, r.Findings)
	return skills.Finding{}
}

func TestMainGuardSampleTokenIsReviewableCautionButModuleLevelIsNot(t *testing.T) {
	dir := t.TempDir()
	guarded := filepath.Join(dir, "guarded")
	result := ScanPlugin(mkGuardPlugin(t, guarded, guardFilesWith(map[string]string{
		"phase6_policy_engine.py": guardEngine + "if __name__ == '__main__':\n    " + guardTokenLine,
	})), "")
	if f := firstFinding(t, result, "hardcoded_secret"); f.Severity != "high" || result.Verdict != "caution" {
		t.Fatalf("guarded %+v verdict %s", f, result.Verdict)
	}
	if allow, _ := ShouldAllowPluginInstall(result, false); allow != nil {
		t.Fatalf("guarded allow %v", *allow)
	}
	if allow, _ := ShouldAllowPluginInstall(result, true); allow == nil || !*allow {
		t.Fatalf("guarded forced allow %v", allow)
	}
	moduleLevel := filepath.Join(dir, "module_level")
	result = ScanPlugin(mkGuardPlugin(t, moduleLevel, guardFilesWith(map[string]string{
		"phase6_policy_engine.py": guardEngine + guardTokenLine,
	})), "")
	if f := firstFinding(t, result, "hardcoded_secret"); f.Severity != "critical" || result.Verdict != "dangerous" {
		t.Fatalf("module level %+v verdict %s", f, result.Verdict)
	}
	if allow, _ := ShouldAllowPluginInstall(result, true); allow == nil || *allow {
		t.Fatalf("module level forced allow %v", allow)
	}
}

func TestOnlyGenericSampleTokensAreDemotedInsideMainGuard(t *testing.T) {
	result := ScanPlugin(mkGuardPlugin(t, t.TempDir(), guardFilesWith(map[string]string{
		"engine.py": "import os\n\nif '__main__' == __name__:\n    os.system('rm -rf /')\n    token = 'sk-abcdefghijklmnopqrstuvwxyz'\n",
		"broken.py": "if __name__ == '__main__':\n    " + guardTokenLine + "def broken(:\n",
	})), "")
	critical := map[[2]string]bool{}
	for _, f := range result.Findings {
		if f.Severity == "critical" {
			critical[[2]string{f.File, f.PatternID}] = true
		}
	}
	for _, want := range [][2]string{{"engine.py", "destructive_root_rm"}, {"engine.py", "openai_key_leaked"}, {"broken.py", "hardcoded_secret"}} {
		if !critical[want] {
			t.Fatalf("%v not critical in %+v", want, result.Findings)
		}
	}
	if result.Verdict != "dangerous" {
		t.Fatalf("verdict %s", result.Verdict)
	}
	if allow, _ := ShouldAllowPluginInstall(result, true); allow == nil || *allow {
		t.Fatalf("forced allow %v", allow)
	}
}

// Guard lines of statements nested in other blocks, parenthesized and multi line tests, none for strings or invalid code
func TestMainGuardBodyLinesFollowsTheAST(t *testing.T) {
	cases := []struct {
		src  string
		want []int
	}{
		{"if (__name__ ==\n        '__main__'):\n    a = 1\n    b = (2,\n         3)\nc = 4\n", []int{3, 4, 5}},
		{"def f():\n    if __name__ == \"__main__\":\n        x = 1\n", []int{3}},
		{"s = '''\nif __name__ == '__main__':\n    x = 1\n'''\n", nil},
		{"if __name__ == b'__main__':\n    x = 1\n", nil},
		{"if __name__ == '__main__' == y:\n    x = 1\n", nil},
		{"if __name__ != '__main__':\n    x = 1\n", nil},
		{"\ufeffif '__main__' == __name__: x = 1; y = 2\n", []int{1}},
		{"if __name__ == '__main__':\r\n    x = 1\r\n    y = 2\r\n", []int{2, 3}},
		{"if __name__ == '__main__':\n    x = 1\ndef broken(:\n", nil},
		{"if __name__ == '__main__':\n    x = '\xff'\n", nil},
	}
	for _, c := range cases {
		p := filepath.Join(t.TempDir(), "m.py")
		if err := os.WriteFile(p, []byte(c.src), 0o644); err != nil {
			t.Fatal(err)
		}
		got := mainGuardBodyLines(p)
		ok := len(got) == len(c.want)
		for _, l := range c.want {
			ok = ok && got[l]
		}
		if !ok {
			t.Errorf("%q: got %v want %v", c.src, got, c.want)
		}
	}
	if got := mainGuardBodyLines(filepath.Join(t.TempDir(), "gone.py")); len(got) != 0 {
		t.Fatalf("missing file %v", got)
	}
}
