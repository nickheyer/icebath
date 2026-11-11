package tuiserver

import (
	"math"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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

// Lowercased value followed by its alphanumeric words
func tokenizeSearchText(value string) []string {
	normalized := strings.ToLower(value)
	out := []string{normalized}
	for _, t := range slashTokenSplit.Split(normalized, -1) {
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Query trimmed, stripped of leading slashes and lowercased
func normalizeSlashSearchQuery(query string) string {
	return strings.ToLower(strings.TrimLeft(pystr.Strip(query), "/"))
}

// Best tier of any field plus offset, infinity on a miss
func scoreFields(fields []string, query string, offset float64) float64 {
	for _, tier := range slashTiers {
		if slices.ContainsFunc(fields, func(f string) bool { return tier.pred(f, query) }) {
			return offset + tier.bump
		}
	}
	return math.Inf(1)
}

// Score of one completion item against the query from its name and description, lower is better
func scoreSlashCompletionItem(item map[string]any, query string) float64 {
	name := strings.TrimLeft(pystr.Strip(itemField(item, "text")), "/")
	return math.Min(scoreFields(tokenizeSearchText(name), query, 0), scoreFields(tokenizeSearchText(itemField(item, "meta")), query, 3))
}

// str of an item field, empty when absent
func itemField(item map[string]any, key string) string {
	v, present := item[key]
	if !present {
		return ""
	}
	return pyrepr.Str(v)
}

// Identity of an item dict, the way id() keys the Python score table
func itemID(item map[string]any) uintptr { return reflect.ValueOf(item).Pointer() }

// Catalog entries the prefix filter missed merged into items and sorted by score, with the score lookup
func fuzzyRankSlashItems(items, catalog []map[string]any, query string) ([]map[string]any, func(map[string]any) float64) {
	seen := map[string]bool{}
	for _, item := range items {
		seen[pystr.Strip(itemField(item, "text"))] = true
	}
	merged := slices.Clone(items)
	for _, item := range catalog {
		if !seen[pystr.Strip(itemField(item, "text"))] && !math.IsInf(scoreSlashCompletionItem(item, query), 1) {
			merged = append(merged, item)
		}
	}
	scores := map[uintptr]float64{}
	for _, item := range merged {
		scores[itemID(item)] = scoreSlashCompletionItem(item, query)
	}
	ranked := slices.DeleteFunc(slices.Clone(merged), func(item map[string]any) bool { return math.IsInf(scores[itemID(item)], 1) })
	slices.SortStableFunc(ranked, func(a, b map[string]any) int {
		sa, sb := scores[itemID(a)], scores[itemID(b)]
		switch {
		case sa < sb:
			return -1
		case sa > sb:
			return 1
		}
		return 0
	})
	return ranked, func(item map[string]any) float64 {
		if s, ok := scores[itemID(item)]; ok {
			return s
		}
		return math.Inf(1)
	}
}
