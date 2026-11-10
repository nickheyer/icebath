package pystr

import (
	"strings"

	"github.com/dlclark/regexp2/v2"
)

// TextwrapOptions are the textwrap.TextWrapper keyword arguments
type TextwrapOptions struct {
	Width             int
	InitialIndent     string
	SubsequentIndent  string
	ExpandTabs        bool
	ReplaceWhitespace bool
	DropWhitespace    bool
	BreakLongWords    bool
	BreakOnHyphens    bool
	TabSize           int
}

// DefaultTextwrap is TextWrapper(width) with every other argument at its Python default
func DefaultTextwrap(width int) TextwrapOptions {
	return TextwrapOptions{Width: width, ExpandTabs: true, ReplaceWhitespace: true, DropWhitespace: true, BreakLongWords: true, BreakOnHyphens: true, TabSize: 8}
}

const textwrapWhitespace = "\t\n\x0b\x0c\r "

var (
	textwrapWordsepRe = regexp2.MustCompile(`([\t\n\x0b\x0c\r ]+|(?<=[\w!"'&.,?])-{2,}(?=\w)|[^\t\n\x0b\x0c\r ]+?(?:-(?:(?<=[^\d\W]{2}-)|(?<=[^\d\W]-[^\d\W]-))(?=[^\d\W]-?[^\d\W])|(?=[\t\n\x0b\x0c\r ]|\z)|(?<=[\w!"'&.,?])(?=-{2,}\w)))`, regexp2.None)
	textwrapSimpleRe  = regexp2.MustCompile(`([\t\n\x0b\x0c\r ]+)`, regexp2.None)
)

// TextwrapWrap is textwrap.wrap with the given TextWrapper options
func TextwrapWrap(text string, o TextwrapOptions) []string {
	chunks := textwrapSplitChunks(textwrapMungeWhitespace(text, o), o)
	return textwrapWrapChunks(chunks, o)
}

// TextwrapWrapNoHyphenBreak is textwrap.wrap with break_on_hyphens False and the other options at their defaults
func TextwrapWrapNoHyphenBreak(text string, width int, breakLongWords bool) []string {
	o := DefaultTextwrap(width)
	o.BreakOnHyphens = false
	o.BreakLongWords = breakLongWords
	return TextwrapWrap(text, o)
}

// Tabs expanded and whitespace characters replaced by spaces as TextWrapper._munge_whitespace does
func textwrapMungeWhitespace(text string, o TextwrapOptions) string {
	if o.ExpandTabs {
		text = expandTabs(text, o.TabSize)
	}
	if o.ReplaceWhitespace {
		text = strings.Map(func(r rune) rune {
			if strings.ContainsRune(textwrapWhitespace, r) {
				return ' '
			}
			return r
		}, text)
	}
	return text
}

// Non empty chunks of re.split with the capturing word separator pattern
func textwrapSplitChunks(text string, o TextwrapOptions) []string {
	re := textwrapSimpleRe
	if o.BreakOnHyphens {
		re = textwrapWordsepRe
	}
	var pieces []string
	last := 0
	m, _ := re.FindStringMatch(text)
	for m != nil {
		start := runeIndexToByte(text, m.RuneIndex)
		end := runeIndexToByte(text, m.RuneIndex+m.RuneLength)
		pieces = append(pieces, text[last:start], m.GroupByNumber(1).String())
		last = end
		m, _ = re.FindNextMatch(m)
	}
	pieces = append(pieces, text[last:])
	chunks := pieces[:0]
	for _, p := range pieces {
		if p != "" {
			chunks = append(chunks, p)
		}
	}
	return chunks
}

// Byte offset of a regexp2 rune index into the text
func runeIndexToByte(text string, runeIndex int) int {
	n := 0
	for i := range text {
		if n == runeIndex {
			return i
		}
		n++
	}
	return len(text)
}

// Lines built from the chunks as TextWrapper._wrap_chunks does
func textwrapWrapChunks(chunks []string, o TextwrapOptions) []string {
	var lines []string
	for l, r := 0, len(chunks)-1; l < r; l, r = l+1, r-1 {
		chunks[l], chunks[r] = chunks[r], chunks[l]
	}
	for len(chunks) > 0 {
		var cur []string
		curLen := 0
		indent := o.InitialIndent
		if len(lines) > 0 {
			indent = o.SubsequentIndent
		}
		width := o.Width - Len(indent)
		if o.DropWhitespace && Strip(chunks[len(chunks)-1]) == "" && len(lines) > 0 {
			chunks = chunks[:len(chunks)-1]
		}
		for len(chunks) > 0 {
			l := Len(chunks[len(chunks)-1])
			if curLen+l > width {
				break
			}
			cur = append(cur, chunks[len(chunks)-1])
			chunks = chunks[:len(chunks)-1]
			curLen += l
		}
		if len(chunks) > 0 && Len(chunks[len(chunks)-1]) > width {
			cur, chunks = textwrapHandleLongWord(chunks, cur, curLen, width, o)
			curLen = 0
			for _, c := range cur {
				curLen += Len(c)
			}
		}
		if o.DropWhitespace && len(cur) > 0 && Strip(cur[len(cur)-1]) == "" {
			curLen -= Len(cur[len(cur)-1])
			cur = cur[:len(cur)-1]
		}
		if len(cur) > 0 {
			lines = append(lines, indent+strings.Join(cur, ""))
		}
	}
	return lines
}

// Splits or carries over a chunk longer than the line as TextWrapper._handle_long_word does
func textwrapHandleLongWord(chunks, cur []string, curLen, width int, o TextwrapOptions) ([]string, []string) {
	spaceLeft := width - curLen
	if width < 1 {
		spaceLeft = 1
	}
	if o.BreakLongWords {
		chunk := []rune(chunks[len(chunks)-1])
		end := spaceLeft
		if o.BreakOnHyphens && len(chunk) > spaceLeft {
			head := string(chunk[:spaceLeft])
			hyphen := strings.LastIndex(head, "-")
			if hyphen > 0 {
				hyphen = Len(head[:hyphen])
				if strings.Trim(string(chunk[:hyphen]), "-") != "" {
					end = hyphen + 1
				}
			}
		}
		if end > len(chunk) {
			end = len(chunk)
		}
		cur = append(cur, string(chunk[:end]))
		chunks[len(chunks)-1] = string(chunk[end:])
		return cur, chunks
	}
	if len(cur) == 0 {
		cur = append(cur, chunks[len(chunks)-1])
		chunks = chunks[:len(chunks)-1]
	}
	return cur, chunks
}

// ExpandTabs is str.expandtabs with the given tab size
func ExpandTabs(s string, tabsize int) string { return expandTabs(s, tabsize) }

// str.expandtabs with the given tab size, columns reset at newlines and carriage returns
func expandTabs(s string, tabsize int) string {
	if !strings.Contains(s, "\t") {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		switch r {
		case '\t':
			if tabsize > 0 {
				n := tabsize - col%tabsize
				b.WriteString(strings.Repeat(" ", n))
				col += n
			}
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
