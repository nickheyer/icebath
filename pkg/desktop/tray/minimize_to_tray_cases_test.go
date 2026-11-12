package tray

import (
	"os"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

type fakeWin struct {
	visible, minimized, destroyed, skipped, focused bool
	calls                                           []string
	handlers                                        map[string][]func()
	onClose                                         func() bool
}

func newWin() *fakeWin { return &fakeWin{visible: true, handlers: map[string][]func(){}} }

func (w *fakeWin) emit(e string) {
	for _, h := range w.handlers[e] {
		h()
	}
}
func (w *fakeWin) IsDestroyed() bool        { return w.destroyed }
func (w *fakeWin) IsMinimized() bool        { return w.minimized }
func (w *fakeWin) IsVisible() bool          { return w.visible }
func (w *fakeWin) Hide()                    { w.calls = append(w.calls, "hide"); w.visible = false }
func (w *fakeWin) Show()                    { w.visible = true; w.emit("show") }
func (w *fakeWin) ShowInactive()            { w.visible = true; w.emit("show") }
func (w *fakeWin) Focus()                   { w.focused = true }
func (w *fakeWin) Blur()                    { w.calls = append(w.calls, "blur"); w.focused = false }
func (w *fakeWin) Restore()                 { w.minimized = false; w.emit("restore") }
func (w *fakeWin) SetSkipTaskbar(skip bool) { w.skipped = skip }
func (w *fakeWin) Send(string, any)         {}
func (w *fakeWin) On(e string, l func())    { w.handlers[e] = append(w.handlers[e], l) }
func (w *fakeWin) minimize()                { w.minimized = true; w.emit("minimize") }
func (w *fakeWin) close() {
	if w.onClose != nil && w.onClose() {
		return
	}
	w.destroyed = true
	w.visible = false
}

type fakeIcon struct{ destroyed *int }

func (i fakeIcon) Destroy() { *i.destroyed++ }

type fakeHost struct {
	wins      []Window
	deferred  []func()
	created   int
	destroyed int
	fail      bool
}

func (h *fakeHost) CreateIcon(func(), bool) (Icon, error) {
	if h.fail {
		return nil, os.ErrInvalid
	}
	h.created++
	return fakeIcon{&h.destroyed}, nil
}
func (h *fakeHost) DockShow()         {}
func (h *fakeHost) DockHide()         {}
func (h *fakeHost) Windows() []Window { return h.wins }
func (h *fakeHost) Defer(f func())    { h.deferred = append(h.deferred, f) }
func (h *fakeHost) flush() {
	for len(h.deferred) > 0 {
		f := h.deferred[0]
		h.deferred = h.deferred[1:]
		f()
	}
}

func setup(t *testing.T, platform string) (*Tray, *fakeWin, *fakeHost) {
	h := &fakeHost{}
	main := newWin()
	h.wins = []Window{main}
	tr := New(Options{
		PreferencesPath: t.TempDir() + "/minimize-to-tray.json", Platform: platform, Host: h,
		WatchHost:            func(func()) (func(), error) { return func() {}, nil },
		RestoreMainWindow:    func() {},
		IsQuittingForHandoff: func() bool { return false },
		Log:                  func(string) {},
	})
	main.onClose = tr.RegisterWindow(main, true)
	return tr, main, h
}

func TestTrayHideDefersPastTheMinimizeDispatch(t *testing.T) {
	tr, main, h := setup(t, "linux")
	tr.Start()
	tr.SetEnabled(true)
	main.minimize()
	if !main.visible {
		t.Fatal("hid inside the minimize dispatch")
	}
	h.flush()
	if main.visible {
		t.Fatal("not hidden after the deferred hide")
	}
}

func TestWindowsRestoreReActivatesTheWindowInsteadOfShowInactive(t *testing.T) {
	tr, main, h := setup(t, "win32")
	tr.Start()
	tr.SetEnabled(true)
	main.minimize()
	h.flush()
	tr.Restore()
	h.flush()
	if !main.visible || main.minimized || main.skipped || !main.focused {
		t.Fatalf("%+v", main)
	}
}

func TestWindowsTrayHideReleasesKeyboardFocusBeforeHiding(t *testing.T) {
	tr, main, _ := setup(t, "win32")
	tr.Start()
	tr.SetEnabled(true)
	main.focused = true
	main.close()
	if main.visible || main.focused || main.destroyed || !reflect.DeepEqual(main.calls, []string{"blur", "hide"}) {
		t.Fatalf("%+v", main)
	}
}

func TestARestoreBeforeTheDeferredHideFiresCancelsTheStaleHide(t *testing.T) {
	tr, main, h := setup(t, "linux")
	tr.Start()
	tr.SetEnabled(true)
	main.minimize()
	main.Restore()
	h.flush()
	if !main.visible || main.minimized {
		t.Fatalf("%+v", main)
	}
}

func TestOptInMinimizeAndPrimaryClosePreserveWindowsWhileExplicitQuitStillExits(t *testing.T) {
	tr, main, h := setup(t, "linux")
	if got := tr.Start(); got != (Status{}) {
		t.Fatalf("%+v", got)
	}
	main.minimize()
	h.flush()
	if !main.visible {
		t.Fatal("hidden while disabled")
	}
	main.Restore()
	if got, _ := tr.SetEnabled(true); got != (Status{Enabled: true, Available: true}) {
		t.Fatalf("%+v", got)
	}
	main.minimize()
	h.flush()
	if main.destroyed || main.visible {
		t.Fatal("minimize did not hide")
	}
	tr.Restore()
	main.close()
	if main.destroyed {
		t.Fatal("close destroyed the window while tray is on")
	}
	tr.BeginQuit()
	main.close()
	if !main.destroyed {
		t.Fatal("quit did not close")
	}
}

func TestLinuxReusesOneTrayWhenMinimizeToTrayIsToggledOffAndOn(t *testing.T) {
	tr, _, h := setup(t, "linux")
	tr.Start()
	tr.SetEnabled(true)
	tr.SetEnabled(false)
	tr.SetEnabled(true)
	if h.created != 1 || h.destroyed != 0 {
		t.Fatalf("created %d destroyed %d", h.created, h.destroyed)
	}
}

func TestPersistenceDisablingFailedTrayCreationAndHandoffNeverStrandHiddenWindows(t *testing.T) {
	tr, main, h := setup(t, "darwin")
	tr.Start()
	tr.SetEnabled(true)
	main.close()
	if main.visible {
		t.Fatal("not hidden")
	}
	tr.SetEnabled(false)
	if !main.visible {
		t.Fatal("disabling stranded a hidden window")
	}
	again := New(Options{PreferencesPath: tr.o.PreferencesPath, Platform: "darwin", Host: &fakeHost{fail: true},
		WatchHost: tr.o.WatchHost, RestoreMainWindow: func() {}, IsQuittingForHandoff: func() bool { return true }, Log: func(string) {}})
	if got := again.Start(); got.Enabled || got.Available {
		t.Fatalf("persisted disabled state not read %+v", got)
	}
	if got, _ := again.SetEnabled(true); !got.Enabled || got.Available {
		t.Fatalf("failed creation reported available %+v", got)
	}
	_ = h
}
