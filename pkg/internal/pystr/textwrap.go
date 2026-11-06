package pystr

import "strings"

// Lines of textwrap.wrap with break_on_hyphens False and the other options at their defaults
func TextwrapWrapNoHyphenBreak(text string, width int, breakLongWords bool) []string {
	text = expandTabs(text, 8)
	text = strings.Map(func(r rune) rune {
		if strings.ContainsRune("\t\n\v\f\r ", r) {
			return ' '
		}
		return r
	}, text)
	var chunks [][]rune
	runes := []rune(text)
	for i := 0; i < len(runes); {
		j := i
		space := runes[i] == ' '
		for j < len(runes) && (runes[j] == ' ') == space {
			j++
		}
		chunks = append(chunks, runes[i:j])
		i = j
	}
	for l, r := 0, len(chunks)-1; l < r; l, r = l+1, r-1 {
		chunks[l], chunks[r] = chunks[r], chunks[l]
	}
	isBlank := func(c []rune) bool { return strings.TrimSpace(string(c)) == "" }
	var lines []string
	for len(chunks) > 0 {
		var cur [][]rune
		curLen := 0
		if isBlank(chunks[len(chunks)-1]) && len(lines) > 0 {
			chunks = chunks[:len(chunks)-1]
		}
		for len(chunks) > 0 {
			l := len(chunks[len(chunks)-1])
			if curLen+l > width {
				break
			}
			cur = append(cur, chunks[len(chunks)-1])
			chunks = chunks[:len(chunks)-1]
			curLen += l
		}
		if len(chunks) > 0 && len(chunks[len(chunks)-1]) > width {
			spaceLeft := width - curLen
			if width < 1 {
				spaceLeft = 1
			}
			if breakLongWords {
				chunk := chunks[len(chunks)-1]
				cur = append(cur, chunk[:spaceLeft])
				chunks[len(chunks)-1] = chunk[spaceLeft:]
			} else if len(cur) == 0 {
				cur = append(cur, chunks[len(chunks)-1])
				chunks = chunks[:len(chunks)-1]
			}
		}
		if len(cur) > 0 && isBlank(cur[len(cur)-1]) {
			cur = cur[:len(cur)-1]
		}
		if len(cur) > 0 {
			var b strings.Builder
			for _, c := range cur {
				b.WriteString(string(c))
			}
			lines = append(lines, b.String())
		}
	}
	return lines
}

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
