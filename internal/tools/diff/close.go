package diff

import (
	"cmp"
	"slices"
)

// Upper bound of the ratio from the characters the texts share in any order
func (m *Matcher[T]) quickRatio() float64 {
	full := map[T]int{}
	for _, elt := range m.b {
		full[elt]++
	}
	avail := map[T]int{}
	matched := 0
	for _, elt := range m.a {
		n, ok := avail[elt]
		if !ok {
			n = full[elt]
		}
		avail[elt] = n - 1
		if n > 0 {
			matched++
		}
	}
	return ratio(matched, len(m.a)+len(m.b))
}

// Upper bound of the ratio from the lengths alone
func (m *Matcher[T]) realQuickRatio() float64 {
	return ratio(min(len(m.a), len(m.b)), len(m.a)+len(m.b))
}

func ratio(matched, total int) float64 {
	if total == 0 {
		return 1
	}
	return 2 * float64(matched) / float64(total)
}

// The n candidates most like the word whose ratio reaches the cutoff, best first and the greater text first between equals, as Python's
// get_close_matches gives them
func Close(word string, candidates []string, n int, cutoff float64) []string {
	type scored struct {
		text  string
		score float64
	}
	var found []scored
	for _, candidate := range candidates {
		m := Runes(candidate, word)
		if m.realQuickRatio() < cutoff || m.quickRatio() < cutoff {
			continue
		}
		if score := m.Ratio(); score >= cutoff {
			found = append(found, scored{candidate, score})
		}
	}
	slices.SortStableFunc(found, func(x, y scored) int { return cmp.Or(cmp.Compare(y.score, x.score), cmp.Compare(y.text, x.text)) })
	out := make([]string, 0, min(n, len(found)))
	for _, s := range found[:min(n, len(found))] {
		out = append(out, s.text)
	}
	return out
}
