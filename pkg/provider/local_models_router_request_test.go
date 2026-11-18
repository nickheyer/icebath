package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func TestSetRuntimeEnabledRefusesAListSectionLikePython(t *testing.T) {
	lmRoutesClient(t)
	configLoadFn = func(context.Context) (map[string]any, error) { return map[string]any{"local_runtime": []any{}}, nil }
	if _, err := lmSetRuntimeEnabled(context.Background(), true); err == nil || err.Error() != "the local_models section of config.yaml is a list, expected a mapping" {
		t.Fatalf("list section %v", err)
	}
	configLoadFn = func(context.Context) (map[string]any, error) { return map[string]any{"local_runtime": nil}, nil }
	if _, err := lmSetRuntimeEnabled(context.Background(), true); err == nil || err.Error() != "the local_models section of config.yaml is nothing, expected a mapping" {
		t.Fatalf("null section %v", err)
	}
}

func TestRouterRequestFollowsRedirectsAsUrllibDoes(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Content-Type")+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		switch r.URL.Path {
		case "/moved/models":
			http.Redirect(w, r, "/models", http.StatusFound)
		case "/temp/models/unload":
			http.Redirect(w, r, "/models/unload", http.StatusTemporaryRedirect)
		case "/loop/models":
			http.Redirect(w, r, "/loop/models", http.StatusFound)
		case "/file/models":
			w.Header().Set("Location", "file:///etc/passwd")
			w.WriteHeader(http.StatusFound)
		case "/models":
			_, _ = w.Write([]byte(`{"data": [{"id": "m"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	endpoint := func(prefix string) map[string]any {
		return map[string]any{"base_url": srv.URL + prefix + "/v1", "api_key": "k"}
	}
	got, err := lmRouterRequest(endpoint("/moved"), "/models", 5*time.Second, nil)
	if err != nil || !reflect.DeepEqual(got, map[string]any{"data": []any{map[string]any{"id": "m"}}}) {
		t.Fatalf("followed get %v %v", got, err)
	}
	mu.Lock()
	seen = nil
	mu.Unlock()
	if _, err := lmRouterRequest(endpoint("/moved"), "/models", 5*time.Second, []byte(`{"model": "m"}`)); err != nil {
		t.Fatalf("post 302 %v", err)
	}
	mu.Lock()
	if !reflect.DeepEqual(seen, []string{"POST /moved/models application/json Bearer k", "GET /models  Bearer k"}) {
		t.Fatalf("post 302 became %q", seen)
	}
	mu.Unlock()
	cases := []struct {
		prefix, path string
		payload      []byte
		text         string
	}{
		{"/temp", "/models/unload", []byte(`{"model": "m"}`), "HTTP 307 Temporary Redirect"},
		{"/loop", "/models", nil, "HTTP 302 " + lmRedirectLoopMsg + "Found"},
		{"/file", "/models", nil, "HTTP 302 Found, the redirect to file:///etc/passwd is not allowed"},
		{"/gone", "/models", nil, "HTTP 404 Not Found"},
	}
	for _, c := range cases {
		_, err := lmRouterRequest(endpoint(c.prefix), c.path, 5*time.Second, c.payload)
		if err == nil || err.Error() != c.text {
			t.Fatalf("%s: %v", c.prefix, err)
		}
	}
	_, err = lmRouterRequest(endpoint("/gone"), "/models", 5*time.Second, nil)
	if repr := pyrepr.ExceptionRepr(err); repr != "<HTTPError 404: 'Not Found'>" {
		t.Fatalf("repr %s", repr)
	}
}
