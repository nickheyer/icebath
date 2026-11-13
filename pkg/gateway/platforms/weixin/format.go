package weixin

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway"
)

const (
	maxMessageLength = 2000
	copyLineWidth    = 120
	chattyLineMax    = 48
	chattyLabelMax   = 24
)

var (
	headerRE    = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*$`)
	tableRuleRE = regexp.MustCompile(`^\s*\|?(?:\s*:?-{3,}:?\s*\|)+\s*:?-{3,}:?\s*\|?\s*$`)
	fenceRE     = regexp.MustCompile("^```([^\n`]*)\\s*$")
	boldOnlyRE  = regexp.MustCompile(`^\*\*[^*]+\*\*$`)
	numberedRE  = regexp.MustCompile(`^\p{Nd}+\.\s`)
)

// Code point length of s
func runeLen(s string) int { return utf8.RuneCountInString(s) }

// Text with trailing Unicode whitespace removed
func rstrip(s string) string { return strings.TrimRightFunc(s, unicode.IsSpace) }

// Lines of s split on every Unicode line boundary, a trailing break adding no empty line
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', '\u2028', '\u2029':
			lines = append(lines, s[start:i])
			i += size
			start = i
		case '\r':
			lines = append(lines, s[start:i])
			i += size
			if i < len(s) && s[i] == '\n' {
				i++
			}
			start = i
		default:
			i += size
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// One markdown line with its fence flag and whether it sits inside a code block before any toggle
type markdownLine struct {
	text        string
	isFence     bool
	inCodeBlock bool
}

// Right trimmed lines of content tagged with fence and code block state
func walkMarkdownLines(content string) []markdownLine {
	var out []markdownLine
	inCode := false
	for _, raw := range splitLines(content) {
		line := rstrip(raw)
		isFence := fenceRE.MatchString(strings.TrimSpace(line))
		out = append(out, markdownLine{text: line, isFence: isFence, inCodeBlock: inCode})
		if isFence {
			inCode = !inCode
		}
	}
	return out
}

// Right trims lines and collapses blank runs to one outside fenced code
func normalizeMarkdownBlocks(content string) string {
	var result []string
	blankRun := 0
	for _, l := range walkMarkdownLines(content) {
		if !l.isFence && !l.inCodeBlock && strings.TrimSpace(l.text) == "" {
			blankRun++
			if blankRun <= 1 {
				result = append(result, "")
			}
			continue
		}
		if l.isFence || !l.inCodeBlock {
			blankRun = 0
		}
		result = append(result, l.text)
	}
	return strings.TrimSpace(strings.Join(result, "\n"))
}

// Wraps long prose lines that WeChat clients make hard to copy, leaving code and table lines whole
func wrapCopyFriendlyLines(content string) string {
	if content == "" {
		return content
	}
	var wrapped []string
	for _, l := range walkMarkdownLines(content) {
		stripped := strings.TrimSpace(l.text)
		keep := l.isFence || l.inCodeBlock || runeLen(l.text) <= copyLineWidth || stripped == "" || strings.HasPrefix(stripped, "|")
		if keep || tableRuleRE.MatchString(stripped) {
			wrapped = append(wrapped, l.text)
			continue
		}
		if lines := wrapLine(l.text, copyLineWidth); len(lines) > 0 {
			wrapped = append(wrapped, lines...)
		} else {
			wrapped = append(wrapped, l.text)
		}
	}
	return strings.TrimSpace(strings.Join(wrapped, "\n"))
}

// Text with tabs expanded to the next multiple of eight columns
func expandTabs(s string) string {
	if !strings.Contains(s, "\t") {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		switch r {
		case '\t':
			n := 8 - col%8
			b.WriteString(strings.Repeat(" ", n))
			col += n
		case '\n', '\r':
			b.WriteRune(r)
			col = 0
		default:
			b.WriteRune(r)
			col++
		}
	}
	return b.String()
}

// Whether r is one of the ASCII whitespace characters word wrapping splits on
func isWrapSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ':
		return true
	}
	return false
}

// Alternating runs of wrap whitespace and words
func wrapChunks(s string) []string {
	var chunks []string
	start := 0
	prevSpace := false
	for i, r := range s {
		space := isWrapSpace(r)
		if i > start && space != prevSpace {
			chunks = append(chunks, s[start:i])
			start = i
		}
		prevSpace = space
	}
	if start < len(s) {
		chunks = append(chunks, s[start:])
	}
	return chunks
}

// Greedy word wrap at width code points keeping long words whole and dropping whitespace at line edges
func wrapLine(line string, width int) []string {
	chunks := wrapChunks(expandTabs(line))
	blank := func(c string) bool { return strings.TrimFunc(c, isWrapSpace) == "" }
	var lines []string
	i := 0
	for i < len(chunks) {
		if len(lines) > 0 && blank(chunks[i]) {
			i++
		}
		var cur []string
		curLen := 0
		for i < len(chunks) && curLen+runeLen(chunks[i]) <= width {
			cur = append(cur, chunks[i])
			curLen += runeLen(chunks[i])
			i++
		}
		if i < len(chunks) && runeLen(chunks[i]) > width && len(cur) == 0 {
			cur = append(cur, chunks[i])
			i++
		}
		if len(cur) > 0 && blank(cur[len(cur)-1]) {
			cur = cur[:len(cur)-1]
		}
		if len(cur) > 0 {
			lines = append(lines, strings.Join(cur, ""))
		}
	}
	return lines
}

// Blank line separated blocks with fenced code kept intact as its own block
func splitMarkdownBlocks(content string) []string {
	var blocks, current []string
	flush := func() {
		if len(current) > 0 {
			blocks = append(blocks, strings.TrimSpace(strings.Join(current, "\n")))
			current = current[:0]
		}
	}
	for _, l := range walkMarkdownLines(content) {
		if l.isFence && !l.inCodeBlock {
			flush()
		}
		if l.isFence || l.inCodeBlock || strings.TrimSpace(l.text) != "" {
			current = append(current, l.text)
		} else {
			flush()
		}
		if l.isFence && l.inCodeBlock {
			flush()
		}
	}
	flush()
	out := blocks[:0]
	for _, b := range blocks {
		if b != "" {
			out = append(out, b)
		}
	}
	return out
}

// Top level lines as separate units, fenced code intact and indented continuations joined to their line
func splitDeliveryUnits(content string) []string {
	var units []string
	for _, block := range splitMarkdownBlocks(content) {
		lines := splitLines(block)
		if fenceRE.MatchString(strings.TrimSpace(lines[0])) {
			units = append(units, block)
			continue
		}
		var current []string
		for _, raw := range lines {
			line := rstrip(raw)
			if len(current) > 0 && strings.TrimSpace(line) != "" && (strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t")) {
				current = append(current, line)
				continue
			}
			units = append(units, strings.TrimSpace(strings.Join(current, "\n")))
			current = nil
			if strings.TrimSpace(line) != "" {
				current = []string{line}
			}
		}
		units = append(units, strings.TrimSpace(strings.Join(current, "\n")))
	}
	out := units[:0]
	for _, u := range units {
		if u != "" {
			out = append(out, u)
		}
	}
	return out
}

// Whether a line reads like a short conversational remark rather than structure
func looksLikeChattyLine(line string) bool {
	stripped := strings.TrimSpace(line)
	if stripped == "" || runeLen(stripped) > chattyLineMax || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
		return false
	}
	for _, p := range []string{">", "-", "*", "【", "#", "|"} {
		if strings.HasPrefix(stripped, p) {
			return false
		}
	}
	return !tableRuleRE.MatchString(stripped) && !boldOnlyRE.MatchString(stripped) && !numberedRE.MatchString(stripped)
}

// Whether a block of two to six chatty lines without a heading should go out as separate bubbles
func shouldSplitShortChatBlock(block string) bool {
	var lines []string
	for _, l := range splitLines(block) {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) < 2 || len(lines) > 6 {
		return false
	}
	first := strings.TrimSpace(lines[0])
	if headerRE.MatchString(first) || (runeLen(first) <= chattyLabelMax && (strings.HasSuffix(first, ":") || strings.HasSuffix(first, "："))) {
		return false
	}
	for _, l := range lines {
		if !looksLikeChattyLine(l) {
			return false
		}
	}
	return true
}

// Markdown blocks packed into chunks within maxLength, an oversized block split fence aware
func packMarkdownBlocks(content string, maxLength int) []string {
	if runeLen(content) <= maxLength {
		return []string{content}
	}
	overflow := func(block string) []string { return gateway.TruncateMessage(block, maxLength, nil) }
	return gateway.GreedyPackBlocks(splitMarkdownBlocks(content), maxLength, nil, "\n\n", overflow)
}

// Message chunks for delivery, compact by default or one per top level line when splitPerLine is set
func splitForDelivery(content string, maxLength int, splitPerLine bool) []string {
	if content == "" {
		return nil
	}
	if splitPerLine {
		if runeLen(content) <= maxLength && !strings.Contains(content, "\n") {
			return []string{content}
		}
		var chunks []string
		for _, unit := range splitDeliveryUnits(content) {
			if runeLen(unit) <= maxLength {
				chunks = append(chunks, unit)
				continue
			}
			chunks = append(chunks, packMarkdownBlocks(unit, maxLength)...)
		}
		out := chunks[:0]
		for _, c := range chunks {
			if c != "" {
				out = append(out, c)
			}
		}
		if len(out) == 0 {
			return []string{content}
		}
		return out
	}
	if runeLen(content) > maxLength {
		if packed := packMarkdownBlocks(content, maxLength); len(packed) > 0 {
			return packed
		}
		return []string{content}
	}
	if shouldSplitShortChatBlock(content) {
		return splitDeliveryUnits(content)
	}
	return []string{content}
}

// Markdown normalized and long prose lines wrapped for WeChat clients
func formatForWeixin(content string) string {
	return wrapCopyFriendlyLines(normalizeMarkdownBlocks(content))
}
