package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	DefaultStartupWatchdogTimeoutS  = 300.0
	startupWatchdogMinTimeoutS      = 30.0
	StartupWatchdogRestartExitCode  = 75
	EnvStartupWatchdog              = "ICEBATH_STARTUP_WATCHDOG"
	EnvStartupWatchdogTimeoutS      = "ICEBATH_STARTUP_WATCHDOG_TIMEOUT_S"
	startupWatchdogPollSliceS       = 5.0
	startupWatchdogCPUProgressMinS  = 1.0
	startupWatchdogMaxLeaseS        = 900.0
	startupWatchdogLedgerJoinS      = 5.0
	startupWatchdogFireExitBoundS   = 10.0
	startupWatchdogArmed            = "armed"
	startupWatchdogDisarmed         = "disarmed"
	startupWatchdogFiring           = "firing"
	startupWatchdogDefaultMaxCPUExt = 3
)

// Probe, exit, sleep and forensic steps one armed watchdog runs with, read once when it is built
type startupWatchdogSeams struct {
	cpuSeconds       func() (float64, bool)
	exit             func(code int)
	sleep            func(seconds float64)
	dumpTraceback    func(file *os.File) error
	writeDumpRecord  func(record *pyjson.Object)
	markExited       func(ctx context.Context, exitCode *int, reason string, h string)
	maxCPUExtensions int
}

var (
	startupWatchdogDefaults = startupWatchdogSeams{
		cpuSeconds:       processCPUSeconds,
		exit:             func(code int) { os.Exit(code) },
		sleep:            func(seconds float64) { time.Sleep(secondsDuration(seconds)) },
		dumpTraceback:    dumpStartupWatchdogTraceback,
		writeDumpRecord:  writeStartupWatchdogDumpRecord,
		markExited:       MarkExited,
		maxCPUExtensions: startupWatchdogDefaultMaxCPUExt,
	}
	startupWatchdogDumpPathFn = func() string { return GetStartupWatchdogDumpPath("") }
	startupWatchdogHandleMu   sync.Mutex
	startupWatchdogHandle     *StartupWatchdogHandle
)

func startupWatchdogLog() *slog.Logger {
	return slog.Default().With("logger", "hermes_startup_watchdog")
}

// Path of logs/gateway-startup-watchdog.log under h, the process home when h is empty
func GetStartupWatchdogDumpPath(h string) string {
	if h == "" {
		h = home.Process()
	}
	return filepath.Join(h, "logs", "gateway-startup-watchdog.log")
}

// True when the watchdog environment switch opts out with 0, false, no or off
func StartupWatchdogDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvStartupWatchdog))) {
	case "0", "false", "no", "off":
		return true
	}
	return false
}

// Deadline seconds from the environment, floor clamped, the default for unset, garbage and non positive values
func ResolveStartupWatchdogTimeout() float64 {
	raw := strings.TrimSpace(os.Getenv(EnvStartupWatchdogTimeoutS))
	if raw == "" {
		return DefaultStartupWatchdogTimeoutS
	}
	value, err := pyint.ParseFloat(raw)
	if err != nil {
		startupWatchdogLog().Warn(fmt.Sprintf("Ignoring non-numeric %s=%s; using default %.0fs",
			EnvStartupWatchdogTimeoutS, pyrepr.Repr(raw), DefaultStartupWatchdogTimeoutS))
		return DefaultStartupWatchdogTimeoutS
	}
	if value <= 0 {
		return DefaultStartupWatchdogTimeoutS
	}
	return max(value, startupWatchdogMinTimeoutS)
}

// Appends to the dump file through write, a failure only logged at debug
func appendStartupWatchdogDump(write func(f *os.File) error, failureMsg string) {
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		path := startupWatchdogDumpPathFn()
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			return err
		}
		defer f.Close()
		return write(f)
	}()
	if err != nil {
		startupWatchdogLog().Debug(failureMsg, "exc_info", err)
	}
}

