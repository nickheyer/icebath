package tuiserver

import (
	"math"
	"reflect"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Score of one completion item against the query from its name and description, lower is better
func scoreSlashCompletionItem(item map[string]any, query string) float64 {
	name := strings.TrimLeft(strings.TrimSpace(itemField(item, "text")), "/")
	return math.Min(tuigateway.ScoreFields(tuigateway.TokenizeSearchText(name), query, 0), tuigateway.ScoreFields(tuigateway.TokenizeSearchText(itemField(item, "meta")), query, 3))
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
		seen[strings.TrimSpace(itemField(item, "text"))] = true
	}
	merged := slices.Clone(items)
	for _, item := range catalog {
		if !seen[strings.TrimSpace(itemField(item, "text"))] && !math.IsInf(scoreSlashCompletionItem(item, query), 1) {
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
