package tty

import (
	"fmt"
	"image/color"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Terminal modes and decorations a view asks for, everything but its cells
type modes struct {
	alt      bool
	focus    bool
	paste    bool
	mouse    tea.MouseMode
	title    string
	kitty    int
	cursor   *tea.Cursor
	fg, bg   color.Color
	progress *tea.ProgressBar
	status   *tea.ProgramStatus
}

// Modes of a view
func modesOf(v *tea.View) modes {
	m := modes{alt: v.AltScreen, focus: v.ReportFocus, paste: !v.DisableBracketedPasteMode, mouse: v.MouseMode, title: v.WindowTitle,
		kitty: kittyFlags(v.KeyboardEnhancements), fg: v.ForegroundColor, bg: v.BackgroundColor}
	if v.Cursor != nil {
		c := *v.Cursor
		m.cursor = &c
	}
	if v.ProgressBar != nil {
		p := *v.ProgressBar
		m.progress = &p
	}
	if v.ProgramStatus != nil {
		p := *v.ProgramStatus
		m.status = &p
	}
	return m
}

// Kitty keyboard flags of the enhancements, disambiguation always on
func kittyFlags(k tea.KeyboardEnhancements) int {
	f := 1
	if k.ReportEventTypes {
		f |= ansi.KittyReportEventTypes
	}
	if k.ReportAlternateKeys {
		f |= ansi.KittyReportAlternateKeys
	}
	if k.ReportAllKeysAsEscapeCodes {
		f |= ansi.KittyReportAllKeysAsEscapeCodes
	}
	if k.ReportAssociatedText {
		f |= ansi.KittyReportAssociatedKeys
	}
	return f
}

// Hex form OSC 10, 11 and 12 take, the way the toolkit's colors print
func hexColor(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}

// DECSCUSR value of a cursor shape and blink
func cursorStyle(c *tea.Cursor) int {
	if c == nil {
		return 0
	}
	s := int(c.Shape)*2 + 1
	if !c.Blink {
		s++
	}
	return s
}

// Sequence reporting a progress bar, its reset for none
func progressSeq(p *tea.ProgressBar) string {
	if p == nil {
		return ansi.ResetProgressBar
	}
	switch p.State {
	case tea.ProgressBarDefault:
		return ansi.SetProgressBar(p.Value)
	case tea.ProgressBarError:
		return ansi.SetErrorProgressBar(p.Value)
	case tea.ProgressBarIndeterminate:
		return ansi.SetIndeterminateProgressBar
	case tea.ProgressBarWarning:
		return ansi.SetWarningProgressBar(p.Value)
	}
	return ansi.ResetProgressBar
}

// Reports whether two optional values hold the same thing
func samePtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Mode sequences that take the terminal from the old modes to the new, with nil old meaning a fresh terminal
func modeDelta(b []byte, old *modes, m *modes) []byte {
	fresh := old == nil
	var o modes
	if !fresh {
		o = *old
	}
	if fresh || o.paste != m.paste {
		if m.paste {
			b = append(b, ansi.SetModeBracketedPaste...)
		} else if !fresh {
			b = append(b, ansi.ResetModeBracketedPaste...)
		}
	}
	if fresh || o.focus != m.focus {
		if m.focus {
			b = append(b, ansi.SetModeFocusEvent...)
		} else if !fresh {
			b = append(b, ansi.ResetModeFocusEvent...)
		}
	}
	if fresh || o.mouse != m.mouse {
		switch m.mouse {
		case tea.MouseModeNone:
			if !fresh && o.mouse != tea.MouseModeNone {
				b = append(b, ansi.ResetModeMouseButtonEvent+ansi.ResetModeMouseAnyEvent+ansi.ResetModeMouseExtSgr...)
			}
		case tea.MouseModeCellMotion:
			if !fresh && o.mouse == tea.MouseModeAllMotion {
				b = append(b, ansi.ResetModeMouseAnyEvent...)
			}
			b = append(b, ansi.SetModeMouseButtonEvent+ansi.SetModeMouseExtSgr...)
		case tea.MouseModeAllMotion:
			if !fresh && o.mouse == tea.MouseModeCellMotion {
				b = append(b, ansi.ResetModeMouseButtonEvent...)
			}
			b = append(b, ansi.SetModeMouseAnyEvent+ansi.SetModeMouseExtSgr...)
		}
	}
	if (fresh && m.title != "") || (!fresh && o.title != m.title) {
		b = append(b, ansi.SetWindowTitle(m.title)...)
	}
	if fresh || o.kitty != m.kitty || o.alt != m.alt {
		b = append(b, ansi.SetModifyOtherKeys2...)
		if fresh || o.alt != m.alt {
			b = append(b, ansi.PushKittyKeyboard(m.kitty)...)
		} else {
			b = append(b, ansi.KittyKeyboard(m.kitty, 1)...)
		}
		b = append(b, ansi.RequestKittyKeyboard...)
	}
	var cc, occ color.Color
	if m.cursor != nil {
		cc = m.cursor.Color
	}
	if o.cursor != nil {
		occ = o.cursor.Color
	}
	for _, c := range [...]struct {
		now, was color.Color
		reset    string
		set      func(string) string
	}{
		{cc, occ, ansi.ResetCursorColor, ansi.SetCursorColor},
		{m.fg, o.fg, ansi.ResetForegroundColor, ansi.SetForegroundColor},
		{m.bg, o.bg, ansi.ResetBackgroundColor, ansi.SetBackgroundColor},
	} {
		if c.now == c.was {
			continue
		}
		if c.now == nil {
			b = append(b, c.reset...)
		} else {
			b = append(b, c.set(hexColor(c.now))...)
		}
	}
	if cs, ocs := cursorStyle(m.cursor), cursorStyle(o.cursor); cs != ocs {
		b = append(b, ansi.SetCursorStyle(cs)...)
	}
	if (fresh && m.progress != nil && m.progress.State != tea.ProgressBarNone) || (!fresh && !samePtr(o.progress, m.progress)) {
		b = append(b, progressSeq(m.progress)...)
	}
	if !samePtr(o.status, m.status) {
		if o.status != nil && (m.status == nil || m.status.ID != o.status.ID) {
			b = append(b, ansi.ClearProgramStatusID(o.status.ID)...)
		}
		if m.status != nil {
			b = append(b, ansi.SetProgramStatus(*m.status)...)
		}
	}
	return b
}

// Sequences that undo the input and decoration modes, leaving the program status as reports outlive the program
func modeUndo(b []byte, m *modes) []byte {
	if m.paste {
		b = append(b, ansi.ResetModeBracketedPaste...)
	}
	if m.focus {
		b = append(b, ansi.ResetModeFocusEvent...)
	}
	if m.mouse != tea.MouseModeNone {
		b = append(b, ansi.ResetModeMouseButtonEvent+ansi.ResetModeMouseAnyEvent+ansi.ResetModeMouseExtSgr...)
	}
	if m.title != "" {
		b = append(b, ansi.SetWindowTitle("")...)
	}
	if m.cursor != nil {
		if cursorStyle(m.cursor) > 1 {
			b = append(b, ansi.SetCursorStyle(0)...)
		}
		if m.cursor.Color != nil {
			b = append(b, ansi.ResetCursorColor...)
		}
	}
	if m.bg != nil {
		b = append(b, ansi.ResetBackgroundColor...)
	}
	if m.fg != nil {
		b = append(b, ansi.ResetForegroundColor...)
	}
	if m.progress != nil && m.progress.State != tea.ProgressBarNone {
		b = append(b, ansi.ResetProgressBar...)
	}
	return b
}
