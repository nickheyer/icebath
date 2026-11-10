package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

const childModeEnv = "ICEBATH_BROWSER_TEST_CHILD"

// Child processes this test binary runs as, a fake Chromium or another Icebath process using the launcher
func runChildMode(mode string) int {
	switch mode {
	case "serving-chromium":
		return runServingChromium()
	case "order-chromium":
		return runOrderChromium()
	case "order-helper":
		return runOrderHelper()
	case "never-ready":
		udd := userDataDirArg()
		os.WriteFile(filepath.Join(udd, "pid.tmp"), []byte(strconv.Itoa(os.Getpid())), 0o644)
		os.Rename(filepath.Join(udd, "pid.tmp"), filepath.Join(udd, "pid"))
		sleepForever()
	case "exit-code":
		os.Stderr.WriteString(os.Getenv("FAKE_STDERR"))
		code, _ := strconv.Atoi(os.Getenv("FAKE_EXIT"))
		return code
	case "profile-user":
		return runProfileUser()
	case "waiting-launcher":
		env := childEnv()
		fmt.Println(defaultUserDataDir("slow", cfgOf(env).homeKey()))
		needsChromiumSandboxBypass = func() bool { return false }
		launchChromium(context.Background(), env, "slow", LaunchOptions{Executable: os.Getenv("CHILD_EXE"), Ephemeral: true, Headed: boolp(false),
			ProcEnv: environ(), Timeout: durp(300 * time.Second), SandboxBypass: true})
		return 0
	case "prepare-dir":
		env := childEnv()
		udd := defaultUserDataDir("browser-exec-default", cfgOf(env).homeKey())
		prepareDir(udd, true)
		fmt.Println(udd)
		if os.Getenv("CHILD_HOLD") != "" {
			bufio.NewReader(os.Stdin).ReadString('\n')
		}
		return 0
	case "default-udd":
		fmt.Println(defaultUserDataDir(os.Getenv("CHILD_KEY"), os.Getenv("CHILD_HOME_KEY")))
		return 0
	case "owner-view":
		ns := os.Getenv("CHILD_NS")
		nsIdentity = func() (string, bool) { return ns, true }
		start, _ := strconv.ParseInt(os.Getenv("CHILD_START"), 10, 64)
		fmt.Println(len(otherLiveHolders(os.Getenv("CHILD_RECORD"), start)))
		return 0
	}
	fmt.Fprintln(os.Stderr, "unknown child mode", mode)
	return 2
}

func sleepForever() {
	for {
		time.Sleep(600 * time.Second)
	}
}

func childEnv() *tools.Env { return &tools.Env{Home: os.Getenv("ICEBATH_HOME")} }

func userDataDirArg() string {
	for _, a := range os.Args {
		if v, ok := strings.CutPrefix(a, "--user-data-dir="); ok {
			return v
		}
	}
	return ""
}

func appendLine(path, line string) {
	if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.WriteString(line + "\n")
		f.Close()
	}
}

func onTerm(fn func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	go func() { <-sigs; fn() }()
}

// browser_launcher_fakes.py, singleton lock, /json/version answer and DevToolsActivePort once ready
func runServingChromium() int {
	udd := userDataDirArg()
	os.MkdirAll(udd, 0o777)
	appendLine(filepath.Join(udd, "spawns"), strconv.Itoa(os.Getpid()))
	lock := filepath.Join(udd, "SingletonLock")
	host, _ := os.Hostname()
	mine := fmt.Sprintf("%s-%d", host, os.Getpid())
	if os.Symlink(mine, lock) != nil {
		target, _ := os.Readlink(lock)
		holder, _ := strconv.Atoi(target[strings.LastIndex(target, "-")+1:])
		if syscall.Kill(holder, 0) == nil {
			return 0
		}
		os.Remove(lock)
		os.Symlink(mine, lock)
	}
	appendLine(filepath.Join(udd, "launches"), strconv.Itoa(os.Getpid()))
	onTerm(func() { os.Remove(lock); os.Exit(0) })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 1
	}
	port := ln.Addr().(*net.TCPAddr).Port
	browserPath := fmt.Sprintf("/devtools/browser/%d", os.Getpid())
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]string{"webSocketDebuggerUrl": fmt.Sprintf("ws://127.0.0.1:%d%s", port, browserPath)})
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
	}))
	if d, err := strconv.ParseFloat(os.Getenv("FAKE_CHROMIUM_READY_DELAY"), 64); err == nil {
		time.Sleep(time.Duration(d * float64(time.Second)))
	}
	tmp := filepath.Join(udd, "DevToolsActivePort.tmp")
	os.WriteFile(tmp, []byte(fmt.Sprintf("%d\n%s\n", port, browserPath)), 0o644)
	os.Rename(tmp, filepath.Join(udd, devtoolsFile))
	sleepForever()
	return 0
}

