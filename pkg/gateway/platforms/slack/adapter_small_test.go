package slack

import (
	"testing"
	"time"
)

func TestSlackReactionsMarker(t *testing.T) {
	if slackReactionsMarker(map[string]any{}) != "" || slackReactionsMarker(map[string]any{"reactions": []any{"x", map[string]any{"name": "::"}}}) != "" {
		t.Fatal("empty marker")
	}
	got := slackReactionsMarker(map[string]any{"reactions": []any{
		map[string]any{"name": "white_check_mark", "count": int64(12)}, map[string]any{"name": "eyes<b>", "count": 1},
		map[string]any{"name": "tada", "count": 2.0}, map[string]any{"name": "ok", "count": true}}})
	if got != "[reactions: :white_check_mark:\u00d712 :eyesb: :tada: :ok:]" {
		t.Fatal(got)
	}
	var many []any
	for i := 0; i < 10; i++ {
		many = append(many, map[string]any{"name": "r", "count": 3})
	}
	if got := slackReactionsMarker(map[string]any{"reactions": many}); got != "[reactions: "+":r:\u00d73 :r:\u00d73 :r:\u00d73 :r:\u00d73 :r:\u00d73 :r:\u00d73 :r:\u00d73 :r:\u00d73 +2 more]" {
		t.Fatal(got)
	}
}

func TestSlashHandoffLanesOrderAndRelease(t *testing.T) {
	l := NewSlashHandoffLanes()
	ahead1, first := l.Reserve("s")
	ahead2, second := l.Reserve("s")
	if ahead1 != nil || ahead2 != first {
		t.Fatal("slots not chained")
	}
	l.Release("s", ahead2, second)
	if second.Done() {
		t.Fatal("later slot opened before the one ahead")
	}
	l.Release("s", ahead1, first)
	select {
	case <-second.Wait():
	case <-time.After(2 * time.Second):
		t.Fatal("later slot never opened")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.tails) != 0 {
		t.Fatal("tail kept")
	}
}
