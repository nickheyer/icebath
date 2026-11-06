package gateway

import (
	"bytes"
	"context"
	"errors"
	"math"
	"math/big"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Gateway exit codes, 75 asks the supervisor for a restart and 78 is a fatal configuration error
const (
	GatewayServiceRestartExitCode = 75
	GatewayFatalConfigExitCode    = 78
)

// Supervisor markers and stop budget constants of the restart path
const (
	ExternalGatewaySupervisorEnv            = "ICEBATH_GATEWAY_EXTERNAL_SUPERVISOR"
	LaunchdLabelEnv                         = "ICEBATH_LAUNCHD_LABEL"
	DefaultGatewayPostInterruptGraceTimeout = 5.0
	CronDrainCleanupReserveS                = 10.0
	SystemdStopHeadroomS                    = 30.0
	SystemdTimeoutStopSecFloor              = 60.0
	LaunchdGUIExitTimeoutClampS             = 60
	LaunchdStopCleanupReserveS              = 10.0
	LaunchdWatchdogDumpMarginS              = 2.0
	launchdJobLabelPrefix                   = "ai.icebath"
)

// Shipped defaults of agent.restart_drain_timeout, gateway.signal_interrupt_grace_timeout and the after turn and cron drains
var (
	DefaultGatewayRestartDrainTimeout         = defaultConfigFloat("agent", "restart_drain_timeout")
	DefaultGatewaySignalInterruptGraceTimeout = defaultConfigFloat("gateway", "signal_interrupt_grace_timeout")
	DefaultGatewayRestartAfterTurnTimeout     = defaultConfigFloat("agent", "restart_after_turn_timeout")
	DefaultGatewayCronDrainTimeout            = defaultConfigFloat("agent", "cron_drain_timeout")
)

var launchdExitTimeoutRE = regexp.MustCompile(`(?m)^[` + pySpaceClass + `]*exit timeout[` + pySpaceClass + `]*=[` + pySpaceClass + `]*(\p{Nd}+)[` + pySpaceClass + `]*$`)

var restartTruthy = map[string]bool{"1": true, "true": true, "yes": true, "on": true}

// float() of a shipped default config value, panicking when the defaults table lacks it as Python raises at import
func defaultConfigFloat(section, key string) float64 {
	var block map[string]any
	switch b := config.Defaults()[section].(type) {
	case config.Tree:
		block = b
	case map[string]any:
		block = b
	}
	v, err := pyint.Float(block[key])
	if err != nil {
		panic("gateway/restart.py DEFAULT_CONFIG[" + section + "][" + key + "]: " + err.Error())
	}
	return v
}

// Python max of two floats, the first wins unless the second compares greater
func pyMaxFloat(a, b float64) float64 {
	if b > a {
		return b
	}
	return a
}

// Python min of two floats, the first wins unless the second compares smaller
func pyMinFloat(a, b float64) float64 {
	if b < a {
		return b
	}
	return a
}

// Exit 78 becomes 0 so launchd parks the job, every other code passes through
func MapFatalConfigExitForLaunchd(returncode int) int {
	if returncode == GatewayFatalConfigExitCode {
		return 0
	}
	return returncode
}

// Seconds of the exit timeout line in launchctl print output, nil when absent
func ParseLaunchdExitTimeout(printOutput any) *float64 {
	text := ""
	if pyrepr.Truthy(printOutput) {
		text = pyrepr.Str(printOutput)
	}
	m := launchdExitTimeoutRE.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	v, err := pyint.ParseFloat(m[1])
	if err != nil {
		return nil
	}
	return &v
}

// Environment reader over the given mapping, the process environment when nil
func environGetter(environ map[string]string) func(string) string {
	if environ == nil {
		return os.Getenv
	}
	return func(k string) string { return environ[k] }
}

// The ai.icebath launchd job label from XPC_SERVICE_NAME or the wrapper re-export, empty when neither names one
func LaunchdJobLabel(environ map[string]string) string {
	get := environGetter(environ)
	for _, name := range []string{"XPC_SERVICE_NAME", LaunchdLabelEnv} {
		if label := pystr.Strip(get(name)); strings.HasPrefix(label, launchdJobLabelPrefix) {
			return label
		}
	}
	return ""
}

// The launchd job label of this process on darwin, empty on every other platform
func LaunchdServiceLabel(environ map[string]string, platform string) string {
	if platform != "darwin" {
		return ""
	}
	return LaunchdJobLabel(environ)
}

// Inputs of ReadLaunchdExitTimeoutS, zero values select the live process defaults
type LaunchdExitTimeoutQuery struct {
	Label    string
	Environ  map[string]string
	UID      *int
	Run      func(argv []string, timeout time.Duration) (stdout string, returncode int, err error)
	Platform string
}

// Runs a command with captured UTF-8 stdout, decoding invalid bytes as replacement characters
func runCaptured(argv []string, timeout time.Duration) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &bytes.Buffer{}
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", 0, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return strings.ToValidUTF8(out.String(), "�"), exitErr.ExitCode(), nil
	}
	if err != nil {
		return "", 0, err
	}
	return strings.ToValidUTF8(out.String(), "�"), 0, nil
}

