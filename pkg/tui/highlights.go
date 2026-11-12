package tui

import (
	"regexp"
	"sort"
	"unicode/utf8"
)

const jsNonSpace = "[^\\t\\n\\v\\f\\r \\x{00a0}\\x{1680}\\x{2000}-\\x{200a}\\x{2028}\\x{2029}\\x{202f}\\x{205f}\\x{3000}\\x{feff}]"

var (
	pasteSnippetRe = regexp.MustCompile(`\[\[[^\n]*?\]\]`)
	atRefRe        = regexp.MustCompile("^@(?:[\\w-]+:(?:`[^`\\n]*`?|\"[^\"\\n]*\"?|'[^'\\n]*'?|" + jsNonSpace + "*)|" + jsNonSpace + "*)")
)

// One run of composer text, ref when it is a slash command, an at reference or a paste token
type composerHighlight struct {
	Ref  bool
	Text string
}

// Byte span of one highlighted reference
type span struct{ start, end int }

// Offset starts the string or follows whitespace
func afterSpaceOrStart(text string, i int) bool {
	if i == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:i])
	return isJSSpace(r)
}

// Word character or dash of a command name
func isWordOrDash(b byte) bool {
	return b == '_' || b == '-' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// Spans of at references that start the text or follow whitespace
func atRefSpans(text string) []span {
	var out []span
	for i := 0; i < len(text); {
		if text[i] != '@' || !afterSpaceOrStart(text, i) {
			i++
			continue
		}
		m := atRefRe.FindStringIndex(text[i:])
		if m == nil || m[1] == 0 {
			i++
			continue
		}
		out = append(out, span{i, i + m[1]})
		i += m[1]
	}
	return out
}

// Spans of slash commands named at the start or mid prose, not paths, plus a bare trailing slash
func slashSpans(text string) []span {
	var out []span
	for i := 0; i < len(text); {
		if text[i] != '/' || !afterSpaceOrStart(text, i) {
			i++
			continue
		}
		j := i + 1
		if j < len(text) && ((text[j] >= 'a' && text[j] <= 'z') || (text[j] >= 'A' && text[j] <= 'Z')) {
			k := j + 1
			for k < len(text) && isWordOrDash(text[k]) {
				k++
			}
			if k >= len(text) || text[k] != '/' {
				out = append(out, span{i, k})
				i = k
				continue
			}
		} else if j == len(text) {
			out = append(out, span{i, j})
		}
		i++
	}
	return out
}

// Composer text split into plain and reference runs that concatenate back to the input
func splitComposerHighlights(text string) []composerHighlight {
	var all []span
	for _, m := range pasteSnippetRe.FindAllStringIndex(text, -1) {
		if m[1] > m[0] {
			all = append(all, span{m[0], m[1]})
		}
	}
	all = append(all, atRefSpans(text)...)
	all = append(all, slashSpans(text)...)
	sort.SliceStable(all, func(a, b int) bool { return all[a].start < all[b].start })
	var kept []span
	for _, s := range all {
		overlaps := false
		for _, prev := range kept {
			if s.start < prev.end && s.end > prev.start {
				overlaps = true
				break
			}
		}
		if !overlaps {
			kept = append(kept, s)
		}
	}
	var out []composerHighlight
	last := 0
	for _, s := range kept {
		if s.start > last {
			out = append(out, composerHighlight{Text: text[last:s.start]})
		}
		out = append(out, composerHighlight{Ref: true, Text: text[s.start:s.end]})
		last = s.end
	}
	if last < len(text) || len(out) == 0 {
		out = append(out, composerHighlight{Text: text[last:]})
	}
	return out
}

// Per byte accent flags of the text, true inside a reference
func highlightMask(text string) []bool {
	mask := make([]bool, len(text))
	offset := 0
	for _, seg := range splitComposerHighlights(text) {
		if seg.Ref {
			for i := offset; i < offset+len(seg.Text); i++ {
				mask[i] = true
			}
		}
		offset += len(seg.Text)
	}
	return mask
}
