package gateway

import (
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Viewer transport keeping every frame the owner writes
type serverRequestViewer struct {
	mu     sync.Mutex
	frames []*pyjson.Object
}

func (v *serverRequestViewer) Write(obj *pyjson.Object) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.frames = append(v.frames, obj)
	return true
}

func (v *serverRequestViewer) Close() {}

func TestAnAutomaticWindowRequestIsNotHandedToAReconnectingViewer(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	events := NewSessionEvents()
	events.Fanout.Attach(&serverRequestViewer{})
	timeout := 10.0
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		AskViewer(events, "s1", "preview.act", pyjson.ObjectOf("action", "click"), nil, &timeout, false)
	}()
	go func() {
		defer wg.Done()
		AskViewerResult(events, "s1", "setup_beat", pyjson.ObjectOf("beat", "name"), nil, &timeout, nil)
	}()
	defer func() {
		WithdrawServerRequests(events, "s1", "preview.act")
		WithdrawServerRequests(events, "s1", "setup_beat")
		wg.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !(ServerRequestWaiting(events, "s1", "preview.act") && ServerRequestWaiting(events, "s1", "setup_beat")) {
		if time.Now().After(deadline) {
			t.Fatal("requests never opened")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var methods []any
	for _, entry := range OpenServerRequests(events, "s1") {
		method, _ := entry.Get("method")
		methods = append(methods, method)
	}
	if len(methods) != 1 || methods[0] != "setup_beat" {
		t.Fatalf("open requests = %v", methods)
	}
}
