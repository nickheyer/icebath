package provider

import "testing"

func TestFlattenMessageTextJoinsVisibleParts(t *testing.T) {
	cases := []struct {
		content any
		sep     string
		want    string
	}{
		{nil, "\n", ""},
		{"plain", "\n", "plain"},
		{[]any{"list-shaped reasoning", "part two"}, "", "list-shaped reasoningpart two"},
		{[]any{map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "text", "text": "b"}}, "\n", "a\nb"},
		{map[string]any{"type": "text", "text": "solo"}, "\n", "solo"},
	}
	for _, c := range cases {
		if got := FlattenMessageText(c.content, c.sep); got != c.want {
			t.Fatalf("FlattenMessageText(%v) = %q, want %q", c.content, got, c.want)
		}
	}
}
