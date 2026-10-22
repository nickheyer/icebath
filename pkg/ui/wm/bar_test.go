package wm

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestChipsShowNumbersAlone(t *testing.T) {
	m := still(t, 100, 30)
	ws1, _ := m.AddWorkspace("1", newStub("alpha"))
	ws2, _ := m.AddWorkspace("2", newStub("beta"))
	if m.chipLabel(0, ws1) != " 1 × " || m.chipLabel(1, ws2) != " 2 " {
		t.Fatalf("%q %q", m.chipLabel(0, ws1), m.chipLabel(1, ws2))
	}
	ws2.SetBusy(true)
	if m.chipLabel(1, ws2) != " 2 ● " {
		t.Fatalf("%q", m.chipLabel(1, ws2))
	}
	m.retargetChips()
	if m.chipAt(1) != -1 || m.chipAt(2) != 0 || m.chipAt(6) != 0 || m.chipAt(7) != 1 || m.chipAt(11) != 1 || m.chipAt(12) != -1 {
		t.Fatalf("%v", m.chips)
	}
	if m.chipX.Target != 2 || m.chipW.Target != 5 {
		t.Fatalf("highlight %v %v", m.chipX.Target, m.chipW.Target)
	}
	m.Switch(1)
	if m.chipX.Target != 5 || m.chipW.Target != 7 {
		t.Fatalf("highlight follows %v %v", m.chipX.Target, m.chipW.Target)
	}
	m.SetBar(func() ([]Segment, []Segment) {
		return []Segment{{Text: "model", Style: Cool}, {Text: "", Style: Dim}, {Text: "ctx 12%", Style: Warm}}, []Segment{{Text: "1m02s", Style: Dim}}
	})
	row := ansi.Strip(strings.Split(m.View().Content, "\n")[0])
	if !strings.HasPrefix(row, "   1  2 ● ×  + ") || strings.Contains(row, "alpha") || !strings.Contains(row, "model · ctx 12% · 1m02s   ≡ commands  ") || strings.Contains(row, "alt+") {
		t.Fatalf("%q", row)
	}
	for _, s := range []SegmentStyle{Plain, Dim, Cool, Warm, Hot, Good, Bad} {
		if m.segmentColor(s).Render("x") == "" {
			t.Fatal("segment style")
		}
	}
	ws1.urgent = true
	m.View()
}

func TestToastsExpireWithTheClock(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m := New(WithMotion(0), WithClock(func() time.Time { return now }))
	m.SetSize(100, 30)
	m.AddWorkspace("1", newStub("a"))
	if m.Toast("", Info, 0) != nil {
		t.Fatal("empty toasts are dropped")
	}
	m.Toast("one", Info, 0)
	m.Toast("two", Error, time.Second)
	if toasts := m.Toasts(); len(toasts) != 2 || toasts[0] != "one" {
		t.Fatalf("%v", toasts)
	}
	now = now.Add(2 * time.Second)
	m.Update(toastTickMsg{})
	if toasts := m.Toasts(); len(toasts) != 1 || toasts[0] != "one" {
		t.Fatalf("%v", toasts)
	}
	now = now.Add(DefaultToastTTL)
	m.Update(toastTickMsg{})
	if len(m.Toasts()) != 0 {
		t.Fatal("expired")
	}
	for i := 0; i < 8; i++ {
		m.Toast("many", Warn, 0)
	}
	if len(m.Toasts()) != 5 {
		t.Fatal("toasts are capped")
	}
	m.SetMotion(1)
	m.Toast("slide", Success, 0)
	if !m.stepToasts(1.0 / 60) {
		t.Fatal("toasts slide in")
	}
	m.SetSize(10, 5)
	m.View()
}

func TestLayoutPersistence(t *testing.T) {
	m, _, _, _ := threeTiles(t)
	f := newStub("dialog")
	f.float = true
	m.OpenHere(f)
	l := m.Layout()
	if len(l.Open) != 2 || l.Open[0] != "beta" || l.Open[1] != "gamma" || l.Ratio != defaultRatio || l.Theme != "blackwall" || l.Motion == nil || *l.Motion != 0 {
		t.Fatalf("%+v", l)
	}
	half := 0.5
	m.Restore(Layout{Ratio: 0.4, Motion: &half})
	if m.Ratio() != 0.4 || m.Motion() != 0 {
		t.Fatal("restore applies the ratio but keeps motion fixed by the option")
	}
	fresh := New()
	fresh.SetSize(100, 30)
	fresh.AddWorkspace("1", newStub("a"))
	fresh.Restore(Layout{Ratio: 0.45, Motion: &half})
	if fresh.Ratio() != 0.45 || fresh.Motion() != 0.5 {
		t.Fatal("restore applies both when nothing fixed them")
	}
	fixed := New(WithRatio(0.7))
	fixed.SetSize(100, 30)
	fixed.AddWorkspace("1", newStub("a"))
	fixed.Restore(Layout{Ratio: 0.45})
	if fixed.Ratio() != 0.7 {
		t.Fatal("the ratio option wins")
	}
	fixed.Restore(Layout{})
	if fixed.Ratio() != 0.7 || fixed.Motion() != 1 {
		t.Fatal("an empty layout changes nothing")
	}
}

func TestThemeSwitchRepaints(t *testing.T) {
	m, a, _, _ := threeTiles(t)
	m.View()
	if m.Active().holds(a).dirty {
		t.Fatal("painted")
	}
	m.OpenPalette()
	m.SetTheme(plainTheme())
	if m.Theme().Name != "plain" || !m.Active().holds(a).dirty {
		t.Fatal("theme switch marks cards dirty")
	}
	if p := m.Overlay().(*Palette); p.theme.Name != "plain" {
		t.Fatal("the palette follows the theme")
	}
	m.View()
}