// test_browser_launcher.py's fake, argv recorded, a helper child and a SIGTERM order log
func runOrderChromium() int {
	udd := userDataDirArg()
	os.MkdirAll(udd, 0o777)
	log := filepath.Join(udd, "order.log")
	raw, _ := json.Marshal(os.Args)
	os.WriteFile(filepath.Join(udd, "argv.json"), raw, 0o644)
	self, _ := os.Executable()
	helper := exec.Command(self)
	helper.Env = append(os.Environ(), childModeEnv+"=order-helper", "FAKE_ORDER_LOG="+log)
	helper.Start()
	onTerm(func() { appendLine(log, "parent"); os.Exit(0) })
	os.WriteFile(filepath.Join(udd, devtoolsFile), []byte("45678\n/devtools/browser/fake-id\n"), 0o644)
	sleepForever()
	return 0
}

func runOrderHelper() int {
	log := os.Getenv("FAKE_ORDER_LOG")
	onTerm(func() { appendLine(log, "child"); os.Exit(0) })
	parent := os.Getppid()
	for os.Getppid() == parent {
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(5 * time.Second)
	return 0
}

// Another Icebath process on a shared profile, holding its browser until a stdin line then exiting normally
func runProfileUser() int {
	env := childEnv()
	if ns := os.Getenv("CHILD_NS"); ns != "" {
		nsIdentity = func() (string, bool) { return ns, true }
	}
	needsChromiumSandboxBypass = func() bool { return false }
	exe, udd := os.Getenv("CHILD_EXE"), os.Getenv("CHILD_UDD")
	stdin := bufio.NewReader(os.Stdin)
	racer := os.Getenv("CHILD_RACER") != ""
	if racer {
		fmt.Println("ready")
		stdin.ReadString('\n')
	}
	if running, _ := strconv.Atoi(os.Getenv("CHILD_RUNNING")); running != 0 {
		runningInstance = func(d string) (int, string, int, bool) {
			if d == udd {
				return 45678, "/devtools/browser/fake-id", running, true
			}
			return 0, "", 0, false
		}
	}
	b, err := launchChromium(context.Background(), env, "profile:"+udd, LaunchOptions{Executable: exe, UserDataDir: udd, Headed: boolp(false),
		ProcEnv: environ(), Timeout: durp(30 * time.Second), SandboxBypass: true})
	owned := map[bool]string{true: "True", false: "False"}[b != nil && b.Owned]
	switch {
	case b != nil && racer:
		fmt.Printf("%d %s %s\n", *b.PID, owned, b.CDPURL)
	case b != nil:
		fmt.Printf("%d %s\n", *b.PID, owned)
	default:
		fmt.Printf("ERR %s\n", err)
	}
	stdin.ReadString('\n')
	stopAllBrowsers(context.Background())
	return 0
}

func boolp(v bool) *bool                  { return &v }
func durp(v time.Duration) *time.Duration { return &v }

func environ() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// A script that runs this test binary as the given fake Chromium mode
func fakeExe(t *testing.T, mode string, extra ...string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "chromium-"+mode)
	script := "#!/bin/sh\nexport " + childModeEnv + "=" + mode + "\nexport " + rootPipeEnv + "='" + os.Getenv(rootPipeEnv) +
		"'\nexport " + childLedgerEnv + "='" + childLedger(t) + "'\n"
	for _, kv := range extra {
		script += "export " + kv + "\n"
	}
	script += "exec '" + self + "' \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Shared launcher fixture, a private home and lock dir, no sandbox bypass or configured args, every browser stopped after
func launcherTestEnv(t *testing.T) *tools.Env {
	t.Helper()
	if isWindowsHost {
		t.Skip("posix fake browser")
	}
	childLedger(t)
	env := testEnv(t)
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", filepath.Join(t.TempDir(), "locks"))
	pin(t, &needsChromiumSandboxBypass, func() bool { return false })
	pin(t, &launcherChromiumArgs, func(*tools.Env) []string { return nil })
	t.Cleanup(func() {
		stopAllBrowsers(context.Background())
		launcherMu.Lock()
		servedHomes = map[string]bool{}
		autoinstallAttempted = false
		launcherMu.Unlock()
	})
	return env
}

func homeRecords(t *testing.T, env *tools.Env) string {
	t.Helper()
	path := recordsDir(env.Home)
	if err := mkdirLeaf(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func launchShared(env *tools.Env, exe, udd string) (*LaunchedBrowser, string) {
	return launchChromium(context.Background(), env, "profile:"+udd, LaunchOptions{Executable: exe, UserDataDir: udd, Headed: boolp(false),
		ProcEnv: environ(), Timeout: durp(30 * time.Second), SandboxBypass: true})
}

func launchEphemeral(env *tools.Env, key string, opts LaunchOptions) (*LaunchedBrowser, string) {
	opts.Ephemeral = true
	opts.SandboxBypass = true
	if opts.ProcEnv == nil {
		opts.ProcEnv = environ()
	}
	return launchChromium(context.Background(), env, key, opts)
}

func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func gone(t *testing.T, pid int64) {
	t.Helper()
	waitUntil(t, 15*time.Second, fmt.Sprintf("pid %d to exit", pid), func() bool { return !launcherPidAlive(int(pid)) })
}

func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// SIGKILLs pid only while it is still one of this binary's fake browsers
func killFake(pid int64) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	self, _ := os.Executable()
	if err == nil && int(pid) != os.Getpid() && strings.Contains(string(raw), self) && strings.Contains(string(raw), "--user-data-dir=") {
		syscall.Kill(int(pid), syscall.SIGKILL)
	}
}

// A process started outside the launcher whose exit a goroutine reaps
type started struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (s *started) running() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

func startProc(t *testing.T, name string, args ...string) *started {
	t.Helper()
	childLedger(t)
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &started{cmd: cmd, done: make(chan struct{})}
	go func() { cmd.Wait(); close(s.done) }()
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-s.done
	})
	return s
}

