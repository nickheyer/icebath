package windows

import (
	"math"
	"os"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

type fakeTimer struct {
	at      time.Duration
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool { was := !t.stopped; t.stopped = true; return was }

// fakeClock runs scheduled callbacks when advanced, in due order
type fakeClock struct {
	mu     sync.Mutex
	now    time.Duration
	timers []*fakeTimer
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Stopper {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now + d, f: f}
	c.timers = append(c.timers, t)
	return t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now + d
	c.mu.Unlock()
	for {
		c.mu.Lock()
		sort.SliceStable(c.timers, func(i, j int) bool { return c.timers[i].at < c.timers[j].at })
		var due *fakeTimer
		for i, t := range c.timers {
			if !t.stopped && t.at <= target {
				due = t
				c.timers = append(c.timers[:i:i], c.timers[i+1:]...)
				break
			}
		}
		if due == nil {
			c.now = target
			c.mu.Unlock()
			return
		}
		c.now = due.at
		c.mu.Unlock()
		due.f()
	}
}

var primary = []Display{{WorkArea: &Rect{0, 0, 1920, 1040}}}
var laptop = []Display{{WorkArea: &Rect{0, 0, 1366, 728}}}

func st(raw map[string]any) State { return *SanitizeWindowState(raw) }

func opt(w, h int, xy ...int) Options {
	o := Options{Width: w, Height: h}
	if len(xy) == 2 {
		o.X, o.Y = intPtr(xy[0]), intPtr(xy[1])
	}
	return o
}

func TestSanitizeWindowStateRejectsMissingGarbageInput(t *testing.T) {
	for _, bad := range []any{nil, "nope", 42.0, map[string]any{}, map[string]any{"width": "x", "height": 800.0}, map[string]any{"width": math.NaN(), "height": 800.0}, map[string]any{"width": 1000.0}} {
		if SanitizeWindowState(bad) != nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}

func TestSanitizeWindowStateKeepsAValidFullStateAndRoundsHiDPIFractions(t *testing.T) {
	got := SanitizeWindowState(map[string]any{"x": 100.6, "y": 50.2, "width": 1400.4, "height": 900.7, "isMaximized": true})
	want := &State{X: intPtr(101), Y: intPtr(50), Width: 1400, Height: 901, IsMaximized: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
}

func TestSanitizeWindowStateFloorsSizeToTheMinimums(t *testing.T) {
	s := st(map[string]any{"width": 10.0, "height": 10.0})
	if s.Width != MinWidth || s.Height != MinHeight {
		t.Fatalf("%+v", s)
	}
}

func TestSanitizeWindowStateDropsAPartialPositionButKeepsTheSize(t *testing.T) {
	got := SanitizeWindowState(map[string]any{"x": 100.0, "width": 1400.0, "height": 900.0})
	if !reflect.DeepEqual(got, &State{Width: 1400, Height: 900}) {
		t.Fatalf("%+v", got)
	}
}

func TestSanitizeWindowStateTreatsIsMaximizedStrictly(t *testing.T) {
	if st(map[string]any{"width": 1400.0, "height": 900.0, "isMaximized": "yes"}).IsMaximized {
		t.Fatal("non boolean maximized accepted")
	}
}

func TestSanitizeWindowStateTreatsBoundsCapturedFullScreenStrictlyAndStaysOptional(t *testing.T) {
	if !st(map[string]any{"width": 1400.0, "height": 900.0, "boundsCapturedFullScreen": true}).BoundsCapturedFullScreen {
		t.Fatal("true not kept")
	}
	if st(map[string]any{"width": 1400.0, "height": 900.0, "boundsCapturedFullScreen": "yes"}).BoundsCapturedFullScreen {
		t.Fatal("string accepted")
	}
	if st(map[string]any{"width": 1400.0, "height": 900.0}).BoundsCapturedFullScreen {
		t.Fatal("absent became true")
	}
}

func TestMatchingWorkAreaAcceptsAWindowOnThePrimaryOrASecondaryDisplay(t *testing.T) {
	dual := append(append([]Display{}, primary...), Display{WorkArea: &Rect{1920, 0, 2560, 1400}})
	if MatchingWorkArea(Rect{100, 100, 1220, 800}, primary, MinVisible) == nil {
		t.Fatal("primary")
	}
	if MatchingWorkArea(Rect{2200, 200, 1220, 800}, dual, MinVisible) == nil {
		t.Fatal("secondary")
	}
}

func TestMatchingWorkAreaRejectsOffScreenSliversAndBadInput(t *testing.T) {
	for _, b := range []Rect{{3000, 100, 1220, 800}, {100, -900, 1220, 800}, {1910, 100, 1220, 800}} {
		if MatchingWorkArea(b, primary, MinVisible) != nil {
			t.Fatalf("%+v matched", b)
		}
	}
	if MatchingWorkArea(Rect{0, 0, 1220, 800}, []Display{}, MinVisible) != nil || MatchingWorkArea(Rect{0, 0, 1220, 800}, nil, MinVisible) != nil {
		t.Fatal("empty displays matched")
	}
}

func TestComputeWindowOptionsCases(t *testing.T) {
	dual := []Display{{WorkArea: &Rect{0, 0, 2560, 1400}}, {WorkArea: &Rect{2560, 0, 1366, 728}}}
	full := []Display{{WorkArea: &Rect{0, 0, 1920, 1040}, Bounds: &Rect{0, 0, 1920, 1080}}}
	cases := []struct {
		name     string
		raw      map[string]any
		displays []Display
		platform string
		want     Options
	}{
		{"test_restores_an_on_screen_position", map[string]any{"x": 200.0, "y": 100.0, "width": 1400.0, "height": 900.0}, primary, "linux", opt(1400, 900, 200, 100)},
		{"test_clamps_a_trusted_saved_position_fully_inside_its_display_work_area", map[string]any{"x": -102.0, "y": 175.0, "width": 960.0, "height": 1032.0}, primary, "linux", opt(960, 1032, 0, 8)},
		{"test_caps_a_positioned_window_to_the_display_it_overlaps", map[string]any{"x": 2700.0, "y": 100.0, "width": 1400.0, "height": 900.0}, dual, "linux", opt(1366, 728, 2560, 0)},
		{"test_keeps_the_size_but_drops_an_off_screen_position", map[string]any{"x": 5000.0, "y": 150.0, "width": 1400.0, "height": 900.0}, primary, "linux", opt(1400, 900)},
		{"test_clamps_a_size_larger_than_the_only_display", map[string]any{"width": 2560.0, "height": 1440.0}, laptop, "linux", opt(1366, 728)},
		{"test_keeps_the_min_floor_on_a_sub_minimum_display", map[string]any{"width": 2000.0, "height": 1500.0}, []Display{{WorkArea: &Rect{0, 0, 360, 480}}}, "linux", opt(MinWidth, MinHeight)},
		{"test_does_not_clamp_when_displays_are_unknown", map[string]any{"width": 2560.0, "height": 1440.0}, []Display{}, "linux", opt(2560, 1440)},
		{"test_recovers_stale_fullscreen_normal_bounds_to_a_centered_windowed_size_on_windows", map[string]any{"x": 0.0, "y": 0.0, "width": 1920.0, "height": 1040.0, "isMaximized": false, "boundsCapturedFullScreen": true}, primary, "win32", opt(1536, 832)},
		{"test_recovers_a_legacy_exact_work_area_snapshot_written_before_the_provenance_flag", map[string]any{"x": 0.0, "y": 0.0, "width": 1920.0, "height": 1040.0, "isMaximized": false}, primary, "win32", opt(1536, 832)},
		{"test_recovers_a_legacy_snapshot_matching_the_full_display_bounds", map[string]any{"x": 0.0, "y": 0.0, "width": 1920.0, "height": 1080.0, "isMaximized": false}, full, "win32", opt(1536, 832)},
		{"test_preserves_a_deliberate_near_fullscreen_normal_window_near_the_origin_on_windows", map[string]any{"x": 0.0, "y": 0.0, "width": 1824.0, "height": 988.0, "isMaximized": false}, primary, "win32", opt(1824, 988, 0, 0)},
		{"test_preserves_deliberate_near_fullscreen_normal_bounds_outside_windows", map[string]any{"x": 0.0, "y": 0.0, "width": 1920.0, "height": 1040.0, "isMaximized": false}, primary, "darwin", opt(1920, 1040, 0, 0)},
		{"test_preserves_full_bounds_when_the_saved_state_is_actually_maximized", map[string]any{"x": 0.0, "y": 0.0, "width": 1920.0, "height": 1040.0, "isMaximized": true}, primary, "win32", opt(1920, 1040, 0, 0)},
		{"test_does_not_shrink_a_large_normal_window_away_from_the_work_area_origin", map[string]any{"x": 240.0, "y": 120.0, "width": 1740.0, "height": 950.0, "isMaximized": false}, primary, "win32", opt(1740, 950, 180, 90)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ComputeWindowOptions(st(c.raw), c.displays, c.platform); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %+v want %+v", got, c.want)
			}
		})
	}
}

func TestDebounceCoalescesABurstIntoOneTrailingRun(t *testing.T) {
	clock := &fakeClock{}
	calls := 0
	d := NewDebouncer(func() { calls++ }, 250*time.Millisecond, clock)
	d.Call()
	d.Call()
	d.Call()
	if calls != 0 {
		t.Fatal(calls)
	}
	clock.Advance(249 * time.Millisecond)
	if calls != 0 {
		t.Fatal(calls)
	}
	clock.Advance(time.Millisecond)
	if calls != 1 {
		t.Fatal(calls)
	}
}

func TestDebounceFlushRunsNowAndCancelsThePendingTimer(t *testing.T) {
	clock := &fakeClock{}
	calls := 0
	d := NewDebouncer(func() { calls++ }, 250*time.Millisecond, clock)
	d.Call()
	d.Flush()
	if calls != 1 {
		t.Fatal(calls)
	}
	clock.Advance(time.Second)
	if calls != 1 {
		t.Fatal(calls)
	}
}

type emitter struct{ handlers map[string][]func() }

func newEmitter() *emitter { return &emitter{handlers: map[string][]func(){}} }

func (e *emitter) On(event string, l func()) { e.handlers[event] = append(e.handlers[event], l) }

func (e *emitter) Emit(event string) {
	for _, l := range e.handlers[event] {
		l()
	}
}

func TestBindGeometryPersistenceSchedulesOnMoveAndResize(t *testing.T) {
	e := newEmitter()
	n := 0
	BindGeometryPersistence(e, func() { n++ })
	e.Emit("move")
	e.Emit("resize")
	e.Emit("focus")
	if n != 2 {
		t.Fatal(n)
	}
}

type geomWindow struct {
	bounds    Rect
	destroyed bool
	minimized bool
	maximized bool
	full      bool
}

func (w *geomWindow) NormalBounds() Rect { return w.bounds }
func (w *geomWindow) Bounds() Rect       { return w.bounds }
func (w *geomWindow) SetBounds(r Rect)   { w.bounds = r }
func (w *geomWindow) IsMaximized() bool  { return w.maximized }
func (w *geomWindow) IsFullScreen() bool { return w.full }
func (w *geomWindow) IsDestroyed() bool  { return w.destroyed }
func (w *geomWindow) IsMinimized() bool  { return w.minimized }
func (w *geomWindow) moveTo(x, y int)    { w.bounds.X, w.bounds.Y = x, y }

type fixedScreen Rect

func (s fixedScreen) WorkAreaOf(Rect) Rect { return Rect(s) }

func TestStateFilePersistsNormalBoundsAndSkipsAppSizedWindows(t *testing.T) {
	f := StateFile{Path: t.TempDir() + "/window-state.json", Log: func(string) {}}
	sizing := NewSizing(fixedScreen{0, 0, 1920, 1080})
	w := &geomWindow{bounds: Rect{10, 20, 1400, 900}, maximized: true}
	f.Persist(w, sizing)
	got := f.Read()
	want := &State{X: intPtr(10), Y: intPtr(20), Width: 1400, Height: 900, IsMaximized: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	w2 := &geomWindow{bounds: Rect{0, 0, 1500, 1000}}
	sizing.MarkAppSized(w2, w2.bounds)
	f.Persist(w2, sizing)
	if !reflect.DeepEqual(f.Read(), want) {
		t.Fatal("app sized window persisted")
	}
	f.Persist(&geomWindow{bounds: Rect{0, 0, 1500, 1000}, minimized: true}, sizing)
	if !reflect.DeepEqual(f.Read(), want) {
		t.Fatal("minimized window persisted")
	}
}

func TestIsAppSizedIsTrueWhileTheWindowSitsAtTheBoundsSizingGaveIt(t *testing.T) {
	s := NewSizing(fixedScreen{0, 0, 1920, 1080})
	w := &geomWindow{bounds: Rect{500, 200, 602, 642}}
	s.MarkAppSized(w, Rect{500, 200, 602, 642})
	if !s.IsAppSized(w) {
		t.Fatal("not app sized")
	}
}

func TestIsAppSizedIsFalseOnceTheUserMovesTheWindowWithoutResizingIt(t *testing.T) {
	s := NewSizing(fixedScreen{0, 0, 1920, 1080})
	w := &geomWindow{bounds: Rect{500, 200, 602, 642}}
	s.MarkAppSized(w, Rect{500, 200, 602, 642})
	w.moveTo(40, 60)
	if s.IsAppSized(w) {
		t.Fatal("still app sized")
	}
}

func TestWindowSizingPutsAnExistingUsersWindowBackWhereItWasWhenTheIntroEnds(t *testing.T) {
	placed := Rect{40, 60, 1500, 900}
	w := &geomWindow{bounds: placed}
	s := NewSizing(fixedScreen{0, 0, 1920, 1080})
	s.HandleSizeRequest(w, w, "onboarding")
	if w.Bounds() == placed {
		t.Fatal("intro did not resize")
	}
	s.HandleSizeRequest(w, w, "normal")
	if w.Bounds() != placed || s.IsAppSized(w) {
		t.Fatalf("%+v", w.Bounds())
	}
}

func TestWindowSizingIgnoresOtherSendersAndUnknownModes(t *testing.T) {
	w := &geomWindow{bounds: Rect{40, 60, 600, 700}}
	other := &geomWindow{bounds: Rect{40, 60, 600, 700}}
	s := NewSizing(fixedScreen{0, 0, 1920, 1080})
	s.HandleSizeRequest(w, other, "normal")
	s.HandleSizeRequest(w, w, "giant")
	if w.Bounds() != (Rect{40, 60, 600, 700}) {
		t.Fatal("resized")
	}
	s.HandleSizeRequest(w, w, "normal")
	if w.Bounds() != (Rect{115, 65, 1690, 950}) || !s.IsAppSized(w) {
		t.Fatalf("%+v", w.Bounds())
	}
}
