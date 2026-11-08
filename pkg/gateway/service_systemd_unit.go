package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

var (
	systemdUnitLookPath         = exec.LookPath
	systemdUnitIsWSL            = hostinfo.IsWSL
	systemdOptionalDirectives   = []string{"RestartMaxDelaySec", "RestartSteps"}
	systemdUnitTestHomeMarkers  = []string{"/pytest-of-", `/icebath_test"`, "/icebath_test/"}
	systemdDaemonReloadTimeout  = 30 * time.Second
	systemdUnitExecutable       = os.Executable
	systemdReplaceDropinTokens  = []string{"Added to end the gateway respawn storm", "--replace", "ExecStart="}
	systemdUnitPathOf           = GetSystemdUnitPath
	systemdGenerateUnit         = GenerateSystemdUnit
	systemdUnitCurrent          = SystemdUnitIsCurrent
	systemdRetireDropin         = retireReplaceDropin
	systemdPrepareLauncher      = prepareServiceLauncher
	systemdRefuseTempHome       = refuseTempHomeServiceWrite
	systemdLoadGatewayConfig    = LoadGatewayConfig
	systemdServiceIdentity      = systemServiceIdentity
	systemdRestartDrain         = getRestartDrainTimeout
	systemdCronDrain            = getCronDrainTimeout
	systemdServicePathDirs      = buildServicePathDirs
	systemdUserLocalPaths       = buildUserLocalPaths
	systemdPythonPath           = getServicePythonPath
	systemdTargetHome           = hermesHomeForTargetUser
	systemdWSLInteropCandidates = []string{
		"/mnt/c/WINDOWS/system32", "/mnt/c/WINDOWS", "/mnt/c/WINDOWS/System32/Wbem",
		"/mnt/c/WINDOWS/System32/WindowsPowerShell/v1.0/", "/mnt/c/WINDOWS/System32/OpenSSH/",
	}
)

// Text of a file read as utf-8-sig, ok false when the file cannot be read
func readUnitText(p string) (string, bool, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", false, nil
	}
	if !utf8.Valid(raw) {
		return "", false, errUnitDecode
	}
	return strings.TrimPrefix(string(raw), "\ufeff"), true, nil
}

// Interpreter the unit runs, the install binary of the project root or else this executable
func getServicePythonPath() string {
	if binary := pm.ResolveInstallBinary(serviceDefProjectRoot()); binary != "" {
		return binary
	}
	exe, _ := systemdUnitExecutable()
	return exe
}

// User local bin dirs that exist and are not already listed
func buildUserLocalPaths(userHome string, entries []string) []string {
	var out []string
	for _, rel := range [][]string{{".local", "bin"}, {".cargo", "bin"}, {"go", "bin"}, {".npm-global", "bin"}} {
		p := pypath.PathJoin(pypath.PathJoin(userHome, rel[0]), rel[1])
		if !slices.Contains(entries, p) && pathExistsSafe(p) {
			out = append(out, p)
		}
	}
	return out
}

// WSL interop tool dirs and the System32 family not already listed, nothing outside WSL
func buildWSLInteropPaths(entries []string) []string {
	if !systemdUnitIsWSL() {
		return nil
	}
	var candidates []string
	for _, name := range []string{"powershell.exe", "cmd.exe", "explorer.exe", "wsl.exe"} {
		if resolved, err := systemdUnitLookPath(name); err == nil && resolved != "" {
			candidates = append(candidates, pypath.PathStr(filepath.Dir(resolved)))
		}
	}
	for _, entry := range systemdWSLInteropCandidates {
		if pathExistsSafe(entry) {
			candidates = append(candidates, entry)
		}
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e] = true
	}
	var out []string
	for _, entry := range candidates {
		if entry != "" && !seen[entry] {
			seen[entry] = true
			out = append(out, entry)
		}
	}
	return out
}

// One Environment line with backslash and quote escaped and percent doubled for systemd
func systemdEnvLine(name, value string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(value)
	return `Environment="` + name + "=" + escaped + "\"\n"
}

// Argv quoted for systemd with specifier and variable expansion escaped
func systemdCommand(argv []string) string {
	quoted := make([]string, len(argv))
	for i, part := range argv {
		quoted[i] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(part) + `"`
	}
	return strings.Join(quoted, " ")
}

