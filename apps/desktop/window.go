package main

import (
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/desktop/windows"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// shellWindow adapts a Wails webview window to the pkg/desktop window interfaces.
type shellWindow struct {
	s         *Shell
	w         *application.WebviewWindow
	destroyed atomic.Bool
	mu        sync.Mutex
	zoom      float64
	opacity   float64
	onClose   func() bool
	destroyFn []func()
	revealed  bool
	normal    *windows.Rect
}

func newShellWindow(s *Shell, w *application.WebviewWindow) *shellWindow {
	sw := &shellWindow{s: s, w: w, opacity: 1}
	track := func(*application.WindowEvent) {
		if w.IsMaximised() || w.IsFullscreen() || w.IsMinimised() {
			return
		}
		r := toRect(w.Bounds())
		sw.mu.Lock()
		sw.normal = &r
		sw.mu.Unlock()
	}
	w.OnWindowEvent(events.Common.WindowDidMove, track)
	w.OnWindowEvent(events.Common.WindowDidResize, track)
	w.OnWindowEvent(events.Common.WindowShow, track)
	w.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		if sw.onClose != nil && !sw.onClose() {
			e.Cancel()
			return
		}
		if sw.destroyed.Swap(true) {
			return
		}
		sw.mu.Lock()
		fns := append([]func(){}, sw.destroyFn...)
		sw.mu.Unlock()
		for _, f := range fns {
			f()
		}
	})
	return sw
}

func (sw *shellWindow) onDestroyed(f func()) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.destroyFn = append(sw.destroyFn, f)
}

func (sw *shellWindow) onRuntimeReady(f func()) {
	sw.w.OnWindowEvent(events.Common.WindowRuntimeReady, func(*application.WindowEvent) { f() })
}

// reveal shows the window once its renderer runtime is up, then calls onRevealed
func (sw *shellWindow) reveal(onRevealed func()) {
	sw.onRuntimeReady(func() {
		sw.mu.Lock()
		first := !sw.revealed
		sw.revealed = true
		sw.mu.Unlock()
		if !first || sw.IsDestroyed() {
			return
		}
		sw.w.Show()
		sw.w.Focus()
		onRevealed()
	})
}

var electronToWails = map[string][]events.WindowEventType{
	"move":              {events.Common.WindowDidMove},
	"moved":             {events.Common.WindowDidMove},
	"resize":            {events.Common.WindowDidResize},
	"resized":           {events.Common.WindowDidResize},
	"maximize":          {events.Common.WindowMaximise},
	"unmaximize":        {events.Common.WindowUnMaximise},
	"minimize":          {events.Common.WindowMinimise},
	"restore":           {events.Common.WindowRestore, events.Common.WindowUnMinimise},
	"hide":              {events.Common.WindowHide},
	"show":              {events.Common.WindowShow},
	"focus":             {events.Common.WindowFocus},
	"enter-full-screen": {events.Common.WindowFullscreen},
	"leave-full-screen": {events.Common.WindowUnFullscreen},
}

// On subscribes to an Electron named window event, the will-full-screen pair exists only on macOS as in Electron
func (sw *shellWindow) On(event string, listener func()) {
	types := electronToWails[event]
	if runtime.GOOS == "darwin" {
		switch event {
		case "will-enter-full-screen":
			types = []events.WindowEventType{events.Mac.WindowWillEnterFullScreen}
		case "will-leave-full-screen":
			types = []events.WindowEventType{events.Mac.WindowWillExitFullScreen}
		}
	}
	for _, t := range types {
		sw.w.OnWindowEvent(t, func(*application.WindowEvent) { listener() })
	}
}

// OnNavigation maps page loads to the runtime ready event, WebKit keeps zoom per view across in page routes
func (sw *shellWindow) OnNavigation(event string, listener func(isMainFrame bool)) {
	if event == "did-finish-load" {
		sw.onRuntimeReady(func() { listener(true) })
	}
}

func (sw *shellWindow) Name() string       { return sw.w.Name() }
func (sw *shellWindow) IsDestroyed() bool  { return sw.destroyed.Load() }
func (sw *shellWindow) IsFocused() bool    { return !sw.IsDestroyed() && sw.w.IsFocused() }
func (sw *shellWindow) IsMinimized() bool  { return !sw.IsDestroyed() && sw.w.IsMinimised() }
func (sw *shellWindow) IsMaximized() bool  { return !sw.IsDestroyed() && sw.w.IsMaximised() }
func (sw *shellWindow) IsFullScreen() bool { return !sw.IsDestroyed() && sw.w.IsFullscreen() }
func (sw *shellWindow) IsVisible() bool    { return !sw.IsDestroyed() && sw.w.IsVisible() }
func (sw *shellWindow) Minimize()          { sw.w.Minimise() }
func (sw *shellWindow) Maximize()          { sw.w.Maximise() }
func (sw *shellWindow) Unmaximize()        { sw.w.UnMaximise() }
func (sw *shellWindow) Close()             { sw.w.Close() }
func (sw *shellWindow) Focus()             { sw.w.Focus() }
func (sw *shellWindow) Show()              { sw.w.Show() }
func (sw *shellWindow) Restore()           { sw.w.Restore() }
func (sw *shellWindow) ShowInactive()      { nativeShowInactive(sw.w) }
func (sw *shellWindow) SetFullScreen(b bool) {
	if b {
		sw.w.Fullscreen()
	} else {
		sw.w.UnFullscreen()
	}
}

