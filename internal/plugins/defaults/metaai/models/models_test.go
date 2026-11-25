package models

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/internal/provider"
)

func TestModelsDropsImageAndVoiceModels(t *testing.T) {
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"muse-spark-1.2"},{"id":"muse-image-2"},{"id":"muse-voice-1"},{"id":"muse-spark-1.2-contributor"}]}`)
		case "/empty/models":
			fmt.Fprint(w, `{"data":[{"id":"muse-voice-1"}]}`)
		default:
			http.Error(w, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()

	ids, ok := catalog{}.Models(t.Context(), provider.Route{Provider: "meta-ai", BaseURL: server.URL + "/v1/", APIKey: "key"})
	if want := []string{"muse-spark-1.2", "muse-spark-1.2-contributor"}; !ok || !reflect.DeepEqual(ids, want) {
		t.Fatalf("Models() = %v, %v, want the chat models %v", ids, ok, want)
	}
	if headers.Get("Authorization") != "Bearer key" {
		t.Fatalf("headers = %v, want the bearer key", headers)
	}
	if ids, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "meta-ai", BaseURL: server.URL + "/empty", APIKey: "key"}); !ok || len(ids) != 0 {
		t.Fatalf("Models() of a catalog without chat models = %v, %v, want an empty listing", ids, ok)
	}
	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "meta-ai", BaseURL: server.URL + "/gone", APIKey: "key"}); ok {
		t.Fatal("Models() answered from a failing endpoint")
	}
	if _, ok := (catalog{}).Models(t.Context(), provider.Route{Provider: "other", BaseURL: server.URL + "/v1", APIKey: "key"}); ok {
		t.Fatal("Models() answered for another provider")
	}
}
