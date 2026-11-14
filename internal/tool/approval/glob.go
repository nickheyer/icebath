package approval

import (
	"strings"
)

type globItem struct {
	any      bool
	star     bool
	literal  rune
	class    bool
	negated  bool
	never    bool
	ranges   [][2]rune
	isSingle bool
}

func parseClass(pat []rune, i int) (globItem, int, bool) {
	j := i
	if j < len(pat) && pat[j] == '!' {
		j++
	}
	if j < len(pat) && pat[j] == ']' {
		j++
	}
	for j < len(pat) && pat[j] != ']' {
		j++
	}
	if j >= len(pat) {
		return globItem{}, i, false
	}
	stuff := pat[i:j]
	var chunks [][]rune
	if !containsRune(stuff, '-') {
		chunks = [][]rune{stuff}
	} else {
		k := i + 1
		if pat[i] == '!' {
			k = i + 2
		}
		start := i
		for {
			idx := -1
			for p := k; p < j; p++ {
				if pat[p] == '-' {
					idx = p
					break
				}
			}
			if idx < 0 {
				break
			}
			chunks = append(chunks, pat[start:idx])
			start = idx + 1
			k = idx + 3
		}
		chunk := pat[start:j]
		if len(chunk) > 0 {
			chunks = append(chunks, chunk)
		} else {
			last := chunks[len(chunks)-1]
			chunks[len(chunks)-1] = append(append([]rune{}, last...), '-')
		}
		for k := len(chunks) - 1; k > 0; k-- {
			prev, cur := chunks[k-1], chunks[k]
			if len(prev) > 0 && len(cur) > 0 && prev[len(prev)-1] > cur[0] {
				merged := append(append([]rune{}, prev[:len(prev)-1]...), cur[1:]...)
				chunks[k-1] = merged
				chunks = append(chunks[:k], chunks[k+1:]...)
			}
		}
	}
	item := globItem{class: true}
	var flat []rune
	var rangeAt []bool
	for c, chunk := range chunks {
		if c > 0 {
			rangeAt = append(rangeAt, true)
			flat = append(flat, '-')
		}
		for _, r := range chunk {
			flat = append(flat, r)
			rangeAt = append(rangeAt, false)
		}
	}
	if len(flat) == 0 {
		item.never = true
		return item, j + 1, true
	}
	if len(flat) == 1 && flat[0] == '!' {
		item.any, item.class = true, false
		return item, j + 1, true
	}
	p := 0
	if flat[0] == '!' {
		item.negated = true
		p = 1
	}
	for p < len(flat) {
		lo := flat[p]
		if p+2 < len(flat) && rangeAt[p+1] {
			item.ranges = append(item.ranges, [2]rune{lo, flat[p+2]})
			p += 3
			continue
		}
		item.ranges = append(item.ranges, [2]rune{lo, lo})
		p++
	}
	return item, j + 1, true
}

func containsRune(rs []rune, r rune) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
}

func compileGlob(pattern string) []globItem {
	pat := []rune(pattern)
	var items []globItem
	for i := 0; i < len(pat); {
		c := pat[i]
		i++
		switch c {
		case '*':
			if len(items) == 0 || !items[len(items)-1].star {
				items = append(items, globItem{star: true})
			}
		case '?':
			items = append(items, globItem{any: true})
		case '[':
			if item, next, ok := parseClass(pat, i); ok {
				items = append(items, item)
				i = next
			} else {
				items = append(items, globItem{literal: '[', isSingle: true})
			}
		default:
			items = append(items, globItem{literal: c, isSingle: true})
		}
	}
	return items
}

func (g globItem) matches(r rune) bool {
	switch {
	case g.any:
		return true
	case g.isSingle:
		return r == g.literal
	case g.never:
		return false
	}
	in := false
	for _, rg := range g.ranges {
		if rg[0] <= r && r <= rg[1] {
			in = true
			break
		}
	}
	return in != g.negated
}

func globMatch(name, pattern string) bool {
	items := compileGlob(pattern)
	text := []rune(name)
	ti, pi := 0, 0
	starP, starT := -1, 0
	for ti < len(text) {
		switch {
		case pi < len(items) && items[pi].star:
			starP, starT = pi, ti
			pi++
		case pi < len(items) && items[pi].matches(text[ti]):
			pi++
			ti++
		case starP >= 0:
			starT++
			ti = starT
			pi = starP + 1
		default:
			return false
		}
	}
	for pi < len(items) && items[pi].star {
		pi++
	}
	return pi == len(items)
}

func hasGlobMeta(pattern string) bool { return strings.ContainsAny(pattern, "*?[") }
