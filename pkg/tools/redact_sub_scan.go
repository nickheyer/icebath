package tools

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Spans re.sub rewrites, a lookbehind miss retrying at the next rune and any decided match resuming at its end
func scanSubMatches(re *regexp.Regexp, text string, lineAnchored bool, boundary func(loc []int) bool, rewrite func(loc []int) (bool, int)) [][]int {
	var out [][]int
	pos := 0
	for pos <= len(text) {
		loc := re.FindStringSubmatchIndex(text[pos:])
		if loc == nil {
			break
		}
		for i := range loc {
			if loc[i] >= 0 {
				loc[i] += pos
			}
		}
		if lineAnchored && loc[0] > 0 && text[loc[0]-1] != '\n' {
			next := strings.IndexByte(text[loc[0]:], '\n')
			if next < 0 {
				break
			}
			pos = loc[0] + next + 1
			continue
		}
		if boundary != nil && !boundary(loc) {
			_, size := utf8.DecodeRuneInString(text[loc[0]:])
			pos = loc[0] + max(size, 1)
			continue
		}
		rewritten, end := rewrite(loc)
		if rewritten {
			out = append(out, loc)
		}
		pos = max(end, loc[0]+1)
	}
	return out
}
