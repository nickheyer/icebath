package consoleutil

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/colorprofile"
	"github.com/rivo/uniseg"
)

// Run of text drawn in one style
type segment struct {
	text  string
	style Style
}

// Styled text as runs, one visual line once laid out
type segments []segment

// Terminal cells s takes, grapheme clusters counted at their display width and control characters at none
func cells(s string) int {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= utf8.RuneSelf || c < 0x20 || c == 0x7f {
			return uniseg.StringWidth(s)
		}
	}
	return len(s)
}

// Longest head of s within width cells, whole grapheme clusters only, and the cells it takes
func cut(s string, width int) (string, int) {
	ascii := true
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= utf8.RuneSelf || c < 0x20 || c == 0x7f {
			ascii = false
			break
		}
	}
	if ascii {
		n := min(len(s), max(0, width))
		return s[:n], n
	}
	total, i, state := 0, 0, -1
	for i < len(s) {
		cluster, _, w, next := uniseg.FirstGraphemeClusterInString(s[i:], state)
		if total+w > width {
			break
		}
		total, i, state = total+w, i+len(cluster), next
	}
	return s[:i], total
}

func spaces(n int) string { return strings.Repeat(" ", max(0, n)) }

// Copy of t with a run appended, the input never written through
func (t segments) with(text string, st Style) segments {
	if text == "" {
		return t
	}
	return append(t[:len(t):len(t)], segment{text, st})
}

// New text of t followed by o
func (t segments) join(o segments) segments {
	out := make(segments, 0, len(t)+len(o))
	return append(append(out, t...), o...)
}

func (t segments) plain() string {
	if len(t) == 1 {
		return t[0].text
	}
	var b strings.Builder
	for _, s := range t {
		b.WriteString(s.text)
	}
	return b.String()
}

func (t segments) width() int {
	n := 0
	for _, s := range t {
		n += cells(s.text)
	}
	return n
}

// Runs covering the bytes from to to of the plain text
func (t segments) slice(from, to int) segments {
	var out segments
	pos := 0
	for _, s := range t {
		end := pos + len(s.text)
		if a, b := max(from, pos), min(to, end); a < b {
			out = append(out, segment{s.text[a-pos : b-pos], s.style})
		}
		pos = end
		if pos >= to {
			break
		}
	}
	return out
}

// Style of the run holding byte offset at of the plain text, the last run's at the end
func (t segments) styleAt(at int) Style {
	pos := 0
	for _, s := range t {
		if pos += len(s.text); at < pos {
			return s.style
		}
	}
	if len(t) > 0 {
		return t[len(t)-1].style
	}
	return Style{}
}

// Runs with base laid under each of them
func (t segments) over(base Style) segments {
	if base.isZero() {
		return t
	}
	out := make(segments, len(t))
	for i, s := range t {
		out[i] = segment{s.text, base.add(s.style)}
	}
	return out
}

// Text cut to width cells, a wide character split at the edge becoming spaces
func (t segments) crop(width int) segments {
	if t.width() <= width {
		return t
	}
	out := make(segments, 0, len(t))
	total := 0
	for _, s := range t {
		head, w := cut(s.text, width-total)
		if len(head) < len(s.text) {
			if fill := head + spaces(width-total-w); fill != "" {
				out = append(out, segment{fill, s.style})
			}
			return out
		}
		out = append(out, s)
		total += w
	}
	return out
}

func (t segments) padTo(width int, st Style) segments { return t.with(spaces(width-t.width()), st) }

// Text without trailing white space
func (t segments) rstrip() segments {
	plain := t.plain()
	return t.slice(0, len(strings.TrimRightFunc(plain, unicode.IsSpace)))
}

// Text with trailing white space dropped only as far as it reaches past width cells
func (t segments) rstripEnd(width int) segments {
	excess := t.width() - width
	if excess <= 0 {
		return t
	}
	plain := t.plain()
	end := len(plain)
	for excess > 0 && end > 0 {
		r, size := utf8.DecodeLastRuneInString(plain[:end])
		if !unicode.IsSpace(r) {
			break
		}
		end -= size
		excess -= max(1, cells(string(r)))
	}
	return t.slice(0, end)
}

