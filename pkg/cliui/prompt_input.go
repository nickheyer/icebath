package cliui

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// What one inline text field asks and how its keys behave
type inputSpec struct {
	title    string
	fallback string
	secret   bool
	mask     string
	cancel   bool
	back     bool
	guardEsc bool
}

// What ended a text field
type inputOutcome int

const (
	inputDone inputOutcome = iota
	inputCancelled
	inputBack
)

// Editing buffer of a text field with emacs style keys and a kill ring of one
type lineEditor struct {
	buf  []rune
	pos  int
	kill string
}

func (e *lineEditor) insert(s string) {
	rs := []rune(s)
	e.buf = append(e.buf[:e.pos], append(rs, e.buf[e.pos:]...)...)
	e.pos += len(rs)
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

func (e *lineEditor) wordStartBefore(p int) int {
	for p > 0 && !isWordRune(e.buf[p-1]) {
		p--
	}
	for p > 0 && isWordRune(e.buf[p-1]) {
		p--
	}
	return p
}

func (e *lineEditor) wordEndAfter(p int) int {
	for p < len(e.buf) && !isWordRune(e.buf[p]) {
		p++
	}
	for p < len(e.buf) && isWordRune(e.buf[p]) {
		p++
	}
	return p
}

func (e *lineEditor) cut(from, to int) {
	e.kill = string(e.buf[from:to])
	e.buf = append(e.buf[:from], e.buf[to:]...)
	e.pos = from
}

// Applies one editing key with prompt_toolkit emacs bindings
func (e *lineEditor) key(k uv.Key) {
	ctrl, alt := k.Mod&uv.ModCtrl != 0, k.Mod&uv.ModAlt != 0
	switch {
	case ctrl && k.Code == 'd':
		if e.pos < len(e.buf) {
			e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
		}
	case (ctrl && k.Code == 'a') || k.Code == uv.KeyHome:
		e.pos = 0
	case (ctrl && k.Code == 'e') || k.Code == uv.KeyEnd:
		e.pos = len(e.buf)
	case (ctrl && k.Code == 'b') || (k.Code == uv.KeyLeft && !ctrl && !alt):
		if e.pos > 0 {
			e.pos--
		}
	case (ctrl && k.Code == 'f') || (k.Code == uv.KeyRight && !ctrl && !alt):
		if e.pos < len(e.buf) {
			e.pos++
		}
	case (alt && k.Code == 'b') || (k.Code == uv.KeyLeft && (ctrl || alt)):
		e.pos = e.wordStartBefore(e.pos)
	case (alt && k.Code == 'f') || (k.Code == uv.KeyRight && (ctrl || alt)):
		e.pos = e.wordEndAfter(e.pos)
	case k.Code == uv.KeyBackspace && alt, ctrl && k.Code == 'w':
		start := e.pos
		if ctrl {
			for start > 0 && unicode.IsSpace(e.buf[start-1]) {
				start--
			}
			for start > 0 && !unicode.IsSpace(e.buf[start-1]) {
				start--
			}
		} else {
			start = e.wordStartBefore(e.pos)
		}
		e.cut(start, e.pos)
	case k.Code == uv.KeyBackspace || (ctrl && k.Code == 'h'):
		if e.pos > 0 {
			e.buf = append(e.buf[:e.pos-1], e.buf[e.pos:]...)
			e.pos--
		}
	case k.Code == uv.KeyDelete:
		if e.pos < len(e.buf) {
			e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
		}
	case alt && k.Code == 'd':
		e.cut(e.pos, e.wordEndAfter(e.pos))
	case ctrl && k.Code == 'k':
		e.cut(e.pos, len(e.buf))
	case ctrl && k.Code == 'u':
		e.cut(0, e.pos)
	case ctrl && k.Code == 'y':
		e.insert(e.kill)
	case ctrl && k.Code == 't':
		if e.pos > 0 && len(e.buf) > 1 {
			p := e.pos
			if p == len(e.buf) {
				p--
			}
			e.buf[p-1], e.buf[p] = e.buf[p], e.buf[p-1]
			e.pos = p + 1
		}
	case k.Text != "" && !ctrl && !alt:
		e.insert(k.Text)
	}
}

// One inline text field, plain or masked, drawn with its own caret
type inputPrompt struct {
	c        *Console
	spec     inputSpec
	ed       lineEditor
	offset   int
	escArmed bool
}

// Cell a rune of the field shows as, the mask for a secret
func (p *inputPrompt) cell(r rune) string {
	if p.spec.secret {
		return p.spec.mask
	}
	if r == '\t' || unicode.IsControl(r) {
		return " "
	}
	return string(r)
}

// Painted field row with the caret, scrolled sideways so the caret stays within the room
func (p *inputPrompt) field(room int) string {
	t := activeTheme()
	caret := func(s string) string {
		if s == "" {
			s = " "
		}
		return p.c.paint(s, textStyle{reverse: true})
	}
	if len(p.ed.buf) == 0 {
		return caret("") + p.c.paint(p.c.clip(p.spec.fallback, room-1), textStyle{color: t.Faint})
	}
	if p.spec.secret && p.spec.mask == "" {
		return caret("")
	}
	if p.ed.pos < p.offset {
		p.offset = p.ed.pos
	}
	width := func(from, to int) int {
		n := 0
		for _, r := range p.ed.buf[from:to] {
			n += ansi.StringWidth(p.cell(r))
		}
		return n
	}
	for p.offset < p.ed.pos && width(p.offset, p.ed.pos) > room-1 {
		p.offset++
	}
	var b strings.Builder
	used := 0
	for i := p.offset; i <= len(p.ed.buf); i++ {
		text := ""
		if i < len(p.ed.buf) {
			text = p.cell(p.ed.buf[i])
		}
		w := max(ansi.StringWidth(text), 1)
		if used+w > room {
			break
		}
		if i == p.ed.pos {
			b.WriteString(caret(text))
		} else {
			b.WriteString(text)
		}
		used += w
	}
	return b.String()
}

// Key hints for the state the field is in
func (p *inputPrompt) hint() string {
	g, t := p.c.glyphs(), activeTheme()
	if p.escArmed {
		return "    " + p.c.paint("press esc again to cancel", textStyle{color: t.Warn})
	}
	empty := len(p.ed.buf) == 0
	parts := []string{"enter confirm"}
	if empty && p.spec.fallback != "" {
		parts = []string{"enter keeps " + p.spec.fallback}
	}
	if p.spec.secret && !empty {
		parts = append(parts, fmt.Sprintf("%d characters", len(p.ed.buf)))
	}
	if p.spec.back && empty {
		parts = append(parts, g.back+" back")
	}
	if p.spec.cancel {
		parts = append(parts, "esc cancel")
	}
	return "    " + p.c.paint(strings.Join(parts, "  "+g.sep+"  "), textStyle{color: t.Faint})
}

// Lines of the field for a terminal of maxRows by cols
func (p *inputPrompt) frame(maxRows, cols int) []string {
	g, t := p.c.glyphs(), activeTheme()
	body := max(min(cols-1, 112)-4, 12)
	lines := p.c.titleLines(p.spec.title, body)
	if len(lines) > maxRows-1 {
		lines = lines[:max(maxRows-1, 1)]
	}
	lines = append(lines, "    "+p.c.paint(g.cursor, textStyle{color: t.Accent, bold: true})+" "+p.field(body-2))
	if len(lines) < maxRows {
		lines = append(lines, p.hint())
	}
	return lines
}

// Answer line a finished field leaves, the text, a masked count, the kept fallback or the cancellation
func (p *inputPrompt) answer(outcome inputOutcome, failed bool) []string {
	g, t := p.c.glyphs(), activeTheme()
	text := string(p.ed.buf)
	switch {
	case outcome == inputBack:
		return nil
	case failed || outcome == inputCancelled:
		return []string{p.c.answerLine(p.spec.title, "cancelled", textStyle{color: t.Faint})}
	case strings.TrimSpace(text) == "" && p.spec.fallback != "":
		return []string{p.c.answerLine(p.spec.title, p.spec.fallback, textStyle{})}
	case strings.TrimSpace(text) == "":
		return []string{p.c.answerLine(p.spec.title, "skipped", textStyle{color: t.Faint})}
	case p.spec.secret:
		shown := strings.Repeat(g.mask, min(len(p.ed.buf), 12))
		return []string{p.c.answerLine(p.spec.title, shown, textStyle{}) + " " +
			p.c.paint(fmt.Sprintf("(%d characters)", len(p.ed.buf)), textStyle{color: t.Faint})}
	}
	return []string{p.c.answerLine(p.spec.title, strings.TrimSpace(text), textStyle{})}
}

// Runs the field until Enter, a navigation key or the end of input, leaving one answer line in its place
func (p *inputPrompt) run() (string, inputOutcome, error) {
	release, err := p.c.holdRaw()
	if err != nil {
		return "", inputDone, errEditorUnavailable
	}
	defer release()
	defer p.c.graceQuiet()
	region := p.c.openRegion()
	defer region.close(nil)
	p.c.enterMode("paste", "\x1b[?2004h", "\x1b[?2004l")
	defer p.c.leaveMode("paste")
	keys := p.c.newKeyReader()
	defer keys.close()
	for {
		_, cols := region.size()
		region.paint(p.frame(region.maxRows(), cols))
		ev, kerr := keys.next()
		if kerr != nil {
			if errors.Is(kerr, ErrEOF) && len(p.ed.buf) > 0 {
				region.close(p.answer(inputDone, false))
				return string(p.ed.buf), inputDone, nil
			}
			region.close(p.answer(inputDone, true))
			return "", inputDone, kerr
		}
		switch e := ev.(type) {
		case pasteEvent:
			p.ed.insert(strings.Join(strings.FieldsFunc(string(e), func(r rune) bool { return r == '\n' }), " "))
		case uv.Key:
			armed := p.escArmed
			p.escArmed = false
			empty := len(p.ed.buf) == 0
			switch {
			case isCtrl(e, 'c'):
				region.close(p.answer(inputDone, true))
				return "", inputDone, ErrInterrupt
			case isCtrl(e, 'd') && empty:
				region.close(p.answer(inputDone, true))
				return "", inputDone, ErrEOF
			case isCtrl(e, 'z'):
				region.paint(nil)
				p.c.suspend()
			case e.Code == uv.KeyEscape:
				if !p.spec.cancel {
					break
				}
				if p.spec.guardEsc && !armed && e.Mod&uv.ModAlt == 0 {
					p.escArmed = true
					break
				}
				region.close(p.answer(inputCancelled, false))
				return "", inputCancelled, nil
			case e.Code == uv.KeyLeft && e.Mod == 0 && empty && p.spec.back:
				region.close(nil)
				return "", inputBack, nil
			case e.Code == uv.KeyEnter || e.Code == uv.KeyKpEnter || isCtrl(e, 'j') || isCtrl(e, 'm'):
				region.close(p.answer(inputDone, false))
				return string(p.ed.buf), inputDone, nil
			default:
				p.ed.key(e)
			}
		}
	}
}
