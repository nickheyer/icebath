package provider

import (
	"container/list"
	"testing"
)

func TestEstimateMessageTokensRoughWrapsUnexported(t *testing.T) {
	row := map[string]any{"role": "user", "content": "hello there, a rough estimate"}
	if got, want := EstimateMessageTokensRough(row, 1500), estimateMessageTokensCached(row, 1500); got != want || got <= 0 {
		t.Fatalf("got %d want %d", got, want)
	}
}

// RetainedBytes is the current retained bytes estimate of the message token memo
func RetainedBytes() int {
	c := msgTokensCache
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.retained
}

// Resets the message token memo
func clearMessageTokenMemo() {
	c := msgTokensCache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order.Init()
	c.index = map[string]*list.Element{}
	c.retained = 0
}
