package windows

import (
	"encoding/json"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ZoomStorageKey              = "hermes:desktop:zoomLevel"
	zoomFactorBase              = 1.2
	MinZoomLevel                = -9.0
	MaxZoomLevel                = 9.0
	ZoomStep                    = 0.1
	DefaultZoomLevel            = 0.0
	ZoomResizeReassertDelay     = 100 * time.Millisecond
	ZoomReassertSettleDelay     = 300 * time.Millisecond
	ZoomReassertMaxSettleChecks = 3
)

// ClampZoomLevel keeps a finite numeric level inside the Chromium bounds and maps anything else to the default
func ClampZoomLevel(value any) float64 {
	f, ok := value.(float64)
	if !ok {
		if i, isInt := value.(int); isInt {
			f, ok = float64(i), true
		}
	}
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return DefaultZoomLevel
	}
	return math.Min(math.Max(f, MinZoomLevel), MaxZoomLevel)
}

// ZoomFactor converts a zoom level to the webview magnification factor
func ZoomFactor(level float64) float64 { return math.Pow(zoomFactorBase, ClampZoomLevel(level)) }

// ZoomLevelToPercent converts a zoom level to a rounded percentage
func ZoomLevelToPercent(level any) int {
	return jsRound(math.Pow(zoomFactorBase, ClampZoomLevel(level)) * 100)
}

// PercentToZoomLevel converts a positive percentage to a clamped zoom level, anything else to the default
func PercentToZoomLevel(percent any) float64 {
	f, ok := percent.(float64)
	if !ok {
		if i, isInt := percent.(int); isInt {
			f, ok = float64(i), true
		}
	}
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return DefaultZoomLevel
	}
	return ClampZoomLevel(math.Log(f/100) / math.Log(zoomFactorBase))
}

// ZoomChanged is the payload of the zoom:changed event.
type ZoomChanged struct {
	Level   float64 `json:"level"`
	Percent int     `json:"percent"`
}

// ZoomTarget is a webview whose zoom the app sets and notifies.
type ZoomTarget interface {
	SetZoomLevel(level float64)
	Send(event string, payload any)
}

// ApplyZoomLevel clamps level, applies it to the webview and tells its renderer
func ApplyZoomLevel(wc ZoomTarget, level any) float64 {
	clamped := ClampZoomLevel(level)
	wc.SetZoomLevel(clamped)
	wc.Send("zoom:changed", ZoomChanged{Level: clamped, Percent: ZoomLevelToPercent(clamped)})
	return clamped
}

// ZoomReassertWindowEvents lists the window events after which zoom is applied again on platform
func ZoomReassertWindowEvents(platform string) []string {
	switch platform {
	case "linux":
		return []string{"show", "restore", "focus", "resize", "move"}
	case "win32":
		return []string{"show", "restore", "focus", "maximize", "unmaximize", "resized", "moved"}
	}
	return []string{"show", "restore", "focus", "resized", "moved"}
}

// IsDebouncedReassertEvent is true for resize and move everywhere and focus on Linux
func IsDebouncedReassertEvent(event, platform string) bool {
	return event == "resize" || event == "move" || (platform == "linux" && event == "focus")
}

// ReassertWindow emits window events and reports whether it is gone.
type ReassertWindow interface {
	On(event string, listener func())
	IsDestroyed() bool
}