// Live launchd ExitTimeOut seconds of this gateway job, nil when no launchd budget applies
func ReadLaunchdExitTimeoutS(q LaunchdExitTimeoutQuery) *float64 {
	platform := q.Platform
	if platform == "" {
		platform = runtime.GOOS
	}
	label := q.Label
	if label == "" {
		label = LaunchdServiceLabel(q.Environ, platform)
	}
	if label == "" {
		return nil
	}
	var uid int
	if q.UID != nil {
		uid = *q.UID
	} else {
		if runtime.GOOS == "windows" {
			return nil
		}
		uid = os.Getuid()
	}
	domain := "gui/" + strconv.Itoa(uid)
	if uid == 0 {
		domain = "system"
	}
	run := q.Run
	if run == nil {
		run = runCaptured
	}
	stdout, code, err := run([]string{"launchctl", "print", domain + "/" + label}, 5*time.Second)
	if err != nil || code != 0 {
		return nil
	}
	return ParseLaunchdExitTimeout(stdout)
}

// Stop drain clamped to the launchd exit timeout minus the cleanup reserve, never extended
func ResolveLaunchdCappedDrain(drainTimeout, launchdExitTimeoutS, cleanupReserveS any) float64 {
	drain := restartSeconds(drainTimeout, 0)
	budget := restartSeconds(launchdExitTimeoutS, 0)
	if budget <= 0.0 {
		return drain
	}
	return pyMinFloat(drain, pyMaxFloat(budget-restartSeconds(cleanupReserveS, 0), 0.0))
}

// Stop timing attributes a runner carries, a nil drain is the shipped default
type StopTiming struct {
	RestartDrainTimeout   *float64
	StopRequestedBySignal bool
	LaunchdExitTimeoutS   *float64
}

func optionalFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// Drain budget of the stop in progress, launchd capped only for a signal driven stop
func EffectiveStopDrainTimeout(runner StopTiming) float64 {
	drain := DefaultGatewayRestartDrainTimeout
	if runner.RestartDrainTimeout != nil {
		drain = *runner.RestartDrainTimeout
	}
	if !runner.StopRequestedBySignal {
		return drain
	}
	return ResolveLaunchdCappedDrain(drain, optionalFloat(runner.LaunchdExitTimeoutS), LaunchdStopCleanupReserveS)
}

// Watchdog leash of the stop in progress, clamped below the launchd exit timeout for a signal driven stop
func EffectiveStopWatchdogDelay(runner StopTiming, watchdogDelay any) float64 {
	leash := restartSeconds(watchdogDelay, 0)
	if !runner.StopRequestedBySignal {
		return leash
	}
	return ResolveLaunchdCappedDrain(leash, optionalFloat(runner.LaunchdExitTimeoutS), LaunchdWatchdogDumpMarginS)
}

// True for a lock_conflict code or a scoped lock code ending in _lock
func IsGlobalStartupConflict(errorCode string) bool {
	code := pystr.Lower(pystr.Strip(errorCode))
	return code != "" && (code == "lock_conflict" || strings.HasSuffix(code, "_lock"))
}

// True when a restarting supervisor owns this process, systemd, launchd, s6 or the explicit opt in
func IsGatewaySupervisorProcess(environ map[string]string) bool {
	get := environGetter(environ)
	xpc := get("XPC_SERVICE_NAME")
	return get("INVOCATION_ID") != "" || get("ICEBATH_S6_SUPERVISED_CHILD") != "" || (xpc != "" && xpc != "0") ||
		LaunchdJobLabel(environ) != "" || restartTruthy[pystr.Lower(pystr.Strip(get(ExternalGatewaySupervisorEnv)))]
}

// True when a generated service or launcher started this gateway rather than a shell
func IsSupervisedGatewayLaunch(environ map[string]string) bool {
	if environGetter(environ)("ICEBATH_SUPERVISED_CHILD") != "" {
		return true
	}
	return IsGatewaySupervisorProcess(environ)
}

