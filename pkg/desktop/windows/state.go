package windows

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	MinWidth                  = 400
	MinHeight                 = 620
	MinVisible                = 48
	recoveredWindowRatio      = 0.8
	legacyExactMatchTolerance = 2
	persistDebounce           = 250 * time.Millisecond
)

// Rect is a window or display rectangle in screen coordinates.
type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Display carries the work area and full bounds of one screen, either may be absent.
type Display struct {
	WorkArea *Rect `json:"workArea,omitempty"`
	Bounds   *Rect `json:"bounds,omitempty"`
}

// State is the sanitized main window geometry stored in window-state.json.
type State struct {
	X                        *int `json:"x,omitempty"`
	Y                        *int `json:"y,omitempty"`
	Width                    int  `json:"width"`
	Height                   int  `json:"height"`
	IsMaximized              bool `json:"isMaximized"`
	BoundsCapturedFullScreen bool `json:"boundsCapturedFullScreen,omitempty"`
}

// Options is the size and optional position a new window opens with.
type Options struct {
	Width  int  `json:"width"`
	Height int  `json:"height"`
	X      *int `json:"x,omitempty"`
	Y      *int `json:"y,omitempty"`
}

// SizeMode names a window size preset.
type SizeMode string

const (
	SizeNormal     SizeMode = "normal"
	SizeOnboarding SizeMode = "onboarding"
)

type sizePreset struct{ shareW, shareH, minW, minH, maxW, maxH float64 }

var windowSizes = map[SizeMode]sizePreset{
	SizeNormal:     {0.88, 0.88, 1280, 820, 1760, 1100},
	SizeOnboarding: {0.5, 0.85, 760, 760, 960, 1000},
}

func jsRound(v float64) int { return int(math.Floor(v + 0.5)) }

func clampInt(v, lo, hi int) int { return max(lo, min(v, hi)) }

func finiteNumber(v any) (float64, bool) {
	f, ok := v.(float64)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func intPtr(v int) *int { return &v }

// SanitizeWindowState returns the stored geometry with minimum size, rounded fields and strict booleans, or nil for garbage
func SanitizeWindowState(raw any) *State {
	rec, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	w, okW := finiteNumber(rec["width"])
	h, okH := finiteNumber(rec["height"])
	if !okW || !okH {
		return nil
	}
	st := &State{Width: max(MinWidth, jsRound(w)), Height: max(MinHeight, jsRound(h)), IsMaximized: rec["isMaximized"] == true}
	if rec["boundsCapturedFullScreen"] == true {
		st.BoundsCapturedFullScreen = true
	}
	x, okX := finiteNumber(rec["x"])
	y, okY := finiteNumber(rec["y"])
	if okX && okY {
		st.X, st.Y = intPtr(jsRound(x)), intPtr(jsRound(y))
	}
	return st
}

func overlap(a Rect, b Rect) (int, int) {
	x := min(a.X+a.Width, b.X+b.Width) - max(a.X, b.X)
	y := min(a.Y+a.Height, b.Y+b.Height) - max(a.Y, b.Y)
	return x, y
}

// MatchingWorkArea returns the work area showing the largest part of bounds when at least minVisible pixels show both ways
func MatchingWorkArea(bounds Rect, displays []Display, minVisible int) *Rect {
	if displays == nil {
		return nil
	}
	var best *Rect
	bestArea := 0
	for _, d := range displays {
		if d.WorkArea == nil {
			continue
		}
		x, y := overlap(bounds, *d.WorkArea)
		if x < minVisible || y < minVisible {
			continue
		}
		if area := x * y; area > bestArea {
			best, bestArea = d.WorkArea, area
		}
	}
	return best
}

// WindowSize returns the preset size for mode inside workArea, never larger than the work area
func WindowSize(mode SizeMode, workArea Rect) Options {
	p := windowSizes[mode]
	w := min(clampInt(jsRound(float64(workArea.Width)*p.shareW), int(p.minW), int(p.maxW)), workArea.Width)
	h := min(clampInt(jsRound(float64(workArea.Height)*p.shareH), int(p.minH), int(p.maxH)), workArea.Height)
	return Options{Width: w, Height: h}
}

func staleFullscreenWorkArea(st State, displays []Display) *Rect {
	if st.IsMaximized || st.X == nil || st.Y == nil || displays == nil {
		return nil
	}
	if !st.BoundsCapturedFullScreen {
		return legacyStaleFullscreenWorkArea(st, displays)
	}
	saved := Rect{*st.X, *st.Y, st.Width, st.Height}
	for _, d := range displays {
		if d.WorkArea == nil {
			continue
		}
		if x, y := overlap(saved, *d.WorkArea); x > 0 && y > 0 {
			return d.WorkArea
		}
	}
	return nil
}

func legacyStaleFullscreenWorkArea(st State, displays []Display) *Rect {
	t := legacyExactMatchTolerance
	abs := func(v int) int { return max(v, -v) }
	exact := func(c Rect) bool {
		return abs(*st.X-c.X) <= t && abs(*st.Y-c.Y) <= t && abs(st.Width-c.Width) <= t && abs(st.Height-c.Height) <= t
	}
	for _, d := range displays {
		for _, c := range []*Rect{d.WorkArea, d.Bounds} {
			if c != nil && exact(*c) {
				return d.WorkArea
			}
		}
	}
	return nil
}

// ComputeWindowOptions fits a saved state to the current displays, recovering stale fullscreen bounds on Windows
func ComputeWindowOptions(st State, displays []Display, platform string) Options {
	opts := Options{Width: st.Width, Height: st.Height}
	capW, capH := 0, 0
	for _, d := range displays {
		if d.WorkArea != nil {
			capW, capH = max(capW, d.WorkArea.Width), max(capH, d.WorkArea.Height)
		}
	}
	if capW != 0 && capH != 0 {
		opts.Width = clampInt(opts.Width, MinWidth, capW)
		opts.Height = clampInt(opts.Height, MinHeight, capH)
	}
	if platform == "win32" {
		if stale := staleFullscreenWorkArea(st, displays); stale != nil {
			opts.Width = clampInt(jsRound(float64(stale.Width)*recoveredWindowRatio), MinWidth, stale.Width)
			opts.Height = clampInt(jsRound(float64(stale.Height)*recoveredWindowRatio), MinHeight, stale.Height)
			return opts
		}
	}
	if st.X != nil && st.Y != nil {
		if wa := MatchingWorkArea(Rect{*st.X, *st.Y, opts.Width, opts.Height}, displays, MinVisible); wa != nil {
			opts.Width = clampInt(opts.Width, MinWidth, wa.Width)
			opts.Height = clampInt(opts.Height, MinHeight, wa.Height)
			opts.X = intPtr(clampInt(*st.X, wa.X, wa.X+wa.Width-opts.Width))
			opts.Y = intPtr(clampInt(*st.Y, wa.Y, wa.Y+wa.Height-opts.Height))
		}
	}
	return opts
}

// Stopper cancels a scheduled callback.
type Stopper interface{ Stop() bool }

// Clock schedules callbacks, the real one is time.AfterFunc.
type Clock interface {
	AfterFunc(d time.Duration, f func()) Stopper
}

type realClock struct{}

func (realClock) AfterFunc(d time.Duration, f func()) Stopper { return time.AfterFunc(d, f) }

// RealClock schedules on the wall clock.
var RealClock Clock = realClock{}

// Debouncer runs fn once after a burst of calls has been quiet for the delay.
type Debouncer struct {
	mu    sync.Mutex
	fn    func()
	delay time.Duration
	clock Clock
	timer Stopper
}

// NewDebouncer coalesces calls into one trailing run of fn after delay on clock
func NewDebouncer(fn func(), delay time.Duration, clock Clock) *Debouncer {
	return &Debouncer{fn: fn, delay: delay, clock: clock}
}

// Call restarts the quiet period
func (d *Debouncer) Call() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil {
		d.timer.Stop()
	}
	var mine Stopper
	mine = d.clock.AfterFunc(d.delay, func() {
		d.mu.Lock()
		if d.timer != mine {
			d.mu.Unlock()
			return
		}
		d.timer = nil
		d.mu.Unlock()
		d.fn()
	})
	d.timer = mine
}

