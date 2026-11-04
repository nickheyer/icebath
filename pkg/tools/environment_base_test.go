package tools

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Tool env rooted in a fresh temp home with the adapter secret hooks installed
func newTestToolEnv(t *testing.T) *Env {
	t.Helper()
	h := t.TempDir()
	installTestEnvHooks(t)
	return &Env{Home: h, Secrets: config.NewSecrets(h), Values: &sync.Map{}}
}

// Installs the hook vars other packages normally provide, restoring them after the test
func installTestEnvHooks(t *testing.T) {
	t.Helper()
	prevB, prevC, prevR, prevP := BundledPlatformSecretEnvs, ConfigEnvTableKeys, RegisteredAdapterSecretEnv, ProfileSecrets
	if BundledPlatformSecretEnvs == nil {
		BundledPlatformSecretEnvs = func() []string { return nil }
	}
	if ConfigEnvTableKeys == nil {
		ConfigEnvTableKeys = func() []string { return nil }
	}
	if RegisteredAdapterSecretEnv == nil {
		RegisteredAdapterSecretEnv = func(env *Env) map[string]bool { return map[string]bool{} }
	}
	if ProfileSecrets == nil {
		ProfileSecrets = testProfileSecrets{}
	}
	t.Cleanup(func() {
		BundledPlatformSecretEnvs, ConfigEnvTableKeys, RegisteredAdapterSecretEnv, ProfileSecrets = prevB, prevC, prevR, prevP
	})
}

// Secret source double with no managed keys, sources or bound scope
type testProfileSecrets struct{}

func (testProfileSecrets) ManagedDotenvKeys() []string   { return nil }
func (testProfileSecrets) SourceSuppliedNames() []string { return nil }
func (testProfileSecrets) BuildProfileSecretScope(profileHome string) map[string]string {
	return config.LoadEnvFile(filepath.Join(profileHome, ".env"))
}
func (testProfileSecrets) CurrentSecretScope(ctx context.Context) map[string]string { return nil }

// Concrete environment for base class tests with an injectable run hook
type stubEnv struct {
	baseEnv
	run func(ctx context.Context, cmd string, login bool, timeout int, stdin *string) (ProcessHandle, error)
}

func (s *stubEnv) EnvType() string          { return "stub" }
func (s *stubEnv) Cleanup(force bool) error { return nil }

func (s *stubEnv) runBash(ctx context.Context, cmd string, login bool, timeout int, stdin *string) (ProcessHandle, error) {
	if s.run == nil {
		return nil, errors.New("Use mock")
	}
	return s.run(ctx, cmd, login, timeout, stdin)
}

// Stub environment with the sudo probe supported and no snapshot taken
func newStubEnv(t *testing.T, env *Env) *stubEnv {
	t.Helper()
	s := &stubEnv{}
	s.passwordlessSudoProbeSupported = true
	initBaseEnv(&s.baseEnv, s, env, "/tmp", 10, nil, true)
	return s
}

// Finished process handle serving canned output through a real pipe
type fakeProc struct {
	code   int
	stdout *os.File
}

func newFakeProc(t *testing.T, output string, code int) *fakeProc {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString(output)
	w.Close()
	return &fakeProc{code: code, stdout: r}
}

func (f *fakeProc) Poll() (int, bool)                        { return f.code, true }
func (f *fakeProc) Kill() error                              { return nil }
func (f *fakeProc) Wait(timeoutSeconds float64) (int, error) { return f.code, nil }
func (f *fakeProc) Stdout() *os.File                         { return f.stdout }
func (f *fakeProc) Pid() int                                 { return 4242 }

func requireBash(t *testing.T) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash required")
	}
	return bash
}

func TestSnapshotStoreReadsDictAndToleratesMissingOrDamagedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshots.json")
	if got := envLoadJSONStore(path); len(got) != 0 {
		t.Fatalf("missing store should be empty, got %v", got)
	}
	os.WriteFile(path, []byte(`{"task": "snapshot"}`), 0o644)
	if got := envLoadJSONStore(path); got["task"] != "snapshot" {
		t.Fatalf("got %v", got)
	}
	os.WriteFile(path, []byte(`["not a mapping"]`), 0o644)
	if got := envLoadJSONStore(path); len(got) != 0 {
		t.Fatalf("list store should read empty, got %v", got)
	}
	os.WriteFile(path, []byte(`{bad json`), 0o644)
	if got := envLoadJSONStore(path); len(got) != 0 {
		t.Fatalf("damaged store should read empty, got %v", got)
	}
}

func TestUpdateJSONStoreIsLockedAndAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store", "snapshots.json")
	if err := envUpdateJSONStore(path, func(m map[string]any) { m["a"] = "1" }); err != nil {
		t.Fatal(err)
	}
	if err := envUpdateJSONStore(path, func(m map[string]any) { m["b"] = "2" }); err != nil {
		t.Fatal(err)
	}
	got := envLoadJSONStore(path)
	if got["a"] != "1" || got["b"] != "2" {
		t.Fatalf("got %v", got)
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("lock file missing: %v", err)
	}
}

