package models

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/internal/provider"
)

func TestModelsListsTheVerboseCatalogOnlyOnTheDefaultEndpoint(t *testing.T) {
	for base, want := range map[string]string{
		"https://api.tokenfactory.nebius.com/v1":    "https://api.tokenfactory.nebius.com/v1/models?verbose=true",
		" https://api.tokenfactory.nebius.com/v1/ ": "https://api.tokenfactory.nebius.com/v1/models?verbose=true",
		"":                       "https://api.tokenfactory.nebius.com/v1/models?verbose=true",
		"https://proxy.test/v1/": "https://proxy.test/v1/models",
		"https://api.tokenfactory.nebius.com/v1/beta": "https://api.tokenfactory.nebius.com/v1/beta/models",
	} {
		if got := target(base); got != want {
			t.Errorf("target(%q) = %q, want %q", base, got, want)
		}
	}

	var requests []string
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests, authorization = append(requests, r.URL.RequestURI()), r.Header.Get("Authorization")
		if r.URL.Path != "/v1/models" {
			http.Error(w, "missing", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"Qwen/Qwen3.5-397B-A17B-fast"},{"id":"deepseek-ai/DeepSeek-V4-Pro"}]}`)
	}))
	defer server.Close()

	ids, ok := catalog{}.Models(t.Context(), provider.Route{Provider: "nebius-token-factory", BaseURL: server.URL + "/v1/", APIKey: "key"})
	if !ok || !reflect.DeepEqual(ids, []string{"Qwen/Qwen3.5-397B-A17B-fast", "deepseek-ai/DeepSeek-V4-Pro"}) {
		t.Fatalf("Models() = %v, %v, want the ids of the custom endpoint", ids, ok)
	}
	if !reflect.DeepEqual(requests, []string{"/v1/models"}) || authorization != "Bearer key" {
		t.Fatalf("requests = %v with %q, want the plain models path of the custom endpoint with the bearer", requests, authorization)
	}
	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "other", BaseURL: server.URL + "/v1", APIKey: "key"}); ok {
		t.Fatal("Models() answered for another provider")
	}
	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "nebius-token-factory", BaseURL: server.URL + "/gone", APIKey: "key"}); ok {
		t.Fatal("Models() answered from a failing endpoint")
	}
}
