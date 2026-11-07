package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Spawn plumbing of the process registry, systemd scopes, output logs, stdin reasons, termination, wsl

// Hooks the gateway and the worker runtime install for the registry
var (
	// SupervisedGatewayPid reports whether this launch is a supervised gateway and the live gateway pid, installed by the gateway
	SupervisedGatewayPid func() (supervisedLaunch bool, runningPid int)
	// IsWorkerProcess reports whether this process is a registered per turn worker, installed by the agent runtime
	IsWorkerProcess func() bool
)

var (
	systemdScopeMu            sync.Mutex
	systemdScopeAvailable     *bool
	systemdScopeProbedAt      time.Time
	systemdScopeProbeTTL      = 60 * time.Second
	systemdRunNoExpand        = true
	scopeDegradedWarned       bool
	minWorkerMemoryMaxBytes   = int64(64 * 1024 * 1024)
	defaultWorkerMemoryMax    = int64(1024 * 1024 * 1024)
	workerMemoryMaxCapBytes   = int64(4 * 1024 * 1024 * 1024)
	lookPathFn                = exec.LookPath
	defaultUserRuntimeDirFn   = func() string { return fmt.Sprintf("/run/user/%d", os.Getuid()) }
	readProcCgroupMemoryMaxFn = readProcCgroupMemoryMax
)

// Finite per worker cgroup limit that never widens host risk
func workerMemoryMaxBytes() int64 {
	var overrideBound int64
	override := strings.TrimSpace(os.Getenv("TERMINAL_LOCAL_MEMORY_MAX_MB"))
	if override != "" {
		parsed := int64(-1)
		if n, err := strconv.ParseInt(override, 10, 64); err == nil {
			parsed = n * 1024 * 1024
		}
		if parsed >= minWorkerMemoryMaxBytes {
			overrideBound = parsed
		} else {
			toolsLogger(nil).Warn(fmt.Sprintf("Ignoring invalid TERMINAL_LOCAL_MEMORY_MAX_MB=%q; expected an integer representing at least %d MiB", override, minWorkerMemoryMaxBytes/(1024*1024)))
		}
	}
	candidates := []int64{}
	if limit, ok := readProcCgroupMemoryMaxFn(); ok && limit >= minWorkerMemoryMaxBytes {
		candidates = append(candidates, limit)
	}
	if physical, ok := physicalMemoryBytes(); ok {
		bound := min(workerMemoryMaxCapBytes, max(minWorkerMemoryMaxBytes, physical/2))
		candidates = append(candidates, bound)
	}
	safe := defaultWorkerMemoryMax
	if len(candidates) > 0 {
		safe = candidates[0]
		for _, c := range candidates[1:] {
			safe = min(safe, c)
		}
	}
	if overrideBound > 0 {
		return min(overrideBound, safe)
	}
	return safe
}

// memory.max of this process's cgroup v2 slice, false when unreadable or unlimited
func readProcCgroupMemoryMax() (int64, bool) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		relative := strings.TrimLeft(strings.TrimPrefix(line, "0::"), "/")
		raw, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", relative, "memory.max"))
		if err != nil {
			return 0, false
		}
		text := strings.TrimSpace(strings.TrimPrefix(string(raw), "\xEF\xBB\xBF"))
		if n, err := strconv.ParseInt(text, 10, 64); err == nil && allDigits([]byte(text)) {
			return n, true
		}
		return 0, false
	}
	return 0, false
}

// systemd-run --user --scope argv shared by the probe and real spawns
func systemdScopeArgv(binary, unitName string, argv ...string) []string {
	out := []string{binary, "--user", "--scope", "--quiet"}
	if systemdRunNoExpand {
		out = append(out, "--expand-environment=no")
	}
	out = append(out, "--unit", unitName, "--collect",
		"--property", "MemoryAccounting=yes",
		"--property", fmt.Sprintf("MemoryMax=%d", workerMemoryMaxBytes()),
		"--")
	return append(out, argv...)
}

