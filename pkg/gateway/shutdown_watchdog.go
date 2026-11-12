package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/tools"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Watchdog defaults, a 60 second leash past the drain, 30 second heartbeats and a 5 second loop floor timer
const (
	DefaultShutdownWatchdogGraceS    = 60.0
	DefaultHeartbeatIntervalS        = 30.0
	DefaultLoopFloorTimerIntervalS   = 5.0
	DefaultLoopWatchdogIntervalS     = 30.0
	DefaultLoopWatchdogTimeoutS      = 10.0
	DefaultLoopWatchdogMaxStrikes    = 3
	shutdownWatchdogDumpRelativeDir  = "logs"
	shutdownWatchdogDumpRelativeFile = "gateway-shutdown-watchdog.log"
	loopHeartbeatRelativeDir         = "state"
	loopHeartbeatRelativeFile        = "gateway.heartbeat"
)

// float() of the value floored, the default when float() rejects it
func watchdogCoerceFloat(value any, def, floor float64) float64 {
	f, err := pyint.Float(value)
	if err != nil {
		return def
	}
	return pyMaxFloat(f, floor)
}

// Launch home when ICEBATH_HOME is set, else the context home, ignoring profile overrides for process identity files
func watchdogProcessHome(ctx context.Context) string {
	if pystr.Strip(os.Getenv("ICEBATH_HOME")) != "" {
		return home.Process()
	}
	return home.Dir(ctx)
}

func watchdogHome(ctx context.Context, h string) string {
	if h != "" {
		return h
	}
	return watchdogProcessHome(ctx)
}

// state/gateway.heartbeat under the given home or the process home
func GetLoopHeartbeatPath(ctx context.Context, h string) string {
	return filepath.Join(watchdogHome(ctx, h), loopHeartbeatRelativeDir, loopHeartbeatRelativeFile)
}

// state/gateway.loop-tick.<pid>.sock, the pid defaulting to this process
func GetLoopTickSocketPath(ctx context.Context, h string, pid *int) string {
	p := os.Getpid()
	if pid != nil {
		p = *pid
	}
	return filepath.Join(watchdogHome(ctx, h), "state", "gateway.loop-tick."+strconv.Itoa(p)+".sock")
}

// logs/gateway-shutdown-watchdog.log under the given home or the process home
func GetShutdownWatchdogDumpPath(ctx context.Context, h string) string {
	return filepath.Join(watchdogHome(ctx, h), shutdownWatchdogDumpRelativeDir, shutdownWatchdogDumpRelativeFile)
}

// Wall clock leash of the shutdown watchdog, the drain plus the grace
func ResolveShutdownWatchdogDelay(drainTimeout, graceS any) float64 {
	grace := watchdogCoerceFloat(graceS, DefaultShutdownWatchdogGraceS, 0)
	return watchdogCoerceFloat(drainTimeout, 0.0, 0) + grace
}

// Python logging CRITICAL, one step above ERROR
const logLevelCritical = slog.LevelError + 4

// threading.TIMEOUT_MAX, the largest wait a lock accepts before raising OverflowError
const pyThreadTimeoutMax = 9223372036.0

var watchdogOSExit = os.Exit

// Flushes queued log records within timeout so the critical line reaches the file before the hard exit
func drainLogQueue(timeoutS float64) {
	logging.DrainLogQueue(time.Duration(timeoutS * float64(time.Second)))
}

// threading.Event.wait, a non positive or NaN timeout polls and one past TIMEOUT_MAX overflows
func pyEventWait(e *pythread.Event, timeoutS float64) (bool, error) {
	if !(timeoutS > 0) {
		return e.IsSet(), nil
	}
	if timeoutS > pyThreadTimeoutMax {
		return false, errors.New("timeout value is too large")
	}
	return e.Wait(timeoutS), nil
}

// Self rescheduling no op loop timer that keeps one timer pending so selector waits stay bounded
type LoopFloorTimerHandle struct {
	loop      EventLoop
	interval  time.Duration
	mu        sync.Mutex
	cancelled bool
	timer     LoopTimer
}

func (h *LoopFloorTimerHandle) tick() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancelled {
		return nil
	}
	timer, err := h.loop.CallLater(h.interval, h.onTimer)
	if err != nil {
		return err
	}
	h.timer = timer
	return nil
}

func (h *LoopFloorTimerHandle) onTimer() {
	if err := h.tick(); err != nil {
		slog.Error(fmt.Sprintf("Exception in callback _LoopFloorTimerHandle._tick(): %v", err))
	}
}

// Stops rescheduling and cancels the pending timer
func (h *LoopFloorTimerHandle) Cancel() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cancelled = true
	if h.timer != nil {
		h.timer.Cancel()
	}
}

