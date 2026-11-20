// Terminal output stage that turns Bubble Tea views into the fewest bytes a measured terminal needs
package tty

import (
	"image/color"
	"strconv"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// Color packed as a kind byte over a palette index or 24 bit RGB, zero for the default
type Color uint32

const (
	kindBasic   Color = 1 << 24
	kindIndexed Color = 2 << 24
	kindRGB     Color = 3 << 24
	kindMask    Color = 0xff << 24
)

// Basic palette entry 0 to 15
func Basic(n uint8) Color { return kindBasic | Color(n&15) }

// Entry of the 256 color palette
func Indexed(n uint8) Color { return kindIndexed | Color(n) }

// Direct 24 bit color
func RGB(r, g, b uint8) Color { return kindRGB | Color(r)<<16 | Color(g)<<8 | Color(b) }

// Packed form of any color, keeping basic and indexed colors as palette references
func ColorOf(c color.Color) Color {
	switch v := c.(type) {
	case nil:
		return 0
	case ansi.BasicColor:
		return Basic(uint8(v))
	case ansi.IndexedColor:
		return Indexed(uint8(v))
	}
	r, g, b, _ := c.RGBA()
	return RGB(uint8(r>>8), uint8(g>>8), uint8(b>>8))
}

// Standard library form of the color, nil for the default
func (c Color) Std() color.Color {
	switch c & kindMask {
	case kindBasic:
		return ansi.BasicColor(c & 0xff)
	case kindIndexed:
		return ansi.IndexedColor(c & 0xff)
	case kindRGB:
		return color.RGBA{R: uint8(c >> 16), G: uint8(c >> 8), B: uint8(c), A: 0xff}
	}
	return nil
}

// Text attributes a cell carries
const (
	AttrBold uint8 = 1 << iota
	AttrFaint
	AttrItalic
	AttrBlink
	AttrRapidBlink
	AttrReverse
	AttrConceal
	AttrStrike
)

// Underline shapes
const (
	UnderNone uint8 = iota
	UnderSingle
	UnderDouble
	UnderCurly
	UnderDotted
	UnderDashed
)

// Colors, underline shape and attributes of a cell, comparable with ==
type Style struct {
	Fg, Bg, Ul Color
	Under      uint8
	Attrs      uint8
}

// Applies SGR parameters to the pen the way the toolkit's serializer means them
func readSGR(params ansi.Params, pen *Style) {
	if len(params) == 0 {
		*pen = Style{}
		return
	}
	for i := 0; i < len(params); i++ {
		param, more, _ := params.Param(i, 0)
		switch {
		case param == 0:
			*pen = Style{}
		case param >= 1 && param <= 9 && param != 4:
			pen.Attrs |= [10]uint8{0, AttrBold, AttrFaint, AttrItalic, 0, AttrBlink, AttrRapidBlink, AttrReverse, AttrConceal, AttrStrike}[param]
		case param == 4:
			next, _, ok := params.Param(i+1, 0)
			if more && ok {
				if next >= 0 && next <= 5 {
					i++
					pen.Under = uint8(next)
				}
			} else {
				pen.Under = UnderSingle
			}
		case param == 22:
			pen.Attrs &^= AttrBold | AttrFaint
		case param == 23:
			pen.Attrs &^= AttrItalic
		case param == 24:
			pen.Under = UnderNone
		case param == 25:
			pen.Attrs &^= AttrBlink | AttrRapidBlink
		case param == 27:
			pen.Attrs &^= AttrReverse
		case param == 28:
			pen.Attrs &^= AttrConceal
		case param == 29:
			pen.Attrs &^= AttrStrike
		case param >= 30 && param <= 37:
			pen.Fg = Basic(uint8(param - 30))
		case param >= 40 && param <= 47:
			pen.Bg = Basic(uint8(param - 40))
		case param >= 90 && param <= 97:
			pen.Fg = Basic(uint8(param - 90 + 8))
		case param >= 100 && param <= 107:
			pen.Bg = Basic(uint8(param - 100 + 8))
		case param == 39:
			pen.Fg = 0
		case param == 49:
			pen.Bg = 0
		case param == 59:
			pen.Ul = 0
		case param == 38 || param == 48 || param == 58:
			var c color.Color
			if n := ansi.ReadStyleColor(params[i:], &c); n > 0 {
				switch param {
				case 38:
					pen.Fg = ColorOf(c)
				case 48:
					pen.Bg = ColorOf(c)
				default:
					pen.Ul = ColorOf(c)
				}
				i += n - 1
			}
		}
	}
}

// Appends a decimal number
func num(b []byte, n int) []byte { return strconv.AppendInt(b, int64(n), 10) }

// Digits of a non negative number
func digits(n int) int {
	d := 1
	for n >= 10 {
		n /= 10
		d++
	}
	return d
}

// Appends the SGR parameters that select a color in the slot whose base is 30, 40 or 50
func appendColor(b []byte, c Color, base int) []byte {
	v := int(c & 0xffffff)
	switch c & kindMask {
	case kindBasic:
		if base == 50 {
			return num(append(b, "58;5;"...), v)
		}
		if v < 8 {
			return num(b, base+v)
		}
		return num(b, base+60+v-8)
	case kindIndexed:
		return num(append(num(b, base+8), ";5;"...), v)
	}
	b = append(num(b, base+8), ";2;"...)
	b = append(num(b, v>>16), ';')
	b = append(num(b, (v>>8)&0xff), ';')
	return num(b, v&0xff)
}

// Appends a parameter with its separator
func param(b []byte, p string) []byte {
	if len(b) > 0 {
		b = append(b, ';')
	}
	return append(b, p...)
}

// Appends a color parameter with its separator
func colorParam(b []byte, c Color, base int) []byte {
	if len(b) > 0 {
		b = append(b, ';')
	}
	return appendColor(b, c, base)
}

// Parameters that turn the attributes of s on from a reset pen
func onParams(b []byte, s Style) []byte {
	for i, p := range [...]string{"1", "2", "3", "5", "6", "7", "8", "9"} {
		if s.Attrs&(1<<i) != 0 {
			b = param(b, p)
		}
	}
	if s.Under != UnderNone {
		b = param(b, underParam(s.Under))
	}
	if s.Fg != 0 {
		b = colorParam(b, s.Fg, 30)
	}
	if s.Bg != 0 {
		b = colorParam(b, s.Bg, 40)
	}
	if s.Ul != 0 {
		b = colorParam(b, s.Ul, 50)
	}
	return b
}

// SGR parameter of an underline shape
func underParam(u uint8) string {
	return [...]string{"24", "4", "4:2", "4:3", "4:4", "4:5"}[u]
}

// Parameters that change the pen from one style to another without a reset
func deltaParams(b []byte, from, to Style) []byte {
	group := func(off string, bits []uint8, ons []string) {
		lost := false
		for _, bit := range bits {
			lost = lost || (from.Attrs&bit != 0 && to.Attrs&bit == 0)
		}
		if lost {
			b = param(b, off)
		}
		for i, bit := range bits {
			if to.Attrs&bit != 0 && (lost || from.Attrs&bit == 0) {
				b = param(b, ons[i])
			}
		}
	}
	group("22", []uint8{AttrBold, AttrFaint}, []string{"1", "2"})
	group("23", []uint8{AttrItalic}, []string{"3"})
	group("25", []uint8{AttrBlink, AttrRapidBlink}, []string{"5", "6"})
	group("27", []uint8{AttrReverse}, []string{"7"})
	group("28", []uint8{AttrConceal}, []string{"8"})
	group("29", []uint8{AttrStrike}, []string{"9"})
	if from.Under != to.Under {
		b = param(b, underParam(to.Under))
	}
	if from.Fg != to.Fg {
		if to.Fg == 0 {
			b = param(b, "39")
		} else {
			b = colorParam(b, to.Fg, 30)
		}
	}
	if from.Bg != to.Bg {
		if to.Bg == 0 {
			b = param(b, "49")
		} else {
			b = colorParam(b, to.Bg, 40)
		}
	}
	if from.Ul != to.Ul {
		if to.Ul == 0 {
			b = param(b, "59")
		} else {
			b = colorParam(b, to.Ul, 50)
		}
	}
	return b
}

// Shortest SGR sequence taking the pen from one style to another, empty when they match
func appendSGR(b []byte, from Style, known bool, to Style) []byte {
	if known && from == to {
		return b
	}
	var reset, delta [96]byte
	r := onParams(reset[:0], to)
	if !known || len(r) == 0 {
		b = append(b, "\x1b["...)
		if len(r) > 0 {
			b = append(append(b, "0;"...), r...)
		}
		return append(b, 'm')
	}
	d := deltaParams(delta[:0], from, to)
	b = append(b, "\x1b["...)
	if len(d) <= len(r)+2 {
		b = append(b, d...)
	} else {
		b = append(append(b, "0;"...), r...)
	}
	return append(b, 'm')
}

// Byte length of the SGR sequence appendSGR would write
func sgrCost(from Style, known bool, to Style) int {
	var buf [128]byte
	return len(appendSGR(buf[:0], from, known, to))
}

// Style the profile can show, colors mapped to the nearest the terminal has
func convertStyle(s Style, p colorprofile.Profile, cache map[Color]Color) Style {
	switch p {
	case colorprofile.TrueColor:
		return s
	case colorprofile.NoTTY:
		return Style{}
	case colorprofile.ASCII:
		s.Fg, s.Bg, s.Ul = 0, 0, 0
		return s
	}
	conv := func(c Color) Color {
		if c == 0 {
			return 0
		}
		if v, ok := cache[c]; ok {
			return v
		}
		v := ColorOf(p.Convert(c.Std()))
		cache[c] = v
		return v
	}
	s.Fg, s.Bg, s.Ul = conv(s.Fg), conv(s.Bg), conv(s.Ul)
	if s.Ul&kindMask == kindBasic {
		s.Ul = Indexed(uint8(s.Ul))
	}
	return s
}

// Hyperlink target and its OSC 8 parameters
type linkKey struct{ url, params string }

// Interned hyperlinks, id zero meaning none
type linkTable struct {
	ids  map[linkKey]uint32
	list []linkKey
}

// Empty table holding only the no link entry
func newLinkTable() *linkTable {
	return &linkTable{ids: map[linkKey]uint32{}, list: []linkKey{{}}}
}

// Id of a hyperlink, adding it on first sight
func (t *linkTable) id(url, params string) uint32 {
	if url == "" {
		return 0
	}
	k := linkKey{url, params}
	if id, ok := t.ids[k]; ok {
		return id
	}
	id := uint32(len(t.list))
	t.list = append(t.list, k)
	t.ids[k] = id
	return id
}

// OSC 8 sequence that opens the link, or closes any link for id zero
func (t *linkTable) appendOSC8(b []byte, id uint32) []byte {
	k := t.list[id]
	b = append(b, "\x1b]8;"...)
	b = append(b, k.params...)
	b = append(b, ';')
	b = append(b, k.url...)
	return append(b, '\a')
}
