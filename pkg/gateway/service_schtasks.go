package gateway

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	term "github.com/charmbracelet/x/term"
	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pydeadline"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	windowsTaskNameDefault        = "Icebath_Gateway"
	windowsTaskDescription        = "Icebath Agent Gateway - Messaging Platform Integration"
	windowsTaskLogonDelay         = "PT30S"
	windowsTaskRestartInterval    = "PT1M"
	windowsTaskRestartCount       = 999
	startAttestationMaxAgeS       = 24 * 3600.0
	attestationCreateTimeTolerS   = 2.0
	attestationClockSlackS        = 60.0
	windowsAccessDeniedWords      = `access is denied|acceso denegado|přístup byl odepřen|拒绝访问|拒絕存取|アクセスが拒否されました|액세스가 거부되었습니다`
	windowsReadyIntervalS         = 0.4
	windowsReadyConfirmS          = 2.0
	windowsElevatedHandoffEnv     = "ICEBATH_GATEWAY_ELEVATED_HANDOFF"
	windowsInstallStartNowEnv     = "ICEBATH_GATEWAY_INSTALL_START_NOW"
	windowsInstallStartOnLoginEnv = "ICEBATH_GATEWAY_INSTALL_START_ON_LOGIN"
)

var (
	errGatewayWindowsOnly    = errors.New("gateway_windows is Windows-only")
	windowsFallbackRe        = regexp.MustCompile(`(?i)(` + windowsAccessDeniedWords + `|schtasks timed out|schtasks produced no output)`)
	windowsAccessDeniedRe    = regexp.MustCompile(`(?i)(` + windowsAccessDeniedWords + `)`)
	windowsFilenameBadRe     = regexp.MustCompile("[<>:\"/\\\\|?*\x00-\x1f]")
	windowsGatewayEnv        = [][2]string{{"PYTHONIOENCODING", "utf-8"}, {"ICEBATH_GATEWAY_DETACHED", "1"}, {"ICEBATH_SUPERVISED_CHILD", "1"}}
	windowsTaskDriftLeaves   = [][2]string{{"Task/Settings/RestartOnFailure/Interval", "RestartOnFailure"}, {"Task/Triggers/LogonTrigger/Delay", "LogonTrigger Delay"}, {"Task/Actions/Exec/Arguments", "launcher arguments"}}
	windowsBreakawayFellBack atomic.Bool
)

// Options of a Windows gateway install, nil choices asked before any elevation
type WindowsInstallOptions struct {
	Force           bool
	StartNow        *bool
	StartOnLogin    *bool
	ElevatedHandoff bool
}

// Yes or no question of the setup wizard, wired by cmd/icebath to the setup prompt
var SetupPromptYesNo func(ctx context.Context, question string, def bool) bool

// Swappable seams of the Windows backend, the functions its tests replace
var (
	winHome                         func(ctx context.Context) string
	winExecSchtasks                 func(args []string) (int, string, string, error)
	winTaskName                     func(ctx context.Context) (string, error)
	winTaskScriptPath               func(ctx context.Context) (string, error)
	winStartupDir                   func() (string, error)
	winStartupEntryPath             func(ctx context.Context) (string, error)
	winLegacyStartupEntryPath       func(ctx context.Context) (string, error)
	winIsTaskRegistered             func(ctx context.Context) (bool, error)
	winWriteTaskScript              func(ctx context.Context) (string, error)
	winResolveTaskUser              func() string
	winInstallScheduledTask         func(ctx context.Context, taskName, scriptPath string) (bool, string, error)
	winIsRunningAsAdmin             func() (bool, error)
	winPromptInstallChoices         func(ctx context.Context, startNow, startOnLogin *bool) (bool, bool)
	winPrintNextSteps               func(ctx context.Context)
	winGatewayPids                  func(ctx context.Context) ([]int, error)
	winSpawnDetached                func(ctx context.Context, homeOverride string) (int, error)
	winReportGatewayStart           func(ctx context.Context, via string) error
	winWaitForGatewayReady          func(ctx context.Context, timeoutS float64) ([]int, error)
	winPrintStartAttestationWarning func(ctx context.Context)
	winRemoveLegacyLaunchers        func(ctx context.Context) error
	winLaunchElevatedInstall        func(ctx context.Context, force bool, startNow, startOnLogin *bool) (bool, error)
	winStdoutIsatty                 func() bool
	winStdinIsatty                  func() bool
	winStdinConsoleModeOK           func() *bool
	winShellExecute                 func(file, params, cwd string) (uintptr, error)
	winPopen                        func(cmd *exec.Cmd) error
	winLauncherBinary               func() string
	winRunningPid                   func(pidPath string, cleanupStale bool) (int, bool, error)
	winStop                         func(ctx context.Context) error
	winStart                        func(ctx context.Context) error
	winWaitForGatewayAbsent         func(ctx context.Context, timeoutS float64) (bool, error)
	winWaitForAPIServerPortFree     func(ctx context.Context, timeout float64) (bool, error)
	winMarkPlannedRestart           func(ctx context.Context)
	winSleep                        = time.Sleep
	winRename                       = os.Rename
	winRemove                       = os.Remove
	winIsStartupEntryInstalled      func(ctx context.Context) (bool, error)
	winInstall                      func(ctx context.Context, o WindowsInstallOptions) error
	winGatewayArgv                  func(ctx context.Context, homeOverride string) ([]string, string, map[string]string, error)
	winResolveDetachedPython        func(pythonExe string) (string, string, []string)
)

// Installs the default seams, assigned here so no initializer cycle forms
func init() {
	winHome = func(ctx context.Context) string { return home.Dir(ctx) }
	winExecSchtasks = execSchtasksOnHost
	winTaskName = windowsTaskName
	winTaskScriptPath = windowsTaskScriptPath
	winStartupDir = windowsStartupDir
	winStartupEntryPath = func(ctx context.Context) (string, error) { return windowsStartupEntry(ctx, ".vbs") }
	winLegacyStartupEntryPath = func(ctx context.Context) (string, error) { return windowsStartupEntry(ctx, ".cmd") }
	winIsTaskRegistered = windowsIsTaskRegistered
	winWriteTaskScript = writeWindowsTaskScript
	winResolveTaskUser = resolveWindowsTaskUser
	winInstallScheduledTask = installWindowsScheduledTask
	winIsRunningAsAdmin = windowsIsRunningAsAdmin
	winPromptInstallChoices = promptWindowsInstallChoices
	winPrintNextSteps = printWindowsNextSteps
	winGatewayPids = func(ctx context.Context) ([]int, error) { return FindGatewayPIDsFor(ctx, nil, false) }
	winSpawnDetached = spawnWindowsDetached
	winReportGatewayStart = reportWindowsGatewayStart
	winWaitForGatewayReady = waitForWindowsGatewayReady
	winPrintStartAttestationWarning = printStartAttestationWarning
	winRemoveLegacyLaunchers = RemoveLegacyLaunchers
	winLaunchElevatedInstall = launchElevatedInstall
	winStdoutIsatty = func() bool { return term.IsTerminal(os.Stdout.Fd()) }
	winStdinIsatty = func() bool { return term.IsTerminal(os.Stdin.Fd()) }
	winStdinConsoleModeOK = stdinConsoleModeOK
	winShellExecute = shellExecuteRunas
	winPopen = func(cmd *exec.Cmd) error {
		if err := cmd.Start(); err != nil {
			return err
		}
		go cmd.Wait()
		return nil
	}
	winLauncherBinary = func() string {
		if binary := pm.ResolveInstallBinary(serviceDefProjectRoot()); binary != "" {
			return binary
		}
		exe, _ := os.Executable()
		return exe
	}
	winRunningPid = GetRunningPid
	winStop = WindowsStop
	winStart = WindowsStart
	winWaitForGatewayAbsent = waitForWindowsGatewayAbsent
	winWaitForAPIServerPortFree = waitForAPIServerPortFree
	winMarkPlannedRestart = func(ctx context.Context) { WritePlannedRestartMarker(ctx, false, false) }
	winIsStartupEntryInstalled = windowsStartupEntryInstalled
	winInstall = WindowsInstall
	winGatewayArgv = windowsGatewayArgv
	winResolveDetachedPython = resolveDetachedPython
}

// Writes one line to the lifecycle output like print
func winPrintln(text string) { fmt.Fprintln(lifecycleOut, text) }

// RuntimeError of every Windows backend entry point on another host
func assertWindows() error {
	if serviceGOOS != "windows" {
		return errGatewayWindowsOnly
	}
	return nil
}

