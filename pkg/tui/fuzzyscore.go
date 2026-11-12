package tui

import (
	"math"
	"sort"

	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Fields of one slash menu entry the ranking scores
type slashScoreItem struct {
	Aliases     []string
	Description string
	ID          string
	Label       string
}

// Tier of an entry against a normalized query, names and aliases before descriptions, infinity on a miss
func scoreSlashMenuItem(item slashScoreItem, query string) float64 {
	var command []string
	for _, f := range append([]string{item.ID, item.Label}, item.Aliases...) {
		if f != "" {
			command = append(command, tuiserver.TokenizeSearchText(f)...)
		}
	}
	description := tuiserver.TokenizeSearchText(item.Description)
	return math.Min(tuiserver.ScoreFields(command, query, 0), tuiserver.ScoreFields(description, query, 3))
}

// Entries matching the query ranked by tier, ties in their original order, all of them for an empty query
func rankSlashItems[T any](items []T, query string, toScoreItem func(T) slashScoreItem) []T {
	normalized := tuiserver.NormalizeSlashSearchQuery(query)
	if normalized == "" {
		return items
	}
	type scored struct {
		item  T
		score float64
	}
	var kept []scored
	for _, it := range items {
		if s := scoreSlashMenuItem(toScoreItem(it), normalized); !math.IsInf(s, 1) {
			kept = append(kept, scored{it, s})
		}
	}
	sort.SliceStable(kept, func(a, b int) bool { return kept[a].score < kept[b].score })
	out := make([]T, len(kept))
	for i, k := range kept {
		out[i] = k.item
	}
	return out
}
