package models

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/internal/provider"
)

type redirect struct {
	target *url.URL
}

func (r redirect) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.URL.Scheme, request.URL.Host = r.target.Scheme, r.target.Host
	return http.DefaultTransport.RoundTrip(request)
}

func TestEndpointConfirmsOnlyTheKimiCodeSurface(t *testing.T) {
	cases := map[string]struct {
		target    string
		confirmed bool
	}{
		"https://api.moonshot.ai/v1":       {"https://api.moonshot.ai/v1/models", false},
		"https://api.moonshot.cn/v1/":      {"https://api.moonshot.cn/v1/models", false},
		"https://api.kimi.com/coding":      {"https://api.kimi.com/coding/v1/models", true},
		"https://api.kimi.com/coding/":     {"https://api.kimi.com/coding/v1/models", true},
		"https://api.kimi.com/coding/v1/":  {"https://api.kimi.com/coding/v1/models", true},
		"https://API.Kimi.com:443/coding":  {"https://API.Kimi.com:443/coding/v1/models", true},
		"https://api.kimi.com:8443/coding": {"https://api.kimi.com:8443/coding/models", false},
		"http://api.kimi.com/coding":       {"http://api.kimi.com/coding/models", false},
		"https://key@api.kimi.com/coding":  {"https://key@api.kimi.com/coding/models", false},
		"https://api.kimi.com/v1":          {"https://api.kimi.com/v1/models", false},
	}
	for base, want := range cases {
		if target, confirmed := endpoint(base); target != want.target || confirmed != want.confirmed {
			t.Errorf("endpoint(%q) = %q, %v, want %q, %v", base, target, confirmed, want.target, want.confirmed)
		}
	}
}

func TestModelsDropsTheBareK3SlugOffTheKimiCodeSurface(t *testing.T) {
	var requests []string
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests, headers = append(requests, r.Host+r.URL.Path), r.Header.Clone()
		if r.URL.Path != "/v1/models" && r.URL.Path != "/coding/v1/models" {
			http.Error(w, "missing", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"kimi-k2.6"},{"id":"K3"},{"id":" k3 "},{"id":"kimi-k3"}]}`)
	}))
	defer server.Close()
	upstream, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	global := catalog{name: "kimi-coding", base: "https://api.moonshot.ai/v1"}
	ids, ok := global.Models(t.Context(), provider.Route{Provider: "kimi-coding", BaseURL: server.URL + "/v1/", APIKey: "key", Headers: map[string]string{"X-Title": "Icebath Agent"}})
	if !ok || !reflect.DeepEqual(ids, []string{"kimi-k2.6", "kimi-k3"}) {
		t.Fatalf("Models() = %v, %v, want the bare k3 slug dropped off the Kimi Code surface", ids, ok)
	}
	if headers.Get("Authorization") != "Bearer key" || headers.Get("X-Title") != "Icebath Agent" {
		t.Fatalf("headers = %v, want the bearer key and the profile headers", headers)
	}

	cn := catalog{name: "kimi-coding-cn", base: server.URL + "/v1"}
	if ids, ok := cn.Models(t.Context(), provider.Route{Provider: "kimi-coding-cn"}); !ok || !reflect.DeepEqual(ids, []string{"kimi-k2.6", "kimi-k3"}) {
		t.Fatalf("Models() without a base url = %v, %v, want the profile endpoint listed", ids, ok)
	}
	if _, ok := global.Models(t.Context(), provider.Route{Provider: "kimi-coding-cn", BaseURL: server.URL + "/v1"}); ok {
		t.Fatal("Models() answered for another provider")
	}
	if _, ok := global.Models(t.Context(), provider.Route{Provider: "kimi-coding", BaseURL: server.URL + "/gone"}); ok {
		t.Fatal("Models() answered from a failing endpoint")
	}

	previous := http.DefaultClient.Transport
	http.DefaultClient.Transport = redirect{target: upstream}
	t.Cleanup(func() { http.DefaultClient.Transport = previous })
	requests = nil
	ids, ok = global.Models(t.Context(), provider.Route{Provider: "kimi-coding", BaseURL: "https://api.kimi.com/coding/", APIKey: "sk-kimi-key"})
	if !ok || !reflect.DeepEqual(ids, []string{"kimi-k2.6", "K3", " k3 ", "kimi-k3"}) {
		t.Fatalf("Models() on the Kimi Code surface = %v, %v, want every id kept", ids, ok)
	}
	if want := []string{"api.kimi.com/coding/v1/models"}; !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests = %v, want %v: the bare /coding surface lists from /coding/v1", requests, want)
	}
}
