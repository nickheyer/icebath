package term

import (
	"encoding/base64"
	"fmt"
	"image/color"
	"net/url"
	"strconv"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
)

// Carries out an operating system command
func (e *Emulator) osc(cmd int, data []byte) {
	e.joiner = false
	text := string(data)
	if i := strings.IndexByte(text, ';'); i >= 0 {
		text = text[i+1:]
	} else {
		text = ""
	}
	switch cmd {
	case 0:
		e.icon, e.title = text, text
		e.emit(Event{Kind: EventTitle, Text: text})
	case 1:
		e.icon = text
	case 2:
		e.title = text
		e.emit(Event{Kind: EventTitle, Text: text})
	case 4:
		e.paletteOp(text)
	case 7:
		e.setCwd(text)
	case 8:
		e.hyperlink(text)
	case 9:
		e.notifyOp(text)
	case 10, 11, 12:
		e.dynamicColor(cmd, text)
	case 52:
		e.clipboardOp(text)
	case 104:
		e.resetPalette(text)
	case 110:
		e.fg = nil
	case 111:
		e.bg = nil
	case 112:
		e.cursorColor = nil
	case 133:
		e.shellMark(text)
	case 777:
		e.notify777(text)
	}
}

// Sets or answers palette entries given as pairs of an index and a color
func (e *Emulator) paletteOp(text string) {
	parts := strings.Split(text, ";")
	for i := 0; i+1 < len(parts); i += 2 {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 || n > 255 {
			continue
		}
		if parts[i+1] == "?" {
			c := e.palette[n]
			if c == nil {
				c = indexedRGB(n)
			}
			e.reply(fmt.Sprintf("\x1b]4;%d;%s\x1b\\", n, xcolor(c)))
			continue
		}
		if c, ok := parseColor(parts[i+1]); ok {
			e.palette[n] = c
		}
	}
}

// Returns palette entries to their defaults, every one when none are named
func (e *Emulator) resetPalette(text string) {
	if strings.TrimSpace(text) == "" {
		e.palette = [256]color.Color{}
		return
	}
	for _, part := range strings.Split(text, ";") {
		if n, err := strconv.Atoi(part); err == nil && n >= 0 && n < 256 {
			e.palette[n] = nil
		}
	}
}

// Sets or answers the default text, background or cursor color
func (e *Emulator) dynamicColor(cmd int, text string) {
	for i, part := range strings.Split(text, ";") {
		at := cmd + i
		if at > 12 {
			return
		}
		slot := map[int]*color.Color{10: &e.fg, 11: &e.bg, 12: &e.cursorColor}[at]
		fallback := map[int]color.Color{10: e.opts.Fg, 11: e.opts.Bg, 12: e.opts.Cursor}[at]
		if part == "?" {
			c := *slot
			if c == nil {
				c = fallback
			}
			if c == nil {
				continue
			}
			e.reply(fmt.Sprintf("\x1b]%d;%s\x1b\\", at, xcolor(c)))
			continue
		}
		if c, ok := parseColor(part); ok {
			*slot = c
		}
	}
}

// Remembers the working directory a program announced as a file URL
func (e *Emulator) setCwd(text string) {
	u, err := url.Parse(strings.TrimSpace(text))
	if err != nil || (u.Scheme != "file" && u.Scheme != "") {
		return
	}
	path := u.Path
	if path == "" {
		return
	}
	e.cwd, e.host = path, u.Host
	e.emit(Event{Kind: EventCwd, Text: path})
}

// Starts or ends a hyperlink on the pen
func (e *Emulator) hyperlink(text string) {
	params, uri, _ := strings.Cut(text, ";")
	if uri == "" {
		e.scr.cur.link = uv.Link{}
		return
	}
	e.scr.cur.link = uv.Link{URL: uri, Params: params}
}

// Shows a notification or progress a program sent through OSC 9
func (e *Emulator) notifyOp(text string) {
	if kind, rest, ok := strings.Cut(text, ";"); ok && kind == "4" {
		state, value, _ := strings.Cut(rest, ";")
		st, _ := strconv.Atoi(state)
		v, _ := strconv.Atoi(value)
		e.progress.state, e.progress.value = min(max(st, 0), 4), min(max(v, 0), 100)
		e.emit(Event{Kind: EventProgress})
		return
	}
	if strings.TrimSpace(text) != "" {
		e.emit(Event{Kind: EventNotify, Text: text})
	}
}

// Shows a notification sent the rxvt way, the word notify then a title and a body
func (e *Emulator) notify777(text string) {
	kind, rest, _ := strings.Cut(text, ";")
	if kind != "notify" {
		return
	}
	title, body, _ := strings.Cut(rest, ";")
	e.emit(Event{Kind: EventNotify, Text: title, Body: body})
}

// Writes the clipboard from base64 text, or answers a query with nothing
func (e *Emulator) clipboardOp(text string) {
	target, payload, _ := strings.Cut(text, ";")
	if target == "" {
		target = "c"
	}
	if payload == "?" {
		e.reply("\x1b]52;" + target + ";\x1b\\")
		return
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return
	}
	e.emit(Event{Kind: EventClipboard, Text: string(data), Body: target})
}

