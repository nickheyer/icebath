package pythread

import (
	"sync"
	"testing"
	"time"
)

func TestRLockReentersOnTheOwnerAndExcludesOthers(t *testing.T) {
	var l RLock
	l.Lock()
	l.Lock()
	acquired := make(chan struct{})
	go func() {
		l.Lock()
		close(acquired)
		l.Unlock()
	}()
	l.Unlock()
	select {
	case <-acquired:
		t.Fatal("another goroutine took a lock still held once")
	case <-time.After(50 * time.Millisecond):
	}
	l.Unlock()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock was not released after the last unlock")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("unlocking an unheld lock must panic")
		}
	}()
	l.Unlock()
}

func TestRLockSerializesConcurrentHolders(t *testing.T) {
	var l RLock
	var wg sync.WaitGroup
	counter := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				l.Lock()
				l.Lock()
				counter++
				l.Unlock()
				l.Unlock()
			}
		}()
	}
	wg.Wait()
	if counter != 800 {
		t.Fatalf("counter %d", counter)
	}
}

func TestRLockTryLockForTimesOutWhileHeldElsewhere(t *testing.T) {
	var l RLock
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		l.Lock()
		close(held)
		<-release
		l.Unlock()
	}()
	<-held
	if l.TryLockFor(20 * time.Millisecond) {
		t.Fatal("acquired a lock another goroutine holds")
	}
	close(release)
	if !l.TryLockFor(2 * time.Second) {
		t.Fatal("lock not acquired after release")
	}
	if !l.TryLockFor(0) {
		t.Fatal("owner could not reenter")
	}
	l.Unlock()
	l.Unlock()
}