func TestPrepareCommandUsesSelectedEnvironmentForNopasswd(t *testing.T) {
	env := newTestToolEnv(t)
	env.Values.Store("session.interactive", true)
	s := newStubEnv(t, env)
	s.run = func(ctx context.Context, cmd string, login bool, timeout int, stdin *string) (ProcessHandle, error) {
		if cmd != "sudo -n true" {
			t.Fatalf("unexpected command %q", cmd)
		}
		return newFakeProc(t, "", 0), nil
	}
	cmd, stdin := s.prepareCommand(context.Background(), "sudo true")
	if cmd != "sudo true" || stdin != nil {
		t.Fatalf("NOPASSWD sudo must stay untouched, got %q %v", cmd, stdin)
	}
}

func TestNopasswdProbeRunsSudoNInsideBackendOnlyWhenSupported(t *testing.T) {
	cases := []struct {
		supported bool
		code      int
		expected  bool
		probed    bool
	}{{true, 0, true, true}, {true, 1, false, true}, {false, 0, false, false}}
	for _, c := range cases {
		env := newTestToolEnv(t)
		s := newStubEnv(t, env)
		s.passwordlessSudoProbeSupported = c.supported
		called := false
		s.run = func(ctx context.Context, cmd string, login bool, timeout int, stdin *string) (ProcessHandle, error) {
			called = true
			if cmd != "sudo -n true" {
				t.Fatalf("probe ran %q", cmd)
			}
			return newFakeProc(t, "", c.code), nil
		}
		if got := s.sudoNopasswdWorks(context.Background()); got != c.expected {
			t.Fatalf("supported=%v code=%d got %v", c.supported, c.code, got)
		}
		if called != c.probed {
			t.Fatalf("supported=%v probed=%v want %v", c.supported, called, c.probed)
		}
	}
}

func TestBoundedOutputCollectorRetainsHeadAndTail(t *testing.T) {
	c := newBoundedOutputCollector(1000, "")
	c.append("HEAD-SENTINEL\n")
	for range 2000 {
		c.append(strings.Repeat("x", 4096))
	}
	c.append("\nTAIL-SENTINEL")
	rendered := c.render("")
	if c.total() <= 8_000_000 {
		t.Fatalf("total %d", c.total())
	}
	if c.bufferedChars() > 1000 || len(rendered) > 1000 {
		t.Fatalf("buffered %d rendered %d", c.bufferedChars(), len(rendered))
	}
	if !strings.HasPrefix(rendered, "HEAD-SENTINEL") || !strings.HasSuffix(rendered, "TAIL-SENTINEL") {
		t.Fatalf("rendered %q", rendered)
	}
	if !strings.Contains(rendered, "[OUTPUT TRUNCATED") {
		t.Fatalf("notice missing in %q", rendered)
	}
}

func TestBoundedOutputCollectorSuffixStaysInsideLimit(t *testing.T) {
	c := newBoundedOutputCollector(120, "")
	c.append(strings.Repeat("A", 10_000))
	rendered := c.render("\n[Command timed out after 1s]")
	if len(rendered) > 120 || !strings.HasSuffix(rendered, "[Command timed out after 1s]") || !strings.Contains(rendered, "[OUTPUT TRUNCATED") {
		t.Fatalf("rendered %q", rendered)
	}
}

func TestBoundedOutputCollectorSpillsFullStream(t *testing.T) {
	spill := filepath.Join(t.TempDir(), "spill", "out.log")
	c := newBoundedOutputCollector(50, spill)
	c.append("0123456789")
	c.append(strings.Repeat("b", 100))
	c.append("END")
	path := c.closeSpill()
	if path != spill {
		t.Fatalf("spill path %q", path)
	}
	data, err := os.ReadFile(spill)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "0123456789"+strings.Repeat("b", 100)+"END" {
		t.Fatalf("spill holds %q", data)
	}
	st, _ := os.Stat(spill)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("spill mode %v", st.Mode().Perm())
	}
}

func TestWrapCommandUsesAtomicTempThenMv(t *testing.T) {
	s := newStubEnv(t, newTestToolEnv(t))
	s.snapshotReady = true
	wrapped := s.wrapCommand("echo hi", "/tmp")
	if !strings.Contains(wrapped, "export -p") || !strings.Contains(wrapped, "> ") || !strings.Contains(wrapped, ".tmp.") || !strings.Contains(wrapped, "mv -f ") {
		t.Fatalf("wrapped %q", wrapped)
	}
	snap := s.snapshotPath
	for _, bad := range []string{"> " + snap + " ", "> '" + snap + "'", "> " + snap + "\n"} {
		if strings.Contains(wrapped, bad) {
			t.Fatalf("live snapshot written in place: %q", bad)
		}
	}
	if !strings.Contains(wrapped, "mktemp ") || !strings.Contains(wrapped, ".tmp.XXXXXXXXXX") || strings.Contains(wrapped, "$BASHPID") || strings.Contains(wrapped, ".tmp.$$") {
		t.Fatalf("temp allocation must use mktemp: %q", wrapped)
	}
}