// Flush cancels the pending run and runs fn now
func (d *Debouncer) Flush() {
	d.mu.Lock()
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	d.mu.Unlock()
	d.fn()
}

// GeometryEvents are the window events that schedule a geometry save.
var GeometryEvents = []string{"move", "resize"}

// EventSource subscribes a listener to a named window event.
type EventSource interface {
	On(event string, listener func())
}

// BindGeometryPersistence schedules a save on every move and resize of win
func BindGeometryPersistence(win EventSource, schedule func()) {
	for _, e := range GeometryEvents {
		win.On(e, schedule)
	}
}

// PersistedWindow is what geometry persistence reads from the main window.
type PersistedWindow interface {
	IsDestroyed() bool
	IsMinimized() bool
	IsMaximized() bool
	IsFullScreen() bool
	NormalBounds() Rect
}

// StateFile reads and writes window-state.json under the app data directory.
type StateFile struct {
	Path string
	Log  func(string)
}

// Read returns the sanitized saved geometry or nil when the file is missing or invalid
func (f StateFile) Read() *State {
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return SanitizeWindowState(v)
}

// Persist writes the normal bounds of win unless it is gone, minimized or still at an app chosen size
func (f StateFile) Persist(win PersistedWindow, sizing *Sizing) {
	if win == nil || win.IsDestroyed() || win.IsMinimized() || sizing.IsAppSized(win) {
		return
	}
	b := win.NormalBounds()
	body, _ := json.MarshalIndent(map[string]any{
		"x": b.X, "y": b.Y, "width": b.Width, "height": b.Height,
		"isMaximized": win.IsMaximized(), "boundsCapturedFullScreen": win.IsFullScreen(),
	}, "", "  ")
	if err := WriteFileAtomic(f.Path, body); err != nil {
		f.Log("[window-state] persist failed: " + err.Error())
	}
}

// WriteFileAtomic writes data through a temp file in the same directory and renames it over path
func WriteFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
