//go:build !windows

package cron

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

type alwaysCancelled struct{}

func (alwaysCancelled) IsSet() bool    { return true }
func (alwaysCancelled) Set()           {}
func (alwaysCancelled) Reason() string { return "" }

func scriptStore(t *testing.T) *Store {
	t.Helper()
	s, _ := newTestStore(t)
	if err := os.MkdirAll(filepath.Join(s.Home(), "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Dir(s.Home()))
	return s
}

func writeScript(t *testing.T, s *Store, rel, body string) string {
	t.Helper()
	p := filepath.Join(s.Home(), "scripts", rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runScript(t *testing.T, s *Store, path string, interpreter *string) (bool, string) {
	t.Helper()
	ok, out, err := s.runJobScript(context.Background(), path, "", nil, interpreter)
	if err != nil {
		t.Fatal(err)
	}
	return ok, out
}

func TestRunJobScriptOutputs(t *testing.T) {
	s := scriptStore(t)
	cases := []struct{ name, body, ref, want string }{
		{"test.py", "print(\"hello from script\")\n", "", "hello from script"},
		{"relative.py", "print(\"relative works\")\n", "relative.py", "relative works"},
		{"emoji.py", "import sys\nsys.stdout.buffer.write(\"backup done \\N{PARTY POPPER} 日次\".encode(\"utf-8\"))\n", "emoji.py", "backup done \U0001F389 日次"},
		{"good.py", "print(\"ok\")\n", "good.py", "ok"},
		{"monitors/check.py", "print(\"sub ok\")\n", "monitors/check.py", "sub ok"},
	}
	for _, c := range cases {
		p := writeScript(t, s, c.name, c.body)
		ref := c.ref
		if ref == "" {
			ref = p
		}
		ok, out := runScript(t, s, ref, nil)
		if !ok || out != c.want {
			t.Fatalf("%s: %v %q", c.name, ok, out)
		}
	}
	writeScript(t, s, "binary_stdout.py", "import sys\nsys.stdout.buffer.write(b\"alert before \\x80 after\\n\")\n")
	ok, out := runScript(t, s, "binary_stdout.py", nil)
	if !ok || !strings.Contains(out, "alert before") || !strings.Contains(out, "�") {
		t.Fatalf("lossy decode %v %q", ok, out)
	}
	ok, out = runScript(t, s, "copied-from-other-profile.py", nil)
	if ok || !strings.Contains(out, "Script not found") || !strings.Contains(out, filepath.Join(s.Home(), "scripts")) {
		t.Fatalf("missing %v %q", ok, out)
	}
	writeScript(t, s, "fail.py", "import sys\nprint('partial')\nsys.stderr.write('boom')\nsys.exit(3)\n")
	ok, out = runScript(t, s, "fail.py", nil)
	if ok || out != "Script exited with code 3\nstderr:\nboom\nstdout:\npartial" {
		t.Fatalf("failure %v %q", ok, out)
	}
}

func TestConfiguredInterpreterIsRefusedUnlessAPythonPath(t *testing.T) {
	s := scriptStore(t)
	writeScript(t, s, "job.py", "print(\"ran\")\n")
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		make     func(d string) string
		expected string
	}{
		{"bare-name", func(d string) string { return "python3" }, "absolute or ~-prefixed"},
		{"missing", func(d string) string { return filepath.Join(d, "missing", "python3") }, "not found"},
		{"directory", func(d string) string { return d }, "not a file"},
		{"not-executable", func(d string) string {
			os.WriteFile(filepath.Join(d, "python3"), nil, 0o644)
			return filepath.Join(d, "python3")
		}, "not executable"},
		{"bash", func(d string) string { return "/bin/bash" }, "must be a Python executable"},
		{"python-symlink-to-bash", func(d string) string {
			os.Symlink("/bin/bash", filepath.Join(d, "python"))
			return filepath.Join(d, "python")
		}, "must be a Python executable"},
		{"pythonw", func(d string) string {
			os.Symlink(py, filepath.Join(d, "pythonw"))
			return filepath.Join(d, "pythonw")
		}, "must be a Python executable"},
	}
	for _, c := range cases {
		interp := c.make(t.TempDir())
		ok, out := runScript(t, s, "job.py", &interp)
		if ok || !strings.Contains(out, c.expected) {
			t.Fatalf("%s: %v %q", c.name, ok, out)
		}
	}
	ok, out := runScript(t, s, "job.py", &py)
	if !ok || out != "ran" {
		t.Fatalf("explicit python %v %q", ok, out)
	}
}

func TestScriptPathContainment(t *testing.T) {
	s := scriptStore(t)
	outside := filepath.Join(filepath.Dir(s.Home()), "outside.py")
	os.WriteFile(outside, []byte("print(\"should not run\")\n"), 0o600)
	evil := filepath.Join(t.TempDir(), "outside_evil.py")
	os.WriteFile(evil, []byte("print(\"escaped\")\n"), 0o600)
	os.Symlink(evil, filepath.Join(s.Home(), "scripts", "sneaky.py"))
	for _, ref := range []string{outside, "~/evil.py", "~/../../../tmp/evil.py", "../../etc/passwd", "sneaky.py"} {
		ok, out := runScript(t, s, ref, nil)
		low := strings.ToLower(out)
		if ok || !(strings.Contains(low, "blocked") || strings.Contains(low, "outside")) {
			t.Fatalf("%s: %v %q", ref, ok, out)
		}
	}
	ok, out := runScript(t, s, "bad\x00name.py", nil)
	if ok || !strings.HasPrefix(out, "Blocked: script path contains a NUL byte: ") {
		t.Fatalf("nul %v %q", ok, out)
	}
	os.MkdirAll(filepath.Join(s.Home(), "scripts", "adir"), 0o700)
	ok, out = runScript(t, s, "adir", nil)
	if ok || !strings.HasPrefix(out, "Script path is not a file: ") {
		t.Fatalf("dir %v %q", ok, out)
	}
}

func swapKillPaths(t *testing.T, result bool) (*[]int, *[]*cronScriptProcess) {
	prevKill, prevFallback := cronKillProcessTree, cronTerminateScriptProcess
	t.Cleanup(func() { cronKillProcessTree, cronTerminateScriptProcess = prevKill, prevFallback })
	var tree []int
	var fallback []*cronScriptProcess
	cronKillProcessTree = func(pid int, _ syscall.Signal) bool { tree = append(tree, pid); return result }
	cronTerminateScriptProcess = func(_ *Store, p *cronScriptProcess) { fallback = append(fallback, p) }
	return &tree, &fallback
}

func fakeScriptProcess(pid int, exited bool) *cronScriptProcess {
	p := &cronScriptProcess{cmd: &exec.Cmd{Process: &os.Process{Pid: pid}}, exited: make(chan struct{})}
	if exited {
		close(p.exited)
	}
	return p
}

func TestScriptTimeoutTreeKillPaths(t *testing.T) {
	s := scriptStore(t)
	tree, fallback := swapKillPaths(t, false)
	p := fakeScriptProcess(12345, false)
	s.terminateCronScriptTree(p)
	if len(*tree) != 1 || (*tree)[0] != 12345 || len(*fallback) != 1 || (*fallback)[0] != p {
		t.Fatalf("failure fallback %v %v", *tree, *fallback)
	}
	tree, fallback = swapKillPaths(t, true)
	p = fakeScriptProcess(0, false)
	s.terminateCronScriptTree(p)
	if len(*tree) != 0 || len(*fallback) != 1 {
		t.Fatalf("invalid pid %v %v", *tree, *fallback)
	}
	tree, fallback = swapKillPaths(t, true)
	s.terminateCronScriptTree(fakeScriptProcess(12345, true))
	if len(*tree) != 0 || len(*fallback) != 0 {
		t.Fatalf("exited %v %v", *tree, *fallback)
	}
}

func TestCancelPathAlsoTreeKills(t *testing.T) {
	s := scriptStore(t)
	prev := cronTerminateScriptTree
	t.Cleanup(func() { cronTerminateScriptTree = prev })
	var calls []int
	cronTerminateScriptTree = func(_ *Store, p *cronScriptProcess) {
		calls = append(calls, p.cmd.Process.Pid)
		p.cmd.Process.Kill()
	}
	p := writeScript(t, s, "long.py", "import time; time.sleep(30)\n")
	ok, out, err := s.runJobScript(context.Background(), p, filepath.Dir(s.Home()), alwaysCancelled{}, nil)
	if err != nil || ok || len(calls) != 1 || out != "Script cancelled: "+lostOwnershipReason {
		t.Fatalf("cancel %v %q %v %v", ok, out, calls, err)
	}
}

func TestSessionDBTimeoutKeepsZero(t *testing.T) {
	s := scriptStore(t)
	if got := s.getSessionDBTimeout(context.Background()); got != 10.0 {
		t.Fatalf("default %v", got)
	}
	t.Setenv("ICEBATH_CRON_SESSION_DB_TIMEOUT", "0")
	if got := s.getSessionDBTimeout(context.Background()); got != 0 {
		t.Fatalf("zero %v", got)
	}
	t.Setenv("ICEBATH_CRON_SESSION_DB_TIMEOUT", "2.5")
	if got := s.getSessionDBTimeout(context.Background()); got != 2.5 {
		t.Fatalf("float %v", got)
	}
}