// InstallZoomReassertOnWindowEvents applies zoom again after window transitions, debounced, with a bounded Linux settle chain
func InstallZoomReassertOnWindowEvents(win ReassertWindow, reassert func(), platform string, clock Clock) {
	if win == nil {
		return
	}
	var mu sync.Mutex
	var resizeTimer, settleTimer Stopper
	settleChecks := 0
	stop := func(t Stopper) {
		if t != nil {
			t.Stop()
		}
	}
	var scheduleSettleCheck func()
	scheduleSettleCheck = func() {
		if platform != "linux" {
			return
		}
		mu.Lock()
		settleChecks++
		if settleChecks > ZoomReassertMaxSettleChecks {
			mu.Unlock()
			return
		}
		settleTimer = clock.AfterFunc(ZoomReassertSettleDelay, func() {
			if !win.IsDestroyed() {
				reassert()
				scheduleSettleCheck()
			}
		})
		mu.Unlock()
	}
	reassertWithSettleCheck := func() {
		mu.Lock()
		settleChecks = 0
		mu.Unlock()
		reassert()
		scheduleSettleCheck()
	}
	for _, event := range ZoomReassertWindowEvents(platform) {
		debounced := IsDebouncedReassertEvent(event, platform)
		win.On(event, func() {
			if win.IsDestroyed() {
				return
			}
			if !debounced {
				mu.Lock()
				stop(settleTimer)
				mu.Unlock()
				reassertWithSettleCheck()
				return
			}
			mu.Lock()
			stop(resizeTimer)
			resizeTimer = clock.AfterFunc(ZoomResizeReassertDelay, func() {
				if win.IsDestroyed() {
					return
				}
				mu.Lock()
				stop(settleTimer)
				mu.Unlock()
				reassertWithSettleCheck()
			})
			mu.Unlock()
		})
	}
}

// NavigationSource emits page loads and in page navigations with their main frame flag.
type NavigationSource interface {
	OnNavigation(event string, listener func(isMainFrame bool))
	IsDestroyed() bool
}

// InstallZoomReassertOnNavigation applies zoom again after full loads and main frame in page routes
func InstallZoomReassertOnNavigation(wc NavigationSource, reassert func()) {
	if wc == nil {
		return
	}
	alive := func() {
		if !wc.IsDestroyed() {
			reassert()
		}
	}
	wc.OnNavigation("did-finish-load", func(bool) { alive() })
	wc.OnNavigation("did-navigate-in-page", func(isMainFrame bool) {
		if isMainFrame {
			alive()
		}
	})
}

// ZoomWiring says whether a window kind follows the app zoom.
type ZoomWiring struct {
	Zoom bool `json:"zoom"`
}

var zoomWindowConfig = map[string]ZoomWiring{
	"chat": {Zoom: true}, "petOverlay": {Zoom: false}, "quickEntry": {Zoom: false}, "wakeIndicator": {Zoom: false},
}

// ZoomWiringForWindowKind returns the zoom wiring of kind, chat for unknown kinds
func ZoomWiringForWindowKind(kind string) ZoomWiring {
	if w, ok := zoomWindowConfig[kind]; ok {
		return w
	}
	return zoomWindowConfig["chat"]
}

// ZoomStateFile persists the last applied zoom level in zoom-state.json.
type ZoomStateFile struct {
	Path string
	Log  func(string)
}

// Read returns the saved level as JavaScript Number reads it, or nil when missing or not finite
func (f ZoomStateFile) Read() *float64 {
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		return nil
	}
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	val, present := v["zoomLevel"]
	if !present {
		return nil
	}
	n := JSNumber(val)
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return nil
	}
	return &n
}

// JSNumber converts a decoded JSON value the way JavaScript Number does
func JSNumber(v any) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case float64:
		return t
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return math.NaN()
		}
		return n
	}
	return math.NaN()
}

// Write stores level as the zoom every window restores
func (f ZoomStateFile) Write(level float64) {
	body, _ := json.MarshalIndent(map[string]any{"zoomLevel": level}, "", "  ")
	if err := WriteFileAtomic(f.Path, body); err != nil {
		f.Log("[zoom] json persist failed: " + err.Error())
	}
}

// AcceleratorZoom maps a window accelerator action to the next zoom level from current
func AcceleratorZoom(action MenuAction, current float64) (float64, bool) {
	switch action {
	case ActionZoomReset:
		return DefaultZoomLevel, true
	case ActionZoomIn:
		return current + ZoomStep, true
	case ActionZoomOut:
		return current - ZoomStep, true
	}
	return 0, false
}