// SystemdUserBusEnv derives XDG_RUNTIME_DIR and the session bus address that reach this user's lingering systemd
func SystemdUserBusEnv(base map[string]string) map[string]string {
	env := map[string]string{}
	if base == nil {
		base = environMap()
	}
	for k, v := range base {
		env[k] = v
	}
	if !isLinuxHost {
		return env
	}
	var runtimeDir string
	if configured := env["XDG_RUNTIME_DIR"]; configured != "" && secureUserRuntimeDir(configured) {
		runtimeDir = configured
	} else {
		runtimeDir = defaultUserRuntimeDirFn()
		if !secureUserRuntimeDir(runtimeDir) {
			return env
		}
	}
	busPath := filepath.Join(runtimeDir, "bus")
	if !ownedUnixSocket(busPath) {
		return env
	}
	env["XDG_RUNTIME_DIR"] = runtimeDir
	env["DBUS_SESSION_BUS_ADDRESS"] = "unix:path=" + busPath
	return env
}

// Cached probe verdict, nil when a reprobe is due
func systemdScopeCached() *bool {
	if systemdScopeAvailable == nil {
		return nil
	}
	if time.Since(systemdScopeProbedAt) >= systemdScopeProbeTTL {
		return nil
	}
	return systemdScopeAvailable
}

// Reports whether systemd-run --user --scope can create a cgroup, probed and cached for a bounded TTL
func systemdRunUserScopeAvailable() bool {
	if v := systemdScopeCached(); v != nil {
		return *v
	}
	systemdScopeMu.Lock()
	defer systemdScopeMu.Unlock()
	if v := systemdScopeCached(); v != nil {
		return *v
	}
	available := false
	if isLinuxHost {
		if binary, err := lookPathFn("systemd-run"); err == nil && binary != "" {
			probeUnit := fmt.Sprintf("icebath-probe-scope-%d-%s", os.Getpid(), randomHex(4))
			var code int
			var stderr string
			for attempt := 0; attempt < 2; attempt++ {
				code, stderr = runSystemdProbe(systemdScopeArgv(binary, probeUnit, "/bin/sh", "-c", "exit 0"))
				if !(code != 0 && systemdRunNoExpand && strings.Contains(stderr, "expand-environment")) {
					break
				}
				systemdRunNoExpand = false
			}
			available = code == 0
			if !available {
				toolsLogger(nil).Debug(fmt.Sprintf("systemd-run --user --scope probe failed (rc=%d): %s", code, strings.TrimSpace(stderr)))
			}
		}
	}
	systemdScopeAvailable = &available
	systemdScopeProbedAt = time.Now()
	return available
}

// Runs the scope probe with a 3s bound and returns its exit code and stderr
var runSystemdProbe = func(argv []string) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = envSlice(SystemdUserBusEnv(nil))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdin = nil
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), stderr.String()
		}
		return -1, err.Error()
	}
	return 0, stderr.String()
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Reports whether this process is the live supervised icebath gateway itself
func isSupervisedGatewayProcess() bool {
	if os.Getenv("_ICEBATH_GATEWAY") != "1" {
		return false
	}
	if SupervisedGatewayPid == nil {
		toolsLogger(nil).Debug("Could not verify supervised gateway process identity: no gateway installed SupervisedGatewayPid")
		return false
	}
	supervised, pid := SupervisedGatewayPid()
	return supervised && pid == os.Getpid()
}

// Reports whether this process runs as the supervised gateway
func IsSupervisedGatewayProcess() bool { return isSupervisedGatewayProcess() }

// Wraps shellArgv in a transient systemd scope with its own memory accounting
func buildSystemdScopeArgv(shellArgv []string, unitSuffix string) []string {
	binary, err := lookPathFn("systemd-run")
	if err != nil || binary == "" {
		return shellArgv
	}
	return systemdScopeArgv(binary, "icebath-worker-"+unitSuffix, shellArgv...)
}

// RestartSafeScopeUnavailable reports a required restart safe scope that could not be created
type RestartSafeScopeUnavailable struct{ Detail string }

