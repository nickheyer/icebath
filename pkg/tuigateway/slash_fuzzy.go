package tuigateway

import (
	"math"
	"regexp"
	"slices"
	"strings"
)

var slashTokenSplit = regexp.MustCompile(`[^a-z0-9]+`)

// Exact, prefix and substring field predicates with their tier bump, in priority order
var slashTiers = []struct {
	bump float64
	pred func(field, q string) bool
}{
	{0, func(field, q string) bool { return field == q || "/"+field == q }},
	{1, func(field, q string) bool { return strings.HasPrefix(field, q) || strings.HasPrefix("/"+field, q) }},
	{2, func(field, q string) bool { return strings.Contains(field, q) }},
}

// Lowercased value followed by its alphanumeric words, the search tokens of a slash menu field
func TokenizeSearchText(value string) []string {
	normalized := strings.ToLower(value)
	out := []string{normalized}
	for _, t := range slashTokenSplit.Split(normalized, -1) {
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Slash query trimmed, stripped of leading slashes and lowercased
func NormalizeSlashSearchQuery(query string) string {
	return strings.ToLower(strings.TrimLeft(strings.TrimSpace(query), "/"))
}

// Best exact, prefix or substring tier of any field plus offset, infinity on a miss
func ScoreFields(fields []string, query string, offset float64) float64 {
	for _, tier := range slashTiers {
		if slices.ContainsFunc(fields, func(f string) bool { return tier.pred(f, query) }) {
			return offset + tier.bump
		}
	}
	return math.Inf(1)
}
