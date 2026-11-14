package tools

import (
	"sync"
	"testing"
	"time"
)

// Executor host recording every activity stamp
type heartbeatHost struct {
	*fakeExecutorHost
	mu      sync.Mutex
	touches []string
}

func (h *heartbeatHost) TouchActivity(label string) {
	h.mu.Lock()
	h.touches = append(h.touches, label)
	h.mu.Unlock()
}

func (h *heartbeatHost) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.touches)
}

func TestHeartbeatExitsOnceWorkerTidIsInterrupted(t *testing.T) {
	host := &heartbeatHost{fakeExecutorHost: &fakeExecutorHost{env: newTestToolEnv(t)}}
	stop := make(chan struct{})
	worker := NewWorkerID()
	t.Cleanup(func() {
		SetInterrupt(worker, false, "")
		close(stop)
	})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		runToolActivityHeartbeat(host, stop, "tool running: terminal", 50*time.Millisecond, worker)
	}()
	time.Sleep(120 * time.Millisecond)
	if host.count() == 0 {
		t.Fatal("heartbeat never stamped while the worker was live")
	}
	SetInterrupt(worker, true, "")
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("heartbeat kept running after its worker was abandoned")
	}
	n := host.count()
	time.Sleep(120 * time.Millisecond)
	if host.count() != n {
		t.Fatalf("stamps kept arriving after exit: %d then %d", n, host.count())
	}
}