// Arms the floor timer, a non positive or uncoercible interval taking the 5 second default
func ArmLoopFloorTimer(loop EventLoop, interval any) (*LoopFloorTimerHandle, error) {
	iv := watchdogCoerceFloat(interval, 0.0, 0)
	if !(iv > 0) {
		iv = DefaultLoopFloorTimerIntervalS
	}
	h := &LoopFloorTimerHandle{loop: loop, interval: secondsDuration(iv)}
	return h, h.tick()
}

// Stop, join and liveness of a running loop liveness watchdog
type LoopLivenessWatchdogHandle struct {
	stopEvent *pythread.Event
	finished  chan struct{}
}

func (h *LoopLivenessWatchdogHandle) Stop() { h.stopEvent.Set() }

// Waits for the watchdog to end, at most timeoutS seconds when it is set
func (h *LoopLivenessWatchdogHandle) Join(timeoutS *float64) {
	if timeoutS == nil {
		<-h.finished
		return
	}
	select {
	case <-h.finished:
	case <-time.After(secondsDuration(pyMaxFloat(0, *timeoutS))):
	}
}

func (h *LoopLivenessWatchdogHandle) IsAlive() bool {
	select {
	case <-h.finished:
		return false
	default:
		return true
	}
}

// Out of loop watchdog that dumps stacks and hard exits after maxStrikes consecutive missed loop probes
func StartLoopLivenessWatchdog(ctx context.Context, loop EventLoop, probeInterval, probeTimeout float64, maxStrikes, exitCode int) *LoopLivenessWatchdogHandle {
	h := &LoopLivenessWatchdogHandle{stopEvent: &pythread.Event{}, finished: make(chan struct{})}
	go pprof.Do(context.Background(), pprof.Labels("name", "gateway-loop-liveness-watchdog"), func(context.Context) {
		defer close(h.finished)
		if err := loopLivenessWatch(ctx, loop, h.stopEvent, probeInterval, probeTimeout, maxStrikes, exitCode); err != nil {
			slog.Error(fmt.Sprintf("Exception in thread gateway-loop-liveness-watchdog: %v", err))
		}
	})
	return h
}

// Probe cycle of the loop liveness watchdog, an error only for a wait the thread runtime refuses
func loopLivenessWatch(ctx context.Context, loop EventLoop, stop *pythread.Event, probeInterval, probeTimeout float64, maxStrikes, exitCode int) error {
	strikes := 0
	for {
		stopped, err := pyEventWait(stop, probeInterval)
		if err != nil || stopped {
			return err
		}
		probe := &pythread.Event{}
		if err := loop.CallSoonThreadsafe(probe.Set); err != nil {
			if !errors.Is(err, ErrLoopClosed) {
				slog.Debug(fmt.Sprintf("Failed to schedule gateway loop liveness probe: %v", err))
			}
			return nil
		}
		deadline := pythread.Monotonic() + probeTimeout
		answered := false
		for !answered {
			if stop.IsSet() {
				return nil
			}
			remaining := deadline - pythread.Monotonic()
			if remaining <= 0 {
				break
			}
			if answered, err = pyEventWait(probe, min(remaining, 0.05)); err != nil {
				return err
			}
		}
		if probe.IsSet() {
			strikes = 0
			continue
		}
		if stop.IsSet() {
			return nil
		}
		strikes++
		if strikes < maxStrikes {
			continue
		}
		if stop.IsSet() {
			return nil
		}
		slog.Log(context.Background(), logLevelCritical, fmt.Sprintf("Gateway event loop missed %d consecutive liveness probes; dumping all thread "+
			"stacks and exiting with code %d so the service supervisor can restart it.", strikes, exitCode))
		writeWatchdogDump(GetShutdownWatchdogDumpPath(context.Background(), ""), probeTimeout*float64(strikes),
			rec2("strikes", int64(strikes), "probe_interval_s", probeInterval), "loop_liveness_watchdog_fired")
		if stop.IsSet() {
			return nil
		}
		markExitedQuietly(ctx, exitCode, "loop_liveness_watchdog")
		hardExit(exitCode)
	}
}

