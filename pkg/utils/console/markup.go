package consoleutil

import (
	"cmp"
	"slices"
	"strings"

	"github.com/charmbracelet/colorprofile"
)

// Style laid over the byte range start to end of a plain text
type span struct {
	start, end int
	style      Style
}

// Whether a byte can follow [ to open a tag
func isTagStart(c byte) bool { return c >= 'a' && c <= 'z' || c == '#' || c == '/' }

// Tag shaped part of markup: the backslashes before it from start, its [ at open, and end just past its ]
type tagMatch struct{ start, open, end int }

// First tag shaped part of s at or after from, the backslashes right before it counted back no further than from
func nextTag(s string, from int) (tagMatch, bool) {
	for i := from; i < len(s); {
		k := strings.IndexByte(s[i:], '[')
		if k < 0 {
			break
		}
		k += i
		if k+1 < len(s) && isTagStart(s[k+1]) && (k == 0 || s[k-1] != 0x1b) {
			e := k + 1
			for e < len(s) && s[e] != ']' && s[e] != '[' {
				e++
			}
			if e == len(s) {
				break
			}
			if s[e] == ']' {
				b := k
				for b > from && s[b-1] == '\\' {
					b--
				}
				return tagMatch{start: b, open: k, end: e + 1}, true
			}
			i = e
			continue
		}
		i = k + 1
	}
	return tagMatch{}, false
}

// Identity an open tag is closed by: its style when its name is a style definition, else its name in lower case
type tagKey struct {
	style Style
	name  string
}

func keyOf(name string) tagKey {
	if st, err := ParseStyle(name); err == nil {
		return tagKey{style: st}
	}
	return tagKey{name: strings.ToLower(strings.TrimSpace(name))}
}

type openTag struct {
	key   tagKey
	style Style
	start int
}