// Records a shell integration mark on the cursor row
func (e *Emulator) shellMark(text string) {
	kind, rest, _ := strings.Cut(text, ";")
	l := e.scr.line(e.scr.cur.y)
	switch kind {
	case "A":
		l.Marks |= MarkPrompt
	case "B":
		l.Marks |= MarkInput
	case "C":
		l.Marks |= MarkOutput
	case "D":
		l.Marks |= MarkEnd
		code, _, _ := strings.Cut(rest, ";")
		if n, err := strconv.Atoi(strings.TrimSpace(code)); err == nil {
			l.Exit = n
		}
	}
}

// Color in the X11 rgb, hex triplet or named spellings programs send
func parseColor(s string) (color.Color, bool) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "rgb:"):
		parts := strings.Split(s[4:], "/")
		if len(parts) != 3 {
			return nil, false
		}
		var ch [3]uint8
		for i, p := range parts {
			v, err := strconv.ParseUint(p, 16, 16)
			if err != nil || len(p) == 0 || len(p) > 4 {
				return nil, false
			}
			ch[i] = uint8(v >> (4 * (len(p) - 2)) & 0xff)
			if len(p) == 1 {
				ch[i] = uint8(v * 17)
			}
		}
		return color.RGBA{R: ch[0], G: ch[1], B: ch[2], A: 0xff}, true
	case strings.HasPrefix(s, "#"):
		hex := s[1:]
		if len(hex) != 6 && len(hex) != 3 && len(hex) != 9 && len(hex) != 12 {
			return nil, false
		}
		per := len(hex) / 3
		var ch [3]uint8
		for i := range ch {
			part := hex[i*per : (i+1)*per]
			v, err := strconv.ParseUint(part, 16, 16)
			if err != nil {
				return nil, false
			}
			if per == 1 {
				ch[i] = uint8(v * 17)
			} else {
				ch[i] = uint8(v >> (4 * (per - 2)))
			}
		}
		return color.RGBA{R: ch[0], G: ch[1], B: ch[2], A: 0xff}, true
	}
	if name, ok := colorNames[strings.ToLower(s)]; ok {
		return name, true
	}
	return nil, false
}

// Color as the X11 rgb answer with four hex digits per channel
func xcolor(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("rgb:%04x/%04x/%04x", r, g, b)
}

// Default color of a palette index, the xterm values
func indexedRGB(n int) color.Color {
	if n < 16 {
		return basicRGB[n]
	}
	if n < 232 {
		n -= 16
		levels := [6]uint8{0, 95, 135, 175, 215, 255}
		return color.RGBA{R: levels[n/36], G: levels[n/6%6], B: levels[n%6], A: 0xff}
	}
	v := uint8(8 + (n-232)*10)
	return color.RGBA{R: v, G: v, B: v, A: 0xff}
}

var basicRGB = [16]color.Color{
	color.RGBA{0, 0, 0, 0xff}, color.RGBA{0xcd, 0, 0, 0xff}, color.RGBA{0, 0xcd, 0, 0xff}, color.RGBA{0xcd, 0xcd, 0, 0xff},
	color.RGBA{0, 0, 0xee, 0xff}, color.RGBA{0xcd, 0, 0xcd, 0xff}, color.RGBA{0, 0xcd, 0xcd, 0xff}, color.RGBA{0xe5, 0xe5, 0xe5, 0xff},
	color.RGBA{0x7f, 0x7f, 0x7f, 0xff}, color.RGBA{0xff, 0, 0, 0xff}, color.RGBA{0, 0xff, 0, 0xff}, color.RGBA{0xff, 0xff, 0, 0xff},
	color.RGBA{0x5c, 0x5c, 0xff, 0xff}, color.RGBA{0xff, 0, 0xff, 0xff}, color.RGBA{0, 0xff, 0xff, 0xff}, color.RGBA{0xff, 0xff, 0xff, 0xff},
}

// X11 color names programs may set colors by
var colorNames = map[string]color.Color{
	"black": basicRGB[0], "red": color.RGBA{0xff, 0, 0, 0xff}, "green": color.RGBA{0, 0x80, 0, 0xff}, "yellow": color.RGBA{0xff, 0xff, 0, 0xff},
	"blue": color.RGBA{0, 0, 0xff, 0xff}, "magenta": color.RGBA{0xff, 0, 0xff, 0xff}, "cyan": color.RGBA{0, 0xff, 0xff, 0xff}, "white": basicRGB[15],
	"gray": color.RGBA{0xbe, 0xbe, 0xbe, 0xff}, "grey": color.RGBA{0xbe, 0xbe, 0xbe, 0xff}, "orange": color.RGBA{0xff, 0xa5, 0, 0xff},
	"purple": color.RGBA{0xa0, 0x20, 0xf0, 0xff}, "brown": color.RGBA{0xa5, 0x2a, 0x2a, 0xff}, "pink": color.RGBA{0xff, 0xc0, 0xcb, 0xff},
}
