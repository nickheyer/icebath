package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Server answering /models with the given ids and 404 for every other path
func modelsServer(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimRight(r.URL.Path, "/") != "/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		data := []map[string]string{}
		for _, id := range ids {
			data = append(data, map[string]string{"id": id})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchModelsBaseURLOverrideUsed(t *testing.T) {
	srv := modelsServer(t, "proxy-model-a")
	p := Profile{Name: "test", BaseURL: "http://127.0.0.1:1", ModelListing: true}
	if ids, ok := p.FetchModels(context.Background(), "test-key", srv.URL, 0); !ok || !reflect.DeepEqual(ids, []string{"proxy-model-a"}) {
		t.Fatalf("caller base url = %v %v", ids, ok)
	}
}

func TestFetchModelsCustomBaseURLBeatsModelsURL(t *testing.T) {
	srv := modelsServer(t, "proxy-model-b")
	p := Profile{Name: "test", BaseURL: "http://127.0.0.1:1", ModelsURL: "http://127.0.0.1:1/models", ModelListing: true}
	if ids, ok := p.FetchModels(context.Background(), "test-key", srv.URL, 0); !ok || !reflect.DeepEqual(ids, []string{"proxy-model-b"}) {
		t.Fatalf("custom base url = %v %v", ids, ok)
	}
}

func TestFetchModelsDefaultBaseURLDoesNotShadowModelsURL(t *testing.T) {
	srv := modelsServer(t, "catalog-model")
	p := Profile{Name: "test", BaseURL: "http://127.0.0.1:1", ModelsURL: srv.URL + "/models", ModelListing: true}
	if ids, ok := p.FetchModels(context.Background(), "test-key", "http://127.0.0.1:1/", 0); !ok || !reflect.DeepEqual(ids, []string{"catalog-model"}) {
		t.Fatalf("echoed default base url = %v %v", ids, ok)
	}
}

func TestCustomProfilePassesBaseURL(t *testing.T) {
	srv := modelsServer(t, "ollama-model")
	ctx := contextWithTempHome(t)
	custom := getProviderProfile(ctx, "custom")
	if custom == nil {
		t.Fatal("custom profile missing")
	}
	p := *custom
	p.BaseURL = "http://127.0.0.1:1"
	if ids, ok := p.FetchModels(ctx, "", srv.URL, 0); !ok || !reflect.DeepEqual(ids, []string{"ollama-model"}) {
		t.Fatalf("custom fetch = %v %v", ids, ok)
	}
	p.BaseURL = ""
	if ids, ok := p.FetchModels(ctx, "", "", 0); ok || ids != nil {
		t.Fatalf("custom without a base url fetched %v", ids)
	}
}

// Fetches through a /models redirect to the Location built from the two server URLs, returning the redirected request headers
func fetchThroughRedirect(t *testing.T, location func(first, second string) string) ([]string, bool, http.Header) {
	t.Helper()
	var mu sync.Mutex
	received := http.Header{}
	var target string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimRight(r.URL.Path, "/") == "/models" {
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		mu.Lock()
		received = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"redirected-model"}]}`))
	})
	first, second := httptest.NewServer(handler), httptest.NewServer(handler)
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)
	target = location(first.URL, second.URL)
	p := Profile{Name: "test", BaseURL: first.URL, ModelListing: true, Headers: map[string]string{"x-api-key": "default-header-secret"}}
	ids, ok := p.FetchModels(context.Background(), "bearer-secret", "", 0)
	mu.Lock()
	defer mu.Unlock()
	return ids, ok, received
}

func TestFetchModelsCrossHostRedirectStripsCredentials(t *testing.T) {
	ids, ok, headers := fetchThroughRedirect(t, func(first, _ string) string {
		return strings.Replace(first, "127.0.0.1", "localhost", 1) + "/redirected"
	})
	if !ok || !reflect.DeepEqual(ids, []string{"redirected-model"}) {
		t.Fatalf("redirected fetch = %v %v", ids, ok)
	}
	if headers.Get("Authorization") != "" || headers.Get("X-Api-Key") != "" {
		t.Fatalf("credentials followed a cross host redirect: %v", headers)
	}
}

func TestFetchModelsSameOriginRedirectKeepsCredentials(t *testing.T) {
	ids, ok, headers := fetchThroughRedirect(t, func(first, _ string) string { return first + "/redirected" })
	if !ok || !reflect.DeepEqual(ids, []string{"redirected-model"}) {
		t.Fatalf("redirected fetch = %v %v", ids, ok)
	}
	if headers.Get("Authorization") != "Bearer bearer-secret" || headers.Get("X-Api-Key") != "default-header-secret" {
		t.Fatalf("same origin redirect dropped credentials: %v", headers)
	}
}

func TestModelPickerPassesTheCredentialBaseURLToFetchModels(t *testing.T) {
	newTestHome(t)
	ctx := context.Background()
	var calls []string
	registerTestProfile(t, Profile{Name: "test-provider", AuthType: AuthTypeAPIKey, KeyEnv: []string{"TEST_PROVIDER_KEY"},
		BaseURLEnv: "TEST_PROVIDER_BASE_URL", BaseURL: "https://default.api.com", ModelListing: true,
		FetchModelsHook: func(_ context.Context, apiKey, baseURL string, _ time.Duration) ([]string, bool) {
			calls = append(calls, apiKey+" "+baseURL)
			return []string{"model-a"}, true
		}})
	t.Setenv("TEST_PROVIDER_KEY", "sk-test")
	t.Setenv("TEST_PROVIDER_BASE_URL", "https://custom.proxy.com")
	ProviderModelIDs(ctx, "test-provider", false)
	if len(calls) != 1 || calls[0] != "sk-test https://custom.proxy.com" {
		t.Fatalf("fetch_models calls = %v", calls)
	}
}

type countingTransport struct {
	mu    sync.Mutex
	calls int
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return nil, fmt.Errorf("network reached for %s", r.URL)
}

func TestProfilesWithoutModelListingNeverHitTheNetwork(t *testing.T) {
	ctx := contextWithTempHome(t)
	opener := &countingTransport{}
	prev := modelsTransport
	modelsTransport = opener
	t.Cleanup(func() { modelsTransport = prev })
	flagged := map[string]bool{}
	for _, p := range ListProviders(ctx) {
		if p.ModelListing {
			continue
		}
		flagged[p.Name] = true
		if ids, ok := p.FetchModels(ctx, "k", p.BaseURL, 0); ok || ids != nil {
			t.Fatalf("%s listed %v", p.Name, ids)
		}
	}
	if !flagged["bedrock"] || !flagged["vertex"] {
		t.Fatalf("flagged profiles = %v", flagged)
	}
	if opener.calls != 0 {
		t.Fatalf("network reached %d times", opener.calls)
	}
}
