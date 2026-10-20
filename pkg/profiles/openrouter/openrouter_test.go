package openrouter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
)

func TestProfileModelsAndKnobs(t *testing.T) {
	p, ok := profiles.Lookup(Name)
	if !ok || p.Vendor != Vendor || p.Key != profiles.Env(KeyEnv) {
		t.Fatalf("%+v %v", p, ok)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" || r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"data":[{"id":"anthropic/claude","name":"Claude","created":5,"context_length":200000,"top_provider":{"max_completion_tokens":32000}},{"id":"x/y","name":"Y"}]}`)
	}))
	defer srv.Close()
	p.BaseURL = srv.URL + "/api/v1"
	prov, _ := p.Build("k")
	models, err := p.ListModels(context.Background(), prov)
	if err != nil || len(models) != 2 || models[0].ID != "anthropic/claude" || models[0].Context != 200000 || models[0].MaxOutput != 32000 || models[1].MaxOutput != 0 {
		t.Fatalf("%v %+v", err, models)
	}
	h := Attribution("https://icebath", "icebath")
	if h.Get("HTTP-Referer") != "https://icebath" || h.Get("X-Title") != "icebath" || len(Attribution("", "")) != 0 {
		t.Fatal(h)
	}
	if string(Providers("anthropic", "google")["provider"]) != `{"allow_fallbacks":false,"order":["anthropic","google"]}` {
		t.Fatal(string(Providers("anthropic", "google")["provider"]))
	}
	prov.Auth = nil
	if _, err := Models(context.Background(), prov); err == nil {
		t.Fatal("auth failure swallowed")
	}
}
