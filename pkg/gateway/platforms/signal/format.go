package signal

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Signal body range styles signal-cli accepts on send
const (
	styleBold      = "BOLD"
	styleItalic    = "ITALIC"
	styleStrike    = "STRIKETHROUGH"
	styleMonospace = "MONOSPACE"
)

// Monospace columns a phone screen fits, wider tables fall back to vertical key value rows
const tableWidth = 40

// Unicode whitespace class used by the heading and bullet rules
const spaceClass = `[\t\n\v\f\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

var (
	fencedBlockRE = regexp.MustCompile("(?s)```.*?```")
	codeBlockRE   = regexp.MustCompile("(?s)```[a-zA-Z0-9_+-]*\n?(.*?)```")
	blankRunRE    = regexp.MustCompile(`\n{3,}`)
	bulletRE      = regexp.MustCompile(`(?m)^([ \t]{0,3})[-*+]` + spaceClass + `+`)
)

// One style range, offsets in bytes while formatting and in UTF-16 code units once rendered
type textStyle struct {
	start, length int
	style         string
}

// Range in the signal-cli start length style syntax
func (s textStyle) String() string { return fmt.Sprintf("%d:%d:%s", s.start, s.length, s.style) }

// Whether r is whitespace the way the markdown rules count it
func isSpace(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f }

// Whether r is a word character for the underscore italic rule
func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

// UTF-16 code units of s
func utf16Units(s string) int {
	n := 0
	for _, r := range s {
		if l := utf16.RuneLen(r); l > 0 {
			n += l
		} else {
			n++
		}
	}
	return n
}

// Applies fn to every stretch outside fenced code, fences kept byte for byte
func outsideFences(s string, fn func(string) string) string {
	var b strings.Builder
	last := 0
	for _, loc := range fencedBlockRE.FindAllStringIndex(s, -1) {
		b.WriteString(fn(s[last:loc[0]]))
		b.WriteString(s[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(fn(s[last:]))
	return b.String()
}

// Markdown bullet markers turned into Unicode bullets since Signal renders dashes literally
func normalizeBullets(s string) string {
	return outsideFences(s, func(part string) string { return bulletRE.ReplaceAllString(part, "${1}• ") })
}

// Every pipe table outside a fence realigned to the phone width and wrapped in a fence so it renders monospace
func fenceTables(s string) string {
	return outsideFences(s, func(part string) string {
		if !strings.Contains(part, "|") {
			return part
		}
		lines := strings.Split(part, "\n")
		out := make([]string, 0, len(lines))
		width := tableWidth
		for i := 0; i < len(lines); {
			if strings.Contains(lines[i], "|") && i+1 < len(lines) && tools.IsTableDivider(lines[i+1]) {
				j := i + 2
				for j < len(lines) && strings.Contains(lines[j], "|") && strings.TrimFunc(lines[j], isSpace) != "" {
					j++
				}
				out = append(out, "```", tools.RealignMarkdownTables(strings.Join(lines[i:j], "\n"), &width), "```")
				i = j
				continue
			}
			out = append(out, lines[i])
			i++
		}
		return strings.Join(out, "\n")
	})
}

// Plain text and Signal style ranges in UTF-16 code units for markdown, code kept verbatim
func markdownToSignal(markdown string) (string, []textStyle) {
	text := strings.TrimFunc(blankRunRE.ReplaceAllString(markdown, "\n\n"), isSpace)
	text = fenceTables(normalizeBullets(text))
	var out strings.Builder
	var styles []textStyle
	rest := text
	for {
		loc := codeBlockRE.FindStringSubmatchIndex(rest)
		prose := rest
		if loc != nil {
			prose = rest[:loc[0]]
		}
		atLineStart := out.Len() == 0 || strings.HasSuffix(out.String(), "\n")
		plain, proseStyles := formatProse(prose, atLineStart)
		for _, st := range proseStyles {
			st.start += out.Len()
			styles = append(styles, st)
		}
		out.WriteString(plain)
		if loc == nil {
			break
		}
		inner := strings.TrimRight(rest[loc[2]:loc[3]], "\n")
		if inner != "" {
			styles = append(styles, textStyle{out.Len(), len(inner), styleMonospace})
		}
		out.WriteString(inner)
		rest = rest[loc[1]:]
	}
	plain := out.String()
	rendered := make([]textStyle, 0, len(styles))
	for _, st := range styles {
		if st.length <= 0 || st.start < 0 || st.start+st.length > len(plain) {
			continue
		}
		rendered = append(rendered, textStyle{utf16Units(plain[:st.start]), utf16Units(plain[st.start : st.start+st.length]), st.style})
	}
	sort.SliceStable(rendered, func(i, j int) bool {
		a, b := rendered[i], rendered[j]
		if a.start != b.start {
			return a.start < b.start
		}
		if a.length != b.length {
			return a.length < b.length
		}
		return a.style < b.style
	})
	return plain, rendered
}

// End of a heading marker at a line start, one to six hashes then whitespace
func headingMarkerEnd(s string, i int) (int, bool) {
	k := 0
	for i+k < len(s) && s[i+k] == '#' {
		k++
	}
	if k == 0 || k > 6 {
		return 0, false
	}
	end := i + k
	for end < len(s) {
		r, size := utf8.DecodeRuneInString(s[end:])
		if !isSpace(r) {
			break
		}
		end += size
	}
	return end, end > i+k
}

// Prose with heading markers and inline markers removed and their bold, italic, strike and monospace ranges in bytes
func formatProse(s string, atLineStart bool) (string, []textStyle) {
	var b strings.Builder
	var headings []textStyle
	last := 0
	for i := 0; i < len(s); i++ {
		if !(i == 0 && atLineStart || i > 0 && s[i-1] == '\n') {
			continue
		}
		end, ok := headingMarkerEnd(s, i)
		if !ok {
			continue
		}
		eol := strings.IndexByte(s[end:], '\n')
		if eol < 0 {
			eol = len(s)
		} else {
			eol += end
		}
		b.WriteString(s[last:i])
		if eol > end {
			headings = append(headings, textStyle{b.Len(), eol - end, styleBold})
		}
		b.WriteString(s[end:eol])
		last, i = eol, eol
	}
	b.WriteString(s[last:])
	return stripInline(b.String(), headings)
}

// Inline marker rule, open and close the same marker with optional newline spanning and lookaround checks
type inlineRule struct {
	marker  string
	dotall  bool
	style   string
	openOK  func(s string, i int) bool
	closeOK func(s string, i int) bool
}

// Rune before byte offset i, zero at the start
func runeBefore(s string, i int) rune {
	if i <= 0 {
		return 0
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return r
}

// Rune at byte offset i, zero at the end
func runeAt(s string, i int) rune {
	if i >= len(s) {
		return 0
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return r
}

// Inline rules in precedence order, an earlier rule claiming a span first
var inlineRules = []inlineRule{
	{marker: "**", dotall: true, style: styleBold},
	{marker: "__", dotall: true, style: styleBold},
	{marker: "~~", dotall: true, style: styleStrike},
	{marker: "`", style: styleMonospace},
	{marker: "*", style: styleItalic,
		openOK: func(s string, i int) bool {
			return (i == 0 || s[i-1] != '*') && i+1 < len(s) && s[i+1] != '*' && s[i+1] != ' '
		},
		closeOK: func(s string, i int) bool { return s[i-1] != '*' && (i+1 >= len(s) || s[i+1] != '*') }},
	{marker: "_", style: styleItalic,
		openOK: func(s string, i int) bool {
			return !isWordRune(runeBefore(s, i)) && i+1 < len(s) && s[i+1] != '_'
		},
		closeOK: func(s string, i int) bool { return s[i-1] != '_' && !isWordRune(runeAt(s, i+1)) }},
}