func (e *RestartSafeScopeUnavailable) Error() string {
	return "cannot create restart-safe systemd scope for gateway child: " + e.Detail
}

// Warns once per process about a host level scope degradation
func warnScopeDegradedOnce(detail, consequence string) {
	systemdScopeMu.Lock()
	already := scopeDegradedWarned
	scopeDegradedWarned = true
	systemdScopeMu.Unlock()
	if already {
		return
	}
	toolsLogger(nil).Warn(detail + "; " + consequence)
}

const cronDegradedConsequence = "cron children are dispatched as direct external subprocesses without restart-safe " +
	"cgroup isolation (killed if the gateway restarts mid-job). Set " +
	"cron.require_restart_safe_scope=true in config.yaml to fail closed instead."

const unitDegradedConsequence = "workers are spawned unmanaged inside this systemd unit's cgroup and will be KILLED when the " +
	"unit exits (Type=oneshot dispatch timers lose every worker within a second). Give the unit's " +
	"user a session bus (`loginctl enable-linger <user>`) so workers get their own scope, or set " +
	"KillMode=process on the unit."

// GatewayChildDispatch says how a managed gateway child is launched
type GatewayChildDispatch struct {
	Mode string
	Argv []string
}

// ScopedSpawnLostUserBus reports a vanished user bus after a scope wrapper exited early and forces a reprobe
func ScopedSpawnLostUserBus(spawnEnv map[string]string) bool {
	base := map[string]string{}
	for k, v := range spawnEnv {
		base[k] = v
	}
	delete(base, "DBUS_SESSION_BUS_ADDRESS")
	if _, ok := SystemdUserBusEnv(base)["DBUS_SESSION_BUS_ADDRESS"]; ok {
		return false
	}
	systemdScopeMu.Lock()
	f := false
	systemdScopeAvailable = &f
	systemdScopeProbedAt = time.Now()
	systemdScopeMu.Unlock()
	return true
}

// RestartSafeGatewayChildArgv places a managed systemd gateway child outside the gateway cgroup
func RestartSafeGatewayChildArgv(command []string, unitSuffix string, requireRestartSafeScope, outlivesParent bool) (GatewayChildDispatch, error) {
	if !isLinuxHost || os.Getenv("INVOCATION_ID") == "" {
		return GatewayChildDispatch{"in_process", command}, nil
	}
	supervisedGateway := isSupervisedGatewayProcess()
	if !supervisedGateway && !outlivesParent {
		return GatewayChildDispatch{"in_process", command}, nil
	}
	degrade := func(detail string) (GatewayChildDispatch, error) {
		if supervisedGateway {
			if requireRestartSafeScope {
				return GatewayChildDispatch{}, &RestartSafeScopeUnavailable{Detail: detail}
			}
			warnScopeDegradedOnce("managed gateway: "+detail, cronDegradedConsequence)
		} else {
			warnScopeDegradedOnce("systemd unit dispatch: "+detail, unitDegradedConsequence)
		}
		return GatewayChildDispatch{"degraded", command}, nil
	}
	if !systemdRunUserScopeAvailable() {
		return degrade(fmt.Sprintf("systemd-run --user --scope is unavailable (usually no reachable user D-Bus session at "+
			"/run/user/%d/bus). On a system-level service install, run "+
			"`sudo loginctl enable-linger <gateway-user>` and restart the gateway.", os.Getuid()))
	}
	scoped := buildSystemdScopeArgv(command, unitSuffix)
	if strings.Join(scoped, "\x00") == strings.Join(command, "\x00") {
		return degrade("systemd-run disappeared after the availability probe")
	}
	return GatewayChildDispatch{"scoped", scoped}, nil
}

