package models

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/internal/provider"
)

func TestModelsListsTheRouterCatalog(t *testing.T) {
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"router/auto"},{"id":""},{"name":"no id"},"text",{"id":"gpt-5.4"},{"id":"router/auto"},{"id":7},{"id":true},{"id":false},{"id":0}]}`)
		case "/list/models":
			fmt.Fprint(w, `[{"id":"a"},{"id":"b"}]`)
		case "/empty/models":
			fmt.Fprint(w, `{"object":"list"}`)
		case "/text/models":
			fmt.Fprint(w, `"nope"`)
		default:
			http.Error(w, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()

	ids, ok := catalog{}.Models(t.Context(), provider.Route{Provider: "router", BaseURL: server.URL + "/v1/", APIKey: "key"})
	if want := []string{"router/auto", "gpt-5.4", "7", "True"}; !ok || !reflect.DeepEqual(ids, want) {
		t.Fatalf("Models() = %v, %v, want %v: every truthy id once in catalog order", ids, ok, want)
	}
	if headers.Get("Authorization") != "Bearer key" || headers.Get("Accept") != "application/json" || headers.Get("User-Agent") != "icebath-cli/dev" {
		t.Fatalf("headers = %v, want the bearer key, json and the catalog user agent", headers)
	}
	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "router", BaseURL: server.URL + "/v1"}); !ok || headers.Get("Authorization") != "" {
		t.Fatalf("Models() without a key = %v, Authorization %q, want an unauthenticated listing", ok, headers.Get("Authorization"))
	}
	if ids, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "router", BaseURL: server.URL + "/list", APIKey: "key"}); !ok || !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Fatalf("Models() of a bare list = %v, %v", ids, ok)
	}
	for _, path := range []string{"/empty", "/text", "/gone"} {
		if ids, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "router", BaseURL: server.URL + path, APIKey: "key"}); ok {
			t.Errorf("Models() at %s = %v, want no listing", path, ids)
		}
	}
	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "other", BaseURL: server.URL + "/v1", APIKey: "key"}); ok {
		t.Fatal("Models() answered for another provider")
	}
}