// One claimed inline span, outer marker bounds and inner text bounds in bytes
type inlineSpan struct {
	start, end, innerStart, innerEnd int
	style                            string
}

// Leftmost non overlapping matches of a rule with the shortest inner text of at least one character
func (r inlineRule) matches(s string) []inlineSpan {
	var spans []inlineSpan
	m := len(r.marker)
	for p := 0; p+m <= len(s); {
		if s[p:p+m] != r.marker || r.openOK != nil && !r.openOK(s, p) {
			p++
			continue
		}
		inner := p + m
		found := -1
		for e := inner + 1; e+m <= len(s); e++ {
			if !r.dotall && s[e-1] == '\n' {
				break
			}
			if s[e:e+m] == r.marker && (r.closeOK == nil || r.closeOK(s, e)) {
				found = e
				break
			}
		}
		if found < 0 {
			p++
			continue
		}
		spans = append(spans, inlineSpan{p, found + m, inner, found, r.style})
		p = found + m
	}
	return spans
}

// Inline markers removed with their ranges recorded, prior ranges shifted past the removed markers
func stripInline(s string, prior []textStyle) (string, []textStyle) {
	var claimed []inlineSpan
	for _, rule := range inlineRules {
		for _, span := range rule.matches(s) {
			overlaps := false
			for _, c := range claimed {
				if span.start < c.end && span.end > c.start {
					overlaps = true
					break
				}
			}
			if !overlaps {
				claimed = append(claimed, span)
			}
		}
	}
	sort.Slice(claimed, func(i, j int) bool { return claimed[i].start < claimed[j].start })
	type removal struct{ pos, length int }
	var removals []removal
	var b strings.Builder
	var styles []textStyle
	last := 0
	for _, c := range claimed {
		removals = append(removals, removal{c.start, c.innerStart - c.start}, removal{c.innerEnd, c.end - c.innerEnd})
		b.WriteString(s[last:c.start])
		styles = append(styles, textStyle{b.Len(), c.innerEnd - c.innerStart, c.style})
		b.WriteString(s[c.innerStart:c.innerEnd])
		last = c.end
	}
	b.WriteString(s[last:])
	shift := func(pos int) int {
		moved := 0
		for _, r := range removals {
			if r.pos >= pos {
				break
			}
			moved += min(r.length, pos-r.pos)
		}
		return pos - moved
	}
	for _, p := range prior {
		start, end := shift(p.start), shift(p.start+p.length)
		if end > start {
			styles = append(styles, textStyle{start, end - start, p.style})
		}
	}
	return b.String(), styles
}