// Stops a transient systemd user scope by unit name, true when stopped or already gone
var stopSystemdUnit = func(unitName string) bool {
	binary, err := lookPathFn("systemctl")
	if err != nil || binary == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--user", "stop", unitName)
	cmd.Env = envSlice(SystemdUserBusEnv(nil))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			text := strings.TrimSpace(stderr.String())
			lower := strings.ToLower(text)
			for _, marker := range []string{"not loaded", "not found", "does not exist"} {
				if strings.Contains(lower, marker) {
					return true
				}
			}
			toolsLogger(nil).Debug(fmt.Sprintf("systemctl --user stop %s exited %d: %s", unitName, exitErr.ExitCode(), text))
			return false
		}
		toolsLogger(nil).Debug(fmt.Sprintf("systemctl --user stop %s failed: %v", unitName, err))
		return false
	}
	return true
}

// FormatUptimeShort renders seconds as 45s, 3m 2s or 2h 5m
func FormatUptimeShort(seconds int) string {
	s := max(0, seconds)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	mins, secs := s/60, s%60
	if mins < 60 {
		return fmt.Sprintf("%dm %ds", mins, secs)
	}
	return fmt.Sprintf("%dh %dm", mins/60, mins%60)
}

// Shell command that reads only the log bytes written since offset, cut at a UTF-8 boundary
func logDeltaCommand(quotedLogPath string, offset int) string {
	q := quotedLogPath
	return fmt.Sprintf("O=%d; ", offset) +
		fmt.Sprintf("S=$({ wc -c < %s; } 2>/dev/null | tr -dc '0-9'); ", q) +
		"S=${S:-0}; " +
		`if [ "$S" -lt "$O" ]; then O=0; fi; ` +
		`N=0; P=$S; while [ "$P" -gt "$O" ] && [ "$N" -lt 3 ]; do ` +
		fmt.Sprintf("B=$(tail -c +$P %s 2>/dev/null | head -c 1 | od -An -to1 | tr -dc '0-9'); ", q) +
		`case "$B" in 2[0-7][0-7]) P=$((P-1)); N=$((N+1));; *) break;; esac; done; ` +
		`if [ "$N" -gt 0 ] || [ "$P" -eq "$S" ]; then ` +
		fmt.Sprintf("B=$(tail -c +$P %s 2>/dev/null | head -c 1 | od -An -to1 | tr -dc '0-9'); ", q) +
		`case "$B" in 3[0-3][0-7]) L=2;; 3[4-5][0-7]) L=3;; 3[6-7][0-7]) L=4;; *) L=1;; esac; ` +
		`if [ "$L" -gt $((N+1)) ]; then S=$((P-1)); fi; fi; ` +
		`echo "$S $O"; ` +
		`if [ "$S" -gt "$O" ]; then ` +
		fmt.Sprintf("tail -c +$((O+1)) %s 2>/dev/null | head -c $((S-O)); fi", q)
}

var wslLauncherNames = setOf("wsl", "wsl.exe")

// Reports whether command routes through a wsl launcher chain whose host pid is the short lived launcher
func isWslLauncherCommand(command string) bool {
	if strings.TrimSpace(command) == "" {
		return false
	}
	candidates := []string{}
	if parts, err := shlexSplit(command); err == nil {
		first := ""
		if len(parts) > 0 {
			first = parts[0]
		}
		candidates = append(candidates, first)
	}
	if words := strings.Fields(command); len(words) > 0 {
		candidates = append(candidates, words[0])
	}
	for _, first := range candidates {
		base := strings.ToLower(strings.Trim(filepath.Base(strings.ReplaceAll(first, "\\", "/")), "'\""))
		if wslLauncherNames[base] {
			return true
		}
	}
	return false
}

// Output logs of worker spawned processes

const outputLogTailInterval = 200 * time.Millisecond

// After the process ended an orphaned grandchild may keep appending, stop draining past this
const outputLogFinalDrainMaxBytes = 1 << 20

func processOutputDir(env *Env) string {
	return filepath.Join(envHome(env), "logs", "process-output")
}