// Path with its suffix replaced, the input unchanged when pathlib would refuse the swap
func winWithSuffix(p, suffix string) string {
	out, err := pypath.WithSuffix(p, suffix)
	if err != nil {
		return p
	}
	return out
}

// Stripped, unquoted, forward slashed and casefolded path text for prefix comparisons
func normalizeWindowsPath(value string) string {
	v := strings.TrimLeft(pystr.Strip(value), `"`)
	return pystr.Casefold(strings.TrimRight(strings.ReplaceAll(v, `\`, "/"), "/"))
}

// Path rendered under the configured home spelling when it resolves inside the home
func preserveHomePath(ctx context.Context, p string) string {
	h := winHome(ctx)
	resolvedHome, err1 := pypath.Resolve(h)
	resolved, err2 := pypath.Resolve(p)
	if err1 != nil || err2 != nil {
		return pypath.PathStr(p)
	}
	sep := string(filepath.Separator)
	homeKey, candidateKey := pypath.Normcase(resolvedHome), pypath.Normcase(resolved)
	if candidateKey == homeKey {
		return pypath.PathStr(h)
	}
	if strings.HasPrefix(candidateKey, strings.TrimRight(homeKey, sep)+sep) {
		rel := strings.TrimLeft(resolved[len(strings.TrimRight(resolvedHome, sep)):], sep)
		return pypath.PathJoin(h, rel)
	}
	return pypath.PathStr(p)
}

// One cmd.exe argument inside a cmd file, embedded quotes doubled and newlines refused
func quoteCmdScriptArg(value string) (string, error) {
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("refusing to quote value containing newline: %s", pyrepr.Repr(value))
	}
	if value == "" {
		return `""`, nil
	}
	if !strings.ContainsAny(value, " \t\"") {
		return value, nil
	}
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`, nil
}

// VBScript double quoted literal with embedded quotes doubled and newlines refused
func quoteVbsString(value string) (string, error) {
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("refusing to quote VBScript value containing newline: %s", pyrepr.Repr(value))
	}
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`, nil
}

// Whether a schtasks failure means falling back to the Startup folder
func windowsShouldFallBack(code int, detail string) bool {
	return code == 124 || windowsFallbackRe.MatchString(detail)
}

// Whether schtasks output says access is denied in any shipped locale
func windowsIsAccessDenied(detail string) bool { return windowsAccessDeniedRe.MatchString(detail) }

// Whether this Windows process runs elevated
func windowsIsRunningAsAdmin() (bool, error) {
	if err := assertWindows(); err != nil {
		return false, err
	}
	return home.ProcessIsElevated(), nil
}

// CLI arguments that keep the current profile
func currentProfileCLIArgs(ctx context.Context) ([]string, error) {
	arg, err := profileArg(ctx, "", "")
	if err != nil || arg == "" {
		return nil, err
	}
	return pyshlex.SplitPOSIX(arg, false)
}

// Launches an elevated hidden gateway subcommand through UAC, true once handed off
func launchElevatedGatewayCommand(ctx context.Context, command string, extra []string) (bool, error) {
	if err := assertWindows(); err != nil {
		return false, err
	}
	selector, err := currentProfileCLIArgs(ctx)
	if err != nil {
		return false, err
	}
	args := append(append(selector, "gateway", command), extra...)
	exe, err := os.Executable()
	if err != nil {
		winPrintln(fmt.Sprintf("⚠ Could not launch elevated gateway %s prompt: %v", command, err))
		return false, nil
	}
	result, err := winShellExecute(exe, pysubprocess.List2cmdline(args), serviceDefProjectRoot())
	if err != nil {
		winPrintln(fmt.Sprintf("⚠ Could not launch elevated gateway %s prompt: %v", command, err))
		return false, nil
	}
	if result <= 32 {
		winPrintln(fmt.Sprintf("⚠ Elevated gateway %s prompt was not started (ShellExecuteW=%d)", command, result))
		return false, nil
	}
	return true, nil
}

// Launches the elevated install with every decision passed as flags and environment
func launchElevatedInstall(ctx context.Context, force bool, startNow, startOnLogin *bool) (bool, error) {
	overrides := [][2]string{{windowsElevatedHandoffEnv, "1"}}
	extra := []string{"--elevated-handoff"}
	if force {
		extra = append(extra, "--force")
	}
	for _, c := range []struct {
		choice *bool
		env    string
		flag   string
	}{{startNow, windowsInstallStartNowEnv, "start-now"}, {startOnLogin, windowsInstallStartOnLoginEnv, "start-on-login"}} {
		if c.choice == nil {
			continue
		}
		if *c.choice {
			overrides, extra = append(overrides, [2]string{c.env, "1"}), append(extra, "--"+c.flag)
		} else {
			overrides, extra = append(overrides, [2]string{c.env, "0"}), append(extra, "--no-"+c.flag)
		}
	}
	type saved struct {
		value string
		set   bool
	}
	old := map[string]saved{}
	for _, kv := range overrides {
		v, ok := os.LookupEnv(kv[0])
		old[kv[0]] = saved{v, ok}
	}
	defer func() {
		for key, s := range old {
			if s.set {
				os.Setenv(key, s.value)
			} else {
				os.Unsetenv(key)
			}
		}
	}()
	for _, kv := range overrides {
		os.Setenv(kv[0], kv[1])
	}
	return launchElevatedGatewayCommand(ctx, "install", extra)
}

// Scheduled Task name scoped per profile
func windowsTaskName(ctx context.Context) (string, error) {
	if err := assertWindows(); err != nil {
		return "", err
	}
	suffix, err := ProfileSuffix(ctx)
	if err != nil {
		return "", err
	}
	if suffix != "" {
		return windowsTaskNameDefault + "_" + suffix, nil
	}
	return windowsTaskNameDefault, nil
}

// Characters illegal in Windows filenames replaced by underscores
func sanitizeWindowsFilename(value string) string {
	return windowsFilenameBadRe.ReplaceAllString(value, "_")
}

// Generated gateway.cmd wrapper under the home gateway-service directory, created on demand
func windowsTaskScriptPath(ctx context.Context) (string, error) {
	if err := assertWindows(); err != nil {
		return "", err
	}
	dir := filepath.Join(winHome(ctx), "gateway-service")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	name, err := winTaskName(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sanitizeWindowsFilename(name)+".cmd"), nil
}

// Startup folder under APPDATA, else under USERPROFILE or HOME AppData Roaming
func windowsStartupDir() (string, error) {
	tail := []string{"Microsoft", "Windows", "Start Menu", "Programs", "Startup"}
	if appdata := pystr.Strip(os.Getenv("APPDATA")); appdata != "" {
		return filepath.Join(append([]string{appdata}, tail...)...), nil
	}
	profile := pystr.Strip(os.Getenv("USERPROFILE"))
	if profile == "" {
		profile = pystr.Strip(os.Getenv("HOME"))
	}
	if profile == "" {
		return "", errors.New("neither APPDATA nor USERPROFILE is set — cannot resolve Startup folder")
	}
	return filepath.Join(append([]string{profile, "AppData", "Roaming"}, tail...)...), nil
}

// Startup folder entry named for the task with the given extension
func windowsStartupEntry(ctx context.Context, ext string) (string, error) {
	if err := assertWindows(); err != nil {
		return "", err
	}
	dir, err := winStartupDir()
	if err != nil {
		return "", err
	}
	name, err := winTaskName(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sanitizeWindowsFilename(name)+ext), nil
}

// Startup folder staging file, also the debris an older failed swap left behind
func windowsStartupStagingPath(ctx context.Context) (string, error) {
	entry, err := winStartupEntryPath(ctx)
	if err != nil {
		return "", err
	}
	return winWithSuffix(entry, ".tmp"), nil
}

// Home when it is a directory so a moved checkout cannot fail the launcher cd, else the project root
func stableGatewayWorkingDir(ctx context.Context, projectRoot string) string {
	if h := winHome(ctx); h != "" && isDirSafe(h) {
		return pypath.PathStr(h)
	}
	return projectRoot
}

// Gateway run argv through the install binary with the profile selector
func windowsGatewayRunArgv(pythonExe, profileArg string) []string {
	args := append(strings.Fields(profileArg), "gateway", "run")
	return pm.InstallRuntimeCommand(serviceDefProjectRoot(), args, pm.LaunchCommandOptions{Binary: pythonExe})
}

// Launcher binary, working directory, home and profile argument of generated launchers
func windowsLauncherSettings(ctx context.Context, homeOverride string) (string, string, string, string, error) {
	h := homeOverride
	if h == "" {
		h = winHome(ctx)
	}
	arg, err := profileArg(ctx, h, "")
	if err != nil {
		return "", "", "", "", err
	}
	return preserveHomePath(ctx, winLauncherBinary()), stableGatewayWorkingDir(ctx, serviceDefProjectRoot()), h, arg, nil
}

// Project root and extra entries rendered under the home spelling for launcher PYTHONPATH
func windowsLauncherPythonpathEntries(ctx context.Context, extra []string) []string {
	entries := []string{preserveHomePath(ctx, serviceDefProjectRoot())}
	for _, e := range extra {
		entries = append(entries, preserveHomePath(ctx, e))
	}
	return entries
}

// Console interpreter for hidden detached runs, swapping a legacy pythonw for its python sibling
func resolveDetachedPython(pythonExe string) (string, string, []string) {
	p := pythonExe
	if name := strings.ToLower(filepath.Base(p)); name == "pythonw.exe" || name == "pythonw" {
		siblingName := "python"
		if filepath.Ext(p) != "" {
			siblingName = "python.exe"
		}
		sibling := filepath.Join(filepath.Dir(p), siblingName)
		if _, err := os.Stat(sibling); err == nil {
			p, pythonExe = sibling, sibling
		}
	}
	return pythonExe, filepath.Dir(filepath.Dir(p)), []string{}
}

// Prepends the non empty entries to the inherited PYTHONPATH in the overlay
func prependPythonpath(overlay map[string]string, entries []string) {
	var clean []string
	for _, e := range entries {
		if e != "" {
			clean = append(clean, e)
		}
	}
	if len(clean) == 0 {
		return
	}
	if existing := os.Getenv("PYTHONPATH"); existing != "" {
		clean = append(clean, existing)
	}
	overlay["PYTHONPATH"] = strings.Join(clean, string(os.PathListSeparator))
}

// CRLF gateway.cmd wrapper setting the gateway environment then running the gateway
func buildGatewayCmdScript(ctx context.Context, pythonPath, workingDir, icebathHome, profileArg string) (string, error) {
	exe, venv, extra := winResolveDetachedPython(pythonPath)
	pythonpath := strings.Join(append(windowsLauncherPythonpathEntries(ctx, extra), "%PYTHONPATH%"), ";")
	cd, err := quoteCmdScriptArg(workingDir)
	if err != nil {
		return "", err
	}
	lines := []string{"@echo off", "rem " + windowsTaskDescription, "cd /d " + cd, `set "ICEBATH_HOME=` + icebathHome + `"`}
	for _, kv := range windowsGatewayEnv {
		lines = append(lines, `set "`+kv[0]+"="+kv[1]+`"`)
	}
	lines = append(lines, `set "VIRTUAL_ENV=`+preserveHomePath(ctx, venv)+`"`, `set "PYTHONPATH=`+pythonpath+`"`)
	var quoted []string
	for _, a := range windowsGatewayRunArgv(exe, profileArg) {
		q, err := quoteCmdScriptArg(a)
		if err != nil {
			return "", err
		}
		quoted = append(quoted, q)
	}
	lines = append(lines, strings.Join(quoted, " "), "exit /b 0")
	return strings.Join(lines, "\r\n") + "\r\n", nil
}

// Quotes every value as a VBScript literal, the first refusal returned
func quoteVbsAll(values ...string) ([]string, error) {
	out := make([]string, len(values))
	for i, v := range values {
		q, err := quoteVbsString(v)
		if err != nil {
			return nil, err
		}
		out[i] = q
	}
	return out, nil
}

// CRLF hidden console gateway.vbs launcher run through wscript so logon never sends it console close events
func buildGatewayVbsScript(ctx context.Context, pythonPath, workingDir, icebathHome, profileArg string) (string, error) {
	exe, venv, extra := winResolveDetachedPython(pythonPath)
	commandLine := pysubprocess.List2cmdline(windowsGatewayRunArgv(exe, profileArg))
	static := strings.Join(windowsLauncherPythonpathEntries(ctx, extra), string(os.PathListSeparator))
	head, err := quoteVbsAll("ICEBATH_HOME", icebathHome)
	if err != nil {
		return "", err
	}
	lines := []string{"' " + windowsTaskDescription, "Option Explicit", "Dim sh, env, existing_pp", `Set sh = CreateObject("WScript.Shell")`, `Set env = sh.Environment("PROCESS")`,
		fmt.Sprintf("env.Item(%s) = %s", head[0], head[1])}
	for _, kv := range windowsGatewayEnv {
		q, err := quoteVbsAll(kv[0], kv[1])
		if err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("env.Item(%s) = %s", q[0], q[1]))
	}
	q, err := quoteVbsAll("VIRTUAL_ENV", preserveHomePath(ctx, venv), "PYTHONPATH", static+string(os.PathListSeparator), static, workingDir, commandLine)
	if err != nil {
		return "", err
	}
	lines = append(lines, fmt.Sprintf("env.Item(%s) = %s", q[0], q[1]),
		fmt.Sprintf("existing_pp = env.Item(%s)", q[2]),
		"If Len(existing_pp) > 0 Then",
		fmt.Sprintf("  env.Item(%s) = %s & existing_pp", q[2], q[3]),
		"Else",
		fmt.Sprintf("  env.Item(%s) = %s", q[2], q[4]),
		"End If",
		"sh.CurrentDirectory = "+q[5],
		fmt.Sprintf("sh.Run %s, 0, False", q[6]))
	return strings.Join(lines, "\r\n") + "\r\n", nil
}

// Startup folder vbs that runs the task launcher hidden and quits silently once the target is gone
func buildStartupLauncher(scriptPath string) (string, error) {
	target := winWithSuffix(scriptPath, ".vbs")
	q, err := quoteVbsAll(target, pysubprocess.List2cmdline([]string{"wscript.exe", target}))
	if err != nil {
		return "", err
	}
	lines := []string{"' " + windowsTaskDescription, "Option Explicit", "Dim fso, sh, target", "target = " + q[0],
		`Set fso = CreateObject("Scripting.FileSystemObject")`, "If Not fso.FileExists(target) Then WScript.Quit 0",
		`Set sh = CreateObject("WScript.Shell")`, fmt.Sprintf("sh.Run %s, 0, False", q[1])}
	return strings.Join(lines, "\r\n") + "\r\n", nil
}

// Writes the gateway.cmd wrapper and the UTF-16 vbs launcher beside it, returning the cmd path
func writeWindowsTaskScript(ctx context.Context) (string, error) {
	if err := assertWindows(); err != nil {
		return "", err
	}
	pythonPath, workingDir, h, arg, err := windowsLauncherSettings(ctx, "")
	if err != nil {
		return "", err
	}
	scriptPath, err := winTaskScriptPath(ctx)
	if err != nil {
		return "", err
	}
	cmdText, err := buildGatewayCmdScript(ctx, pythonPath, workingDir, h, arg)
	if err != nil {
		return "", err
	}
	if err := windowsAtomicWrite(scriptPath, cmdText, winWithSuffix(scriptPath, ".tmp"), false); err != nil {
		return "", err
	}
	vbsPath := winWithSuffix(scriptPath, ".vbs")
	vbsText, err := buildGatewayVbsScript(ctx, pythonPath, workingDir, h, arg)
	if err != nil {
		return "", err
	}
	if err := windowsAtomicWrite(vbsPath, vbsText, vbsPath+".tmp", true); err != nil {
		return "", err
	}
	return scriptPath, nil
}

// Text encoded as UTF-8, or as BOM led little endian UTF-16 for Windows Script Host
func windowsEncodeText(content string, asUTF16 bool) []byte {
	if !asUTF16 {
		return []byte(content)
	}
	units := utf16.Encode(append([]rune{0xfeff}, []rune(content)...))
	out := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(out[2*i:], u)
	}
	return out
}

// Writes content verbatim through tmp then renames over path, removing tmp even when the rename fails
func windowsAtomicWrite(path, content, tmp string, asUTF16 bool) error {
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, windowsEncodeText(content, asUTF16), 0o666); err != nil {
		return err
	}
	return winRename(tmp, path)
}

// DOMAIN backslash USER when known, else the bare user name, else empty
func resolveWindowsTaskUser() string {
	username := os.Getenv("USERNAME")
	if username == "" {
		username = os.Getenv("USER")
	}
	if username == "" {
		username = os.Getenv("LOGNAME")
	}
	if username == "" || strings.Contains(username, `\`) {
		return username
	}
	if domain := os.Getenv("USERDOMAIN"); domain != "" {
		return domain + `\` + username
	}
	return username
}

// Ampersand and angle brackets escaped like xml.sax.saxutils.escape
func saxEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// Task Scheduler XML with long running defaults that runs the vbs launcher through wscript
func buildScheduledTaskXML(taskName, launcherPath, user string) string {
	principal := ""
	if user != "" {
		principal = "\n      <UserId>" + saxEscape(user) + "</UserId>"
	}
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>` + saxEscape(windowsTaskDescription) + `</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <Delay>` + windowsTaskLogonDelay + `</Delay>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">` + principal + `
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure>
      <Interval>` + windowsTaskRestartInterval + `</Interval>
      <Count>` + strconv.Itoa(windowsTaskRestartCount) + `</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>wscript.exe</Command>
      <Arguments>//B //Nologo "` + saxEscape(launcherPath) + `"</Arguments>
    </Exec>
  </Actions>
</Task>
`
}

// First non empty of the texts like Python's or chain
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Deletes then creates the Scheduled Task from XML, never /Change, reporting success and detail
func installWindowsScheduledTask(ctx context.Context, taskName, scriptPath string) (bool, string, error) {
	deleteCode, deleteOut, deleteErr, err := winExecSchtasks([]string{"/Delete", "/F", "/TN", taskName})
	if err != nil {
		return false, "", err
	}
	deleteDetail := pystr.Strip(firstNonEmpty(deleteErr, deleteOut))
	if strings.Contains(strings.ToLower(deleteDetail), "cannot find") {
		deleteDetail = ""
	}
	if deleteCode != 0 && deleteDetail != "" && windowsIsAccessDenied(deleteDetail) {
		return false, fmt.Sprintf("schtasks /Delete failed (code %d): %s", deleteCode, deleteDetail), nil
	}
	user := winResolveTaskUser()
	launcherPath := winWithSuffix(scriptPath, ".vbs")
	xmlPath := winWithSuffix(launcherPath, ".task.xml")
	if err := os.WriteFile(xmlPath, windowsEncodeText(buildScheduledTaskXML(taskName, launcherPath, user), true), 0o666); err != nil {
		return false, "", err
	}
	defer os.Remove(xmlPath)
	base := []string{"/Create", "/F", "/TN", taskName, "/XML", xmlPath}
	variants := [][]string{base}
	if user != "" {
		variants = [][]string{append(slices.Clone(base), "/RU", user, "/NP", "/IT"), base}
	}
	lastCode, lastErr := 1, ""
	for _, argv := range variants {
		code, out, errText, err := winExecSchtasks(argv)
		if err != nil {
			return false, "", err
		}
		if code == 0 {
			return true, "Created Scheduled Task " + pyrepr.Repr(taskName), nil
		}
		lastCode, lastErr = code, firstNonEmpty(errText, out)
	}
	if deleteDetail != "" {
		lastErr = fmt.Sprintf("%s (delete detail: %s)", pystr.Strip(lastErr), deleteDetail)
	}
	return false, fmt.Sprintf("schtasks /Create failed (code %d): %s", lastCode, pystr.Strip(lastErr)), nil
}

// Whether a path exists, every stat failure counted as absent
func winExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Writes the Startup folder fallback launcher, dropping a legacy cmd entry, and returns its path
func installWindowsStartupEntry(ctx context.Context, scriptPath string) (string, error) {
	entry, err := winStartupEntryPath(ctx)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(entry), 0o777); err != nil {
		return "", err
	}
	text, err := buildStartupLauncher(scriptPath)
	if err != nil {
		return "", err
	}
	staging, err := windowsStartupStagingPath(ctx)
	if err != nil {
		return "", err
	}
	if err := windowsAtomicWrite(entry, text, staging, true); err != nil {
		return "", err
	}
	legacy, err := winLegacyStartupEntryPath(ctx)
	if err != nil {
		return "", err
	}
	if winExists(legacy) {
		_ = winRemove(legacy)
	}
	return entry, nil
}

// Startup entry and legacy cmd paths in that order
func windowsStartupEntries(ctx context.Context) ([]string, error) {
	entry, err := winStartupEntryPath(ctx)
	if err != nil {
		return nil, err
	}
	legacy, err := winLegacyStartupEntryPath(ctx)
	if err != nil {
		return nil, err
	}
	return []string{entry, legacy}, nil
}

// Unlinks both Startup folder entries, reporting removals and the ones a lock or denial kept
func removeWindowsStartupEntries(ctx context.Context) ([]string, []string, error) {
	paths, err := windowsStartupEntries(ctx)
	if err != nil {
		return nil, nil, err
	}
	var done, warnings []string
	for _, p := range paths {
		err := winRemove(p)
		switch {
		case err == nil:
			done = append(done, "Removed redundant Windows login item: "+p)
		case errors.Is(err, fs.ErrNotExist):
		default:
			warnings = append(warnings, "Could not remove redundant Windows login item: "+p+" (locked or access denied; it still fires at logon)")
		}
	}
	return done, warnings, nil
}

// Startup entries that fire the gateway a second time at logon beside a task or the vbs fallback
func RedundantAutostartEntries(ctx context.Context) ([]string, error) {
	paths, err := windowsStartupEntries(ctx)
	if err != nil {
		return nil, err
	}
	var entries []string
	for _, p := range paths {
		if winExists(p) {
			entries = append(entries, p)
		}
	}
	registered, err := winIsTaskRegistered(ctx)
	if err != nil {
		return nil, err
	}
	if registered || len(entries) == 0 {
		return entries, nil
	}
	return entries[1:], nil
}

// Converges logon persistence to one mechanism, returning done and warning messages
func ReconcileAutostartLaunchers(ctx context.Context) ([]string, []string, error) {
	registered, err := winIsTaskRegistered(ctx)
	if err != nil {
		return nil, nil, err
	}
	if registered {
		return removeWindowsStartupEntries(ctx)
	}
	legacy, err := winLegacyStartupEntryPath(ctx)
	if err != nil {
		return nil, nil, err
	}
	if winExists(legacy) {
		script, err := winWriteTaskScript(ctx)
		if err != nil {
			return nil, nil, err
		}
		entry, err := installWindowsStartupEntry(ctx, script)
		if err != nil {
			return nil, nil, err
		}
		if winExists(legacy) {
			return nil, []string{"Could not remove legacy Windows login item: " + legacy + " (locked or access denied; it still fires at logon beside " + entry + ")"}, nil
		}
		return []string{"Migrated legacy Windows login item to: " + entry}, nil, nil
	}
	entry, err := winStartupEntryPath(ctx)
	if err != nil {
		return nil, nil, err
	}
	if winExists(entry) {
		script, err := winTaskScriptPath(ctx)
		if err != nil {
			return nil, nil, err
		}
		if _, err := installWindowsStartupEntry(ctx, script); err != nil {
			return nil, nil, err
		}
	}
	return nil, nil, nil
}

// Argv, working directory and environment overlay of the gateway subprocess without a cmd layer
func windowsGatewayArgv(ctx context.Context, homeOverride string) ([]string, string, map[string]string, error) {
	if err := assertWindows(); err != nil {
		return nil, "", nil, err
	}
	pythonPath, workingDir, h, arg, err := windowsLauncherSettings(ctx, homeOverride)
	if err != nil {
		return nil, "", nil, err
	}
	exe, venv, extra := resolveDetachedPython(pythonPath)
	overlay := map[string]string{"ICEBATH_HOME": h}
	for _, kv := range windowsGatewayEnv {
		overlay[kv[0]] = kv[1]
	}
	overlay["VIRTUAL_ENV"] = preserveHomePath(ctx, venv)
	entries := []string{preserveHomePath(ctx, serviceDefProjectRoot())}
	for _, e := range extra {
		entries = append(entries, preserveHomePath(ctx, e))
	}
	prependPythonpath(overlay, entries)
	return windowsGatewayRunArgv(exe, arg), workingDir, overlay, nil
}

// Hidden console respawn argv, cwd and overlay keeping arguments after the interpreter, unchanged off Windows
func windowlessGatewayRestartSpec(ctx context.Context, runArgv []string, homeArg string) ([]string, string, map[string]string) {
	if len(runArgv) == 0 || serviceGOOS != "windows" {
		return runArgv, "", map[string]string{}
	}
	exe, venv, extra := resolveDetachedPython(runArgv[0])
	h := homeArg
	if h == "" {
		h = winHome(ctx)
	}
	resolved, err := pypath.Resolve(h)
	if err != nil {
		resolved = ""
	}
	overlay := map[string]string{"PYTHONIOENCODING": "utf-8", "ICEBATH_GATEWAY_DETACHED": "1", "VIRTUAL_ENV": venv}
	if resolved != "" {
		overlay["ICEBATH_HOME"] = resolved
	}
	prependPythonpath(overlay, append([]string{serviceDefProjectRoot()}, extra...))
	return append([]string{exe}, runArgv[1:]...), stableGatewayWorkingDir(ctx, serviceDefProjectRoot()), overlay
}

// Launches the gateway fully detached with a hidden console, retrying without job breakaway when refused
func spawnWindowsDetached(ctx context.Context, homeOverride string) (int, error) {
	if err := assertWindows(); err != nil {
		return 0, err
	}
	argv, workingDir, overlay, err := winGatewayArgv(ctx, homeOverride)
	if err != nil {
		return 0, err
	}
	target := homeOverride
	if target == "" {
		target = winHome(ctx)
	}
	env, err := tools.ServedProfileChildEnv(ctx, nil, nil, target, true)
	if err != nil {
		return 0, err
	}
	for k, v := range overlay {
		env[k] = v
	}
	logDir := filepath.Join(winHome(ctx), "logs")
	if err := os.MkdirAll(logDir, 0o777); err != nil {
		return 0, err
	}
	strayLog := filepath.Join(logDir, "gateway-stdio.log")
	popen := func(breakaway string, allow bool) (int, error) {
		logFile, err := os.OpenFile(strayLog, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
		if err != nil {
			return 0, err
		}
		defer logFile.Close()
		environ := make([]string, 0, len(env)+1)
		for k, v := range env {
			if k != windowsGatewayBreakawayEnv {
				environ = append(environ, k+"="+v)
			}
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir, cmd.Env = workingDir, append(environ, windowsGatewayBreakawayEnv+"="+breakaway)
		cmd.Stdout, cmd.Stderr = logFile, logFile
		cmd.SysProcAttr = home.DetachAttr(allow)
		if err := winPopen(cmd); err != nil {
			return 0, err
		}
		return cmd.Process.Pid, nil
	}
	pid, err := popen("1", true)
	if err == nil {
		windowsBreakawayFellBack.Store(false)
		return pid, nil
	}
	code := "None"
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		code = strconv.Itoa(int(errno))
	}
	slog.Warn(fmt.Sprintf("Gateway breakaway spawn failed (error=%s); retrying without CREATE_BREAKAWAY_FROM_JOB", code))
	pid, err = popen("0", false)
	if err != nil {
		return 0, err
	}
	windowsBreakawayFellBack.Store(true)
	return pid, nil
}

// Whether a human can answer a prompt, isatty confirmed by the console mode where that fact exists
func stdinIsInteractive(isatty bool, consoleModeOK *bool) bool {
	return isatty && (consoleModeOK == nil || *consoleModeOK)
}

// Whether no human can answer a prompt, as dashboard spawned actions declare
func setupIsNoninteractive() bool {
	switch strings.ToLower(pystr.Strip(os.Getenv("ICEBATH_NONINTERACTIVE"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Asks the wired setup prompt, panicking like any unassigned wiring hook
func setupPromptYesNo(ctx context.Context, question string, def bool) bool {
	if SetupPromptYesNo == nil {
		panic("gateway.SetupPromptYesNo is not wired")
	}
	return SetupPromptYesNo(ctx, question, def)
}

// Install choice from a yes or no environment value, nil when unset or unrecognized
func installChoiceFromEnv(name string) *bool {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return nil
	}
	switch strings.ToLower(pystr.Strip(raw)) {
	case "1", "true", "yes", "y", "on":
		return new(true)
	case "0", "false", "no", "n", "off":
		return new(false)
	}
	return nil
}

// Start now and start on login answers, from arguments, the environment, then the prompt
func promptWindowsInstallChoices(ctx context.Context, startNow, startOnLogin *bool) (bool, bool) {
	if startNow == nil {
		startNow = installChoiceFromEnv(windowsInstallStartNowEnv)
	}
	if startOnLogin == nil {
		startOnLogin = installChoiceFromEnv(windowsInstallStartOnLoginEnv)
	}
	if startNow != nil && startOnLogin != nil {
		return *startNow, *startOnLogin
	}
	if startNow == nil {
		startNow = new(setupPromptYesNo(ctx, "Start the gateway now after install?", true))
	}
	if startOnLogin == nil {
		startOnLogin = new(setupPromptYesNo(ctx, "Start the gateway automatically on Windows login with a Scheduled Task?", true))
	}
	return *startNow, *startOnLogin
}

// Comma separated pid list
func joinPids(pids []int) string {
	parts := make([]string, len(pids))
	for i, p := range pids {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ", ")
}

// Prints the already running notice with its pids
func reportWindowsAlreadyRunning(pids []int) {
	winPrintln("✓ Gateway already running (PID: " + joinPids(pids) + ")")
}

// Spawns the gateway unless one already runs for this profile, scanning when no pids are given
func startOrReportRunning(ctx context.Context, pids []int, scanned bool) error {
	if !scanned {
		var err error
		if pids, err = winGatewayPids(ctx); err != nil {
			return err
		}
	}
	if len(pids) > 0 {
		reportWindowsAlreadyRunning(pids)
		return nil
	}
	if _, err := winSpawnDetached(ctx, ""); err != nil {
		return err
	}
	return winReportGatewayStart(ctx, "direct spawn")
}

// Installs the Startup folder fallback unless a task survives, then starts once when asked
func installWindowsStartupFallback(ctx context.Context, scriptPath string, startNow bool, detail string) error {
	lines := pystr.Splitlines(detail, false)
	if len(lines) == 0 {
		return errors.New("list index out of range")
	}
	winPrintln("↻ Scheduled Task install blocked (" + lines[0] + ") — using Startup folder fallback")
	registered, err := winIsTaskRegistered(ctx)
	if err != nil {
		return err
	}
	if registered {
		winPrintln("⚠ Scheduled Task is still registered — skipped the Startup fallback to avoid a duplicate autostart.")
		winPrintln("  If that task is disabled or broken, run 'icebath gateway uninstall', then install again.")
	} else {
		entry, err := installWindowsStartupEntry(ctx, scriptPath)
		if err != nil {
			return err
		}
		winPrintln("✓ Installed Windows login item: " + entry)
	}
	winPrintln("  Task script: " + scriptPath)
	pids, err := winGatewayPids(ctx)
	if err != nil {
		return err
	}
	if len(pids) > 0 || startNow {
		if err := startOrReportRunning(ctx, pids, true); err != nil {
			return err
		}
	} else {
		arg, err := profileArg(ctx, "", "")
		if err != nil {
			return err
		}
		startCmd := "icebath gateway start"
		if arg != "" {
			startCmd = "icebath " + arg + " gateway start"
		}
		winPrintln("ℹ Startup fallback installed; gateway not started now.")
		winPrintln("  Start manually with: " + startCmd)
	}
	winPrintNextSteps(ctx)
	return nil
}

// Offers the UAC prompt for a task install, true when an elevated child took over
func offerElevatedInstall(ctx context.Context, headline string, force, startNow, startOnLogin bool) (bool, error) {
	winPrintln(headline)
	winPrintln("  UAC is Windows' admin approval prompt; it is needed to create/update the Scheduled Task.")
	if !setupPromptYesNo(ctx, "  Open the UAC prompt now?", false) {
		winPrintln("  Skipped elevation. Falling back to Startup folder.")
		return false, nil
	}
	launched, err := winLaunchElevatedInstall(ctx, force, &startNow, &startOnLogin)
	if err != nil {
		return false, err
	}
	if launched {
		winPrintln("✓ Launched elevated Icebath gateway install prompt.")
		if startNow {
			winPrintln("  Approve the Windows UAC prompt; the elevated install will start the gateway afterwards.")
		} else {
			winPrintln("  Approve the Windows UAC prompt, then run: icebath gateway status")
		}
		return true, nil
	}
	winPrintln("⚠ Falling back to Startup folder because elevation was unavailable or cancelled.")
	return false, nil
}

// Installs the gateway as a Windows Scheduled Task with the Startup folder fallback, always reconciling
func WindowsInstall(ctx context.Context, o WindowsInstallOptions) error {
	if err := assertWindows(); err != nil {
		return err
	}
	startNow, startOnLogin := winPromptInstallChoices(ctx, o.StartNow, o.StartOnLogin)
	if !startOnLogin {
		winPrintln("ℹ Skipped Windows login auto-start install.")
		if startNow {
			return startOrReportRunning(ctx, nil, false)
		}
		winPrintln("ℹ Gateway not started and no auto-start service installed.")
		winPrintln("  Run in the foreground later with: icebath gateway run")
		return nil
	}
	taskName, err := winTaskName(ctx)
	if err != nil {
		return err
	}
	scriptPath, err := winWriteTaskScript(ctx)
	if err != nil {
		return err
	}
	staging, err := windowsStartupStagingPath(ctx)
	if err != nil {
		return err
	}
	_ = os.Remove(staging)
	if o.Force {
		if err := winRemoveLegacyLaunchers(ctx); err != nil {
			return err
		}
	}
	admin, err := winIsRunningAsAdmin()
	if err != nil {
		return err
	}
	if !admin && !o.ElevatedHandoff {
		handed, err := offerElevatedInstall(ctx, "↻ Scheduled Task install may need administrator approval on this Windows account.", o.Force, startNow, startOnLogin)
		if err != nil || handed {
			return err
		}
		return installWindowsStartupFallback(ctx, scriptPath, startNow, "administrator approval was not used")
	}
	ok, detail, err := winInstallScheduledTask(ctx, taskName, scriptPath)
	if err != nil {
		return err
	}
	if ok {
		winPrintln("✓ " + detail)
		winPrintln("  Task script: " + scriptPath)
		winPrintln("ℹ Gateway auto-start installed for Windows login.")
		done, warnings, err := removeWindowsStartupEntries(ctx)
		if err != nil {
			return err
		}
		for _, m := range done {
			winPrintln("✓ " + m)
		}
		for _, m := range warnings {
			winPrintln("⚠ " + m)
		}
		if startNow {
			if err := startOrReportRunning(ctx, nil, false); err != nil {
				return err
			}
		} else {
			winPrintln("ℹ Gateway not started now.")
			winPrintln("  Start manually with: icebath gateway start")
		}
		winPrintNextSteps(ctx)
		return nil
	}
	if windowsIsAccessDenied(detail) {
		admin, err := winIsRunningAsAdmin()
		if err != nil {
			return err
		}
		if !admin {
			lines := pystr.Splitlines(detail, false)
			if len(lines) == 0 {
				return errors.New("list index out of range")
			}
			handed, err := offerElevatedInstall(ctx, "↻ Scheduled Task install needs administrator approval ("+lines[0]+")", o.Force, startNow, startOnLogin)
			if err != nil || handed {
				return err
			}
		}
	}
	if windowsShouldFallBack(1, detail) {
		return installWindowsStartupFallback(ctx, scriptPath, startNow, detail)
	}
	return errors.New("Windows gateway install failed: " + detail)
}

// Live gateway pids of the active profile for the readiness poll
func liveWindowsGatewayPids(ctx context.Context) ([]int, error) {
	return FindGatewayPIDsFor(ctx, nil, false)
}

// Rechecks a fresh gateway for the confirmation window, empty when it vanished meanwhile
func confirmWindowsGatewayStable(ctx context.Context, initial []int, confirmS, intervalS float64) ([]int, error) {
	if confirmS <= 0 {
		return initial, nil
	}
	pids := initial
	deadline := pythread.Monotonic() + confirmS
	for pythread.Monotonic() < deadline {
		winSleep(time.Duration(intervalS * float64(time.Second)))
		var err error
		if pids, err = liveWindowsGatewayPids(ctx); err != nil {
			return nil, err
		}
		if len(pids) == 0 {
			return nil, nil
		}
	}
	return pids, nil
}

// Polls for a gateway that stays visible through the confirmation window, empty at the timeout
func waitForWindowsGatewayReady(ctx context.Context, timeoutS float64) ([]int, error) {
	deadline := pythread.Monotonic() + timeoutS
	for pythread.Monotonic() < deadline {
		pids, err := liveWindowsGatewayPids(ctx)
		if err != nil {
			return nil, err
		}
		if len(pids) > 0 {
			confirmed, err := confirmWindowsGatewayStable(ctx, pids, windowsReadyConfirmS, windowsReadyIntervalS)
			if err != nil {
				return nil, err
			}
			if len(confirmed) > 0 {
				return confirmed, nil
			}
			continue
		}
		winSleep(time.Duration(windowsReadyIntervalS * float64(time.Second)))
	}
	return nil, nil
}

// Start attestation marker under the home state directory
func startAttestationPath(ctx context.Context) string {
	return filepath.Join(winHome(ctx), "state", "gateway.start-attestation.json")
}

// Persists the pids a start vouched for with their create times, never failing the caller
func writeStartAttestation(ctx context.Context, pids []int, via string) {
	path := startAttestationPath(ctx)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		slog.Debug("Failed to write gateway start attestation", "error", err)
		return
	}
	pidList := make([]any, len(pids))
	createTimes := pyjson.NewObject()
	for i, p := range pids {
		pidList[i] = int64(p)
		if ct := tools.ProcessCreateTime(p); ct != nil {
			createTimes.Set(strconv.Itoa(p), *ct)
		}
	}
	payload := pyjson.NewObject()
	payload.Set("pids", pidList)
	payload.Set("via", via)
	payload.Set("ts", pydatetime.ISOFormatUTC(time.Now()))
	payload.Set("generation", strings.ReplaceAll(uuid.NewString(), "-", ""))
	payload.Set("create_times", createTimes)
	text, err := pyjson.Dumps(payload)
	if err == nil {
		tmp := winWithSuffix(path, ".json.tmp")
		if err = os.WriteFile(tmp, []byte(text), 0o666); err == nil {
			err = os.Rename(tmp, path)
		}
	}
	if err != nil {
		slog.Debug("Failed to write gateway start attestation", "error", err)
	}
}

// Removes the start attestation marker, ignoring every failure
func clearStartAttestation(ctx context.Context) { _ = os.Remove(startAttestationPath(ctx)) }

// JSON value of a utf-8-sig file, false when absent, unreadable or invalid
func readJSONSig(path string) (any, bool) {
	raw, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(raw) {
		return nil, false
	}
	data, err := pyjson.Loads(strings.TrimPrefix(string(raw), "\ufeff"))
	return data, err == nil
}

// Parsed start attestation of any JSON type, nil when absent or unreadable
func readStartAttestation(ctx context.Context) any {
	data, ok := readJSONSig(startAttestationPath(ctx))
	if !ok {
		return nil
	}
	return data
}

// Positive integer pids of an attestation, empty when any entry is malformed
func attestedPidsFrom(data any) []*big.Int {
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return nil
	}
	raw, _ := obj.Get("pids")
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	pids := make([]*big.Int, 0, len(list))
	for _, p := range list {
		n, isInt := pyrepr.Int(p)
		if !pyIsInt(p) || !isInt || n.Sign() <= 0 {
			return nil
		}
		pids = append(pids, n)
	}
	return pids
}

// Create time an attestation bound pid to, nil for older markers
func attestedCreateTime(data any, pid *big.Int) *float64 {
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return nil
	}
	raw, _ := obj.Get("create_times")
	times, ok := raw.(*pyjson.Object)
	if !ok {
		return nil
	}
	value, _ := times.Get(pid.String())
	if f, ok := value.(float64); ok {
		return &f
	}
	if n, ok := pyrepr.Int(value); ok && pyIsInt(value) {
		f, _ := new(big.Float).SetInt(n).Float64()
		return &f
	}
	return nil
}

// Whether a decoded JSON value equals pid under Python numeric equality
func pyEqualsInt(v any, pid *big.Int) bool {
	r, ok := pyNumber(v)
	return ok && r != nil && r.Cmp(new(big.Rat).SetInt(pid)) == 0
}

// Whether the lifecycle ledger shows a clean exit for pid, or proves the bound incarnation is not the one recorded
func attestedPidExitedCleanly(ctx context.Context, pid *big.Int, createTime *float64) bool {
	raw, err := os.ReadFile(GetLifecycleSentinelPath(ctx, winHome(ctx)))
	if err != nil {
		return false
	}
	var data any
	parsed := utf8.Valid(raw)
	if parsed {
		data, err = pyjson.Loads(strings.TrimPrefix(string(raw), "\ufeff"))
		parsed = err == nil
	}
	obj, isObj := data.(*pyjson.Object)
	if !parsed || !isObj {
		return createTime != nil
	}
	recorded, _ := obj.Get("pid")
	if createTime != nil {
		if !pyEqualsInt(recorded, pid) {
			return true
		}
		birth, _ := obj.Get("create_time")
		if f, ok := pyFloatOf(birth); ok && max(f-*createTime, *createTime-f) > attestationCreateTimeTolerS {
			return true
		}
	}
	phase, _ := obj.Get("phase")
	return phase == "exited" && pyEqualsInt(recorded, pid)
}

// Float of a JSON int or float, false for anything else including bools
func pyFloatOf(v any) (float64, bool) {
	if f, ok := v.(float64); ok {
		return f, true
	}
	if n, ok := pyrepr.Int(v); ok && pyIsInt(v) {
		f, _ := new(big.Float).SetInt(n).Float64()
		return f, true
	}
	return 0, false
}

// Whether attested pids died, no gateway running now and no clean exit recorded for any of them
func attestedDead(ctx context.Context, attested []*big.Int, currentPids []int, data any) bool {
	if len(currentPids) > 0 {
		return false
	}
	for _, pid := range attested {
		if attestedPidExitedCleanly(ctx, pid, attestedCreateTime(data, pid)) {
			return false
		}
	}
	return true
}

// Warning for a gateway that died after a start reported success, consuming the marker once
func CheckStartAttestation(ctx context.Context, currentPids []int, scanned bool) (string, error) {
	data := readStartAttestation(ctx)
	if data == nil {
		return "", nil
	}
	attested := attestedPidsFrom(data)
	if len(attested) == 0 {
		clearStartAttestation(ctx)
		return "", nil
	}
	if !scanned {
		pids, err := winGatewayPids(ctx)
		if err != nil {
			return "", nil
		}
		currentPids = pids
	}
	clearStartAttestation(ctx)
	if !attestedDead(ctx, attested, currentPids, data) {
		return "", nil
	}
	return formatAttestationWarning(ctx, attested, data.(*pyjson.Object)), nil
}

// Truthy value of key rendered with str, else the fallback
func pyOrStr(obj *pyjson.Object, key, fallback string) string {
	v, _ := obj.Get(key)
	if !pyTruthy(v) {
		return fallback
	}
	return pyrepr.Str(v)
}

// Multi line warning naming the attested pids and the Job Object cause with the task recovery hint
func formatAttestationWarning(ctx context.Context, attested []*big.Int, data *pyjson.Object) string {
	parts := make([]string, len(attested))
	for i, p := range attested {
		parts[i] = p.String()
	}
	lines := []string{
		"⚠ The previous gateway start (" + pyOrStr(data, "via", "direct spawn") + ", " + pyOrStr(data, "ts", "unknown time") + ") reported success, but the " +
			"process (PID " + strings.Join(parts, ", ") + ") died without a clean " + "shutdown record.",
		"  This usually means the shell that ran `icebath gateway start` was inside " + "a Windows Job Object that killed the gateway on exit (#91675).",
	}
	if hint := taskRunHint(ctx, "  Recovery: schtasks /Run /TN {}   (Task Scheduler starts the gateway outside any Job Object)"); hint != "" {
		lines = append(lines, hint)
	}
	return strings.Join(lines, "\n")
}

// Format with the task name filled in when a Scheduled Task is registered, empty otherwise
func taskRunHint(ctx context.Context, format string) string {
	registered, err := winIsTaskRegistered(ctx)
	if err != nil || !registered {
		return ""
	}
	name, err := winTaskName(ctx)
	if err != nil {
		return ""
	}
	return strings.Replace(format, "{}", name, 1)
}

// Prints the task run hint when there is one
func printTaskRunHint(ctx context.Context, format string) {
	if hint := taskRunHint(ctx, format); hint != "" {
		winPrintln(hint)
	}
}

// Prints the pending stale attestation warning, never failing
func printStartAttestationWarning(ctx context.Context) {
	warning, err := CheckStartAttestation(ctx, nil, false)
	if err == nil && warning != "" {
		winPrintln(warning)
	}
}

// Reports a verified start with an attestation, or the failure with log paths and the recovery hint
func reportWindowsGatewayStart(ctx context.Context, via string) error {
	pids, err := winWaitForGatewayReady(ctx, 6.0)
	if err != nil {
		return err
	}
	if len(pids) > 0 {
		winPrintln("✓ Gateway started via " + via + " (PID: " + joinPids(pids) + ")")
		if windowsBreakawayFellBack.Load() {
			winPrintln("⚠ The gateway could not break away from this shell's Job Object; it may be killed when this shell exits.")
			printTaskRunHint(ctx, "  If it dies, start it with: schtasks /Run /TN {}")
		}
		writeStartAttestation(ctx, pids, via)
		return nil
	}
	h := winHome(ctx)
	winPrintln("✗ Gateway start via " + via + " FAILED — no stable gateway process detected within the verification window.")
	winPrintln("  (The process may have been created and then killed — e.g. by a parent Job Object, #91675.)")
	winPrintln("  Check the log for startup errors:\n    type " + h + `\logs\gateway.log` + "\n    type " + h + `\logs\gateway-stdio.log`)
	printTaskRunHint(ctx, "  Recovery: schtasks /Run /TN {}   (starts the gateway outside any Job Object)")
	return nil
}

// Prints the status and log viewing next steps
func printWindowsNextSteps(ctx context.Context) {
	winPrintln("\nNext steps:\n  icebath gateway status                      # Check status")
	winPrintln("  type " + winHome(ctx) + `\logs\gateway.log` + "       # View logs")
}

// Whether schtasks answers the task query with exit code zero
func windowsIsTaskRegistered(ctx context.Context) (bool, error) {
	name, err := winTaskName(ctx)
	if err != nil {
		return false, err
	}
	code, _, _, err := winExecSchtasks([]string{"/Query", "/TN", name})
	return err == nil && code == 0, err
}

// Whether a Startup folder entry, current or legacy, exists
func windowsStartupEntryInstalled(ctx context.Context) (bool, error) {
	paths, err := windowsStartupEntries(ctx)
	if err != nil {
		return false, err
	}
	return winExists(paths[0]) || winExists(paths[1]), nil
}

// Whether the Scheduled Task or the Startup fallback is present
func WindowsIsInstalled(ctx context.Context) (bool, error) {
	registered, err := winIsTaskRegistered(ctx)
	if err != nil || registered {
		return registered, err
	}
	return windowsStartupEntryInstalled(ctx)
}

// Registered task XML, false when schtasks cannot export it
func queryScheduledTaskXML(taskName string) (string, bool, error) {
	code, out, errText, err := winExecSchtasks([]string{"/Query", "/TN", taskName, "/XML"})
	if err != nil {
		return "", false, err
	}
	if code != 0 || pystr.Strip(out) == "" {
		slog.Debug(fmt.Sprintf("Could not query Scheduled Task XML for %s: %s", pyrepr.Repr(taskName), pystr.Strip(firstNonEmpty(errText, out))))
		return "", false, nil
	}
	return out, true, nil
}

// Drift fragments between a registered task export and the template, empty when aligned or unparsable
func CompareScheduledTaskDrift(registeredXML, templateXML string) []string {
	live, want := tools.TaskXMLLeafValues(registeredXML), tools.TaskXMLLeafValues(templateXML)
	if live == nil || want == nil {
		return nil
	}
	var missing, differs []string
	for _, leaf := range windowsTaskDriftLeaves {
		w, inWant := want[leaf[0]]
		l, inLive := live[leaf[0]]
		switch {
		case inWant && !inLive:
			missing = append(missing, leaf[1])
		case inWant && l != w:
			differs = append(differs, leaf[1])
		}
	}
	var drift []string
	if len(missing) > 0 {
		drift = append(drift, "missing: "+strings.Join(missing, ", "))
	}
	for _, label := range differs {
		drift = append(drift, label+" differs")
	}
	if live["Task@version"] != want["Task@version"] {
		drift = append(drift, "version "+live["Task@version"]+" vs "+want["Task@version"])
	}
	return drift
}

// Drift between the registered task and the current template, empty when aligned or unqueryable
func scheduledTaskDrift(ctx context.Context, taskName string) ([]string, error) {
	registered, ok, err := queryScheduledTaskXML(taskName)
	if err != nil || !ok {
		return nil, err
	}
	script, err := winTaskScriptPath(ctx)
	if err != nil {
		return nil, err
	}
	return CompareScheduledTaskDrift(registered, buildScheduledTaskXML(taskName, winWithSuffix(script, ".vbs"), winResolveTaskUser())), nil
}

// Warns when the registered task predates the current template with the repair command
func printScheduledTaskDrift(ctx context.Context, taskName string) error {
	drift, err := scheduledTaskDrift(ctx, taskName)
	if err != nil || len(drift) == 0 {
		return err
	}
	winPrintln("⚠ Scheduled Task registration predates the current template (" + strings.Join(drift, "; ") + ")")
	winPrintln("  Repair: icebath gateway start  (or: icebath gateway install)")
	return nil
}

// Re-registers a drifted task from the current template, true only when schtasks accepted it
func ReconcileScheduledTask(ctx context.Context, taskName string) (bool, error) {
	drift, err := scheduledTaskDrift(ctx, taskName)
	if err != nil || len(drift) == 0 {
		return false, err
	}
	winPrintln("↻ Repairing outdated Scheduled Task registration (" + strings.Join(drift, "; ") + ")")
	script, err := winWriteTaskScript(ctx)
	if err != nil {
		return false, err
	}
	ok, detail, err := winInstallScheduledTask(ctx, taskName, script)
	if err != nil {
		return false, err
	}
	mark := "⚠"
	if ok {
		mark = "✓"
	}
	winPrintln(mark + " " + detail)
	if !ok {
		winPrintln("  Repair manually: icebath gateway install")
	}
	return ok, nil
}

// Starts the gateway through the detached launch, offering login persistence when none is installed
func WindowsStart(ctx context.Context) error {
	if err := assertWindows(); err != nil {
		return err
	}
	winPrintStartAttestationWarning(ctx)
	pids, err := winGatewayPids(ctx)
	if err != nil {
		return err
	}
	if len(pids) > 0 {
		reportWindowsAlreadyRunning(pids)
		return nil
	}
	registered, err := winIsTaskRegistered(ctx)
	if err != nil {
		return err
	}
	startupInstalled := false
	if !registered {
		if startupInstalled, err = winIsStartupEntryInstalled(ctx); err != nil {
			return err
		}
	}
	if !registered && !startupInstalled {
		startOnLogin := installChoiceFromEnv(windowsInstallStartOnLoginEnv)
		if startOnLogin == nil {
			winPrintln("✗ Gateway service is not installed")
			if setupIsNoninteractive() || !winStdoutIsatty() || !stdinIsInteractive(winStdinIsatty(), winStdinConsoleModeOK()) {
				startOnLogin = new(false)
			} else {
				startOnLogin = new(setupPromptYesNo(ctx, "  Install it now so the gateway starts on login?", true))
			}
		}
		if *startOnLogin {
			return winInstall(ctx, WindowsInstallOptions{StartNow: new(true), StartOnLogin: new(true)})
		}
		winPrintln("ℹ Login auto-start not installed; add it later with: icebath gateway install")
	} else {
		if registered, err = winIsTaskRegistered(ctx); err != nil {
			return err
		}
		if registered {
			name, err := winTaskName(ctx)
			if err != nil {
				return err
			}
			if _, err := ReconcileScheduledTask(ctx, name); err != nil {
				return err
			}
		}
	}
	if _, err := winSpawnDetached(ctx, ""); err != nil {
		return err
	}
	return winReportGatewayStart(ctx, "direct spawn")
}

// Writes the planned stop marker and waits for pid to exit, the only drain request Windows has
func drainWindowsGatewayPid(ctx context.Context, pid int, drainTimeout float64) bool {
	if pid <= 0 {
		return false
	}
	WritePlannedStopMarker(ctx, pid)
	return pydeadline.PollUntil(func() (bool, bool) {
		gone := !statusPidExists(pid)
		return gone, gone
	}, time.Duration(max(drainTimeout, 1.0)*float64(time.Second)), 500*time.Millisecond)
}

// Restart drain timeout clamped to between one and thirty seconds so stop never wedges
func windowsStopDrainTimeout(ctx context.Context) float64 {
	configured, err := getRestartDrainTimeout(ctx)
	if err != nil || configured == 0 {
		configured = 30.0
	}
	return max(1.0, min(configured, 30.0))
}

// Pid and the start time observed when it was identified as a gateway
type gatewayPidIdentity struct {
	pid   int
	start any
}

// Start time fingerprints captured before any drain so a later kill detects a recycled pid
func gatewayPidIdentities(pids []int) []gatewayPidIdentity {
	out := make([]gatewayPidIdentity, len(pids))
	for i, pid := range pids {
		out[i] = gatewayPidIdentity{pid, GetProcessStartTime(pid)}
	}
	return out
}

// Force kills the fingerprinted gateway pids, refusing any whose live process no longer matches
func forceTerminateKnownGatewayPids(identities []gatewayPidIdentity) int {
	own, killed := os.Getpid(), 0
	for _, id := range identities {
		if id.pid <= 0 || id.pid == own || !statusPidExists(id.pid) {
			continue
		}
		err := TerminatePid(id.pid, true, id.start)
		switch {
		case err == nil:
			killed++
		case errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH):
		case errors.Is(err, fs.ErrPermission):
			winPrintln(fmt.Sprintf("⚠ Permission denied to kill PID %d", id.pid))
		default:
			winPrintln(fmt.Sprintf("Failed to kill PID %d: %s", id.pid, strings.TrimPrefix(err.Error(), ErrRefusedKill.Error()+": ")))
		}
	}
	return killed
}

// Gateway pids of the active profile with the primary first, a failed scan leaving only the primary
func collectGatewayStopPids(ctx context.Context, primary int) []int {
	var pids []int
	if primary > 0 {
		pids = append(pids, primary)
	}
	scanned, err := winGatewayPids(ctx)
	if err != nil {
		return pids
	}
	for _, pid := range scanned {
		if pid > 0 && !slices.Contains(pids, pid) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// Stops the gateway with the planned stop marker, then schtasks End, then a bounded kill of known pids
func WindowsStop(ctx context.Context) error {
	if err := assertWindows(); err != nil {
		return err
	}
	clearStartAttestation(ctx)
	pid, running, err := winRunningPid("", true)
	if err != nil {
		return err
	}
	primary := 0
	if running {
		primary = pid
	}
	identities := gatewayPidIdentities(collectGatewayStopPids(ctx, primary))
	drained := running && drainWindowsGatewayPid(ctx, pid, windowsStopDrainTimeout(ctx))
	stoppedAny := drained
	registered, err := winIsTaskRegistered(ctx)
	if err != nil {
		return err
	}
	if registered {
		name, err := winTaskName(ctx)
		if err != nil {
			return err
		}
		code, _, errText, err := winExecSchtasks([]string{"/End", "/TN", name})
		if err != nil {
			return err
		}
		if code == 0 {
			stoppedAny = true
		} else if !strings.Contains(strings.ToLower(errText), "not running") {
			winPrintln(fmt.Sprintf("⚠ schtasks /End returned code %d: %s", code, pystr.Strip(errText)))
		}
	}
	known := map[int]bool{}
	for _, id := range identities {
		known[id.pid] = true
	}
	var late []int
	for _, p := range collectGatewayStopPids(ctx, 0) {
		if !known[p] {
			late = append(late, p)
		}
	}
	identities = append(identities, gatewayPidIdentities(late)...)
	if killed := forceTerminateKnownGatewayPids(identities); killed > 0 {
		stoppedAny = true
		winPrintln(fmt.Sprintf("✓ Killed %d gateway process(es)", killed))
	}
	switch {
	case stoppedAny && drained:
		winPrintln("✓ Gateway stopped (drained cleanly)")
	case stoppedAny:
		winPrintln("✓ Gateway stopped")
	default:
		winPrintln("✗ No gateway was running")
	}
	return nil
}

// Waits until neither the runtime lock nor the process scan finds a gateway, false at the timeout
func waitForWindowsGatewayAbsent(ctx context.Context, timeoutS float64) (bool, error) {
	const intervalS = 0.5
	var scanErr error
	absent := pydeadline.PollUntil(func() (bool, bool) {
		_, running, err := winRunningPid("", true)
		if err != nil {
			scanErr = err
			return false, true
		}
		if running {
			return false, false
		}
		pids, err := winGatewayPids(ctx)
		if err != nil {
			scanErr = err
			return false, true
		}
		return len(pids) == 0, len(pids) == 0
	}, time.Duration(max(timeoutS, intervalS)*float64(time.Second)), time.Duration(intervalS*float64(time.Second)))
	return absent, scanErr
}

// Stops then starts once the old gateway is gone, failing loudly when either side does not converge
func WindowsRestart(ctx context.Context) error {
	if err := assertWindows(); err != nil {
		return err
	}
	if err := winStop(ctx); err != nil {
		return err
	}
	absent, err := winWaitForGatewayAbsent(ctx, 30.0)
	if err != nil {
		return err
	}
	if !absent {
		winPrintln("⚠ Gateway still present after stop; forcing termination before restart...")
		forceTerminateKnownGatewayPids(gatewayPidIdentities(collectGatewayStopPids(ctx, 0)))
		if absent, err = winWaitForGatewayAbsent(ctx, 10.0); err != nil {
			return err
		}
		if !absent {
			return errors.New("Gateway process still detected after force kill; refusing to start a duplicate. Investigate stray PIDs before retrying.")
		}
	}
	if _, err := winWaitForAPIServerPortFree(ctx, 10.0); err != nil {
		return err
	}
	winMarkPlannedRestart(ctx)
	if err := winStart(ctx); err != nil {
		return err
	}
	pids, err := winWaitForGatewayReady(ctx, 15.0)
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		return errors.New("Gateway restart did not produce a running gateway process. Check logs/gateway.log and run `icebath gateway status`.")
	}
	return nil
}
