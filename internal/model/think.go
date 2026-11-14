package model

import (
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
)

var (
	thinkTagNames  = []string{"think", "thinking", "reasoning", "thought", "REASONING_SCRATCHPAD", "思考", "反思", "推理", "推敲"}
	thinkOpenTags  = tagForms("<", ">")
	thinkCloseTags = tagForms("</", ">")
	thinkAllTags   = append(append([]string{}, thinkOpenTags...), thinkCloseTags...)
	thinkMaxTagLen = maxLen(thinkAllTags)
	inlineThinkRes = inlineReasoningPatterns()
	plainThinkRe   = regexp.MustCompile(`(?s)<think>(.*?)</think>`)
)

func tagForms(open, end string) []string {
	out := make([]string, len(thinkTagNames))
	for i, name := range thinkTagNames {
		out[i] = open + asciiLower(name) + end
	}
	return out
}

func maxLen(tags []string) int {
	n := 0
	for _, t := range tags {
		n = max(n, len(t))
	}
	return n
}

func inlineReasoningPatterns() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(thinkTagNames))
	for i, name := range thinkTagNames {
		q := regexp.QuoteMeta(name)
		out[i] = regexp.MustCompile(`(?is)<` + q + `>(.*?)</` + q + `>`)
	}
	return out
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

type thinkScrubber struct {
	inBlock     bool
	buf         string
	lastNewline bool
	lastHidden  string
}

func newThinkScrubber() *thinkScrubber { return &thinkScrubber{lastNewline: true} }

func (t *thinkScrubber) emit(out *[]string, text string) {
	text = stripOrphanCloseTags(text)
	if text != "" {
		*out = append(*out, text)
		t.lastNewline = strings.HasSuffix(text, "\n")
	}
}

func (t *thinkScrubber) feed(text string) string {
	t.lastHidden = ""
	if text == "" {
		return ""
	}
	buf := t.buf + text
	t.buf = ""
	var out, hidden []string
	for buf != "" {
		if t.inBlock {
			idx, n := firstTag(buf, thinkCloseTags)
			if idx < 0 {
				hidden = append(hidden, t.holdPartial(buf, thinkCloseTags))
				break
			}
			hidden = append(hidden, buf[:idx])
			buf = buf[idx+n:]
			t.inBlock = false
			continue
		}
		pairStart, pairEnd, havePair := earliestClosedPair(buf)
		openIdx, openLen := t.openAtBoundary(buf, out)
		if havePair && (openIdx < 0 || pairStart <= openIdx) {
			t.emit(&out, buf[:pairStart])
			gt := strings.IndexByte(buf[pairStart:], '>') + pairStart
			lt := strings.LastIndexByte(buf[pairStart:pairEnd], '<') + pairStart
			hidden = append(hidden, buf[gt+1:lt])
			buf = buf[pairEnd:]
			continue
		}
		if openIdx >= 0 {
			t.emit(&out, buf[:openIdx])
			t.inBlock = true
			buf = buf[openIdx+openLen:]
			continue
		}
		t.emit(&out, t.holdPartial(buf, thinkAllTags))
		break
	}
	t.lastHidden = strings.Join(hidden, "")
	return strings.Join(out, "")
}

func (t *thinkScrubber) holdPartial(buf string, tags []string) string {
	held := maxPartialSuffix(buf, tags)
	if held == 0 {
		t.buf = ""
		return buf
	}
	t.buf = buf[len(buf)-held:]
	return buf[:len(buf)-held]
}

func (t *thinkScrubber) flush() string {
	tail := t.buf
	if t.inBlock {
		tail = ""
	}
	t.buf, t.inBlock, t.lastNewline = "", false, true
	if tail == "" {
		return ""
	}
	return stripOrphanCloseTags(tail)
}

func firstTag(buf string, tags []string) (int, int) {
	lower := asciiLower(buf)
	best, bestLen := -1, 0
	for _, tag := range tags {
		if idx := strings.Index(lower, tag); idx >= 0 && (best < 0 || idx < best || (idx == best && len(tag) < bestLen)) {
			best, bestLen = idx, len(tag)
		}
	}
	return best, bestLen
}

func earliestClosedPair(buf string) (int, int, bool) {
	lower := asciiLower(buf)
	start, end, found := 0, 0, false
	for i, open := range thinkOpenTags {
		o := strings.Index(lower, open)
		if o < 0 {
			continue
		}
		c := strings.Index(lower[o+len(open):], thinkCloseTags[i])
		if c < 0 {
			continue
		}
		e := o + len(open) + c + len(thinkCloseTags[i])
		if !found || o < start || (o == start && e < end) {
			start, end, found = o, e, true
		}
	}
	return start, end, found
}