func TestInitSessionBootstrapAlsoAtomicAndMktemp(t *testing.T) {
	s := newStubEnv(t, newTestToolEnv(t))
	captured := ""
	s.run = func(ctx context.Context, cmd string, login bool, timeout int, stdin *string) (ProcessHandle, error) {
		if captured == "" {
			captured = cmd
		}
		return nil, errors.New("stop after capture")
	}
	s.initSession(context.Background())
	if !strings.Contains(captured, ".tmp.") || !strings.Contains(captured, "mv -f ") || !strings.Contains(captured, "mktemp ") {
		t.Fatalf("bootstrap %q", captured)
	}
	if strings.Contains(captured, "$BASHPID") || strings.Contains(captured, ".tmp.$$") {
		t.Fatalf("bootstrap %q", captured)
	}
}

// Environment running real bash -lc for snapshot file mode and stdin tests
type execEnv struct {
	baseEnv
	bash    string
	tempDir string
}

func (e *execEnv) EnvType() string          { return "exec" }
func (e *execEnv) Cleanup(force bool) error { return nil }
func (e *execEnv) TempDir() string          { return e.tempDir }

func (e *execEnv) runBash(ctx context.Context, cmd string, login bool, timeout int, stdin *string) (ProcessHandle, error) {
	args := []string{e.bash}
	if login {
		args = append(args, "-l")
	}
	args = append(args, "-c", cmd)
	return popenBash(args, stdin, func(c *exec.Cmd) { c.Dir = e.Cwd() })
}

func newExecEnv(t *testing.T, env *Env, dir string) *execEnv {
	t.Helper()
	e := &execEnv{bash: requireBash(t), tempDir: dir}
	initBaseEnv(&e.baseEnv, e, env, dir, 10, nil, true)
	return e
}

