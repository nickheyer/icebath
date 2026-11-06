package pythread

import (
	"sync"
	"testing"
)

func TestLocalIsPerGoroutine(t *testing.T) {
	var l Local
	l.Set("main")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, ok := l.Get(); ok {
			t.Error("a fresh goroutine saw the caller's value")
		}
		l.Set("child")
		if v, _ := l.Get(); v != "child" {
			t.Errorf("child read %v", v)
		}
		l.Delete()
	}()
	wg.Wait()
	if v, ok := l.Get(); !ok || v != "main" {
		t.Fatalf("main read %v %v", v, ok)
	}
	l.Delete()
	if _, ok := l.Get(); ok || len(l.vals) != 0 {
		t.Fatalf("delete left %v", l.vals)
	}
	if GoroutineID() == 0 {
		t.Fatal("goroutine id unparsed")
	}
}

func TestLocalDropsSlotsOfFinishedGoroutines(t *testing.T) {
	var l Local
	var wg sync.WaitGroup
	for i := 0; i < localPruneFloor+10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Set(i)
		}()
		wg.Wait()
	}
	l.Set("main")
	l.mu.Lock()
	n := len(l.vals)
	l.mu.Unlock()
	if n > 12 {
		t.Fatalf("finished goroutines kept %d slots", n)
	}
	if v, ok := l.Get(); !ok || v != "main" {
		t.Fatalf("live slot lost: %v %v", v, ok)
	}
}
