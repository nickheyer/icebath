package google

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
)

func TestProfileAndModels(t *testing.T) {
	p, ok := profiles.Lookup(Name)
	if !ok || p.Vendor != Vendor || p.AuthName != "x-goog-api-key" || p.Key != profiles.Env(KeyEnv) {
		t.Fatalf("%+v %v", p, ok)
	}
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RequestURI())
		if r.Header.Get("x-goog-api-key") != "k" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("pageToken") == "" {
			io.WriteString(w, `{"models":[{"name":"models/gemini-2.5-pro","displayName":"Gemini 2.5 Pro","inputTokenLimit":1048576,"outputTokenLimit":65536,"supportedGenerationMethods":["generateContent","countTokens"]},{"name":"models/embedding-001","supportedGenerationMethods":["embedContent"]}],"nextPageToken":"p2"}`)
			return
		}
		io.WriteString(w, `{"models":[{"name":"models/gemini-2.5-flash","displayName":"Flash","supportedGenerationMethods":["generateContent"]}]}`)
	}))
	defer srv.Close()
	p.BaseURL = srv.URL
	prov, _ := p.Build("k")
	models, err := p.ListModels(context.Background(), prov)
	if err != nil || len(models) != 2 || models[0].ID != "gemini-2.5-pro" || models[0].Context != 1048576 || models[0].MaxOutput != 65536 || models[1].ID != "gemini-2.5-flash" {
		t.Fatalf("%v %+v", err, models)
	}
	if len(queries) != 2 || queries[1] != "/v1beta/models?pageSize=1000&pageToken=p2" {
		t.Fatalf("%v", queries)
	}
	if string(Thinking(1024)["generationConfig"]) != `{"thinkingConfig":{"thinkingBudget":1024,"includeThoughts":true}}` {
		t.Fatal(Thinking(1024))
	}
	prov.Auth = nil
	if _, err := Models(context.Background(), prov); err == nil {
		t.Fatal("auth failure swallowed")
	}
}
