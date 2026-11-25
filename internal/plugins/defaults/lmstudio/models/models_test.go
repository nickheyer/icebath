package models

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/internal/provider"
)

func TestModelsListsChatModelsFromTheNativeAPI(t *testing.T) {
	var paths []string
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths, headers = append(paths, r.URL.Path), r.Header.Clone()
		switch r.URL.Path {
		case "/api/v1/models":
			fmt.Fprint(w, `{"models":[{"type":"llm","key":"qwen3-8b"},{"type":" Embedding ","key":"nomic-embed"},{"type":"llm","key":"","id":"from-id"},{"type":"vlm","key":"qwen3-8b"},{"key":"  gemma  "},"loose",{"type":"llm"}]}`)
		case "/openai/api/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"openai-shape"}]}`)
		default:
			http.Error(w, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()

	want := []string{"qwen3-8b", "from-id", "gemma"}
	for _, base := range []string{server.URL + "/v1", server.URL + "/api/v1/", server.URL + "/api", server.URL} {
		ids, ok := catalog{}.Models(t.Context(), provider.Route{Provider: "lmstudio", BaseURL: base, APIKey: " key "})
		if !ok || !reflect.DeepEqual(ids, want) {
			t.Fatalf("Models(%s) = %v, %v, want %v", base, ids, ok, want)
		}
	}
	if want := []string{"/api/v1/models", "/api/v1/models", "/api/v1/models", "/api/v1/models"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want the native models endpoint under the server root", paths)
	}
	if headers.Get("Authorization") != "Bearer key" || headers.Get("User-Agent") != "icebath-cli/dev" {
		t.Fatalf("headers = %v, want the trimmed bearer and the user agent", headers)
	}

	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "lmstudio", BaseURL: server.URL + "/v1"}); !ok || headers.Get("Authorization") != "" {
		t.Fatalf("Models() without a key = %v, headers %v, want a listing without a bearer", ok, headers)
	}
	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "other", BaseURL: server.URL + "/v1"}); ok {
		t.Fatal("Models() answered for another provider")
	}
	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "lmstudio", BaseURL: server.URL + "/openai/v1"}); ok {
		t.Fatal("Models() answered from a payload without a models list")
	}
	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "lmstudio", BaseURL: server.URL + "/gone/v1"}); ok {
		t.Fatal("Models() answered from a failing endpoint")
	}
	requests := len(paths)
	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "lmstudio", BaseURL: " / "}); ok || len(paths) != requests {
		t.Fatal("Models() answered or sent a request without a server root")
	}
}
