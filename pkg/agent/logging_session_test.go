package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/logging"
)

type sessionTagHandler struct {
	mu   sync.Mutex
	tags []string
}

func (h *sessionTagHandler) Handle(_ context.Context, rec *logging.Record) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tags = append(h.tags, rec.SessionTag)
	return true
}

func (h *sessionTagHandler) Level() int { return logging.LevelDebug }

func (h *sessionTagHandler) Close() error { return nil }

func TestSetLoggingSessionContextTagsTheCallingGoroutinesRecords(t *testing.T) {
	h := &sessionTagHandler{}
	lg := logging.GetLogger("agent.test_logging_session")
	lg.SetLevel(logging.LevelDebug)
	lg.SetPropagate(false)
	lg.AddHandler(h)
	t.Cleanup(func() { lg.RemoveHandler(h); lg.SetPropagate(true); logging.ClearSessionContext() })

	setLoggingSessionContext("sess-tag-1")
	lg.Info("first")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); lg.Info("other goroutine") }()
	wg.Wait()
	setLoggingSessionContext("sess-tag-2")
	lg.Info("second")

	h.mu.Lock()
	defer h.mu.Unlock()
	want := []string{" [sess-tag-1]", "", " [sess-tag-2]"}
	if len(h.tags) != len(want) {
		t.Fatalf("tags = %q, want %q", h.tags, want)
	}
	for i := range want {
		if h.tags[i] != want[i] {
			t.Fatalf("tags = %q, want %q", h.tags, want)
		}
	}
}
