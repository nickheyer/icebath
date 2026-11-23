//go:build !icebath_core && !icebath_noextras

package tuiserver

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/home"
)

// Polls pred until it holds or the timeout passes
func waitFor(pred func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return pred()
}

// One global event a fake broadcast recorded
type recordedEvent struct {
	name    string
	payload map[string]any
}

// Recorder of every global event the watcher broadcasts
type eventLog struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (l *eventLog) all() []recordedEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]recordedEvent(nil), l.events...)
}

// Payloads of every display.status event
func (l *eventLog) statuses() []map[string]any {
	var out []map[string]any
	for _, ev := range l.all() {
		if ev.name == "display.status" {
			out = append(out, ev.payload)
		}
	}
	return out
}

// Watches home as the launch home with a recording broadcast, waiting until the watcher seeded it
func watching(t *testing.T, h string) *eventLog {
	t.Helper()
	log := &eventLog{}
	prevBroadcast := broadcastGlobalEvent
	servedProfileMu.Lock()
	prevHome := icebathHome
	icebathHome = h
	broadcastGlobalEvent = func(event string, payload map[string]any) {
		log.mu.Lock()
		log.events = append(log.events, recordedEvent{event, payload})
		log.mu.Unlock()
	}
	servedProfileMu.Unlock()
	t.Cleanup(func() {
		servedProfileMu.Lock()
		icebathHome, broadcastGlobalEvent = prevHome, prevBroadcast
		servedProfileMu.Unlock()
	})
	ensureLeaseWatcher()
	if !waitFor(func() bool {
		leaseWatchMu.Lock()
		defer leaseWatchMu.Unlock()
		_, seeded := leaseEpochs[home.Key(h)]
		return seeded
	}, 3*time.Second) {
		t.Fatal("watcher never seeded the home")
	}
	return log
}

func TestScreenStartedOrStoppedByAnotherProcessIsBroadcastAsStatus(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(filepath.Join(h, "bot-desktop"), 0o700); err != nil {
		t.Fatal(err)
	}
	events := watching(t, h)
	if err := pollRuntimeFiles(); err != nil {
		t.Fatal(err)
	}
	if len(events.statuses()) != 0 {
		t.Fatalf("seed broadcast %v", events.statuses())
	}

	writeFile(t, filepath.Join(h, "bot-desktop", "launcher.pid"), "424242 1.5")
	writeFile(t, filepath.Join(h, "bot-desktop", "env"), "DISPLAY=:77\n")
	if err := pollRuntimeFiles(); err != nil {
		t.Fatal(err)
	}
	statuses := events.statuses()
	if len(statuses) == 0 || statuses[len(statuses)-1]["profile_key"] != h {
		t.Fatalf("statuses %v", statuses)
	}

	if err := os.Remove(filepath.Join(h, "bot-desktop", "env")); err != nil {
		t.Fatal(err)
	}
	if err := pollRuntimeFiles(); err != nil {
		t.Fatal(err)
	}
	statuses = events.statuses()
	if len(statuses) != 2 || statuses[1]["running"] != false {
		t.Fatalf("statuses %v", statuses)
	}
}
