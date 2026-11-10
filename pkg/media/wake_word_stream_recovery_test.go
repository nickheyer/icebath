package media

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Capture stand in driven by a read function, recording its closes in a shared list
type scriptedCapture struct {
	read    func(c *scriptedCapture, stop wakeWaiter) ([]int16, bool, error)
	closed  *captureList
	unblock chan struct{}
	once    sync.Once
}

func (c *scriptedCapture) Read(stop wakeWaiter) ([]int16, bool, error) { return c.read(c, stop) }
func (c *scriptedCapture) Rate() int                                   { return WakeSampleRate }
func (c *scriptedCapture) Close() {
	c.closed.add(c)
	if c.unblock != nil {
		c.once.Do(func() { close(c.unblock) })
	}
}

// Goroutine safe ordered list of captures
type captureList struct {
	mu    sync.Mutex
	items []*scriptedCapture
}

func (l *captureList) add(c *scriptedCapture) {
	l.mu.Lock()
	l.items = append(l.items, c)
	l.mu.Unlock()
}

func (l *captureList) snapshot() []*scriptedCapture {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*scriptedCapture(nil), l.items...)
}

func recoveryEngine() *fakeWakeEngine {
	e := newFakeWakeEngine(false)
	e.frameLength = 1280
	return e
}

func TestListenerRecoversAfterTransientCaptureFailure(t *testing.T) {
	recovered := &pythread.Event{}
	opened, closed := &captureList{}, &captureList{}
	engine := recoveryEngine()
	det := NewWakeWordDetector(engine, noWake, WakeDetectorOptions{InputDevice: "chosen microphone"})
	var mu sync.Mutex
	var failures []string
	fail := func(msg string) { mu.Lock(); failures = append(failures, msg); mu.Unlock() }
	det.openCapture = func(frameLength int) (wakeCapture, error) {
		if det.inputDevice != "chosen microphone" {
			fail("input device changed")
		}
		if frameLength != engine.FrameLength() {
			fail("frame length changed")
		}
		if prev := opened.snapshot(); len(prev) > 0 && !slices.Contains(closed.snapshot(), prev[len(prev)-1]) {
			fail("close failed stream before reopening")
		}
		c := &scriptedCapture{closed: closed, read: func(c *scriptedCapture, stop wakeWaiter) ([]int16, bool, error) {
			if len(opened.snapshot()) == 1 {
				return nil, false, errors.New("transient microphone disconnect")
			}
			recovered.Set()
			stop.Wait(5)
			return nil, false, nil
		}}
		opened.add(c)
		return c, nil
	}
	if err := det.Start(); err != nil {
		t.Fatal(err)
	}
	ok := recovered.Wait(3)
	det.Stop()
	if !ok {
		t.Fatal("listener never reopened capture after a transient read failure")
	}
	for _, c := range opened.snapshot() {
		if !slices.Contains(closed.snapshot(), c) {
			t.Fatal("an opened capture was never closed")
		}
	}
	if len(failures) > 0 {
		t.Fatal(failures)
	}
}

func TestCaptureRecoveryIsBoundedAndCancellable(t *testing.T) {
	for _, failure := range []string{"read", "open", "cancel", "external"} {
		t.Run(failure, func(t *testing.T) {
			var calls []int
			closed := &captureList{}
			var failures []*WakeWordDetector
			det := NewWakeWordDetector(recoveryEngine(), noWake, WakeDetectorOptions{
				OnFailure: func(d *WakeWordDetector) { failures = append(failures, d) }, ExternalAudio: failure == "external"})
			det.openCapture = func(frameLength int) (wakeCapture, error) {
				calls = append(calls, frameLength)
				if failure == "open" && len(calls) > 1 {
					return nil, errors.New("still unavailable")
				}
				return &scriptedCapture{closed: closed, read: func(*scriptedCapture, wakeWaiter) ([]int16, bool, error) {
					return nil, false, errors.New("device unavailable")
				}}, nil
			}
			det.stop.wait = func(float64) bool {
				if failure == "cancel" {
					det.stop.Set()
					return true
				}
				return false
			}
			ready := &pythread.Event{}
			var errs []error
			det.run(ready, &errs)
			if !ready.IsSet() || len(errs) != 0 {
				t.Fatalf("ready %v errors %v", ready.IsSet(), errs)
			}
			wantCalls := 4
			if failure == "cancel" || failure == "external" {
				wantCalls = 1
			}
			if len(calls) != wantCalls {
				t.Fatalf("reopening must consume a finite per-arm retry budget, calls %d", len(calls))
			}
			wantClosed := 1
			if failure == "read" {
				wantClosed = 4
			}
			if n := len(closed.snapshot()); n != wantClosed {
				t.Fatalf("closed %d, want %d", n, wantClosed)
			}
			if failure == "cancel" && len(failures) != 0 || failure != "cancel" && (len(failures) != 1 || failures[0] != det) {
				t.Fatalf("failures %v", failures)
			}
		})
	}
}

func TestHaltReleasesReaderWedgedInRecoveredCapture(t *testing.T) {
	recovered := &pythread.Event{}
	opened, closed := &captureList{}, &captureList{}
	det := NewWakeWordDetector(recoveryEngine(), noWake, WakeDetectorOptions{})
	det.openCapture = func(int) (wakeCapture, error) {
		c := &scriptedCapture{closed: closed, unblock: make(chan struct{}), read: func(c *scriptedCapture, _ wakeWaiter) ([]int16, bool, error) {
			if !recovered.IsSet() {
				recovered.Set()
				return nil, false, errors.New("transient microphone disconnect")
			}
			select {
			case <-c.unblock:
			case <-time.After(30 * time.Second):
			}
			return nil, false, nil
		}}
		opened.add(c)
		return c, nil
	}
	swap(t, &wakeHaltJoinSeconds, 0.2)
	if err := det.Start(); err != nil {
		t.Fatal(err)
	}
	defer det.Stop()
	if !recovered.Wait(3) {
		t.Fatal("listener never hit the transient read failure")
	}
	waitFor(3*time.Second, func() bool { return len(opened.snapshot()) >= 2 })
	if len(opened.snapshot()) != 2 {
		t.Fatal("listener never recovered a fresh capture")
	}
	t0 := time.Now()
	det.Pause()
	if time.Since(t0) >= 2500*time.Millisecond {
		t.Fatal("pause() must abort the wedged recovered reader, not block on it")
	}
	if !slices.Contains(closed.snapshot(), opened.snapshot()[1]) {
		t.Fatal("halt must abort the RECOVERED capture, not the dead one")
	}
}
