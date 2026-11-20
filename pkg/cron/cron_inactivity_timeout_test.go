package cron

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// Idle seconds shared between the test and the watchdog goroutine
type sharedIdle struct{ bits atomic.Uint64 }

func (s *sharedIdle) set(v float64) { s.bits.Store(uint64(v * 1e6)) }
func (s *sharedIdle) get() (float64, error) {
	return float64(s.bits.Load()) / 1e6, nil
}

// Runs the loop on its own goroutine and returns its result channel
func watchInBackground(idle *sharedIdle, limit float64, poll time.Duration, stop chan struct{}) <-chan bool {
	out := make(chan bool, 1)
	go func() {
		out <- inactivityWatchdogLoop(idle.get, limit, poll, chanStop(stop), func() bool { return false }, nil)
	}()
	return out
}

func TestInactivityWatchdogLoopFiresWhenIdleCrossesLimit(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	stop := make(chan struct{})
	idle := &sharedIdle{}
	results := watchInBackground(idle, 0.2, 50*time.Millisecond, stop)
	time.Sleep(120 * time.Millisecond)
	idle.set(1.0)
	select {
	case fired := <-results:
		close(stop)
		if !fired {
			t.Fatal("watchdog returned without firing")
		}
	case <-time.After(2 * time.Second):
		close(stop)
		t.Fatal("watchdog still alive")
	}
}

func TestInactivityWatchdogLoopStopsWhenFutureCompletesBeforeIdleLimit(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	stop := make(chan struct{})
	fired := inactivityWatchdogLoop(func() (float64, error) { return 0, nil }, 10, 50*time.Millisecond, chanStop(stop), func() bool { return true }, nil)
	if fired {
		t.Fatal("a finished run fired the watchdog")
	}
}

func TestInactivityWatchdogLoopFiresWhileCallerThreadIsBlocked(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	stop := make(chan struct{})
	idle := &sharedIdle{}
	idle.set(1.0)
	results := watchInBackground(idle, 0.15, 50*time.Millisecond, stop)
	time.Sleep(400 * time.Millisecond)
	select {
	case fired := <-results:
		close(stop)
		if !fired {
			t.Fatal("watchdog returned without firing")
		}
	case <-time.After(2 * time.Second):
		close(stop)
		t.Fatal("watchdog still alive")
	}
}

// Host clocks where wall time runs through a sleep and monotonic time pauses
type hostSleepClock struct {
	mu         sync.Mutex
	wall, mono float64
}

// Stop event that advances both clocks by each poll and sleeps the host once
type hostSleepStop struct {
	clock        *hostSleepClock
	polls        int
	duringSample bool
}

func (s *hostSleepStop) Wait(timeout time.Duration) bool {
	s.clock.mu.Lock()
	defer s.clock.mu.Unlock()
	s.polls++
	s.clock.wall += timeout.Seconds()
	s.clock.mono += timeout.Seconds()
	if s.polls == 3 && !s.duringSample {
		s.clock.wall += 900.0
	}
	return s.polls > 1000
}

func TestHostSleepSleepIsNotIdleTimeButAwakeIdleStillFires(t *testing.T) {
	for _, duringSample := range []bool{false, true} {
		t.Run(map[bool]string{false: "False", true: "True"}[duringSample], func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			clock := &hostSleepClock{wall: 1_000_000.0, mono: 50.0}
			lastActivityWall, startMono := clock.wall, clock.mono
			stop := &hostSleepStop{clock: clock, duringSample: duringSample}
			readIdle := func() (float64, error) {
				clock.mu.Lock()
				defer clock.mu.Unlock()
				idle := clock.wall - lastActivityWall
				if duringSample && stop.polls == 3 {
					clock.wall += 900.0
				}
				return idle, nil
			}
			meter := state.NewAwakeIdleMeterWithClocks(
				func() float64 { clock.mu.Lock(); defer clock.mu.Unlock(); return clock.wall },
				func() float64 { clock.mu.Lock(); defer clock.mu.Unlock(); return clock.mono })
			if !inactivityWatchdogLoop(readIdle, 600.0, 5*time.Second, stop, func() bool { return false }, meter) {
				t.Fatal("awake idle never fired")
			}
			if awake := clock.mono - startMono; awake < 600.0 || awake >= 605.0 {
				t.Fatalf("awake idle %v", awake)
			}
		})
	}
}