func TestSnapshotAndCwdFilesArePrivate(t *testing.T) {
	if isWindowsHost {
		t.Skip("POSIX file modes")
	}
	dir := t.TempDir()
	e := newExecEnv(t, newTestToolEnv(t), dir)
	e.initSession(context.Background())
	if !e.SnapshotReady() {
		t.Fatal("snapshot not ready")
	}
	userFile := filepath.Join(dir, "user-created.txt")
	if _, err := e.Execute(context.Background(), "touch "+shQuote(userFile), ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(e.snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode %v", st.Mode().Perm())
	}
	if _, err := os.Stat(e.cwdFile); err == nil {
		t.Fatal("cwd file must not be written")
	}
}

func TestExtractCwdFromOutputHappyPath(t *testing.T) {
	s := newStubEnv(t, newTestToolEnv(t))
	marker := s.cwdMarker
	res := &ExecResult{Output: "hello\n" + marker + "/home/user" + marker + "\n"}
	s.extractCwdFromOutput(res)
	if s.Cwd() != "/home/user" || strings.Contains(res.Output, marker) || !res.CwdObserved || res.Cwd != "/home/user" {
		t.Fatalf("cwd %q output %q observed %v", s.Cwd(), res.Output, res.CwdObserved)
	}
}

func TestEmbedStdinHeredocUniqueDelimiter(t *testing.T) {
	r1 := embedStdinHeredoc("cat", "data")
	r2 := embedStdinHeredoc("cat", "data")
	if strings.Split(r1, "'")[1] == strings.Split(r2, "'")[1] {
		t.Fatal("delimiters must differ")
	}
}

func TestEmbedStdinHeredocCompoundCommandReceivesStdinAsGroup(t *testing.T) {
	bash := requireBash(t)
	command := embedStdinHeredoc(`IFS= read -r first; IFS= read -r second; printf '<%s|%s>' "$first" "$second"`, "alpha\nbeta")
	out, err := exec.Command(bash, "-c", command).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "<alpha|beta>" {
		t.Fatalf("got %q", out)
	}
}

func TestInitSessionSnapshotReadyFalseOnFailure(t *testing.T) {
	s := newStubEnv(t, newTestToolEnv(t))
	s.run = func(ctx context.Context, cmd string, login bool, timeout int, stdin *string) (ProcessHandle, error) {
		return nil, errors.New("bash not found")
	}
	s.initSession(context.Background())
	if s.SnapshotReady() {
		t.Fatal("snapshot must not be ready")
	}
}

func TestInitSessionPrefersNonloginWhenLoginBashIsDead(t *testing.T) {
	s := newStubEnv(t, newTestToolEnv(t))
	s.run = func(ctx context.Context, cmd string, login bool, timeout int, stdin *string) (ProcessHandle, error) {
		if login {
			return newFakeProc(t, "", 1), nil
		}
		return newFakeProc(t, "", 0), nil
	}
	s.initSession(context.Background())
	if s.SnapshotReady() || !s.PreferNonlogin() {
		t.Fatalf("ready=%v nonlogin=%v", s.SnapshotReady(), s.PreferNonlogin())
	}
	var logins []bool
	s.run = func(ctx context.Context, cmd string, login bool, timeout int, stdin *string) (ProcessHandle, error) {
		logins = append(logins, login)
		return newFakeProc(t, "", 0), nil
	}
	if _, err := s.Execute(context.Background(), "echo test", ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(logins) == 0 || logins[0] {
		t.Fatalf("execute must run non login bash, got %v", logins)
	}
}

func TestCwdMarkerUniquePerInstance(t *testing.T) {
	env := newTestToolEnv(t)
	if newStubEnv(t, env).cwdMarker == newStubEnv(t, env).cwdMarker {
		t.Fatal("markers must differ")
	}
}

func TestSanitizeTaskIDForPath(t *testing.T) {
	out := SanitizeTaskIDForPath("session:agent:main:telegram:dm:12345")
	if strings.ContainsAny(out, ":/\\") {
		t.Fatalf("unsafe chars in %q", out)
	}
	for _, v := range []string{"default", "task-01.abc_def", "astropy__astropy-12907"} {
		if SanitizeTaskIDForPath(v) != v {
			t.Fatalf("%q must pass through", v)
		}
	}
	if SanitizeTaskIDForPath("a:b") != SanitizeTaskIDForPath("a:b") || SanitizeTaskIDForPath("a:b") == SanitizeTaskIDForPath("a_b") || SanitizeTaskIDForPath("!!!") == SanitizeTaskIDForPath("@@@") {
		t.Fatal("digest must disambiguate")
	}
	if SanitizeTaskIDForPath("") != "default" {
		t.Fatal("empty id")
	}
	for _, v := range []string{".", "..", "../../etc", `..\..\escape`} {
		out := SanitizeTaskIDForPath(v)
		if out == "." || out == ".." || strings.ContainsAny(out, "/\\") {
			t.Fatalf("%q -> %q", v, out)
		}
	}
	a := SanitizeTaskIDForPath(strings.Repeat("a", 300) + ":1")
	b := SanitizeTaskIDForPath(strings.Repeat("a", 300) + ":2")
	if len(a) > sandboxDirMaxLen || strings.Contains(a, ":") || a == b {
		t.Fatalf("%q %q", a, b)
	}
	target := filepath.Join(t.TempDir(), "docker", SanitizeTaskIDForPath("session:agent:main:telegram:dm:12345"))
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestThreadedProcessHandle(t *testing.T) {
	h, err := NewThreadedProcessHandle(func() (string, int) { return "hello world", 0 }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := h.Wait(5); err != nil || code != 0 {
		t.Fatalf("wait %d %v", code, err)
	}
	data, _ := readAllFile(h.Stdout())
	if !strings.Contains(data, "hello world") {
		t.Fatalf("stdout %q", data)
	}

	release := make(chan struct{})
	h2, _ := NewThreadedProcessHandle(func() (string, int) { <-release; return "done", 0 }, nil)
	if _, done := h2.Poll(); done {
		t.Fatal("poll must report running")
	}
	close(release)
	h2.Wait(5)
	if code, done := h2.Poll(); !done || code != 0 {
		t.Fatalf("poll after wait %d %v", code, done)
	}

	called := make(chan struct{}, 1)
	h3, _ := NewThreadedProcessHandle(func() (string, int) { time.Sleep(200 * time.Millisecond); return "", 0 }, func() { called <- struct{}{} })
	h3.Kill()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("cancel not invoked")
	}

	h4, _ := NewThreadedProcessHandle(func() (string, int) { return "ok", 0 }, func() { panic("cancel failed") })
	h4.Kill()
	h4.Wait(5)

	h5, _ := NewThreadedProcessHandle(func() (string, int) { return "line1\nline2\nline3\n", 0 }, nil)
	h5.Wait(5)
	data, _ = readAllFile(h5.Stdout())
	lines := strings.SplitAfter(data, "\n")
	if len(lines) < 3 || lines[0] != "line1\n" {
		t.Fatalf("lines %q", lines)
	}

	h6, _ := NewThreadedProcessHandle(func() (string, int) { return "hello 世界 🌍\n", 0 }, nil)
	h6.Wait(5)
	data, _ = readAllFile(h6.Stdout())
	if !strings.Contains(data, "世界") || !strings.Contains(data, "🌍") {
		t.Fatalf("unicode lost: %q", data)
	}
}

func readAllFile(f *os.File) (string, error) {
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String(), nil
		}
	}
}

func TestDrainStopsAfterExitWhenGrandchildHoldsPipe(t *testing.T) {
	bash := requireBash(t)
	p, err := popenBash([]string{bash, "-c", "sleep 3 & echo started"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := newBoundedOutputCollector(unboundedCaptureChars, "")
	start := time.Now()
	d := startDrainThread(p, c, nil)
	d.join(5 * time.Second)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("drain hung for %s", time.Since(start))
	}
	if !strings.Contains(c.render(""), "started") {
		t.Fatalf("output %q", c.render(""))
	}
	KillProcessTree(p.Pid())
}

func TestUTF8DecoderAcrossChunkBoundary(t *testing.T) {
	d := &utf8Decoder{}
	enc := []byte("日")
	out := d.decode(enc[:1]) + d.decode(enc[1:]) + d.decode([]byte{0xff, 0xfe, 'x'}) + d.finish()
	if out != "日��x" {
		t.Fatalf("got %q", out)
	}
	d2 := &utf8Decoder{}
	if got := d2.decode(enc[:2]) + d2.finish(); got != "��" {
		t.Fatalf("dangling sequence %q", got)
	}
}

// Pipeline status wrapper tests

const testCwdMarker = "__ICEBATH_CWD_test__"
const testPipeMarker = "__ICEBATH_PIPE_test__"

func pipelineScript(command, pipeMarker string) string {
	return wrapCommandScript(command, wrapScriptArgs{
		snapshotScriptArgs: snapshotScriptArgs{quotedCwd: "/tmp", quotedSnap: "/nonexistent-snapshot", snapTmpTemplate: "/tmp/icebath-snap.XXXXXX", cwdMarker: testCwdMarker},
		snapshotReady:      false,
		pipeMarker:         pipeMarker,
		persistEnv:         true,
	})
}

func runPipelineScript(t *testing.T, command string) (int, string, string) {
	t.Helper()
	bash := requireBash(t)
	cmd := exec.Command(bash, "-c", pipelineScript(command, testPipeMarker))
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, stdout.String(), stderr.String()
}

func TestPipelineStatusCapturesTheLeadingCommand(t *testing.T) {
	_, out, _ := runPipelineScript(t, "false | true")
	if !strings.Contains(out, testPipeMarker+"1 0"+testPipeMarker) {
		t.Fatalf("output %q", out)
	}
	_, out, _ = runPipelineScript(t, "false")
	if !strings.Contains(out, testPipeMarker+"1"+testPipeMarker) {
		t.Fatalf("output %q", out)
	}
}

func TestCapturingPipestatusChangesNothingTheModelSees(t *testing.T) {
	cases := []struct {
		command string
		code    int
		stdout  string
	}{
		{"if false; then echo yes; fi", 0, ""},
		{"while read line; do echo \"$line\"; done < /dev/null", 0, ""},
		{"set -o pipefail; false | true", 1, ""},
		{"! false", 0, ""},
		{"! true", 1, ""},
		{"echo 'unbalanced", 2, ""},
		{`echo "unbalanced`, 2, ""},
		{"echo $(echo unclosed", 2, ""},
		{"echo hi \\", 0, "hi \\\n"},
		{"cat <<'EOF'\nunterminated heredoc", 0, "unterminated heredoc\n"},
		{"exit 3", 3, ""},
		{"trap 'echo T' ERR; false", 1, "T\n"},
	}
	for _, c := range cases {
		code, out, stderr := runPipelineScript(t, c.command)
		for _, marker := range []string{testPipeMarker, testCwdMarker} {
			if _, cleaned, ok := splitCwdMarker(out, marker); ok {
				out = cleaned
			}
		}
		if code != c.code || out != c.stdout {
			t.Fatalf("%q: got (%d, %q) want (%d, %q)", c.command, code, out, c.code, c.stdout)
		}
		if strings.Contains(stderr, "__icebath") {
			t.Fatalf("%q: wrapper text in stderr %q", c.command, stderr)
		}
	}
}

func TestExtractPipelineStatusIntoResult(t *testing.T) {
	s := newStubEnv(t, newTestToolEnv(t))
	res := &ExecResult{Output: "x\n" + s.pipeMarker + "1 0" + s.pipeMarker + "\n" + s.cwdMarker + "/tmp" + s.cwdMarker + "\n"}
	s.extractCwdFromOutput(res)
	if len(res.PipelineStatus) != 2 || res.PipelineStatus[0] != 1 || res.PipelineStatus[1] != 0 || res.Output != "x" {
		t.Fatalf("%+v", res)
	}
}

// Snapshot session leak tests

func TestSnapshotExclusionRegexMatchesBridgedSessionVars(t *testing.T) {
	rx := regexp.MustCompile(snapshotExcludedEnvRegex)
	for _, name := range append(append([]string{}, SessionContextEnvVars...), delegatedChildEnvMarker) {
		if !rx.MatchString(`declare -x ` + name + `="whatever"`) {
			t.Fatalf("%s should be excluded from the snapshot", name)
		}
	}
}

func TestExportDumpDropsEveryBridgedVarAndTheDelegationMarker(t *testing.T) {
	bash := requireBash(t)
	scoped := append(append([]string{}, SessionContextEnvVars...), delegatedChildEnvMarker)
	var exports []string
	for _, n := range scoped {
		exports = append(exports, `export `+n+`="x"`)
	}
	exports = append(exports, `export ICEBATH_HOME="/h"`, `export MYVAR="keep"`)
	script := strings.Join(exports, "; ") + "; " + exportDumpExcludingSessionVars("/dev/stdout", nil)
	out, err := exec.Command(bash, "-c", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range scoped {
		if strings.Contains(string(out), "declare -x "+n+"=") {
			t.Fatalf("%s persisted into the snapshot", n)
		}
	}
	if !strings.Contains(string(out), `declare -x ICEBATH_HOME="/h"`) || !strings.Contains(string(out), `declare -x MYVAR="keep"`) {
		t.Fatalf("ordinary exports missing: %q", out)
	}
}

func TestExportDumpExcludesExtraNamesQuoted(t *testing.T) {
	dump := exportDumpExcludingSessionVars("/dev/stdout", []string{"ZED", "ALPHA", "", "ALPHA"})
	if !strings.Contains(dump, "ICEBATH_UI_SESSION_ID ALPHA ZED 2>/dev/null") {
		t.Fatalf("dump %q", dump)
	}
}

func TestMultilineSessionValueNotExecutedViaSnapshot(t *testing.T) {
	bash := requireBash(t)
	for _, envName := range []string{"ICEBATH_SESSION_CHAT_NAME", "ICEBATH_SESSION_USER_NAME"} {
		dir := t.TempDir()
		marker := filepath.Join(dir, "pwned")
		snap := filepath.Join(dir, "snap")
		script := "set -e\nexport " + envName + "\n" + exportDumpExcludingSessionVars(shQuote(snap), nil) + "\n" +
			"if grep -qE 'pwned|touch |" + envName + "' " + shQuote(snap) + "; then echo LEAKED_INTO_SNAPSHOT >&2; exit 2; fi\n" +
			"bash -c 'source " + snap + " >/dev/null 2>&1 || true'\n" +
			"if [ -e " + shQuote(marker) + " ]; then echo PAYLOAD_EXECUTED >&2; exit 3; fi\n" +
			"if ! grep -qE '^declare -x PATH=' " + shQuote(snap) + "; then echo PATH_MISSING >&2; exit 4; fi\n"
		cmd := exec.Command(bash, "-c", script)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), envName+"=demo\ntouch "+marker+" #")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %q", envName, err, out)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s payload executed", envName)
		}
	}
}

