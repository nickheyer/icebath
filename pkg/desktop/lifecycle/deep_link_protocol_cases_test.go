package lifecycle

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

type registrar struct{ calls [][]any }

func (r *registrar) SetAsDefaultProtocolClient(protocol, execPath string, args []string) error {
	if execPath == "" && args == nil {
		r.calls = append(r.calls, []any{protocol})
	} else {
		r.calls = append(r.calls, []any{protocol, execPath, args})
	}
	return nil
}

func runRegistration(env map[string]string, defaultApp bool) (*registrar, []string) {
	r := &registrar{}
	var log []string
	RegisterDeepLinkProtocol(r, "icebath", func(k string) string { return env[k] }, defaultApp, []string{"/electron", "."}, "/electron",
		func(e string) string { return "/checkout/" + e }, func(l string) { log = append(log, l) })
	return r, log
}

func TestTheE2EDevGuardLeavesTheOSIcebathHandlerUntouchedNormalLaunchesStillRegister(t *testing.T) {
	skipped, log := runRegistration(map[string]string{"ICEBATH_DESKTOP_SKIP_PROTOCOL_REGISTRATION": "1"}, true)
	if len(skipped.calls) != 0 || !strings.Contains(strings.Join(log, "\n"), "registration skipped") {
		t.Fatal(skipped.calls, log)
	}
	dev, _ := runRegistration(nil, true)
	if !reflect.DeepEqual(dev.calls, [][]any{{"icebath", "/electron", []string{"/checkout/."}}}) {
		t.Fatal(dev.calls)
	}
	packaged, _ := runRegistration(nil, false)
	if !reflect.DeepEqual(packaged.calls, [][]any{{"icebath"}}) {
		t.Fatal(packaged.calls)
	}
}

type mainWin struct {
	destroyed, minimized, visible, fullscreen, focused bool
	sent                                               []any
	actions                                            []string
}

func (w *mainWin) IsDestroyed() bool    { return w.destroyed }
func (w *mainWin) IsMinimized() bool    { return w.minimized }
func (w *mainWin) IsVisible() bool      { return w.visible }
func (w *mainWin) IsFullScreen() bool   { return w.fullscreen }
func (w *mainWin) IsFocused() bool      { return w.focused }
func (w *mainWin) Restore()             { w.actions = append(w.actions, "restore") }
func (w *mainWin) Show()                { w.actions = append(w.actions, "show") }
func (w *mainWin) ShowInactive()        { w.actions = append(w.actions, "showInactive") }
func (w *mainWin) Focus()               { w.actions = append(w.actions, "focus") }
func (w *mainWin) SetFullScreen(f bool) { w.actions = append(w.actions, "fullscreen") }
func (w *mainWin) Activate()            { w.actions = append(w.actions, "activate") }
func (w *mainWin) Send(e string, p any) { w.sent = append(w.sent, []any{e, p}) }

func TestDeepLinksQueueUntilTheRendererIsReadyAndAcceptBothSchemes(t *testing.T) {
	w := &mainWin{visible: true}
	var log []string
	d := NewDeepLinks(false, func() MainWindow { return w }, func(l string) { log = append(log, l) })
	d.Handle("icebath://session/a%20b?x=1&x=2")
	if len(w.sent) != 0 {
		t.Fatal("delivered before ready")
	}
	d.OpenUpdates()
	if got := d.Ready(); !got.OK {
		t.Fatal("ready result")
	}
	want := []any{
		[]any{"open-updates", nil},
		[]any{"deep-link", DeepLink{Kind: "session", Name: "a b", Params: map[string]string{"x": "2"}}},
	}
	if !reflect.DeepEqual(w.sent, want) {
		t.Fatal(w.sent)
	}
	d.Handle("hermes://chat/new")
	if last := w.sent[len(w.sent)-1].([]any); !reflect.DeepEqual(last[1], DeepLink{Kind: "chat", Name: "new", Params: map[string]string{}}) {
		t.Fatal(last)
	}
	d.Handle("ftp://chat/new")
	d.Handle("icebath://copilot-key/other")
	if len(w.sent) != 3 || !strings.Contains(strings.Join(log, "\n"), "ignoring scheme ftp") || !strings.Contains(strings.Join(log, "\n"), "ignoring copilot-key path: other") {
		t.Fatal(w.sent, log)
	}
}

func TestDeepLinksClosePreviewRestoresTheMainWindowBeforeAsking(t *testing.T) {
	w := &mainWin{minimized: true, fullscreen: true}
	d := NewDeepLinks(false, func() MainWindow { return w }, func(string) {})
	d.Handle("icebath://close-preview")
	if !reflect.DeepEqual(w.actions, []string{"restore", "show", "focus", "fullscreen"}) || !reflect.DeepEqual(w.sent, []any{[]any{"close-preview-requested", nil}}) {
		t.Fatal(w.actions, w.sent)
	}
	if d.ExtractDeepLink([]string{"/app", "--x", "hermes://a/b"}) != "hermes://a/b" || !HasClosePreviewFlag([]string{"--close-preview"}) {
		t.Fatal("argv parsing")
	}
}