// Activate restores, shows and focuses the window
func (sw *shellWindow) Activate() {
	if sw.IsDestroyed() {
		return
	}
	if sw.w.IsMinimised() {
		sw.w.Restore()
	}
	sw.w.Show()
	sw.w.Focus()
}

func toRect(r application.Rect) windows.Rect {
	return windows.Rect{X: r.X, Y: r.Y, Width: r.Width, Height: r.Height}
}

func (sw *shellWindow) Bounds() windows.Rect { return toRect(sw.w.Bounds()) }

// NormalBounds returns the last bounds seen while the window was neither maximized, fullscreen nor minimized
func (sw *shellWindow) NormalBounds() windows.Rect {
	sw.mu.Lock()
	n := sw.normal
	sw.mu.Unlock()
	if n != nil {
		return *n
	}
	return sw.Bounds()
}
func (sw *shellWindow) SetBounds(r windows.Rect) {
	sw.w.SetBounds(application.Rect{X: r.X, Y: r.Y, Width: r.Width, Height: r.Height})
}

func (sw *shellWindow) ZoomLevel() float64 {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.zoom
}

func (sw *shellWindow) SetZoomLevel(level float64) {
	sw.mu.Lock()
	sw.zoom = level
	sw.mu.Unlock()
	sw.w.SetZoom(windows.ZoomFactor(level))
}

func (sw *shellWindow) reassertZoom() {
	if sw.IsDestroyed() {
		return
	}
	factor := windows.ZoomFactor(sw.ZoomLevel())
	if math.Abs(sw.w.GetZoom()-factor) > 1e-9 {
		sw.w.SetZoom(factor)
	}
}

// Send delivers an event to this window only, matching webContents.send
func (sw *shellWindow) Send(event string, payload any) {
	if sw.IsDestroyed() {
		return
	}
	sw.w.DispatchWailsEvent(&application.CustomEvent{Name: event, Data: payload})
}

func (sw *shellWindow) SetTitleBarOverlay(o windows.TitleBarOverlay) {
	if sw.s.platform.IsMac() {
		nativeSetTrafficLightInset(sw.w, o.Height)
	}
}

func (sw *shellWindow) SetBackgroundColor(color string) { sw.w.SetBackgroundColour(hexRGBA(color)) }

func (sw *shellWindow) SetBackgroundMaterial(material string) {
	nativeSetBackgroundMaterial(sw.w, material)
}

func (sw *shellWindow) SetVibrancy(material string) { nativeSetVibrancy(sw.w, material) }

func (sw *shellWindow) Opacity() float64 {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.opacity
}

func (sw *shellWindow) SetOpacity(o float64) {
	sw.mu.Lock()
	sw.opacity = o
	sw.mu.Unlock()
	nativeSetOpacity(sw.w, o)
}

// hexRGBA parses #rrggbb or #rrggbbaa into a Wails colour
func hexRGBA(hex string) application.RGBA {
	h := strings.TrimPrefix(hex, "#")
	parse := func(s string) uint8 {
		v, _ := strconv.ParseUint(s, 16, 8)
		return uint8(v)
	}
	c := application.RGBA{Alpha: 255}
	if len(h) >= 6 {
		c.Red, c.Green, c.Blue = parse(h[0:2]), parse(h[2:4]), parse(h[4:6])
	}
	if len(h) == 8 {
		c.Alpha = parse(h[6:8])
	}
	return c
}

func macBackdrop(o windows.SurfaceOptions) application.MacBackdrop {
	if o.Vibrancy != "" && !o.SetBackground {
		return application.MacBackdropTranslucent
	}
	return application.MacBackdropNormal
}

func windowsBackdrop(o windows.SurfaceOptions) application.BackdropType {
	switch o.BackgroundMaterial {
	case "acrylic":
		return application.Acrylic
	case "tabbed":
		return application.Tabbed
	case "mica":
		return application.Mica
	}
	return application.None
}

// wailsHost exposes the shell windows and screens to pkg/desktop/windows.
type wailsHost struct{ shell *Shell }

func (h *wailsHost) Windows() []windows.Window { return h.shell.windowsList() }

func (h *wailsHost) MainWindow() windows.Window {
	if w := h.shell.mainWindow(); w != nil {
		return w
	}
	return nil
}

func (h *wailsHost) WorkAreaOf(b windows.Rect) windows.Rect {
	if wa := windows.MatchingWorkArea(b, h.shell.displays(), 1); wa != nil {
		return *wa
	}
	return h.shell.primaryWorkArea()
}

func (h *wailsHost) ShouldUseDarkColors() bool {
	switch nativeThemeSource() {
	case "dark":
		return true
	case "light":
		return false
	}
	return h.shell.app.Env.IsDarkMode()
}

func (h *wailsHost) SetThemeSource(mode string) { nativeSetThemeSource(mode) }
