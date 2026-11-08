package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydeadline"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	launchctlBootstrapEIO     = 5
	launchdUnsupportedReason  = "launchd domain unsupported (exit 5/125)"
	launchdBootstrapTimeout   = 30 * time.Second
	launchdKickstartTimeout   = 30 * time.Second
	launchdLongTimeout        = 90 * time.Second
	launchdProbeTimeout       = 5 * time.Second
	launchdRestartVerifyWaitS = 15.0
	externalSupervisorFlag    = "--external-supervisor"
)

var (
	launchdDomainMu       sync.Mutex
	resolvedLaunchdDomain string

	launchctlRun                    = runLaunchctl
	launchdSupervisingProcess       = LaunchctlLabelSupervisingProcess
	launchdPrintServicePID          = LaunchdPrintServicePID
	launchdSpawnDetachedGateway     = spawnDetachedGateway
	launchdStartBackground          = startBackgroundProcess
	launchdPrepareServiceLauncher   = prepareServiceLauncher
	launchdRefreshPlist             = RefreshLaunchdPlistIfNeeded
	launchdRequestSelfRestart       = requestGatewaySelfRestart
	launchdWaitForGatewayExit       = waitForGatewayExit
	launchdWaitForAPIServerPortFree = waitForAPIServerPortFree
	launchdWaitForServicePID        = waitForLaunchdServicePID
	launchdRetryBootstrapSleep      = time.Sleep
	launchdGatewayExitPollInterval  = 300 * time.Millisecond
	launchdTCPConnect               = func(addr string, timeout time.Duration) (net.Conn, error) {
		return net.DialTimeout("tcp", addr, timeout)
	}
	launchdLoadGatewayConfig         = LoadGatewayConfig
	launchdServicePIDPollInterval    = 500 * time.Millisecond
	launchdTCPPortFreePollInterval   = 100 * time.Millisecond
	launchdTempRootCandidates        = []string{"/tmp", "/var/tmp", "/private/tmp", "/private/var/tmp"}
	launchdGatewayRunCommandOverride func(ctx context.Context) ([]string, error)
	launchdLabelOf                   = GetLaunchdLabel
	launchdPlistPathOf               = GetLaunchdPlistPath
	launchdDomainOf                  = launchdDomain
	launchdPlistIsCurrentFn          = LaunchdPlistIsCurrent
	launchdGeneratePlist             = GenerateLaunchdPlist
	launchdRetryBootstrap            = retryLaunchctlBootstrapUntilRegistered
	launchdRefuseTempHome            = refuseTempHomeServiceWrite
)

// Runs a launchctl argv under a timeout, captured like capture_output or on the terminal like a bare run
func runLaunchctl(ctx context.Context, timeout time.Duration, capture bool, argv ...string) (CommandResult, error) {
	if capture {
		return RunServiceCommand(ctx, timeout, argv...)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.WaitDelay = time.Second
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return CommandResult{}, fmt.Errorf("%w: %q after %v", ErrCommandTimeout, argv, timeout)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return CommandResult{ReturnCode: exitErr.ExitCode()}, nil
	}
	if err != nil && (errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist)) {
		return CommandResult{}, fmt.Errorf("%w: %q", ErrCommandNotFound, argv[0])
	}
	return CommandResult{}, err
}

// Launchctl run that raises the CalledProcessError on a non zero exit
func launchctlChecked(ctx context.Context, timeout time.Duration, capture bool, argv ...string) error {
	res, err := launchctlRun(ctx, timeout, capture, argv...)
	if err != nil {
		return err
	}
	if res.ReturnCode != 0 {
		return &CalledProcessError{Cmd: argv, ReturnCode: res.ReturnCode, Stdout: res.Stdout, Stderr: res.Stderr}
	}
	return nil
}

// True for the launchctl failures a domain probe swallows, a non zero exit, a timeout or a missing binary
func launchctlProbeMiss(res CommandResult, err error) (bool, error) {
	if err == nil {
		return res.ReturnCode != 0, nil
	}
	if errors.Is(err, ErrCommandTimeout) || errors.Is(err, ErrCommandNotFound) {
		return true, nil
	}
	return false, err
}

// Launchd domain managing the label, the gui domain then the user domain, else the managername heuristic
func probeLaunchdDomainForLabel(ctx context.Context, label string) (string, error) {
	uid := serviceGetuid()
	gui, user := fmt.Sprintf("gui/%d", uid), fmt.Sprintf("user/%d", uid)
	for _, domain := range []string{gui, user} {
		res, err := launchctlRun(ctx, launchdProbeTimeout, true, "launchctl", "print", domain+"/"+label)
		miss, err := launchctlProbeMiss(res, err)
		if err != nil {
			return "", err
		}
		if !miss {
			return domain, nil
		}
	}
	res, err := launchctlRun(ctx, launchdProbeTimeout, true, "launchctl", "managername")
	if _, err := launchctlProbeMiss(res, err); err != nil {
		return "", err
	}
	if err == nil && strings.Contains(res.Stdout, "Aqua") {
		return gui, nil
	}
	return user, nil
}

// Domain managing this profile's gateway, cached per process so start, stop and restart agree
func launchdDomain(ctx context.Context) (string, error) {
	launchdDomainMu.Lock()
	defer launchdDomainMu.Unlock()
	if resolvedLaunchdDomain == "" {
		label, err := launchdLabelOf(ctx)
		if err != nil {
			return "", err
		}
		domain, err := probeLaunchdDomainForLabel(ctx, label)
		if err != nil {
			return "", err
		}
		resolvedLaunchdDomain = domain
	}
	return resolvedLaunchdDomain, nil
}

