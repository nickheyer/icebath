package gateway

import (
	"context"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Socket path systemd set in NOTIFY_SOCKET, empty where the platform has no unix sockets
func systemdNotifySocket() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	return strings.TrimSpace(os.Getenv("NOTIFY_SOCKET"))
}

// Sends one sd_notify datagram when systemd configured a socket, false on any failure
func SystemdNotify(message string) bool {
	address := systemdNotifySocket()
	if address == "" || message == "" {
		return false
	}
	return systemdNotifySend(address, message)
}

// Watchdog interval systemd requested in seconds, nil when unset, invalid or not positive
func SystemdWatchdogIntervalSeconds() *float64 {
	raw := ""
	if systemdNotifySocket() != "" {
		raw = os.Getenv("WATCHDOG_USEC")
	}
	usec, err := pyint.ParseFloat(raw)
	if err != nil {
		return nil
	}
	interval := usec / 1e6
	if math.IsInf(interval, 0) || math.IsNaN(interval) || interval <= 0 {
		return nil
	}
	return &interval
}

type systemdWatchdogTaskKey struct{}

// Running feed loop of a watchdog with its cancel and completion
type systemdWatchdogTask struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Whether the feed loop has returned
func (t *systemdWatchdogTask) finished() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// Feeds systemd while the event loop keeps making progress
type SystemdWatchdog struct {
	mu                  sync.Mutex
	configEnabled       bool
	IntervalSeconds     *float64
	lagToleranceSeconds any
	task                *systemdWatchdogTask
	unhealthy           bool
	stopping            bool
	stoppingNotified    bool
}

// Watchdog reading WATCHDOG_USEC now, lag tolerance nil for a quarter of the interval
func NewSystemdWatchdog(configEnabled bool, lagToleranceSeconds any) *SystemdWatchdog {
	return &SystemdWatchdog{configEnabled: configEnabled, IntervalSeconds: SystemdWatchdogIntervalSeconds(),
		lagToleranceSeconds: lagToleranceSeconds}
}

// True when config allows it and systemd requested a watchdog interval
func (w *SystemdWatchdog) Enabled() bool { return w.configEnabled && w.IntervalSeconds != nil }

// True once a late tick marked the loop unhealthy
func (w *SystemdWatchdog) Unhealthy() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.unhealthy
}

// Whether a feed loop was started and not yet stopped
func (w *SystemdWatchdog) HasTask() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.task != nil
}

// Seconds a tick may run late, the configured value or a quarter interval floored at 0.1
func (w *SystemdWatchdog) lagTolerance() float64 {
	interval := 0.0
	if w.IntervalSeconds != nil {
		interval = *w.IntervalSeconds
	}
	fallback := math.Max(0.1, interval*0.25)
	if w.lagToleranceSeconds == nil {
		return fallback
	}
	value, err := pyint.Float(w.lagToleranceSeconds)
	if err != nil {
		return fallback
	}
	if math.IsInf(value, 0) || math.IsNaN(value) {
		return fallback
	}
	return math.Max(0, value)
}

// Starts the feed loop on a running event loop, true when one runs afterwards
func (w *SystemdWatchdog) Start(ctx context.Context) bool {
	if !w.Enabled() {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.task != nil && !w.task.finished() {
		return true
	}
	if !pyctx.RunningLoop(ctx) {
		return false
	}
	w.stopping, w.unhealthy, w.stoppingNotified = false, false, false
	taskCtx, cancel := context.WithCancel(ctx)
	task := &systemdWatchdogTask{cancel: cancel, done: make(chan struct{})}
	w.task = task
	go func() {
		defer close(task.done)
		w.run(context.WithValue(taskCtx, systemdWatchdogTaskKey{}, task))
	}()
	return true
}

// Tells systemd the gateway is ready with a one line status
func (w *SystemdWatchdog) Ready(status string) bool {
	if status == "" {
		status = "Gateway running"
	}
	safe := strings.ReplaceAll(status, "\n", " ")
	return w.Enabled() && SystemdNotify("READY=1\nSTATUS="+safe)
}

// Feeds systemd when the tick woke inside its lag budget, else marks the loop unhealthy once
func (w *SystemdWatchdog) RecordTick(scheduledAt, now float64) bool {
	w.mu.Lock()
	if !w.Enabled() || w.stopping || w.unhealthy {
		w.mu.Unlock()
		return false
	}
	lag := now - scheduledAt
	if math.IsInf(lag, 0) || math.IsNaN(lag) || lag > w.lagTolerance() {
		w.unhealthy = true
		w.mu.Unlock()
		SystemdNotify("STATUS=watchdog unhealthy: event loop progress is late")
		return false
	}
	w.mu.Unlock()
	SystemdNotify("WATCHDOG=1")
	return true
}

// Ticks at half the interval until stopped, cancelled or unhealthy
func (w *SystemdWatchdog) run(ctx context.Context) {
	if w.IntervalSeconds == nil {
		return
	}
	cadence := math.Max(0.01, *w.IntervalSeconds/2.0)
	scheduledAt := pythread.Monotonic() + cadence
	for {
		w.mu.Lock()
		halted := w.stopping || w.unhealthy
		w.mu.Unlock()
		if halted {
			return
		}
		wait := math.Max(0, scheduledAt-pythread.Monotonic())
		timer := time.NewTimer(time.Duration(wait * float64(time.Second)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		now := pythread.Monotonic()
		if !w.RecordTick(scheduledAt, now) {
			return
		}
		scheduledAt += cadence
		if scheduledAt < now {
			scheduledAt = now + cadence
		}
	}
}

// Stops feeding systemd and sends STOPPING=1 at most once
func (w *SystemdWatchdog) Stop(ctx context.Context) {
	w.mu.Lock()
	w.stopping = true
	task := w.task
	w.mu.Unlock()
	if task != nil && ctx.Value(systemdWatchdogTaskKey{}) != task {
		task.cancel()
		<-task.done
	}
	w.mu.Lock()
	w.task = nil
	notify := w.Enabled() && !w.stoppingNotified
	if notify {
		w.stoppingNotified = true
	}
	w.mu.Unlock()
	if notify {
		SystemdNotify("STOPPING=1")
	}
}