// Appends one JSON metadata line beside the stack dump
func writeStartupWatchdogDumpRecord(record *pyjson.Object) {
	appendStartupWatchdogDump(func(f *os.File) error {
		text, err := pyjson.Dumps(record, pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
		if err != nil {
			return err
		}
		_, err = f.WriteString(text + "\n")
		return err
	}, "Failed to write startup watchdog dump record")
}

// Writes every goroutine stack to file, to stderr when file is nil
func dumpStartupWatchdogTraceback(file *os.File) error {
	if file == nil {
		file = os.Stderr
	}
	_, err := file.Write(append(allGoroutineStacks(), '\n'))
	return err
}

// Logs at level and swallows a panicking handler
func startupWatchdogLogQuietly(level slog.Level, msg string) {
	defer func() { _ = recover() }()
	startupWatchdogLog().Log(context.Background(), level, msg)
}

// Records the watchdog exit in the lifecycle sentinel and swallows any failure
func (h *StartupWatchdogHandle) markLifecycleExit() {
	defer func() { _ = recover() }()
	code := h.ExitCode
	h.seams.markExited(context.Background(), &code, "startup_liveness_watchdog", "")
}

// Disarm and inspect handle of the armed startup watchdog goroutine
type StartupWatchdogHandle struct {
	TimeoutS float64
	ExitCode int

	armedAt    time.Time
	mu         sync.Mutex
	state      string
	deadline   time.Time
	disarmedCh chan struct{}
	disarmOnce sync.Once
	done       chan struct{}
	started    bool
	extensions int
	leaseUntil time.Time
	leasePhase any
	leaseCount int
	fireDone   chan struct{}
	seams      startupWatchdogSeams
}

func newStartupWatchdogHandle(timeoutS float64, exitCode int) *StartupWatchdogHandle {
	now := time.Now()
	return &StartupWatchdogHandle{TimeoutS: timeoutS, ExitCode: exitCode, armedAt: now, state: startupWatchdogArmed,
		deadline: now.Add(secondsDuration(timeoutS)), disarmedCh: make(chan struct{}), done: make(chan struct{}),
		fireDone: make(chan struct{}), seams: startupWatchdogDefaults}
}

// Stands the watchdog down unless the fire transition already claimed the state
func (h *StartupWatchdogHandle) Disarm() {
	h.mu.Lock()
	if h.state == startupWatchdogArmed {
		h.state = startupWatchdogDisarmed
	}
	h.mu.Unlock()
	h.disarmOnce.Do(func() { close(h.disarmedCh) })
}

// Pushes the deadline out to now plus the timeout plus extra seconds, garbage counting as zero
func (h *StartupWatchdogHandle) Kick(extraS any) {
	extra, err := pyint.Float(extraS)
	if err != nil || !(extra > 0) {
		extra = 0
	}
	h.mu.Lock()
	h.deadline = time.Now().Add(secondsDuration(h.TimeoutS + extra))
	h.mu.Unlock()
}

// Claims a progress lease of up to expected seconds for a phase, clamped per call
func (h *StartupWatchdogHandle) Lease(expectedS any, phase string) {
	expected, err := pyint.Float(expectedS)
	if err != nil || !(expected > 0) {
		return
	}
	expected = min(expected, startupWatchdogMaxLeaseS)
	h.mu.Lock()
	defer h.mu.Unlock()
	if until := time.Now().Add(secondsDuration(expected)); until.After(h.leaseUntil) {
		h.leaseUntil = until
	}
	if phase != "" {
		h.leasePhase = phase
	}
	h.leaseCount++
}

func (h *StartupWatchdogHandle) Disarmed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state == startupWatchdogDisarmed
}

// True while the watchdog goroutine is still running
func (h *StartupWatchdogHandle) IsAlive() bool {
	if !h.started {
		return false
	}
	select {
	case <-h.done:
		return false
	default:
		return true
	}
}

// Waits for the watchdog goroutine to finish, at most timeout when it is positive
func (h *StartupWatchdogHandle) Join(timeout time.Duration) {
	if !h.started {
		return
	}
	if timeout <= 0 {
		<-h.done
		return
	}
	select {
	case <-h.done:
	case <-time.After(timeout):
	}
}

func (h *StartupWatchdogHandle) cpuExtensions() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.extensions
}

