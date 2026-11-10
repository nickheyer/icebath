package provider

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Bounce recorder standing in for the runtime refresh a staged model triggers
func lmBounceRecorder(t *testing.T) func() bool {
	t.Helper()
	var mu sync.Mutex
	bounced := false
	lmDeps.refreshLocalRuntime = func(context.Context) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		bounced = true
		return true, nil
	}
	return func() bool {
		mu.Lock()
		defer mu.Unlock()
		return bounced
	}
}

func TestSearchRouteRequiresQueryAndMapsErrors(t *testing.T) {
	lmRoutesClient(t)
	data, err := LocalModelsSearch(context.Background(), "  ", 20)
	if err != nil {
		t.Fatal(err)
	}
	if hits, ok := data["hits"].([]any); !ok || len(hits) != 0 || len(data) != 1 {
		t.Fatalf("blank query %v", data)
	}
	lmDeps.searchModels = func(q string, limit int) ([]HFModelHit, error) { return nil, errors.New("HF down") }
	_, err = LocalModelsSearch(context.Background(), "qwen", 20)
	if lmStatusOf(err) != 502 || err.(*HTTPException).Detail != "Hugging Face search unavailable: HF down" {
		t.Fatalf("search failure %v", err)
	}
}

func TestBrowsedDownloadStagesAndBounces(t *testing.T) {
	lmRoutesClient(t)
	lmFakeOpener(t, append([]byte("GGUF"), bytes.Repeat([]byte{0}, 60)...))
	bounced := lmBounceRecorder(t)
	resp, err := LocalModelsDownloadBrowsed(context.Background(), "someone/Some-GGUF", []string{"Some-Model-Q4_K_M.gguf"})
	if err != nil {
		t.Fatal(err)
	}
	status := lmPoll(t, resp["job_id"].(string), 10*time.Second, "done", "error")
	if status["status"] != "done" {
		t.Fatalf("status %v", status["error"])
	}
	if _, err := os.Stat(filepath.Join(localModelsDir(), "Some-Model-Q4_K_M.gguf")); err != nil {
		t.Fatal(err)
	}
	if !bounced() {
		t.Fatal("router was not bounced")
	}
}

func TestBrowsedDownloadRejectsNonGguf(t *testing.T) {
	lmRoutesClient(t)
	if _, err := LocalModelsDownloadBrowsed(context.Background(), "a/b", []string{"model.safetensors"}); lmStatusOf(err) != 422 {
		t.Fatalf("err %v", err)
	}
}

func TestSideloadLinksAndBounces(t *testing.T) {
	lmRoutesClient(t)
	src := filepath.Join(t.TempDir(), "My-Local-Model-Q5_K_M.gguf")
	if err := os.WriteFile(src, append([]byte("GGUF"), bytes.Repeat([]byte{0}, 32)...), 0o644); err != nil {
		t.Fatal(err)
	}
	bounced := lmBounceRecorder(t)
	resp, err := LocalModelsSideload(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if resp["model_id"] != "My-Local-Model-Q5_K_M" {
		t.Fatalf("model_id %v", resp["model_id"])
	}
	if _, err := os.Stat(filepath.Join(localModelsDir(), filepath.Base(src))); err != nil {
		t.Fatal(err)
	}
	if !bounced() {
		t.Fatal("router was not bounced")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("original touched %v", err)
	}
	again, err := LocalModelsSideload(context.Background(), src)
	if err != nil || again["already_present"] != true {
		t.Fatalf("second sideload %v %v", again, err)
	}
}

func TestSideloadRejectsNonGguf(t *testing.T) {
	lmRoutesClient(t)
	src := filepath.Join(t.TempDir(), "model.bin")
	if err := os.WriteFile(src, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LocalModelsSideload(context.Background(), src); lmStatusOf(err) != 422 {
		t.Fatalf("err %v", err)
	}
}

func TestSideloadReadsThePathAsPathlibDoes(t *testing.T) {
	lmRoutesClient(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "Trailing-Q4_K_M.gguf")
	if err := os.WriteFile(src, []byte("GGUF"), 0o644); err != nil {
		t.Fatal(err)
	}
	lmBounceRecorder(t)
	resp, err := LocalModelsSideload(context.Background(), dir+"//./Trailing-Q4_K_M.gguf/")
	if err != nil || resp["model_id"] != "Trailing-Q4_K_M" {
		t.Fatalf("sideload %v %v", resp, err)
	}
	if _, err := os.Lstat(filepath.Join(localModelsDir(), "Trailing-Q4_K_M.gguf")); err != nil {
		t.Fatal(err)
	}
}

func TestSetRuntimeEnabledRefusesAListSectionLikePython(t *testing.T) {
	lmRoutesClient(t)
	configLoadFn = func(context.Context) (map[string]any, error) { return map[string]any{"local_runtime": []any{}}, nil }
	if _, err := lmSetRuntimeEnabled(context.Background(), true); err == nil || err.Error() != "list indices must be integers or slices, not str" {
		t.Fatalf("list section %v", err)
	}
	configLoadFn = func(context.Context) (map[string]any, error) { return map[string]any{"local_runtime": nil}, nil }
	if _, err := lmSetRuntimeEnabled(context.Background(), true); err == nil || err.Error() != "'NoneType' object does not support item assignment" {
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
		{"/temp", "/models/unload", []byte(`{"model": "m"}`), "HTTP Error 307: Temporary Redirect"},
		{"/loop", "/models", nil, "HTTP Error 302: " + lmRedirectLoopMsg + "Found"},
		{"/file", "/models", nil, "HTTP Error 302: Found - Redirection to url 'file:///etc/passwd' is not allowed"},
		{"/gone", "/models", nil, "HTTP Error 404: Not Found"},
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
