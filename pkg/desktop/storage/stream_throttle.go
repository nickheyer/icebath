package storage

import (
	"sync"
	"time"
)

// Idle time after the last turn ends before chat windows are throttled again
const rethrottleDelay = 5 * time.Second

// Chat window whose renderer background throttling the controller switches
type ThrottleWindow interface {
	IsDestroyed() bool
	// True when the renderer contents are gone while the window still exists
	ContentsDestroyed() bool
	IsFullScreen() bool
	// webContents.setBackgroundThrottling, failures swallowed by the implementation
	SetBackgroundThrottling(allowed bool)
	OnClosed(func())
	OnEnterFullScreen(func())
	OnLeaveFullScreen(func())
}

// Runs chat windows unthrottled while any turn is in flight and restores throttling once idle
type StreamThrottle struct {
	mu             sync.Mutex
	windows        []ThrottleWindow
	keepFullscreen bool
	unthrottled    bool
	lastBusy       bool
	trailing       *time.Timer
}

// Throttle controller, fullscreen windows kept painting when keepFullscreenPainting is set
func NewStreamThrottle(keepFullscreenPainting bool) *StreamThrottle {
	return &StreamThrottle{keepFullscreen: keepFullscreenPainting}
}

// True while windows run unthrottled
func (t *StreamThrottle) IsUnthrottled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.unthrottled
}

func (t *StreamThrottle) anyFullscreen() bool {
	if !t.keepFullscreen {
		return false
	}
	for _, w := range t.windows {
		if !w.IsDestroyed() && w.IsFullScreen() {
			return true
		}
	}
	return false
}

func (t *StreamThrottle) remove(w ThrottleWindow) {
	for i, x := range t.windows {
		if x == w {
			t.windows = append(t.windows[:i], t.windows[i+1:]...)
			return
		}
	}
}

// Applies the current state to windows, dropping destroyed ones, outside the lock
func (t *StreamThrottle) apply(targets []ThrottleWindow) {
	t.mu.Lock()
	allowed := !t.unthrottled
	var live []ThrottleWindow
	for _, w := range targets {
		if w.IsDestroyed() {
			t.remove(w)
			continue
		}
		live = append(live, w)
	}
	t.mu.Unlock()
	for _, w := range live {
		if !w.ContentsDestroyed() {
			w.SetBackgroundThrottling(allowed)
		}
	}
}

func (t *StreamThrottle) all() []ThrottleWindow {
	return append([]ThrottleWindow(nil), t.windows...)
}

// Arms the trailing rethrottle unless a turn, a fullscreen window or a pending timer holds it off
func (t *StreamThrottle) armRethrottleIfIdle() {
	if t.lastBusy || t.anyFullscreen() || !t.unthrottled || t.trailing != nil {
		return
	}
	var timer *time.Timer
	timer = time.AfterFunc(rethrottleDelay, func() {
		t.mu.Lock()
		if t.trailing != timer {
			t.mu.Unlock()
			return
		}
		t.trailing = nil
		t.unthrottled = false
		targets := t.all()
		t.mu.Unlock()
		t.apply(targets)
	})
	t.trailing = timer
}

// Cancels the trailing rethrottle and unthrottles, true when windows must be updated
func (t *StreamThrottle) unthrottle() bool {
	if t.trailing != nil {
		t.trailing.Stop()
		t.trailing = nil
	}
	if t.unthrottled {
		return false
	}
	t.unthrottled = true
	return true
}

// Adds a chat window and applies the current state to it
func (t *StreamThrottle) Register(w ThrottleWindow) {
	t.mu.Lock()
	t.windows = append(t.windows, w)
	t.mu.Unlock()
	w.OnClosed(func() {
		t.mu.Lock()
		t.remove(w)
		t.armRethrottleIfIdle()
		t.mu.Unlock()
	})
	if t.keepFullscreen {
		w.OnEnterFullScreen(func() {
			t.mu.Lock()
			changed := t.unthrottle()
			targets := t.all()
			t.mu.Unlock()
			if changed {
				t.apply(targets)
			}
		})
		w.OnLeaveFullScreen(func() {
			t.mu.Lock()
			t.armRethrottleIfIdle()
			t.mu.Unlock()
		})
	}
	t.apply([]ThrottleWindow{w})
}

// Records whether any turn is in flight, unthrottling at once and rethrottling after the idle delay
func (t *StreamThrottle) Update(busy bool) {
	t.mu.Lock()
	t.lastBusy = busy
	if busy {
		changed := t.unthrottle()
		targets := t.all()
		t.mu.Unlock()
		if changed {
			t.apply(targets)
		}
		return
	}
	if !t.anyFullscreen() {
		t.armRethrottleIfIdle()
	}
	t.mu.Unlock()
}
