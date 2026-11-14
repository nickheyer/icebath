package files

import (
	"errors"
	"testing"
)

type fakeTimer struct {
	next, period int64
	fn           func()
	stopped      bool
}

// fakeClock stands in for vitest fake timers
type fakeClock struct {
	now    int64
	timers []*fakeTimer
}

func (c *fakeClock) NowMS() int64 { return c.now }

func (c *fakeClock) Every(ms int64, fn func()) func() {
	tm := &fakeTimer{next: c.now + ms, period: ms, fn: fn}
	c.timers = append(c.timers, tm)
	return func() { tm.stopped = true }
}

func (c *fakeClock) advance(ms int64) {
	end := c.now + ms
	for {
		var due *fakeTimer
		for _, tm := range c.timers {
			if !tm.stopped && tm.next <= end && (due == nil || tm.next < due.next) {
				due = tm
			}
		}
		if due == nil {
			break
		}
		c.now = due.next
		due.next += due.period
		due.fn()
	}
	c.now = end
}

func (c *fakeClock) timerCount() int {
	n := 0
	for _, tm := range c.timers {
		if !tm.stopped {
			n++
		}
	}
	return n
}

type fakeRawWatcher struct {
	listener WatchListener
	closes   int
}

func (f *fakeRawWatcher) Close() { f.closes++ }

func (f *fakeRawWatcher) emit(times int, filename string) {
	for i := 0; i < times; i++ {
		f.listener("rename", filename)
	}
}

type stormSetup struct {
	fw         *fakeRawWatcher
	guard      *GuardedWatch
	clock      *fakeClock
	events     [][2]string
	pollChange int
	trips      int
	snap       string
}

func setupStorm(t *testing.T, platform string, snapshot func() (string, error)) *stormSetup {
	t.Helper()
	s := &stormSetup{fw: &fakeRawWatcher{}, clock: &fakeClock{}, snap: "a"}
	if snapshot == nil {
		snapshot = func() (string, error) { return s.snap, nil }
	}
	g, err := NewGuardedWatch(GuardedWatchOptions{
		Platform:     platform,
		Watch:        func(l WatchListener) (Closer, error) { s.fw.listener = l; return s.fw, nil },
		OnEvent:      func(e, f string) { s.events = append(s.events, [2]string{e, f}) },
		Snapshot:     snapshot,
		OnPollChange: func() { s.pollChange++ },
		OnTrip:       func(int, int64) { s.trips++ },
		Clock:        s.clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.guard = g
	return s
}

func TestGuardedWatchArmsOnlyOnWin32(t *testing.T) {
	if !StormBreakerArmed("win32") || StormBreakerArmed("darwin") || StormBreakerArmed("linux") {
		t.Fatal("armed")
	}
}

func TestGuardedWatchForwardsEventsUnderTheThresholdAndStaysOnTheWatcher(t *testing.T) {
	s := setupStorm(t, "win32", nil)
	s.fw.emit(WatchStormMaxEvents, "x")
	if len(s.events) != WatchStormMaxEvents || s.fw.closes != 0 || s.guard.Mode() != "watch" {
		t.Fatal(len(s.events), s.fw.closes, s.guard.Mode())
	}
}

func TestGuardedWatchTheWindowResetsSoASteadyRateUnderTheThresholdNeverTrips(t *testing.T) {
	s := setupStorm(t, "win32", nil)
	for i := 0; i < 5; i++ {
		s.fw.emit(WatchStormMaxEvents, "x")
		s.clock.advance(WatchStormWindowMS)
	}
	if s.guard.Mode() != "watch" || s.fw.closes != 0 {
		t.Fatal(s.guard.Mode(), s.fw.closes)
	}
}

func TestGuardedWatchAWin32StormClosesTheWatcherDropsFurtherEventsAndPolls(t *testing.T) {
	s := setupStorm(t, "win32", nil)
	s.fw.emit(WatchStormMaxEvents, "x")
	s.fw.emit(1, "target.md")
	s.fw.emit(50_000, "x")
	if s.fw.closes != 1 || s.guard.Mode() != "poll" || s.trips != 1 || len(s.events) != WatchStormMaxEvents+1 ||
		s.events[len(s.events)-1] != [2]string{"rename", "target.md"} {
		t.Fatal(s.fw.closes, s.guard.Mode(), s.trips, len(s.events))
	}
	s.clock.advance(WatchStormPollMS * 3)
	if s.pollChange != 0 {
		t.Fatal(s.pollChange)
	}
	s.snap = "b"
	s.clock.advance(WatchStormPollMS - 1)
	if s.pollChange != 0 {
		t.Fatal(s.pollChange)
	}
	s.clock.advance(1)
	if s.pollChange != 1 {
		t.Fatal(s.pollChange)
	}
	s.clock.advance(WatchStormPollMS * 2)
	if s.pollChange != 1 {
		t.Fatal(s.pollChange)
	}
}

func TestGuardedWatchTheSameStormOnOtherPlatformsIsForwardedUntouched(t *testing.T) {
	s := setupStorm(t, "darwin", nil)
	s.fw.emit(WatchStormMaxEvents*3, "x")
	if len(s.events) != WatchStormMaxEvents*3 || s.fw.closes != 0 || s.trips != 0 || s.guard.Mode() != "watch" {
		t.Fatal(len(s.events), s.fw.closes, s.trips)
	}
}

func TestGuardedWatchCloseStopsThePollAndIsIdempotent(t *testing.T) {
	s := setupStorm(t, "win32", nil)
	s.fw.emit(WatchStormMaxEvents+1, "x")
	s.guard.Close()
	s.guard.Close()
	s.snap = "b"
	s.clock.advance(WatchStormPollMS * 3)
	if s.pollChange != 0 || s.fw.closes != 1 || s.guard.Mode() != "closed" || s.clock.timerCount() != 0 {
		t.Fatal(s.pollChange, s.fw.closes, s.guard.Mode(), s.clock.timerCount())
	}
}

func TestGuardedWatchCloseBeforeATripClosesTheWatcherOnce(t *testing.T) {
	s := setupStorm(t, "win32", nil)
	s.guard.Close()
	if s.fw.closes != 1 || s.guard.Mode() != "closed" {
		t.Fatal(s.fw.closes)
	}
}

func TestGuardedWatchAThrowingSnapshotTargetVanishedIsTreatedAsAChangeNotACrash(t *testing.T) {
	gone := false
	s := setupStorm(t, "win32", func() (string, error) {
		if gone {
			return "", errors.New("ENOENT")
		}
		return "a", nil
	})
	s.fw.emit(WatchStormMaxEvents+1, "x")
	gone = true
	s.clock.advance(WatchStormPollMS)
	if s.pollChange != 1 {
		t.Fatal(s.pollChange)
	}
}

func TestGuardedWatchAnEmptyDirectoryVanishingIsStillAChangeErrorIsNotTheEmptySnapshot(t *testing.T) {
	gone := false
	s := setupStorm(t, "win32", func() (string, error) {
		if gone {
			return "", errors.New("ENOENT")
		}
		return "", nil
	})
	s.fw.emit(WatchStormMaxEvents+1, "x")
	gone = true
	s.clock.advance(WatchStormPollMS)
	if s.pollChange != 1 {
		t.Fatal(s.pollChange)
	}
}
