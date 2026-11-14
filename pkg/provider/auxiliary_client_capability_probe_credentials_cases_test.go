package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Whether the runtime main api_key is the very credential value that was bound
func auxRuntimeAPIKeyIs(ctx context.Context, credential any) bool {
	got := auxRuntimeMainValue(ctx, "api_key")
	if reflect.TypeOf(got) != reflect.TypeOf(credential) {
		return false
	}
	switch credential.(type) {
	case string, nil:
		return got == credential
	}
	return reflect.ValueOf(got).Pointer() == reflect.ValueOf(credential).Pointer()
}

func TestCapabilityPathsShareConcreteBearer(t *testing.T) {
	for _, tc := range []struct {
		credential any
		expected   string
	}{{func() string { return "minted" }, "minted"}, {"static", "static"}} {
		ctx := auxMainTestHome(t, "")
		ctx = SetRuntimeMain(ctx, "custom", "fixture", RuntimeMainOptions{APIKey: tc.credential})
		if got := modelsMaterializeProbeAPIKey(ctx, auxRuntimeMainValue(ctx, "api_key")); got != tc.expected {
			t.Fatalf("runtime credential materialized to %q", got)
		}
		if got := authHeaders(modelsMaterializeProbeAPIKey(ctx, tc.credential)); !reflect.DeepEqual(got, map[string]string{"Authorization": "Bearer " + tc.expected}) {
			t.Fatalf("metadata auth headers %v", got)
		}
		if got := lmstudioRequestHeaders(ctx, tc.credential)["Authorization"]; got != "Bearer "+tc.expected {
			t.Fatalf("lmstudio header %q", got)
		}
		var mu sync.Mutex
		var requests []*http.Request
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			requests = append(requests, r.Clone(context.Background()))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"capabilities": ["thinking"]}`))
		}))
		t.Cleanup(srv.Close)
		OllamaModelSupportsThinking(ctx, "fixture", srv.URL+"/v1", tc.credential, 5*time.Second)
		mu.Lock()
		if len(requests) == 0 || requests[0].Header.Get("Authorization") != "Bearer "+tc.expected {
			mu.Unlock()
			t.Fatalf("ollama probe requests %v", requests)
		}
		mu.Unlock()
		if !auxRuntimeAPIKeyIs(ctx, tc.credential) {
			t.Fatalf("runtime main api_key is not the bound credential: %T", auxRuntimeMainValue(ctx, "api_key"))
		}
	}
}

func TestFailedCallableNeverBecomesABearer(t *testing.T) {
	failed := func() (string, error) { return "", errors.New("secret-bearing command failure") }
	ctx := auxMainTestHome(t, "")
	for _, value := range []any{failed, func() any { return struct{}{} }, struct{}{}, nil} {
		if got := authHeaders(modelsMaterializeProbeAPIKey(ctx, value)); len(got) != 0 {
			t.Fatalf("%T became a bearer %v", value, got)
		}
		if _, ok := lmstudioRequestHeaders(ctx, value)["Authorization"]; ok {
			t.Fatalf("%T became an lmstudio bearer", value)
		}
	}
	url := "http://localhost:11434/v1"
	ctx = auxMainTestHome(t, "providers:\n  ollama:\n    base_url: "+url+"\n    api_key: provider-fallback\n    extra_headers:\n      aUtHoRiZaTiOn: Bearer configured-fallback\n      X-Probe-Fixture: preserved\n")
	for _, value := range []any{failed, func() any { return struct{}{} }, func() string { return "" }} {
		bound := SetRuntimeMain(ctx, "custom", "fixture", RuntimeMainOptions{APIKey: value})
		if got := modelsMaterializeProbeAPIKey(bound, auxRuntimeMainValue(bound, "api_key")); got != "" {
			t.Fatalf("%T runtime credential materialized to %q", value, got)
		}
		if got := getOllamaNativeHeaders(bound, url, value); !reflect.DeepEqual(got, map[string]string{"X-Probe-Fixture": "preserved"}) {
			t.Fatalf("%T native headers %v", value, got)
		}
		if !auxRuntimeAPIKeyIs(bound, value) {
			t.Fatalf("runtime main api_key is not the bound %T", value)
		}
	}
	if got := getOllamaNativeHeaders(ctx, url, nil)["aUtHoRiZaTiOn"]; got != "Bearer configured-fallback" {
		t.Fatalf("configured authentication %q", got)
	}
}
