package main

import (
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/desktop/storage"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// throttleWindow adapts a chat window to the stream throttle over its webview hidden page throttling
type throttleWindow struct {
	sw      *shellWindow
	allowed atomic.Bool
}

func (t *throttleWindow) IsDestroyed() bool { return t.sw.IsDestroyed() }

// The webview lives exactly as long as its window
func (t *throttleWindow) ContentsDestroyed() bool { return t.sw.IsDestroyed() }

func (t *throttleWindow) IsFullScreen() bool { return t.sw.IsFullScreen() }

// Applies the throttling state on the UI thread, reapplied on every load by installThrottle
func (t *throttleWindow) SetBackgroundThrottling(allowed bool) {
	t.allowed.Store(allowed)
	application.InvokeAsync(func() {
		if !t.sw.IsDestroyed() {
			nativeSetBackgroundThrottling(t.sw, allowed)
		}
	})
}

func (t *throttleWindow) OnClosed(f func())          { t.sw.onDestroyed(f) }
func (t *throttleWindow) OnEnterFullScreen(f func()) { t.sw.On("enter-full-screen", f) }
func (t *throttleWindow) OnLeaveFullScreen(f func()) { t.sw.On("leave-full-screen", f) }

var streamThrottle struct {
	once sync.Once
	t    *storage.StreamThrottle
}

// Stream throttle created on first use, after the display opened, keeping fullscreen painting on native Wayland
func (s *Shell) streamThrottle() *storage.StreamThrottle {
	streamThrottle.once.Do(func() {
		streamThrottle.t = storage.NewStreamThrottle(nativeKeepFullscreenPainting())
	})
	return streamThrottle.t
}

// shellThrottle hands the system service the lazily created stream throttle
type shellThrottle struct{ shell *Shell }

func (t shellThrottle) Update(busy bool) { t.shell.streamThrottle().Update(busy) }

// Registers a chat window with the stream throttle and keeps its webview state across loads
func (s *Shell) installThrottle(sw *shellWindow) {
	tw := &throttleWindow{sw: sw}
	tw.allowed.Store(true)
	nativeInstallThrottle(tw)
	sw.onRuntimeReady(func() { tw.SetBackgroundThrottling(tw.allowed.Load()) })
	s.streamThrottle().Register(tw)
}
