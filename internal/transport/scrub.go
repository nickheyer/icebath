package transport

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Tag names models wrap their reasoning in inside the reply's text
var ThinkTags = []string{"think", "thinking", "reasoning", "thought", "REASONING_SCRATCHPAD", "思考", "反思", "推理", "推敲"}

var (
	openTags, closeTags, allTags [][]rune
	longestTag                   int
	orphanClose                  *regexp.Regexp
	inlineTags                   []*regexp.Regexp
)

func init() {
	var closers []string
	for _, name := range ThinkTags {
		lower := strings.ToLower(name)
		openTags = append(openTags, []rune("<"+lower+">"))
		closeTags = append(closeTags, []rune("</"+lower+">"))
		closers = append(closers, regexp.QuoteMeta("</"+lower+">"))
		inlineTags = append(inlineTags, regexp.MustCompile(`(?is)<`+regexp.QuoteMeta(name)+`>(.*?)</`+regexp.QuoteMeta(name)+`>`))
	}
	allTags = append(slices.Clone(openTags), closeTags...)
	for _, tag := range allTags {
		longestTag = max(longestTag, len(tag))
	}
	orphanClose = regexp.MustCompile("(?i)(?:" + strings.Join(closers, "|") + ")[ \t\n\r]*")
}

// Reasoning blocks of a text joined with blank lines, each once
func InlineReasoning(text string) string {
	var parts []string
	for _, pattern := range inlineTags {
		for _, m := range pattern.FindAllStringSubmatch(text, -1) {
			if part := strings.TrimSpace(m[1]); part != "" && !slices.Contains(parts, part) {
				parts = append(parts, part)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// Hides reasoning blocks from streamed reply text across the deltas they are split over; a block opens only at the start of a line
type Scrubber struct {
	inBlock bool
	held    []rune
	newline bool
	hidden  string
}

func NewScrubber() *Scrubber {
	return &Scrubber{newline: true}
}

// Reasoning the latest Feed took out of the text
func (s *Scrubber) Hidden() string {
	return s.hidden
}

// Visible part of one delta, empty when it is all reasoning or held back as the start of a tag
func (s *Scrubber) Feed(text string) string {
	s.hidden = ""
	if text == "" {
		return ""
	}
	buf := append(s.held, []rune(text)...)
	s.held = nil
	var out, hidden []string
	for len(buf) > 0 {
		if s.inBlock {
			at, size := firstTag(buf, closeTags)
			if at < 0 {
				hidden = append(hidden, string(s.hold(buf, closeTags)))
				break
			}
			hidden = append(hidden, string(buf[:at]))
			buf = buf[at+size:]
			s.inBlock = false
			continue
		}
		start, end, paired := closedPair(buf)
		open, size := s.openAtBoundary(buf, out)
		if paired && (open < 0 || start <= open) {
			s.emit(&out, buf[:start])
			inner := index(buf, []rune{'>'}, start) + 1
			innerEnd := lastIndex(buf, '<', start, end)
			hidden = append(hidden, string(buf[inner:innerEnd]))
			buf = buf[end:]
			continue
		}
		if open >= 0 {
			s.emit(&out, buf[:open])
			s.inBlock = true
			buf = buf[open+size:]
			continue
		}
		s.emit(&out, s.hold(buf, allTags))
		break
	}
	s.hidden = strings.Join(hidden, "")
	return strings.Join(out, "")
}

// Held prose released at the end of the stream, an unterminated block dropped
func (s *Scrubber) Flush() string {
	tail := ""
	if !s.inBlock {
		tail = string(s.held)
	}
	s.held, s.inBlock, s.newline = nil, false, true
	return stripOrphans(tail)
}

func (s *Scrubber) emit(out *[]string, text []rune) {
	if t := stripOrphans(string(text)); t != "" {
		*out = append(*out, t)
		s.newline = strings.HasSuffix(t, "\n")
	}
}

// Holds back a trailing start of a tag and gives the rest
func (s *Scrubber) hold(buf []rune, tags [][]rune) []rune {
	n := partialSuffix(buf, tags)
	if n == 0 {
		return buf
	}
	s.held = slices.Clone(buf[len(buf)-n:])
	return buf[:len(buf)-n]
}

// Earliest opening tag at the start of a line whose text before it is blank, -1 for none
func (s *Scrubber) openAtBoundary(buf []rune, emitted []string) (int, int) {
	lower := lowered(buf)
	best, size := -1, 0
	for _, tag := range openTags {
		at := index(lower, tag, 0)
		for at >= 0 && !s.boundary(buf, at, emitted) {
			at = index(lower, tag, at+1)
		}
		if at >= 0 && (best < 0 || at < best || at == best && len(tag) < size) {
			best, size = at, len(tag)
		}
	}
	return best, size
}

func (s *Scrubber) boundary(buf []rune, at int, emitted []string) bool {
	newline := s.newline
	if len(emitted) > 0 {
		newline = strings.HasSuffix(emitted[len(emitted)-1], "\n")
	}
	if at == 0 {
		return newline
	}
	before := buf[:at]
	nl := lastIndex(before, '\n', 0, len(before))
	return (newline || nl >= 0) && strings.TrimSpace(string(before[nl+1:])) == ""
}

func stripOrphans(text string) string {
	if !strings.Contains(text, "</") {
		return text
	}
	return orphanClose.ReplaceAllString(text, "")
}

func lowered(r []rune) []rune {
	out := make([]rune, len(r))
	for i, c := range r {
		out[i] = unicode.ToLower(c)
	}
	return out
}

func index(hay, needle []rune, from int) int {
	for i := max(from, 0); i+len(needle) <= len(hay); i++ {
		if slices.Equal(hay[i:i+len(needle)], needle) {
			return i
		}
	}
	return -1
}

func lastIndex(hay []rune, c rune, from, to int) int {
	for i := to - 1; i >= from; i-- {
		if hay[i] == c {
			return i
		}
	}
	return -1
}

// Earliest of the tags in the buffer, ignoring case, and its length
func firstTag(buf []rune, tags [][]rune) (int, int) {
	lower := lowered(buf)
	best, size := -1, 0
	for _, tag := range tags {
		if at := index(lower, tag, 0); at >= 0 && (best < 0 || at < best || at == best && len(tag) < size) {
			best, size = at, len(tag)
		}
	}
	return best, size
}

// Start and end of the earliest closed block in the buffer
func closedPair(buf []rune) (int, int, bool) {
	lower := lowered(buf)
	start, end, found := 0, 0, false
	for i := range openTags {
		open := index(lower, openTags[i], 0)
		if open < 0 {
			continue
		}
		closing := index(lower, closeTags[i], open+len(openTags[i]))
		if closing < 0 {
			continue
		}
		if e := closing + len(closeTags[i]); !found || open < start || open == start && e < end {
			start, end, found = open, e, true
		}
	}
	return start, end, found
}

// Longest end of the buffer that is the start of a tag but not the whole of it
func partialSuffix(buf []rune, tags [][]rune) int {
	lower := lowered(buf)
	for n := min(longestTag-1, len(lower)); n > 0; n-- {
		suffix := lower[len(lower)-n:]
		for _, tag := range tags {
			if len(tag) > n && slices.Equal(tag[:n], suffix) {
				return n
			}
		}
	}
	return 0
}
