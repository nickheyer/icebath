package provider

import "testing"

func TestEstimateMessageTokensRoughWrapsUnexported(t *testing.T) {
	row := map[string]any{"role": "user", "content": "hello there, a rough estimate"}
	if got, want := EstimateMessageTokensRough(row, 1500), estimateMessageTokensCached(row, 1500); got != want || got <= 0 {
		t.Fatalf("got %d want %d", got, want)
	}
}
