package gatewaycli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/profiles"
)

const fakeS6PID = 4242

// Scripted s6 tool double recording every call, svstat answers from up or an override
type fakeS6 struct {
	mu       sync.Mutex
	up       bool
	override *s6Result
	err      error
	calls    [][]string
}

func installFakeS6(t *testing.T) *fakeS6 {
	t.Helper()
	f := &fakeS6{up: true}
	old := s6Run
	t.Cleanup(func() { s6Run = old })
	s6Run = func(_ time.Duration, check bool, cmd string, args ...string) (s6Result, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, append([]string{cmd}, args...))
		if cmd != "s6-svstat" {
			return s6Result{}, nil
		}
		if f.err != nil {
			return s6Result{}, f.err
		}
		if f.override != nil {
			return *f.override, nil
		}
		table := map[string]string{"up": "false", "pid": "-1", "pgid": "-1", "exitcode": "0"}
		if f.up {
			table = map[string]string{"up": "true", "pid": "4242", "pgid": "4242", "exitcode": "-1"}
		}
		var vals []string
		for _, field := range strings.Split(args[1], ",") {
			v, ok := table[field]
			if !ok {
				t.Fatalf("unknown s6-svstat field %q", field)
			}
			vals = append(vals, v)
		}
		return s6Result{Stdout: strings.Join(vals, " ") + "\n"}, nil
	}
	return f
}

func (f *fakeS6) downIssuedAfterSvstat() (bool, bool) {
	sawSvstat := false
	for _, c := range f.calls {
		if c[0] == "s6-svstat" {
			sawSvstat = true
		}
		if c[0] == "s6-svc" && len(c) > 1 && c[1] == "-d" {
			return true, sawSvstat
		}
	}
	return false, sawSvstat
}

// Records planned stop markers written by the s6 stop path
func recordMarkers(t *testing.T, result bool) *[]int {
	var marked []int
	old := writePlannedStop
	t.Cleanup(func() { writePlannedStop = old })
	writePlannedStop = func(pid int) bool { marked = append(marked, pid); return result }
	return &marked
}

func TestSeedSuperviseSkeletonCreatesExpectedLayout(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	svc := filepath.Join(t.TempDir(), "gateway-foo")
	if err := os.Mkdir(svc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := seedSuperviseSkeleton(svc); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"event", "supervise", "supervise/event"} {
		if st, err := os.Stat(filepath.Join(svc, rel)); err != nil || !st.IsDir() {
			t.Fatalf("missing %s/", rel)
		}
	}
	if st, _ := os.Stat(filepath.Join(svc, "supervise")); st.Mode().Perm() != 0o755 {
		t.Fatalf("supervise mode %o", st.Mode().Perm())
	}
	st, err := os.Stat(filepath.Join(svc, "supervise", "control"))
	if err != nil || st.Mode()&os.ModeNamedPipe == 0 || st.Mode().Perm() != 0o660 {
		t.Fatalf("control fifo %v %v", st, err)
	}
	for _, rel := range []string{"event", "supervise/event"} {
		var raw syscall.Stat_t
		if err := syscall.Stat(filepath.Join(svc, rel), &raw); err != nil {
			t.Fatal(err)
		}
		if mode := raw.Mode & 0o7777; mode != 0o3730 {
			t.Fatalf("%s mode %o want 3730", rel, mode)
		}
	}
}

func TestRenderRunScriptUsesReplaceToTakeOverStaleHolder(t *testing.T) {
	def := renderS6RunScript("default", map[string]string{})
	if !strings.Contains(def, "icebath gateway run --replace") || strings.Contains(def, "icebath -p default") {
		t.Fatalf("default script %q", def)
	}
	for _, text := range []string{def, renderS6RunScript("coder", map[string]string{})} {
		n := 0
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, "gateway run") {
				n++
				if !strings.Contains(line, "--replace") {
					t.Fatalf("line lacks --replace: %q", line)
				}
			}
		}
		if n == 0 {
			t.Fatalf("no gateway run line in %q", text)
		}
	}
}

func TestRenderFinishScriptDoesNotRestartOnCleanExit(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("needs sh")
	}
	script := filepath.Join(t.TempDir(), "finish")
	if err := os.WriteFile(script, []byte(renderS6FinishScript()), 0o755); err != nil {
		t.Fatal(err)
	}
	for code, want := range map[string]int{"0": 125, "78": 125, "1": 0, "137": 0} {
		err := exec.Command("sh", script, code).Run()
		got := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			got = ee.ExitCode()
		}
		if got != want {
			t.Errorf("finish %s exited %d want %d", code, got, want)
		}
	}
}

