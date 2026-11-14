package windows

import "testing"

type backdropWin struct {
	background                   string
	materialWrites, colourWrites int
}

func (w *backdropWin) SetBackgroundColor(c string) { w.background = c; w.colourWrites++ }

func (w *backdropWin) SetBackgroundMaterial(m string) {
	w.materialWrites++
	if m == "none" {
		w.SetBackgroundColor("#FFFFFFFF")
	} else {
		w.SetBackgroundColor("#00000000")
	}
}

func newBackdropWin() *backdropWin { return &backdropWin{background: "#initial"} }

func TestWindowsBackdropUpdatesKeepsThemedColorAfterClearingTheMaterial(t *testing.T) {
	clearMode := g(60)
	clearMode.Mode = "clear"
	for _, c := range []struct {
		state   TranslucencyState
		backing bool
		themed  string
	}{{g(0), true, "#111111"}, {g(0), false, "#f7f7f7"}, {clearMode, false, "#242424"}} {
		w := newBackdropWin()
		ApplyWindowsBackdrop(w, c.state, c.backing, true, c.themed)
		if w.background != c.themed || w.materialWrites != 1 {
			t.Fatalf("%+v %+v", c, w)
		}
	}
}

func TestWindowsBackdropUpdatesKeepsActiveGlassTransparentWhenChangingItsMaterial(t *testing.T) {
	w := newBackdropWin()
	ApplyWindowsBackdrop(w, glassState(60, "popover", 0), false, true, "#111111")
	if w.background != "#00000000" {
		t.Fatal(w.background)
	}
}

func TestWindowsBackdropUpdatesCanUpdateOnlyTheBackingWithoutTouchingTheMaterial(t *testing.T) {
	w := newBackdropWin()
	ApplyWindowsBackdrop(w, g(0), true, false, "#222222")
	if w.background != "#222222" || w.materialWrites != 0 {
		t.Fatalf("%+v", w)
	}
}

func TestWindowsBackdropUpdatesDoesNotWriteNativeStateForTintOnlyOrOpacityOnlyChanges(t *testing.T) {
	w := newBackdropWin()
	ApplyWindowsBackdrop(w, g(60), false, false, "#111111")
	if w.background != "#initial" || w.materialWrites != 0 || w.colourWrites != 0 {
		t.Fatalf("%+v", w)
	}
}
