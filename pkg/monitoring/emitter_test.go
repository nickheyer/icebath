package monitoring

import (
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func gatewayHealthDict(name string) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("event", "gateway_health")
	o.Set("name", name)
	return o
}

func TestEmitNeverRaisesWhenDisabled(t *testing.T) {
	em := NewMonitoringEmitter(false)
	em.Emit(gatewayHealthDict("gateway.health_snapshot"))
	if em.Stats().Queued != 0 {
		t.Fatalf("queued %d", em.Stats().Queued)
	}
	em.Emit(42)
	em.Close()
}

func TestProcessSingletonStaysDormantUntilSubscribed(t *testing.T) {
	ResetEmitterForTests(nil)
	defer ResetEmitterForTests(nil)
	Emit(gatewayHealthDict("gateway.lifecycle"))
	singleton := GetEmitter()
	if singleton.Stats().Queued != 0 || singleton.started.Load() {
		t.Fatalf("dormant singleton queued or started: %+v", singleton.Stats())
	}
	sub := NewSubscriber(func([]*pyjson.Object) {})
	singleton.Subscribe(sub)
	Emit(gatewayHealthDict("gateway.lifecycle"))
	if !singleton.started.Load() {
		t.Fatal("subscribed singleton did not start")
	}
	singleton.Unsubscribe(sub)
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	em := NewMonitoringEmitter(true)
	var mu sync.Mutex
	var seen []string
	cb := NewSubscriber(func(batch []*pyjson.Object) {
		mu.Lock()
		defer mu.Unlock()
		for _, ev := range batch {
			name, _ := ev.Get("name")
			seen = append(seen, name.(string))
		}
	})
	em.Subscribe(cb)
	em.Emit(gatewayHealthDict("a"))
	em.Flush(2.0)
	em.Unsubscribe(cb)
	em.Emit(gatewayHealthDict("b"))
	em.Flush(2.0)
	em.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "a" {
		t.Fatalf("seen %v", seen)
	}
}

func TestFullQueueDropsOldestAndSubscriberPanicIsIsolated(t *testing.T) {
	em := NewMonitoringEmitter(true)
	em.started.Store(true)
	for i := 0; i < maxQueue+3; i++ {
		em.Emit(gatewayHealthDict("x"))
	}
	if st := em.Stats(); st.Queued != maxQueue || st.Dropped != 3 {
		t.Fatalf("stats %+v", st)
	}
	em.started.Store(false)
	var mu sync.Mutex
	got := 0
	em.Subscribe(NewSubscriber(func([]*pyjson.Object) { panic("boom") }))
	em.Subscribe(NewSubscriber(func(b []*pyjson.Object) { mu.Lock(); got += len(b); mu.Unlock() }))
	em.Emit(gatewayHealthDict("y"))
	em.Flush(10)
	em.Close()
	mu.Lock()
	defer mu.Unlock()
	if got != maxQueue || em.Stats().Dispatched != maxQueue {
		t.Fatalf("delivered %d dispatched %d", got, em.Stats().Dispatched)
	}
}
