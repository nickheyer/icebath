package tty

import (
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestFreshModesFollowBubbleTea(t *testing.T) {
	v := tea.NewView("x")
	v.AltScreen, v.ReportFocus, v.MouseMode, v.WindowTitle = true, true, tea.MouseModeAllMotion, "title"
	v.KeyboardEnhancements.ReportEventTypes = true
	v.BackgroundColor = color.RGBA{1, 2, 3, 255}
	v.Cursor = &tea.Cursor{Shape: tea.CursorBar, Blink: false, Color: color.RGBA{9, 8, 7, 255}}
	v.ProgressBar = tea.NewProgressBar(tea.ProgressBarDefault, 40)
	v.ProgramStatus = &tea.ProgramStatus{State: ansi.ProgramStateWorking, ID: "a"}
	m := modesOf(&v)
	got := string(modeDelta(nil, nil, &m))
	want := ansi.SetModeBracketedPaste + ansi.SetModeFocusEvent + ansi.SetModeMouseAnyEvent + ansi.SetModeMouseExtSgr +
		ansi.SetWindowTitle("title") + ansi.SetModifyOtherKeys2 + ansi.PushKittyKeyboard(3) + ansi.RequestKittyKeyboard +
		ansi.SetCursorColor("#090807") + ansi.SetBackgroundColor("#010203") + ansi.SetCursorStyle(6) + ansi.SetProgressBar(40) +
		ansi.SetProgramStatus(*v.ProgramStatus)
	if got != want {
		t.Fatalf("fresh modes\n got %q\nwant %q", got, want)
	}
}

func TestModeChangesWriteOnlyTheDifference(t *testing.T) {
	a := tea.NewView("")
	a.MouseMode = tea.MouseModeCellMotion
	a.ProgramStatus = &tea.ProgramStatus{State: ansi.ProgramStateWorking, ID: "a"}
	b := a
	b.MouseMode = tea.MouseModeAllMotion
	b.KeyboardEnhancements.ReportAlternateKeys = true
	b.ProgramStatus = &tea.ProgramStatus{State: ansi.ProgramStateDone, ID: "b"}
	ma, mb := modesOf(&a), modesOf(&b)
	got := string(modeDelta(nil, &ma, &mb))
	want := ansi.ResetModeMouseButtonEvent + ansi.SetModeMouseAnyEvent + ansi.SetModeMouseExtSgr + ansi.SetModifyOtherKeys2 +
		ansi.KittyKeyboard(5, 1) + ansi.RequestKittyKeyboard + ansi.ClearProgramStatusID("a") + ansi.SetProgramStatus(*b.ProgramStatus)
	if got != want {
		t.Fatalf("delta\n got %q\nwant %q", got, want)
	}
	if again := modeDelta(nil, &mb, &mb); len(again) != 0 {
		t.Fatalf("unchanged modes wrote %q", again)
	}
}

func TestUndoResetsWhatWasSet(t *testing.T) {
	v := tea.NewView("")
	v.ReportFocus, v.MouseMode, v.WindowTitle = true, tea.MouseModeCellMotion, "t"
	v.ForegroundColor = color.White
	v.Cursor = &tea.Cursor{Shape: tea.CursorUnderline, Blink: true}
	m := modesOf(&v)
	got := string(modeUndo(nil, &m))
	for _, part := range []string{ansi.ResetModeBracketedPaste, ansi.ResetModeFocusEvent, ansi.ResetModeMouseButtonEvent, ansi.SetWindowTitle(""), ansi.SetCursorStyle(0), ansi.ResetForegroundColor} {
		if !strings.Contains(got, part) {
			t.Errorf("undo %q lacks %q", got, part)
		}
	}
	if strings.Contains(got, ansi.ResetBackgroundColor) || strings.Contains(got, ansi.ResetProgressBar) {
		t.Errorf("undo %q resets what was never set", got)
	}
}