// True inside a Docker or Podman container, where only the exit 75 restart survives
var IsContainerRestartContext = func() bool {
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// Non negative float of the value, the fallback when float() rejects it
func restartSeconds(value any, fallback float64) float64 {
	f, err := pyint.Float(value)
	if err != nil {
		return fallback
	}
	return pyMaxFloat(f, 0.0)
}

// Timeout where 0 disables, None blank or non numeric input and optionally non finite input take the default
func parseTimeoutKeepingZero(raw any, def float64, finite bool) float64 {
	if s, ok := raw.(string); raw == nil || ok && pystr.Strip(s) == "" {
		return def
	}
	value, err := pyint.Float(raw)
	if err != nil {
		return def
	}
	if finite && (math.IsInf(value, 0) || math.IsNaN(value)) {
		return def
	}
	return pyMaxFloat(0.0, value)
}

// Configured restart drain timeout, a falsy value including 0 takes the shipped default
func ParseRestartDrainTimeout(raw any) float64 {
	if !pyrepr.Truthy(raw) {
		raw = nil
	}
	return parseTimeoutKeepingZero(raw, DefaultGatewayRestartDrainTimeout, false)
}

// After turn wait cap of an in band restart, 0 keeps the immediate drain
func ParseRestartAfterTurnTimeout(raw any) float64 {
	return parseTimeoutKeepingZero(raw, DefaultGatewayRestartAfterTurnTimeout, false)
}

// Cron and api_server drain floor, 0 opts out
func ParseCronDrainTimeout(raw any) float64 {
	return parseTimeoutKeepingZero(raw, DefaultGatewayCronDrainTimeout, false)
}

// Post interrupt grace of an unexpected signal, non finite input takes the default
func ParseSignalInterruptGraceTimeout(raw any) float64 {
	return parseTimeoutKeepingZero(raw, DefaultGatewaySignalInterruptGraceTimeout, true)
}

// Seconds the stop drain may wait on cron and api_server work, bounded by the watchdog leash and never below the drain
func ResolveCronDrainBudget(drainTimeout, cronDrainTimeout, watchdogDelay, elapsed, cleanupReserveS any) float64 {
	drain := restartSeconds(drainTimeout, 0)
	floor := restartSeconds(cronDrainTimeout, 0)
	if floor <= 0.0 {
		return drain
	}
	ceiling := restartSeconds(watchdogDelay, 0) - restartSeconds(elapsed, 0) - restartSeconds(cleanupReserveS, CronDrainCleanupReserveS)
	return pyMaxFloat(drain, pyMinFloat(floor, ceiling))
}

// Seconds systemd TimeoutStopSec must cover, int() of the float so infinity and NaN raise as Python does
func ResolveSystemdTimeoutStopSec(drainTimeout, cronDrainTimeout, cleanupReserveS, headroomS, floorS any) (*big.Int, error) {
	drain := restartSeconds(drainTimeout, 0)
	cron := restartSeconds(cronDrainTimeout, 0)
	cronBudget := 0.0
	if cron > 0.0 {
		cronBudget = cron + restartSeconds(cleanupReserveS, 0)
	}
	v := pyMaxFloat(restartSeconds(floorS, 0), pyMaxFloat(drain, cronBudget)+restartSeconds(headroomS, 0))
	if math.IsInf(v, 0) {
		return nil, &PyError{"OverflowError", "cannot convert float infinity to integer"}
	}
	if math.IsNaN(v) {
		return nil, &PyError{"ValueError", "cannot convert float NaN to integer"}
	}
	n, _ := big.NewFloat(math.Trunc(v)).Int(nil)
	return n, nil
}

// TimeoutStopSec with the shipped cron drain, reserve, headroom and floor
func DefaultSystemdTimeoutStopSec(drainTimeout any) (*big.Int, error) {
	return ResolveSystemdTimeoutStopSec(drainTimeout, DefaultGatewayCronDrainTimeout, CronDrainCleanupReserveS, SystemdStopHeadroomS, SystemdTimeoutStopSecFloor)
}

// Seconds a CLI waits for the gateway to exit after SIGUSR1, infinite for a non finite drain
func ResolveRestartExitWaitBudget(drainTimeout, afterTurnTimeout, cronDrainTimeout, headroom any) (float64, error) {
	for _, v := range []any{drainTimeout, cronDrainTimeout} {
		s := restartSeconds(v, 0)
		if math.IsInf(s, 0) || math.IsNaN(s) {
			return math.Inf(1), nil
		}
	}
	stop, err := ResolveSystemdTimeoutStopSec(drainTimeout, cronDrainTimeout, CronDrainCleanupReserveS, SystemdStopHeadroomS, SystemdTimeoutStopSecFloor)
	if err != nil {
		return 0, err
	}
	stopF, _ := new(big.Float).SetInt(stop).Float64()
	return restartSeconds(afterTurnTimeout, 0) + stopF + restartSeconds(headroom, 0), nil
}