func TestWrapCommandRestoresPassthroughAndSkipsRedumpWithoutPersistence(t *testing.T) {
	s := newStubEnv(t, newTestToolEnv(t))
	s.snapshotReady = true
	s.persistEnv = false
	s.snapshotPassthroughNames["MY_TOKEN"] = true
	s.profileScopedPassthrough = true
	wrapped := s.wrapCommand("echo hi", "/tmp")
	if !strings.Contains(wrapped, "_ICEBATH_RUNTIME_PASSTHROUGH_MY_TOKEN_PRESENT=${MY_TOKEN+x}") {
		t.Fatalf("save line missing: %q", wrapped)
	}
	if strings.Contains(wrapped, "export -p") {
		t.Fatalf("re dump must be skipped without persistence: %q", wrapped)
	}
	if !strings.Contains(wrapped, cwdMarkerPrintf(s.cwdMarker)) {
		t.Fatal("cwd marker must still run")
	}
}

func TestQuoteCwdForCd(t *testing.T) {
	cases := map[string]string{"~": "~", "~/": "$HOME", "~/my dir": "$HOME/'my dir'", "/tmp/x y": "'/tmp/x y'", "/tmp": "/tmp"}
	for in, want := range cases {
		if got := quoteCwdForCd(in); got != want {
			t.Fatalf("%q -> %q want %q", in, got, want)
		}
	}
}

