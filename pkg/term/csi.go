package term

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Answer to a primary device attributes request, a VT220 with columns, selective erase and color
const primaryDA = "\x1b[?62;1;6;9;15;22c"

// Answer to a secondary device attributes request, an xterm of a version programs trust for modern features
const secondaryDA = "\x1b[>41;380;0c"

// Answer to a tertiary device attributes request, a unit with no serial
const tertiaryDA = "\x1bP!|00000000\x1b\\"

// Kitty keyboard flag stack depth a screen keeps, the oldest dropped beyond it
const kittyDepth = 16

// Pixel size a cell is reported as by window and mouse reports
const (
	cellPixelW = 8
	cellPixelH = 16
)

// Carries out a control sequence
func (e *Emulator) csi(cmd ansi.Cmd, params ansi.Params) {
	e.joiner = false
	first := func(def int) int {
		v, _, _ := params.Param(0, def)
		return v
	}
	count := func() int { return max(first(1), 1) }
	prefix, inter, final := cmd.Prefix(), cmd.Intermediate(), cmd.Final()
	switch {
	case prefix == '?':
		e.csiPrivate(inter, final, params)
		return
	case prefix == '>':
		e.csiGreater(inter, final, params)
		return
	case prefix == '=':
		e.csiEquals(inter, final, params)
		return
	case prefix == '<':
		if final == 'u' {
			e.popKitty(first(1))
		}
		return
	case inter != 0:
		e.csiIntermediate(inter, final, params)
		return
	}
	s := e.scr
	c := &s.cur
	switch final {
	case '@':
		e.insertCells(count())
	case 'A':
		e.moveBy(0, -count())
	case 'B', 'e':
		e.moveBy(0, count())
	case 'C', 'a':
		e.moveBy(count(), 0)
	case 'D':
		e.moveBy(-count(), 0)
	case 'E':
		e.moveBy(0, count())
		e.carriageReturn()
	case 'F':
		e.moveBy(0, -count())
		e.carriageReturn()
	case 'G', '`':
		e.moveTo(first(1)-1, e.relRow(c.y))
	case 'H', 'f':
		col, _, _ := params.Param(1, 1)
		e.moveTo(col-1, first(1)-1)
	case 'I':
		for i := 0; i < count(); i++ {
			e.tab()
		}
	case 'J':
		e.eraseDisplay(first(0), false)
	case 'K':
		e.eraseLine(first(0), false)
	case 'L':
		e.insertLines(count())
	case 'M':
		e.deleteLines(count())
	case 'P':
		e.deleteCells(count())
	case 'S':
		e.scrollRegionUp(count())
	case 'T', '^':
		e.scrollRegionDown(count())
	case 'X':
		e.eraseCells(count())
	case 'Z':
		for i := 0; i < count(); i++ {
			c.x = s.prevTab(c.x)
		}
		c.wrap = false
	case 'b':
		e.repeat(count())
	case 'c':
		if first(0) == 0 {
			e.reply(primaryDA)
		}
	case 'd':
		e.moveTo(e.relCol(c.x), first(1)-1)
	case 'g':
		e.clearTabs(first(0))
	case 'h':
		e.setModes(params, true)
	case 'l':
		e.setModes(params, false)
	case 'm':
		e.sgr(params)
	case 'n':
		e.deviceStatus(first(0))
	case 'r':
		e.setScrollRegion(first(1), paramOr(params, 1, e.h))
	case 's':
		if e.modes.margins {
			e.setMargins(first(1), paramOr(params, 1, e.w))
		} else {
			e.saveCursor()
		}
	case 't':
		e.windowOp(params)
	case 'u':
		e.restoreCursor()
	case 'x':
		if first(0) <= 1 {
			e.reply(fmt.Sprintf("\x1b[%d;1;1;128;128;1;0x", first(0)+2))
		}
	}
}

// Parameter i, or def when it is missing or zero
func paramOr(params ansi.Params, i, def int) int {
	v, _, ok := params.Param(i, def)
	if !ok || v == 0 {
		return def
	}
	return v
}

// Row relative to the origin the cursor moves against
func (e *Emulator) relRow(y int) int {
	if e.scr.cur.origin {
		return y - e.scr.top
	}
	return y
}

