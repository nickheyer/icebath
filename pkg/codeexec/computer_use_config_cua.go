package codeexec

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	xterm "github.com/charmbracelet/x/term"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

const cuaLSRegister = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"

// Package seams of the cua setup the tests swap as hermes monkeypatches module attributes
var (
	cuaSetupEnsure = func(ctx context.Context, name string, opts pm.EnsureOptions) error {
		_, err := pm.Ensure(ctx, name, opts)
		return err
	}
	cuaSetupRunText = func(argv []string, timeout float64, env map[string]string, hide bool) (*completedProcess, error) {
		return runQuiet(argv, timeout, runOpts{Env: env, Stdin: os.Stdin, HideWindow: hide})
	}
	cuaSchtasksQuery         = schtasksQueryBytes
	cuaResolvedDriverCmd     = func() (string, error) { return resolveCuaDriverCmd(nil) }
	cuaValidateAppSignature  = validateCuaDriverAppSignature
	cuaSetupWhich            = func(cmd string) (string, bool) { return pyshutil.Which(cmd, nil) }
	cuaSetupStdoutTTY        = func() bool { return xterm.IsTerminal(os.Stdout.Fd()) }
	cuaSetupMonotonic        = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	cuaDriverContractCacheMu sync.Mutex
	cuaDriverContractCache   = map[string]any{}
)

// Prints the warning and its hints then answers false like a failed setup step
func cuaFail(con *cliui.Console, message string, hints ...string) bool {
	con.PrintWarning(message)
	for _, h := range hints {
		con.PrintInfo(h)
	}
	return false
}

// Last three lines of the stderr or else stdout of a run, each indented and cut at 200 characters
func cuaPrintOutputTail(con *cliui.Console, result *completedProcess, printer func(string)) {
	text := result.Stderr
	if text == "" {
		text = result.Stdout
	}
	lines := pystr.Splitlines(pystr.Strip(text), false)
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	if printer == nil {
		printer = con.PrintInfo
	}
	for _, line := range lines {
		r := []rune(line)
		if len(r) > 200 {
			r = r[:200]
		}
		printer("      " + string(r))
	}
}

// Hides Windows children unless their output streams to a real console
func cuaPostSetupNoWindow(streamsToConsole bool) bool {
	flags := cuPlatform == "win32"
	if flags && streamsToConsole && cuaSetupStdoutTTY() {
		return false
	}
	return flags
}

// Driver command named by the override variable, cua-driver otherwise
func cuaDriverCmd() string {
	if v := pystr.Strip(os.Getenv(cuaDriverCmdEnv)); v != "" {
		return v
	}
	return "cua-driver"
}

// First non blank line of a version banner, stripped and bounded to limit characters
func cuaVersionSummary(raw string, limit int) string {
	for _, line := range pystr.Splitlines(raw, false) {
		if s := pystr.Strip(line); s != "" {
			r := []rune(s)
			if len(r) > limit {
				r = r[:limit]
			}
			return string(r)
		}
	}
	return ""
}

// Shallow dict copy of a contract state
func copyPyObject(o *pyjson.Object) *pyjson.Object {
	out := pyjson.NewObject()
	for _, k := range o.Keys {
		v, _ := o.Get(k)
		out.Set(k, v)
	}
	return out
}

// Runtime manifest check cached for thirty seconds by binary path, mtime and size
var cuaDriverContractStatus = func(env *tools.Env, binary string) (*pyjson.Object, error) {
	resolved := binary
	if resolved == "" {
		var err error
		if resolved, err = cuaResolvedDriverCmd(); err != nil {
			return nil, err
		}
	}
	if resolved == "" {
		return cuaDriverRuntimeContractStatus(env, "")
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return cuaDriverRuntimeContractStatus(env, resolved)
	}
	fingerprint := fmt.Sprintf("%s\x00%d\x00%d", resolved, st.ModTime().UnixNano(), st.Size())
	now := cuaSetupMonotonic()
	cuaDriverContractCacheMu.Lock()
	cached, hit := cuaDriverContractCache["state"].(*pyjson.Object)
	checkedAt, _ := cuaDriverContractCache["checked_at"].(float64)
	if hit && cuaDriverContractCache["fingerprint"] == fingerprint && now-checkedAt < 30.0 {
		out := copyPyObject(cached)
		cuaDriverContractCacheMu.Unlock()
		return out, nil
	}
	cuaDriverContractCacheMu.Unlock()
	state, err := cuaDriverRuntimeContractStatus(env, resolved)
	if err != nil {
		return nil, err
	}
	cuaDriverContractCacheMu.Lock()
	cuaDriverContractCache["fingerprint"] = fingerprint
	cuaDriverContractCache["checked_at"] = now
	cuaDriverContractCache["state"] = copyPyObject(state)
	cuaDriverContractCacheMu.Unlock()
	return state, nil
}