// Ordered JSON object of alternating keys and values
func rec2(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// Kills in flight foreground command trees then exits without any cleanup
func hardExit(exitCode int) {
	func() {
		defer func() { recover() }()
		tools.KillLiveForegroundProcesses(true)
	}()
	watchdogOSExit(exitCode)
}

// Stamps the lifecycle ledger, exit marker and runtime status as degraded before a hard exit skips teardown
func markExitedQuietly(ctx context.Context, exitCode int, reason string) {
	quietly := func(fn func()) {
		defer func() { recover() }()
		fn()
	}
	quietly(func() { MarkExited(context.Background(), &exitCode, reason, "") })
	quietly(func() { observability.StampExit("watchdog", "none") })
	quietly(func() {
		fields := map[string]any{"gateway_state": "degraded", "exit_reason": reason}
		if exitCode == GatewayServiceRestartExitCode {
			fields["restart_requested"] = true
		}
		wait := 250 * time.Millisecond
		WriteRuntimeStatus(ctx, RuntimeStatusUpdate{Fields: fields}, false, &wait)
	})
}

// Rewrites the loop heartbeat file atomically with pid, time, memory and extra fields, returning its path
func WriteLoopHeartbeat(ctx context.Context, pid *int, startTime *float64, h string, extra *pyjson.Object) string {
	path := GetLoopHeartbeatPath(ctx, h)
	payload := pyjson.NewObject()
	p := os.Getpid()
	if pid != nil {
		p = *pid
	}
	payload.Set("pid", int64(p))
	payload.Set("updated_at", ledgerNowISO())
	payload.Set("monotonic", pythread.Monotonic())
	if startTime != nil {
		payload.Set("start_time", *startTime)
	}
	if mem := SampleMemory(); mem.Len() > 0 {
		payload.Set("mem", mem)
	}
	if extra != nil {
		for _, k := range extra.Keys {
			payload.Set(k, extra.Vals[k])
		}
	}
	if err := config.AtomicJSONWriteWith(ctx, path, payload); err != nil {
		slog.Debug(fmt.Sprintf("Failed to write gateway loop heartbeat: %v", err))
	}
	return path
}

// Every goroutine stack, the faulthandler all threads dump
func allGoroutineStacks() []byte {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return buf[:n]
		}
		buf = make([]byte, 2*len(buf))
	}
}

// Appends a metadata header and every goroutine stack to the dump file, then repeats the stacks on stderr
func writeWatchdogDump(dumpPath string, delayS float64, snapshot any, event string) {
	header := pyjson.NewObject()
	header.Set("event", event)
	header.Set("pid", int64(os.Getpid()))
	header.Set("delay_s", delayS)
	header.Set("fired_at", ledgerNowISO())
	if pyrepr.Truthy(snapshot) {
		header.Set("snapshot", snapshot)
	} else {
		header.Set("snapshot", pyjson.NewObject())
	}
	os.MkdirAll(filepath.Dir(dumpPath), 0o777)
	if text, err := pyjson.Dumps(header, pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil })); err == nil {
		if f, err := os.OpenFile(dumpPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666); err == nil {
			f.WriteString(text + "\n--- faulthandler dump (all threads) ---\n")
			f.Write(allGoroutineStacks())
			f.WriteString("--- end dump ---\n")
			f.Close()
		}
	}
	delay, _ := pyrepr.Format(delayS, ".0f")
	fmt.Fprintf(os.Stderr, "Gateway %s after %ss (pid=%d); dumping all thread stacks.\n", strings.ReplaceAll(event, "_", " "), delay, os.Getpid())
	os.Stderr.Write(allGoroutineStacks())
}

// Keyword options of the shutdown watchdog, nil ExitCode is 1 and empty DumpPath and Name take the defaults
type ShutdownWatchdogOptions struct {
	Done       *pythread.Event
	SnapshotFn func() (any, error)
	ExitCode   *int
	DumpPath   string
	Name       string
}

// Hard exit backstop for a wedged shutdown that fires unless the returned event is set within delayS seconds
func ArmShutdownWatchdog(ctx context.Context, delayS any, opts ShutdownWatchdogOptions) *pythread.Event {
	done := opts.Done
	if done == nil {
		done = &pythread.Event{}
	}
	delay := watchdogCoerceFloat(delayS, DefaultShutdownWatchdogGraceS, 0)
	if delay <= 0 {
		return done
	}
	exitCode, name := 1, opts.Name
	if opts.ExitCode != nil {
		exitCode = *opts.ExitCode
	}
	if name == "" {
		name = "gateway-shutdown-watchdog"
	}
	go pprof.Do(context.Background(), pprof.Labels("name", name), func(context.Context) {
		deadline := pythread.Monotonic() + delay
		for now := pythread.Monotonic(); now < deadline; now = pythread.Monotonic() {
			if done.Wait(min(deadline-now, 1.0)) {
				return
			}
		}
		if done.IsSet() {
			return
		}
		snapshot := shutdownSnapshot(opts.SnapshotFn)
		target := opts.DumpPath
		if target == "" {
			target = GetShutdownWatchdogDumpPath(context.Background(), "")
		}
		writeWatchdogDump(target, delay, snapshot, "shutdown_watchdog_fired")
		shown, _ := pyrepr.Format(delay, ".0f")
		slog.Log(context.Background(), logLevelCritical, fmt.Sprintf("Shutdown watchdog fired after %ss — forcing process exit "+
			"(asyncio drain path appears wedged; see %s)", shown, target))
		os.Stdout.Sync()
		os.Stderr.Sync()
		func() {
			defer func() { recover() }()
			RemovePidFile()
			ReleaseGatewayRuntimeLock()
		}()
		func() {
			defer func() { recover() }()
			drainLogQueue(1.0)
		}()
		markExitedQuietly(ctx, exitCode, "shutdown_watchdog")
		hardExit(exitCode)
	})
	return done
}