// Column relative to the origin the cursor moves against
func (e *Emulator) relCol(x int) int {
	if e.scr.cur.origin && e.modes.margins {
		return x - e.scr.left
	}
	return x
}

// Prints the last printed grapheme n more times
func (e *Emulator) repeat(n int) {
	cell := e.lastCell()
	if cell == nil {
		return
	}
	content := cell.Content
	for i := 0; i < n; i++ {
		for _, r := range content {
			e.print(r)
		}
	}
}

// Clears the tab stop at the cursor for 0 and every tab stop for 3
func (e *Emulator) clearTabs(mode int) {
	s := e.scr
	switch mode {
	case 0:
		if s.cur.x < len(s.tabs) {
			s.tabs[s.cur.x] = false
		}
	case 3:
		for i := range s.tabs {
			s.tabs[i] = false
		}
	}
}

// Sets the scroll region from top to bottom, one based, and homes the cursor
func (e *Emulator) setScrollRegion(top, bot int) {
	s := e.scr
	top, bot = min(max(top, 1), e.h), min(max(bot, 1), e.h)
	if top >= bot {
		return
	}
	s.top, s.bot = top-1, bot-1
	e.moveTo(0, 0)
}

// Sets the left and right margins, one based, and homes the cursor
func (e *Emulator) setMargins(left, right int) {
	s := e.scr
	left, right = min(max(left, 1), e.w), min(max(right, 1), e.w)
	if left >= right {
		return
	}
	s.left, s.right = left-1, right-1
	e.moveTo(0, 0)
}

// Answers a device status report request
func (e *Emulator) deviceStatus(kind int) {
	switch kind {
	case 5:
		e.reply("\x1b[0n")
	case 6:
		c := e.scr.cur
		e.reply(fmt.Sprintf("\x1b[%d;%dR", e.relRow(c.y)+1, e.relCol(c.x)+1))
	}
}

// Carries out a window operation, answering the size and title queries and keeping the title stack
func (e *Emulator) windowOp(params ansi.Params) {
	op, _, _ := params.Param(0, 0)
	arg, _, _ := params.Param(1, 0)
	switch op {
	case 11:
		e.reply("\x1b[1t")
	case 13:
		e.reply("\x1b[3;0;0t")
	case 14:
		e.reply(fmt.Sprintf("\x1b[4;%d;%dt", e.h*cellPixelH, e.w*cellPixelW))
	case 15:
		e.reply(fmt.Sprintf("\x1b[5;%d;%dt", e.h*cellPixelH, e.w*cellPixelW))
	case 16:
		e.reply(fmt.Sprintf("\x1b[6;%d;%dt", cellPixelH, cellPixelW))
	case 18, 19:
		e.reply(fmt.Sprintf("\x1b[%d;%d;%dt", op-10, e.h, e.w))
	case 20:
		e.reply("\x1b]L" + e.icon + "\x1b\\")
	case 21:
		e.reply("\x1b]l" + e.title + "\x1b\\")
	case 22:
		if arg == 0 || arg == 1 {
			e.icons = append(e.icons, e.icon)
		}
		if arg == 0 || arg == 2 {
			e.titles = append(e.titles, e.title)
		}
	case 23:
		if (arg == 0 || arg == 1) && len(e.icons) > 0 {
			e.icon, e.icons = e.icons[len(e.icons)-1], e.icons[:len(e.icons)-1]
		}
		if (arg == 0 || arg == 2) && len(e.titles) > 0 {
			e.title, e.titles = e.titles[len(e.titles)-1], e.titles[:len(e.titles)-1]
			e.emit(Event{Kind: EventTitle, Text: e.title})
		}
	}
}