// Plain text of console markup and the spans its tags lay over it, outer before inner: [style] opens, [/style] or [/] closes, [link=address] links, \[ is a literal bracket, a tag naming no style or closing nothing stays as written; SGR and OSC 8 sequences written in the text style what follows them over the tags, other escape sequences are dropped
func parseMarkup(markup string) (string, []span) {
	if strings.IndexByte(markup, '[') < 0 && strings.IndexByte(markup, 0x1b) < 0 {
		return stripControl(markup), nil
	}
	var b strings.Builder
	b.Grow(len(markup))
	var raw Style
	var rawSpans []span
	rawStart := 0
	setRaw := func(st Style) {
		if st == raw {
			return
		}
		if !raw.isZero() && b.Len() > rawStart {
			rawSpans = append(rawSpans, span{rawStart, b.Len(), raw})
		}
		raw, rawStart = st, b.Len()
	}
	text := func(s string) {
		s = stripControl(strings.ReplaceAll(s, `\[`, "["))
		if strings.IndexByte(s, 0x1b) < 0 {
			b.WriteString(s)
			return
		}
		runs, end := scanANSI(s, raw, true)
		for _, r := range runs {
			setRaw(r.style)
			b.WriteString(r.text)
		}
		setRaw(end)
	}
	var stack []openTag
	var spans []span
	pos := 0
	for {
		m, ok := nextTag(markup, pos)
		if !ok {
			break
		}
		text(markup[pos:m.start])
		backslashes := m.open - m.start
		b.WriteString(strings.Repeat(`\`, backslashes/2))
		pos = m.end
		literal := markup[m.open:m.end]
		if backslashes%2 == 1 {
			text(literal)
			continue
		}
		name, params, hasParams := strings.Cut(literal[1:len(literal)-1], "=")
		if closing, ok := strings.CutPrefix(name, "/"); ok {
			idx := len(stack) - 1
			if closing = strings.TrimSpace(closing); closing != "" {
				key := keyOf(closing)
				for idx >= 0 && stack[idx].key != key {
					idx--
				}
			}
			if idx < 0 {
				text(literal)
				continue
			}
			spans = append(spans, span{stack[idx].start, b.Len(), stack[idx].style})
			stack = slices.Delete(stack, idx, idx+1)
			continue
		}
		def := name
		if hasParams {
			def = name + " " + params
		}
		st, err := ParseStyle(def)
		if err != nil {
			text(literal)
			continue
		}
		stack = append(stack, openTag{key: keyOf(name), style: st, start: b.Len()})
	}
	text(markup[pos:])
	setRaw(Style{})
	for i := len(stack) - 1; i >= 0; i-- {
		spans = append(spans, span{stack[i].start, b.Len(), stack[i].style})
	}
	slices.Reverse(spans)
	slices.SortStableFunc(spans, func(x, y span) int { return cmp.Compare(x.start, y.start) })
	return b.String(), append(spans, rawSpans...)
}

// Plain text cut at every span edge, each piece drawn in its covering spans combined in list order, later spans over earlier ones
func styled(plain string, spans []span) segments {
	if plain == "" {
		return nil
	}
	if len(spans) == 0 {
		return segments{{plain, Style{}}}
	}
	type edge struct {
		at, idx int
		open    bool
	}
	edges := make([]edge, 0, 2*len(spans))
	for i, sp := range spans {
		if sp.start < sp.end {
			edges = append(edges, edge{sp.start, i, true}, edge{sp.end, i, false})
		}
	}
	slices.SortFunc(edges, func(x, y edge) int {
		if c := cmp.Compare(x.at, y.at); c != 0 {
			return c
		}
		if x.open == y.open {
			return 0
		}
		if x.open {
			return 1
		}
		return -1
	})
	out := make(segments, 0, len(edges)+1)
	var active []int
	pos := 0
	emit := func(to int) {
		if to <= pos {
			return
		}
		var st Style
		for _, idx := range active {
			st = st.add(spans[idx].style)
		}
		out = append(out, segment{plain[pos:to], st})
		pos = to
	}
	for _, e := range edges {
		emit(e.at)
		at, found := slices.BinarySearch(active, e.idx)
		if e.open && !found {
			active = slices.Insert(active, at, e.idx)
		} else if !e.open && found {
			active = slices.Delete(active, at, at+1)
		}
	}
	emit(len(plain))
	return out
}

// Styled text of console markup
func markupText(markup string) segments {
	plain, spans := parseMarkup(markup)
	return styled(plain, spans)
}

// Zero width tag pair Escape ends text with after its trailing backslashes, so a tag written right after them is not escaped by them
const escapeStop = "[none][/none]"

// Markup that prints the given text as written wherever it is placed in other markup, no part of it read as a tag: tag shaped parts escaped, a backslash before a bracket doubled, trailing backslashes doubled and closed off by an empty tag pair
func Escape(text string) string {
	tail := len(text) - len(strings.TrimRight(text, `\`))
	if tail == 0 && strings.IndexByte(text, '[') < 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text) + tail + len(escapeStop) + 8)
	pos := 0
	for m, ok := nextTag(text, 0); ok; m, ok = nextTag(text, pos) {
		escapeBrackets(&b, text[pos:m.start])
		b.WriteString(strings.Repeat(`\`, 2*(m.open-m.start)+1))
		b.WriteString(text[m.open:m.end])
		pos = m.end
	}
	escapeBrackets(&b, text[pos:])
	if tail > 0 {
		b.WriteString(strings.Repeat(`\`, tail))
		b.WriteString(escapeStop)
	}
	return b.String()
}

// Writes text that holds no tag with a backslash added before every bracket a backslash precedes, the one that markup turns back into the bracket alone
func escapeBrackets(b *strings.Builder, text string) {
	for {
		i := strings.Index(text, `\[`)
		if i < 0 {
			b.WriteString(text)
			return
		}
		b.WriteString(text[:i+1])
		b.WriteString(`\[`)
		text = text[i+2:]
	}
}

// Text of console markup with its tags resolved, their styles and any escape sequences dropped
func Plain(markup string) string {
	plain, _ := parseMarkup(markup)
	return plain
}

// Text of console markup, styled with escape sequences for the terminal's color profile when color is set, plain otherwise
func Render(markup string, color bool) string {
	if !color {
		return Plain(markup)
	}
	return renderLines(markupText(markup).lines(), TerminalProfile(), true)
}

// Profile for rendering when color is set, none otherwise
func colorFor(color bool) colorprofile.Profile {
	if color {
		return TerminalProfile()
	}
	return colorprofile.NoTTY
}
