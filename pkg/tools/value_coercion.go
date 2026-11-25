package tools

import (
	"maps"
	"regexp"
	"slices"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Whole number written in ASCII digits with an optional sign
var wholeNumberText = regexp.MustCompile(`^[+-]?\d+$`)

// JSON text on one line with a space after each comma and colon, non ASCII text kept as UTF-8
func spacedJSON(v any) string {
	out, err := jsonutil.Encode(v, jsonutil.Spaced())
	if err != nil {
		return ErrorResult(err)
	}
	return out
}

// Strings as a JSON list value
func enumValues(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Whole number a tool argument holds: a number cut toward zero, true and false as 1 and 0, or decimal text; false for anything else
func wholeNumberArg(v any) (int, bool) {
	n, err := anyutil.ToInt(v)
	return int(n), err == nil
}

// Map view of a value, empty for anything else
func asMapOrEmpty(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// Sorted keys of a set
func sortedSetKeys(set map[string]bool) []string {
	return slices.Sorted(maps.Keys(set))
}
