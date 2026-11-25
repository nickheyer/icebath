package models

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/internal/provider"
)

func TestModelsFollowsEveryPage(t *testing.T) {
	var queries []string
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.Error(w, "missing", http.StatusNotFound)
			return
		}
		queries, headers = append(queries, r.URL.RawQuery), r.Header.Clone()
		switch r.URL.Query().Get("after_id") {
		case "":
			fmt.Fprint(w, `{"data":[{"id":"claude-a"},{"id":"claude-b"}],"has_more":true,"last_id":"claude-b"}`)
		case "claude-b":
			fmt.Fprint(w, `{"data":[{"id":"claude-b"},{"id":"claude-c"}],"has_more":true,"last_id":"claude-c"}`)
		default:
			fmt.Fprint(w, `{"data":[{"id":"claude-d"}],"has_more":true,"last_id":"claude-c"}`)
		}
	}))
	defer server.Close()

	ids, ok := catalog{}.Models(t.Context(), provider.Route{Provider: "anthropic", BaseURL: server.URL, APIKey: "key"})
	if !ok || !reflect.DeepEqual(ids, []string{"claude-a", "claude-b", "claude-c", "claude-d"}) {
		t.Fatalf("Models() = %v, %v, want the ids of every page once", ids, ok)
	}
	if want := []string{"limit=1000", "after_id=claude-b&limit=1000", "after_id=claude-c&limit=1000"}; !reflect.DeepEqual(queries, want) {
		t.Fatalf("queries = %v, want %v: a cursor seen before must end the walk", queries, want)
	}
	if headers.Get("x-api-key") != "key" || headers.Get("anthropic-version") != "2023-06-01" || headers.Get("Authorization") != "" {
		t.Fatalf("headers = %v, want the api key header", headers)
	}

	queries = nil
	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "anthropic", BaseURL: server.URL + "/v1", APIKey: "token", Auth: provider.AuthOAuth}); !ok {
		t.Fatal("Models() with a base url ending in /v1 failed")
	}
	if headers.Get("Authorization") != "Bearer token" || headers.Get("x-api-key") != "" || !strings.HasSuffix(headers.Get("anthropic-beta"), "claude-code-20250219,oauth-2025-04-20") {
		t.Fatalf("headers = %v, want a bearer token for an oauth route", headers)
	}
	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "other", BaseURL: server.URL, APIKey: "key"}); ok {
		t.Fatal("Models() answered for another provider")
	}
	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "anthropic", BaseURL: server.URL}); ok {
		t.Fatal("Models() answered without a key")
	}
	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "anthropic", BaseURL: server.URL + "/gone", APIKey: "key"}); ok {
		t.Fatal("Models() answered from a failing endpoint")
	}
}