func TestS6StopWritesPlannedStopMarkerForSupervisedPid(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "gateway-coder"), 0o755)
	f := installFakeS6(t)
	marked := recordMarkers(t, true)
	_ = NewS6ServiceManager(dir).Stop("gateway-coder")
	down, svstatFirst := f.downIssuedAfterSvstat()
	if len(*marked) != 1 || (*marked)[0] != fakeS6PID || !down || !svstatFirst {
		t.Fatalf("marked %v down %v svstat first %v", *marked, down, svstatFirst)
	}
}

func TestS6StopWhenDownWritesNoMarkerButStillStops(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "gateway-coder"), 0o755)
	f := installFakeS6(t)
	f.up = false
	marked := recordMarkers(t, true)
	_ = NewS6ServiceManager(dir).Stop("gateway-coder")
	if down, _ := f.downIssuedAfterSvstat(); len(*marked) != 0 || !down {
		t.Fatalf("marked %v down %v", *marked, down)
	}
}

func TestS6StopMarkerWriteFailureStillStops(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "gateway-coder"), 0o755)
	f := installFakeS6(t)
	attempted := recordMarkers(t, false)
	_ = NewS6ServiceManager(dir).Stop("gateway-coder")
	if down, _ := f.downIssuedAfterSvstat(); len(*attempted) != 1 || !down {
		t.Fatalf("attempted %v down %v", *attempted, down)
	}
}

func TestS6SupervisedPidReadsUpPid(t *testing.T) {
	f := installFakeS6(t)
	m := NewS6ServiceManager(t.TempDir())
	if got := m.supervisedPID("gateway-coder"); got != fakeS6PID {
		t.Fatalf("pid %d", got)
	}
	f.up = false
	if got := m.supervisedPID("gateway-coder"); got != 0 {
		t.Fatalf("down pid %d", got)
	}
}

func TestS6SupervisedPidNoneOnUnusableSvstat(t *testing.T) {
	cases := []struct {
		res *s6Result
		err error
	}{
		{res: &s6Result{ReturnCode: 1, Stdout: "s6-svstat: fatal: unable to read status\n"}},
		{res: &s6Result{Stdout: "garbage\n"}},
		{res: &s6Result{Stdout: ""}},
		{res: &s6Result{Stdout: "4242 17\n"}},
		{res: &s6Result{Stdout: "0\n"}},
		{err: &os.PathError{Op: "fork/exec", Path: "/command/s6-svstat", Err: os.ErrNotExist}},
		{err: errors.New("Command '/command/s6-svstat' timed out after 5.0 seconds")},
	}
	for i, c := range cases {
		f := installFakeS6(t)
		f.override, f.err = c.res, c.err
		if got := NewS6ServiceManager(t.TempDir()).supervisedPID("gateway-coder"); got != 0 {
			t.Errorf("case %d pid %d", i, got)
		}
	}
}

func TestS6IsRunningFollowsSvstatUpField(t *testing.T) {
	f := installFakeS6(t)
	m := NewS6ServiceManager(t.TempDir())
	if ok, _ := m.IsRunning("gateway-coder"); !ok {
		t.Fatal("up service not running")
	}
	f.up = false
	if ok, _ := m.IsRunning("gateway-coder"); ok {
		t.Fatal("down service running")
	}
	f.override = &s6Result{ReturnCode: 1}
	if ok, _ := m.IsRunning("gateway-coder"); ok {
		t.Fatal("failed svstat running")
	}
}

// Setup half of a rendered log run script before the s6-log exec
func logRunSetupFragment(rendered string) string {
	var keep strings.Builder
	for _, line := range strings.SplitAfter(rendered, "\n") {
		if strings.HasPrefix(line, "#!/") || strings.Contains(line, "shellcheck") {
			continue
		}
		if strings.Contains(line, "s6-log") {
			break
		}
		keep.WriteString(line)
	}
	return "#!/bin/sh\n" + keep.String()
}

func TestS6LogRunCreatesLeafAsIcebathWithoutChown(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	installFakeS6(t)
	dir := t.TempDir()
	if err := NewS6ServiceManager(dir).RegisterProfileGateway("coder", false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "gateway-coder", "log", "run"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "chown ") {
			t.Fatalf("log/run invokes chown: %q", text)
		}
	}
	mk, rm, ex := strings.Index(text, `s6-setuidgid icebath mkdir -p "$log_dir"`), strings.Index(text, `s6-setuidgid icebath rm -f "$log_dir/lock"`), strings.Index(text, "s6-log 1 ")
	if mk < 0 || rm < 0 || ex < 0 || !(mk < rm && rm < ex) {
		t.Fatalf("ordering %d %d %d in %q", mk, rm, ex, text)
	}
	if !strings.Contains(text, "else\n  mkdir -p \"$log_dir\"\n  rm -f \"$log_dir/lock\"\nfi\n") {
		t.Fatalf("unprivileged branch missing: %q", text)
	}
	after := strings.SplitN(text, "fi\n", 2)[1]
	if strings.Contains(after, `rm -f "$log_dir/lock"`) || strings.Contains(text, `/opt/data/logs/gateways"`) {
		t.Fatalf("lock removal or hardcoded path after the branch: %q", text)
	}
}

