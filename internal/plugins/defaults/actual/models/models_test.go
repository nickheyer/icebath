package models

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/internal/provider"
)

func TestModelsListsTheNormalizedActualSurface(t *testing.T) {
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"actual-large"},{"id":"actual-small"}]}`)
		case "/relay/models":
			fmt.Fprint(w, `{"data":[{"id":"relay-model"}]}`)
		default:
			http.Error(w, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()

	ids, ok := catalog{}.Models(t.Context(), provider.Route{Provider: "actual", BaseURL: server.URL, APIKey: "key"})
	if want := []string{"actual-large", "actual-small"}; !ok || !reflect.DeepEqual(ids, want) {
		t.Fatalf("Models() of a loopback root = %v, %v, want %v from its /v1 surface", ids, ok, want)
	}
	if headers.Get("Authorization") != "Bearer key" {
		t.Fatalf("headers = %v, want the bearer key", headers)
	}
	if ids, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "actual", BaseURL: server.URL + "/relay/", APIKey: "key"}); !ok || !reflect.DeepEqual(ids, []string{"relay-model"}) {
		t.Fatalf("Models() of a relay path = %v, %v, want the relay listing as given", ids, ok)
	}
	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "actual", BaseURL: server.URL + "/gone", APIKey: "key"}); ok {
		t.Fatal("Models() answered from a failing endpoint")
	}
	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "other", BaseURL: server.URL, APIKey: "key"}); ok {
		t.Fatal("Models() answered for another provider")
	}
}