// Carries out a control sequence with an intermediate byte
func (e *Emulator) csiIntermediate(inter, final byte, params ansi.Params) {
	first, _, _ := params.Param(0, 0)
	switch {
	case inter == ' ' && final == 'q':
		e.setCursorStyle(first)
	case inter == '!' && final == 'p':
		e.softReset()
	case inter == '$' && final == 'p':
		e.reportMode(first, false)
	case inter == '"' && final == 'q':
		e.protect = first == 1
	case inter == '\'' && final == '}':
		e.insertColumns(max(first, 1))
	case inter == '\'' && final == '~':
		e.deleteColumns(max(first, 1))
	case inter == '#' && (final == 'p' || final == '{'):
		e.scr.pens = append(e.scr.pens, e.scr.cur.pen)
		if len(e.scr.pens) > 10 {
			e.scr.pens = e.scr.pens[1:]
		}
	case inter == '#' && (final == 'q' || final == '}'):
		if n := len(e.scr.pens); n > 0 {
			e.scr.cur.pen, e.scr.pens = e.scr.pens[n-1], e.scr.pens[:n-1]
		}
	case inter == '*' && final == 'y':
		e.checksum(params)
	}
}

// Sets the cursor shape and blink from DECSCUSR
func (e *Emulator) setCursorStyle(n int) {
	switch n {
	case 0, 1:
		e.shape, e.modes.blinkCursor = CursorBlock, true
	case 2:
		e.shape, e.modes.blinkCursor = CursorBlock, false
	case 3:
		e.shape, e.modes.blinkCursor = CursorUnderline, true
	case 4:
		e.shape, e.modes.blinkCursor = CursorUnderline, false
	case 5:
		e.shape, e.modes.blinkCursor = CursorBar, true
	case 6:
		e.shape, e.modes.blinkCursor = CursorBar, false
	}
}

// Returns modes, margins, the pen and the cursor to their defaults without touching the screen
func (e *Emulator) softReset() {
	s := e.scr
	e.modes.insert, e.modes.cursorKeys, e.modes.keypadApp, e.modes.newline = false, false, false, false
	e.modes.autowrap, e.modes.showCursor, e.modes.margins = true, true, false
	e.modes.reverse = false
	s.top, s.bot = 0, e.h-1
	s.left, s.right = 0, e.w-1
	s.cur.pen = uv.Style{}
	s.cur.origin, s.cur.wrap, s.cur.ss = false, false, 0
	s.cur.g = [4]charset{charsetASCII, charsetASCII, charsetASCII, charsetASCII}
	s.cur.gl, s.cur.gr = 0, 2
	s.saved, s.kept = s.cur, false
	e.protect = false
	e.shape = CursorBlock
	e.modes.blinkCursor = true
}

// Answers a checksum request for a rectangle with the sum of its characters, as DECRQCRA asks
func (e *Emulator) checksum(params ansi.Params) {
	id, _, _ := params.Param(0, 0)
	top := max(paramOr(params, 2, 1), 1)
	left := max(paramOr(params, 3, 1), 1)
	bot := min(paramOr(params, 4, e.h), e.h)
	right := min(paramOr(params, 5, e.w), e.w)
	var sum uint16
	for y := top - 1; y < bot; y++ {
		for x := left - 1; x < right; x++ {
			c := e.scr.line(y).Cells[x]
			if c.Width == 0 {
				continue
			}
			for _, r := range c.Content {
				sum += uint16(r)
			}
		}
	}
	e.reply(fmt.Sprintf("\x1bP%d!~%04X\x1b\\", id, -sum))
}

// Carries out a control sequence with the ? prefix, the DEC private modes and their reports
func (e *Emulator) csiPrivate(inter, final byte, params ansi.Params) {
	first, _, _ := params.Param(0, 0)
	switch {
	case inter == '$' && final == 'p':
		e.reportMode(first, true)
	case inter != 0:
		return
	}
	switch final {
	case 'h':
		e.setPrivateModes(params, true)
	case 'l':
		e.setPrivateModes(params, false)
	case 'J':
		e.eraseDisplay(first, true)
	case 'K':
		e.eraseLine(first, true)
	case 'n':
		e.privateStatus(first)
	case 'u':
		e.reply(fmt.Sprintf("\x1b[?%du", e.kittyFlags()))
	case 'W':
		if first == 5 {
			e.scr.tabs = defaultTabs(e.w)
		}
	case 'm':
		if first == 4 {
			e.reply(fmt.Sprintf("\x1b[>4;%dm", e.modifyKeys))
		}
	}
}

