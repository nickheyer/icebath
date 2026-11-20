package agent

import "testing"

func TestServerSideToolPredicate(t *testing.T) {
	fn := func(name string) map[string]any { return map[string]any{"name": name} }
	for _, tc := range []struct {
		apiKwargs  map[string]any
		serverSide bool
	}{
		{map[string]any{"tools": []any{map[string]any{"type": "function", "function": fn("x")}}}, false},
		{map[string]any{"tools": []any{map[string]any{"name": "x", "input_schema": map[string]any{}}}}, false},
		{map[string]any{"tools": []any{map[string]any{"type": "custom", "name": "x"}}}, false},
		{map[string]any{"tools": []any{map[string]any{"type": "web_search"}}}, true},
		{map[string]any{"tools": []any{map[string]any{"type": "web_search_20250305", "name": "web_search"}}}, true},
		{map[string]any{"extra_body": map[string]any{"plugins": []any{map[string]any{"id": "pareto-router", "min_coding_score": 0.5}}}}, false},
		{map[string]any{"extra_body": map[string]any{"plugins": []any{map[string]any{"id": "web"}}}}, true},
		{map[string]any{"web_search_options": map[string]any{}}, true},
		{map[string]any{"extra_body": map[string]any{"tools": []any{map[string]any{"type": "web_search"}}}}, true},
		{map[string]any{"extra_body": map[string]any{"tools": []any{map[string]any{"type": "function", "function": fn("x")}}}}, false},
	} {
		if got := hasServerSideTools(tc.apiKwargs); got != tc.serverSide {
			t.Errorf("hasServerSideTools(%v) = %v", tc.apiKwargs, got)
		}
	}
}