// Runs the forensics then exits, an escort goroutine bounding the whole path
func (h *StartupWatchdogHandle) fire() {
	go h.exitEscort()
	elapsed := time.Since(h.armedAt).Seconds()
	h.mu.Lock()
	extensions, leaseCount, leasePhase := h.extensions, h.leaseCount, h.leasePhase
	h.mu.Unlock()
	startupWatchdogLogQuietly(logLevelCritical, fmt.Sprintf("Gateway startup did not reach a live event loop within %.0fs "+
		"(elapsed %.0fs, %d extension(s)), holds no progress lease "+
		"and shows no CPU progress; dumping all thread stacks and "+
		"exiting with code %d so the service supervisor can restart "+
		"it (OOF-298).", h.TimeoutS, elapsed, extensions, h.ExitCode))
	h.seams.writeDumpRecord(pyjson.ObjectOf(
		"ts", startupWatchdogISONow(),
		"tag", "startup_watchdog.fired",
		"pid", os.Getpid(),
		"timeout_s", h.TimeoutS,
		"elapsed_s", pyint.Round(elapsed, 3),
		"extensions", extensions,
		"lease_count", leaseCount,
		"last_lease_phase", leasePhase,
		"exit_code", h.ExitCode,
	))
	appendStartupWatchdogDump(h.seams.dumpTraceback, "Startup watchdog file-based faulthandler dump failed")
	if err := h.seams.dumpTraceback(nil); err != nil {
		startupWatchdogLog().Debug("Startup watchdog faulthandler dump failed", "exc_info", err)
	}
	ledgerDone := make(chan struct{})
	go func() {
		defer close(ledgerDone)
		h.markLifecycleExit()
	}()
	select {
	case <-ledgerDone:
	case <-time.After(secondsDuration(startupWatchdogLedgerJoinS)):
	}
	close(h.fireDone)
	h.seams.exit(h.ExitCode)
}

// Hard exits when the forensic path has not finished inside its bound
func (h *StartupWatchdogHandle) exitEscort() {
	h.seams.sleep(startupWatchdogFireExitBoundS)
	select {
	case <-h.fireDone:
		return
	default:
	}
	h.seams.exit(h.ExitCode)
}

// Sets a new deadline, false once the handle is no longer armed
func (h *StartupWatchdogHandle) extendIfArmed(deadline time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state != startupWatchdogArmed {
		return false
	}
	h.deadline = deadline
	return true
}

