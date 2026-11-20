package gatewayrun

import (
	"context"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// test_pre_gateway_dispatch.py test_internal_events_bypass_hook
func TestInternalEventsBypassHook(t *testing.T) {
	server, err := fakellm.New([]any{fakellm.Text{Text: "ok"}}, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	r := admissionRunner(t, server)
	ctx := context.Background()
	var mu sync.Mutex
	count := 0
	registerGatewayHook(ctx, "pre_gateway_dispatch", func(context.Context, map[string]any) (any, error) {
		mu.Lock()
		count++
		mu.Unlock()
		return map[string]any{"action": "skip"}, nil
	})
	event := admissionEvent("hi")
	event.Internal = true
	if _, err := r.handleMessage(ctx, event); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Fatalf("hook fired %d times for an internal event", count)
	}
	key, err := r.sessionKeyForSource(ctx, event.Source)
	if err != nil {
		t.Fatal(err)
	}
	if a := r.cachedAgentFor(key, true); a != nil {
		t.Cleanup(func() { r.cleanupAgentResources(context.Background(), a) })
	}
}

// test_pre_gateway_dispatch.py test_hook_fires_without_session_store_attribute
func TestHookFiresWithoutSessionStoreAttribute(t *testing.T) {
	server, err := fakellm.New(nil, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	r := admissionRunner(t, server)
	t.Setenv("GATEWAY_ALLOW_ALL_USERS", "")
	r.sessionStore = nil
	ctx := context.Background()
	var mu sync.Mutex
	seen := map[string]any{}
	registerGatewayHook(ctx, "pre_gateway_dispatch", func(_ context.Context, kwargs map[string]any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		store, present := kwargs["session_store"]
		if !present {
			store = "MISSING"
		}
		seen["session_store"] = store
		return map[string]any{"action": "skip", "reason": "plugin-handled"}, nil
	})
	result, err := r.handleMessage(ctx, admissionEvent("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if result != nil {
		t.Fatalf("result = %#v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if store, fired := seen["session_store"]; !fired || store != nil {
		t.Fatalf("seen = %#v", seen)
	}
	adapter, _ := r.adapters.Get("whatsapp")
	if sent := adapter.(*cleanupCaptureAdapter).sent; len(sent) != 0 {
		t.Fatalf("adapter sent %#v", sent)
	}
}
