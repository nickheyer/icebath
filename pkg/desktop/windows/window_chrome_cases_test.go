package windows

import (
	"math"
	"reflect"
	"slices"
	"testing"
)

type ctlWindow struct {
	destroyed, maximized, minimized, closed bool
	focusCalls                              int
}

func (w *ctlWindow) IsDestroyed() bool { return w.destroyed }
func (w *ctlWindow) IsMaximized() bool { return w.maximized }
func (w *ctlWindow) Minimize()         { w.minimized = true }
func (w *ctlWindow) Maximize()         { w.maximized = true }
func (w *ctlWindow) Unmaximize()       { w.maximized = false }
func (w *ctlWindow) Close()            { w.closed = true }
func (w *ctlWindow) Focus()            { w.focusCalls++ }

func TestPerformWindowControlMinimizesTheSenderWindow(t *testing.T) {
	w := &ctlWindow{}
	if !PerformWindowControl(w, "minimize") || !w.minimized {
		t.Fatal("not minimized")
	}
}

func TestPerformWindowControlTogglesMaximizeAndRestore(t *testing.T) {
	w := &ctlWindow{}
	if !PerformWindowControl(w, "toggle-maximize") || !w.maximized {
		t.Fatal("not maximized")
	}
	if !PerformWindowControl(w, "toggle-maximize") || w.maximized {
		t.Fatal("not restored")
	}
}

func TestPerformWindowControlRestoresKeyboardFocusAfterMaximizeAndRestore(t *testing.T) {
	w := &ctlWindow{}
	PerformWindowControl(w, "toggle-maximize")
	if w.focusCalls != 1 {
		t.Fatal(w.focusCalls)
	}
	PerformWindowControl(w, "toggle-maximize")
	if w.focusCalls != 2 {
		t.Fatal(w.focusCalls)
	}
}

func TestPerformWindowControlClosesTheSenderWindow(t *testing.T) {
	w := &ctlWindow{}
	if !PerformWindowControl(w, "close") || !w.closed {
		t.Fatal("not closed")
	}
}

