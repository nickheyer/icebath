package system

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// LagWatchdogOptions are the tick cadence, the lag that gets logged and the scheduler of the UI thread ticks
type LagWatchdogOptions struct {
	Cadence   time.Duration
	Threshold time.Duration
	Now       func() time.Time
	Log       func(line string)
	// Runs tick every cadence on the main thread until the returned stop is called
	SetInterval func(tick func(), every time.Duration) (stop func())
}

// LagWatchdog logs main thread ticks that arrive at least the threshold late
type LagWatchdog struct {
	mu         sync.Mutex
	o          LagWatchdogOptions
	stop       func()
	expectedAt time.Time
}

// Watchdog that is idle until Start
func NewLagWatchdog(o LagWatchdogOptions) *LagWatchdog { return &LagWatchdog{o: o} }

func (w *LagWatchdog) tick() {
	w.mu.Lock()
	if w.stop == nil {
		w.mu.Unlock()
		return
	}
	observed := w.o.Now()
	lag := int64(math.Max(0, float64(observed.Sub(w.expectedAt).Milliseconds())))
	expected := w.expectedAt
	w.expectedAt = observed.Add(w.o.Cadence)
	w.mu.Unlock()
	if lag >= w.o.Threshold.Milliseconds() {
		w.o.Log(fmt.Sprintf("[diagnostics] main-process event loop lagged %dms (expected tick at %dms, observed at %dms)", lag, expected.UnixMilli(), observed.UnixMilli()))
	}
}

// Starts ticking, a running watchdog left as it is
func (w *LagWatchdog) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop != nil {
		return
	}
	w.expectedAt = w.o.Now().Add(w.o.Cadence)
	w.stop = w.o.SetInterval(w.tick, w.o.Cadence)
}

// Stops ticking
func (w *LagWatchdog) Stop() {
	w.mu.Lock()
	stop := w.stop
	w.stop = nil
	w.mu.Unlock()
	if stop != nil {
		stop()
	}
}