// Empties the contract cache before an install re-reads the selected binary
func clearCuaDriverContractCache() {
	cuaDriverContractCacheMu.Lock()
	cuaDriverContractCache = map[string]any{}
	cuaDriverContractCacheMu.Unlock()
}

// Whether the selected driver passes its contract with the host integration its platform needs
var cuaDriverInstallReady = func(env *tools.Env) (bool, error) {
	state, err := cuaDriverContractStatus(env, "")
	if err != nil {
		return false, err
	}
	if v, _ := state.Get("ready"); !pyrepr.Truthy(v) {
		return false, nil
	}
	if cuPlatform == "darwin" {
		b, _ := state.Get("binary")
		return resolveCuaDriverAppPath(pyrepr.Str(b)) != "", nil
	}
	return cuPlatform != "win32" || cuaDriverAutostartRegisteredWindows(env, "") || !cuaAutostartOptIn(env), nil
}

// Reconciles the PM pin or validates the override, then the host setup of an explicit install command
var installCuaDriver = func(ctx context.Context, env *tools.Env, con *cliui.Console, showInstallerProgress bool) (bool, error) {
	override := pystr.Strip(os.Getenv(cuaDriverCmdEnv))
	binary, err := cuaResolvedDriverCmd()
	if err != nil {
		return false, err
	}
	freshInstall := binary == ""
	if override != "" {
		if binary == "" {
			return cuaFail(con, "    ICEBATH_CUA_DRIVER_CMD does not resolve to an executable: "+override,
				"    Fix or unset the override before running computer-use install."), nil
		}
	} else {
		if showInstallerProgress {
			con.PrintInfo("    Preparing the pinned cua-driver with Icebath PM...")
		}
		if err := cuaSetupEnsure(ctx, "cua-driver", pm.EnsureOptions{Explicit: true}); err != nil {
			return cuaFail(con, "    cua-driver preparation failed: "+err.Error()), nil
		}
		if binary, err = cuaResolvedDriverCmd(); err != nil {
			return false, err
		}
	}
	if binary == "" {
		return cuaFail(con, "    PM did not select a usable cua-driver executable."), nil
	}
	clearCuaDriverContractCache()
	contract, err := cuaDriverContractStatus(env, binary)
	if err != nil {
		return false, err
	}
	if v, _ := contract.Get("ready"); !pyrepr.Truthy(v) {
		hint := "    Run: icebath computer-use doctor"
		if override != "" {
			hint = "    Update the binary selected by ICEBATH_CUA_DRIVER_CMD, or unset the override."
		}
		reason := "unknown error"
		if r, _ := contract.Get("reason"); pyrepr.Truthy(r) {
			reason = pyrepr.Str(r)
		}
		return cuaFail(con, "    cua-driver runtime contract is unusable: "+reason+".", hint), nil
	}
	if cuPlatform == "win32" && !repairCuaDriverAutostartWindows(env, con, binary, showInstallerProgress) {
		return cuaFail(con, "    cua-driver is compatible, but Windows autostart setup failed."), nil
	}
	if cuPlatform == "darwin" {
		app := resolveCuaDriverAppPath(binary)
		if app == "" {
			return cuaFail(con, "    macOS computer use requires the signed CuaDriver.app, not a bare binary.",
				"    The PM cua-driver package must include its signed macOS app bundle."), nil
		}
		if err := cuaValidateAppSignature(env, app); err != nil {
			return cuaFail(con, "    cua-driver macOS app registration failed: "+err.Error()), nil
		}
		result, err := cuaSetupRunText([]string{cuaLSRegister, "-f", app}, 15, nil, false)
		if err != nil {
			return cuaFail(con, "    cua-driver macOS app registration failed: "+err.Error()), nil
		}
		if result.Returncode != 0 {
			cuaPrintOutputTail(con, result, nil)
			return cuaFail(con, "    cua-driver macOS app registration failed."), nil
		}
	}
	if showInstallerProgress {
		version := "unknown version"
		if v, _ := contract.Get("version"); pyrepr.Truthy(v) {
			version = pyrepr.Str(v)
		}
		con.PrintSuccess("    cua-driver ready: " + version + ".")
		printCuaPlatformNotes(con, cuPlatform == "win32", cuPlatform == "linux", freshInstall)
	}
	return true, nil
}