// Text with each tab replaced by spaces up to the next multiple of size cells, columns counted from each line start
func (t segments) expandTabs(size int) segments {
	tabbed := false
	for _, s := range t {
		if strings.IndexByte(s.text, '\t') >= 0 {
			tabbed = true
			break
		}
	}
	if !tabbed {
		return t
	}
	out := make(segments, 0, len(t))
	col := 0
	for _, s := range t {
		var b strings.Builder
		rest := s.text
		for rest != "" {
			i := strings.IndexAny(rest, "\t\n")
			if i < 0 {
				b.WriteString(rest)
				col += cells(rest)
				break
			}
			b.WriteString(rest[:i])
			col += cells(rest[:i])
			if rest[i] == '\n' {
				b.WriteByte('\n')
				col = 0
			} else {
				n := size - col%size
				b.WriteString(spaces(n))
				col += n
			}
			rest = rest[i+1:]
		}
		out = append(out, segment{b.String(), s.style})
	}
	return out
}

// Text split at its newlines, one empty line for empty text
func (t segments) lines() []segments {
	var out []segments
	var cur segments
	for _, s := range t {
		rest := s.text
		for {
			i := strings.IndexByte(rest, '\n')
			if i < 0 {
				if rest != "" {
					cur = append(cur, segment{rest, s.style})
				}
				break
			}
			if i > 0 {
				cur = append(cur, segment{rest[:i], s.style})
			}
			out = append(out, cur)
			cur = nil
			rest = rest[i+1:]
		}
	}
	return append(out, cur)
}

// Text made to fit width cells as overflow says, an ellipsis ending what it cuts or a plain cut, then padded to width in padStyle when pad is set
func (t segments) truncate(width int, overflow string, pad bool, padStyle Style) segments {
	w := t.width()
	if w > width {
		if overflow == "ellipsis" && width > 0 {
			head := t.crop(width - 1)
			t = head.with("…", t.styleAt(len(head.plain())))
		} else {
			t = t.crop(max(0, width))
		}
		w = t.width()
	}
	if pad && w < width {
		t = t.with(spaces(width-w), padStyle)
	}
	return t
}

// Plain text of laid out lines joined by newlines
func plainLines(lines []segments) string {
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l.plain())
	}
	return b.String()
}

// One line drawn for the profile, neighbouring runs of the same style joined, hyperlinks written when links is set
func renderLine(l segments, p colorprofile.Profile, links bool) string {
	var b strings.Builder
	if p <= colorprofile.NoTTY {
		for _, s := range l {
			b.WriteString(s.text)
		}
		return b.String()
	}
	for i := 0; i < len(l); {
		st, j := l[i].style, i+1
		for j < len(l) && l[j].style == st {
			j++
		}
		if j == i+1 {
			if l[i].text != "" {
				st.write(&b, l[i].text, p, links)
			}
		} else {
			var run strings.Builder
			for _, s := range l[i:j] {
				run.WriteString(s.text)
			}
			if run.Len() > 0 {
				st.write(&b, run.String(), p, links)
			}
		}
		i = j
	}
	return b.String()
}

// Lines drawn for the profile and joined by newlines
func renderLines(lines []segments, p colorprofile.Profile, links bool) string {
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(renderLine(l, p, links))
	}
	return b.String()
}

// Text with every hyperlink written out as its address in parentheses after the linked words, for output that cannot carry links
func inlineLinks(t segments) segments {
	linked := false
	for _, s := range t {
		if s.style.link != "" {
			linked = true
			break
		}
	}
	if !linked {
		return t
	}
	out := make(segments, 0, len(t)+2)
	for i := 0; i < len(t); {
		link := t[i].style.link
		if link == "" {
			out = append(out, t[i])
			i++
			continue
		}
		var label strings.Builder
		var last Style
		for ; i < len(t) && t[i].style.link == link; i++ {
			last = t[i].style.withLink("")
			out = append(out, segment{t[i].text, last})
			label.WriteString(t[i].text)
		}
		if text := strings.TrimSpace(label.String()); text != link && "mailto:"+text != link {
			out = append(out, segment{" (" + link + ")", last})
		}
	}
	return out
}

// Text with the bell, backspace, vertical tab, form feed and carriage return characters removed
func stripControl(s string) string {
	if !strings.ContainsAny(s, "\a\b\v\f\r") {
		return s
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '\a', '\b', '\v', '\f', '\r':
			return -1
		}
		return r
	}, s)
}
