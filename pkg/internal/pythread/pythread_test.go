package pythread

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestEventSetWakesWaitersAndClearRearms(t *testing.T) {
	var e Event
	if e.IsSet() || e.Wait(0.01) {
		t.Fatal("fresh event must be clear")
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !e.Wait(-1) {
				t.Error("wait returned false after set")
			}
		}()
	}
	time.Sleep(10 * time.Millisecond)
	e.Set()
	e.Set()
	wg.Wait()
	e.Clear()
	if e.IsSet() || e.Wait(0.01) {
		t.Fatal("cleared event must block again")
	}
}

func TestBoundedSemaphoreRefusesOverRelease(t *testing.T) {
	s := NewBoundedSemaphore(2)
	if !s.Acquire(false) || !s.Acquire(false) || s.Acquire(false) {
		t.Fatal("two slots then none")
	}
	if s.Release() != nil || s.Release() != nil {
		t.Fatal("releasing held slots must succeed")
	}
	if err := s.Release(); !errors.Is(err, ErrSemaphoreReleasedTooManyTimes) {
		t.Fatalf("over release = %v", err)
	}
}

func TestMonotonicAdvances(t *testing.T) {
	a := Monotonic()
	time.Sleep(time.Millisecond)
	if b := Monotonic(); b <= a {
		t.Fatalf("monotonic went %v -> %v", a, b)
	}
}
