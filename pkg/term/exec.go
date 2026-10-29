package term

import (
	"github.com/charmbracelet/x/ansi"
)

// Carries out a C0 control character
func (e *Emulator) execute(b byte) {
	e.joiner = false
	switch b {
	case ansi.BEL:
		e.emit(Event{Kind: EventBell})
	case ansi.BS:
		e.backspace()
	case ansi.HT:
		e.tab()
	case ansi.LF, ansi.VT, ansi.FF:
		e.lineFeed()
	case ansi.CR:
		e.carriageReturn()
	case ansi.SO:
		e.scr.cur.gl = 1
	case ansi.SI:
		e.scr.cur.gl = 0
	}
}

// Carries out an escape sequence
func (e *Emulator) esc(cmd ansi.Cmd) {
	e.joiner = false
	final := cmd.Final()
	switch inter := cmd.Intermediate(); inter {
	case 0:
		e.escPlain(final)
	case '#':
		if final == '8' {
			e.alignmentTest()
		}
	case '(', ')', '*', '+':
		e.designate(int(inter-'('), final)
	case '-', '.', '/':
		e.designate(int(inter-'-')+1, final)
	}
}

// Picks the character set a G slot holds, anything but the graphics and UK sets reading as ASCII
func (e *Emulator) designate(slot int, final byte) {
	set := charsetASCII
	switch final {
	case '0':
		set = charsetGraphics
	case 'A':
		set = charsetUK
	}
	e.scr.cur.g[slot] = set
}

// Carries out an escape sequence without an intermediate byte
func (e *Emulator) escPlain(final byte) {
	c := &e.scr.cur
	switch final {
	case '7':
		e.saveCursor()
	case '8':
		e.restoreCursor()
	case 'D':
		e.index()
	case 'E':
		e.index()
		e.carriageReturn()
	case 'H':
		if c.x < len(e.scr.tabs) {
			e.scr.tabs[c.x] = true
		}
	case 'M':
		e.reverseIndex()
	case 'N':
		c.ss = 2
	case 'O':
		c.ss = 3
	case 'Z':
		e.reply(primaryDA)
	case 'c':
		e.reset()
	case '=':
		e.modes.keypadApp = true
	case '>':
		e.modes.keypadApp = false
	case 'n':
		c.gl = 2
	case 'o':
		c.gl = 3
	case '|':
		c.gr = 3
	case '}':
		c.gr = 2
	case '~':
		c.gr = 1
	}
}