// Agent double tracking activity on the fake wall clock and recording each interrupt on the fake monotonic clock
type sleepFirstAgent struct {
	clock         *hostSleepClock
	lastActivity  float64
	started       chan struct{}
	finish        chan struct{}
	finishOnce    sync.Once
	interruptedAt []float64
}

func (a *sleepFirstAgent) RunConversation(context.Context, string, string) (map[string]any, error) {
	a.clock.mu.Lock()
	a.lastActivity = a.clock.wall
	a.clock.mu.Unlock()
	close(a.started)
	select {
	case <-a.finish:
	case <-time.After(20 * time.Second):
	}
	return map[string]any{"final_response": "finished"}, nil
}

func (a *sleepFirstAgent) GetActivitySummary() map[string]any {
	a.clock.mu.Lock()
	defer a.clock.mu.Unlock()
	return map[string]any{"last_activity_ts": a.lastActivity, "seconds_since_activity": a.clock.wall - a.lastActivity}
}

func (a *sleepFirstAgent) Interrupt(string) {
	a.clock.mu.Lock()
	a.interruptedAt = append(a.interruptedAt, a.clock.mono-1000.0)
	a.clock.mu.Unlock()
	a.finishOnce.Do(func() { close(a.finish) })
}

func (a *sleepFirstAgent) SessionID() string         { return "" }
func (a *sleepFirstAgent) SessionAPICalls() int      { return 0 }
func (a *sleepFirstAgent) SetEndSessionOnClose(bool) {}
func (a *sleepFirstAgent) Close() error              { return nil }

// Stop event whose five second waits advance both fake clocks and report the stop without blocking
type fakeClockStop struct {
	stop  <-chan struct{}
	clock *hostSleepClock
}

func (s fakeClockStop) Wait(timeout time.Duration) bool {
	if timeout == 5*time.Second {
		s.clock.mu.Lock()
		s.clock.wall += timeout.Seconds()
		s.clock.mono += timeout.Seconds()
		s.clock.mu.Unlock()
		select {
		case <-s.stop:
			return true
		default:
			return false
		}
	}
	return chanStop(s.stop).Wait(timeout)
}

func TestSleepBeforeWatchdogThreadStarts(t *testing.T) {
	s, _ := newTestStore(t)
	t.Setenv("ICEBATH_CRON_TIMEOUT", "600")
	clock := &hostSleepClock{wall: 1000.0, mono: 1000.0}
	agent := &sleepFirstAgent{clock: clock, lastActivity: 1000.0, started: make(chan struct{}), finish: make(chan struct{})}
	prevMeter, prevStart, prevStop := newWatchdogMeter, startInactivityWatchdog, inactivityStopEvent
	t.Cleanup(func() {
		newWatchdogMeter, startInactivityWatchdog, inactivityStopEvent = prevMeter, prevStart, prevStop
	})
	newWatchdogMeter = func() *state.AwakeIdleMeter {
		return state.NewAwakeIdleMeterWithClocks(
			func() float64 { clock.mu.Lock(); defer clock.mu.Unlock(); return clock.wall },
			func() float64 { clock.mu.Lock(); defer clock.mu.Unlock(); return clock.mono })
	}
	startInactivityWatchdog = func(run func()) {
		select {
		case <-agent.started:
		case <-time.After(5 * time.Second):
			t.Error("worker did not start")
		}
		clock.mu.Lock()
		clock.wall += 900
		clock.mu.Unlock()
		go run()
	}
	inactivityStopEvent = func(stop <-chan struct{}) watchdogStop { return fakeClockStop{stop: stop, clock: clock} }
	defer agent.finishOnce.Do(func() { close(agent.finish) })
	job := &Job{ID: "probe", Schedule: &Schedule{Kind: "every"}}
	_, err := s.runAgentWithWatchdog(context.Background(), agent, "probe", job, "probe", "probe", "probe", nil, nil)
	if cronExceptionName(err) != "TimeoutError" {
		t.Fatalf("err = %v (%s)", err, cronExceptionName(err))
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	if len(agent.interruptedAt) == 0 || slices.Min(agent.interruptedAt) < 600.0 {
		t.Fatalf("interrupted at %v", agent.interruptedAt)
	}
}