// PowerShell single quoted literal with embedded quotes doubled
func psSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// Runs a schtasks query capturing raw stdout bytes, the task XML carrying its own encoding
func schtasksQueryBytes(argv []string, timeout float64, hide bool) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout*float64(time.Second)))
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = time.Second
	var stdout bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, io.Discard
	if hide {
		hideConsoleWindow(cmd)
	}
	if err := cmd.Start(); err != nil {
		return 0, nil, &spawnError{err}
	}
	err := cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		return 0, nil, &subprocessTimeout{Cmd: argv, Timeout: timeout}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), stdout.Bytes(), nil
	} else if err != nil {
		return 0, nil, &spawnError{err}
	}
	return 0, stdout.Bytes(), nil
}

// Exec Command texts of a task XML in any namespace, false when the bytes do not parse
func taskExecCommands(raw []byte) ([]string, bool) {
	var r io.Reader = bytes.NewReader(raw)
	switch {
	case bytes.HasPrefix(raw, []byte{0xff, 0xfe}), bytes.HasPrefix(raw, []byte{'<', 0}):
		r = transform.NewReader(r, unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder())
	case bytes.HasPrefix(raw, []byte{0xfe, 0xff}), bytes.HasPrefix(raw, []byte{0, '<'}):
		r = transform.NewReader(r, unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewDecoder())
	case bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}):
		r = bytes.NewReader(raw[3:])
	}
	d := xml.NewDecoder(r)
	d.Strict = true
	d.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
		l := strings.ToLower(label)
		if strings.HasPrefix(l, "utf-16") || l == "utf-8" || l == "us-ascii" || l == "ascii" {
			return input, nil
		}
		enc, err := htmlindex.Get(l)
		if err != nil {
			return nil, err
		}
		return transform.NewReader(input, enc.NewDecoder()), nil
	}
	type frame struct {
		name     string
		text     strings.Builder
		children bool
	}
	var stack []*frame
	var commands []string
	sawRoot := false
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if len(stack) == 0 {
				if sawRoot {
					return nil, false
				}
				sawRoot = true
			} else {
				stack[len(stack)-1].children = true
			}
			stack = append(stack, &frame{name: t.Name.Local})
		case xml.CharData:
			if n := len(stack); n > 0 && !stack[n-1].children {
				stack[n-1].text.Write(t)
			}
		case xml.EndElement:
			n := len(stack)
			top := stack[n-1]
			if top.name == "Command" && n >= 3 && stack[n-2].name == "Exec" {
				commands = append(commands, top.text.String())
			}
			stack = stack[:n-1]
		}
	}
	if !sawRoot {
		return nil, false
	}
	return commands, true
}