// Forgets the cached launchd domain so the next lifecycle call probes again
func ResetLaunchdDomainCache() {
	launchdDomainMu.Lock()
	resolvedLaunchdDomain = ""
	launchdDomainMu.Unlock()
}

// True when launchctl failed because the job is not loaded in the domain
func launchdErrorIndicatesUnloaded(e *CalledProcessError) bool {
	return e.ReturnCode == 3 || e.ReturnCode == 113 || e.ReturnCode == 125
}

// True when launchctl cannot manage the domain even after a fresh bootstrap
func launchctlDomainUnsupported(returncode int) bool { return returncode == 5 || returncode == 125 }

// Bootstraps a launchd job, booting out a stale registration once on EIO before the retry
func launchctlBootstrap(ctx context.Context, domain, plistPath, label string, timeout time.Duration) error {
	bootstrap := []string{"launchctl", "bootstrap", domain, plistPath}
	err := launchctlChecked(ctx, timeout, false, bootstrap...)
	var cpe *CalledProcessError
	if !errors.As(err, &cpe) || cpe.ReturnCode != launchctlBootstrapEIO {
		return err
	}
	if _, err := launchctlRun(ctx, timeout, true, "launchctl", "bootout", domain+"/"+label); err != nil {
		return err
	}
	return launchctlChecked(ctx, timeout, false, bootstrap...)
}

// Log the launchd reload watchdog tails for persistent orphan detection
func launchdReloadLogPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "logs", "launchd-reload.log")
}

// Appends a local timestamped line to the reload log, swallowing every file error
func appendLaunchdReloadLog(ctx context.Context, message string) {
	p := launchdReloadLogPath(ctx)
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", lifecycleNow().Local().Format("2006-01-02 15:04:05 -0700"), message)
}

// Bootstrap retry window of a plist reload, the drain timeout with a thirty second floor
func launchdReloadBudget(ctx context.Context) (float64, error) {
	drain, err := getRestartDrainTimeout(ctx)
	if err != nil {
		return 0, err
	}
	return math.Max(30.0, drain), nil
}

// Retries the bootstrap every two seconds until the label supervises a process or the deadline passes
func retryLaunchctlBootstrapUntilRegistered(ctx context.Context, domain, plistPath, label string, deadline time.Time) (bool, error) {
	for attempt := 1; ; attempt++ {
		var outcome string
		err := launchctlBootstrap(ctx, domain, plistPath, label, launchdBootstrapTimeout)
		var cpe *CalledProcessError
		switch {
		case err == nil:
			if launchdSupervisingProcess(ctx, label) {
				return true, nil
			}
			outcome = fmt.Sprintf("exited 0 but %s/%s has no supervised process (launchctl list)", domain, label)
		case errors.As(err, &cpe):
			outcome = fmt.Sprintf("failed (rc=%d) for %s/%s", cpe.ReturnCode, domain, label)
		case errors.Is(err, ErrCommandTimeout):
			outcome = fmt.Sprintf("timed out for %s/%s", domain, label)
		default:
			return false, err
		}
		appendLaunchdReloadLog(ctx, fmt.Sprintf("bootstrap attempt %d %s — retrying", attempt, outcome))
		if !lifecycleNow().Before(deadline) {
			return false, nil
		}
		launchdRetryBootstrapSleep(2 * time.Second)
	}
}

// Marker that records launchd cannot supervise the gateway on this host
func launchdUnsupportedMarkerPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), ".gateway-launchd-unsupported")
}

// Python isoformat of a UTC instant, microseconds only when non zero
func utcISOFormat(t time.Time) string {
	t = t.UTC()
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s + "+00:00"
}

// Persists the unsupported marker with its write time and reason, swallowing file errors
func writeLaunchdUnsupportedMarker(ctx context.Context) {
	payload := fmt.Sprintf(`{"written_at": %s, "reason": %s}`, strconv.Quote(utcISOFormat(lifecycleNow())), strconv.Quote(launchdUnsupportedReason))
	_ = os.WriteFile(launchdUnsupportedMarkerPath(ctx), []byte(payload), 0o666)
}

// Removes the unsupported marker after launchd accepted the job again
func clearLaunchdUnsupportedMarker(ctx context.Context) {
	_ = os.Remove(launchdUnsupportedMarkerPath(ctx))
}

// True when the unsupported marker exists
func launchdUnsupportedMarkerExists(ctx context.Context) bool {
	return pathExistsSafe(launchdUnsupportedMarkerPath(ctx))
}

// Shell joined argv the way shlex.join quotes it
func shlexJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = pyshlex.Quote(a)
	}
	return strings.Join(quoted, " ")
}

// Unsupervised replacing gateway run argv pinned to the default profile when the home is the root
func gatewayRunCommand(ctx context.Context) ([]string, error) {
	if launchdGatewayRunCommandOverride != nil {
		return launchdGatewayRunCommandOverride(ctx)
	}
	arg, err := profileArg(ctx, "", "")
	if err != nil {
		return nil, err
	}
	selector, err := pyshlex.SplitPOSIX(arg, false)
	if err != nil {
		return nil, err
	}
	if len(selector) == 0 {
		mine, err1 := pypath.Resolve(home.Dir(ctx))
		root, err2 := pypath.Resolve(home.RootOf(home.Dir(ctx)))
		if err1 != nil || err2 != nil {
			return nil, errors.Join(err1, err2)
		}
		if mine == root {
			selector = []string{"--profile", "default"}
		}
	}
	args := append(selector, "gateway", "run", "--replace")
	return pm.InstallRuntimeCommand(serviceDefProjectRoot(), args, pm.LaunchCommandOptions{}), nil
}