// One outbound message body with its style ranges
type formattedChunk struct {
	text   string
	styles []textStyle
}

// Styles clipped to one chunk window in UTF-16 units and rebased to the chunk start
func stylesForChunk(styles []textStyle, chunkStart, chunkEnd int) []textStyle {
	var out []textStyle
	for _, st := range styles {
		start, end := max(st.start, chunkStart), min(st.start+st.length, chunkEnd)
		if start < end {
			out = append(out, textStyle{start - chunkStart, end - start, st.style})
		}
	}
	return out
}

// Splits formatted text into chunks within the UTF-16 limit, preferring line then word breaks, numbering several
func splitFormatted(text string, styles []textStyle, maxUnits int) []formattedChunk {
	if utf16Units(text) <= maxUnits {
		return []formattedChunk{{text, styles}}
	}
	bodyLimit := max(1, maxUnits-10)
	runes := []rune(text)
	offsets := make([]int, len(runes)+1)
	for i, r := range runes {
		n := utf16.RuneLen(r)
		if n < 1 {
			n = 1
		}
		offsets[i+1] = offsets[i] + n
	}
	total := offsets[len(runes)]
	var chunks []formattedChunk
	for start := 0; offsets[start] < total; {
		budget := min(total, offsets[start]+bodyLimit)
		end := start + 1
		for end < len(offsets) && offsets[end] <= budget {
			end++
		}
		end = max(end-1, start+1)
		cut, next := end, end
		if end < len(runes) {
			cut, next = preferredBreak(runes, start, end)
		}
		chunks = append(chunks, formattedChunk{string(runes[start:cut]), stylesForChunk(styles, offsets[start], offsets[cut])})
		start = next
	}
	if len(chunks) > 1 {
		for i := range chunks {
			chunks[i].text += fmt.Sprintf(" (%d/%d)", i+1, len(chunks))
		}
	}
	return chunks
}

// Cut before the last newline or space in the back half of the window, the separator dropped
func preferredBreak(runes []rune, start, end int) (cut, next int) {
	floor := start + (end-start)/2
	for _, sep := range []rune{'\n', ' '} {
		for i := end; i > floor; i-- {
			if runes[i] == sep {
				return i, i + 1
			}
		}
	}
	return end, end
}
