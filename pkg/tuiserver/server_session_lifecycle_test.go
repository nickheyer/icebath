package tuiserver

import (
	"context"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Level and text of every record logged while the case runs
type levelCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *levelCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *levelCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
	return nil
}
func (c *levelCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *levelCapture) WithGroup(string) slog.Handler      { return c }

// Whether a record at or above level holds every needle
func (c *levelCapture) has(level slog.Level, needles ...string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r.Level < level {
			continue
		}
		matched := true
		for _, needle := range needles {
			matched = matched && strings.Contains(r.Message, needle)
		}
		if matched {
			return true
		}
	}
	return false
}

func captureLevels(t *testing.T) *levelCapture {
	t.Helper()
	capture := &levelCapture{}
	saved := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(saved) })
	return capture
}

// Transport that holds the first resume loading frame until released, the pause inside the history read
type heldLoadingTransport struct {
	started, release chan struct{}
	once             sync.Once
}

func (h *heldLoadingTransport) Write(obj *pyjson.Object) bool {
	frame, _ := pyjson.Plain(obj).(map[string]any)
	params, _ := frame["params"].(map[string]any)
	payload, _ := params["payload"].(map[string]any)
	if params["type"] == "session.resume_progress" && payload["status"] == "loading" {
		h.once.Do(func() {
			close(h.started)
			<-h.release
		})
	}
	return true
}
func (h *heldLoadingTransport) Close() {}

// Waits until a goroutine whose stack names fn is parked on a lock, the interleave point of a race case
func waitBlockedIn(t *testing.T, fn string) {
	t.Helper()
	buf := make([]byte, 1<<20)
	var seen []string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		seen = nil
		for _, stack := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
			if !strings.Contains(stack, fn) {
				continue
			}
			header, _, _ := strings.Cut(stack, "\n")
			if strings.Contains(header, "[sync.Mutex.Lock") || strings.Contains(header, "[sync.Cond.Wait") || strings.Contains(header, "[sync.RWMutex") {
				return
			}
			seen = append(seen, stack)
		}
	}
	t.Fatalf("no goroutine parked in %s %v", fn, seen)
}

// Transport that holds the message start frame of a turn until released
type heldStartTransport struct {
	started, release chan struct{}
	once             sync.Once
}

func (h *heldStartTransport) Write(obj *pyjson.Object) bool {
	frame, _ := pyjson.Plain(obj).(map[string]any)
	if params, _ := frame["params"].(map[string]any); params["type"] == "message.start" {
		h.once.Do(func() {
			close(h.started)
			<-h.release
		})
	}
	return true
}
func (h *heldStartTransport) Close() {}
