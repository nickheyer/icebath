package jsonschemautil

import "cmp"

// Most relevant of a list of errors, nil for an empty list: among the errors, the one highest in the value, not from anyOf or oneOf, whose value has a type its schema rules out, the first of equals; then into its causes for as long as one of them is more relevant than the rest, there preferring the deepest, from anyOf or oneOf, whose value has the type its schema expects
func BestMatch(errs []*ValidationError) *ValidationError {
	if len(errs) == 0 {
		return nil
	}
	best, bestKey := errs[0], relevanceOf(errs[0])
	for _, e := range errs[1:] {
		if k := relevanceOf(e); k.compare(bestKey) > 0 {
			best, bestKey = e, k
		}
	}
	for len(best.Causes) > 0 {
		first, second := -1, -1
		var firstKey, secondKey relevance
		for i, c := range best.Causes {
			k := relevanceOf(c)
			switch {
			case first < 0 || k.compare(firstKey) < 0:
				second, secondKey = first, firstKey
				first, firstKey = i, k
			case second < 0 || k.compare(secondKey) < 0:
				second, secondKey = i, k
			}
		}
		if second >= 0 && firstKey.compare(secondKey) == 0 {
			return best
		}
		best = best.Causes[first]
	}
	return best
}

// How much an error says about what is wrong, compared field by field
type relevance struct {
	depth int
	weak  bool
	typed bool
}

func relevanceOf(e *ValidationError) relevance {
	return relevance{
		depth: len(e.InstancePath),
		weak:  e.Keyword == "anyOf" || e.Keyword == "oneOf",
		typed: e.node != nil && e.node.hasTypes && e.node.types.match(e.value),
	}
}

// Greater for the error higher in the value, then for one not from anyOf or oneOf, then for one whose value has a type its schema rules out
func (r relevance) compare(o relevance) int {
	if c := cmp.Compare(o.depth, r.depth); c != 0 {
		return c
	}
	if c := compareBool(!r.weak, !o.weak); c != 0 {
		return c
	}
	return compareBool(!r.typed, !o.typed)
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}
