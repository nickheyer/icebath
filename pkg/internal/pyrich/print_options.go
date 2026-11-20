package pyrich

import (
	"strconv"
	"strings"
	"unicode"
)

// PrintOptions are the console print keywords a renderable honours beyond the console width
type PrintOptions struct {
	// Style laid under every segment, as print(style=...)
	Style string
	// Lines left unwrapped and uncropped so the terminal wraps them, as print(soft_wrap=True)
	SoftWrap bool
}

// spanRenderable is a renderable able to hand its lines over as styled spans, wrapped or not
type spanRenderable interface {
	spanLines(width int, noWrap bool) []styledText
}

var sgrAttributes = map[int][]struct {
	index int
	value int8
}{
	1: {{0, 1}}, 2: {{1, 1}}, 3: {{2, 1}}, 4: {{3, 1}}, 5: {{4, 1}}, 6: {{5, 1}}, 7: {{6, 1}}, 8: {{7, 1}}, 9: {{8, 1}},
	21: {{9, 1}}, 22: {{1, -1}, {0, -1}}, 23: {{2, -1}}, 24: {{3, -1}}, 25: {{4, -1}}, 26: {{5, -1}}, 27: {{6, -1}},
	28: {{7, -1}}, 29: {{8, -1}}, 51: {{10, 1}}, 52: {{11, 1}}, 53: {{12, 1}}, 54: {{10, -1}, {11, -1}}, 55: {{12, -1}},
}

// Style in force after one SGR parameter list, as rich AnsiDecoder folds codes into its running style
func applySGR(cur Style, params string) Style {
	codes := []int{}
	for _, p := range strings.Split(params, ";") {
		if p == "" {
			codes = append(codes, 0)
			continue
		}
		if n, err := strconv.Atoi(p); err == nil {
			codes = append(codes, min(255, n))
		}
	}
	for i := 0; i < len(codes); i++ {
		code := codes[i]
		switch {
		case code == 0:
			cur = Style{}
		case code == 38 || code == 48:
			name := ""
			if i+1 < len(codes) && codes[i+1] == 5 && i+2 < len(codes) {
				name = "color(" + strconv.Itoa(codes[i+2]) + ")"
				i += 2
			} else if i+1 < len(codes) && codes[i+1] == 2 && i+4 < len(codes) {
				name = "#" + hexByte(codes[i+2]) + hexByte(codes[i+3]) + hexByte(codes[i+4])
				i += 4
			} else if i+1 < len(codes) {
				i++
			}
			if name == "" {
				continue
			}
			if code == 38 {
				cur.Color = name
			} else {
				cur.Bgcolor = name
			}
		case code >= 30 && code <= 37:
			cur.Color = "color(" + strconv.Itoa(code-30) + ")"
		case code == 39:
			cur.Color = "default"
		case code >= 40 && code <= 47:
			cur.Bgcolor = "color(" + strconv.Itoa(code-40) + ")"
		case code == 49:
			cur.Bgcolor = "default"
		case code >= 90 && code <= 97:
			cur.Color = "color(" + strconv.Itoa(code-90+8) + ")"
		case code >= 100 && code <= 107:
			cur.Bgcolor = "color(" + strconv.Itoa(code-100+8) + ")"
		default:
			for _, a := range sgrAttributes[code] {
				*cur.attrs()[a.index] = a.value
			}
		}
	}
	return cur
}

func hexByte(n int) string {
	s := strconv.FormatInt(int64(n&0xff), 16)
	if len(s) < 2 {
		return "0" + s
	}
	return s
}

// Styled spans of one ANSI line, each SGR and hyperlink run decoded into the style it draws in
func decodeANSILine(line string) styledText {
	var out styledText
	cur := Style{}
	pos := 0
	emit := func(text string) {
		if text != "" {
			out = append(out, span{text, cur})
		}
	}
	for _, m := range escapeSeqRE.FindAllStringIndex(line, -1) {
		emit(line[pos:m[0]])
		seq := line[m[0]:m[1]]
		pos = m[1]
		if strings.HasPrefix(seq, "\x1b]8;") {
			_, url, _ := strings.Cut(seq[len("\x1b]8;"):len(seq)-2], ";")
			cur.Link = url
			continue
		}
		cur = applySGR(cur, seq[2:len(seq)-1])
	}
	emit(line[pos:])
	return out
}

// Spans with the base style under each of them, as Segment.apply_style
func (t styledText) over(base Style) styledText {
	out := make(styledText, len(t))
	for i, s := range t {
		out[i] = span{s.text, base.Add(s.style)}
	}
	return out
}

// Trailing whitespace beyond width cells removed, as rich Text.rstrip_end
func (t styledText) rstripEnd(width int) styledText {
	p := []rune(t.plain())
	n := len(p)
	if n <= width {
		return t
	}
	ws := 0
	for ws < n && unicode.IsSpace(p[n-1-ws]) {
		ws++
	}
	return t.slice(0, n-min(ws, n-width))
}

// Lines of the text as spans, wrapped at width or left whole for the terminal to wrap
func (t *Text) spanLines(width int, noWrap bool) []styledText {
	var out []styledText
	for _, line := range strings.Split(t.Plain, "\n") {
		spans := decodeANSILine(line)
		if noWrap {
			out = append(out, spans.expandTabs(8).rstripEnd(width))
			continue
		}
		out = append(out, spans.wrap(width, "", Style{}, false)...)
	}
	return out
}

// Lines of the document as spans, top level lines uncropped when left unwrapped
func (m *Markdown) spanLines(width int, noWrap bool) []styledText {
	lines := m.lines(width, noWrap)
	if noWrap {
		return lines
	}
	out := make([]styledText, len(lines))
	for i, l := range lines {
		out[i] = l.crop(width)
	}
	return out
}

// Span lines of any renderable at width, decoded from its rendered lines when it holds no spans of its own
func renderableSpans(r Renderable, color bool, system string, width int, noWrap bool) []styledText {
	if sr, ok := r.(spanRenderable); ok {
		return sr.spanLines(width, noWrap)
	}
	var out []styledText
	for _, line := range r.RenderLines(color, system, width) {
		out = append(out, decodeANSILine(line))
	}
	return out
}

// Prints a renderable with the print keywords applied, the error a style Rich cannot parse raises
func (c *Console) PrintRenderableWith(r Renderable, o PrintOptions) error {
	base, err := ParseStyle(o.Style)
	if err != nil {
		return err
	}
	var lines []string
	for _, l := range renderableSpans(r, c.Color, c.system(), c.Width, o.SoftWrap) {
		lines = append(lines, l.over(base).render(c.Color, c.system()))
	}
	c.write(lines)
	return nil
}
