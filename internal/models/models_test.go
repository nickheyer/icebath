package models

import (
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/internal/provider"
)

type fixed map[string][]string

func (f fixed) Models(_ context.Context, route provider.Route) ([]string, bool) {
	ids, ok := f[route.Provider]
	return ids, ok
}

func TestRegistryAsksRegistrantsInOrder(t *testing.T) {
	host := New()
	nested := new(Registry)
	host.Register(nested)
	nested.Register(fixed{"a": {"a-1"}})
	host.Register(fixed{"a": {"a-2"}, "b": {"b-1"}})
	if ids, ok := host.Models(t.Context(), provider.Route{Provider: "a"}); !ok || !reflect.DeepEqual(ids, []string{"a-1"}) {
		t.Fatalf("Models(a) = %v, %v, want the first registrant that answers", ids, ok)
	}
	if ids, ok := host.Models(t.Context(), provider.Route{Provider: "b"}); !ok || !reflect.DeepEqual(ids, []string{"b-1"}) {
		t.Fatalf("Models(b) = %v, %v", ids, ok)
	}
	if _, ok := host.Models(t.Context(), provider.Route{Provider: "c"}); ok {
		t.Fatal("Models(c) answered, want no registrant to answer")
	}
}

func TestListReadsOpenAIShapedCatalogs(t *testing.T) {
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		switch r.URL.Path {
		case "/v1/models":
			w.Write([]byte(`{"data":[{"id":"one"},{"id":"two"},{"id":"one"},{"name":"no id"},"text",{"id":7}]}`))
		case "/mixed/models":
			w.Write([]byte(`{"data":[{"id":"chat-model"},{"id":"painter-1","type":"image"},{"id":"acme/flux-pro"},{"id":"vision-model","architecture":{"output_modalities":["text"]}},{"id":"clips","architecture":{"output_modalities":["video"]}}]}`))
		case "/packed/models":
			if r.Header.Get("Accept-Encoding") != "gzip" {
				http.Error(w, "expected the route header", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Encoding", "gzip")
			packed := gzip.NewWriter(w)
			packed.Write([]byte(`{"data":[{"id":"packed"}]}`))
			packed.Close()
		case "/bare/models":
			w.Write([]byte(`[{"id":"bare"}]`))
		case "/catalog":
			w.Write([]byte(`{"data":[{"id":"listed"}]}`))
		case "/empty/models":
			w.Write([]byte(`{"object":"list"}`))
		case "/text/models":
			w.Write([]byte(`"nope"`))
		case "/broken/models":
			w.Write([]byte(`{`))
		default:
			http.Error(w, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()

	ids, ok := List(t.Context(), provider.Route{BaseURL: server.URL + "/v1/", APIKey: "secret", Headers: map[string]string{"X-Title": "icebath"}}, "")
	if !ok || !reflect.DeepEqual(ids, []string{"one", "two", "one", ""}) {
		t.Fatalf("List() = %q, %v, want every row that has an id, in order, a non string id as empty", ids, ok)
	}
	if seen.Get("Authorization") != "Bearer secret" || seen.Get("X-Title") != "icebath" || seen.Get("Accept") != "application/json" || seen.Get("User-Agent") != "icebath-cli/dev" {
		t.Fatalf("List() sent headers %v, want the bearer key, the route headers, Accept and the user agent", seen)
	}
	if ids, ok := List(t.Context(), provider.Route{BaseURL: server.URL + "/mixed"}, ""); !ok || !reflect.DeepEqual(ids, []string{"chat-model", "vision-model"}) {
		t.Fatalf("List() of a catalog with generation rows = %q, %v, want only the chat rows", ids, ok)
	}
	if !IsKnownNonChatModel("painter-1") || IsKnownNonChatModel("chat-model") {
		t.Fatal("a generation row seen in a catalog was not remembered as not chat")
	}
	if ids, ok := List(t.Context(), provider.Route{BaseURL: server.URL + "/packed", Headers: map[string]string{"Accept-Encoding": "gzip"}}, ""); !ok || !reflect.DeepEqual(ids, []string{"packed"}) {
		t.Fatalf("List() of a gzip reply = %q, %v, want it unpacked", ids, ok)
	}
	if ids, ok := List(t.Context(), provider.Route{BaseURL: server.URL + "/bare"}, ""); !ok || !reflect.DeepEqual(ids, []string{"bare"}) {
		t.Fatalf("List() of a bare list = %v, %v", ids, ok)
	}
	if seen.Get("Authorization") != "" {
		t.Fatalf("List() sent Authorization %q without a key", seen.Get("Authorization"))
	}
	if ids, ok := List(t.Context(), provider.Route{BaseURL: server.URL + "/v1"}, server.URL+"/catalog"); !ok || !reflect.DeepEqual(ids, []string{"listed"}) {
		t.Fatalf("List() with a models url = %v, %v, want it to win over the base url", ids, ok)
	}
	if ids, ok := List(t.Context(), provider.Route{BaseURL: server.URL + "/empty"}, ""); !ok || len(ids) != 0 {
		t.Fatalf("List() without a data key = %v, %v, want an empty listing", ids, ok)
	}
	for _, path := range []string{"/text", "/broken", "/gone"} {
		if ids, ok := List(t.Context(), provider.Route{BaseURL: server.URL + path}, ""); ok {
			t.Errorf("List() of %s = %v, true, want a failed listing", path, ids)
		}
	}
	if _, ok := List(t.Context(), provider.Route{}, ""); ok {
		t.Fatal("List() without any address answered")
	}
}