func TestS6LogRunNeverInvokesChownWithSymlinkedLogDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink and /bin/sh required")
	}
	tmp := t.TempDir()
	h := filepath.Join(tmp, "icebath")
	leaf := filepath.Join(h, "logs", "gateways", "coder")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(tmp, "victim")
	os.Mkdir(victim, 0o755)
	os.WriteFile(filepath.Join(victim, "marker"), []byte("keep"), 0o644)
	os.WriteFile(filepath.Join(victim, "lock"), []byte("keep-lock"), 0o644)
	before, _ := os.Stat(victim)
	bin := filepath.Join(tmp, "bin")
	os.Mkdir(bin, 0o755)
	recorder := filepath.Join(tmp, "chown_calls.txt")
	scripts := map[string]string{
		"chown":        "#!/bin/sh\nprintf \"%s\\n\" \"$*\" >> \"" + recorder + "\"\nexit 0\n",
		"id":           "#!/bin/sh\nif [ \"$1\" = \"-u\" ]; then echo 0; exit 0; fi\nexit 1\n",
		"s6-setuidgid": "#!/bin/sh\nshift\nICEBATH_TEST_DROPPED=1 exec \"$@\"\n",
		"rm":           "#!/bin/sh\nif [ -n \"$ICEBATH_TEST_DROPPED\" ]; then\n  exit 0\nfi\nexec /bin/rm \"$@\"\n",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(tmp, "log_run_setup.sh")
	os.WriteFile(script, []byte(logRunSetupFragment(renderS6LogRun("coder"))), 0o755)
	stop := make(chan struct{})
	done := make(chan struct{})
	clear := func() {
		if st, err := os.Lstat(leaf); err == nil {
			if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
				os.Remove(leaf)
			} else {
				os.Remove(leaf)
			}
		}
	}
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			clear()
			if os.Symlink(victim, leaf) == nil {
				time.Sleep(time.Millisecond)
				clear()
				os.Mkdir(leaf, 0o755)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	env := append(os.Environ(), "ICEBATH_HOME="+h, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for i := 0; i < 40; i++ {
		cmd := exec.Command("/bin/sh", script)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			close(stop)
			<-done
			t.Fatalf("setup failed %v %s", err, out)
		}
		if st, err := os.Stat(filepath.Join(victim, "lock")); err != nil || !st.Mode().IsRegular() {
			close(stop)
			<-done
			t.Fatal("symlinked leaf let root unlink victim/lock")
		}
	}
	close(stop)
	<-done
	if raw, err := os.ReadFile(recorder); err == nil && strings.TrimSpace(string(raw)) != "" {
		t.Fatalf("chown invoked: %s", raw)
	}
	after, _ := os.Stat(victim)
	if after.Sys().(*syscall.Stat_t).Uid != before.Sys().(*syscall.Stat_t).Uid {
		t.Fatal("victim owner changed")
	}
	if m, _ := os.ReadFile(filepath.Join(victim, "marker")); string(m) != "keep" {
		t.Fatal("marker changed")
	}
	if l, _ := os.ReadFile(filepath.Join(victim, "lock")); string(l) != "keep-lock" {
		t.Fatal("lock changed")
	}
}

func TestServiceManagerHooksInstalledForProfiles(t *testing.T) {
	h := profiles.GatewayServiceHooks
	if h.ServiceManagerDetect == nil || h.ServiceManagerGet == nil {
		t.Fatal("pkg/gatewaycli must install the service manager hooks")
	}
	root := t.TempDir()
	comm := filepath.Join(root, "comm")
	base := filepath.Join(root, "basedir")
	os.WriteFile(comm, []byte("s6-svscan\n"), 0o644)
	os.Mkdir(base, 0o755)
	oldP, oldB := s6ProcRoot, s6BaseDir
	t.Cleanup(func() { s6ProcRoot, s6BaseDir = oldP, oldB })
	s6ProcRoot, s6BaseDir = comm, base
	if kind, err := h.ServiceManagerDetect(); err != nil || kind != "s6" {
		t.Fatalf("detect %q %v", kind, err)
	}
	mgr, err := h.ServiceManagerGet()
	if err != nil || mgr == nil || !mgr.SupportsRuntimeRegistration() {
		t.Fatalf("get %v %v", mgr, err)
	}
	if s6, ok := mgr.(*S6ServiceManager); !ok || s6.Scandir != S6DynamicScandir {
		t.Fatalf("manager %#v", mgr)
	}
}
