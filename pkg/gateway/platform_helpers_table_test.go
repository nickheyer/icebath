package gateway

import (
	"strings"
	"testing"
)

func TestConvertTableToBullets(t *testing.T) {
	has := func(out string, want ...string) bool {
		for _, w := range want {
			if !strings.Contains(out, w) {
				return false
			}
		}
		return true
	}
	out := ConvertTableToBullets("| Player | Score |\n|--------|-------|\n| Alice  | 150   |\n| Bob    | 120   |")
	if !has(out, "**Alice**", "• Score: 150", "**Bob**", "• Score: 120") || strings.Contains(out, "• Player: Alice") {
		t.Fatalf("basic_table %q", out)
	}
	out = ConvertTableToBullets("| Name | Age | City |\n|:-----|----:|:----:|\n| Ada  |  30 | NYC  |")
	if !has(out, "**Ada**\n• Age: 30\n• City: NYC") || strings.Contains(out, "• Name: Ada") {
		t.Fatalf("three_column_table %q", out)
	}
	out = ConvertTableToBullets("|        | Score | Rank |\n|--------|-------|------|\n| Alice  | 150   | 1    |\n| Bob    | 120   | 2    |")
	if !has(out, "**Alice**\n• Score: 150\n• Rank: 1") {
		t.Fatalf("row_label_column %q", out)
	}
	out = ConvertTableToBullets("head1 | head2\n--- | ---\na | b\nc | d")
	if !has(out, "**a**", "• head2: b") || strings.Contains(out, "• head1: a") {
		t.Fatalf("bare_pipe_table %q", out)
	}
}