// Snapshot callback result, an error or panic recorded as snapshot_error
func shutdownSnapshot(fn func() (any, error)) (snapshot any) {
	if fn == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			snapshot = rec2("snapshot_error", fmt.Sprintf("Exception(%s)", pyrepr.StrRepr(fmt.Sprint(r))))
		}
	}()
	v, err := fn()
	if err != nil {
		return rec2("snapshot_error", fmt.Sprintf("Exception(%s)", pyrepr.StrRepr(err.Error())))
	}
	return v
}

// Answers one liveness ping with a single byte from the loop, proving the loop dispatches
func tickSocketHandler(conn net.Conn) {
	conn.Write([]byte("1"))
	conn.Close()
}

// Keyword options of the heartbeat task, a nil IntervalS taking the 30 second default
type LoopHeartbeatOptions struct {
	IntervalS      any
	StartTime      *float64
	Home           string
	ShouldContinue func() bool
}

// Loop tick witness listener over AF_UNIX when the path fits, TCP loopback otherwise, with its socket path or port
func openTickServer(ctx context.Context, h string) (net.Listener, string, any, error) {
	if l, path, ok, err := listenTickUnix(ctx, h); ok || err != nil {
		return l, path, nil, err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", nil, err
	}
	var port any
	if addr, ok := l.Addr().(*net.TCPAddr); ok {
		port = int64(addr.Port)
	}
	return l, "", port, nil
}

// Runs fn on the loop and waits for its answer
func onLoop(ctx context.Context, loop EventLoop, fn func() bool) (bool, error) {
	res := make(chan bool, 1)
	if err := loop.CallSoonThreadsafe(func() { res <- fn() }); err != nil {
		return false, err
	}
	select {
	case v := <-res:
		return v, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// Sleeps on a loop timer so a frozen loop stalls the caller
func loopSleep(ctx context.Context, loop EventLoop, seconds float64) error {
	fired := make(chan struct{})
	timer, err := loop.CallLater(secondsDuration(seconds), func() { close(fired) })
	if err != nil {
		return err
	}
	select {
	case <-fired:
		return nil
	case <-ctx.Done():
		timer.Cancel()
		return ctx.Err()
	}
}

// Rewrites the heartbeat off loop on a loop paced cadence and serves the loop tick witness until ctx ends or gated off
func LoopHeartbeatForever(ctx context.Context, loop EventLoop, opts LoopHeartbeatOptions) error {
	interval := watchdogCoerceFloat(opts.IntervalS, DefaultHeartbeatIntervalS, 1.0)
	listener, tickSocketPath, tickTCPPort, err := openTickServer(ctx, opts.Home)
	if err != nil {
		listener, tickTCPPort = nil, nil
		slog.Warn(fmt.Sprintf("Loop tick socket unavailable — liveness probes will have no "+
			"loop-scheduling witness and will not escalate on a stale heartbeat: %v", err))
	}
	if listener != nil {
		defer func() {
			listener.Close()
			if tickSocketPath != "" {
				os.Remove(tickSocketPath)
			}
		}()
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				if loop.CallSoonThreadsafe(func() { tickSocketHandler(conn) }) != nil {
					conn.Close()
				}
			}
		}()
	}
	extra := rec2("loop_tick_socket", listener != nil, "loop_tick_tcp_port", tickTCPPort)
	gate := func() (bool, error) {
		if opts.ShouldContinue == nil {
			return true, nil
		}
		return onLoop(ctx, loop, opts.ShouldContinue)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		WriteLoopHeartbeat(ctx, nil, opts.StartTime, opts.Home, extra)
		if ok, err := gate(); err != nil || !ok {
			return err
		}
		if err := loopSleep(ctx, loop, interval); err != nil {
			return err
		}
		if ok, err := gate(); err != nil || !ok {
			return err
		}
	}
}
