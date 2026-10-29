package term

import (
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// What the pointer did
type MouseKind int

const (
	MousePress MouseKind = iota
	MouseRelease
	MouseMotion
	MouseWheel
)

// Pointer event in cell coordinates inside the grid
type Mouse struct {
	X, Y   int
	Button uv.MouseButton
	Mod    uv.KeyMod
	Kind   MouseKind
}

// Bytes the program receives for the pointer event, false when it does not track it and the host keeps the event
func (e *Emulator) Mouse(m Mouse) ([]byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	md := e.modes
	if m.Mod&uv.ModShift != 0 && !md.x10Mouse {
		return nil, false
	}
	wheel := m.Kind == MouseWheel
	switch {
	case md.x10Mouse:
		if m.Kind != MousePress || m.Button < uv.MouseLeft || m.Button > uv.MouseRight {
			return nil, false
		}
		return []byte(ansi.MouseX10(ansi.EncodeMouseButton(m.Button, false, false, false, false), min(m.X, 222), min(m.Y, 222))), true
	case md.mouseAny:
	case md.mouseButton:
		if m.Kind == MouseMotion && m.Button == uv.MouseNone {
			return nil, false
		}
	case md.mouseNormal:
		if m.Kind == MouseMotion {
			return nil, false
		}
	default:
		return nil, false
	}
	if wheel && (m.Button < uv.MouseWheelUp || m.Button > uv.MouseWheelRight) {
		return nil, false
	}
	shift, alt, ctrl := m.Mod&uv.ModShift != 0, m.Mod&(uv.ModAlt|uv.ModMeta) != 0, m.Mod&uv.ModCtrl != 0
	button := m.Button
	release := m.Kind == MouseRelease
	if release && !md.mouseSGR {
		button = uv.MouseNone
	}
	b := ansi.EncodeMouseButton(button, m.Kind == MouseMotion, shift, alt, ctrl)
	if b == 0xff {
		return nil, false
	}
	x, y := m.X, m.Y
	switch {
	case md.mouseSGR && md.mousePixels:
		return []byte(ansi.MouseSgr(b, x*cellPixelW+cellPixelW/2, y*cellPixelH+cellPixelH/2, release)), true
	case md.mouseSGR:
		return []byte(ansi.MouseSgr(b, x, y, release)), true
	case md.mouseURXVT:
		return []byte("\x1b[" + itoa(int(b)+32) + ";" + itoa(x+1) + ";" + itoa(y+1) + "M"), true
	case md.mouseUTF8:
		out := []byte("\x1b[M")
		out = utf8.AppendRune(out, rune(int(b)+32))
		out = utf8.AppendRune(out, rune(min(x, 2015)+33))
		out = utf8.AppendRune(out, rune(min(y, 2015)+33))
		return out, true
	}
	return []byte(ansi.MouseX10(b, min(x, 222), min(y, 222))), true
}

// Reports whether a wheel turn should reach the program as arrow keys, the alternate scroll of xterm
func (e *Emulator) AltScroll() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.modes.alt && e.modes.altScroll && !e.tracking()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
