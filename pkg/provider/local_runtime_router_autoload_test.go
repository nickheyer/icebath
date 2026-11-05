package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"
)

// Router double answering every route with status and body, recording each request URI
func lr7Router(t *testing.T, status int, body string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestLocalRuntimeCapabilityReadDoesNotLoadTheModel(t *testing.T) {
	srv, seen := lr7Router(t, http.StatusOK, "{}")
	prev := capManagedRoot
	t.Cleanup(func() { capManagedRoot = prev })
	capManagedRoot = func(context.Context) (string, string, bool) { return srv.URL, "k", true }
	propsModalities(context.Background(), "qwen")
	if got := seen(); !reflect.DeepEqual(got, []string{"/props?model=qwen&autoload=false"}) {
		t.Fatalf("seen %v", got)
	}
}

func TestLocalRuntimePrefillReadDoesNotLoadTheModel(t *testing.T) {
	srv, seen := lr7Router(t, http.StatusOK, "[]")
	prev := loadEndpoint
	t.Cleanup(func() { loadEndpoint = prev })
	loadEndpoint = func() (string, string, bool) { return srv.URL, "k", true }
	if processed, ok, err := GetPrefillProgress("qwen"); ok || processed != 0 || err != nil {
		t.Fatalf("prefill %d %v %v", processed, ok, err)
	}
	if got := seen(); !reflect.DeepEqual(got, []string{"/slots?model=qwen&autoload=false"}) {
		t.Fatalf("seen %v", got)
	}
}

func TestLocalRuntimeAnUnloadedModelCountsAsIdle(t *testing.T) {
	srv, seen := lr7Router(t, http.StatusBadRequest, `{"error":{"message":"model is not loaded"}}`)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	sup := &LlamaServerSupervisor{Port: port}
	model := "qwen"
	if idle := sup.probeIdle(&model); idle == nil || !*idle {
		t.Fatalf("idle %v", idle)
	}
	if got := seen(); !reflect.DeepEqual(got, []string{"/slots?model=qwen&autoload=false"}) {
		t.Fatalf("seen %v", got)
	}
}

func TestLocalRuntimeContextProbeReadsLaunchArgsForAnUnloadedModel(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	notLoaded := map[string]any{"error": map[string]any{"message": "model is not loaded"}}
	ft.on("GET", "http://127.0.0.1:1/models", 200, map[string]any{"data": []any{map[string]any{"id": "qwen", "status": map[string]any{
		"value": "unloaded", "args": []any{"llama-server.exe", "--ctx-size", "65536", "--jinja"}}}}})
	ft.on("GET", "http://127.0.0.1:1/props?model=qwen&autoload=false", 400, notLoaded)
	ft.on("GET", "http://127.0.0.1:1/props", 400, notLoaded)
	n, ok, err := LlamacppContext(newProbeClient(ctx, "http://127.0.0.1:1", nil, 0, 0), "http://127.0.0.1:1", "qwen")
	if n != 65536 || !ok || err != nil {
		t.Fatalf("context %d %v %v", n, ok, err)
	}
	urls := ft.urls()
	if !slices.Contains(urls, "http://127.0.0.1:1/props?model=qwen&autoload=false") || slices.Contains(urls, "http://127.0.0.1:1/props?model=qwen") {
		t.Fatalf("urls %v", urls)
	}
}

func TestLocalRuntimeContextProbeSurvivesAnOddModelsBody(t *testing.T) {
	for _, body := range []any{[]any{"not", "an", "object"}, map[string]any{"data": "oops"},
		map[string]any{"data": []any{map[string]any{"id": "qwen", "status": "loading"}}}} {
		ctx, ft, _ := metaTest(t)
		ft.on("GET", "http://127.0.0.1:1/models", 200, body)
		if n, ok, err := RouterLaunchCtx(newProbeClient(ctx, "http://127.0.0.1:1", nil, 0, 0), "http://127.0.0.1:1", "qwen"); ok || n != 0 || err != nil {
			t.Fatalf("body %v: %d %v %v", body, n, ok, err)
		}
	}
}
