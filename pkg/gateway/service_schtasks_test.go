package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Replaces a seam for the test and restores it on cleanup
func schtasksSwap[T any](t *testing.T, p *T, v T) {
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// Lifecycle output buffer installed for the test with the host faked as Windows
func schtasksWindowsHost(t *testing.T) *bytes.Buffer {
	out := &bytes.Buffer{}
	schtasksSwap[io.Writer](t, &lifecycleOut, out)
	schtasksSwap(t, &serviceGOOS, "windows")
	return out
}

// Text of a BOM led UTF-16 file
func readUTF16File(t *testing.T, path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return decodeUTF16(t, raw)
}

// Text of BOM led little endian UTF-16 bytes
func decodeUTF16(t *testing.T, raw []byte) string {
	if len(raw) < 2 || raw[0] != 0xff || raw[1] != 0xfe {
		t.Fatalf("missing UTF-16 LE BOM: % x", raw[:min(len(raw), 4)])
	}
	units := make([]uint16, (len(raw)-2)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(raw[2+2*i:])
	}
	return string(utf16.Decode(units))
}

// Sorted names in a directory
func dirNames(t *testing.T, dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

func TestLocalizedAccessDeniedUsesExistingFallbackPaths(t *testing.T) {
	detail := "错误: 拒绝访问。"
	if !windowsShouldFallBack(1, detail) || !windowsIsAccessDenied(detail) {
		t.Fatal("localized denial must reach the elevation and Startup fallback handling")
	}
}

func TestBuildGatewayArgvKeepsVenvConsolePythonForUvVenv(t *testing.T) {
	schtasksWindowsHost(t)
	tmp := t.TempDir()
	project := filepath.Join(tmp, "project")
	scripts := filepath.Join(project, "venv", "Scripts")
	icebathHome := filepath.Join(tmp, "icebath-home")
	base := filepath.Join(tmp, "uv", "python", "cpython-3.11-windows-x86_64-none")
	for _, d := range []string{scripts, filepath.Join(project, "venv", "Lib", "site-packages"), icebathHome, base} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	venvPython := filepath.Join(scripts, "python.exe")
	for _, exe := range []string{venvPython, filepath.Join(scripts, "pythonw.exe"), filepath.Join(base, "pythonw.exe")} {
		if err := os.WriteFile(exe, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", icebathHome)
	schtasksSwap(t, &serviceDefProjectRoot, func() string { return project })
	schtasksSwap(t, &winLauncherBinary, func() string { return venvPython })
	argv, cwd, overlay, err := windowsGatewayArgv(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) < 2 || argv[0] != venvPython || argv[1] != pm.PinDefaultHomeFlag {
		t.Fatalf("argv = %q", argv)
	}
	if cwd != icebathHome {
		t.Fatalf("cwd = %q", cwd)
	}
	if overlay["VIRTUAL_ENV"] != filepath.Join(project, "venv") {
		t.Fatalf("VIRTUAL_ENV = %q", overlay["VIRTUAL_ENV"])
	}
	if !slices.Contains(strings.Split(overlay["PYTHONPATH"], string(os.PathListSeparator)), project) {
		t.Fatalf("PYTHONPATH = %q", overlay["PYTHONPATH"])
	}
}

// Popen double recording each command and failing the calls whose index is in fail
type popenRecorder struct {
	calls []*exec.Cmd
	fail  map[int]error
	pids  []int
}

// Records cmd and answers a fake process or the planned failure
func (r *popenRecorder) popen(cmd *exec.Cmd) error {
	r.calls = append(r.calls, cmd)
	if err := r.fail[len(r.calls)]; err != nil {
		return err
	}
	cmd.Process = &os.Process{Pid: r.pids[len(r.calls)-1]}
	return nil
}

// Warning log buffer installed as the default slog handler for the test
func captureWarnings(t *testing.T) *bytes.Buffer {
	buf := &bytes.Buffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

// Environment of a recorded command as a map
func cmdEnv(cmd *exec.Cmd) map[string]string {
	env := map[string]string{}
	for _, kv := range cmd.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	return env
}

func TestSpawnDetachedMarksPrimaryBreakawaySuccess(t *testing.T) {
	schtasksWindowsHost(t)
	tmp := t.TempDir()
	argv := []string{"icebath.exe", "gateway", "run"}
	schtasksSwap(t, &winGatewayArgv, func(context.Context, string) ([]string, string, map[string]string, error) {
		return argv, tmp, map[string]string{"ICEBATH_GATEWAY_DETACHED": "1"}, nil
	})
	schtasksSwap(t, &winHome, func(context.Context) string { return tmp })
	rec := &popenRecorder{pids: []int{12345}}
	schtasksSwap(t, &winPopen, rec.popen)
	logs := captureWarnings(t)
	pid, err := spawnWindowsDetached(context.Background(), "")
	if err != nil || pid != 12345 || len(rec.calls) != 1 {
		t.Fatalf("pid=%d err=%v calls=%d", pid, err, len(rec.calls))
	}
	cmd := rec.calls[0]
	if !slices.Equal(cmd.Args, argv) || cmd.Dir != tmp || !reflect.DeepEqual(cmd.SysProcAttr, home.DetachAttr(true)) {
		t.Fatalf("args=%q dir=%q attr=%+v", cmd.Args, cmd.Dir, cmd.SysProcAttr)
	}
	if cmdEnv(cmd)[windowsGatewayBreakawayEnv] != "1" || cmd.Stdin != nil || cmd.Stdout != cmd.Stderr {
		t.Fatalf("env marker, null stdin or shared log handle wrong: %+v", cmd)
	}
	if logs.Len() != 0 || windowsBreakawayFellBack.Load() {
		t.Fatalf("unexpected warning %q", logs.String())
	}
}

func TestSpawnDetachedWarnsAndMarksNoBreakawayFallback(t *testing.T) {
	schtasksWindowsHost(t)
	tmp := t.TempDir()
	argv := []string{"icebath.exe", "gateway", "run"}
	schtasksSwap(t, &winGatewayArgv, func(context.Context, string) ([]string, string, map[string]string, error) {
		return argv, tmp, map[string]string{"ICEBATH_GATEWAY_DETACHED": "1", "SECRET_SENTINEL": "do-not-log"}, nil
	})
	schtasksSwap(t, &winHome, func(context.Context) string { return tmp })
	rec := &popenRecorder{pids: []int{0, 23456}, fail: map[int]error{1: syscall.Errno(5)}}
	schtasksSwap(t, &winPopen, rec.popen)
	t.Cleanup(func() { windowsBreakawayFellBack.Store(false) })
	logs := captureWarnings(t)
	pid, err := spawnWindowsDetached(context.Background(), "")
	if err != nil || pid != 23456 || len(rec.calls) != 2 {
		t.Fatalf("pid=%d err=%v calls=%d", pid, err, len(rec.calls))
	}
	primary, fallback := rec.calls[0], rec.calls[1]
	if !slices.Equal(primary.Args, argv) || !slices.Equal(fallback.Args, argv) || primary.Dir != tmp || fallback.Dir != tmp {
		t.Fatal("both attempts must run the same argv in the same cwd")
	}
	if !reflect.DeepEqual(primary.SysProcAttr, home.DetachAttr(true)) || !reflect.DeepEqual(fallback.SysProcAttr, home.DetachAttr(false)) {
		t.Fatalf("creation flags primary=%+v fallback=%+v", primary.SysProcAttr, fallback.SysProcAttr)
	}
	if primary.Stdin != nil || fallback.Stdin != nil || primary.Stdout != primary.Stderr || fallback.Stdout != fallback.Stderr {
		t.Fatal("null stdin and one shared log handle per attempt")
	}
	if primary.Stdout.(*os.File).Name() != fallback.Stdout.(*os.File).Name() {
		t.Fatal("both attempts log to the same stray log")
	}
	pe, fe := cmdEnv(primary), cmdEnv(fallback)
	if pe[windowsGatewayBreakawayEnv] != "1" || fe[windowsGatewayBreakawayEnv] != "0" {
		t.Fatalf("breakaway markers %q %q", pe[windowsGatewayBreakawayEnv], fe[windowsGatewayBreakawayEnv])
	}
	delete(pe, windowsGatewayBreakawayEnv)
	delete(fe, windowsGatewayBreakawayEnv)
	if !reflect.DeepEqual(pe, fe) {
		t.Fatal("the retry keeps every other variable")
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "5") || strings.Contains(lines[0], "do-not-log") || strings.Contains(lines[0], tmp) {
		t.Fatalf("warnings = %q", logs.String())
	}
	if !windowsBreakawayFellBack.Load() {
		t.Fatal("fallback flag not set")
	}
}

func TestStableWindowsGatewayWorkingDir(t *testing.T) {
	t.Run("test_stable_gateway_working_dir_uses_hermes_home", func(t *testing.T) {
		tmp := t.TempDir()
		h := filepath.Join(tmp, ".icebath")
		if err := os.Mkdir(h, 0o755); err != nil {
			t.Fatal(err)
		}
		schtasksSwap(t, &winHome, func(context.Context) string { return h })
		if got := stableGatewayWorkingDir(context.Background(), filepath.Join(tmp, "checkout")); got != h {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_stable_gateway_working_dir_falls_back_to_project_root", func(t *testing.T) {
		tmp := t.TempDir()
		project := filepath.Join(tmp, "checkout")
		schtasksSwap(t, &winHome, func(context.Context) string { return filepath.Join(tmp, "missing", ".icebath") })
		if got := stableGatewayWorkingDir(context.Background(), project); got != project {
			t.Fatalf("got %q", got)
		}
	})
}

func TestElevatedGatewayCommandUsesHiddenConsolePython(t *testing.T) {
	schtasksWindowsHost(t)
	root := t.TempDir()
	profileHome := filepath.Join(root, "profiles", "alice")
	if err := os.MkdirAll(profileHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", profileHome)
	type call struct{ file, params, cwd string }
	var calls []call
	schtasksSwap(t, &winShellExecute, func(file, params, cwd string) (uintptr, error) {
		calls = append(calls, call{file, params, cwd})
		return 33, nil
	})
	ok, err := launchElevatedGatewayCommand(context.Background(), "install", []string{"--start-now", "--elevated-handoff"})
	if err != nil || !ok || len(calls) != 1 {
		t.Fatalf("ok=%v err=%v calls=%d", ok, err, len(calls))
	}
	exe, _ := os.Executable()
	if calls[0].file != exe || !strings.Contains(calls[0].params, "--profile alice gateway install --start-now --elevated-handoff") || calls[0].cwd == "" {
		t.Fatalf("call = %+v", calls[0])
	}
}

func TestInstallScheduledTaskRecreatesInsteadOfChange(t *testing.T) {
	tmp := t.TempDir()
	scriptPath := filepath.Join(tmp, "Icebath_Gateway_alice.cmd")
	schtasksSwap(t, &winResolveTaskUser, func() string { return `DOMAIN\\alice` })
	var calls [][]string
	xmlSeen := ""
	schtasksSwap(t, &winExecSchtasks, func(args []string) (int, string, string, error) {
		calls = append(calls, slices.Clone(args))
		switch args[0] {
		case "/Delete":
			return 0, "SUCCESS", "", nil
		case "/Create":
			xmlSeen = readUTF16File(t, args[slices.Index(args, "/XML")+1])
			return 0, "SUCCESS", "", nil
		}
		t.Fatalf("unexpected schtasks args: %q", args)
		return 0, "", "", nil
	})
	ok, _, err := installWindowsScheduledTask(context.Background(), "Icebath_Gateway_alice", scriptPath)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	for _, c := range calls {
		if slices.Contains(c, "/Change") {
			t.Fatal("/Change must never be used")
		}
	}
	if !slices.Equal(calls[0][:4], []string{"/Delete", "/F", "/TN", "Icebath_Gateway_alice"}) || calls[1][0] != "/Create" || !slices.Contains(calls[1], "/XML") || slices.Contains(calls[1], "/SC") {
		t.Fatalf("calls = %q", calls)
	}
	for _, want := range []string{"<Delay>PT30S</Delay>", "<StartWhenAvailable>true</StartWhenAvailable>", "<StopOnIdleEnd>false</StopOnIdleEnd>",
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>", "<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>", "<RestartOnFailure>", "<Count>999</Count>", "<Command>wscript.exe</Command>",
		"//B //Nologo", "Icebath_Gateway_alice.vbs"} {
		if !strings.Contains(xmlSeen, want) {
			t.Fatalf("task XML lacks %q", want)
		}
	}
	if strings.Contains(xmlSeen, "cmd.exe") {
		t.Fatal("the task must never launch cmd.exe")
	}
}

func TestGatewayVbsScriptIsConsoleLess(t *testing.T) {
	schtasksSwap(t, &winResolveDetachedPython, func(string) (string, string, []string) {
		return `C:\venv\Scripts\pythonw.exe`, `C:\venv`, nil
	})
	content, err := buildGatewayVbsScript(context.Background(), `C:\venv\Scripts\python.exe`, `C:\Icebath`, `C:\Icebath`, "--profile work")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`CreateObject("WScript.Shell")`, "pythonw.exe", pm.PinDefaultHomeFlag, "gateway run", ", 0, False",
		"ICEBATH_HOME", "PYTHONIOENCODING", "ICEBATH_GATEWAY_DETACHED", "VIRTUAL_ENV", "PYTHONPATH", "--profile", "work"} {
		if !strings.Contains(content, want) {
			t.Fatalf("launcher lacks %q", want)
		}
	}
	if strings.Contains(strings.ToLower(content), "cmd.exe") || !strings.HasSuffix(content, "\r\n") {
		t.Fatal("launcher must avoid cmd.exe and end with CRLF")
	}
}

func TestAtomicWriteLeavesNoStagingFileWhenSwapFails(t *testing.T) {
	startup := filepath.Join(t.TempDir(), "Startup")
	if err := os.Mkdir(startup, 0o755); err != nil {
		t.Fatal(err)
	}
	schtasksSwap(t, &winRename, func(string, string) error { return &os.LinkError{Op: "rename", Err: fs.ErrPermission} })
	err := windowsAtomicWrite(filepath.Join(startup, "Icebath_Gateway.vbs"), "launcher\r\n", filepath.Join(startup, "Icebath_Gateway.tmp"), false)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v", err)
	}
	if names := dirNames(t, startup); len(names) != 0 {
		t.Fatalf("leftovers %q", names)
	}
}

// Startup folder holding the vbs fallback and a legacy cmd entry with the naming seams pointed at them
func startupWithFallbackAndLegacyEntries(t *testing.T, dir string) (string, string) {
	startup := filepath.Join(dir, "Startup")
	if err := os.MkdirAll(startup, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "gateway-service", "Icebath_Gateway_alice.cmd")
	vbs, cmd := filepath.Join(startup, "Icebath_Gateway_alice.vbs"), filepath.Join(startup, "Icebath_Gateway_alice.cmd")
	text, err := buildStartupLauncher(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vbs, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cmd, []byte("@echo off"), 0o644); err != nil {
		t.Fatal(err)
	}
	schtasksSwap(t, &serviceGOOS, "windows")
	schtasksSwap(t, &winTaskName, func(context.Context) (string, error) { return "Icebath_Gateway_alice", nil })
	schtasksSwap(t, &winStartupEntryPath, func(context.Context) (string, error) { return vbs, nil })
	schtasksSwap(t, &winLegacyStartupEntryPath, func(context.Context) (string, error) { return cmd, nil })
	schtasksSwap(t, &winWriteTaskScript, func(context.Context) (string, error) { return script, nil })
	return startup, script
}

func TestScheduledTaskInstallRemovesStartupEntriesThatWouldDoubleLaunch(t *testing.T) {
	out := schtasksWindowsHost(t)
	startup, _ := startupWithFallbackAndLegacyEntries(t, t.TempDir())
	schtasksSwap(t, &winPromptInstallChoices, func(context.Context, *bool, *bool) (bool, bool) { return false, true })
	schtasksSwap(t, &winIsRunningAsAdmin, func() (bool, error) { return true, nil })
	schtasksSwap(t, &winInstallScheduledTask, func(context.Context, string, string) (bool, string, error) { return true, "created", nil })
	schtasksSwap(t, &winPrintNextSteps, func(context.Context) {})
	if err := WindowsInstall(context.Background(), WindowsInstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if names := dirNames(t, startup); len(names) != 0 {
		t.Fatalf("startup still holds %q", names)
	}
	if !strings.Contains(out.String(), "Removed redundant Windows login item") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestReconcileLeavesOneAutostartMechanism(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	startup, _ := startupWithFallbackAndLegacyEntries(t, tmp)
	registered := true
	schtasksSwap(t, &winIsTaskRegistered, func(context.Context) (bool, error) { return registered, nil })
	if entries, _ := RedundantAutostartEntries(ctx); len(entries) != 2 {
		t.Fatalf("redundant = %q", entries)
	}
	done, warnings, err := ReconcileAutostartLaunchers(ctx)
	if err != nil || len(done) != 2 || len(warnings) != 0 {
		t.Fatalf("done=%q warnings=%q err=%v", done, warnings, err)
	}
	if entries, _ := RedundantAutostartEntries(ctx); len(entries) != 0 {
		t.Fatalf("redundant after reconcile = %q", entries)
	}
	if names := dirNames(t, startup); len(names) != 0 {
		t.Fatalf("startup still holds %q", names)
	}
	registered = false
	startupWithFallbackAndLegacyEntries(t, filepath.Join(tmp, "no-task"))
	entries, _ := RedundantAutostartEntries(ctx)
	if len(entries) != 1 || filepath.Ext(entries[0]) != ".cmd" {
		t.Fatalf("redundant without task = %q", entries)
	}
	if _, _, err := ReconcileAutostartLaunchers(ctx); err != nil {
		t.Fatal(err)
	}
	if entries, _ := RedundantAutostartEntries(ctx); len(entries) != 0 {
		t.Fatalf("redundant after migration = %q", entries)
	}
	if names := dirNames(t, filepath.Join(tmp, "no-task", "Startup")); !slices.Equal(names, []string{"Icebath_Gateway_alice.vbs"}) {
		t.Fatalf("startup holds %q", names)
	}
}

func TestReconcileRewritesAnOlderStartupEntryAsUtf16(t *testing.T) {
	startup, script := startupWithFallbackAndLegacyEntries(t, filepath.Join(t.TempDir(), "Usuário 方舟"))
	if err := os.Remove(filepath.Join(startup, "Icebath_Gateway_alice.cmd")); err != nil {
		t.Fatal(err)
	}
	schtasksSwap(t, &winIsTaskRegistered, func(context.Context) (bool, error) { return false, nil })
	schtasksSwap(t, &winTaskScriptPath, func(context.Context) (string, error) { return script, nil })
	done, warnings, err := ReconcileAutostartLaunchers(context.Background())
	if err != nil || len(done) != 0 || len(warnings) != 0 {
		t.Fatalf("done=%q warnings=%q err=%v", done, warnings, err)
	}
	want, _ := buildStartupLauncher(script)
	if got := readUTF16File(t, filepath.Join(startup, "Icebath_Gateway_alice.vbs")); got != want {
		t.Fatalf("rewritten launcher = %q", got)
	}
	if names := dirNames(t, startup); !slices.Equal(names, []string{"Icebath_Gateway_alice.vbs"}) {
		t.Fatalf("startup holds %q", names)
	}
}

const preHardeningTaskXML = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.3" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Settings>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <UseUnifiedSchedulingEngine>true</UseUnifiedSchedulingEngine>
  </Settings>
  <Triggers>
    <LogonTrigger />
  </Triggers>
  <Actions Context="Author">
    <Exec>
      <Command>wscript.exe</Command>
      <Arguments>"C:\Users\me\.icebath\gateway-service\Icebath_Gateway.vbs"</Arguments>
    </Exec>
  </Actions>
</Task>
`

const driftLauncher = `C:\Users\me\.icebath\gateway-service\Icebath_Gateway.vbs`

func TestScheduledTaskDriftNamesMissingHardeningLeaves(t *testing.T) {
	template := buildScheduledTaskXML("Icebath_Gateway", driftLauncher, `PC\me`)
	want := []string{"missing: RestartOnFailure, LogonTrigger Delay", "launcher arguments differs", "version 1.3 vs 1.4"}
	if drift := CompareScheduledTaskDrift(preHardeningTaskXML, template); !slices.Equal(drift, want) {
		t.Fatalf("drift = %q", drift)
	}
	out := schtasksWindowsHost(t)
	schtasksSwap(t, &winExecSchtasks, func(args []string) (int, string, string, error) {
		if slices.Contains(args, "/XML") {
			return 0, preHardeningTaskXML, "", nil
		}
		return 0, "", "", nil
	})
	schtasksSwap(t, &winTaskScriptPath, func(context.Context) (string, error) { return winWithSuffix(driftLauncher, ".cmd"), nil })
	schtasksSwap(t, &winResolveTaskUser, func() string { return `PC\me` })
	if err := printScheduledTaskDrift(context.Background(), "Icebath_Gateway"); err != nil {
		t.Fatal(err)
	}
	printed := strings.Split(out.String(), "\n")
	if !strings.HasPrefix(printed[0], "⚠ Scheduled Task registration predates the current template (missing: RestartOnFailure") || !strings.Contains(printed[1], "icebath gateway install") {
		t.Fatalf("printed = %q", printed)
	}
}

func TestScheduledTaskDriftIsSilentWhenAlignedOrUnqueryable(t *testing.T) {
	template := buildScheduledTaskXML("Icebath_Gateway", driftLauncher, `PC\me`)
	exported := strings.Replace(template, `<UserId>PC\me</UserId>`, "<UserId>S-1-5-21-1-2-3-1001</UserId>", 1)
	if exported == template {
		t.Fatal("export fixture did not change")
	}
	if drift := CompareScheduledTaskDrift(exported, template); len(drift) != 0 {
		t.Fatalf("aligned drift = %q", drift)
	}
	if drift := CompareScheduledTaskDrift("not xml", template); len(drift) != 0 {
		t.Fatalf("unparsable drift = %q", drift)
	}
	out := schtasksWindowsHost(t)
	schtasksSwap(t, &winExecSchtasks, func([]string) (int, string, string, error) {
		return 1, "", "ERROR: The system cannot find the file specified.", nil
	})
	if err := printScheduledTaskDrift(context.Background(), "Icebath_Gateway"); err != nil || out.Len() != 0 {
		t.Fatalf("printed %q err=%v", out.String(), err)
	}
}

func TestReconcileScheduledTaskReregistersOnlyOnDrift(t *testing.T) {
	schtasksWindowsHost(t)
	scriptPath := filepath.Join(t.TempDir(), "gateway.cmd")
	template := buildScheduledTaskXML("Icebath_Gateway", winWithSuffix(scriptPath, ".vbs"), `PC\me`)
	var calls [][]string
	registered := preHardeningTaskXML
	schtasksSwap(t, &winExecSchtasks, func(args []string) (int, string, string, error) {
		calls = append(calls, slices.Clone(args))
		if slices.Contains(args, "/XML") && slices.Contains(args, "/Query") {
			return 0, registered, "", nil
		}
		if slices.Contains(args, "/Create") {
			registered = readUTF16File(t, args[slices.Index(args, "/XML")+1])
		}
		return 0, "", "", nil
	})
	schtasksSwap(t, &winWriteTaskScript, func(context.Context) (string, error) { return scriptPath, nil })
	schtasksSwap(t, &winTaskScriptPath, func(context.Context) (string, error) { return scriptPath, nil })
	schtasksSwap(t, &winResolveTaskUser, func() string { return `PC\me` })
	ok, err := ReconcileScheduledTask(context.Background(), "Icebath_Gateway")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	var mutations []string
	for _, c := range calls {
		if c[0] == "/Delete" || c[0] == "/Create" {
			mutations = append(mutations, c[0])
		}
	}
	if !slices.Equal(mutations, []string{"/Delete", "/Create"}) || !strings.Contains(registered, "<RestartOnFailure>") {
		t.Fatalf("mutations=%q", mutations)
	}
	if drift := CompareScheduledTaskDrift(registered, template); len(drift) != 0 {
		t.Fatalf("drift after repair = %q", drift)
	}
	calls = nil
	if ok, err := ReconcileScheduledTask(context.Background(), "Icebath_Gateway"); err != nil || ok {
		t.Fatalf("aligned reconcile ok=%v err=%v", ok, err)
	}
	for _, c := range calls {
		if c[0] == "/Delete" || c[0] == "/Create" {
			t.Fatal("an aligned task must not be re-registered")
		}
	}
}

func TestStdinInteractiveOnlyWhenIsattyAndAConsoleAnswersGetConsoleMode(t *testing.T) {
	if stdinIsInteractive(true, new(false)) || !stdinIsInteractive(true, new(true)) || stdinIsInteractive(false, new(true)) || !stdinIsInteractive(true, nil) {
		t.Fatal("isatty needs a console mode answer where that fact exists")
	}
}

// Uninstalled host whose start records installs and spawns instead of performing them
func arrangeUninstalledStart(t *testing.T) (*bytes.Buffer, *[]WindowsInstallOptions, *int) {
	out := schtasksWindowsHost(t)
	for _, k := range []string{windowsInstallStartOnLoginEnv, "ICEBATH_NONINTERACTIVE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	installs, spawns := &[]WindowsInstallOptions{}, new(0)
	schtasksSwap(t, &winPrintStartAttestationWarning, func(context.Context) {})
	schtasksSwap(t, &winGatewayPids, func(context.Context) ([]int, error) { return nil, nil })
	schtasksSwap(t, &winIsTaskRegistered, func(context.Context) (bool, error) { return false, nil })
	schtasksSwap(t, &winIsStartupEntryInstalled, func(context.Context) (bool, error) { return false, nil })
	schtasksSwap(t, &winInstall, func(_ context.Context, o WindowsInstallOptions) error { *installs = append(*installs, o); return nil })
	schtasksSwap(t, &winSpawnDetached, func(context.Context, string) (int, error) { *spawns++; return 4242, nil })
	schtasksSwap(t, &winReportGatewayStart, func(context.Context, string) error { return nil })
	schtasksSwap(t, &winStdinConsoleModeOK, func() *bool { return new(true) })
	schtasksSwap(t, &winStdoutIsatty, func() bool { return true })
	return out, installs, spawns
}

// Prompt double failing the test when asked
func promptMustNotRun(t *testing.T, why string) {
	schtasksSwap(t, &SetupPromptYesNo, func(context.Context, string, bool) bool { t.Fatal(why); return false })
}

func TestStartWithNulStdinStartsTheGatewayButNeverInstallsLoginPersistence(t *testing.T) {
	out, installs, spawns := arrangeUninstalledStart(t)
	schtasksSwap(t, &winStdinIsatty, func() bool { return true })
	schtasksSwap(t, &winStdinConsoleModeOK, func() *bool { return new(false) })
	promptMustNotRun(t, "no prompt on a NUL stdin")
	if err := WindowsStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*installs) != 0 || *spawns != 1 || !strings.Contains(out.String(), "icebath gateway install") {
		t.Fatalf("installs=%v spawns=%d out=%q", *installs, *spawns, out.String())
	}
}

func TestStartWithCapturedStdoutNeverAsksEvenOnAConsoleStdin(t *testing.T) {
	out, installs, spawns := arrangeUninstalledStart(t)
	schtasksSwap(t, &winStdinIsatty, func() bool { return true })
	schtasksSwap(t, &winStdoutIsatty, func() bool { return false })
	promptMustNotRun(t, "no prompt into captured stdout")
	if err := WindowsStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*installs) != 0 || *spawns != 1 || !strings.Contains(out.String(), "icebath gateway install") {
		t.Fatalf("installs=%v spawns=%d out=%q", *installs, *spawns, out.String())
	}
}

func TestStartWithoutTtyStartsTheGatewayButNeverInstallsLoginPersistence(t *testing.T) {
	out, installs, spawns := arrangeUninstalledStart(t)
	schtasksSwap(t, &winStdinIsatty, func() bool { return false })
	promptMustNotRun(t, "no prompt without a TTY")
	if err := WindowsStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if len(*installs) != 0 || *spawns != 1 || !strings.Contains(text, "icebath gateway install") || strings.Contains(text, "did not complete") {
		t.Fatalf("installs=%v spawns=%d out=%q", *installs, *spawns, text)
	}
}

func TestStartOnTtyHandsBothAnswersToInstallAndHonoursTheEnvOptOut(t *testing.T) {
	_, installs, spawns := arrangeUninstalledStart(t)
	schtasksSwap(t, &winStdinIsatty, func() bool { return true })
	schtasksSwap(t, &SetupPromptYesNo, func(context.Context, string, bool) bool { return true })
	if err := WindowsStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := WindowsInstallOptions{StartNow: new(true), StartOnLogin: new(true)}
	if len(*installs) != 1 || !reflect.DeepEqual((*installs)[0], want) || *spawns != 0 {
		t.Fatalf("installs=%+v spawns=%d", *installs, *spawns)
	}
	*installs = nil
	t.Setenv(windowsInstallStartOnLoginEnv, "0")
	promptMustNotRun(t, "env override must skip the prompt")
	if err := WindowsStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*installs) != 0 || *spawns != 1 {
		t.Fatalf("installs=%+v spawns=%d", *installs, *spawns)
	}
}

func TestWindowlessGatewayRestartSpec(t *testing.T) {
	t.Run("test_noop_on_non_windows", func(t *testing.T) {
		schtasksSwap(t, &serviceGOOS, "linux")
		argv := []string{"/path/venv/bin/icebath", "gateway", "run"}
		got, cwd, env := windowlessGatewayRestartSpec(context.Background(), slices.Clone(argv), "")
		if !slices.Equal(got, argv) || cwd != "" || len(env) != 0 {
			t.Fatalf("got=%q cwd=%q env=%v", got, cwd, env)
		}
	})
	t.Run("test_empty_argv_is_safe", func(t *testing.T) {
		got, cwd, env := windowlessGatewayRestartSpec(context.Background(), []string{}, "")
		if len(got) != 0 || cwd != "" || len(env) != 0 {
			t.Fatalf("got=%q cwd=%q env=%v", got, cwd, env)
		}
	})
	t.Run("test_windows_keeps_console_python_and_preserves_tail", func(t *testing.T) {
		schtasksSwap(t, &serviceGOOS, "windows")
		h := t.TempDir()
		schtasksSwap(t, &winHome, func(context.Context) string { return h })
		argv := []string{"C:/venv/Scripts/python.exe", pm.PinDefaultHomeFlag, "--profile", "work", "gateway", "run", "--replace"}
		got, cwd, env := windowlessGatewayRestartSpec(context.Background(), slices.Clone(argv), "")
		if got[0] != argv[0] || !slices.Equal(got[1:], argv[1:]) || cwd != h {
			t.Fatalf("got=%q cwd=%q", got, cwd)
		}
		if env["VIRTUAL_ENV"] != filepath.Dir(filepath.Dir(argv[0])) || env["PYTHONPATH"] == "" {
			t.Fatalf("env = %v", env)
		}
	})
}

func TestRestartStampsTheMarkerAfterTheOldGatewayIsGone(t *testing.T) {
	schtasksWindowsHost(t)
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	marker := filepath.Join(tmp, PlannedRestartMarkerFilename)
	seen := map[string]bool{}
	schtasksSwap(t, &winWaitForGatewayAbsent, func(context.Context, float64) (bool, error) { return true, nil })
	schtasksSwap(t, &winWaitForAPIServerPortFree, func(context.Context, float64) (bool, error) { return true, nil })
	schtasksSwap(t, &winStop, func(context.Context) error {
		if _, ok := seen["marker_at_stop"]; !ok {
			seen["marker_at_stop"] = winExists(marker)
		}
		return nil
	})
	schtasksSwap(t, &winStart, func(context.Context) error {
		if _, ok := seen["marker_at_start"]; !ok {
			seen["marker_at_start"] = winExists(marker)
		}
		return nil
	})
	schtasksSwap(t, &winWaitForGatewayReady, func(context.Context, float64) ([]int, error) { return []int{1}, nil })
	if err := WindowsRestart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen["marker_at_stop"] || !seen["marker_at_start"] {
		t.Fatalf("the draining gateway must not see the marker and the next boot must find it: %v", seen)
	}
	schtasksSwap(t, &winStop, func(context.Context) error { return nil })
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	schtasksSwap(t, &winWaitForGatewayReady, func(context.Context, float64) ([]int, error) {
		return nil, errors.New("Gateway restart did not produce a running gateway process.")
	})
	if err := WindowsRestart(context.Background()); err == nil {
		t.Fatal("restart must fail when no gateway comes back")
	}
	if !winExists(marker) {
		t.Fatal("the marker stays for the next boot")
	}
}
