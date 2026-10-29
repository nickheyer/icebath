package term

import (
	"image/color"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Applies a select graphic rendition sequence to the pen
func (e *Emulator) sgr(params ansi.Params) {
	pen := &e.scr.cur.pen
	if len(params) == 0 {
		*pen = uv.Style{}
		return
	}
	for i := 0; i < len(params); i++ {
		p, more, _ := params.Param(i, 0)
		switch p {
		case 0:
			*pen = uv.Style{}
		case 1:
			pen.Attrs |= uv.AttrBold
		case 2:
			pen.Attrs |= uv.AttrFaint
		case 3:
			pen.Attrs |= uv.AttrItalic
		case 4:
			pen.Underline = uv.UnderlineSingle
			if more && i+1 < len(params) {
				i++
				style, _, _ := params.Param(i, 1)
				pen.Underline = underlineStyle(style)
			}
		case 5:
			pen.Attrs |= uv.AttrBlink
		case 6:
			pen.Attrs |= uv.AttrRapidBlink
		case 7:
			pen.Attrs |= uv.AttrReverse
		case 8:
			pen.Attrs |= uv.AttrConceal
		case 9:
			pen.Attrs |= uv.AttrStrikethrough
		case 21:
			pen.Underline = uv.UnderlineDouble
		case 22:
			pen.Attrs &^= uv.AttrBold | uv.AttrFaint
		case 23:
			pen.Attrs &^= uv.AttrItalic
		case 24:
			pen.Underline = uv.UnderlineNone
		case 25:
			pen.Attrs &^= uv.AttrBlink | uv.AttrRapidBlink
		case 27:
			pen.Attrs &^= uv.AttrReverse
		case 28:
			pen.Attrs &^= uv.AttrConceal
		case 29:
			pen.Attrs &^= uv.AttrStrikethrough
		case 30, 31, 32, 33, 34, 35, 36, 37:
			pen.Fg = ansi.BasicColor(p - 30)
		case 38:
			c, n := extendedColor(params, i)
			if n > 0 {
				pen.Fg = c
			}
			i += n
		case 39:
			pen.Fg = nil
		case 40, 41, 42, 43, 44, 45, 46, 47:
			pen.Bg = ansi.BasicColor(p - 40)
		case 48:
			c, n := extendedColor(params, i)
			if n > 0 {
				pen.Bg = c
			}
			i += n
		case 49:
			pen.Bg = nil
		case 58:
			c, n := extendedColor(params, i)
			if n > 0 {
				pen.UnderlineColor = c
			}
			i += n
		case 59:
			pen.UnderlineColor = nil
		case 90, 91, 92, 93, 94, 95, 96, 97:
			pen.Fg = ansi.BasicColor(p - 90 + 8)
		case 100, 101, 102, 103, 104, 105, 106, 107:
			pen.Bg = ansi.BasicColor(p - 100 + 8)
		}
	}
}

// Underline style named by the subparameter of SGR 4
func underlineStyle(n int) uv.Underline {
	switch n {
	case 0:
		return uv.UnderlineNone
	case 2:
		return uv.UnderlineDouble
	case 3:
		return uv.UnderlineCurly
	case 4:
		return uv.UnderlineDotted
	case 5:
		return uv.UnderlineDashed
	}
	return uv.UnderlineSingle
}

// Color named after SGR 38, 48 or 58 at i, in semicolon or colon form, with the parameters it used
func extendedColor(params ansi.Params, i int) (color.Color, int) {
	kind, more, ok := params.Param(i+1, -1)
	if !ok {
		return nil, 0
	}
	switch kind {
	case 5:
		n, _, ok := params.Param(i+2, -1)
		if !ok || n < 0 || n > 255 {
			return nil, 0
		}
		return ansi.IndexedColor(n), 2
	case 2:
		at := i + 2
		// The colon form may carry a color space id before the channels
		if more && len(params) > i+5 && params[i+1].HasMore() && params[i+2].HasMore() && params[i+3].HasMore() && params[i+4].HasMore() {
			at = i + 3
		}
		r, _, okr := params.Param(at, -1)
		g, _, okg := params.Param(at+1, -1)
		b, _, okb := params.Param(at+2, -1)
		if !okr || !okg || !okb {
			return nil, 0
		}
		clamp := func(v int) uint8 { return uint8(min(max(v, 0), 255)) }
		return color.RGBA{R: clamp(r), G: clamp(g), B: clamp(b), A: 0xff}, at + 2 - i
	}
	return nil, 0
}