func TestSplitCwdMarker(t *testing.T) {
	m := "__M__"
	payload, cleaned, ok := splitCwdMarker("a\n"+m+"/x"+m+"\nb\n", m)
	if !ok || payload != "/x" || cleaned != "ab\n" {
		t.Fatalf("%q %q %v", payload, cleaned, ok)
	}
	if _, _, ok := splitCwdMarker("no marker", m); ok {
		t.Fatal("missing marker must not match")
	}
	if _, _, ok := splitCwdMarker("half "+m+" only", m); ok {
		t.Fatal("single marker must not match")
	}
}

func TestRemoteArgvExportsAndJoins(t *testing.T) {
	argv := RemoteArgv([]string{"docker", "exec", "-i", "c"}, []string{"ls", "-l", "a b"}, map[string]string{"B": "2", "A": "x y"}, false)
	want := []string{"docker", "exec", "-i", "c", "bash", "-c", "export A='x y'; export B=2; exec ls -l 'a b'"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("got %q", argv)
	}
	joined := RemoteArgv([]string{"ssh", "host"}, []string{"true"}, nil, true)
	if len(joined) != 3 || joined[2] != "bash -c ' exec true'" {
		t.Fatalf("joined %q", joined)
	}
}

// Environment double exposing a host side exec prefix for the stream tests
type prefixEnv struct {
	stubEnv
	prefix []string
}

func (p *prefixEnv) ExecPrefix(user string, interactive bool) ([]string, error) { return p.prefix, nil }

