package gatewayrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Loop double recording every callback the watcher schedules instead of running it
type capturingLoop struct {
	mu       sync.Mutex
	captured []func()
}

func (l *capturingLoop) CallSoonThreadsafe(fn func()) error {
	l.mu.Lock()
	l.captured = append(l.captured, fn)
	l.mu.Unlock()
	return nil
}

func (l *capturingLoop) CallLater(time.Duration, func()) (gateway.LoopTimer, error) { return nil, nil }

func (l *capturingLoop) calls() []func() {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]func(){}, l.captured...)
}

// Writes a planned stop marker into the process home naming targetPid
func writePlannedStopMarker(t *testing.T, dir string, targetPid int, startTime any) string {
	t.Helper()
	marker := filepath.Join(dir, ".gateway-planned-stop.json")
	data, err := json.Marshal(map[string]any{
		"target_pid": targetPid, "target_start_time": startTime, "stopper_pid": targetPid,
		"written_at": pydatetime.ISOFormat(time.Now().UTC()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return marker
}

// Running runner double holding only the flags the watcher reads
func watcherRunner() *GatewayRunner {
	r := allocGatewayRunner()
	r.running = true
	return r
}

func TestWatcherFiresShutdownWhenMarkerAppears(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	writePlannedStopMarker(t, dir, os.Getpid(), gateway.GetProcessStartTime(os.Getpid()))
	loop := &capturingLoop{}
	var handled []*int
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPlannedStopWatcher(&pythread.Event{}, watcherRunner(), loop, func(sig *int) { handled = append(handled, sig) }, 0.05)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Watcher should exit after firing")
	}
	calls := loop.calls()
	if len(calls) != 1 {
		t.Fatalf("Expected exactly one shutdown invocation, got %d", len(calls))
	}
	calls[0]()
	if len(handled) != 1 || handled[0] != nil {
		t.Fatalf("the handler must be called with signal=None, got %v", handled)
	}
}

func TestWatcherToleratesMarkerPathResolutionErrors(t *testing.T) {
	t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), "nonexistent"))
	loop := &capturingLoop{}
	stop := &pythread.Event{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPlannedStopWatcher(stop, watcherRunner(), loop, func(*int) {}, 0.05)
	}()
	time.Sleep(200 * time.Millisecond)
	stop.Set()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Watcher should still honour stop_event after errors")
	}
	if len(loop.calls()) != 0 {
		t.Fatal("no shutdown may fire when the marker never exists")
	}
}

func TestWatcherDoesNotFireForForeignPidMarker(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	marker := writePlannedStopMarker(t, dir, os.Getpid()+1, -1)
	loop := &capturingLoop{}
	stop := &pythread.Event{}
	handled := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPlannedStopWatcher(stop, watcherRunner(), loop, func(*int) { handled++ }, 0.05)
	}()
	time.Sleep(200 * time.Millisecond)
	stop.Set()
	<-done
	if len(loop.calls()) != 0 || handled != 0 {
		t.Fatal("Watcher fired on a foreign-PID marker (#34597 regression)")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the foreign marker must be kept: %v", err)
	}
}