// ProgramArguments running command under osascript so macOS grants the job a Local Network identity
func LaunchdProgramArguments(command []string, stdoutLog, stderrLog string) []string {
	shell := fmt.Sprintf("exec %s >> %s 2>> %s", shlexJoin(command), pyshlex.Quote(stdoutLog), pyshlex.Quote(stderrLog))
	javascript := `ObjC.import("stdlib"); ` +
		fmt.Sprintf("const status=$.system(%s); ", pyJSONString(shell)) +
		"const signal=status & 127; " +
		"$.exit(status === -1 ? 1 : signal === 0 ? (status >> 8) & 255 : 128 + signal);"
	return []string{"/usr/bin/osascript", "-l", "JavaScript", "-e", javascript}
}

// JSON string literal with json.dumps ensure_ascii escaping
func pyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20 || (r >= 0x80 && r <= 0xffff):
				fmt.Fprintf(&b, `\u%04x`, r)
			case r > 0xffff:
				r -= 0x10000
				fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Gateway run wrapped by the stderr timestamper, the launchd form carrying the external supervisor flag without replace
func timestampedStderrGatewayCommand(ctx context.Context, errorLog string, externalSupervisor bool) ([]string, error) {
	root := serviceDefProjectRoot()
	inner, err := gatewayRunCommand(ctx)
	if err != nil {
		return nil, err
	}
	if externalSupervisor {
		arg, err := profileArg(ctx, "", "")
		if err != nil {
			return nil, err
		}
		selector, err := pyshlex.SplitPOSIX(arg, false)
		if err != nil {
			return nil, err
		}
		full := pm.InstallationCommand(root, append(selector, "gateway", "run"), pm.LaunchCommandOptions{})
		inner = slices.DeleteFunc(full, func(part string) bool { return part == "--replace" })
		if !slices.Contains(inner, externalSupervisorFlag) {
			inner = append(inner, externalSupervisorFlag)
		}
	}
	args := append([]string{"--error-log", errorLog, "--"}, inner...)
	opts := pm.LaunchCommandOptions{Subcommand: []string{StderrTimestampEntry}}
	if externalSupervisor {
		return pm.InstallationCommand(root, args, opts), nil
	}
	return pm.InstallRuntimeCommand(root, args, opts), nil
}

