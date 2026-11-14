package plugins

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

type builtThing struct{ n int }

// test_lazy_singleton_builds_once_and_returns_same_instance and test_lazy_singleton_reset_rebuilds
func TestLazySingletonBuildsOnceAndResets(t *testing.T) {
	calls := 0
	get := LazySingleton(func() (*builtThing, error) { calls++; return &builtThing{calls}, nil })
	a, _ := get.Get()
	b, _ := get.Get()
	if a != b || calls != 1 {
		t.Fatalf("built %d times", calls)
	}
	get.Reset()
	c, _ := get.Get()
	if c == a || calls != 2 {
		t.Fatalf("reset did not rebuild %d", calls)
	}
}

// test_lazy_singleton_concurrent_first_call_builds_once and test_slot_concurrent_first_call_builds_once
func TestSingletonConcurrentFirstCallBuildsOnce(t *testing.T) {
	var calls atomic.Int32
	start := make(chan struct{})
	get := LazySingleton(func() (*builtThing, error) { calls.Add(1); return &builtThing{}, nil })
	slot := &SingletonSlot[*builtThing]{}
	results := make([]*builtThing, 32)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				results[i], _ = get.Get()
			} else {
				results[i], _ = slot.Get(func() (*builtThing, error) { calls.Add(1); return &builtThing{}, nil })
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if calls.Load() != 2 {
		t.Fatalf("factories ran %d times", calls.Load())
	}
	for i := 2; i < len(results); i++ {
		if results[i] != results[i%2] {
			t.Fatal("callers saw different instances")
		}
	}
}

// test_slot_caches_first_value and test_slot_factory_exception_not_cached
func TestSingletonSlotFirstValueAndFailedFactory(t *testing.T) {
	slot := &SingletonSlot[string]{}
	if _, ok := slot.Peek(); ok {
		t.Fatal("unset slot peeked a value")
	}
	if _, err := slot.Get(func() (string, error) { return "", errors.New("boom") }); err == nil || err.Error() != "boom" {
		t.Fatalf("error %v", err)
	}
	if _, ok := slot.Peek(); ok {
		t.Fatal("a failed build was cached")
	}
	first, _ := slot.Get(func() (string, error) { return "first", nil })
	second, _ := slot.Get(func() (string, error) { return "second", nil })
	if v, ok := slot.Peek(); first != "first" || second != "first" || !ok || v != "first" {
		t.Fatalf("got %q %q", first, second)
	}
	slot.Reset()
	if _, ok := slot.Peek(); ok {
		t.Fatal("reset kept the value")
	}
}