// Waits out the deadline, honoring a phase lease first and capped CPU progress second, then fires
func (h *StartupWatchdogHandle) run() {
	defer close(h.done)
	lastCPU, lastOK := h.seams.cpuSeconds()
	for {
		h.mu.Lock()
		if h.state != startupWatchdogArmed {
			h.mu.Unlock()
			return
		}
		deadline := h.deadline
		h.mu.Unlock()
		if remaining := time.Until(deadline); remaining > 0 {
			timer := time.NewTimer(min(remaining, secondsDuration(startupWatchdogPollSliceS)))
			select {
			case <-h.disarmedCh:
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		now := time.Now()
		h.mu.Lock()
		leaseUntil, leasePhase := h.leaseUntil, h.leasePhase
		h.mu.Unlock()
		if leaseUntil.After(now) {
			target := now.Add(secondsDuration(min(startupWatchdogPollSliceS, h.TimeoutS)))
			if leaseUntil.After(target) {
				target = leaseUntil
			}
			if !h.extendIfArmed(target) {
				return
			}
			phase := leasePhase
			if phase == nil || phase == "" {
				phase = "unknown"
			}
			startupWatchdogLogQuietly(slog.LevelWarn, fmt.Sprintf("Gateway startup exceeded %.0fs but phase %s holds a "+
				"progress lease for another %.0fs — honoring it.", h.TimeoutS, pyrepr.Repr(phase), leaseUntil.Sub(now).Seconds()))
			lastCPU, lastOK = h.seams.cpuSeconds()
			continue
		}
		cpu, ok := h.seams.cpuSeconds()
		if delta := cpu - lastCPU; ok && lastOK && delta >= startupWatchdogCPUProgressMinS && h.cpuExtensions() < h.seams.maxCPUExtensions {
			lastCPU, lastOK = cpu, ok
			h.mu.Lock()
			h.extensions++
			extensions := h.extensions
			h.mu.Unlock()
			if !h.extendIfArmed(time.Now().Add(secondsDuration(h.TimeoutS))) {
				return
			}
			startupWatchdogLogQuietly(slog.LevelWarn, fmt.Sprintf("Gateway startup exceeded %.0fs but is consuming CPU "+
				"(%.1fs this window); extending the startup watchdog "+
				"deadline (CPU-fallback extension %d of %d — phases "+
				"doing long legitimate work should call "+
				"report_startup_progress instead).", h.TimeoutS, delta, extensions, h.seams.maxCPUExtensions))
			continue
		}
		h.mu.Lock()
		if h.state != startupWatchdogArmed {
			h.mu.Unlock()
			return
		}
		h.state = startupWatchdogFiring
		h.mu.Unlock()
		h.fire()
		return
	}
}

func (h *StartupWatchdogHandle) start() bool {
	h.started = true
	go h.run()
	return true
}

// UTC wall clock in the datetime isoformat shape, microseconds dropped when zero
func startupWatchdogISONow() string {
	now := time.Now().UTC()
	if now.Nanosecond()/1000 == 0 {
		return now.Format("2006-01-02T15:04:05+00:00")
	}
	return now.Format("2006-01-02T15:04:05.000000+00:00")
}

// Arms the process wide startup watchdog with the environment timeout and the service restart exit code
func ArmStartupWatchdog(ctx context.Context) {
	armStartupWatchdog(nil, StartupWatchdogRestartExitCode)
}

// Arms the singleton watchdog and returns it, the live one when already armed, nil when disabled
func armStartupWatchdog(timeoutS any, exitCode int) (handle *StartupWatchdogHandle) {
	defer func() {
		if r := recover(); r != nil {
			startupWatchdogLog().Debug("Failed to arm gateway startup watchdog", "exc_info", fmt.Errorf("%v", r))
			handle = nil
		}
	}()
	if StartupWatchdogDisabled() {
		return nil
	}
	startupWatchdogHandleMu.Lock()
	defer startupWatchdogHandleMu.Unlock()
	if startupWatchdogHandle != nil && startupWatchdogHandle.IsAlive() {
		return startupWatchdogHandle
	}
	resolved := 0.0
	if timeoutS != nil {
		value, err := pyint.Float(timeoutS)
		if err != nil {
			startupWatchdogLog().Debug("Failed to arm gateway startup watchdog", "exc_info", err)
			return nil
		}
		resolved = value
	}
	if !(resolved > 0) {
		resolved = ResolveStartupWatchdogTimeout()
	}
	handle = newStartupWatchdogHandle(resolved, exitCode)
	if !handle.start() {
		return nil
	}
	startupWatchdogHandle = handle
	state.SetStartupProgressLease(func(expectedS float64, phase string) { ReportStartupProgress(expectedS, phase) })
	return handle
}

// Disarms the process wide startup watchdog when one is armed
func DisarmStartupWatchdog() {
	startupWatchdogHandleMu.Lock()
	defer startupWatchdogHandleMu.Unlock()
	handle := startupWatchdogHandle
	startupWatchdogHandle = nil
	state.SetStartupProgressLease(nil)
	if handle != nil {
		handle.Disarm()
	}
}

func armedStartupWatchdog() *StartupWatchdogHandle {
	startupWatchdogHandleMu.Lock()
	defer startupWatchdogHandleMu.Unlock()
	return startupWatchdogHandle
}

// Extends the armed watchdog deadline before an intentional idle wait, doing nothing when unarmed
func KickStartupWatchdog(extraS any) {
	if handle := armedStartupWatchdog(); handle != nil {
		handle.Kick(extraS)
	}
}

// Declares a phase owned progress lease on the armed watchdog, doing nothing when unarmed
func ReportStartupProgress(expectedS any, phase string) {
	if handle := armedStartupWatchdog(); handle != nil {
		handle.Lease(expectedS, phase)
	}
}
