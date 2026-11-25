package models

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/internal/provider"
)

func TestTargetListsTheCallerBaseOnlyWhenItDiffers(t *testing.T) {
	for base, want := range map[string]string{
		"":                                       "https://api.commandcode.ai/provider/v1/models",
		"https://api.commandcode.ai/provider/v1": "https://api.commandcode.ai/provider/v1/models",
		" https://api.commandcode.ai/provider/v1/ ": "https://api.commandcode.ai/provider/v1/models",
		"https://proxy.test/v1/":                    "https://proxy.test/v1/models",
	} {
		if got := target(base); got != want {
			t.Errorf("target(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestModelsListsThePublicCatalog(t *testing.T) {
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"deepseek/deepseek-v4-pro"},{"id":"claude-sonnet-4-6"},{"name":"unnamed"},"row",{"id":null},{"id":7},{"id":true},{"id":"claude-sonnet-4-6"}]}`)
		case "/list/models":
			fmt.Fprint(w, `[{"id":"claude-sonnet-4-6"}]`)
		case "/flat/models":
			fmt.Fprint(w, `{"data":"none"}`)
		default:
			http.Error(w, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()

	chat, anthropic := catalog{name: "commandcode"}, catalog{name: "commandcode-anthropic", prefix: "claude-"}
	ids, ok := chat.Models(t.Context(), provider.Route{Provider: "commandcode", BaseURL: server.URL + "/v1", APIKey: "key", Headers: map[string]string{"X-Title": "Icebath Agent"}})
	if want := []string{"deepseek/deepseek-v4-pro", "claude-sonnet-4-6", "None", "7", "True", "claude-sonnet-4-6"}; !ok || !reflect.DeepEqual(ids, want) {
		t.Fatalf("Models() = %v, %v, want %v", ids, ok, want)
	}
	if headers.Get("Authorization") != "" || headers.Get("X-Title") != "" || headers.Get("Accept") != "application/json" || headers.Get("User-Agent") != "icebath-cli/dev" {
		t.Fatalf("headers = %v, want the unauthenticated catalog headers only", headers)
	}
	ids, ok = anthropic.Models(t.Context(), provider.Route{Provider: "commandcode-anthropic", BaseURL: server.URL + "/v1/", APIKey: "key"})
	if want := []string{"claude-sonnet-4-6", "claude-sonnet-4-6"}; !ok || !reflect.DeepEqual(ids, want) {
		t.Fatalf("Models() for the Anthropic endpoint = %v, %v, want %v", ids, ok, want)
	}
	if ids, ok := chat.Models(t.Context(), provider.Route{Provider: "commandcode", BaseURL: server.URL + "/flat"}); !ok || !reflect.DeepEqual(ids, []string{}) {
		t.Fatalf("Models() of a mapping without a data list = %v, %v, want an empty listing", ids, ok)
	}
	if _, ok := chat.Models(t.Context(), provider.Route{Provider: "commandcode", BaseURL: server.URL + "/list"}); ok {
		t.Fatal("Models() answered from a reply that is not a mapping")
	}
	if _, ok := chat.Models(t.Context(), provider.Route{Provider: "commandcode", BaseURL: server.URL + "/gone"}); ok {
		t.Fatal("Models() answered from a failing endpoint")
	}
	if _, ok := anthropic.Models(t.Context(), provider.Route{Provider: "commandcode", BaseURL: server.URL + "/v1"}); ok {
		t.Fatal("Models() answered for another provider")
	}
}
