package models

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
)

func TestModelsKeepsToolUsingLanguageModels(t *testing.T) {
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[
				{"id":"moonshotai/kimi-k2.6","type":"language","tags":["reasoning","tool-use"]},
				{"id":"openai/gpt-image-1","type":"image","tags":["tool-use"]},
				{"id":"google/gemini-3-flash","type":"language","tags":["vision"]},
				{"id":"","type":"language","tags":["tool-use"]},
				{"id":7,"type":"language","tags":["tool-use"]},
				{"id":"x/untagged","type":"language"},
				"text",
				{"id":"zai/glm-5.1","type":"language","tags":["tool-use"]}
			]}`)
		case "/embeddings/models":
			fmt.Fprint(w, `{"data":[{"id":"openai/text-embedding-3","type":"embedding","tags":["tool-use"]}]}`)
		case "/list/models":
			fmt.Fprint(w, `[{"id":"zai/glm-5.1","type":"language","tags":["tool-use"]}]`)
		default:
			http.Error(w, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()

	ids, ok := catalog{}.Models(t.Context(), provider.Route{Provider: "ai-gateway", BaseURL: server.URL + "/v1/", APIKey: " key "})
	if want := []string{"moonshotai/kimi-k2.6", "zai/glm-5.1"}; !ok || !reflect.DeepEqual(ids, want) {
		t.Fatalf("Models() = %v, %v, want %v: language models tagged tool-use only", ids, ok, want)
	}
	if headers.Get("Authorization") != "Bearer key" || headers.Get("User-Agent") != "icebath-cli/dev" {
		t.Fatalf("headers = %v, want the trimmed bearer key and the catalog user agent", headers)
	}
	for _, path := range []string{"/embeddings", "/list", "/gone"} {
		if ids, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "ai-gateway", BaseURL: server.URL + path, APIKey: "key"}); ok {
			t.Errorf("Models() at %s = %v, want no catalog", path, ids)
		}
	}
	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "ai-gateway", BaseURL: server.URL + "/v1", APIKey: " "}); ok {
		t.Fatal("Models() answered without a key")
	}
	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "other", BaseURL: server.URL + "/v1", APIKey: "key"}); ok {
		t.Fatal("Models() answered for another provider")
	}

	registry := &models.Registry{}
	Register(registry)
	ids, ok = registry.Models(t.Context(), provider.Route{Provider: "ai-gateway", BaseURL: server.URL + "/embeddings", APIKey: "key"})
	if want := []string{"openai/text-embedding-3"}; !ok || !reflect.DeepEqual(ids, want) {
		t.Fatalf("registered Models() = %v, %v, want the plain listing %v once the catalog has nothing", ids, ok, want)
	}
}
