package memory

import (
	"log/slog"
	"regexp"
	"strings"
)

// Python re whitespace class for str patterns, every code point str isspace accepts
const pySpaceClass = `\t\n\x0b\f\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

var (
	fenceTagRe        = regexp.MustCompile(`(?i)</?[` + pySpaceClass + `]*memory-context[` + pySpaceClass + `]*>`)
	internalContextRe = regexp.MustCompile(`(?i)<[` + pySpaceClass + `]*memory-context[` + pySpaceClass + `]*>(?s:.)*?</[` + pySpaceClass + `]*memory-context[` + pySpaceClass + `]*>`)
	internalNoteRe    = regexp.MustCompile(`(?i)\[System note:[` + pySpaceClass + `]*The following is recalled memory context,[` + pySpaceClass + `]*NOT new user input\.[` + pySpaceClass + `]*Treat as (?:informational background data|authoritative reference data[^\]]*)\.\][` + pySpaceClass + `]*`)
	recallBulletRe    = regexp.MustCompile(`^[-*+][` + pySpaceClass + `]+[^` + pySpaceClass + `]`)
)

// Strips fence tags, injected context blocks and system notes from provider output
func SanitizeContext(text string) string {
	for _, re := range []*regexp.Regexp{internalContextRe, internalNoteRe, fenceTagRe} {
		text = re.ReplaceAllLiteralString(text, "")
	}
	return text
}

const (
	openContextTag  = "<memory-context>"
	closeContextTag = "</memory-context>"
)

// StreamingContextScrubber drops memory context spans that straddle streaming deltas, one per response
type StreamingContextScrubber struct {
	inSpan          bool
	buf             []rune
	atBlockBoundary bool
}

// NewStreamingContextScrubber returns a scrubber at a block boundary outside any span
func NewStreamingContextScrubber() *StreamingContextScrubber {
	s := &StreamingContextScrubber{}
	s.Reset()
	return s
}

// Clears span state and the held tail for a new top level response
func (s *StreamingContextScrubber) Reset() {
	s.inSpan = false
	s.buf = nil
	s.atBlockBoundary = true
}

// Visible portion of text, holding a possible partial tag tail for the next call
func (s *StreamingContextScrubber) Feed(text string) string {
	if text == "" {
		return ""
	}
	buf := append(append([]rune(nil), s.buf...), []rune(text)...)
	s.buf = nil
	var out strings.Builder
	for len(buf) > 0 {
		var tag []rune
		var idx, held int
		if s.inSpan {
			tag = []rune(closeContextTag)
			idx = runeFind(lowerRunes(buf), tag, 0)
			held = maxPartialSuffix(buf, tag)
		} else {
			tag = []rune(openContextTag)
			idx = s.findBoundaryOpenTag(buf)
			n := len(tag)
			pending := 0
			if runeHasSuffix(lowerRunes(buf), tag) && s.endsAtBlockBoundary(sliceTo(buf, -n)) {
				pending = n
			}
			held = pending
			if held == 0 {
				held = maxPartialSuffix(buf, tag)
			}
		}
		if idx == -1 {
			if !s.inSpan {
				visible := buf
				if held > 0 {
					visible = sliceTo(buf, -held)
				}
				s.appendVisible(&out, visible)
			}
			if held > 0 {
				s.buf = append([]rune(nil), sliceFrom(buf, -held)...)
			} else {
				s.buf = nil
			}
			break
		}
		if !s.inSpan {
			s.appendVisible(&out, sliceTo(buf, idx))
		}
		buf = sliceFrom(buf, idx+len(tag))
		s.inSpan = !s.inSpan
	}
	return out.String()
}

// Held back tail at end of stream, discarded inside an unterminated span
func (s *StreamingContextScrubber) Flush() string {
	tail := ""
	if !s.inSpan {
		tail = string(s.buf)
	}
	s.buf = nil
	s.inSpan = false
	return tail
}

// Length of the longest buffer suffix that is a case insensitive prefix of tag, zero when none
func maxPartialSuffix(buf, tag []rune) int {
	tagLower, bufLower := lowerRunes(tag), lowerRunes(buf)
	for i := min(len(bufLower), len(tagLower)-1); i > 0; i-- {
		if runeHasPrefix(tagLower, sliceFrom(bufLower, -i)) {
			return i
		}
	}
	return 0
}

// Opening fence index only where it starts a block, at a line start with a newline after it
func (s *StreamingContextScrubber) findBoundaryOpenTag(buf []rune) int {
	bufLower := lowerRunes(buf)
	tag := []rune(openContextTag)
	idx := runeFind(bufLower, tag, 0)
	for idx != -1 {
		after := idx + len(tag)
		if s.endsAtBlockBoundary(sliceTo(buf, idx)) && after < len(buf) && (buf[after] == '\r' || buf[after] == '\n') {
			return idx
		}
		idx = runeFind(bufLower, tag, idx+1)
	}
	return -1
}

// Whether emitting text leaves the stream at a line start
func (s *StreamingContextScrubber) endsAtBlockBoundary(text []rune) bool {
	last := -1
	for i := len(text) - 1; i >= 0; i-- {
		if text[i] == '\n' {
			last = i
			break
		}
	}
	tail := text[last+1:]
	return pyStrip(string(tail)) == "" && (last >= 0 || s.atBlockBoundary)
}

func (s *StreamingContextScrubber) appendVisible(out *strings.Builder, text []rune) {
	if len(text) > 0 {
		out.WriteString(string(text))
		s.atBlockBoundary = s.endsAtBlockBoundary(text)
	}
}

// Python lower over a rune slice, which may change length for the dotted capital I
func lowerRunes(rs []rune) []rune {
	return []rune(pyLower(string(rs)))
}

// Python slice end resolution with a negative index counted from the end
func pySliceIndex(n, i int) int {
	if i < 0 {
		i += n
	}
	return max(0, min(i, n))
}

func sliceTo(rs []rune, i int) []rune { return rs[:pySliceIndex(len(rs), i)] }

func sliceFrom(rs []rune, i int) []rune { return rs[pySliceIndex(len(rs), i):] }

// First index of needle in hay at or after start, -1 when absent
func runeFind(hay, needle []rune, start int) int {
	for i := max(start, 0); i+len(needle) <= len(hay); i++ {
		if runeHasPrefix(hay[i:], needle) {
			return i
		}
	}
	return -1
}

func runeHasPrefix(s, prefix []rune) bool {
	if len(prefix) > len(s) {
		return false
	}
	for i, r := range prefix {
		if s[i] != r {
			return false
		}
	}
	return true
}

func runeHasSuffix(s, suffix []rune) bool {
	return len(suffix) <= len(s) && runeHasPrefix(s[len(s)-len(suffix):], suffix)
}

// Drops a self contained recalled bullet an earlier line of the same section already states
func dropRepeatedRecallLines(text string) string {
	lines := strings.Split(text, "\n")
	seen := map[string]bool{}
	kept := make([]string, 0, len(lines))
	for index, line := range lines {
		stripped := pyStrip(line)
		if stripped != "" && startsWithSpace(line) {
			kept = append(kept, line)
			continue
		}
		isBullet := recallBulletRe.MatchString(stripped)
		if stripped != "" && !isBullet {
			seen = map[string]bool{}
		}
		if isBullet {
			following := ""
			if index+1 < len(lines) {
				following = lines[index+1]
			}
			carriesContinuation := pyStrip(following) != "" && startsWithSpace(following)
			if !carriesContinuation {
				if seen[stripped] {
					continue
				}
				seen[stripped] = true
			}
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func startsWithSpace(s string) bool {
	for _, r := range s {
		return pyIsSpace(r)
	}
	return false
}

// Prefetched memory wrapped in a fenced block with the system note, empty for blank input
func BuildMemoryContextBlock(rawContext string) string {
	if pyStrip(rawContext) == "" {
		return ""
	}
	sanitized := SanitizeContext(rawContext)
	if sanitized != rawContext {
		slog.Default().Warn("memory provider returned pre-wrapped context; stripped")
	}
	clean := dropRepeatedRecallLines(sanitized)
	return "<memory-context>\n" +
		"[System note: The following is recalled memory context, " +
		"NOT new user input. Treat as authoritative reference data — " +
		"this is the agent's persistent memory and should inform all responses.]\n\n" +
		clean + "\n" +
		"</memory-context>"
}

// Memory context fencing registers nothing at import
func registerMemoryContextFencing() {}