func TestExecPrefixAndStreamsSupport(t *testing.T) {
	plain := newStubEnv(t, newTestToolEnv(t))
	if SupportsStreams(plain) || ExecPrefix(plain, "", true) != nil {
		t.Fatal("backend without a prefix cannot stream")
	}
	if _, err := OpenStream(plain, []string{"true"}, nil, "", nil); err == nil || !strings.Contains(err.Error(), "cannot host a long-lived stdio stream") {
		t.Fatalf("err %v", err)
	}
	p := &prefixEnv{prefix: []string{"env"}}
	initBaseEnv(&p.baseEnv, p, newTestToolEnv(t), "/tmp", 10, nil, true)
	if !SupportsStreams(p) {
		t.Fatal("prefix backend must stream")
	}
	res, err := RunIn(context.Background(), p, []string{"sh", "-c", "cat; echo tail"}, map[string]string{"X": "1"}, "", 10*time.Second, []byte("in"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ReturnCode != 0 || string(res.Stdout) != "intail\n" {
		t.Fatalf("%d %q", res.ReturnCode, res.Stdout)
	}
}

// Port forward tests

func relayOnHost(t *testing.T) func() {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required")
	}
	prev := openStreamFn
	openStreamFn = func(e Environment, argv []string, childEnv map[string]string, user string, stderr *os.File) (*StreamProcess, error) {
		cmd := exec.Command("python3", argv[1:]...)
		stdinR, stdinW, _ := os.Pipe()
		stdoutR, stdoutW, _ := os.Pipe()
		cmd.Stdin, cmd.Stdout = stdinR, stdoutW
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		stdinR.Close()
		stdoutW.Close()
		s := &StreamProcess{Cmd: cmd, Stdin: stdinW, Stdout: stdoutR, done: make(chan struct{})}
		go func() {
			defer close(s.done)
			s.code = exitCodeOf(cmd, cmd.Wait())
		}()
		return s, nil
	}
	return func() {
		openStreamFn = prev
		portForwards.Lock()
		for k, f := range portForwards.m {
			f.Close()
			delete(portForwards.m, k)
		}
		portForwards.Unlock()
	}
}

func closedPort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func oneShotServer(t *testing.T, reply []byte, readRequest bool) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		ln.Close()
		if err != nil {
			return
		}
		if readRequest {
			buf := make([]byte, 65536)
			conn.Read(buf)
		}
		conn.Write(reply)
		conn.Close()
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func relayThrough(t *testing.T, localPort int, request []byte) []byte {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	conn.Write(request)
	conn.(*net.TCPConn).CloseWrite()
	var out []byte
	buf := make([]byte, 65536)
	for {
		n, err := conn.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return out
		}
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestOnlyAConnectionThatNeverReachesItsTargetCountsAsAForwardFailure(t *testing.T) {
	defer relayOnHost(t)()
	env := newStubEnv(t, newTestToolEnv(t))
	live := oneShotServer(t, []byte("devtools"), false)
	port, err := ForwardPort(env, live, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := relayThrough(t, port, nil); string(got) != "devtools" {
		t.Fatalf("relayed %q", got)
	}
	dead := closedPort(t)
	port, _ = ForwardPort(env, dead, "", "")
	if got := relayThrough(t, port, nil); len(got) != 0 {
		t.Fatalf("dead port relayed %q", got)
	}
	waitUntil(t, "the dead port's forward failure", func() bool { return ForwardFailures(env, dead, "") > 0 })
	if ForwardFailures(env, dead, "") != 1 || ForwardFailures(env, live, "") != 0 {
		t.Fatalf("failures dead=%d live=%d", ForwardFailures(env, dead, ""), ForwardFailures(env, live, ""))
	}
}

func TestA404OnTheOldBrowserPathCountsAsAForwardFailure(t *testing.T) {
	defer relayOnHost(t)()
	notFound := []byte("HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n")
	env := newStubEnv(t, newTestToolEnv(t))
	stale := oneShotServer(t, notFound, true)
	request := []byte("GET /devtools/browser/old-id HTTP/1.1\r\nHost: 127.0.0.1\r\nUpgrade: websocket\r\n\r\n")
	port, _ := ForwardPort(env, stale, "", "")
	if got := relayThrough(t, port, request); !strings.HasPrefix(string(got), "HTTP/1.1 404") {
		t.Fatalf("relayed %q", got)
	}
	waitUntil(t, "the stale-path 404 counted as a failure", func() bool { return ForwardFailures(env, stale, "") == 1 })
	other := oneShotServer(t, notFound, true)
	port, _ = ForwardPort(env, other, "", "")
	if got := relayThrough(t, port, []byte("GET /missing HTTP/1.1\r\n\r\n")); !strings.HasPrefix(string(got), "HTTP/1.1 404") {
		t.Fatalf("relayed %q", got)
	}
	live := oneShotServer(t, []byte("HTTP/1.1 101 Switching Protocols\r\n\r\n"), true)
	port, _ = ForwardPort(env, live, "", "")
	if got := relayThrough(t, port, request); !strings.HasPrefix(string(got), "HTTP/1.1 101") {
		t.Fatalf("relayed %q", got)
	}
	if ForwardFailures(env, other, "") != 0 || ForwardFailures(env, live, "") != 0 {
		t.Fatal("relayed replies must not count as failures")
	}
}

func TestExecuteTimesOutAndKillsTheCommand(t *testing.T) {
	dir := t.TempDir()
	e := newExecEnv(t, newTestToolEnv(t), dir)
	start := time.Now()
	res, err := e.Execute(context.Background(), "echo before; sleep 30; echo after", ExecOptions{Timeout: 1})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
	if res.ReturnCode != 124 || !res.TimedOut || !strings.Contains(res.Output, "[Command timed out after 1s]") || !strings.Contains(res.Output, "before") {
		t.Fatalf("%+v", res)
	}
	if res.CwdObserved {
		t.Fatal("a killed command emits no cwd marker")
	}
}

func TestExecuteInterruptKillsAndReports130(t *testing.T) {
	dir := t.TempDir()
	e := newExecEnv(t, newTestToolEnv(t), dir)
	worker := NewWorkerID()
	ctx := WithWorker(context.Background(), worker)
	go func() {
		time.Sleep(300 * time.Millisecond)
		SetInterrupt(worker, true, "")
	}()
	defer SetInterrupt(worker, false, "")
	res, err := e.Execute(ctx, "sleep 30", ExecOptions{Timeout: 20})
	if err != nil {
		t.Fatal(err)
	}
	if res.ReturnCode != 130 || !res.Interrupted || !strings.Contains(res.Output, "[Command interrupted]") {
		t.Fatalf("%+v", res)
	}
}

func TestExecuteYieldHandsTheProcessOver(t *testing.T) {
	dir := t.TempDir()
	e := newExecEnv(t, newTestToolEnv(t), dir)
	worker := NewWorkerID()
	ctx := WithWorker(context.Background(), worker)
	var handed ProcessHandle
	go func() {
		time.Sleep(300 * time.Millisecond)
		RequestYield(worker)
	}()
	res, err := e.Execute(ctx, "echo early; sleep 30", ExecOptions{Timeout: 20, YieldHandler: func(p ProcessHandle, soFar string) *ExecResult {
		handed = p
		return &ExecResult{Output: "yielded:" + strings.TrimSpace(soFar), ReturnCode: 0, YieldedSessionID: "bg1"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if handed == nil || res.YieldedSessionID != "bg1" || !strings.HasPrefix(res.Output, "yielded:early") {
		t.Fatalf("%+v", res)
	}
	if _, done := handed.Poll(); done {
		t.Fatal("yielded process must stay alive")
	}
	KillProcessTree(handed.Pid())
}

func TestExecuteStdinAndCwdPersistence(t *testing.T) {
	dir := t.TempDir()
	e := newExecEnv(t, newTestToolEnv(t), dir)
	e.initSession(context.Background())
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	stdin := "alpha\n"
	res, err := e.Execute(context.Background(), "cat; cd sub", ExecOptions{Stdin: &stdin})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "alpha") || !res.CwdObserved {
		t.Fatalf("%+v", res)
	}
	if real, _ := filepath.EvalSymlinks(sub); filepath.Clean(e.Cwd()) != real {
		t.Fatalf("cwd %q want %q", e.Cwd(), real)
	}
	res, _ = e.Execute(context.Background(), "pwd -P", ExecOptions{})
	if real, _ := filepath.EvalSymlinks(sub); strings.TrimSpace(res.Output) != real {
		t.Fatalf("next command ran in %q", res.Output)
	}
}

func TestFetchFileAndRealpath(t *testing.T) {
	dir := t.TempDir()
	e := newExecEnv(t, newTestToolEnv(t), dir)
	src := filepath.Join(dir, "src.bin")
	payload := []byte("hello\x00world\n" + strings.Repeat("z", 5000))
	os.WriteFile(src, payload, 0o644)
	dest := filepath.Join(dir, "dest.bin")
	if err := e.FetchFile(context.Background(), src, dest, 1<<20); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Fatal("payload mismatch")
	}
	err := e.FetchFile(context.Background(), src, dest, 100)
	var fetchErr *FileFetchError
	if !errors.As(err, &fetchErr) || !strings.Contains(err.Error(), "delivery limit") {
		t.Fatalf("err %v", err)
	}
	if err := e.FetchFile(context.Background(), filepath.Join(dir, "missing"), dest, 100); !errors.As(err, &fetchErr) {
		t.Fatalf("err %v", err)
	}
	real, _ := filepath.EvalSymlinks(src)
	if got := e.FetchRealpath(context.Background(), src); got != real {
		t.Fatalf("realpath %q want %q", got, real)
	}
	if got := e.FetchRealpath(context.Background(), filepath.Join(dir, "nope", "x")); got != "" {
		t.Fatalf("missing realpath %q", got)
	}
}

func TestKillLiveForegroundProcesses(t *testing.T) {
	dir := t.TempDir()
	e := newExecEnv(t, newTestToolEnv(t), dir)
	done := make(chan *ExecResult, 1)
	go func() {
		res, _ := e.Execute(context.Background(), "sleep 30", ExecOptions{Timeout: 20})
		done <- res
	}()
	waitUntil(t, "the foreground command to register", func() bool {
		liveForeground.mu.Lock()
		defer liveForeground.mu.Unlock()
		return len(liveForeground.procs) > 0
	})
	if n := KillLiveForegroundProcesses(false); n != 1 {
		t.Fatalf("signalled %d", n)
	}
	select {
	case res := <-done:
		if res.ReturnCode == 0 {
			t.Fatalf("killed command reported %+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("execute did not return after the kill")
	}
}

func TestEnvironmentConnectionErrorDefaultHint(t *testing.T) {
	err := NewEnvironmentConnectionError("ssh host down", "")
	if err.Error() != "ssh host down" || !strings.Contains(err.RetryHint, "infrastructure failure") {
		t.Fatalf("%+v", err)
	}
	if NewEnvironmentConnectionError("x", "custom").RetryHint != "custom" {
		t.Fatal("custom hint lost")
	}
}

func TestSandboxDirHonoursOverride(t *testing.T) {
	env := newTestToolEnv(t)
	dir, err := SandboxDir(env)
	if err != nil || dir != filepath.Join(env.Home, "sandboxes") {
		t.Fatalf("%q %v", dir, err)
	}
	custom := filepath.Join(t.TempDir(), "custom")
	t.Setenv("TERMINAL_SANDBOX_DIR", custom)
	ResetTerminalScopeCache()
	dir, err = SandboxDir(env)
	if err != nil || dir != custom {
		t.Fatalf("%q %v", dir, err)
	}
	if st, err := os.Stat(custom); err != nil || !st.IsDir() {
		t.Fatal("sandbox dir must be created")
	}
}