// Starts argv detached with stdin and stderr on the null device and stdout on out, not waiting for it
func startBackgroundProcess(argv []string, out *os.File, env []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Env = out, env
	if out == nil {
		cmd.Stdout = nil
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// Launches the gateway detached with gateway.log stdout and timestamped gateway.error.log stderr
func spawnDetachedGateway(ctx context.Context) (bool, error) {
	logDir := filepath.Join(home.Dir(ctx), "logs")
	if err := os.MkdirAll(logDir, 0o777); err != nil {
		return false, err
	}
	childEnv, err := tools.ServedProfileChildEnv(ctx, nil, nil, home.Dir(ctx), true)
	if err != nil {
		return false, err
	}
	delete(childEnv, "_ICEBATH_GATEWAY")
	command, err := timestampedStderrGatewayCommand(ctx, filepath.Join(logDir, "gateway.error.log"), false)
	if err != nil {
		return false, err
	}
	out, err := os.OpenFile(filepath.Join(logDir, "gateway.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return false, nil
	}
	defer out.Close()
	env := make([]string, 0, len(childEnv))
	for k, v := range childEnv {
		env = append(env, k+"="+v)
	}
	slices.Sort(env)
	return launchdStartBackground(command, out, env) == nil, nil
}

// Starts the gateway detached when launchd cannot manage it, printing the manual workaround on failure
func launchdFallbackToDetached(ctx context.Context, reason string, exitOnFailure bool) (bool, error) {
	shown := home.Display(home.Dir(ctx))
	writeLaunchdUnsupportedMarker(ctx)
	lifecyclePrintf("⚠ launchd cannot manage the gateway on this macOS version (%s).\n", reason)
	started, err := launchdSpawnDetachedGateway(ctx)
	if err != nil {
		return false, err
	}
	if started {
		lifecyclePrintf("✓ Started gateway as a background process instead\n")
		lifecyclePrintf("  It will NOT auto-start at login or auto-restart on crash.\n")
		lifecyclePrintf("  Logs: %s/logs/gateway.log\n", shown)
		lifecyclePrintf("  Stop it with: icebath gateway stop\n")
		return true, nil
	}
	printLifecycleError("Failed to start the gateway as a background process.")
	lifecyclePrintf("  Try manually: nohup icebath gateway run --replace > %s/logs/gateway.log 2>&1 &\n", shown)
	if exitOnFailure {
		return false, &SystemExitError{Code: 1}
	}
	return false, nil
}

// Detached fallback for an unmanageable domain the label is not supervised in, the failure itself otherwise
func launchdDegradeOrRaise(ctx context.Context, exc *CalledProcessError, what string) error {
	if !launchctlDomainUnsupported(exc.ReturnCode) {
		return exc
	}
	label, err := launchdLabelOf(ctx)
	if err != nil {
		return err
	}
	if launchdSupervisingProcess(ctx, label) {
		lifecyclePrintf("⚠ %s failed (exit %d), but launchd still supervises %s\n", what, exc.ReturnCode, label)
		lifecyclePrintf("  Not switching to the detached fallback — this host manages the job.\n")
		lifecyclePrintf("  Apply the definition with: icebath gateway stop && icebath gateway install --force\n")
		return exc
	}
	_, err = launchdFallbackToDetached(ctx, fmt.Sprintf("%s exit %d", what, exc.ReturnCode), true)
	return err
}

// Text escaped the way html.escape quotes it
func htmlEscapeQuote(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#x27;").Replace(s)
}

// LaunchAgent plist running this profile's supervised gateway with its PATH, home and restart policy
func GenerateLaunchdPlist(ctx context.Context) (string, error) {
	workingDir := stableServiceWorkingDir(ctx)
	icebathHome, err := pypath.Resolve(home.Dir(ctx))
	if err != nil {
		return "", err
	}
	logDir := filepath.Join(home.Dir(ctx), "logs")
	if err := os.MkdirAll(logDir, 0o777); err != nil {
		return "", err
	}
	label, err := launchdLabelOf(ctx)
	if err != nil {
		return "", err
	}
	priority := appendNodeDirForService(ctx, buildServicePathDirs(ctx, ""), "")
	for _, p := range strings.Split(os.Getenv("PATH"), ":") {
		if p != "" {
			priority = append(priority, p)
		}
	}
	var unique []string
	seen := map[string]bool{}
	for _, p := range priority {
		if !seen[p] {
			seen[p] = true
			unique = append(unique, p)
		}
	}
	sanePath := strings.Join(unique, ":")
	stdoutLog, stderrLog := filepath.Join(logDir, "gateway.log"), filepath.Join(logDir, "gateway.error.log")
	command, err := timestampedStderrGatewayCommand(ctx, stderrLog, true)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, part := range LaunchdProgramArguments(command, stdoutLog, stderrLog) {
		parts = append(parts, "<string>"+htmlEscapeQuote(part)+"</string>")
	}
	progArgsXML := strings.Join(parts, "\n        ")
	nofileBlock := ""
	if target := ConfiguredNofileSoftLimit(nil); target != nil && target.Sign() != 0 {
		nofileBlock = "\n    <key>SoftResourceLimits</key>\n    <dict>\n        <key>NumberOfFiles</key>\n        <integer>" + target.String() + "</integer>\n    </dict>\n"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + label + `</string>

    <key>ProgramArguments</key>
    <array>
        ` + progArgsXML + `
    </array>

    <key>WorkingDirectory</key>
    <string>` + workingDir + `</string>

    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>` + sanePath + `</string>

        <key>ICEBATH_HOME</key>
        <string>` + icebathHome + `</string>
        <key>ICEBATH_SUPERVISED_CHILD</key>
        <string>1</string>
    </dict>

    <key>LimitLoadToSessionType</key>
    <array>
        <string>Aqua</string>
        <string>Background</string>
    </array>

    <key>RunAtLoad</key>
    <true/>

    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>

    <!-- SuccessfulExit=false parks a clean stop (exit 0), including gateway
         EX_CONFIG 78 after stderr_timestamp maps it to 0 — launchd cannot
         honor RestartPreventExitStatus, and KeepAlive=true respawned token
         collisions forever (#89477). Exit 75 and crashes still relaunch.
         ThrottleInterval raises launchd's default 10s minimum respawn interval
         to 30s so a crash-looping gateway can't hammer launchd into a rapid
         respawn storm; ExitTimeOut is the graceful-drain headroom before
         launchd escalates from SIGTERM to SIGKILL on stop. The per-user
         (gui) launchd domain clamps it to 60s regardless of what is written
         here, so 60 is the most a LaunchAgent can get; the gateway reads
         the live value at boot and fits its signal-driven drain inside it
         (gateway.restart.read_launchd_exit_timeout_s). -->
    <key>ThrottleInterval</key>
    <integer>30</integer>

    <key>ExitTimeOut</key>
    <integer>60</integer>
` + nofileBlock + `
    <key>StandardOutPath</key>
    <string>` + stdoutLog + `</string>

    <key>StandardErrorPath</key>
    <string>` + stderrLog + `</string>
</dict>
</plist>
`, nil
}

// True when the installed plist matches the generated one apart from the PATH payload
func LaunchdPlistIsCurrent(ctx context.Context) (bool, error) {
	plistPath, err := launchdPlistPathOf(ctx)
	if err != nil || !pathExistsSafe(plistPath) {
		return false, err
	}
	raw, err := os.ReadFile(plistPath)
	if err != nil {
		return false, err
	}
	data := bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		return false, fmt.Errorf("'utf-8' codec can't decode %s", plistPath)
	}
	installed := string(data)
	generated, err := launchdGeneratePlist(ctx)
	if err != nil {
		return false, err
	}
	return normalizeLaunchdPlistForComparison(installed) == normalizeLaunchdPlistForComparison(generated), nil
}

// Hands the bootout and bootstrap cycle to a transient launchctl submit job, true when it spawned
func spawnDeferredLaunchdReload(ctx context.Context, domain, label, target, plistPath string, gatewayPid int) (bool, error) {
	reloadLog := launchdReloadLogPath(ctx)
	_ = os.MkdirAll(filepath.Dir(reloadLog), 0o777)
	appendLaunchdReloadLog(ctx, "Launchd reload helper started for "+target)
	budgetF, err := launchdReloadBudget(ctx)
	if err != nil {
		return false, err
	}
	budget := int(budgetF)
	qTarget, qLabel, qLog := pyshlex.Quote(target), pyshlex.Quote(label), pyshlex.Quote(reloadLog)
	stamp := "$(date '+%Y-%m-%d %H:%M:%S %z')"
	listed := fmt.Sprintf(`launchctl list %s 2>/dev/null | grep -qE '\"PID\" = [0-9]+;'`, qLabel)
	submitLabel := fmt.Sprintf("%s.reload.%d.%d", label, os.Getpid(), lifecycleNow().Unix())
	script := "sleep 2; " +
		fmt.Sprintf("launchctl bootout %s 2>/dev/null; ", qTarget) +
		fmt.Sprintf("_wait_deadline=$(($(date +%%s) + %d)); ", budget) +
		fmt.Sprintf("while kill -0 %d 2>/dev/null; do   if [ $(date +%%s) -ge $_wait_deadline ]; then ", gatewayPid) +
		fmt.Sprintf("    echo \"[%s] old gateway pid %d still alive after %ds drain wait — bootstrapping anyway\" >> %s; ", stamp, gatewayPid, budget, qLog) +
		"    break;   fi;   sleep 1; done; " +
		fmt.Sprintf("sleep 1; _deadline=$(($(date +%%s) + %d)); while :; do ", budget) +
		fmt.Sprintf("  launchctl bootstrap %s %s 2>/dev/null; ", pyshlex.Quote(domain), pyshlex.Quote(plistPath)) +
		fmt.Sprintf("  if %s; then break; fi; ", listed) +
		fmt.Sprintf("  echo \"[%s] bootstrap not yet registered for %s — retrying\" >> %s; ", stamp, qTarget, qLog) +
		"  if [ $(date +%s) -ge $_deadline ]; then break; fi;   sleep 2; done; " +
		fmt.Sprintf("if ! %s; then ", listed) +
		fmt.Sprintf("  echo \"[%s] FAILED launchd reload for %s — service NOT registered after %ds of retries\" >> %s; ", stamp, qTarget, budget, qLog) +
		"fi; " +
		fmt.Sprintf("launchctl remove %s 2>/dev/null", pyshlex.Quote(submitLabel))
	argv := []string{"launchctl", "submit", "-l", submitLabel, "-o", reloadLog, "-e", reloadLog, "--", "/bin/bash", "-c", script}
	if err := launchdStartBackground(argv, nil, nil); err != nil {
		slog.Warn(fmt.Sprintf("Deferred launchd reload could not be spawned: %s", pythonExceptionText(err)))
		appendLaunchdReloadLog(ctx, fmt.Sprintf("FAILED to spawn launchd reload helper for %s: %s — falling back to in-process bootout/bootstrap", target, pythonExceptionText(err)))
		return false, nil
	}
	return true, nil
}

// Boots the job out, waits out the old gateway's drain, then bootstraps until launchd registers it
func bootoutDrainAndRebootstrap(ctx context.Context, domain, plistPath, label string, oldPid int, haveOld bool, budget float64) (bool, error) {
	target := domain + "/" + label
	if _, err := launchctlRun(ctx, launchdLongTimeout, true, "launchctl", "bootout", target); err != nil {
		return false, err
	}
	if haveOld && !lifecycleWaitForPIDExit(oldPid, budget, nil) {
		appendLaunchdReloadLog(ctx, fmt.Sprintf("old gateway pid %d still alive after %ds drain wait — bootstrapping %s anyway", oldPid, int(budget), target))
	}
	return launchdRetryBootstrap(ctx, domain, plistPath, label, lifecycleNow().Add(time.Duration(budget*float64(time.Second))))
}

// Rewrites a stale installed plist and reloads it so launchd reads the new definition, true when reloaded
func RefreshLaunchdPlistIfNeeded(ctx context.Context) (bool, error) {
	plistPath, err := launchdPlistPathOf(ctx)
	if err != nil || !pathExistsSafe(plistPath) {
		return false, err
	}
	current, err := launchdPlistIsCurrentFn(ctx)
	if err != nil || current {
		return false, err
	}
	if owns, err := definitionBelongsToHome(plistPath, home.Dir(ctx), "rewrite"); err != nil || !owns {
		return false, err
	}
	newPlist, err := launchdGeneratePlist(ctx)
	if err != nil {
		return false, err
	}
	if launchdRefuseTempHome(newPlist, "launchd plist") {
		return false, nil
	}
	if err := launchdPrepareServiceLauncher(ctx, false, ""); err != nil {
		return false, err
	}
	if err := os.WriteFile(plistPath, []byte(newPlist), 0o666); err != nil {
		return false, err
	}
	label, err := launchdLabelOf(ctx)
	if err != nil {
		return false, err
	}
	domain, err := launchdDomainOf(ctx)
	if err != nil {
		return false, err
	}
	target := domain + "/" + label
	gatewayPid, havePid := lifecycleRunningPid()
	if havePid {
		spawned, err := spawnDeferredLaunchdReload(ctx, domain, label, target, plistPath, gatewayPid)
		if err != nil {
			return false, err
		}
		if spawned {
			lifecyclePrintf("↻ Updated gateway launchd service definition; reload deferred to a transient launchd job (survives the bootout of this process)\n")
			return true, nil
		}
	}
	budget, err := launchdReloadBudget(ctx)
	if err != nil {
		return false, err
	}
	ok, err := bootoutDrainAndRebootstrap(ctx, domain, plistPath, label, gatewayPid, havePid, budget)
	if err != nil {
		return false, err
	}
	if !ok {
		appendLaunchdReloadLog(ctx, fmt.Sprintf("FAILED launchd reload of %s — service NOT registered after retrying for %ds (in-process fallback path)", target, int(budget)))
		slog.Error(fmt.Sprintf("launchd reload of %s failed — service not registered after %ds of retries; see %s", target, int(budget), launchdReloadLogPath(ctx)))
		return false, nil
	}
	lifecyclePrintf("↻ Updated gateway launchd service definition to match the current Icebath install\n")
	return true, nil
}

// Expanded and resolved path, false when resolution fails
func resolveOrNone(raw string) (string, bool) {
	expanded, err := pypath.PathExpanduser(raw)
	if err != nil {
		return "", false
	}
	resolved, err := pypath.Resolve(expanded)
	return resolved, err == nil
}

// False with guidance printed when the existing definition pins a home other than the given one
func definitionBelongsToHome(definitionPath, h, action string) (bool, error) {
	if !pathExistsSafe(definitionPath) {
		return true, nil
	}
	raw, err := ServicePinnedHome(definitionPath)
	if err != nil {
		return false, err
	}
	if raw == "" {
		return true, nil
	}
	a, okA := resolveOrNone(raw)
	b, okB := resolveOrNone(h)
	if okA == okB && a == b {
		return true, nil
	}
	lifecyclePrintf("✗ Refusing to %s %s: it runs ICEBATH_HOME=%s, but this process has ICEBATH_HOME=%s.\n", action, definitionPath, raw, h)
	lifecyclePrintf("  That file is another install's gateway. Use that install, or pass --force-unit-path\n")
	lifecyclePrintf("  to `icebath gateway install` if you really mean to repoint it at this home.\n")
	return false, nil
}

// Exits 1 rather than operate a LaunchAgent that pins another home
func RefuseForeignLaunchdPlist(ctx context.Context, verb string) error {
	plistPath, err := launchdPlistPathOf(ctx)
	if err != nil {
		return err
	}
	owns, err := definitionBelongsToHome(plistPath, home.Dir(ctx), verb)
	if err != nil {
		return err
	}
	if !owns {
		return &SystemExitError{Code: 1}
	}
	return nil
}

// Temp directory home baked into a service definition, empty when every pinned home is durable
func tempHomeInServiceDefinition(definition string) string {
	roots := map[string]bool{}
	if tmp, err := pypath.Gettempdir(); err == nil {
		if resolved, err := pypath.Resolve(tmp); err == nil {
			roots[resolved] = true
		}
	}
	for _, r := range launchdTempRootCandidates {
		roots[r] = true
	}
	for _, raw := range tools.ServiceDefinitionHomes(definition) {
		resolved, err := pypath.Resolve(strings.Trim(pystr.Strip(raw), `"`))
		if err != nil {
			continue
		}
		if roots[resolved] {
			return pystr.Strip(raw)
		}
		for _, parent := range pypath.PathParents(resolved) {
			if roots[parent] {
				return pystr.Strip(raw)
			}
		}
	}
	return ""
}

// Refuses with guidance when a service definition carries a temp directory home
func refuseTempHomeServiceWrite(definition, kind string) bool {
	tempHome := tempHomeInServiceDefinition(definition)
	if tempHome == "" {
		return false
	}
	lifecyclePrintf("✗ Refusing to write the gateway %s: ICEBATH_HOME resolves to a temporary directory (%s).\n", kind, tempHome)
	lifecyclePrintf("  This usually means a test/E2E environment exported ICEBATH_HOME. Unset it (or run from a clean shell) and retry.\n")
	return true
}

// Prints a launchd success line and clears the unsupported marker
func launchdOK(ctx context.Context, message string) {
	lifecyclePrintf("%s\n", message)
	clearLaunchdUnsupportedMarker(ctx)
}

// Kickstarts the label in the cached domain on the terminal
func launchctlKickstartCurrent(ctx context.Context, label string) error {
	domain, err := launchdDomainOf(ctx)
	if err != nil {
		return err
	}
	return launchctlChecked(ctx, launchdKickstartTimeout, false, "launchctl", "kickstart", domain+"/"+label)
}

// Bootstraps then kickstarts, false after degrading to the detached fallback
func launchdBootstrapAndKickstart(ctx context.Context, plistPath, label string) (bool, error) {
	domain, err := launchdDomainOf(ctx)
	if err != nil {
		return false, err
	}
	err = launchctlBootstrap(ctx, domain, plistPath, label, launchdBootstrapTimeout)
	if err == nil {
		err = launchctlKickstartCurrent(ctx, label)
	}
	var cpe *CalledProcessError
	if errors.As(err, &cpe) {
		return false, launchdDegradeOrRaise(ctx, cpe, "launchctl")
	}
	return err == nil, err
}

// Starts the launchd gateway, regenerating a missing plist and rebootstrapping an unloaded job
func LaunchdStart(ctx context.Context) error {
	if err := RefuseForeignLaunchdPlist(ctx, "start"); err != nil {
		return err
	}
	plistPath, err := launchdPlistPathOf(ctx)
	if err != nil {
		return err
	}
	label, err := launchdLabelOf(ctx)
	if err != nil {
		return err
	}
	if !pathExistsSafe(plistPath) {
		newPlist, err := launchdGeneratePlist(ctx)
		if err != nil {
			return err
		}
		if launchdRefuseTempHome(newPlist, "launchd plist") {
			return &SystemExitError{Code: 1}
		}
		lifecyclePrintf("↻ launchd plist missing; regenerating service definition\n")
		if err := os.MkdirAll(filepath.Dir(plistPath), 0o777); err != nil {
			return err
		}
		if err := launchdPrepareServiceLauncher(ctx, false, ""); err != nil {
			return err
		}
		if err := os.WriteFile(plistPath, []byte(newPlist), 0o666); err != nil {
			return err
		}
		ok, err := launchdBootstrapAndKickstart(ctx, plistPath, label)
		if ok {
			launchdOK(ctx, "✓ Service started")
		}
		return err
	}
	if _, err := launchdRefreshPlist(ctx); err != nil {
		return err
	}
	if err := launchctlKickstartCurrent(ctx, label); err != nil {
		var cpe *CalledProcessError
		if !errors.As(err, &cpe) || !launchdErrorIndicatesUnloaded(cpe) {
			return err
		}
		lifecyclePrintf("↻ launchd job was unloaded; reloading service definition\n")
		ok, err := launchdBootstrapAndKickstart(ctx, plistPath, label)
		if err != nil || !ok {
			return err
		}
	}
	launchdOK(ctx, "✓ Service started")
	return nil
}

// Stops the launchd gateway by booting the job out then waiting out or killing the recorded pid
func LaunchdStop(ctx context.Context) error {
	if err := RefuseForeignLaunchdPlist(ctx, "stop"); err != nil {
		return err
	}
	domain, err := launchdDomainOf(ctx)
	if err != nil {
		return err
	}
	label, err := launchdLabelOf(ctx)
	if err != nil {
		return err
	}
	markPlannedStop(0, false)
	if err := launchctlChecked(ctx, launchdLongTimeout, true, "launchctl", "bootout", domain+"/"+label); err != nil {
		var cpe *CalledProcessError
		if !errors.As(err, &cpe) || !(launchdErrorIndicatesUnloaded(cpe) || launchctlDomainUnsupported(cpe.ReturnCode)) {
			return err
		}
	}
	launchdWaitForGatewayExit(10.0, 5.0, true)
	lifecyclePrintf("✓ Service stopped\n")
	return nil
}

// Kickstart with kill of the label in the domain, captured, raising so callers own failure accounting
func launchdKickstart(ctx context.Context, label, domain string) error {
	return launchctlChecked(ctx, launchdLongTimeout, true, "launchctl", "kickstart", "-k", domain+"/"+label)
}

// Polls the label every half second until it runs a fresh pid or the timeout passes
func waitForLaunchdServicePID(ctx context.Context, label string, oldPid int, haveOld bool, timeout float64, domain string) (bool, error) {
	var probeErr error
	fresh := pydeadline.PollUntil(func() (bool, bool) {
		_, pid, ok, err := launchdPrintServicePID(ctx, domain, label)
		if err != nil {
			probeErr = err
			return false, true
		}
		got := ok && pid > 0 && (!haveOld || pid != oldPid)
		return got, got
	}, time.Duration(math.Max(timeout, 0.5)*float64(time.Second)), launchdServicePIDPollInterval)
	return fresh && probeErr == nil, probeErr
}

// True when pid is this process or one of its ancestors
func isPidAncestorOfCurrentProcess(target int) bool {
	if target <= 0 {
		return false
	}
	seen := map[int]bool{}
	for pid := os.Getpid(); pid != 0 && !seen[pid]; {
		if pid == target {
			return true
		}
		seen[pid] = true
		parent, ok := getParentPID(pid)
		if !ok {
			parent = 0
		}
		pid = parent
	}
	return false
}

// Asks a running gateway ancestor to restart itself through SIGUSR1, true when the signal went out
func requestGatewaySelfRestart(pid int) bool {
	if !hasSIGUSR1 || !isPidAncestorOfCurrentProcess(pid) {
		return false
	}
	return lifecycleSendSIGUSR1(pid) == nil
}

// Waits for the recorded gateway pid to exit, force killing it after the grace period, true once gone
func waitForGatewayExit(timeout, forceAfter float64, haveForce bool) bool {
	start := lifecycleNow()
	deadline := start.Add(time.Duration(timeout * float64(time.Second)))
	forceDeadline := start.Add(time.Duration(forceAfter * float64(time.Second)))
	forceSent := false
	for lifecycleNow().Before(deadline) {
		pid, ok := lifecycleRunningPid()
		if !ok {
			return true
		}
		if haveForce && !forceSent && !lifecycleNow().Before(forceDeadline) {
			if err := lifecycleTerminatePid(pid, true, lifecycleProcessStart(pid)); err != nil {
				return true
			}
			lifecyclePrintf("⚠ Gateway PID %d did not exit gracefully; sent SIGKILL\n", pid)
			forceSent = true
		}
		lifecycleSleep(launchdGatewayExitPollInterval)
	}
	if remaining, ok := lifecycleRunningPid(); ok {
		lifecyclePrintf("⚠ Gateway PID %d still running after %ss — restart may fail\n", remaining, pyrFloat(timeout))
		return false
	}
	return true
}

// Waits until nothing accepts TCP connections on host and port, a refused or unreachable connect counting as free
func waitForTCPPortFree(host string, port int, timeout float64) bool {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	return pydeadline.PollUntil(func() (bool, bool) {
		conn, err := launchdTCPConnect(addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return false, false
		}
		if errors.Is(err, syscall.ECONNREFUSED) {
			return true, true
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return false, false
		}
		return true, true
	}, time.Duration(timeout*float64(time.Second)), launchdTCPPortFreePollInterval)
}

// Waits for an enabled api_server listen address to stop accepting, warning when it never frees
func waitForAPIServerPortFree(ctx context.Context, timeout float64) (bool, error) {
	cfg, err := launchdLoadGatewayConfig(ctx)
	if err != nil {
		return false, err
	}
	pconfig := cfg.Platforms[PlatformAPIServer]
	if pconfig == nil || !pconfig.Enabled {
		return true, nil
	}
	extra := pconfig.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	hostValue, port := APIServerListenAddress(extra)
	host := pyStrValue(hostValue)
	freed := waitForTCPPortFree(host, port, timeout)
	if !freed {
		lifecyclePrintf("⚠ %s:%d still accepting connections — new api_server may fail to bind\n", host, port)
	}
	return freed, nil
}

// Restarts the launchd gateway gracefully, escalating a wedged loop and reloading an unloaded job
func LaunchdRestart(ctx context.Context) error {
	if err := RefuseForeignLaunchdPlist(ctx, "restart"); err != nil {
		return err
	}
	label, err := launchdLabelOf(ctx)
	if err != nil {
		return err
	}
	domain, err := launchdDomainOf(ctx)
	if err != nil {
		return err
	}
	target := domain + "/" + label
	refreshOK, err := launchdRefreshPlist(ctx)
	if err != nil {
		return err
	}
	err = launchdRestartBody(ctx, label, domain, target, refreshOK)
	var cpe *CalledProcessError
	if !errors.As(err, &cpe) {
		return err
	}
	if !launchdErrorIndicatesUnloaded(cpe) {
		return launchdDegradeOrRaise(ctx, cpe, "launchctl kickstart")
	}
	lifecyclePrintf("↻ launchd job was unloaded; reloading\n")
	err = launchdReloadUnloaded(ctx, target)
	if errors.As(err, &cpe) {
		return launchdDegradeOrRaise(ctx, cpe, "launchctl")
	}
	if err != nil {
		return err
	}
	launchdOK(ctx, "✓ Service restarted")
	return nil
}

// Boots out the drained job then bootstraps and kickstarts it fresh
func launchdReloadUnloaded(ctx context.Context, target string) error {
	if _, err := launchctlRun(ctx, launchdLongTimeout, true, "launchctl", "bootout", target); err != nil {
		return err
	}
	return launchdBootstrapKickstartTarget(ctx, target)
}

// Bootstraps the installed plist into the cached domain and kickstarts the target on the terminal
func launchdBootstrapKickstartTarget(ctx context.Context, target string) error {
	plistPath, err := launchdPlistPathOf(ctx)
	if err != nil {
		return err
	}
	domain, err := launchdDomainOf(ctx)
	if err != nil {
		return err
	}
	if err := launchctlChecked(ctx, launchdBootstrapTimeout, false, "launchctl", "bootstrap", domain, plistPath); err != nil {
		return err
	}
	return launchctlChecked(ctx, launchdKickstartTimeout, false, "launchctl", "kickstart", target)
}

// Restart attempt whose launchctl failures surface as CalledProcessError for the reload policy
func launchdRestartBody(ctx context.Context, label, domain, target string, refreshOK bool) error {
	pid, havePid := lifecycleRunningPid()
	if havePid && launchdRequestSelfRestart(pid) {
		launchdOK(ctx, "✓ Service restart requested")
		return nil
	}
	if havePid && lifecycleProbeLiveness(ctx, pid, LoopLivenessOptions{}) == GatewayLoopWedged {
		lifecyclePrintf("⚠ Gateway PID %d event loop is unresponsive — skipping drain and forcing a bounded stop...\n", pid)
		lifecycleEscalate(pid, escalateDefaultTermGraceS, escalateDefaultKillWaitS)
		havePid = false
	}
	if havePid {
		budget, err := lifecycleRestartExitWaitBudget(ctx)
		if err != nil {
			return err
		}
		lifecyclePrintf("→ Stopping gateway (PID %d) — draining in-flight runs (up to %.0fs)...\n", pid, budget)
		hook := DrainProgressReporterHook
		if hook == nil {
			panic("gateway: the hermes_cli/update_cmd_drain_report.py port must install DrainProgressReporterHook")
		}
		if lifecycleGracefulRestart(pid, budget, hook(ctx, "", budget)) {
			revived, err := launchdWaitForServicePID(ctx, label, pid, true, launchdRestartVerifyWaitS, domain)
			if err != nil {
				return err
			}
			if revived {
				launchdOK(ctx, "✓ Service restart requested")
				return nil
			}
			lifecyclePrintf("⚠ launchd did not revive the gateway after its graceful exit — forcing restart\n")
		} else {
			lifecyclePrintf("⚠ Gateway drain timed out after %.0fs — forcing launchd restart\n", budget)
		}
	}
	if !refreshOK {
		plistPath, err := launchdPlistPathOf(ctx)
		if err != nil {
			return err
		}
		if pathExistsSafe(plistPath) {
			current, err := launchdPlistIsCurrentFn(ctx)
			if err != nil {
				return err
			}
			if !current {
				lifecyclePrintf("↻ launchd job was not re-registered by the plist refresh; reloading\n")
				if err := launchdBootstrapKickstartTarget(ctx, target); err != nil {
					return err
				}
				launchdOK(ctx, "✓ Service restarted")
				return nil
			}
		}
	}
	if _, err := launchdWaitForAPIServerPortFree(ctx, 10.0); err != nil {
		return err
	}
	if err := launchctlChecked(ctx, launchdLongTimeout, true, "launchctl", "kickstart", "-k", target); err != nil {
		return err
	}
	launchdOK(ctx, "✓ Service restarted")
	return nil
}
