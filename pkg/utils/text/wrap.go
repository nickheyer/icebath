package textutil

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Settings of Wrap, the zero value filling plain paragraphs
type WrapOptions struct {
	// Prefix of the first line, counted in its width
	Indent string
	// Prefix of every line after the first, counted in its width
	NextIndent string
	// Compound words stay whole instead of breaking after their hyphens
	NoHyphenBreaks bool
	// A word longer than a line stays whole on a line of its own instead of being cut
	KeepLongWords bool
	// Line breaks, vertical tabs and form feeds stay as they are instead of each becoming a space
	KeepWhitespace bool
}

// Lines of text filled to at most width characters each, tabs expanded to stops every eight columns, words split at white space, after the hyphens of compound words and around dashes, and white space dropped where a line breaks
func Wrap(text string, width int, o WrapOptions) []string {
	text = expandTabs(text)
	if !o.KeepWhitespace {
		text = spaceOut(text)
	}
	return fillLines(splitChunks(text, !o.NoHyphenBreaks), width, o)
}

// Piece of text Wrap never splits unless it is longer than a line
type wrapChunk struct {
	text  string
	width int
	space bool
}

// Text with each tab replaced by the spaces up to the next multiple of eight columns, columns restarting after CR or LF
func expandTabs(s string) string {
	if strings.IndexByte(s, '\t') < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	col := 0
	for i := 0; i < len(s); {
		switch c := s[i]; c {
		case '\t':
			n := 8 - col%8
			b.WriteString("        "[:n])
			col += n
			i++
		case '\n', '\r':
			b.WriteByte(c)
			col = 0
			i++
		default:
			_, size := utf8.DecodeRuneInString(s[i:])
			b.WriteString(s[i : i+size])
			col++
			i += size
		}
	}
	return b.String()
}

// Text with every CR, LF, vertical tab and form feed turned into a space
func spaceOut(s string) string {
	if strings.IndexAny(s, "\n\v\f\r") < 0 {
		return s
	}
	b := []byte(s)
	for i, c := range b {
		switch c {
		case '\n', '\v', '\f', '\r':
			b[i] = ' '
		}
	}
	return string(b)
}

func isWrapSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	return false
}

// Runs of white space and the words between them, words cut at their hyphen breaks when hyphens is set
func splitChunks(text string, hyphens bool) []wrapChunk {
	chunks := make([]wrapChunk, 0, 2*strings.Count(text, " ")+1)
	for i := 0; i < len(text); {
		space := isWrapSpace(text[i])
		j := i + 1
		for j < len(text) && isWrapSpace(text[j]) == space {
			j++
		}
		if space || !hyphens {
			chunks = appendChunk(chunks, text[i:j], space)
		} else {
			chunks = appendWordParts(chunks, text[i:j])
		}
		i = j
	}
	return chunks
}

func appendChunk(chunks []wrapChunk, s string, space bool) []wrapChunk {
	if s == "" {
		return chunks
	}
	return append(chunks, wrapChunk{text: s, width: utf8.RuneCountInString(s), space: space})
}

// Pieces of a word cut after the hyphens of compound words and around runs of two or more hyphens that stand between words
func appendWordParts(chunks []wrapChunk, w string) []wrapChunk {
	start := 0
	for i := 0; i < len(w); {
		if w[i] != '-' {
			i++
			continue
		}
		j := i + 1
		for j < len(w) && w[j] == '-' {
			j++
		}
		switch {
		case j-i > 1:
			before, _ := utf8.DecodeLastRuneInString(w[:i])
			after, _ := utf8.DecodeRuneInString(w[j:])
			if i > 0 && j < len(w) && isWordPunct(before) && isWordRune(after) {
				chunks = appendChunk(chunks, w[start:i], false)
				chunks = appendChunk(chunks, w[i:j], false)
				start = j
			}
		case i > start && hyphenBreak(w, i):
			chunks = appendChunk(chunks, w[start:j], false)
			start = j
		}
		i = j
	}
	return appendChunk(chunks, w[start:], false)
}

// Whether the word may break after its lone hyphen at h: a letter before it that follows a letter or a letter and hyphen, and a letter after it that comes before a letter or a hyphen and letter
func hyphenBreak(w string, h int) bool {
	b1, n1 := utf8.DecodeLastRuneInString(w[:h])
	if !isHyphenLetter(b1) {
		return false
	}
	b2, n2 := utf8.DecodeLastRuneInString(w[:h-n1])
	if !isHyphenLetter(b2) {
		b3, _ := utf8.DecodeLastRuneInString(w[:h-n1-n2])
		if b2 != '-' || !isHyphenLetter(b3) {
			return false
		}
	}
	a1, m1 := utf8.DecodeRuneInString(w[h+1:])
	if !isHyphenLetter(a1) {
		return false
	}
	a2, m2 := utf8.DecodeRuneInString(w[h+1+m1:])
	if a2 == '-' {
		a2, _ = utf8.DecodeRuneInString(w[h+1+m1+m2:])
	}
	return isHyphenLetter(a2)
}

// Word character that is not a decimal digit
func isHyphenLetter(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || (unicode.IsNumber(r) && !unicode.IsDigit(r))
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

// Word character or one of the punctuation marks a dash between words may follow
func isWordPunct(r rune) bool { return isWordRune(r) || strings.ContainsRune(`!"'&.,?`, r) }

// Lines filled greedily from the chunks, white space dropped at line starts after the first line and at every line end
func fillLines(chunks []wrapChunk, width int, o WrapOptions) []string {
	var lines []string
	firstWidth, nextWidth := utf8.RuneCountInString(o.Indent), utf8.RuneCountInString(o.NextIndent)
	var b strings.Builder
	for len(chunks) > 0 {
		indent, room := o.Indent, width-firstWidth
		if len(lines) > 0 {
			indent, room = o.NextIndent, width-nextWidth
			if chunks[0].space {
				chunks = chunks[1:]
				continue
			}
		}
		b.Reset()
		used, trailing := 0, 0
		for len(chunks) > 0 && used+chunks[0].width <= room {
			c := chunks[0]
			b.WriteString(c.text)
			used += c.width
			trailing = 0
			if c.space {
				trailing = len(c.text)
			}
			chunks = chunks[1:]
		}
		if len(chunks) > 0 && chunks[0].width > room {
			c := &chunks[0]
			switch {
			case !o.KeepLongWords:
				left := room - used
				if room < 1 {
					left = 1
				}
				cut := len(First(c.text, left))
				if !o.NoHyphenBreaks && c.width > left {
					if h := strings.LastIndexByte(c.text[:cut], '-'); h > 0 && strings.Trim(c.text[:h], "-") != "" {
						cut = h + 1
					}
				}
				if cut > 0 {
					b.WriteString(c.text[:cut])
					trailing = 0
					if c.space {
						trailing = cut
					}
					if cut == len(c.text) {
						chunks = chunks[1:]
					} else {
						c.width -= utf8.RuneCountInString(c.text[:cut])
						c.text = c.text[cut:]
					}
				}
			case b.Len() == 0:
				b.WriteString(c.text)
				trailing = 0
				if c.space {
					trailing = len(c.text)
				}
				chunks = chunks[1:]
			}
		}
		if line := b.String()[:b.Len()-trailing]; line != "" {
			lines = append(lines, indent+line)
		}
	}
	return lines
}