// Answers a DEC private device status request
func (e *Emulator) privateStatus(kind int) {
	switch kind {
	case 6:
		c := e.scr.cur
		e.reply(fmt.Sprintf("\x1b[?%d;%d;1R", e.relRow(c.y)+1, e.relCol(c.x)+1))
	case 15:
		e.reply("\x1b[?13n")
	case 25:
		e.reply("\x1b[?20n")
	case 26:
		e.reply("\x1b[?27;1;0;0n")
	case 53, 55:
		e.reply("\x1b[?53n")
	case 56:
		e.reply("\x1b[?57;1n")
	case 62:
		e.reply("\x1b[0*{")
	case 63:
		e.reply("\x1bP0!~0000\x1b\\")
	case 75:
		e.reply("\x1b[?70n")
	case 85:
		e.reply("\x1b[?83n")
	}
}

// Carries out a control sequence with the > prefix, the xterm key modifier settings, version query and kitty push
func (e *Emulator) csiGreater(inter, final byte, params ansi.Params) {
	first, _, _ := params.Param(0, 0)
	if inter != 0 {
		return
	}
	switch final {
	case 'c':
		e.reply(secondaryDA)
	case 'm':
		if first != 4 {
			return
		}
		level, _, ok := params.Param(1, 0)
		if !ok {
			level = 0
		}
		e.modifyKeys = min(max(level, 0), 2)
	case 'n':
		if first == 4 {
			e.modifyKeys = 0
		}
	case 'q':
		if first == 0 {
			name := e.opts.Version
			if name == "" {
				name = "icebath"
			}
			e.reply("\x1bP>|" + name + "\x1b\\")
		}
	case 'u':
		e.pushKitty(first)
	}
}

// Carries out a control sequence with the = prefix, the tertiary attributes and the kitty flag set
func (e *Emulator) csiEquals(inter, final byte, params ansi.Params) {
	first, _, _ := params.Param(0, 0)
	if inter != 0 {
		return
	}
	switch final {
	case 'c':
		if first == 0 {
			e.reply(tertiaryDA)
		}
	case 'u':
		mode, _, _ := params.Param(1, 1)
		e.setKitty(first, mode)
	}
}

// Pushes kitty keyboard flags on the shown screen
func (e *Emulator) pushKitty(flags int) {
	s := e.scr
	s.kitty = append(s.kitty, flags&ansi.KittyAllFlags)
	if len(s.kitty) > kittyDepth {
		s.kitty = s.kitty[1:]
	}
}

// Pops n kitty keyboard flag entries off the shown screen
func (e *Emulator) popKitty(n int) {
	s := e.scr
	n = max(n, 1)
	if n >= len(s.kitty) {
		s.kitty = nil
		return
	}
	s.kitty = s.kitty[:len(s.kitty)-n]
}

// Replaces, adds or removes kitty keyboard flags on the top of the stack
func (e *Emulator) setKitty(flags, mode int) {
	s := e.scr
	flags &= ansi.KittyAllFlags
	if len(s.kitty) == 0 {
		s.kitty = []int{0}
	}
	top := &s.kitty[len(s.kitty)-1]
	switch mode {
	case 2:
		*top |= flags
	case 3:
		*top &^= flags
	default:
		*top = flags
	}
}

// Sets or resets ANSI modes
func (e *Emulator) setModes(params ansi.Params, on bool) {
	params.ForEach(0, func(_, mode int, _ bool) {
		switch mode {
		case 2:
			e.modes.lockKeyboard = on
		case 4:
			e.modes.insert = on
		case 12:
			e.modes.noEcho = on
		case 20:
			e.modes.newline = on
		}
	})
}

// Sets or resets DEC private modes
func (e *Emulator) setPrivateModes(params ansi.Params, on bool) {
	params.ForEach(0, func(_, mode int, _ bool) {
		e.setPrivateMode(mode, on)
	})
}