// LD_LIBRARY_PATH line from the environment or else the installed unit, components remapped for a system unit
func ldLibraryPathLine(ctx context.Context, system bool, targetHomeDir string) (string, error) {
	raw := os.Getenv("LD_LIBRARY_PATH")
	if raw == "" {
		unitPath, err := systemdUnitPathOf(ctx, system)
		if err != nil {
			return "", err
		}
		if raw, err = unitEnvironmentValue(unitPath, "LD_LIBRARY_PATH"); err != nil {
			return "", err
		}
	}
	var components []string
	for _, p := range strings.Split(raw, ":") {
		if p == "" {
			continue
		}
		if system {
			remapped, err := remapPathForUser(p, targetHomeDir)
			if err != nil {
				return "", err
			}
			p = remapped
		}
		components = append(components, p)
	}
	if len(components) == 0 {
		return "", nil
	}
	return systemdEnvLine("LD_LIBRARY_PATH", strings.Join(components, ":")), nil
}

// Watchdog seconds of the service home's effective gateway config, zero when it cannot be loaded
func systemdWatchdogSeconds(ctx context.Context, icebathHome string) int {
	cfg, err := systemdLoadGatewayConfig(home.WithOverride(ctx, icebathHome))
	if err != nil || cfg == nil {
		return 0
	}
	return CoerceSystemdWatchdogSeconds(cfg.SystemdWatchdogSeconds, "")
}

// Systemd unit running this profile's supervised gateway, a system unit as the target account
func GenerateSystemdUnit(ctx context.Context, system bool, runAsUser string) (string, error) {
	pythonPath := systemdPythonPath()
	workingDir := stableServiceWorkingDir(ctx)
	projectRoot := serviceDefProjectRoot()
	pathEntries := systemdServicePathDirs(ctx, "")
	if !system {
		pathEntries = appendNodeDirForService(ctx, pathEntries, "")
	}
	drain, err := systemdRestartDrain(ctx)
	if err != nil {
		return "", err
	}
	cronDrain, err := systemdCronDrain(ctx)
	if err != nil {
		return "", err
	}
	restartTimeout, err := ResolveSystemdTimeoutStopSec(drain, cronDrain, CronDrainCleanupReserveS, SystemdStopHeadroomS, SystemdTimeoutStopSecFloor)
	if err != nil {
		return "", err
	}
	var icebathHome, profile, userHome, identityLines, orderingLines, envLines, wantedBy string
	if system {
		username, groupName, homeDir, uid, err := systemdServiceIdentity(runAsUser)
		if err != nil {
			return "", err
		}
		if icebathHome, err = systemdTargetHome(ctx, homeDir); err != nil {
			return "", err
		}
		targetRoot := pypath.PathJoin(homeDir, ".icebath")
		resolvedHome, errHome := pypath.Resolve(icebathHome)
		resolvedRoot, errRoot := pypath.Resolve(targetRoot)
		if errHome != nil {
			return "", errHome
		}
		if errRoot != nil {
			return "", errRoot
		}
		if _, ok := pypath.PathRelativeTo(resolvedHome, resolvedRoot); ok {
			profile, err = profileArg(ctx, icebathHome, targetRoot)
		} else {
			profile, err = profileArg(ctx, icebathHome, "")
		}
		if err != nil {
			return "", err
		}
		if pythonPath, err = remapPathForUser(pythonPath, homeDir); err != nil {
			return "", err
		}
		if icebathHome != "" {
			workingDir = icebathHome
		} else if workingDir, err = remapPathForUser(workingDir, homeDir); err != nil {
			return "", err
		}
		if projectRoot, err = remapPathForUser(projectRoot, homeDir); err != nil {
			return "", err
		}
		for i, p := range pathEntries {
			if pathEntries[i], err = remapPathForUser(p, homeDir); err != nil {
				return "", err
			}
		}
		var targetNode []string
		targetNode = appendNodeDirForService(ctx, targetNode, icebathHome)
		var merged []string
		for _, e := range targetNode {
			if !slices.Contains(pathEntries, e) {
				merged = append(merged, e)
			}
		}
		pathEntries = append(merged, pathEntries...)
		userHome = homeDir
		identityLines = "User=" + username + "\nGroup=" + groupName + "\n"
		orderingLines = fmt.Sprintf("After=user@%d.service\nWants=user@%d.service\n", uid, uid)
		ld, err := ldLibraryPathLine(ctx, true, homeDir)
		if err != nil {
			return "", err
		}
		envLines = `Environment="HOME=` + homeDir + "\"\n" + `Environment="USER=` + username + "\"\n" + `Environment="LOGNAME=` + username + "\"\n" + ld
		wantedBy = "multi-user.target"
	} else {
		if icebathHome, err = pypath.Resolve(home.Dir(ctx)); err != nil {
			return "", err
		}
		if profile, err = profileArg(ctx, icebathHome, ""); err != nil {
			return "", err
		}
		if userHome, err = serviceDefUserHome(); err != nil {
			return "", err
		}
		if envLines, err = ldLibraryPathLine(ctx, false, ""); err != nil {
			return "", err
		}
		wantedBy = "default.target"
	}
	systemdType, watchdogDirectives := "simple", ""
	if seconds := systemdWatchdogSeconds(ctx, icebathHome); seconds > 0 {
		systemdType, watchdogDirectives = "notify", fmt.Sprintf("NotifyAccess=main\nWatchdogSec=%ds\n", seconds)
	}
	pathEntries = append(pathEntries, systemdUserLocalPaths(userHome, pathEntries)...)
	pathEntries = append(pathEntries, buildWSLInteropPaths(pathEntries)...)
	pathEntries = append(pathEntries, "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin")
	selector, err := pyshlex.SplitPOSIX(profile, false)
	if err != nil {
		return "", err
	}
	opts := pm.LaunchCommandOptions{Binary: pythonPath, Home: icebathHome}
	start := pm.InstallationCommand(projectRoot, append(selector, "gateway", "run"), opts)
	opts.Subcommand = []string{CgroupCleanupEntry}
	cleanup := pm.InstallationCommand(projectRoot, nil, opts)
	opts.Subcommand = []string{SystemdStopMarkEntry}
	stopMark := pm.InstallationCommand(projectRoot, nil, opts)
	return "[Unit]\nDescription=" + ServiceDescription + "\nAfter=network-online.target\nWants=network-online.target\n" +
		orderingLines + "StartLimitIntervalSec=0\n\n[Service]\nType=" + systemdType + "\n" +
		watchdogDirectives + identityLines + "ExecStart=" + systemdCommand(start) + "\nWorkingDirectory=" + workingDir + "\n" +
		envLines + `Environment="PATH=` + strings.Join(pathEntries, ":") + "\"\n\n" +
		`Environment="ICEBATH_HOME=` + icebathHome + "\"\n" + `Environment="ICEBATH_SUPERVISED_CHILD=1"` + "\n" +
		"Restart=always\nRestartSec=5\n" +
		fmt.Sprintf("RestartForceExitStatus=%d\nSuccessExitStatus=%d\nRestartPreventExitStatus=%d\n", GatewayServiceRestartExitCode, GatewayServiceRestartExitCode, GatewayFatalConfigExitCode) +
		"KillMode=mixed\nKillSignal=SIGTERM\nExecReload=/bin/kill -USR1 $MAINPID\n" +
		"ExecStop=-" + systemdCommand(stopMark) + "\nExecStopPost=-" + systemdCommand(cleanup) + "\n" +
		"TimeoutStopSec=" + restartTimeout.String() + "\nStandardOutput=journal\nStandardError=journal\n\n[Install]\nWantedBy=" + wantedBy + "\n", nil
}

