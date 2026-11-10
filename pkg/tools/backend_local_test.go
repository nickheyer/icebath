package tools

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Local environment without the snapshot bootstrap, like LocalEnvironment.__new__ in the hermes tests
func newUninitLocalEnv(t *testing.T, env *Env, cwd string, childEnv map[string]string) *localEnv {
	t.Helper()
	l := &localEnv{}
	l.passwordlessSudoProbeSupported = true
	l.profileScopedPassthrough = true
	l.isLocal = true
	l.usesHostFilesystem = true
	initBaseEnv(&l.baseEnv, l, env, resolveLocalInitialCwd(cwd), 10, childEnv, true)
	return l
}

// Tool env whose logger records every line into the returned buffer
func newLoggingToolEnv(t *testing.T) (*Env, *bytes.Buffer) {
	t.Helper()
	env := newTestToolEnv(t)
	buf := &bytes.Buffer{}
	env.Log = slog.New(slog.NewTextHandler(buf, nil))
	return env, buf
}

// Run env of the backend failing the test on a scope error
func mustRunEnv(t *testing.T, env *Env) map[string]string {
	t.Helper()
	run, err := makeRunEnv(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// Sanitized env failing the test on a scope error
func mustSanitize(t *testing.T, env *Env, base, extra map[string]string) map[string]string {
	t.Helper()
	out, err := SanitizeSubprocessEnv(env, base, extra)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func clearTempVars(t *testing.T) {
	t.Helper()
	for _, v := range []string{"TERMINAL_TEMP_DIR", "TMPDIR", "TMP", "TEMP"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
}

func TestLocalTempDirOverrideHonored(t *testing.T) {
	clearTempVars(t)
	target := t.TempDir()
	l := newUninitLocalEnv(t, newTestToolEnv(t), target, map[string]string{"TERMINAL_TEMP_DIR": target})
	if got := l.TempDir(); filepath.Clean(got) != filepath.Clean(target) {
		t.Fatalf("got %q want %q", got, target)
	}
}

func TestLocalTempDirFromProcessEnv(t *testing.T) {
	clearTempVars(t)
	target := t.TempDir()
	t.Setenv("TERMINAL_TEMP_DIR", target)
	l := newUninitLocalEnv(t, newTestToolEnv(t), target, nil)
	if got := l.TempDir(); filepath.Clean(got) != filepath.Clean(target) {
		t.Fatalf("got %q want %q", got, target)
	}
}

func TestLocalTempDirNonExistentFallsThrough(t *testing.T) {
	clearTempVars(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	l := newUninitLocalEnv(t, newTestToolEnv(t), t.TempDir(), map[string]string{"TERMINAL_TEMP_DIR": missing})
	if l.TempDir() == missing {
		t.Fatal("missing path must not be used")
	}
}

func TestLocalTempDirEmptyFallsThrough(t *testing.T) {
	clearTempVars(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	l := newUninitLocalEnv(t, newTestToolEnv(t), tmp, map[string]string{"TERMINAL_TEMP_DIR": ""})
	if got := l.TempDir(); filepath.Clean(got) != filepath.Clean(tmp) {
		t.Fatalf("got %q", got)
	}
}

func TestLocalTempDirDefaultIsIcebathCacheNotTmp(t *testing.T) {
	clearTempVars(t)
	env := newTestToolEnv(t)
	l := newUninitLocalEnv(t, env, env.Home, nil)
	got := l.TempDir()
	if filepath.Clean(got) != filepath.Join(env.Home, "cache", "terminal") {
		t.Fatalf("got %q", got)
	}
	if st, err := os.Stat(got); err != nil || !st.IsDir() {
		t.Fatal("default temp dir must exist")
	}
}

func TestLocalTempDirTmpdirStillBeatsDefault(t *testing.T) {
	clearTempVars(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	l := newUninitLocalEnv(t, newTestToolEnv(t), tmp, nil)
	if got := l.TempDir(); filepath.Clean(got) != filepath.Clean(tmp) {
		t.Fatalf("got %q", got)
	}
}

func TestCleanupTerminalTempCache(t *testing.T) {
	env := newTestToolEnv(t)
	root := defaultTerminalTempDir(env)
	os.MkdirAll(root, 0o755)
	old := time.Now().Add(-100 * time.Hour)
	write := func(name string, when time.Time) string {
		p := filepath.Join(root, name)
		os.WriteFile(p, []byte("x"), 0o644)
		os.Chtimes(p, when, when)
		return p
	}
	stale := write("icebath-snap-deadbeef.sh", old)
	keep := write("icebath-snap-cafef00d.sh", time.Now())
	livePid := write("icebath_bg_live1.pid", old)
	liveLog := write("icebath_bg_live1.log", time.Now())
	for _, sfx := range []string{"log", "pid", "exit"} {
		write("icebath_bg_dead1."+sfx, old)
	}
	staleDir := filepath.Join(root, "stale-dir")
	os.Mkdir(staleDir, 0o755)
	os.WriteFile(filepath.Join(staleDir, "f"), []byte("x"), 0o644)
	os.Chtimes(filepath.Join(staleDir, "f"), old, old)
	os.Chtimes(staleDir, old, old)
	liveDir := filepath.Join(root, "live-dir")
	os.Mkdir(liveDir, 0o755)
	os.WriteFile(filepath.Join(liveDir, "f"), []byte("x"), 0o644)
	os.Chtimes(liveDir, old, old)

	removed := CleanupTerminalTempCache(env, 24)
	if removed != 5 {
		t.Fatalf("removed %d", removed)
	}
	for _, p := range []string{keep, livePid, liveLog, liveDir} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s must survive", p)
		}
	}
	for _, p := range []string{stale, filepath.Join(root, "icebath_bg_dead1.log"), staleDir} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s must be pruned", p)
		}
	}
}

func TestResolveSafeCwd(t *testing.T) {
	env := newTestToolEnv(t)
	dir := t.TempDir()
	if got := resolveSafeCwd(env, dir); got != dir {
		t.Fatalf("got %q", got)
	}
	if got := resolveSafeCwd(env, ""); got != os.TempDir() {
		t.Fatalf("hopeless path -> %q", got)
	}
	missing := filepath.Join(dir, "gone", "deeper")
	if got := resolveSafeCwd(env, missing); got != dir {
		t.Fatalf("missing child -> %q want %q", got, dir)
	}
}

func TestInaccessibleCwdFallback(t *testing.T) {
	if isWindowsHost || os.Geteuid() == 0 {
		t.Skip("chmod based access denial needs a non root POSIX user")
	}
	env := newTestToolEnv(t)
	dir := t.TempDir()
	denied := filepath.Join(dir, "rootlike")
	os.Mkdir(denied, 0o755)
	os.Chmod(denied, 0)
	t.Cleanup(func() { os.Chmod(denied, 0o755) })
	if st, err := os.Stat(denied); err != nil || !st.IsDir() {
		t.Fatal("stat should still succeed")
	}
	if cwdUsable(denied) {
		t.Fatal("unenterable directory must not be usable")
	}
	if got := resolveSafeCwd(env, filepath.Join(denied, "sub", "dir")); got != dir {
		t.Fatalf("got %q want %q", got, dir)
	}
	requireBash(t)
	l := NewLocalEnvironment(context.Background(), env, denied, 30, nil, true)
	defer l.Cleanup(false)
	res, err := l.Execute(context.Background(), "echo ALIVE", ExecOptions{})
	if err != nil || res.ReturnCode != 0 || !strings.Contains(res.Output, "ALIVE") {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestRunBashRecoversWhenCwdDeletedAfterInit(t *testing.T) {
	requireBash(t)
	env, logs := newLoggingToolEnv(t)
	base := t.TempDir()
	wedged := filepath.Join(base, "wedge-repro")
	os.Mkdir(wedged, 0o755)
	l := newUninitLocalEnv(t, env, wedged, nil)
	os.RemoveAll(wedged)
	if _, err := l.Execute(context.Background(), "echo hello", ExecOptions{}); err != nil {
		t.Fatalf("spawn must not fail on a deleted cwd: %v", err)
	}
	real, _ := filepath.EvalSymlinks(base)
	if filepath.Clean(l.Cwd()) != real {
		t.Fatalf("cwd %q want %q", l.Cwd(), real)
	}
	if !strings.Contains(logs.String(), "missing on disk") {
		t.Fatalf("warning missing: %s", logs.String())
	}
}

func TestRunBashNoWarningWhenCwdStillExists(t *testing.T) {
	requireBash(t)
	env, logs := newLoggingToolEnv(t)
	dir := t.TempDir()
	l := newUninitLocalEnv(t, env, dir, nil)
	if _, err := l.Execute(context.Background(), "echo hello", ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if real, _ := filepath.EvalSymlinks(dir); filepath.Clean(l.Cwd()) != real {
		t.Fatalf("cwd %q", l.Cwd())
	}
	if strings.Contains(logs.String(), "missing on disk") {
		t.Fatalf("unexpected warning: %s", logs.String())
	}
}

func TestUpdateCwdRejectsMissingPaths(t *testing.T) {
	base := t.TempDir()
	original := filepath.Join(base, "starting")
	os.Mkdir(original, 0o755)
	l := newUninitLocalEnv(t, newTestToolEnv(t), original, nil)
	deleted := filepath.Join(base, "wedge-repro")
	res := &ExecResult{Output: "x\n" + l.cwdMarker + deleted + l.cwdMarker + "\n"}
	l.updateCwd(res)
	if l.Cwd() != original || res.CwdObserved || res.Cwd != "" {
		t.Fatalf("cwd %q %+v", l.Cwd(), res)
	}
	next := filepath.Join(base, "next")
	os.Mkdir(next, 0o755)
	res = &ExecResult{Output: "x\n" + l.cwdMarker + next + l.cwdMarker + "\n"}
	l.updateCwd(res)
	if l.Cwd() != next || !res.CwdObserved || res.Cwd != next {
		t.Fatalf("cwd %q %+v", l.Cwd(), res)
	}
}

func TestRelativeInitialCwdResolvesFromParent(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "icebath-agent")
	os.Mkdir(project, 0o755)
	chdir(t, base)
	if got := resolveLocalInitialCwd("icebath-agent"); got != project {
		t.Fatalf("got %q", got)
	}
	chdir(t, project)
	if got := resolveLocalInitialCwd("icebath-agent"); got != project {
		t.Fatalf("tail match -> %q", got)
	}
	requireBash(t)
	chdir(t, base)
	l := NewLocalEnvironment(context.Background(), newTestToolEnv(t), "icebath-agent", 5, nil, true)
	defer l.Cleanup(false)
	res, err := l.Execute(context.Background(), "pwd", ExecOptions{Timeout: 5})
	if err != nil || res.ReturnCode != 0 || strings.TrimSpace(res.Output) != project {
		t.Fatalf("%v %+v", err, res)
	}
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(prev) })
}

// Tool env with terminal.shell_init_files and auto_source_bashrc configured
func shellInitEnv(t *testing.T, files []string, auto bool) *Env {
	t.Helper()
	env := newTestToolEnv(t)
	raw := make([]any, 0, len(files))
	for _, f := range files {
		raw = append(raw, f)
	}
	env.Config = config.Tree{"terminal": map[string]any{"shell_init_files": raw, "auto_source_bashrc": auto}}
	return env
}

func TestResolveShellInitFiles(t *testing.T) {
	if isWindowsHost {
		t.Skip("auto sourcing is POSIX only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	env := shellInitEnv(t, nil, true)
	if got := resolveShellInitFiles(env); len(got) != 0 {
		t.Fatalf("no rc files -> %v", got)
	}
	bashrc := filepath.Join(home, ".bashrc")
	os.WriteFile(bashrc, []byte("export MARKER=seen\n"), 0o644)
	if got := resolveShellInitFiles(env); len(got) != 1 || got[0] != bashrc {
		t.Fatalf("got %v", got)
	}
	profile := filepath.Join(home, ".profile")
	bashProfile := filepath.Join(home, ".bash_profile")
	os.WriteFile(profile, []byte("export FROM_PROFILE=1\n"), 0o644)
	os.WriteFile(bashProfile, []byte("export FROM_BASH_PROFILE=1\n"), 0o644)
	got := resolveShellInitFiles(env)
	if strings.Join(got, ",") != strings.Join([]string{profile, bashProfile, bashrc}, ",") {
		t.Fatalf("order %v", got)
	}
	explicit := shellInitEnv(t, []string{filepath.Join(home, "does-not-exist.sh")}, false)
	if got := resolveShellInitFiles(explicit); len(got) != 0 {
		t.Fatalf("missing explicit file must be skipped, got %v", got)
	}
}

func TestPrependShellInit(t *testing.T) {
	if prependShellInit("echo hi", nil) != "echo hi" {
		t.Fatal("empty list must not change the command")
	}
	wrapped := prependShellInit("echo hi", []string{"/tmp/a.sh", "/tmp/b.sh"})
	for _, want := range []string{"echo hi", "/tmp/a.sh", "/tmp/b.sh", "|| true", "set +e"} {
		if !strings.Contains(wrapped, want) {
			t.Fatalf("%q missing in %q", want, wrapped)
		}
	}
	if !strings.Contains(prependShellInit("echo hi", []string{"/tmp/o'malley.sh"}), `o'\''malley`) {
		t.Fatal("single quote must be escaped")
	}
}

func TestExportedEnvChangesPersistBetweenCommands(t *testing.T) {
	requireBash(t)
	l := NewLocalEnvironment(context.Background(), newTestToolEnv(t), t.TempDir(), 15, nil, true)
	defer l.Cleanup(false)
	first, _ := l.Execute(context.Background(), `export ICEBATH_STICKY_ENV_PROBE="sticky"; export PATH="/tmp/icebath-session-bin:$PATH"; echo "first=$ICEBATH_STICKY_ENV_PROBE"`, ExecOptions{})
	second, _ := l.Execute(context.Background(), `echo "second=$ICEBATH_STICKY_ENV_PROBE"; echo "PATH=$PATH"`, ExecOptions{})
	if first.ReturnCode != 0 || second.ReturnCode != 0 || !strings.Contains(first.Output, "first=sticky") {
		t.Fatalf("%+v %+v", first, second)
	}
	if !strings.Contains(second.Output, "second=sticky") || !strings.Contains(second.Output, "/tmp/icebath-session-bin") {
		t.Fatalf("second %q", second.Output)
	}
}

func TestSnapshotPicksUpInitFileExports(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	initFile := filepath.Join(dir, "custom-init.sh")
	os.WriteFile(initFile, []byte("export ICEBATH_SHELL_INIT_PROBE=\"probe-ok\"\nexport PATH=\"/opt/shell-init-probe/bin:$PATH\"\n"), 0o644)
	l := NewLocalEnvironment(context.Background(), shellInitEnv(t, []string{initFile}, false), dir, 15, nil, true)
	defer l.Cleanup(false)
	res, _ := l.Execute(context.Background(), `echo "PROBE=$ICEBATH_SHELL_INIT_PROBE"; echo "PATH=$PATH"`, ExecOptions{})
	if !strings.Contains(res.Output, "PROBE=probe-ok") || !strings.Contains(res.Output, "/opt/shell-init-probe/bin") {
		t.Fatalf("%q", res.Output)
	}
}

func TestProfilePathExportSurvivesBashrcInteractiveGuard(t *testing.T) {
	if isWindowsHost {
		t.Skip("POSIX rc files")
	}
	requireBash(t)
	home := t.TempDir()
	fakeNBin := filepath.Join(home, "fake-n", "bin")
	os.MkdirAll(fakeNBin, 0o755)
	os.WriteFile(filepath.Join(home, ".profile"), []byte("export PATH=\""+fakeNBin+":$PATH\"\nexport FROM_PROFILE=profile-ok\n"), 0o644)
	os.WriteFile(filepath.Join(home, ".bashrc"), []byte("case $- in\n    *i*) ;;\n      *) return;;\nesac\nexport FROM_BASHRC=bashrc-should-not-appear\n"), 0o644)
	t.Setenv("HOME", home)
	l := NewLocalEnvironment(context.Background(), shellInitEnv(t, nil, true), home, 15, nil, true)
	defer l.Cleanup(false)
	res, _ := l.Execute(context.Background(), `echo "PATH=$PATH"; echo "FROM_PROFILE=$FROM_PROFILE"; echo "FROM_BASHRC=$FROM_BASHRC"`, ExecOptions{})
	if !strings.Contains(res.Output, "FROM_PROFILE=profile-ok") || !strings.Contains(res.Output, fakeNBin) {
		t.Fatalf("%q", res.Output)
	}
	if strings.Contains(res.Output, "FROM_BASHRC=bashrc-should-not-appear") {
		t.Fatalf("bashrc export ran: %q", res.Output)
	}
}

func TestLocalPersistentControlsExportsButCwdAlwaysCarries(t *testing.T) {
	requireBash(t)
	for _, c := range []struct {
		persistent bool
		exported   string
	}{{true, "yes"}, {false, "none"}} {
		dir := t.TempDir()
		os.Mkdir(filepath.Join(dir, "sub"), 0o755)
		l := NewLocalEnvironment(context.Background(), newTestToolEnv(t), dir, 15, nil, c.persistent)
		run := func(cmd string) string {
			res, err := l.Execute(context.Background(), cmd, ExecOptions{})
			if err != nil {
				t.Fatal(err)
			}
			return strings.TrimSpace(res.Output)
		}
		run("cd sub && export ICEBATH_LP_EXPORT=yes; ICEBATH_LP_PLAIN=yes")
		if got := run("echo ${ICEBATH_LP_EXPORT:-none} ${ICEBATH_LP_PLAIN:-none}"); got != c.exported+" none" {
			t.Fatalf("persistent=%v got %q", c.persistent, got)
		}
		real, _ := filepath.EvalSymlinks(filepath.Join(dir, "sub"))
		if got := run("pwd -P"); got != real {
			t.Fatalf("persistent=%v cwd %q want %q", c.persistent, got, real)
		}
		l.Cleanup(false)
	}
}

func TestProfileFunctionsSurviveOnlyWithPersistenceOff(t *testing.T) {
	if isWindowsHost {
		t.Skip("POSIX rc files")
	}
	requireBash(t)
	for _, c := range []struct {
		persistent bool
		later      string
	}{{true, "fn:no"}, {false, "fn:yes"}} {
		home := t.TempDir()
		for _, rc := range []string{".bashrc", ".bash_profile"} {
			os.WriteFile(filepath.Join(home, rc), []byte("icebath_lp_fn(){ echo FN; }\n"), 0o644)
		}
		t.Setenv("HOME", home)
		l := NewLocalEnvironment(context.Background(), shellInitEnv(t, nil, true), home, 15, nil, c.persistent)
		probe := "type icebath_lp_fn >/dev/null 2>&1 && echo fn:yes || echo fn:no"
		run := func() string {
			res, _ := l.Execute(context.Background(), probe, ExecOptions{})
			return strings.TrimSpace(res.Output)
		}
		if got := run(); got != "fn:yes" {
			t.Fatalf("persistent=%v first %q", c.persistent, got)
		}
		if got := run(); got != c.later {
			t.Fatalf("persistent=%v second %q want %q", c.persistent, got, c.later)
		}
		l.Cleanup(false)
	}
}

// Session context leak tests

func TestEngagedUnsetContextStripsForeignSessionKey(t *testing.T) {
	env := newTestToolEnv(t)
	env.Values.Store(sessionContextEngagedKey, true)
	t.Setenv("ICEBATH_SESSION_KEY", "agent:main:discord:thread:FOREIGN_CONCURRENT:FOREIGN_CONCURRENT")
	if _, ok := mustRunEnv(t, env)["ICEBATH_SESSION_KEY"]; ok {
		t.Fatal("foreign concurrent session key leaked into subprocess env")
	}
}

func TestBoundSessionVarsOverrideForeignGlobal(t *testing.T) {
	env := newTestToolEnv(t)
	t.Setenv("ICEBATH_SESSION_KEY", "agent:main:discord:thread:FOREIGN:FOREIGN")
	BindSessionContext(env, map[string]string{"ICEBATH_SESSION_KEY": "agent:main:discord:group:MY_BUGS_ROOT:111", "ICEBATH_SESSION_PLATFORM": "discord", "ICEBATH_SESSION_CHAT_ID": "MY_BUGS_ROOT"})
	if got := mustRunEnv(t, env)["ICEBATH_SESSION_KEY"]; got != "agent:main:discord:group:MY_BUGS_ROOT:111" {
		t.Fatalf("got %q", got)
	}
}

func TestUnengagedProcessPreservesEnvironFallback(t *testing.T) {
	env := newTestToolEnv(t)
	t.Setenv("ICEBATH_SESSION_KEY", "cli-session-key")
	t.Setenv("ICEBATH_SESSION_ID", "cli-session-id")
	run := mustRunEnv(t, env)
	if run["ICEBATH_SESSION_KEY"] != "cli-session-key" || run["ICEBATH_SESSION_ID"] != "cli-session-id" {
		t.Fatalf("%q %q", run["ICEBATH_SESSION_KEY"], run["ICEBATH_SESSION_ID"])
	}
}

func TestExplicitEmptyThreadIDOverridesStaleValue(t *testing.T) {
	env := newTestToolEnv(t)
	t.Setenv("ICEBATH_SESSION_THREAD_ID", "stale-thread-from-prior-turn")
	BindSessionContext(env, map[string]string{"ICEBATH_SESSION_KEY": "mm:chan", "ICEBATH_SESSION_PLATFORM": "mattermost", "ICEBATH_SESSION_CHAT_ID": "chan", "ICEBATH_SESSION_THREAD_ID": ""})
	run := mustRunEnv(t, env)
	if v, ok := run["ICEBATH_SESSION_THREAD_ID"]; !ok || v != "" {
		t.Fatalf("bound empty thread id did not win: %q %v", v, ok)
	}
	if run["ICEBATH_SESSION_KEY"] != "mm:chan" {
		t.Fatalf("key %q", run["ICEBATH_SESSION_KEY"])
	}
}

func TestSanitizeSubprocessEnvSessionStripAndOverride(t *testing.T) {
	env := newTestToolEnv(t)
	env.Values.Store(sessionContextEngagedKey, true)
	stale := map[string]string{"PATH": "/usr/bin:/bin", "ICEBATH_SESSION_KEY": "agent:main:discord:thread:FOREIGN_BG:FOREIGN_BG", "ICEBATH_SESSION_THREAD_ID": "FOREIGN_BG"}
	sanitized := mustSanitize(t, env, stale, nil)
	if _, ok := sanitized["ICEBATH_SESSION_KEY"]; ok {
		t.Fatal("background subprocess inherited foreign key")
	}
	if _, ok := sanitized["ICEBATH_SESSION_THREAD_ID"]; ok {
		t.Fatal("foreign thread id survived")
	}
	BindSessionContext(env, map[string]string{"ICEBATH_SESSION_KEY": "agent:main:discord:group:REAL_BG:222"})
	if got := mustSanitize(t, env, stale, nil)["ICEBATH_SESSION_KEY"]; got != "agent:main:discord:group:REAL_BG:222" {
		t.Fatalf("got %q", got)
	}
}

func TestIcebathSubprocessEnvSessionKeyHandling(t *testing.T) {
	env := newTestToolEnv(t)
	t.Setenv("ICEBATH_SESSION_KEY", "cli-fallback-key")
	if got := IcebathSubprocessEnv(env, false, nil)["ICEBATH_SESSION_KEY"]; got != "cli-fallback-key" {
		t.Fatalf("unengaged fallback lost: %q", got)
	}
	env.Values.Store(sessionContextEngagedKey, true)
	if _, ok := IcebathSubprocessEnv(env, false, nil)["ICEBATH_SESSION_KEY"]; ok {
		t.Fatal("foreign session key leaked into non terminal spawn env")
	}
}

// Background child drain tests

func pkillPattern(pattern string) {
	exec.Command("pkill", "-9", "-f", pattern).Run()
}

func TestPlainBackgroundReturnsPromptly(t *testing.T) {
	requireBash(t)
	l := NewLocalEnvironment(context.Background(), newTestToolEnv(t), t.TempDir(), 15, nil, true)
	defer l.Cleanup(false)
	defer pkillPattern("sleep 60")
	start := time.Now()
	res, err := l.Execute(context.Background(), "sleep 60 & echo icebath_8340_plain_bg", ExecOptions{Timeout: 15})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("hung for %s", elapsed)
	}
	if res.ReturnCode != 0 || !strings.Contains(res.Output, "icebath_8340_plain_bg") {
		t.Fatalf("%+v", res)
	}
	start = time.Now()
	res, _ = l.Execute(context.Background(), "setsid sleep 60 > /dev/null 2>&1 < /dev/null & disown; echo started", ExecOptions{Timeout: 15})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("setsid path hung for %s", elapsed)
	}
	if res.ReturnCode != 0 || !strings.Contains(res.Output, "started") {
		t.Fatalf("%+v", res)
	}
}

func TestDefaultCaptureIsFullFidelityAndBoundedCaptureSpills(t *testing.T) {
	requireBash(t)
	env := newTestToolEnv(t)
	l := NewLocalEnvironment(context.Background(), env, t.TempDir(), 15, nil, true)
	defer l.Cleanup(false)
	cmd := "printf 'START-MARK\\n'; head -c 200000 /dev/zero | tr '\\0' 'y'; printf '\\nEND-MARK'"
	res, err := l.Execute(context.Background(), cmd, ExecOptions{Timeout: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.ReturnCode != 0 || strings.Contains(res.Output, "[OUTPUT TRUNCATED") || !strings.HasPrefix(res.Output, "START-MARK") || !strings.HasSuffix(res.Output, "END-MARK") || len(res.Output) <= 200000 {
		t.Fatalf("code %d len %d", res.ReturnCode, len(res.Output))
	}
	bounded, err := l.Execute(context.Background(), cmd, ExecOptions{Timeout: 10, BoundedCapture: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bounded.Output, "[OUTPUT TRUNCATED") || bounded.FullOutputPath == "" || bounded.OutputTotalChars <= 200000 {
		t.Fatalf("bounded %+v", bounded)
	}
	if !strings.HasPrefix(bounded.FullOutputPath, filepath.Join(env.Home, "cache", "terminal-output")) {
		t.Fatalf("spill path %q", bounded.FullOutputPath)
	}
	data, _ := os.ReadFile(bounded.FullOutputPath)
	if !strings.HasPrefix(string(data), "START-MARK") {
		t.Fatalf("spill %q", data[:20])
	}
}

func TestUTF8MultibyteAcrossReadBoundaryAndInvalidBytes(t *testing.T) {
	requireBash(t)
	l := NewLocalEnvironment(context.Background(), newTestToolEnv(t), t.TempDir(), 15, nil, true)
	defer l.Cleanup(false)
	res, _ := l.Execute(context.Background(), `for i in $(seq 1 10000); do printf '\xe6\x97\xa5'; done; printf '\n'`, ExecOptions{Timeout: 10})
	if res.ReturnCode != 0 || strings.Count(res.Output, "\u65e5") != 10000 {
		t.Fatalf("lost multibyte chars: %d / 10000", strings.Count(res.Output, "\u65e5"))
	}
	res, _ = l.Execute(context.Background(), `printf 'before '; printf '\xff\xfe'; printf ' after\n'`, ExecOptions{Timeout: 15})
	if res.ReturnCode != 0 || !strings.Contains(res.Output, "before \ufffd\ufffd after") {
		t.Fatalf("%q", res.Output)
	}
}

// Credential scrub tests

func TestIcebathSubprocessEnvStripsByDefault(t *testing.T) {
	env := newTestToolEnv(t)
	base := map[string]string{
		"OPENAI_API_KEY": "sk", "ANTHROPIC_API_KEY": "sk", "GH_TOKEN": "g", "TELEGRAM_BOT_TOKEN": "t",
		"MODAL_TOKEN_SECRET": "m", "ICEBATH_DASHBOARD_SESSION_TOKEN": "d", "AUXILIARY_VISION_API_KEY": "a",
		"AUXILIARY_VISION_MODEL": "gpt", "GATEWAY_RELAY_ID": "r", "GATEWAY_RELAY_SECRET": "s", "PATH": "/bin", "MYVAR": "keep",
	}
	out := IcebathSubprocessEnv(env, false, base)
	for _, gone := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GH_TOKEN", "TELEGRAM_BOT_TOKEN", "MODAL_TOKEN_SECRET", "ICEBATH_DASHBOARD_SESSION_TOKEN", "AUXILIARY_VISION_API_KEY", "GATEWAY_RELAY_ID", "GATEWAY_RELAY_SECRET"} {
		if _, ok := out[gone]; ok {
			t.Fatalf("%s must be stripped", gone)
		}
	}
	if out["AUXILIARY_VISION_MODEL"] != "gpt" || out["MYVAR"] != "keep" || out["PYTHONUTF8"] != "1" {
		t.Fatalf("%v", out)
	}
	inherit := IcebathSubprocessEnv(env, true, base)
	if inherit["OPENAI_API_KEY"] != "sk" || inherit["ANTHROPIC_API_KEY"] != "sk" {
		t.Fatal("provider keys must pass when inheriting")
	}
	for _, gone := range []string{"GH_TOKEN", "TELEGRAM_BOT_TOKEN", "MODAL_TOKEN_SECRET", "GATEWAY_RELAY_ID", "AUXILIARY_VISION_API_KEY"} {
		if _, ok := inherit[gone]; ok {
			t.Fatalf("%s must be stripped even when inheriting", gone)
		}
	}
	if inherit["PYTHONUTF8"] != "1" {
		t.Fatal("PYTHONUTF8 missing when inheriting")
	}
}

func TestDelegatedChildContextScrubsKanbanKeysAndSetsMarker(t *testing.T) {
	prev := KanbanHomeResolver
	KanbanHomeResolver = nil
	defer func() { KanbanHomeResolver = prev }()
	env := newTestToolEnv(t)
	env.Values.Store("session.delegated_child", true)
	base := map[string]string{"ICEBATH_KANBAN_TASK": "42", "ICEBATH_KANBAN_RUN_ID": "r", "ICEBATH_SESSION_SOURCE": "kanban", "PATH": "/bin"}
	out := IcebathSubprocessEnv(env, false, base)
	for _, gone := range []string{"ICEBATH_KANBAN_TASK", "ICEBATH_KANBAN_RUN_ID", "ICEBATH_SESSION_SOURCE"} {
		if _, ok := out[gone]; ok {
			t.Fatalf("%s must be scrubbed", gone)
		}
	}
	if out[delegatedChildEnvMarker] != "1" {
		t.Fatalf("marker %q", out[delegatedChildEnvMarker])
	}
	KanbanHomeResolver = func(env *Env) string { return "/boards/root" }
	if got := IcebathSubprocessEnv(env, false, base)[delegatedChildEnvMarker]; got != "/boards/root" {
		t.Fatalf("marker %q", got)
	}
	inherited := map[string]string{delegatedChildEnvMarker: "/other/root", "ICEBATH_KANBAN_TASK": "1"}
	if got := IcebathSubprocessEnv(env, false, inherited)[delegatedChildEnvMarker]; got != "/other/root" {
		t.Fatalf("inherited marker %q", got)
	}
	plain := newTestToolEnv(t)
	if _, ok := IcebathSubprocessEnv(plain, false, map[string]string{"PATH": "/bin"})[delegatedChildEnvMarker]; ok {
		t.Fatal("non delegated process must not carry the marker")
	}
}

func TestForcePrefixUnwrapsOnlyFromExtras(t *testing.T) {
	env := newTestToolEnv(t)
	base := map[string]string{"_ICEBATH_FORCE_OPENAI_API_KEY": "base", "PATH": "/bin"}
	extra := map[string]string{"_ICEBATH_FORCE_OPENAI_API_KEY": "extra", "_ICEBATH_FORCE_AUXILIARY_X_API_KEY": "never"}
	out := mustSanitize(t, env, base, extra)
	if out["OPENAI_API_KEY"] != "extra" {
		t.Fatalf("forced extra lost: %v", out)
	}
	if _, ok := out["_ICEBATH_FORCE_OPENAI_API_KEY"]; ok {
		t.Fatal("force prefix must not survive")
	}
	if _, ok := out["AUXILIARY_X_API_KEY"]; ok {
		t.Fatal("internal secret must not be forced through")
	}
	if _, ok := mustSanitize(t, env, base, nil)["OPENAI_API_KEY"]; ok {
		t.Fatal("forced base value must be dropped")
	}
}

func TestBuildersStripRuntimeMarkersAndOwnedPythonpath(t *testing.T) {
	env := newTestToolEnv(t)
	root := RunningSourceRoot()
	if root == "" {
		t.Skip("source root unknown")
	}
	base := map[string]string{"PATH": "/bin", "PYTHONPATH": root + string(os.PathListSeparator) + "/user/lib" + string(os.PathListSeparator) + root + "/child" + string(os.PathListSeparator), "VIRTUAL_ENV": "/venv", "CONDA_PREFIX": "/c", "PYTHONHOME": "/p"}
	out := mustSanitize(t, env, base, nil)
	if out["PYTHONPATH"] != "/user/lib"+string(os.PathListSeparator)+root+"/child"+string(os.PathListSeparator) {
		t.Fatalf("PYTHONPATH %q", out["PYTHONPATH"])
	}
	for _, gone := range activeVenvMarkerVars {
		if _, ok := out[gone]; ok {
			t.Fatalf("%s must be stripped", gone)
		}
	}
	only := mustSanitize(t, env, map[string]string{"PATH": "/bin", "PYTHONPATH": root}, nil)
	if _, ok := only["PYTHONPATH"]; ok {
		t.Fatal("owned only PYTHONPATH must be removed")
	}
	kept, err := BuildSubprocessEnv(context.Background(), env, SubprocessEnvOptions{Base: map[string]string{"PYTHONHOME": "/p"}, ScrubSecrets: false})
	if err != nil || kept["PYTHONHOME"] != "/p" {
		t.Fatal("no scrub path must preserve PYTHONHOME")
	}
}

func TestSupervisedIdentityAndProfileGates(t *testing.T) {
	child := map[string]string{"ICEBATH_GATEWAY_EXTERNAL_SUPERVISOR": "true", "NOTIFY_SOCKET": "/s", "WATCHDOG_USEC": "1", "KEEP": "1"}
	StripSupervisedProcessIdentity(child)
	if child[GatewayProfilePinEnv] != "1" || child["KEEP"] != "1" {
		t.Fatalf("%v", child)
	}
	for _, gone := range supervisedProcessIdentityVars {
		if _, ok := child[gone]; ok {
			t.Fatalf("%s must be stripped", gone)
		}
	}
	cases := map[string]bool{
		"DISCORD_ALLOWED_CHANNELS": true, "TELEGRAM_ALLOW_ALL_USERS": true, "GATEWAY_ALLOWED_USERS": true,
		"WHATSAPP_GROUP_ALLOW_FROM": true, "QQ_ALLOWED_USERS": true, "DEMO_ALLOWED_SENDER": false,
		"ICEBATH_MEDIA_ALLOW_DIRS": false, "HERMES_ALLOW_PRIVATE_URLS": false, "_DISCORD_ALLOWED_X": false, "DISCORD_TOKEN": false,
	}
	for name, want := range cases {
		if got := IsProfileGateEnv(name, nil); got != want {
			t.Fatalf("%s gate=%v want %v", name, got, want)
		}
	}
	prev := RegisteredPlatformNames
	RegisteredPlatformNames = func() []string { return []string{"my-plugin"} }
	defer func() { RegisteredPlatformNames = prev }()
	if !IsProfileGateEnv("MY_PLUGIN_ALLOWED_USERS", nil) {
		t.Fatal("registered plugin platform must own gates")
	}
	env := map[string]string{"DISCORD_ALLOWED_CHANNELS": "1", "DISCORD_BOT_NAME": "x"}
	StripProfileGateEnv(env)
	if _, ok := env["DISCORD_ALLOWED_CHANNELS"]; ok || env["DISCORD_BOT_NAME"] != "x" {
		t.Fatalf("%v", env)
	}
}

func TestStripLaunchProfileEnvDropsResidueForRoutedHome(t *testing.T) {
	installTestEnvHooks(t)
	launch := t.TempDir()
	t.Setenv("ICEBATH_HOME", launch)
	os.WriteFile(filepath.Join(launch, ".env"), []byte("ICEBATH_MODEL=gpt\nMY_SETTING=1\nPATH=/x\n"), 0o600)
	RecordLaunchDotenvKeys([]string{"LOADED_LATER"})
	prev := ProfileSecrets
	ProfileSecrets = managedSecrets{managed: []string{"ORG_POLICY_FLAG"}}
	defer func() { ProfileSecrets = prev }()
	child := map[string]string{
		"ICEBATH_MODEL": "gpt", "MY_SETTING": "1", "PATH": "/x", "TERMINAL_ENV": "docker", "LOADED_LATER": "v",
		"ORG_POLICY_FLAG": "policy", "UNRELATED": "u", "DISCORD_ALLOWED_CHANNELS": "c",
	}
	env := &Env{Home: launch, Secrets: config.NewSecrets(launch)}
	StripLaunchProfileEnv(env, child, "")
	if len(child) != 8 {
		t.Fatalf("launch home must be a no op: %v", child)
	}
	routed := t.TempDir()
	StripLaunchProfileEnv(env, child, routed)
	for _, gone := range []string{"ICEBATH_MODEL", "MY_SETTING", "TERMINAL_ENV", "LOADED_LATER", "DISCORD_ALLOWED_CHANNELS"} {
		if _, ok := child[gone]; ok {
			t.Fatalf("%s must be stripped: %v", gone, child)
		}
	}
	for _, keep := range []string{"PATH", "ORG_POLICY_FLAG", "UNRELATED"} {
		if _, ok := child[keep]; !ok {
			t.Fatalf("%s must survive: %v", keep, child)
		}
	}
	t.Setenv("ORG_POLICY_FLAG", "admin")
	RestoreManagedEnv(child)
	if child["ORG_POLICY_FLAG"] != "admin" {
		t.Fatal("managed value must be re applied")
	}
}

// Secret source double with administrator managed keys
type managedSecrets struct {
	managed []string
}

func (m managedSecrets) ManagedDotenvKeys() []string   { return m.managed }
func (m managedSecrets) SourceSuppliedNames() []string { return nil }
func (m managedSecrets) BuildProfileSecretScope(profileHome string) map[string]string {
	return config.LoadEnvFile(filepath.Join(profileHome, ".env"))
}
func (m managedSecrets) CurrentSecretScope(ctx context.Context) map[string]string { return nil }

func TestServedProfileChildEnvOverlaysTargetSecrets(t *testing.T) {
	env := newTestToolEnv(t)
	target := t.TempDir()
	os.WriteFile(filepath.Join(target, ".env"), []byte("OPENAI_API_KEY=target-key\n"), 0o600)
	t.Setenv("OPENAI_API_KEY", "launch-key")
	child, err := ServedProfileChildEnv(context.Background(), env, nil, target, true)
	if err != nil {
		t.Fatal(err)
	}
	if child["OPENAI_API_KEY"] != "target-key" || child["ICEBATH_HOME"] != target {
		t.Fatalf("%q %q", child["OPENAI_API_KEY"], child["ICEBATH_HOME"])
	}
	noInherit, _ := ServedProfileChildEnv(context.Background(), env, nil, target, false)
	if _, ok := noInherit["OPENAI_API_KEY"]; ok {
		t.Fatal("provider key must be scrubbed without inheritance")
	}
	t.Setenv("ICEBATH_HOME", env.Home)
	env.Values.Store("session.multiplex", true)
	if _, err := ServedProfileChildEnv(context.Background(), env, nil, "", true); err == nil {
		t.Fatal("unscoped multiplex inherit must fail closed")
	}
}

func TestSanePathAndIcebathBinDir(t *testing.T) {
	if isWindowsHost {
		t.Skip("POSIX PATH")
	}
	env := newTestToolEnv(t)
	got := appendMissingSanePathEntries(env, "/custom/bin::/usr/bin:/custom/bin")
	parts := strings.Split(got, ":")
	if parts[0] != "/custom/bin" || parts[1] != "/usr/bin" {
		t.Fatalf("operator precedence lost: %q", got)
	}
	if !strings.Contains(got, "/opt/homebrew/bin") || strings.Contains(got, "::") || strings.Count(got, "/custom/bin") != 1 {
		t.Fatalf("%q", got)
	}
	bin := filepath.Join(env.Home, "bin")
	os.Mkdir(bin, 0o755)
	if !strings.Contains(appendMissingSanePathEntries(env, "/usr/bin"), bin) {
		t.Fatal("managed home bin must be appended")
	}
	once := prependIcebathBinDir("/usr/bin")
	if prependIcebathBinDir(once) != once {
		t.Fatal("bin dir repair must be idempotent")
	}
	if prependMissingPathEntries("/a:/b", []string{"/b", "/c"}) != "/c:/a:/b" {
		t.Fatal("missing entries go first, listed ones stay")
	}
}

func TestMsysHelpersAreNoOpsOffWindows(t *testing.T) {
	if isWindowsHost {
		t.Skip("POSIX host")
	}
	if windowsToMsysPath(`C:\Users\x`) != `C:\Users\x` || bashSafePath(`C:\Users\x`) != `C:\Users\x` || msysToWindowsPath("/c/Users/x") != "/c/Users/x" {
		t.Fatal("translations must be no ops off Windows")
	}
	if len(gitBashBinDirs()) != 0 {
		t.Fatal("no Git Bash dirs off Windows")
	}
	run := mustRunEnv(t, newTestToolEnv(t))
	if _, ok := run["MSYS_NO_PATHCONV"]; ok {
		t.Fatal("MSYS defaults are Windows only")
	}
	if !strings.Contains(run["PATH"], "/usr/bin") {
		t.Fatalf("PATH %q", run["PATH"])
	}
}

func TestAppendDroppedPathEntries(t *testing.T) {
	if isWindowsHost {
		t.Skip("POSIX PATH")
	}
	dir := t.TempDir()
	out := appendDroppedPathEntries("echo hi", dir+":/does/not/exist:"+dir+":relative/bin")
	if !strings.HasPrefix(out, "for __icebath_p in "+shQuote(dir)+" relative/bin; do") || !strings.HasSuffix(out, "\necho hi") {
		t.Fatalf("%q", out)
	}
	if appendDroppedPathEntries("echo hi", "/does/not/exist") != "echo hi" {
		t.Fatal("no usable entries must leave the command alone")
	}
}

func TestLocalBackendProviderRegistered(t *testing.T) {
	p := GetTerminalBackend("local")
	if p == nil || p.Name() != "local" || p.IsRemote() || p.IsContainer() {
		t.Fatalf("%v", p)
	}
	if err := RegisterTerminalBackend(localBackend{}); err == nil {
		t.Fatal("plugins must not register the reserved local name")
	}
	requireBash(t)
	e, err := p.CreateEnvironment(context.Background(), newTestToolEnv(t), CreateOptions{Cwd: t.TempDir(), Timeout: 5, ContainerConfig: map[string]any{"persistent": false}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cleanup(false)
	if e.EnvType() != "local" || !e.UsesHostFilesystem() || !EnvUsesHostFilesystem(e) {
		t.Fatal("local environment flags")
	}
	if e.(*localEnv).persistEnv {
		t.Fatal("persistent false must reach the environment")
	}
}

func TestInterruptedCommandReportsNoCwd(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	l := NewLocalEnvironment(context.Background(), newTestToolEnv(t), dir, 15, nil, true)
	defer l.Cleanup(false)
	done, _ := l.Execute(context.Background(), "cd sub", ExecOptions{})
	real, _ := filepath.EvalSymlinks(sub)
	if !done.CwdObserved || filepath.Clean(done.Cwd) != real {
		t.Fatalf("%+v", done)
	}
	worker := NewWorkerID()
	ctx := WithWorker(context.Background(), worker)
	go func() { time.Sleep(300 * time.Millisecond); SetInterrupt(worker, true, "") }()
	defer SetInterrupt(worker, false, "")
	res, _ := l.Execute(ctx, "cd / && sleep 30", ExecOptions{Timeout: 20})
	if res.ReturnCode != 130 || res.CwdObserved || res.Cwd != "" {
		t.Fatalf("%+v", res)
	}
	if filepath.Clean(l.Cwd()) != real {
		t.Fatalf("interrupt moved the cwd to %q", l.Cwd())
	}
}

func TestIsRoutedHomeSeparatesOtherHomesFromProcessHome(t *testing.T) {
	if IsRoutedHome(home.Process()) {
		t.Fatal("process home reported as routed")
	}
	if !IsRoutedHome(t.TempDir()) {
		t.Fatal("unrelated directory not reported as routed")
	}
}

func TestSessionToolEnvHookIsUnsetUntilInstalled(t *testing.T) {
	if SessionToolEnv != nil {
		t.Fatal("hook installed before the agent package sets it")
	}
	SessionToolEnv = func(env *Env) map[string]string { return map[string]string{"PATH": env.Home} }
	t.Cleanup(func() { SessionToolEnv = nil })
	if got := SessionToolEnv(&Env{Home: "/h"}); got["PATH"] != "/h" {
		t.Fatalf("installed hook got %v", got)
	}
}

func TestIcebathSubprocessEnvCheckedReturnsTheDamagedRecordError(t *testing.T) {
	env := newTestToolEnv(t)
	base := map[string]string{"OPENAI_API_KEY": "sk", "PATH": "/bin"}
	out, err := IcebathSubprocessEnvChecked(env, false, base)
	if err != nil || out["PATH"] != "/bin" {
		t.Fatalf("checked env = %v %v", out, err)
	}
	if _, ok := out["OPENAI_API_KEY"]; ok {
		t.Fatal("checked env kept a credential")
	}
	t.Setenv("VIRTUAL_ENV", damageRuntimeRecord(t))
	t.Setenv("PYTHONPATH", "/home/user/my-lib")
	if _, err := IcebathSubprocessEnvChecked(env, false, nil); err == nil {
		t.Fatal("damaged runtime record swallowed")
	}
}
