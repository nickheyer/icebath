package diffutil

import (
	"cmp"
	"slices"
)

// Up to n candidates whose Ratio against word, compared by characters, is at least cutoff, best first and equal scores in candidate order; nothing when n is not positive
func CloseMatches(word string, candidates []string, n int, cutoff float64) []string {
	if n <= 0 || len(candidates) == 0 {
		return nil
	}
	w := []rune(word)
	m := newMatcher(w)
	have := make(map[rune]int, len(w))
	for _, r := range w {
		have[r]++
	}
	used := make(map[rune]int, len(have))
	type hit struct {
		score float64
		text  string
	}
	var hits []hit
	var buf []rune
	for _, c := range candidates {
		buf = buf[:0]
		for _, r := range c {
			buf = append(buf, r)
		}
		total := len(buf) + len(w)
		if ratio(min(len(buf), len(w)), total) < cutoff {
			continue
		}
		clear(used)
		shared := 0
		for _, r := range buf {
			if used[r] < have[r] {
				used[r]++
				shared++
			}
		}
		if ratio(shared, total) < cutoff {
			continue
		}
		if score := ratio(m.common(buf), total); score >= cutoff {
			hits = append(hits, hit{score, c})
		}
	}
	slices.SortStableFunc(hits, func(x, y hit) int { return cmp.Compare(y.score, x.score) })
	out := make([]string, 0, min(n, len(hits)))
	for _, h := range hits[:min(n, len(hits))] {
		out = append(out, h.text)
	}
	return out
}