// Unit text without the directives older systemd silently drops
func stripOptionalSystemdDirectives(text string) string {
	var kept []string
	for _, line := range pystr.Splitlines(text, false) {
		stripped := pystr.Strip(line)
		key, _, _ := strings.Cut(stripped, "=")
		if stripped != "" && !strings.HasPrefix(stripped, "#") && slices.Contains(systemdOptionalDirectives, pystr.Strip(key)) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// Account named by the unit's first User line, empty when absent or blank
func readSystemdUserFromUnit(unitPath string) (string, error) {
	if !pathExistsSafe(unitPath) {
		return "", nil
	}
	raw, err := os.ReadFile(unitPath)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(raw) {
		return "", errUnitDecode
	}
	for _, line := range pystr.Splitlines(strings.TrimPrefix(string(raw), "\ufeff"), false) {
		if strings.HasPrefix(line, "User=") {
			_, value, _ := strings.Cut(line, "=")
			return pystr.Strip(value), nil
		}
	}
	return "", nil
}

// True when the installed unit matches the generated one apart from optional directives, after syncing the unit home
func SystemdUnitIsCurrent(ctx context.Context, system bool) (bool, error) {
	if err := lifecycleSyncHome(ctx, system); err != nil {
		return false, err
	}
	unitPath, err := systemdUnitPathOf(ctx, system)
	if err != nil || !pathExistsSafe(unitPath) {
		return false, err
	}
	raw, err := os.ReadFile(unitPath)
	if err != nil {
		return false, err
	}
	if !utf8.Valid(raw) {
		return false, errUnitDecode
	}
	installed := strings.TrimPrefix(string(raw), "\ufeff")
	expectedUser := ""
	if system {
		if expectedUser, err = readSystemdUserFromUnit(unitPath); err != nil {
			return false, err
		}
	}
	expected, err := systemdGenerateUnit(ctx, system, expectedUser)
	if err != nil {
		return false, err
	}
	norm := func(text string) string { return normalizeServiceDefinition(stripOptionalSystemdDirectives(text)) }
	return norm(installed) == norm(expected), nil
}

// Removes the legacy replace drop-in only when its text proves an older install wrote it
func retireReplaceDropin(ctx context.Context, system bool) (bool, error) {
	unitPath, err := systemdUnitPathOf(ctx, system)
	if err != nil {
		return false, err
	}
	dropin := filepath.Join(filepath.Dir(unitPath), filepath.Base(unitPath)+".d", "20-replace.conf")
	text, ok, err := readUnitText(dropin)
	if !ok {
		return false, err
	}
	for _, token := range systemdReplaceDropinTokens {
		if !strings.Contains(text, token) {
			return false, nil
		}
	}
	if err := os.Remove(dropin); err != nil {
		return false, err
	}
	return true, nil
}

// Home this process acts for when writing the unit, a system unit's account remap or else its pinned home
func serviceHomeForUnit(ctx context.Context, unitPath string, system bool) (string, error) {
	if !system {
		return home.Dir(ctx), nil
	}
	if pystr.Strip(os.Getenv("ICEBATH_HOME")) == "" {
		pinned := ""
		if pathExistsSafe(unitPath) {
			var err error
			if pinned, err = ServicePinnedHome(unitPath); err != nil {
				return "", err
			}
		}
		if pinned != "" {
			return pypath.PathExpanduser(pinned)
		}
		return home.Dir(ctx), nil
	}
	user, err := readSystemdUserFromUnit(unitPath)
	if err != nil {
		return "", err
	}
	if user == "" {
		return home.Dir(ctx), nil
	}
	_, _, homeDir, _, err := systemdServiceIdentity(user)
	var userErr *ServiceUserError
	if errors.As(err, &userErr) {
		return home.Dir(ctx), nil
	}
	if err != nil {
		return "", err
	}
	return systemdTargetHome(ctx, homeDir)
}

// Whether the unit pins the home this process would pin there, guidance printed when it does not
func unitBelongsToCaller(ctx context.Context, unitPath string, system bool, action, runAsUser string) (bool, error) {
	h, err := serviceHomeForUnit(ctx, unitPath, system)
	if err != nil {
		return false, err
	}
	if system && runAsUser != "" {
		_, _, homeDir, _, err := systemdServiceIdentity(runAsUser)
		var userErr *ServiceUserError
		switch {
		case errors.As(err, &userErr):
		case err != nil:
			return false, err
		default:
			if h, err = systemdTargetHome(ctx, homeDir); err != nil {
				return false, err
			}
		}
	}
	return definitionBelongsToHome(unitPath, h, action)
}

// Rewrites the installed unit of this home when its generated definition changed, true when it acted
func refreshSystemdUnitIfNeeded(ctx context.Context, system bool) (bool, error) {
	unitPath, err := systemdUnitPathOf(ctx, system)
	if err != nil || !pathExistsSafe(unitPath) {
		return false, err
	}
	if ok, err := unitBelongsToCaller(ctx, unitPath, system, "rewrite", ""); err != nil || !ok {
		return false, err
	}
	current, err := systemdUnitCurrent(ctx, system)
	if err != nil {
		return false, err
	}
	retired, err := systemdRetireDropin(ctx, system)
	if err != nil {
		return false, err
	}
	if retired {
		if _, err := runSystemctlChecked(ctx, []string{"daemon-reload"}, system, systemdDaemonReloadTimeout); err != nil {
			return false, err
		}
		lifecyclePrintf("↻ Removed the stale Icebath --replace drop-in from the gateway %s service\n", ServiceScopeLabel(system))
		if current {
			return true, nil
		}
	} else if current {
		return false, nil
	}
	expectedUser := ""
	if system {
		if expectedUser, err = readSystemdUserFromUnit(unitPath); err != nil {
			return false, err
		}
	}
	newUnit, err := systemdGenerateUnit(ctx, system, expectedUser)
	if err != nil {
		return false, err
	}
	if !system && slices.ContainsFunc(systemdUnitTestHomeMarkers, func(m string) bool { return strings.Contains(newUnit, m) }) {
		return false, nil
	}
	if systemdRefuseTempHome(newUnit, "systemd unit") {
		return false, nil
	}
	if err := systemdPrepareLauncher(ctx, system, expectedUser); err != nil {
		return false, err
	}
	if err := os.WriteFile(unitPath, []byte(newUnit), 0o666); err != nil {
		return false, err
	}
	if _, err := runSystemctlChecked(ctx, []string{"daemon-reload"}, system, systemdDaemonReloadTimeout); err != nil {
		return false, err
	}
	lifecyclePrintf("↻ Updated gateway %s service definition to match the current Icebath install\n", ServiceScopeLabel(system))
	return true, nil
}
