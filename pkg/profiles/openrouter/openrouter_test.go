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

func TestModelsCarryPricingAndCapabilities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"anthropic/claude-sonnet-4.6","name":"Claude Sonnet 4.6","context_length":1000000,"top_provider":{"max_completion_tokens":128000},
		"pricing":{"prompt":"0.000003","completion":"0.000015","input_cache_read":"0.0000003","input_cache_write":"0.00000375"},
		"architecture":{"input_modalities":["text","image","file"]},"supported_parameters":["tools","reasoning","max_tokens"]},
		{"id":"bare/model","name":"Bare"}]}`)
	}))
	defer srv.Close()
	p := Profile()
	p.BaseURL = srv.URL
	prov, err := p.Build("key")
	if err != nil {
		t.Fatal(err)
	}
	models, err := Models(context.Background(), prov)
	if err != nil || len(models) != 2 {
		t.Fatalf("%v %+v", err, models)
	}
	m := models[0]
	if m.Pricing == nil || m.Pricing.Input != 3 || m.Pricing.Output != 15 || m.Pricing.CacheRead < 0.29 || m.Pricing.CacheRead > 0.31 || m.Pricing.CacheWrite < 3.74 || m.Pricing.CacheWrite > 3.76 {
		t.Fatalf("%+v", m.Pricing)
	}
	if m.Capabilities == nil || !m.Capabilities.Tools || !m.Capabilities.Thinking || !m.Capabilities.Vision || !m.Capabilities.Documents || m.Vendor != Vendor {
		t.Fatalf("%+v", m)
	}
	if models[1].Pricing != nil || models[1].Capabilities != nil {
		t.Fatalf("missing metadata must stay nil: %+v", models[1])
	}
	if perMillion("junk") != 0 || perMillion("-1") != 0 {
		t.Fatal("bad rates must be zero")
	}
}