func (e *Emulator) setPrivateMode(mode int, on bool) {
	m := &e.modes
	switch mode {
	case 1:
		m.cursorKeys = on
	case 3:
		if !m.allowWide {
			return
		}
		m.wide = on
		e.eraseDisplay(2, false)
		e.scr.top, e.scr.bot = 0, e.h-1
		e.scr.left, e.scr.right = 0, e.w-1
		e.moveTo(0, 0)
	case 5:
		m.reverse = on
	case 6:
		e.scr.cur.origin = on
		e.moveTo(0, 0)
	case 7:
		m.autowrap = on
		e.scr.cur.wrap = false
	case 9:
		m.x10Mouse = on
	case 12:
		m.blinkCursor = on
	case 25:
		m.showCursor = on
	case 40:
		m.allowWide = on
	case 45:
		m.reverseWrap = on
	case 47:
		e.switchScreen(on, false, false)
	case 66:
		m.keypadApp = on
	case 67:
		m.backspaceBS = on
	case 69:
		m.margins = on
		e.scr.left, e.scr.right = 0, e.w-1
	case 1000, 1001:
		m.mouseNormal = on
	case 1002:
		m.mouseButton = on
	case 1003:
		m.mouseAny = on
	case 1004:
		m.focus = on
	case 1005:
		m.mouseUTF8 = on
	case 1006:
		m.mouseSGR = on
	case 1007:
		m.altScroll = on
	case 1015:
		m.mouseURXVT = on
	case 1016:
		m.mousePixels = on
	case 1042:
		m.urgentBell = on
	case 1047:
		e.switchScreen(on, true, false)
	case 1048:
		if on {
			e.saveCursor()
		} else {
			e.restoreCursor()
		}
	case 1049:
		e.switchScreen(on, true, true)
	case 2004:
		m.bracketedPaste = on
	case 2026:
		m.sync = on
		if on {
			e.syncAt = time.Now()
		}
	}
}

// Answers a mode request with whether the mode is set, reset, fixed or unknown
func (e *Emulator) reportMode(mode int, private bool) {
	value := e.modeValue(mode, private)
	if private {
		e.reply(fmt.Sprintf("\x1b[?%d;%d$y", mode, value))
		return
	}
	e.reply(fmt.Sprintf("\x1b[%d;%d$y", mode, value))
}

// DECRPM value of a mode, 1 set, 2 reset, 3 always set, 4 always reset and 0 unknown
func (e *Emulator) modeValue(mode int, private bool) int {
	flag := func(on bool) int {
		if on {
			return 1
		}
		return 2
	}
	m := e.modes
	if !private {
		switch mode {
		case 2:
			return flag(m.lockKeyboard)
		case 4:
			return flag(m.insert)
		case 12:
			return flag(m.noEcho)
		case 20:
			return flag(m.newline)
		}
		return 0
	}
	switch mode {
	case 1:
		return flag(m.cursorKeys)
	case 2:
		return 3
	case 3:
		return flag(m.wide)
	case 4, 8:
		return 4
	case 5:
		return flag(m.reverse)
	case 6:
		return flag(e.scr.cur.origin)
	case 7:
		return flag(m.autowrap)
	case 9:
		return flag(m.x10Mouse)
	case 12:
		return flag(m.blinkCursor)
	case 25:
		return flag(m.showCursor)
	case 40:
		return flag(m.allowWide)
	case 45:
		return flag(m.reverseWrap)
	case 47, 1047, 1049:
		return flag(m.alt)
	case 66:
		return flag(m.keypadApp)
	case 67:
		return flag(m.backspaceBS)
	case 69:
		return flag(m.margins)
	case 1000, 1001:
		return flag(m.mouseNormal)
	case 1002:
		return flag(m.mouseButton)
	case 1003:
		return flag(m.mouseAny)
	case 1004:
		return flag(m.focus)
	case 1005:
		return flag(m.mouseUTF8)
	case 1006:
		return flag(m.mouseSGR)
	case 1007:
		return flag(m.altScroll)
	case 1015:
		return flag(m.mouseURXVT)
	case 1016:
		return flag(m.mousePixels)
	case 1042:
		return flag(m.urgentBell)
	case 1048:
		return flag(e.scr.kept)
	case 2004:
		return flag(m.bracketedPaste)
	case 2026:
		return flag(m.sync)
	case 2027:
		return 3
	}
	return 0
}

// Hex digits of a string, as XTGETTCAP names and values travel
func hexOf(s string) string {
	var sb strings.Builder
	for _, b := range []byte(s) {
		sb.WriteString(strconv.FormatUint(uint64(b)>>4, 16))
		sb.WriteString(strconv.FormatUint(uint64(b)&0xf, 16))
	}
	return strings.ToUpper(sb.String())
}
