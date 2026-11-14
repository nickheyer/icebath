package skills

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func astPids(findings []ASTFinding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, f.PatternID)
	}
	return out
}

func writeASTFixture(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestASTScanBypassPayloadDetected(t *testing.T) {
	f := writeASTFixture(t, "exfil.py", "import importlib\n"+
		"parts = ['o', 's']\n"+
		"m = importlib.import_module(''.join(parts))\n"+
		"e = m.__dict__[''.join(['e','n','v'])]\n")
	pids := strings.Join(astPids(ASTScanPath(f)), " ")
	for _, want := range []string{"dynamic_import", "importlib_import", "dict_access"} {
		if !strings.Contains(" "+pids+" ", " "+want+" ") {
			t.Fatalf("%s missing from %q", want, pids)
		}
	}
}

func TestASTScanSyntaxErrorDoesNotCrash(t *testing.T) {
	if got := ASTScanPath(writeASTFixture(t, "bad.py", "def broken(\n")); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestASTScanRecursionErrorDoesNotCrash(t *testing.T) {
	f := writeASTFixture(t, "deep.py", "a"+strings.Repeat(".x", 5000)+"\n")
	orig := astScanRecursionLimit
	astScanRecursionLimit = 200
	defer func() { astScanRecursionLimit = orig }()
	if got := ASTScanPath(f); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestFormatASTReportWithFindings(t *testing.T) {
	findings := []ASTFinding{
		{"a.py", 1, "importlib_import", "import importlib — ..."},
		{"a.py", 3, "dynamic_import", "importlib.import_module() — ..."},
	}
	out := FormatASTReport(findings, "test")
	for _, want := range []string{"test", "a.py", "L1", "L3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q missing from %q", want, out)
		}
	}
}

// Findings recorded from CPython 3.14 running _scan_source under the icebath skills audit --deep frame depth
func TestASTScanSourceRecursionDepthMatchesCPython(t *testing.T) {
	cases := []struct {
		head, step string
		n          int
		tail, want string
	}{
		{"__import__(y)", ".x", 492, "", "1:dynamic_import_computed"},
		{"__import__(y)", ".x", 493, "", ""},
		{"__import__(y)", ".x", 5000, "", ""},
		{"a.__dict__[k]", ".x", 492, "", "1:dict_access"},
		{"a.__dict__[k]", ".x", 493, "", ""},
		{"a.__dict__[k]", ".x", 5000, "", ""},
		{"getattr(a, b)", ".__dict__[k]", 196, "", "1:dict_access*196 1:dynamic_getattr"},
		{"getattr(a, b)", ".__dict__[k]", 197, "", "1:dict_access*197"},
		{"getattr(a, b)", ".__dict__[k]", 5000, "", "1:dict_access*197"},
		{"q.import_module(z)", ".y.__dict__[k]", 139, "\nimport importlib, importlib.util\nfrom importlib import x\n", "1:dict_access*139 1:dynamic_import 2:importlib_import*2 3:importlib_import"},
		{"q.import_module(z)", ".y.__dict__[k]", 140, "\nimport importlib, importlib.util\nfrom importlib import x\n", "1:dict_access*140 1:dynamic_import"},
		{"q.import_module(z)", ".y.__dict__[k]", 141, "\nimport importlib, importlib.util\nfrom importlib import x\n", "1:dict_access*141"},
		{"q.import_module(z)", ".y.__dict__[k]", 5000, "\nimport importlib, importlib.util\nfrom importlib import x\n", "1:dict_access*141"},
		{"f(__import__(y), getattr(a, b))", ".z", 488, "\nfrom importlib.util import q\n", "1:dynamic_import_computed 1:dynamic_getattr 2:importlib_import"},
		{"f(__import__(y), getattr(a, b))", ".z", 489, "\nfrom importlib.util import q\n", "1:dynamic_import_computed"},
		{"f(__import__(y), getattr(a, b))", ".z", 490, "\nfrom importlib.util import q\n", ""},
		{"f(__import__(y), getattr(a, b))", ".z", 5000, "\nfrom importlib.util import q\n", ""},
	}
	for _, c := range cases {
		var got []string
		for _, f := range astScanSource(c.head+strings.Repeat(c.step, c.n)+c.tail+"\n", "f.py") {
			k := strconv.Itoa(f.Line) + ":" + f.PatternID
			if last := len(got) - 1; last >= 0 && strings.SplitN(got[last], "*", 2)[0] == k {
				cnt := 1
				if parts := strings.SplitN(got[last], "*", 2); len(parts) == 2 {
					cnt, _ = strconv.Atoi(parts[1])
				}
				got[last] = k + "*" + strconv.Itoa(cnt+1)
				continue
			}
			got = append(got, k)
		}
		if g := strings.Join(got, " "); g != c.want {
			t.Errorf("%s%s*%d: got %q want %q", c.head, c.step, c.n, g, c.want)
		}
	}
}

func TestASTScanSourceMemoryErrorPropagates(t *testing.T) {
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(error).Error(), "Parser stack overflowed") {
			t.Fatalf("recovered %v", r)
		}
	}()
	astScanSource("a"+strings.Repeat(" if a else a", 10000)+"\n", "f.py")
}
