package cron

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/tools"
)

type alwaysCancelled struct{}

func (alwaysCancelled) IsSet() bool    { return true }
func (alwaysCancelled) Set()           {}
func (alwaysCancelled) Reason() string { return "" }

func scriptStore(t *testing.T) *Store {
	t.Helper()
	s, _ := newTestStore(t)
	for _, dir := range []string{"scripts", filepath.Join("cron", "output")} {
		if err := os.MkdirAll(filepath.Join(s.Home(), dir), 0o700); err != nil {
			t.Fatal(err)
		}
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

func hostPython(t *testing.T) string {
	t.Helper()
	python, err := cronHostPython()
	if err != nil {
		t.Fatal(err)
	}
	return python
}

func blockedOrOutside(out string) bool {
	low := strings.ToLower(out)
	return strings.Contains(low, "blocked") || strings.Contains(low, "outside")
}

func TestRunJobScriptSuccessfulScript(t *testing.T) {
	s := scriptStore(t)
	script := writeScript(t, s, "test.py", "print(\"hello from script\")\n")
	ok, out := runScript(t, s, script, nil)
	if !ok || out != "hello from script" {
		t.Fatalf("got %v %q", ok, out)
	}
}

func TestRunJobScriptScriptStdoutNonUtf8DecodedLossily(t *testing.T) {
	s := scriptStore(t)
	script := writeScript(t, s, "binary_stdout.py", "import sys\nsys.stdout.buffer.write(b\"alert before \\x80 after\\n\")\n")
	ok, out := runScript(t, s, script, nil)
	if !ok || !strings.Contains(out, "alert before") || !strings.Contains(out, "\ufffd") {
		t.Fatalf("got %v %q", ok, out)
	}
}

func TestRunJobScriptScriptRelativePath(t *testing.T) {
	s := scriptStore(t)
	writeScript(t, s, "relative.py", "print(\"relative works\")\n")
	ok, out := runScript(t, s, "relative.py", nil)
	if !ok || out != "relative works" {
		t.Fatalf("got %v %q", ok, out)
	}
}

func TestRunJobScriptMissingScriptNamesTheProfileFolder(t *testing.T) {
	s := scriptStore(t)
	ok, out := runScript(t, s, "copied-from-other-profile.py", nil)
	if ok || !strings.Contains(out, "Script not found") || !strings.Contains(out, filepath.Join(s.Home(), "scripts")) {
		t.Fatalf("got %v %q", ok, out)
	}
	scripts := filepath.Join(s.Home(), "scripts")
	want := "Script not found: " + filepath.Join(scripts, "copied-from-other-profile.py") + ". Cron scripts are looked up only in this profile's folder (" +
		scripts + "); if the job was copied from another profile, copy the script there too, or edit the job with `icebath cron edit`."
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestRunJobScriptScriptSubprocessEnvSanitized(t *testing.T) {
	s := scriptStore(t)
	var names []string
	for name := range tools.ProviderEnvBlocklist() {
		names = append(names, name)
	}
	blocked := slices.Min(names)
	t.Setenv(blocked, "must_not_leak")
	writeScript(t, s, "env_probe.py", "import os\nkey = "+pyrepr.StrRepr(blocked)+"\nprint(\"PRESENT\" if os.environ.get(key) else \"ABSENT\")\n")
	ok, out := runScript(t, s, "env_probe.py", nil)
	if !ok || out != "ABSENT" {
		t.Fatalf("got %v %q", ok, out)
	}
}

func runBootstrapArgv(t *testing.T, argv []string) string {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bootstrap failed %v %s", err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func TestRunJobScriptEmojiStdoutRoundTripsThroughScriptCapture(t *testing.T) {
	s := scriptStore(t)
	writeScript(t, s, "emoji.py", "import sys\nsys.stdout.buffer.write(\"backup done \\N{PARTY POPPER} 日次\".encode(\"utf-8\"))\n")
	ok, out := runScript(t, s, "emoji.py", nil)
	if !ok || out != "backup done \U0001F389 日次" {
		t.Fatalf("got %v %q", ok, out)
	}
}

func TestScriptPathContainmentAbsolutePathOutsideScriptsDirBlocked(t *testing.T) {
	s := scriptStore(t)
	outside := filepath.Join(s.Home(), "outside.py")
	if err := os.WriteFile(outside, []byte("print(\"should not run\")\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, out := runScript(t, s, outside, nil)
	if ok || !blockedOrOutside(out) {
		t.Fatalf("got %v %q", ok, out)
	}
	want := "Blocked: script path resolves outside the scripts directory (" + filepath.Join(s.Home(), "scripts") + "): " + pyrepr.StrRepr(outside)
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestScriptPathContainmentTildePathBlocked(t *testing.T) {
	s := scriptStore(t)
	ok, out := runScript(t, s, "~/evil.py", nil)
	if ok || !blockedOrOutside(out) {
		t.Fatalf("got %v %q", ok, out)
	}
}

func TestScriptPathContainmentTildeTraversalBlocked(t *testing.T) {
	s := scriptStore(t)
	ok, out := runScript(t, s, "~/../../../tmp/evil.py", nil)
	if ok || !blockedOrOutside(out) {
		t.Fatalf("got %v %q", ok, out)
	}
}

func TestScriptPathContainmentRelativeTraversalStillBlocked(t *testing.T) {
	s := scriptStore(t)
	ok, out := runScript(t, s, "../../etc/passwd", nil)
	if ok || !blockedOrOutside(out) {
		t.Fatalf("got %v %q", ok, out)
	}
}

func TestScriptPathContainmentRelativePathInsideScriptsDirAllowed(t *testing.T) {
	s := scriptStore(t)
	writeScript(t, s, "good.py", "print(\"ok\")\n")
	ok, out := runScript(t, s, "good.py", nil)
	if !ok || out != "ok" {
		t.Fatalf("got %v %q", ok, out)
	}
}

func TestScriptPathContainmentSubdirectoryInsideScriptsDirAllowed(t *testing.T) {
	s := scriptStore(t)
	writeScript(t, s, filepath.Join("monitors", "check.py"), "print(\"sub ok\")\n")
	ok, out := runScript(t, s, "monitors/check.py", nil)
	if !ok || out != "sub ok" {
		t.Fatalf("got %v %q", ok, out)
	}
}

func TestScriptPathRefusalsForNulUnknownUserAndDirectories(t *testing.T) {
	s := scriptStore(t)
	ok, out := runScript(t, s, "bad\x00name.py", nil)
	if ok || out != "Blocked: script path contains a NUL byte: 'bad\\x00name.py'" {
		t.Fatalf("nul %v %q", ok, out)
	}
	ok, out = runScript(t, s, "~no_such_user_zz/job.py", nil)
	if ok || out != "Blocked: script path is not a valid filesystem path: '~no_such_user_zz/job.py'" {
		t.Fatalf("unknown user %v %q", ok, out)
	}
	if err := os.MkdirAll(filepath.Join(s.Home(), "scripts", "adir"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"adir", "adir//", "./adir/."} {
		ok, out = runScript(t, s, ref, nil)
		if ok || out != "Script path is not a file: "+filepath.Join(s.Home(), "scripts", "adir") {
			t.Fatalf("dir %q %v %q", ref, ok, out)
		}
	}
	ok, out = runScript(t, s, "", nil)
	if ok || out != "Script path is not a file: "+filepath.Join(s.Home(), "scripts") {
		t.Fatalf("empty %v %q", ok, out)
	}
}

func swapKillPaths(t *testing.T, result bool) (*[]int, *[]*cronScriptProcess) {
	prevKill, prevFallback := cronKillProcessTree, cronTerminateScriptProcess
	t.Cleanup(func() { cronKillProcessTree, cronTerminateScriptProcess = prevKill, prevFallback })
	var tree []int
	var fallback []*cronScriptProcess
	cronKillProcessTree = func(pid int, _ syscall.Signal) bool { tree = append(tree, pid); return result }
	cronTerminateScriptProcess = func(_ *Store, p *cronScriptProcess) error { fallback = append(fallback, p); return nil }
	return &tree, &fallback
}

func fakeScriptProcess(pid int, exited bool) *cronScriptProcess {
	p := &cronScriptProcess{cmd: &exec.Cmd{Process: &os.Process{Pid: pid}}, exited: make(chan struct{})}
	if exited {
		close(p.exited)
	}
	return p
}

func TestScriptTimeoutTreeKillUnifiedTreeKillFailureFallsBack(t *testing.T) {
	s, logs := newTestStore(t)
	tree, fallback := swapKillPaths(t, false)
	p := fakeScriptProcess(12345, false)
	if err := s.terminateCronScriptTree(p); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*tree, []int{12345}) || len(*fallback) != 1 || (*fallback)[0] != p {
		t.Fatalf("got %v %v", *tree, *fallback)
	}
	if !strings.Contains(logs.String(), "Cron script tree-kill reported no signal for pid 12345; falling back to process-group termination") {
		t.Fatalf("warning missing from %s", logs.String())
	}
}

func TestScriptTimeoutTreeKillInvalidPidNeverReachesUnifiedTreeKill(t *testing.T) {
	s, logs := newTestStore(t)
	tree, fallback := swapKillPaths(t, true)
	p := fakeScriptProcess(0, false)
	if err := s.terminateCronScriptTree(p); err != nil {
		t.Fatal(err)
	}
	if len(*tree) != 0 || len(*fallback) != 1 || (*fallback)[0] != p {
		t.Fatalf("got %v %v", *tree, *fallback)
	}
	if !strings.Contains(logs.String(), "Cron script tree-kill received invalid pid 0; falling back to process-group termination") {
		t.Fatalf("warning missing from %s", logs.String())
	}
}

func TestScriptTimeoutTreeKillAlreadyExitedProcIsLeftAlone(t *testing.T) {
	s, _ := newTestStore(t)
	tree, fallback := swapKillPaths(t, true)
	if err := s.terminateCronScriptTree(fakeScriptProcess(12345, true)); err != nil {
		t.Fatal(err)
	}
	if len(*tree) != 0 || len(*fallback) != 0 {
		t.Fatalf("got %v %v", *tree, *fallback)
	}
}

func TestScriptTreeKillThatPanicsFallsBackToTheProcessGroup(t *testing.T) {
	s, logs := newTestStore(t)
	_, fallback := swapKillPaths(t, true)
	cronKillProcessTree = func(int, syscall.Signal) bool { panic("psutil gone") }
	p := fakeScriptProcess(777, false)
	if err := s.terminateCronScriptTree(p); err != nil {
		t.Fatal(err)
	}
	if len(*fallback) != 1 || (*fallback)[0] != p || !strings.Contains(logs.String(), "Cron script tree-kill failed for pid 777; falling back to process-group termination") {
		t.Fatalf("got %v %s", *fallback, logs.String())
	}
}

func TestScriptTimeoutTreeKillCancelPathAlsoTreeKills(t *testing.T) {
	s := scriptStore(t)
	prev := cronTerminateScriptTree
	t.Cleanup(func() { cronTerminateScriptTree = prev })
	var calls []int
	cronTerminateScriptTree = func(_ *Store, p *cronScriptProcess) error {
		calls = append(calls, p.cmd.Process.Pid)
		return p.cmd.Process.Kill()
	}
	script := writeScript(t, s, "long.py", "import time; time.sleep(30)\n")
	ok, out, err := s.runJobScript(context.Background(), script, s.Home(), alwaysCancelled{}, nil)
	if err != nil || ok || !strings.Contains(out, "ownership was lost") || len(calls) != 1 {
		t.Fatalf("cancel %v %q %v %v", ok, out, calls, err)
	}
	if out != "Script cancelled: cron fire ownership was lost" {
		t.Fatalf("got %q", out)
	}
}

func TestScriptTimeoutTreeKillTimeoutLeavesNoSetsidGrandchild(t *testing.T) {
	s := scriptStore(t)
	pidFile := filepath.Join(s.Home(), "grandchild.pid")
	script := writeScript(t, s, "spawner.py", "import subprocess, sys, time\n"+
		"p = subprocess.Popen(\n"+
		"    [sys.executable, '-c', 'import time; time.sleep(30)'],\n"+
		"    start_new_session=True,\n"+
		"    stdin=subprocess.DEVNULL,\n"+
		"    stdout=subprocess.DEVNULL,\n"+
		"    stderr=subprocess.DEVNULL,\n"+
		")\n"+
		"open("+pyrepr.StrRepr(pidFile)+", 'w').write(str(p.pid))\n"+
		"time.sleep(30)\n")
	t.Setenv("ICEBATH_CRON_SCRIPT_TIMEOUT", "2")
	prev := scriptTimeoutOverride
	scriptTimeoutOverride = defaultScriptTimeout
	t.Cleanup(func() { scriptTimeoutOverride = prev })
	ok, out, err := s.runJobScript(context.Background(), script, s.Home(), nil, nil)
	if err != nil || ok || !strings.HasPrefix(out, "Script timed out after 2s:") {
		t.Fatalf("expected the timeout path, got success=%v, output=%q, err=%v", ok, out, err)
	}
	if out != "Script timed out after 2s: "+script {
		t.Fatalf("got %q", out)
	}
	grandchild := 0
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && grandchild == 0; {
		raw, readErr := os.ReadFile(pidFile)
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); readErr == nil && convErr == nil {
			grandchild = pid
		} else {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if grandchild == 0 {
		t.Fatal("spawner never wrote the grandchild pid")
	}
	t.Cleanup(func() {
		if scriptProcessLive(grandchild) {
			killScriptProcess(grandchild)
		}
	})
	for deadline := time.Now().Add(5 * time.Second); scriptProcessLive(grandchild) && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if scriptProcessLive(grandchild) {
		t.Fatalf("grandchild pid %d survived the script timeout, the timeout path orphaned an own-session descendant", grandchild)
	}
}

func mediaTimeoutStore(t *testing.T, cfg config.Tree) *Store {
	t.Helper()
	s, _ := newTestStore(t)
	s.loadConfig = func() (config.Tree, error) { return cfg, nil }
	return s
}

func TestMediaSendTimeoutResolutionDefault(t *testing.T) {
	s := mediaTimeoutStore(t, config.Tree{})
	t.Setenv("ICEBATH_CRON_MEDIA_SEND_TIMEOUT", "")
	if got := s.getMediaSendTimeout(context.Background()); defaultMediaSendTimeout != 300 || got != defaultMediaSendTimeout {
		t.Fatalf("got %d", got)
	}
}

func TestMediaSendTimeoutResolutionEnvWins(t *testing.T) {
	s := mediaTimeoutStore(t, config.Tree{"cron": map[string]any{"media_send_timeout_seconds": 900}})
	t.Setenv("ICEBATH_CRON_MEDIA_SEND_TIMEOUT", "45")
	if got := s.getMediaSendTimeout(context.Background()); got != 45 {
		t.Fatalf("got %d", got)
	}
}

func TestMediaSendTimeoutResolutionConfigValue(t *testing.T) {
	s := mediaTimeoutStore(t, config.Tree{"cron": map[string]any{"media_send_timeout_seconds": 900}})
	t.Setenv("ICEBATH_CRON_MEDIA_SEND_TIMEOUT", "")
	if got := s.getMediaSendTimeout(context.Background()); got != 900 {
		t.Fatalf("got %d", got)
	}
}

func TestMediaSendTimeoutResolutionInvalidEnvFallsBack(t *testing.T) {
	for _, bad := range []string{"abc", "-5", "0", ""} {
		s := mediaTimeoutStore(t, config.Tree{})
		t.Setenv("ICEBATH_CRON_MEDIA_SEND_TIMEOUT", bad)
		if got := s.getMediaSendTimeout(context.Background()); got != defaultMediaSendTimeout {
			t.Fatalf("%q got %d", bad, got)
		}
	}
}

func TestMediaSendTimeoutResolutionInvalidConfigFallsBack(t *testing.T) {
	s := mediaTimeoutStore(t, config.Tree{"cron": map[string]any{"media_send_timeout_seconds": "nope"}})
	t.Setenv("ICEBATH_CRON_MEDIA_SEND_TIMEOUT", "")
	if got := s.getMediaSendTimeout(context.Background()); got != defaultMediaSendTimeout {
		t.Fatalf("got %d", got)
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

func TestScriptRunReportsAKilledScriptThatIsNeverReaped(t *testing.T) {
	s := scriptStore(t)
	prevKill, prevFallback := cronKillProcessTree, cronTerminateScriptProcess
	t.Cleanup(func() { cronKillProcessTree, cronTerminateScriptProcess = prevKill, prevFallback })
	cronKillProcessTree = func(int, syscall.Signal) bool { return false }
	var started *cronScriptProcess
	cronTerminateScriptProcess = func(_ *Store, p *cronScriptProcess) error {
		started = p
		seconds := 1.0
		return &pysubprocess.TimeoutExpired{Cmd: p.cmd.Args, TimeoutFloat: &seconds}
	}
	script := writeScript(t, s, "long.py", "import time; time.sleep(30)\n")
	python := hostPython(t)
	ok, out, err := s.runJobScript(context.Background(), script, "", alwaysCancelled{}, &python)
	if started != nil {
		started.cmd.Process.Kill()
		started.wait(5 * time.Second)
	}
	want := "Script execution failed: Command '[" + pyrepr.StrRepr(python) + ", " + pyrepr.StrRepr(script) + "]' timed out after 1.0 seconds"
	if err != nil || ok || out != want {
		t.Fatalf("got %v %q %v want %q", ok, out, err, want)
	}
}

func swapBashLookup(t *testing.T, found string) {
	t.Helper()
	prevWhich, prevFallback := cronWhich, cronBashFallback
	t.Cleanup(func() { cronWhich, cronBashFallback = prevWhich, prevFallback })
	cronWhich = func(name string, _ *string) (string, bool) {
		if name == "bash" && found != "" {
			return found, true
		}
		return "", false
	}
	cronBashFallback = filepath.Join(t.TempDir(), "no-bash-here")
}

func TestCronSchedulerBashResolutionShScriptUsesBashFoundOnPath(t *testing.T) {
	s, _ := newTestStore(t)
	tmp := t.TempDir()
	script := filepath.Join(tmp, "job.sh")
	if err := os.WriteFile(script, []byte("echo hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	found := filepath.Join(tmp, "git", "bin", "bash.exe")
	swapBashLookup(t, found)
	argv, refusal, err := s.scriptArgv(script, nil)
	if err != nil || refusal != "" || !reflect.DeepEqual(argv, []string{found, script}) {
		t.Fatalf("got %q %q %v", argv, refusal, err)
	}
}

func TestCronSchedulerBashResolutionMissingBashReturnsActionableError(t *testing.T) {
	s, _ := newTestStore(t)
	script := filepath.Join(t.TempDir(), "job.sh")
	if err := os.WriteFile(script, []byte("echo hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	swapBashLookup(t, "")
	argv, refusal, err := s.scriptArgv(script, nil)
	if err != nil || argv != nil || !strings.Contains(refusal, "bash not found") || !strings.Contains(refusal, "job.sh") {
		t.Fatalf("got %q %q %v", argv, refusal, err)
	}
	want := "Cannot run .sh/.bash script 'job.sh': bash not found on PATH. On Windows, install Git for Windows (which ships Git Bash) or rewrite the script as Python (.py)."
	if refusal != want {
		t.Fatalf("got %q", refusal)
	}
}

func TestScriptArgvPicksBashOnlyForShellSuffixes(t *testing.T) {
	s, _ := newTestStore(t)
	swapBashLookup(t, "/opt/bash")
	prev := cronSysExecutable
	t.Cleanup(func() { cronSysExecutable = prev })
	cronSysExecutable = func() (string, error) { return "/opt/python3", nil }
	for name, bash := range map[string]bool{"job.sh": true, "JOB.BASH": true, "job.Sh": true, ".sh": false, "job.sh.py": false, "job": false} {
		argv, refusal, err := s.scriptArgv(filepath.Join("/scripts", name), nil)
		if err != nil || refusal != "" || len(argv) != 2 || (argv[0] == "/opt/bash") != bash {
			t.Fatalf("%s got %q %q %v", name, argv, refusal, err)
		}
	}
}

func TestRunJobScriptNulPathFailsCleanly(t *testing.T) {
	s := scriptStore(t)
	ok, out := runScript(t, s, "~user\x00bad.sh", nil)
	if ok || !strings.Contains(out, "NUL byte") {
		t.Fatalf("got %v %q", ok, out)
	}
}
