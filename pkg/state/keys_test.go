package state

import (
	"crypto/sha256"
	"encoding/hex"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestProfileFromSessionKey(t *testing.T) {
	cases := []struct {
		key     string
		profile string
		ok      bool
	}{
		{"agent:main:local:dm:x", "default", true},
		{"agent:main~:local:dm:x", "main", true},
		{"agent:work:local:dm:x", "work", true},
		{"agent::x", "default", true},
		{"cli:x", "", false},
		{"", "", false},
		{"agent", "", false},
	}
	for _, c := range cases {
		profile, ok := ProfileFromSessionKey(c.key)
		if profile != c.profile || ok != c.ok {
			t.Errorf("ProfileFromSessionKey(%q) = %q %v, want %q %v", c.key, profile, ok, c.profile, c.ok)
		}
	}
}

func TestCronRequestIdentity(t *testing.T) {
	if got := CronRequestID("job", 42); got != "cron:job:42" {
		t.Errorf("CronRequestID = %q", got)
	}
	lo, hi := CronRequestRange("job")
	if lo != "cron:job:" || hi != "cron:job;" {
		t.Errorf("CronRequestRange = %q %q", lo, hi)
	}
	if _, hi, err := PrefixRange("ab\u00e9"); err != nil || hi != "ab\u00ea" {
		t.Errorf("PrefixRange bumps the final character, got %q %v", hi, err)
	}
	if _, _, err := PrefixRange(""); err == nil {
		t.Error("an empty prefix has no final character")
	}
	job, exec, ok := CronRunFromRequest("cron:job:7", nil)
	if !ok || job != "job" || exec != "7" {
		t.Errorf("split = %q %q %v", job, exec, ok)
	}
	for _, request := range []any{"cron:a:b:c", "cron:job:", "cron::7", "other:job:7", 7} {
		if _, _, ok := CronRunFromRequest(request, nil); ok {
			t.Errorf("%v should not split", request)
		}
	}
	named := "a:b"
	job, exec, ok = CronRunFromRequest("cron:a:b:9", &named)
	if !ok || job != "a:b" || exec != "9" {
		t.Errorf("named split = %q %q %v", job, exec, ok)
	}
	other := "zzz"
	if _, _, ok := CronRunFromRequest("cron:a:b:9", &other); ok {
		t.Error("a job id that does not prefix the request must not split")
	}
	empty := ""
	if _, _, ok := CronRunFromRequest("cron::9", &empty); ok {
		t.Error("an empty job id never splits")
	}
	if job, ok := LegacyCronRunJob("cron_nightly_backup_20260102_030405"); !ok || job != "nightly_backup" {
		t.Errorf("legacy job = %q %v", job, ok)
	}
	if _, ok := LegacyCronRunJob("cron_x_2026010_030405"); ok {
		t.Error("short stamp is not a legacy run")
	}
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestAdmissionFingerprintAndLocalIdentity(t *testing.T) {
	payload := map[string]any{"text": "caf\u00e9 <b>&", "n": 1, "f": 1.5, "big": 1e16, "list": []any{nil, true, "\u2028"}}
	got, err := AdmissionFingerprint("telegram:1", payload)
	if err != nil {
		t.Fatal(err)
	}
	want := sha(`{"payload":{"big":1e+16,"f":1.5,"list":[null,true,"` + "\u2028" + `"],"n":1,"text":"caf` + "\u00e9" + ` <b>&"},"target":"telegram:1"}`)
	if got != want {
		t.Errorf("fingerprint = %s, want %s", got, want)
	}
	if _, err := AdmissionFingerprint("t", map[string]any{"x": math.NaN()}); err == nil {
		t.Error("allow_nan=False refuses NaN")
	}
	id, err := LocalIdentity("p\u00e9", nil, "r")
	if err != nil || id != "local-"+sha(`["p\u00e9",null,"r"]`) {
		t.Errorf("local identity = %s %v", id, err)
	}
}

func TestPyDumpsDefaults(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{pyObject{{"b", 1.0}, {"a", "x\"\\\n\t\b\f\x01\x7f\u00e9\U0001F600"}}, `{"b": 1.0, "a": "x\"\\\n\t\b\f\u0001\u007f\u00e9\ud83d\ude00"}`},
		{[]any{0.1, 1e-05, 123456789012345680.0, math.Copysign(0, -1), math.Inf(1)}, `[0.1, 1e-05, 1.2345678901234568e+17, -0.0, Infinity]`},
		{"\xed\xa0\x80", `"\ud800"`},
	}
	for _, c := range cases {
		got, err := pyDumps(c.in, pyJSONDefault)
		if err != nil || got != c.want {
			t.Errorf("pyDumps(%v) = %s %v, want %s", c.in, got, err, c.want)
		}
	}
	if got := pyRepr("a'b\n\u00e9\x00"); got != `"a'b\n`+"\u00e9"+`\x00"` {
		t.Errorf("pyRepr = %s", got)
	}
}

// Collects the module packages a package imports, transitively
func moduleImports(t *testing.T, root, module, pkg string, seen map[string]bool) {
	if seen[pkg] {
		return
	}
	seen[pkg] = true
	dir := filepath.Join(root, strings.TrimPrefix(pkg, module+"/"))
	pkgs, err := parser.ParseDir(token.NewFileSet(), dir, func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pkgs {
		for _, f := range p.Files {
			for _, spec := range f.Imports {
				path, _ := strconv.Unquote(spec.Path.Value)
				if strings.HasPrefix(path, module+"/") {
					moduleImports(t, root, module, path, seen)
				}
			}
		}
	}
}

func TestStorageImportsWithoutGateway(t *testing.T) {
	const module = "github.com/nickheyer/icebath"
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	moduleImports(t, root, module, module+"/pkg/state", seen)
	for pkg := range seen {
		if strings.Contains(pkg, "/gateway") {
			t.Errorf("pkg/state pulls in %s", pkg)
		}
	}
}