func startFake(t *testing.T, exe, udd string) *started {
	t.Helper()
	s := startProc(t, exe, "--remote-debugging-port=0", "--user-data-dir="+udd)
	waitUntil(t, 15*time.Second, "the fake to come up", func() bool { return lexists(filepath.Join(udd, devtoolsFile)) })
	return s
}

func notedPids(udd, name string) []int {
	raw, _ := os.ReadFile(filepath.Join(udd, name))
	var out []int
	for _, f := range strings.Fields(string(raw)) {
		n, _ := strconv.Atoi(f)
		out = append(out, n)
	}
	return out
}

// This test binary run as another Icebath process in a child mode, talking over stdin and stdout
type child struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Reader
	done  chan struct{}
}

func startChild(t *testing.T, mode string, env ...string) *child {
	t.Helper()
	cmd := childCommand(t, mode, env...)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &child{cmd: cmd, stdin: stdin, out: bufio.NewReader(stdout), done: make(chan struct{})}
	t.Cleanup(c.kill)
	return c
}

func (c *child) line() string {
	l, _ := c.out.ReadString('\n')
	return strings.TrimSpace(l)
}

// Releases the child with a stdin line and waits for its normal exit
func (c *child) finish(t *testing.T) {
	t.Helper()
	io.WriteString(c.stdin, "\n")
	waitErr := make(chan error, 1)
	go func() { io.Copy(io.Discard, c.out); waitErr <- c.cmd.Wait() }()
	select {
	case err := <-waitErr:
		close(c.done)
		if err != nil {
			t.Fatalf("child exited with %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("child did not exit")
	}
}

func (c *child) kill() {
	select {
	case <-c.done:
		return
	default:
	}
	syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	c.cmd.Wait()
	close(c.done)
}

// Another process holding the shared profile, returning its browser pid and whether it launched it
func profileUser(t *testing.T, exe, udd string, env ...string) (*child, int64, bool) {
	t.Helper()
	c := startChild(t, "profile-user", append([]string{"CHILD_EXE=" + exe, "CHILD_UDD=" + udd}, env...)...)
	fields := strings.Fields(c.line())
	if len(fields) != 2 {
		t.Fatalf("profile user said %q", fields)
	}
	pid, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		t.Fatalf("profile user said %q", fields)
	}
	return c, pid, fields[1] == "True"
}

func writeStateJSON(t *testing.T, path string, data map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(data)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func globNames(dir, pattern string) []string {
	paths, _ := filepath.Glob(filepath.Join(dir, pattern))
	for i, p := range paths {
		paths[i] = filepath.Base(p)
	}
	return paths
}

func childEnvFor(homeDir string) *tools.Env { return &tools.Env{Home: homeDir} }

// Records every log line at or above its level so a test can assert on what was logged
type recordingLogs struct {
	mu      sync.Mutex
	records []slog.Record
}

func (r *recordingLogs) Enabled(context.Context, slog.Level) bool { return true }
func (r *recordingLogs) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	r.records = append(r.records, rec)
	r.mu.Unlock()
	return nil
}
func (r *recordingLogs) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recordingLogs) WithGroup(string) slog.Handler      { return r }

func (r *recordingLogs) at(level slog.Level) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, rec := range r.records {
		if rec.Level >= level {
			out = append(out, rec.Message)
		}
	}
	return out
}

func (r *recordingLogs) atLeastWarn() []string { return r.at(slog.LevelWarn) }

func captureLogs(t *testing.T) *recordingLogs {
	t.Helper()
	logs := &recordingLogs{}
	old := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(old) })
	return logs
}