func (t *thinkScrubber) openAtBoundary(buf string, emitted []string) (int, int) {
	lower := asciiLower(buf)
	best, bestLen := -1, 0
	for _, tag := range thinkOpenTags {
		idx := strings.Index(lower, tag)
		for idx >= 0 && !t.blockBoundary(buf, idx, emitted) {
			next := strings.Index(lower[idx+1:], tag)
			if next < 0 {
				idx = -1
				break
			}
			idx += 1 + next
		}
		if idx >= 0 && (best < 0 || idx < best || (idx == best && len(tag) < bestLen)) {
			best, bestLen = idx, len(tag)
		}
	}
	return best, bestLen
}

func (t *thinkScrubber) blockBoundary(buf string, idx int, emitted []string) bool {
	priorNewline := t.lastNewline
	if len(emitted) > 0 {
		priorNewline = strings.HasSuffix(emitted[len(emitted)-1], "\n")
	}
	if idx == 0 {
		return priorNewline
	}
	preceding := buf[:idx]
	lastNL := strings.LastIndexByte(preceding, '\n')
	if lastNL < 0 && !priorNewline {
		return false
	}
	return strings.TrimSpace(preceding[lastNL+1:]) == ""
}

func maxPartialSuffix(buf string, tags []string) int {
	lower := asciiLower(buf)
	for i := min(len(lower), thinkMaxTagLen-1); i > 0; i-- {
		suffix := lower[len(lower)-i:]
		for _, tag := range tags {
			if len(tag) > i && strings.HasPrefix(tag, suffix) {
				return i
			}
		}
	}
	return 0
}

func stripOrphanCloseTags(text string) string {
	if !strings.Contains(text, "</") {
		return text
	}
	lower := asciiLower(text)
	var b strings.Builder
	i := 0
	for i < len(text) {
		matched := 0
		if text[i] == '<' {
			for _, tag := range thinkCloseTags {
				if strings.HasPrefix(lower[i:], tag) {
					matched = len(tag)
					break
				}
			}
		}
		if matched == 0 {
			b.WriteByte(text[i])
			i++
			continue
		}
		i += matched
		for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\n' || text[i] == '\r') {
			i++
		}
	}
	return b.String()
}

func flattenReasoningText(raw []byte) string {
	v, err := wire.ParseValue(raw)
	if err != nil {
		return ""
	}
	if s, ok := v.Str(); ok {
		return s
	}
	return wire.FlattenContent(raw, "")
}

func extractReasoning(m *wire.Message) string {
	var parts []string
	add := func(text string) {
		if text == "" {
			return
		}
		for _, p := range parts {
			if p == text {
				return
			}
		}
		parts = append(parts, text)
	}
	add(m.Reasoning)
	if m.ReasoningContent != nil {
		add(*m.ReasoningContent)
	}
	for _, d := range m.ReasoningDetails {
		v, err := wire.ParseValue(d)
		if err != nil || !v.IsObject() {
			continue
		}
		for _, key := range []string{"summary", "thinking", "content", "text"} {
			if f := v.Get(key); f.Truthy() {
				add(flattenReasoningText(f.Raw()))
				break
			}
		}
	}
	if len(parts) == 0 && m.Parts != nil {
		for _, p := range m.Parts {
			raw := p.JSON()
			v, err := wire.ParseValue(raw)
			if err != nil || !v.IsObject() || v.Get("type").Text() != "thinking" {
				continue
			}
			field := v.Get("thinking")
			if !field.Truthy() {
				field = v.Get("text")
			}
			if field.Truthy() {
				add(strings.TrimSpace(flattenReasoningText(field.Raw())))
			}
		}
	}
	if len(parts) == 0 && m.Parts == nil && m.Text != "" {
		for _, re := range inlineThinkRes {
			for _, match := range re.FindAllStringSubmatch(m.Text, -1) {
				add(strings.TrimSpace(match[1]))
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func plainThinkBlocks(content string) string {
	var blocks []string
	for _, match := range plainThinkRe.FindAllStringSubmatch(content, -1) {
		if b := strings.TrimSpace(match[1]); b != "" {
			blocks = append(blocks, b)
		}
	}
	return strings.Join(blocks, "\n\n")
}
