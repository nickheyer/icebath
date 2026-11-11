package dashboard

import (
	"context"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Prefix of the metadata text frame a host terminal sends before its replay
const hostTerminalMetaPrefix = "\x00ICEBATH_TERMINAL_META:"

// Whether the app serves host shells, only on the Webapp surface bound to loopback or behind authentication
func hostTerminalRequestAllowed(app *FastAPI) bool {
	if !PolicyOf(app.State).HostTerminal {
		return false
	}
	if app.State.Bool("auth_required") {
		return true
	}
	bound, _ := app.State.Get("bound_host").(string)
	return wsLoopbackHosts[strings.ToLower(pystr.Strip(bound))]
}

// Executable shell path of a candidate, empty when it is blank, relative or not found
func hostShellCommand(candidate string) string {
	raw := pystr.Strip(candidate)
	if raw == "" {
		return ""
	}
	if found := tools.LocateCommand(raw); len(found) > 0 {
		return found[0]
	}
	return ""
}

// Interactive shell argv and lowercase name from the native Desktop ladder of this platform
func hostShellSpec() ([]string, string) {
	var ladder []string
	var fallback string
	if runtime.GOOS == "windows" {
		systemRoot := os.Getenv("SystemRoot")
		if systemRoot == "" {
			systemRoot = os.Getenv("windir")
		}
		if systemRoot == "" {
			systemRoot = `C:\Windows`
		}
		windowsPowerShell := filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
		ladder = []string{"pwsh.exe", "pwsh", windowsPowerShell, "powershell.exe", os.Getenv("COMSPEC")}
		fallback = "cmd.exe"
	} else {
		ladder = []string{os.Getenv("SHELL"), "/bin/zsh", "/bin/bash", "/bin/sh"}
		fallback = "/bin/sh"
	}
	command := fallback
	for _, candidate := range ladder {
		if found := hostShellCommand(candidate); found != "" {
			command = found
			break
		}
	}
	name := strings.ToLower(filepath.Base(command))
	var args []string
	switch {
	case strings.HasPrefix(name, "pwsh") || strings.HasPrefix(name, "powershell"):
		args = []string{"-NoLogo"}
	case strings.HasPrefix(name, "cmd"):
		args = []string{}
	case strings.Contains(name, "zsh") || strings.Contains(name, "bash"):
		args = []string{"-il"}
	default:
		args = []string{"-i"}
	}
	return append([]string{command}, args...), name
}

// Requested directory resolved, a file's parent, or the user home when it is neither
func hostSafeCwd(requested string) (string, error) {
	fallback, err := pypath.PathHome()
	if err != nil {
		return "", err
	}
	raw := pystr.Strip(requested)
	if raw == "" {
		raw = fallback
	}
	expanded, err := pypath.PathExpanduser(raw)
	if err != nil {
		return fallback, nil
	}
	candidate, err := pypath.Resolve(expanded)
	if err != nil {
		return fallback, nil
	}
	if st, err := os.Stat(candidate); err == nil {
		if st.IsDir() {
			return candidate, nil
		}
		if st.Mode().IsRegular() {
			return pypath.PathParent(candidate), nil
		}
	}
	return fallback, nil
}

// LC_CTYPE a host shell env needs added, false to leave it alone
func terminalLCCtype(env map[string]string, platform string) (string, bool) {
	if env["LC_ALL"] != "" || env["LC_CTYPE"] != "" || platform == "win32" {
		return "", false
	}
	if platform == "darwin" {
		return "UTF-8", true
	}
	if env["LANG"] != "" {
		return "", false
	}
	return "C.UTF-8", true
}

// Argv, cwd, env and shell name of the Webapp host terminal acting for profile home h
func resolveHostTerminalArgv(ctx context.Context, h, requestedCwd string) ([]string, string, map[string]string, string, error) {
	resolved, err := pypath.Resolve(h)
	if err != nil {
		return nil, "", nil, "", err
	}
	process, err := pypath.Resolve(home.Process())
	if err != nil {
		return nil, "", nil, "", err
	}
	sctx := home.WithOverride(ctx, h)
	if resolved != process {
		provider.ActivateMultiProfileHosting()
		sctx = gateway.ProfileRuntimeScope(sctx, h, true)
	} else {
		sctx = provider.WithLaunchProfileRuntimeScope(sctx, h)
	}
	if err := tools.EnforceNoRefusal(sctx); err != nil {
		return nil, "", nil, "", err
	}
	baseEnv, err := tools.ServedProfileChildEnv(sctx, nil, nil, h, false)
	if err != nil {
		return nil, "", nil, "", err
	}
	for _, pair := range config.TerminalConfigEnvMap {
		delete(baseEnv, pair[1])
	}
	scope := tools.GetTerminalScope(sctx)
	if scope == nil {
		return nil, "", nil, "", errors.New("AssertionError: no terminal scope bound for the host terminal")
	}
	for key, value := range scope.Policy {
		baseEnv[key] = value
	}
	env, err := tools.BuildSubprocessEnv(sctx, nil, tools.SubprocessEnvOptions{Base: baseEnv, InheritProfileHome: true, ScrubSecrets: true})
	if err != nil {
		return nil, "", nil, "", err
	}
	for key := range env {
		if key == "npm_config_prefix" || strings.HasPrefix(key, "npm_config_") || strings.HasPrefix(key, "npm_package_") {
			delete(env, key)
		}
	}
	for _, key := range []string{"NO_COLOR", "FORCE_COLOR", "COLORFGBG"} {
		delete(env, key)
	}
	env["COLORTERM"] = "truecolor"
	env["TERM"] = "xterm-256color"
	env["TERM_PROGRAM"] = "Icebath"
	env["TERM_PROGRAM_VERSION"] = pm.DunderVersion()
	env["ICEBATH_DESKTOP_TERMINAL"] = "1"
	if lcCtype, ok := terminalLCCtype(env, hostinfo.OSFamily()); ok {
		env["LC_CTYPE"] = lcCtype
	}
	argv, shellName := hostShellSpec()
	cwd, err := hostSafeCwd(requestedCwd)
	if err != nil {
		return nil, "", nil, "", err
	}
	return argv, cwd, env, shellName, nil
}

// Query dimension as an int clamped to 2 through maximum, the default when absent or not an int
func queryDimension(raw string, def, maximum int) int {
	if raw == "" {
		return max(2, min(maximum, def))
	}
	n, ok := pystr.IntText(raw)
	if !ok {
		return def
	}
	if n.Cmp(big.NewInt(int64(maximum))) > 0 {
		return max(2, maximum)
	}
	if n.Cmp(big.NewInt(2)) < 0 {
		return 2
	}
	return int(n.Int64())
}