// The log a worker spawned process writes to, empty outside a registered worker
func workerOutputLog(env *Env, sessionID string) string {
	if IsWorkerProcess == nil || !IsWorkerProcess() {
		return ""
	}
	directory := processOutputDir(env)
	os.MkdirAll(directory, 0o700)
	cutoff := time.Now().Add(-ResultRetentionSeconds)
	exits, _ := filepath.Glob(filepath.Join(directory, "proc_*.exit"))
	for _, exited := range exits {
		st, err := os.Stat(exited)
		if err != nil {
			continue
		}
		if st.ModTime().Before(cutoff) {
			os.Remove(strings.TrimSuffix(exited, ".exit") + ".log")
			os.Remove(exited)
		}
	}
	return filepath.Join(directory, sessionID+".log")
}

// A checkpointed log path, only when it names this profile's output directory
func adoptableLog(env *Env, path any) string {
	text, _ := path.(string)
	if text == "" {
		return ""
	}
	if filepath.Clean(filepath.Dir(text)) == filepath.Clean(processOutputDir(env)) && filepath.Ext(text) == ".log" {
		return text
	}
	return ""
}

// Owner only append file for the child's stdout and stderr
func openOutputLog(log string) (*os.File, error) {
	return os.OpenFile(log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}

func exitPathOf(log string) string {
	return strings.TrimSuffix(log, filepath.Ext(log)) + ".exit"
}

// Prefixes command so the shell writes its exit status beside the log
func recordExitCommand(log, command string) string {
	target := shQuote(filepath.ToSlash(exitPathOf(log)))
	return "__icebath_exit_file=" + target + "; trap 'printf \"%s\\n\" \"$?\" > \"$__icebath_exit_file\"' EXIT\n" + command
}

// Exit status the shell recorded beside the log, false when none was written
func recordedExitCode(log string) (int, bool) {
	data, err := os.ReadFile(exitPathOf(log))
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(string(data), "\xEF\xBB\xBF")))
	if err != nil {
		return 0, false
	}
	return n, true
}

// Why a background process has no writable stdin

const stdinNonInteractive = "Pass input non-interactively instead (arguments, a here-doc, or `printf ... | cmd`)."

// Why a pty relaunch of this local pipe mode session would not help, empty when it would
func noPtyReason(s *ProcessSession) string {
	if s.PtyFallback != "" {
		return fmt.Sprintf("pty=true was requested but %s, so it runs without stdin", s.PtyFallback)
	}
	if s.OutputLog != "" {
		return "background processes here run in an isolated worker that cannot give them a pseudo-terminal"
	}
	if !ptyAvailable() {
		return "the PTY library is not available on this host"
	}
	return ""
}

// Error for an exited pipe mode process that had no stdin
func alreadyExitedError(s *ProcessSession) string {
	if s.Process == nil || s.Process.stdinDone != nil {
		return "Process has already finished"
	}
	if reason := noPtyReason(s); reason != "" {
		return fmt.Sprintf("Process has already finished. It ran without stdin (%s), so if it read stdin, it got end-of-file at once. %s", reason, stdinNonInteractive)
	}
	return "Process has already finished. It was started without stdin (terminal background=true without " +
		"pty=true), so if it read stdin, it got end-of-file at once. Relaunch it with " +
		"terminal(background=true, pty=true) to send input."
}

// Error for process_manage write or submit on a session without a stdin pipe or PTY
func stdinUnavailableError(s *ProcessSession) string {
	if s.Process == nil {
		return "Process stdin not available: it runs on a non-local terminal backend (or was recovered " +
			"after a restart), which has no live stdin. " + stdinNonInteractive
	}
	if reason := noPtyReason(s); reason != "" {
		return fmt.Sprintf("This process has no stdin: %s. Relaunching with pty=true would not help. %s", reason, stdinNonInteractive)
	}
	return "This process was started without stdin (terminal background=true without pty=true). " +
		"Relaunch it with terminal(background=true, pty=true) to send input."
}

// Process tree termination and post kill survivor verification

// Reports a host process that is running and not a zombie
func procAlive(pid int) bool {
	return pid > 0 && identityProber.Running(pid) && !identityProber.Zombie(pid)
}