// Whether the cua-driver-serve logon task runs the selected binary, a task for an older version is not ready
var cuaDriverAutostartRegisteredWindows = func(env *tools.Env, binary string) bool {
	if cuPlatform != "win32" {
		return false
	}
	if binary == "" {
		resolved, err := cuaResolvedDriverCmd()
		if err != nil || resolved == "" {
			return false
		}
		binary = resolved
	}
	rc, out, err := cuaSchtasksQuery([]string{"schtasks.exe", "/Query", "/TN", "cua-driver-serve", "/XML"}, 10, cuaPostSetupNoWindow(false))
	if err != nil || rc != 0 {
		return false
	}
	commands, ok := taskExecCommands(out)
	if !ok {
		return false
	}
	want := pypath.Normcase(binary)
	for _, c := range commands {
		if pypath.Normcase(strings.Trim(pystr.Strip(c), `"`)) == want {
			return true
		}
	}
	return false
}

// Opt in to the per boot cua-driver-serve logon task, unreadable config failing closed to on demand
var cuaAutostartOptIn = func(env *tools.Env) (on bool) {
	defer func() {
		if recover() != nil {
			on = false
		}
	}()
	tree, err := loadCUConfig(env)
	if err != nil || tree == nil {
		return false
	}
	section := tree["computer_use"]
	if !pyrepr.Truthy(section) {
		return false
	}
	m, ok := section.(map[string]any)
	if !ok {
		return false
	}
	return pyrepr.Truthy(m["autostart"])
}

// Elevated registration body with a structured FilePath and ArgumentList so the exit code propagates
func cuaAutostartRegistrationPSCommand(binary string) string {
	return "$exe = " + psSingleQuote(binary) + "; " +
		"$proc = Start-Process -FilePath $exe -ArgumentList @('autostart','enable') " +
		"-Verb RunAs -Wait -PassThru -ErrorAction Stop; exit $proc.ExitCode"
}

// Registers the opted in autostart task for the selected binary, true when nothing needs repair
var repairCuaDriverAutostartWindows = func(env *tools.Env, con *cliui.Console, driverCmd string, verbose bool) bool {
	if cuPlatform != "win32" {
		return true
	}
	if !cuaAutostartOptIn(env) {
		return true
	}
	binary, ok := cuaSetupWhich(driverCmd)
	if !ok {
		return false
	}
	if cuaDriverAutostartRegisteredWindows(env, binary) {
		return true
	}
	ps, found := cuaSetupWhich("powershell")
	if !found {
		if ps, found = cuaSetupWhich("powershell.exe"); !found {
			ps = "powershell"
		}
	}
	if verbose {
		con.PrintInfo("    Registering cua-driver auto-start...")
	} else {
		con.PrintInfo("    Repairing cua-driver auto-start registration...")
	}
	result, err := cuaSetupRunText([]string{ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-Command", cuaAutostartRegistrationPSCommand(binary)}, 300, sanitizedCuaDriverEnv(env), cuaPostSetupNoWindow(false))
	if err != nil {
		var timeout *subprocessTimeout
		if errors.As(err, &timeout) {
			return cuaFail(con, "    cua-driver autostart registration timed out.")
		}
		return cuaFail(con, "    cua-driver autostart registration failed: "+err.Error())
	}
	if result.Returncode == 0 {
		return cuaDriverAutostartRegisteredWindows(env, binary)
	}
	con.PrintWarning("    cua-driver autostart registration failed.")
	cuaPrintOutputTail(con, result, nil)
	con.PrintInfo("    From an elevated shell, run: & " + psSingleQuote(binary) + " autostart enable")
	return false
}

// Per platform guidance after a ready install
func printCuaPlatformNotes(con *cliui.Console, isWindows, isLinux, freshInstall bool) {
	switch {
	case isWindows:
		con.PrintInfo("    cua-driver may spawn a UIAccess worker (cua-driver-uia.exe);")
		con.PrintInfo("    Windows/SmartScreen may prompt the first time it runs.")
	case isLinux:
		con.PrintWarning("    Linux support is alpha.")
	default:
		if freshInstall {
			con.PrintInfo("    IMPORTANT — grant macOS permissions now:")
		} else {
			con.PrintInfo("    Grant macOS permissions if not done yet:")
		}
		con.PrintInfo("      System Settings > Privacy & Security > Accessibility")
		con.PrintInfo("      System Settings > Privacy & Security > Screen Recording")
	}
}
