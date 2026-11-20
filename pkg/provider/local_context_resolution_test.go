package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const llamacppGrantedWindow = 262144

// Serves a llama-server router with the model unloaded, /v1/models carrying meta null and /props answering from the preset
func llamacppRouter(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body any
		switch {
		case strings.HasPrefix(r.URL.Path, "/props") || strings.HasPrefix(r.URL.Path, "/v1/props"):
			body = map[string]any{"default_generation_settings": map[string]any{"n_ctx": llamacppGrantedWindow}}
		case r.URL.Path == "/v1/models":
			body = map[string]any{"data": []any{map[string]any{"id": "Qwen-Test-UD-Q4_K_M", "owned_by": "llamacpp", "meta": nil, "status": map[string]any{"value": "unloaded"}}}}
		default:
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := json.Marshal(body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

func TestUnloadedLlamacppModelResolvesGrantedWindow(t *testing.T) {
	ctx, _, _ := metaTest(t)
	metadataTransport = nil
	router := llamacppRouter(t)
	settleDetection(router, "llamacpp")
	if got := queryLocalContextLengthUncached(ctx, "Qwen-Test-UD-Q4_K_M", router, ""); got != llamacppGrantedWindow {
		t.Fatalf("resolved %d; an unloaded model must resolve the preset window from /props, not fall through to name-pattern catch-alls", got)
	}
}

func TestPropsBeatsMetaWhenModelLoaded(t *testing.T) {
	ctx, _, _ := metaTest(t)
	metadataTransport = nil
	router := llamacppRouter(t)
	settleDetection(router, "llamacpp")
	if got := queryLocalContextLengthUncached(ctx, "Qwen-Test-UD-Q4_K_M", router, ""); got != llamacppGrantedWindow {
		t.Fatalf("resolved %d", got)
	}
}

func TestNonLlamacppServersSkipProps(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(*http.Request) (*http.Response, error) { return jsonResponse(nil, 404, map[string]any{}, nil), nil }
	settleDetection("http://127.0.0.1:9999/v1", "vllm")
	queryLocalContextLengthUncached(ctx, "m", "http://127.0.0.1:9999/v1", "")
	for _, u := range ft.urls() {
		if strings.Contains(u, "/props") {
			t.Fatalf("a vllm server was asked for /props: %v", ft.urls())
		}
	}
}
