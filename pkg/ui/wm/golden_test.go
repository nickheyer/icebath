package wm

import (
	"testing"

	"github.com/charmbracelet/x/exp/golden"

	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Manager at 100 by 30 with motion off, settled once so titles are decoded
func scene(t *testing.T, opts ...Option) (*Model, *stub, *stub) {
	t.Helper()
	m := still(t, 100, 30, opts...)
	a, b := newStub("TRACE"), newStub("ICE")
	a.body = "▌ you\n  check disk usage\n\n▌ agent\n  Sure, checking now."
	b.body = "context 12%\ncost $0.01"
	m.AddWorkspace("s1", a)
	m.AddWorkspace("s2", newStub("other"))
	m.OpenHere(b)
	return m, a, b
}

func TestGoldenTilesAndFloat(t *testing.T) {
	m, a, _ := scene(t)
	f := sized{newStub("APPROVAL"), 40, 6}
	f.float = true
	f.body = "⚑ Bash\n  echo hi"
	m.OpenHere(f)
	m.Focus(a)
	m.Advance(1.0 / 60)
	golden.RequireEqual(t, m.View().Content)
}

func TestGoldenZoomed(t *testing.T) {
	m, _, b := scene(t)
	m.Focus(b)
	m.ToggleZoom()
	m.Advance(1.0 / 60)
	golden.RequireEqual(t, m.View().Content)
}

func TestGoldenPaletteAndToast(t *testing.T) {
	m, _, _ := scene(t)
	m.Toast("copied 1 line", Success, 0)
	m.OpenPalette()
	m.Update(key('z'))
	m.Advance(1.0 / 60)
	golden.RequireEqual(t, m.View().Content)
}

func TestGoldenPlainTheme(t *testing.T) {
	m, _, _ := scene(t, WithTheme(theme.Plain()))
	m.Advance(1.0 / 60)
	golden.RequireEqual(t, m.View().Content)
}

func TestGoldenLightNarrow(t *testing.T) {
	m := still(t, 60, 24, WithTheme(theme.Light()))
	a, b := newStub("TRACE"), newStub("ICE")
	a.body = "narrow stage"
	m.AddWorkspace("s1", a)
	m.OpenHere(b)
	m.OpenHere(newStub("JOBS"))
	m.Focus(a)
	m.Advance(1.0 / 60)
	golden.RequireEqual(t, m.View().Content)
}

func TestGoldenCascade(t *testing.T) {
	m := New(WithTheme(theme.Blackwall()), WithCascade(true))
	m.SetSize(100, 30)
	a := newStub("TRACE")
	a.body = "▌ you\n  check disk usage"
	m.AddWorkspace("s1", a)
	m.Focus(a)
	for i := 0; i < 120; i++ {
		m.Advance(1.0 / 60)
	}
	golden.RequireEqual(t, m.View().Content)
}

func TestGoldenDwindleWithHints(t *testing.T) {
	m := still(t, 100, 30, WithLayout(Dwindle))
	a := hinted{newStub("TRACE")}
	a.body = "▌ you\n  check disk usage"
	m.AddWorkspace("s1", a)
	m.OpenHere(newStub("scout-1"))
	m.OpenHere(newStub("ICE"))
	m.Focus(a)
	m.Advance(1.0 / 60)
	golden.RequireEqual(t, m.View().Content)
}

func TestGoldenLayeredFloats(t *testing.T) {
	m, _, _ := scene(t)
	pinned := sized{newStub("PINNED"), 40, 6}
	pinned.float = true
	pinned.body = "stays above the focused dialog"
	m.OpenHere(pinned)
	m.ShiftLayer(1)
	dialog := sized{newStub("DIALOG"), 40, 6}
	dialog.float = true
	dialog.body = "focused, yet painted under"
	m.OpenHere(dialog)
	for range 3 {
		m.Move(Up)
	}
	m.Move(Right)
	m.Advance(1.0 / 60)
	golden.RequireEqual(t, m.View().Content)
}
