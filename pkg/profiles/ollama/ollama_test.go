package ollama

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/profiles"
)

func TestProfileModelsAndKnobs(t *testing.T) {
	p, ok := profiles.Lookup(Name)
	if !ok || p.Vendor != Vendor || p.Auth != profiles.AuthNone || p.BaseURL != Host(p.BaseURL) {
		t.Fatalf("%+v %v", p, ok)
	}
	for in, want := range map[string]string{"": BaseURL, "10.0.0.5:11434": "http://10.0.0.5:11434", "https://ollama.local/": "https://ollama.local", " host ": "http://host"} {
		if Host(in) != want {
			t.Fatalf("%q: %s", in, Host(in))
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		io.WriteString(w, `{"models":[{"name":"llama3.2:latest","model":"llama3.2:latest","modified_at":"2025-01-01T00:00:00Z","details":{"family":"llama","parameter_size":"3.2B"}},{"name":"bare"}]}`)
	}))
	defer srv.Close()
	p = Profile(srv.URL)
	prov, _ := p.Open(context.Background())
	models, err := p.ListModels(context.Background(), prov)
	if err != nil || len(models) != 2 || models[0].ID != "llama3.2:latest" || models[0].Name != "llama 3.2B" || models[0].Created != 1735689600 || models[1].Name != "bare" {
		t.Fatalf("%v %+v", err, models)
	}
	if string(ContextWindow(8192)["options"]) != `{"num_ctx":8192}` || string(KeepAlive(10 * time.Minute)["keep_alive"]) != `"10m0s"` {
		t.Fatal(ContextWindow(8192), KeepAlive(10*time.Minute))
	}
	prov.BaseURL = srv.URL + "/nope"
	if _, err := Models(context.Background(), prov); err == nil {
		t.Fatal("http failure swallowed")
	}
}

func TestProfileFromEnvironmentFallsBackOnBadHost(t *testing.T) {
	if p := fromEnv(""); p.BaseURL != BaseURL {
		t.Fatalf("%+v", p)
	}
	if p := fromEnv("gpu-box:11434"); p.BaseURL != "http://gpu-box:11434" {
		t.Fatalf("%+v", p)
	}
	if p := fromEnv("http://bad host/"); p.BaseURL != BaseURL {
		t.Fatalf("an unusable host must fall back instead of panicking: %+v", p)
	}
}