// Terminates a host visible pid and its descendants after re validating its start time
func (r *ProcessRegistry) terminateHostPid(env *Env, pid int, expectedStart *int64) {
	if expectedStart != nil && !hostPidIsOurs(pid, expectedStart) {
		toolsLogger(nil).Warn(fmt.Sprintf("Refusing to terminate host pid %d: start-time mismatch — PID was recycled onto an unrelated process.", pid))
		return
	}
	if isWindowsHost {
		terminateHostPidWindows(pid)
		return
	}
	if !identityProber.Running(pid) {
		return
	}
	descendants := identityProber.Children(pid, true)
	identityProber.Terminate(pid)
	grace := r.daemonTermGraceSeconds(env)
	waitForExit := func(targets []int) {
		if grace <= 0 {
			return
		}
		deadline := time.Now().Add(time.Duration(grace * float64(time.Second)))
		for time.Now().Before(deadline) {
			anyAlive := false
			for _, p := range targets {
				if procAlive(p) {
					anyAlive = true
					break
				}
			}
			if !anyAlive {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitForExit([]int{pid})
	remaining := descendants
	if grace > 0 {
		remaining = []int{}
		for _, p := range descendants {
			if procAlive(p) {
				remaining = append(remaining, p)
			}
		}
	}
	for _, p := range remaining {
		identityProber.Terminate(p)
	}
	targets := append([]int{pid}, remaining...)
	if grace <= 0 {
		return
	}
	waitForExit(targets)
	if procAlive(pid) {
		known := map[int]bool{}
		for _, p := range targets {
			known[p] = true
		}
		for _, p := range identityProber.Children(pid, true) {
			if !known[p] {
				targets = append(targets, p)
			}
		}
	}
	for _, p := range targets {
		if procAlive(p) {
			identityProber.Kill(p)
			toolsLogger(nil).Info(fmt.Sprintf("Escalated to SIGKILL for pid %d (ignored SIGTERM within %.1fs grace)", p, grace))
		}
	}
}

// Terminates a host pid and its descendants after revalidating its start time, a nil env reading the process home config
func TerminateHostPid(env *Env, pid int, expectedStart *int64) {
	if env == nil {
		h := home.Process()
		env = &Env{Home: h}
		if tree, err := config.Load(h, config.NewSecrets(h).Lookup()); err == nil {
			env.Config = tree
		}
	}
	processRegistry.terminateHostPid(env, pid, expectedStart)
}

// Pids of living non zombie descendants of a host pid
func liveDescendants(pid int) []int {
	out := []int{}
	for _, c := range identityProber.Children(pid, true) {
		if procAlive(c) {
			out = append(out, c)
		}
	}
	return out
}

// Kill signals need a scheduling tick to land before survivors can be judged
const killSettleSeconds = 1.0

// Host pids still alive once the kill signals have had time to land, fail closed
func (r *ProcessRegistry) postKillSurvivors(s *ProcessSession) []int {
	deadline := time.Now().Add(time.Duration(killSettleSeconds * float64(time.Second)))
	for {
		survivors := r.probeSurvivors(s)
		if len(survivors) == 0 || !time.Now().Before(deadline) {
			return survivors
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (r *ProcessRegistry) probeSurvivors(s *ProcessSession) []int {
	survivors := []int{}
	if s.Process != nil {
		if _, done := s.Process.Poll(); !done {
			pid := s.Process.Pid()
			if pid == 0 {
				pid = s.Pid
			}
			survivors = append(survivors, pid)
		}
	}
	if s.pty != nil && s.pty.IsAlive() {
		survivors = append(survivors, s.Pid)
	}
	if s.PidScope == "host" && s.Pid != 0 {
		if r.detachedHostFate(s.Pid, s.HostStartTime) == "running" {
			if !containsInt(survivors, s.Pid) {
				survivors = append(survivors, s.Pid)
			}
			for _, pid := range liveDescendants(s.Pid) {
				if !containsInt(survivors, pid) {
					survivors = append(survivors, pid)
				}
			}
		}
	}
	out := survivors[:0]
	for _, pid := range survivors {
		if pid != 0 {
			out = append(out, pid)
		}
	}
	return out
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