func TestPerformWindowControlRejectsUnknownActionsAndDestroyedWindows(t *testing.T) {
	if PerformWindowControl(&ctlWindow{}, "unknown") {
		t.Fatal("unknown handled")
	}
	if PerformWindowControl(&ctlWindow{destroyed: true}, "minimize") {
		t.Fatal("destroyed handled")
	}
	if PerformWindowControl(nil, "minimize") {
		t.Fatal("nil handled")
	}
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestCustomWindowControlsUseTheSameWSLKernelFallbackAsTheMainProcess(t *testing.T) {
	if !CustomWindowControlsEnabled("linux", envOf(nil), "5.15.167.4-microsoft-standard-WSL2") {
		t.Fatal("WSL kernel not detected")
	}
	if CustomWindowControlsEnabled("linux", envOf(nil), "6.8.0-generic") {
		t.Fatal("plain kernel detected as WSL")
	}
}

func TestCustomWindowControlsReadWSLEnvVarsWithoutTouchingTheFilesystem(t *testing.T) {
	if !CustomWindowControlsEnabled("linux", envOf(map[string]string{"WSL_INTEROP": "/run/WSL/1_interop"}), "") {
		t.Fatal("interop not detected")
	}
	if CustomWindowControlsEnabled("linux", envOf(nil), "") {
		t.Fatal("empty env detected")
	}
	if CustomWindowControlsEnabled("darwin", envOf(map[string]string{"WSL_DISTRO_NAME": "Ubuntu"}), "") {
		t.Fatal("darwin detected")
	}
}

func TestWindowControlStateReportsLiveMaximize(t *testing.T) {
	if got := WindowControlState(&ctlWindow{maximized: true}, true); got != (ControlState{true, true}) {
		t.Fatalf("%+v", got)
	}
	if got := WindowControlState(&ctlWindow{maximized: true, destroyed: true}, false); got != (ControlState{false, false}) {
		t.Fatalf("%+v", got)
	}
}

type recorder struct{ order []string }

func (r *recorder) On(event string, l func()) {
	r.order = append(r.order, event)
	handlersOf[r] = append(handlersOf[r], hl{event, l})
}

type hl struct {
	event string
	l     func()
}

var handlersOf = map[*recorder][]hl{}

func (r *recorder) emit(event string) {
	for _, h := range handlersOf[r] {
		if h.event == event {
			h.l()
		}
	}
}

func TestWindowChromeEventsPublishesForTheEmittingWindowOnly(t *testing.T) {
	for _, event := range ChromeStateEvents {
		t.Run(event, func(t *testing.T) {
			p, s := &recorder{}, &recorder{}
			type call struct {
				fs     *bool
				target *recorder
			}
			var calls []call
			pub := func(fs *bool, target *recorder) { calls = append(calls, call{fs, target}) }
			BindWindowChromeEvents(p, pub)
			BindWindowChromeEvents(s, pub)
			s.emit(event)
			if len(calls) != 1 || calls[0].fs != nil || calls[0].target != s {
				t.Fatalf("%+v", calls)
			}
		})
	}
}

func TestWindowChromeEventsRoutesTransitionsToTheirOwnerWithTheTransitionState(t *testing.T) {
	for event, want := range map[string]bool{"will-enter-full-screen": true, "enter-full-screen": true, "will-leave-full-screen": false, "leave-full-screen": false} {
		t.Run(event, func(t *testing.T) {
			p, s := &recorder{}, &recorder{}
			var got []*bool
			var targets []*recorder
			pub := func(fs *bool, target *recorder) { got = append(got, fs); targets = append(targets, target) }
			BindWindowChromeEvents(p, pub)
			BindWindowChromeEvents(s, pub)
			s.emit(event)
			if len(got) != 1 || got[0] == nil || *got[0] != want || targets[0] != s {
				t.Fatalf("%v", got)
			}
		})
	}
}

func roles(items []MenuItem) []string {
	var out []string
	for _, i := range items {
		out = append(out, i.Role)
	}
	return out
}

func TestMacOSRegistersTheNativeWindowMenuSoAppKitWindowCommandsRemainAvailable(t *testing.T) {
	m := WindowMenuTemplate(true)
	if m.Role != "windowMenu" || !reflect.DeepEqual(roles(m.Submenu), []string{"minimize", "zoom", "front"}) {
		t.Fatalf("%+v", m)
	}
}

func TestNonMacOSKeepsTheExplicitCrossPlatformWindowMenu(t *testing.T) {
	m := WindowMenuTemplate(false)
	if m.Label != "Window" || !reflect.DeepEqual(roles(m.Submenu), []string{"minimize", ""}) || m.Submenu[1].Label != "Close" || !m.Submenu[1].NoAccelerator {
		t.Fatalf("%+v", m)
	}
}

func recordStartup(isMac bool) []string {
	var order []string
	InstallApplicationMenuAfterFirstWindow(MenuStartup[string]{
		IsMac:              isMac,
		BuildMenu:          func() string { order = append(order, "build-menu"); return "menu" },
		SetApplicationMenu: func(m string) { order = append(order, "set-menu:"+m) },
		CreateWindow:       func() { order = append(order, "create-window") },
	})
	return order
}

func TestMacOSInstallsTheApplicationMenuOnlyAfterTheFirstWindowExists(t *testing.T) {
	if got := recordStartup(true); !reflect.DeepEqual(got, []string{"create-window", "build-menu", "set-menu:menu"}) {
		t.Fatal(got)
	}
}

func TestOtherPlatformsCreateTheWindowAndNeverBuildAnApplicationMenu(t *testing.T) {
	if got := recordStartup(false); !reflect.DeepEqual(got, []string{"create-window"}) {
		t.Fatal(got)
	}
}

func TestApplicationMenuTemplateCarriesTheFileEditViewWindowHelpMenus(t *testing.T) {
	var labels []string
	for _, m := range ApplicationMenuTemplate(true, "Icebath") {
		labels = append(labels, m.Label)
	}
	if !reflect.DeepEqual(labels, []string{"Icebath", "File", "Edit", "View", "Window", "Help"}) {
		t.Fatal(labels)
	}
	view := ApplicationMenuTemplate(false, "Icebath")[2]
	if view.Label != "View" || view.Submenu[2].Accelerator != "Ctrl+Shift+I" || view.Submenu[4].Action != ActionZoomReset {
		t.Fatalf("%+v", view)
	}
}

func TestStorageKeyStaysStableSoPersistedZoomSurvivesUpgrades(t *testing.T) {
	if ZoomStorageKey != "hermes:desktop:zoomLevel" {
		t.Fatal(ZoomStorageKey)
	}
}

func TestClampZoomLevelRejectsGarbageAndEnforcesBounds(t *testing.T) {
	cases := []struct {
		in   any
		want float64
	}{{math.NaN(), DefaultZoomLevel}, {math.Inf(1), DefaultZoomLevel}, {nil, DefaultZoomLevel}, {"2", DefaultZoomLevel}, {0.3, 0.3}, {-42.0, -9}, {42.0, 9}}
	for _, c := range cases {
		if got := ClampZoomLevel(c.in); got != c.want {
			t.Fatalf("%v -> %v", c.in, got)
		}
	}
}

func TestLevel0IsExactly100PercentChromiumActualSizeBaseline(t *testing.T) {
	if ZoomLevelToPercent(0.0) != 100 || PercentToZoomLevel(100.0) != 0 {
		t.Fatal("baseline")
	}
}

func TestTheShippedDefaultZoomLevelIs100Percent(t *testing.T) {
	if ZoomLevelToPercent(DefaultZoomLevel) != 100 {
		t.Fatal("default")
	}
}

func TestPercentToZoomLevelRejectsGarbageByFallingBackToTheShippedDefault(t *testing.T) {
	for _, in := range []any{math.NaN(), 0.0, -50.0, nil} {
		if PercentToZoomLevel(in) != DefaultZoomLevel {
			t.Fatal(in)
		}
	}
}

var zoomPresets = []float64{50, 67, 75, 80, 90, 100, 110, 125, 150, 175, 200}

func TestPresetPercentagesRoundtripWithinRounding(t *testing.T) {
	for _, p := range zoomPresets {
		if got := ZoomLevelToPercent(PercentToZoomLevel(p)); got != int(p) {
			t.Fatalf("%v -> %v", p, got)
		}
	}
}

func TestConversionIsMonotonicAcrossThePresetRange(t *testing.T) {
	prev := math.Inf(-1)
	for _, p := range zoomPresets {
		l := PercentToZoomLevel(p)
		if !(l > prev) {
			t.Fatalf("%v not above %v", l, prev)
		}
		prev = l
	}
}

func TestExtremePercentagesClampToTheLevelBounds(t *testing.T) {
	if PercentToZoomLevel(1.0) != -9 || PercentToZoomLevel(1_000_000.0) != 9 {
		t.Fatal("extremes")
	}
}

type zoomWin struct {
	destroyed bool
	order     []string
	handlers  map[string]func()
}

func (w *zoomWin) On(event string, l func()) {
	if w.handlers == nil {
		w.handlers = map[string]func(){}
	}
	w.order = append(w.order, event)
	w.handlers[event] = l
}
func (w *zoomWin) IsDestroyed() bool { return w.destroyed }

func TestInstallZoomReassertOnWindowEventsWiresWindowsMaximizeTransitionsInAdditionToNormalLifecycleEvents(t *testing.T) {
	w := &zoomWin{}
	calls := 0
	InstallZoomReassertOnWindowEvents(w, func() { calls++ }, "win32", &fakeClock{})
	if !reflect.DeepEqual(w.order, ZoomReassertWindowEvents("win32")) {
		t.Fatal(w.order)
	}
	for _, e := range []string{"show", "restore", "focus", "maximize", "unmaximize", "resized", "moved"} {
		w.handlers[e]()
	}
	if calls != 7 {
		t.Fatal(calls)
	}
}

func TestZoomReassertWindowEventsDoesNotAddWindowsOnlyMaximizeEventsOnMacOS(t *testing.T) {
	if !reflect.DeepEqual(ZoomReassertWindowEvents("darwin"), []string{"show", "restore", "focus", "resized", "moved"}) {
		t.Fatal("darwin events")
	}
}

func TestIsDebouncedReassertEventDebouncesFocusOnlyOnLinuxNotWindowsMacOS(t *testing.T) {
	for _, c := range []struct {
		e, p string
		want bool
	}{{"focus", "linux", true}, {"focus", "win32", false}, {"focus", "darwin", false}, {"resize", "linux", true}, {"resize", "win32", true}, {"show", "linux", false}, {"restore", "linux", false}} {
		if IsDebouncedReassertEvent(c.e, c.p) != c.want {
			t.Fatal(c)
		}
	}
}

func TestInstallZoomReassertOnWindowEventsDebouncesLinuxResizeMoveAndFocusEventsAtTheTrailingEdge(t *testing.T) {
	clock := &fakeClock{}
	w := &zoomWin{}
	calls := 0
	InstallZoomReassertOnWindowEvents(w, func() { calls++ }, "linux", clock)
	if !reflect.DeepEqual(w.order, ZoomReassertWindowEvents("linux")) {
		t.Fatal(w.order)
	}
	half := ZoomResizeReassertDelay / 2
	w.handlers["resize"]()
	clock.Advance(half)
	w.handlers["move"]()
	clock.Advance(half)
	if calls != 0 {
		t.Fatal(calls)
	}
	clock.Advance(half)
	if calls != 1 {
		t.Fatal(calls)
	}
	w.handlers["focus"]()
	clock.Advance(half)
	if calls != 1 {
		t.Fatal(calls)
	}
	clock.Advance(half)
	if calls != 2 {
		t.Fatal(calls)
	}
	w.handlers["resize"]()
	w.destroyed = true
	clock.Advance(ZoomResizeReassertDelay)
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestInstallZoomReassertOnWindowEventsReVerifiesLinuxZoomAfterTheDebouncedReAssertSoADroppedReApplyIsRetried(t *testing.T) {
	clock := &fakeClock{}
	w := &zoomWin{}
	calls := 0
	InstallZoomReassertOnWindowEvents(w, func() { calls++ }, "linux", clock)
	w.handlers["resize"]()
	clock.Advance(ZoomResizeReassertDelay)
	if calls != 1 {
		t.Fatal(calls)
	}
	for i := 1; i <= ZoomReassertMaxSettleChecks; i++ {
		clock.Advance(ZoomReassertSettleDelay)
		if calls != 1+i {
			t.Fatalf("settle check %d: %d", i, calls)
		}
	}
	clock.Advance(ZoomReassertSettleDelay * 2)
	if calls != 1+ZoomReassertMaxSettleChecks {
		t.Fatal(calls)
	}
}

func TestInstallZoomReassertOnWindowEventsResetsTheSettleChainOnAFreshReAssert(t *testing.T) {
	clock := &fakeClock{}
	w := &zoomWin{}
	calls := 0
	InstallZoomReassertOnWindowEvents(w, func() { calls++ }, "linux", clock)
	w.handlers["show"]()
	clock.Advance(ZoomReassertSettleDelay)
	if calls != 2 {
		t.Fatal(calls)
	}
	w.handlers["restore"]()
	if calls != 3 {
		t.Fatal(calls)
	}
	for i := 1; i <= ZoomReassertMaxSettleChecks; i++ {
		clock.Advance(ZoomReassertSettleDelay)
		if calls != 3+i {
			t.Fatalf("restarted check %d: %d", i, calls)
		}
	}
	clock.Advance(ZoomReassertSettleDelay * 2)
	if calls != 3+ZoomReassertMaxSettleChecks {
		t.Fatal(calls)
	}
}

func TestInstallZoomReassertOnWindowEventsSkipsDestroyedWindows(t *testing.T) {
	w := &zoomWin{destroyed: true}
	calls := 0
	InstallZoomReassertOnWindowEvents(w, func() { calls++ }, "win32", &fakeClock{})
	for _, e := range ZoomReassertWindowEvents("win32") {
		w.handlers[e]()
	}
	if calls != 0 {
		t.Fatal(calls)
	}
}

type navSource struct {
	order    []string
	handlers map[string]func(bool)
}

func (n *navSource) OnNavigation(e string, l func(bool)) {
	if n.handlers == nil {
		n.handlers = map[string]func(bool){}
	}
	n.order = append(n.order, e)
	n.handlers[e] = l
}
func (n *navSource) IsDestroyed() bool { return false }

func TestInstallZoomReassertOnNavigationCoversFullLoadsAndMainFrameInPageRoutes(t *testing.T) {
	n := &navSource{}
	calls := 0
	InstallZoomReassertOnNavigation(n, func() { calls++ })
	if !reflect.DeepEqual(n.order, []string{"did-finish-load", "did-navigate-in-page"}) {
		t.Fatal(n.order)
	}
	n.handlers["did-finish-load"](true)
	n.handlers["did-navigate-in-page"](true)
	if calls != 2 {
		t.Fatal(calls)
	}
	n.handlers["did-navigate-in-page"](false)
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestZoomWiringForWindowKinds(t *testing.T) {
	for kind, want := range map[string]bool{"chat": true, "petOverlay": false, "wakeIndicator": false, "unknown": true, "": true} {
		if ZoomWiringForWindowKind(kind).Zoom != want {
			t.Fatal(kind)
		}
	}
}

type zoomCalls struct{ calls []any }

func (z *zoomCalls) SetZoomLevel(l float64) { z.calls = append(z.calls, []any{"setZoomLevel", l}) }
func (z *zoomCalls) Send(event string, payload any) {
	z.calls = append(z.calls, []any{"send", event, payload})
}

func TestApplyZoomLevelAppliesTheLevelThenNotifiesTheRenderer(t *testing.T) {
	z := &zoomCalls{}
	if ApplyZoomLevel(z, 3.0) != 3 {
		t.Fatal("applied")
	}
	want := []any{[]any{"setZoomLevel", 3.0}, []any{"send", "zoom:changed", ZoomChanged{Level: 3, Percent: ZoomLevelToPercent(3.0)}}}
	if !reflect.DeepEqual(z.calls, want) {
		t.Fatal(z.calls)
	}
}

func TestApplyZoomLevelClampsGarbageBeforeApplyingAndNotifying(t *testing.T) {
	z := &zoomCalls{}
	clamped := ClampZoomLevel(99.0)
	if ApplyZoomLevel(z, 99.0) != clamped {
		t.Fatal("applied")
	}
	want := []any{[]any{"setZoomLevel", clamped}, []any{"send", "zoom:changed", ZoomChanged{Level: clamped, Percent: ZoomLevelToPercent(clamped)}}}
	if !reflect.DeepEqual(z.calls, want) {
		t.Fatal(z.calls)
	}
}

func TestZoomStateFileRoundTripsTheLevel(t *testing.T) {
	f := ZoomStateFile{Path: t.TempDir() + "/zoom-state.json", Log: func(string) {}}
	if f.Read() != nil {
		t.Fatal("empty read")
	}
	f.Write(1.5)
	if got := f.Read(); got == nil || *got != 1.5 {
		t.Fatal(got)
	}
}

func TestWindowsReservesTheOverlayFallbackWidth(t *testing.T) {
	if NativeOverlayWidth(false) != OverlayFallbackWidth {
		t.Fatal("windows width")
	}
}

func TestWSLgDisablesTheUndersizedNativeOverlayInFavorOfRendererControls(t *testing.T) {
	if !TitleBarOverlayOptions(TitleBarOverlayInput{Platform: "wslg", TitlebarHeight: 34, Color: "#000000", Dark: true}).Disabled {
		t.Fatal("wslg overlay enabled")
	}
}

func TestNativeWindowsAndLinuxKeepTheSameWindowControlsOverlay(t *testing.T) {
	in := TitleBarOverlayInput{TitlebarHeight: 34, Color: "rgba(1, 0, 0, 0)", Dark: true}
	want := TitleBarOverlay{Color: "rgba(1, 0, 0, 0)", Height: 34, SymbolColor: "#f7f7f7"}
	for _, p := range []string{"windows", "linux"} {
		in.Platform = p
		if got := TitleBarOverlayOptions(in); got != want {
			t.Fatalf("%s %+v", p, got)
		}
	}
}

func TestMacOSKeepsItsHeightOnlyTrafficLightOverlay(t *testing.T) {
	got := TitleBarOverlayOptions(TitleBarOverlayInput{Platform: "mac", DarwinMajor: 24, TitlebarHeight: 34, Color: "#000000"})
	if got != (TitleBarOverlay{Height: 34}) {
		t.Fatalf("%+v", got)
	}
}

func TestPlainLinuxPaintsTheWCOTooSoItReservesTheFallbackWidthAndMacOSReservesNothing(t *testing.T) {
	if NativeOverlayWidth(false) != OverlayFallbackWidth || NativeOverlayWidth(true) != 0 {
		t.Fatal("widths")
	}
}

func TestMacTitleBarOverlayHeightAroundTahoe(t *testing.T) {
	if MacTitleBarOverlayHeight(MacOSTahoeDarwinMajor-1, 34) != 34 || MacTitleBarOverlayHeight(MacOSTahoeDarwinMajor, 34) != 0 || MacTitleBarOverlayHeight(MacOSTahoeDarwinMajor+1, 34) != 0 || MacTitleBarOverlayHeight(0, 0) != 0 {
		t.Fatal("tahoe heights")
	}
}

func TestScaledOverlayHeightTracksTheZoomFactorInWholePixels(t *testing.T) {
	if ScaledOverlayHeight(34, 1) != 34 || ScaledOverlayHeight(34, 1.2) != 41 || ScaledOverlayHeight(34, 0.9) != 31 {
		t.Fatal("scaled")
	}
}

func TestScaledOverlayHeightClampsGarbageInputInsteadOfBreakingTheOverlay(t *testing.T) {
	if ScaledOverlayHeight(34, math.NaN()) != 34 || ScaledOverlayHeight(34, 0) != 34 || ScaledOverlayHeight(34, -2) != 34 || !math.IsNaN(ScaledOverlayHeight(math.NaN(), 1.5)) {
		t.Fatal("garbage")
	}
}

func TestScaledOverlayHeightNeverReturnsAZeroHeightOverlay(t *testing.T) {
	if ScaledOverlayHeight(34, 0.01) != 1 {
		t.Fatal("zero height")
	}
}

func TestTitleBarOverlayOptionsScalesTheWindowsLinuxOverlayHeightWithZoom(t *testing.T) {
	base := TitleBarOverlayInput{Platform: "windows", TitlebarHeight: 34, Color: "#101010", Foreground: "#eeeeee"}
	base.ZoomFactor = 1
	if got := TitleBarOverlayOptions(base); got != (TitleBarOverlay{Color: "#101010", Height: 34, SymbolColor: "#eeeeee"}) {
		t.Fatalf("%+v", got)
	}
	base.ZoomFactor = 1.2
	if got := TitleBarOverlayOptions(base); got != (TitleBarOverlay{Color: "#101010", Height: 41, SymbolColor: "#eeeeee"}) {
		t.Fatalf("%+v", got)
	}
}

func TestTitleBarOverlayOptionsKeepsWSLgAndMacOSUntouchedByZoom(t *testing.T) {
	if !TitleBarOverlayOptions(TitleBarOverlayInput{Platform: "wslg", TitlebarHeight: 34, ZoomFactor: 1.5}).Disabled {
		t.Fatal("wslg")
	}
	if got := TitleBarOverlayOptions(TitleBarOverlayInput{Platform: "mac", TitlebarHeight: 34, ZoomFactor: 1.5}); got != (TitleBarOverlay{Height: 34}) {
		t.Fatalf("%+v", got)
	}
}

func TestDefaultZoomLeavesTheOverlayByteIdenticalToToday(t *testing.T) {
	got := TitleBarOverlayOptions(TitleBarOverlayInput{Platform: "windows", TitlebarHeight: 34, Color: "#101010"})
	if got != (TitleBarOverlay{Color: "#101010", Height: 34, SymbolColor: "#242424"}) {
		t.Fatalf("%+v", got)
	}
}

func TestChromeThemeSourcePersistsAndRejectsUnknownModes(t *testing.T) {
	path := t.TempDir() + "/native-theme.json"
	c := NewChrome(path, func(string) {})
	if c.ThemeSource() != "system" || c.SetNativeTheme("sepia") || !c.SetNativeTheme("dark") || c.SetNativeTheme("dark") {
		t.Fatal("theme switches")
	}
	if NewChrome(path, func(string) {}).ThemeSource() != "dark" {
		t.Fatal("not persisted")
	}
	if c.SetTitleBarTheme(map[string]any{"background": "#123", "foreground": "#ffffff"}) || !c.SetTitleBarTheme(map[string]any{"background": "#123456", "foreground": "#ffffff"}) {
		t.Fatal("hex validation")
	}
	if c.WindowBackgroundColor(true) != "#123456" || !slices.Contains([]string{"#ffffff"}, c.Overlay("windows", 0, true, 1).SymbolColor) {
		t.Fatal("theme not applied")
	}
}
