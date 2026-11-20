package plugins

import (
	"context"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestASlowCallbackForOneSessionDoesNotSkipAnotherSessionsCompaction(t *testing.T) {
	m := NewManager(t.TempDir())
	m.discovered = true
	c := newContext(&Manifest{Name: "compaction-observer", Source: "user"}, m)
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var seen []string
	c.RegisterHook("post_compaction", func(_ context.Context, kwargs map[string]any) (any, error) {
		session, _ := kwargs["session_id"].(string)
		mu.Lock()
		seen = append(seen, session)
		mu.Unlock()
		if session == "a" {
			close(entered)
			select {
			case <-release:
			case <-time.After(10 * time.Second):
			}
		}
		return nil, nil
	}, false)
	first := make(chan struct{})
	go func() {
		defer close(first)
		m.InvokeHook(context.Background(), "post_compaction", map[string]any{"session_id": "a"})
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first session's callback never started")
	}
	m.InvokeHook(context.Background(), "post_compaction", map[string]any{"session_id": "b"})
	close(release)
	select {
	case <-first:
	case <-time.After(10 * time.Second):
		t.Fatal("the first session's callback never finished")
	}
	mu.Lock()
	defer mu.Unlock()
	sort.Strings(seen)
	if !reflect.DeepEqual(seen, []string{"a", "b"}) {
		t.Fatalf("sessions seen %v", seen)
	}
}
