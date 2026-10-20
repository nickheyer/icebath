package mistral

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
	if !ok || p.Vendor != Vendor || p.Auth != profiles.AuthBearer || p.Key != profiles.Env(KeyEnv) {
		t.Fatalf("%+v %v", p, ok)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"object":"list","data":[{"id":"mistral-large-latest","name":"Large","created":1,"max_context_length":131072,"capabilities":{"completion_chat":true}},{"id":"mistral-embed","capabilities":{"completion_chat":false}},{"id":"legacy"}]}`)
	}))
	defer srv.Close()
	p.BaseURL = srv.URL
	prov, _ := p.Build("k")
	models, err := p.ListModels(context.Background(), prov)
	if err != nil || len(models) != 2 || models[0].ID != "mistral-large-latest" || models[0].Name != "Large" || models[0].Context != 131072 || models[1].Name != "legacy" {
		t.Fatalf("%v %+v", err, models)
	}
	prov.Auth = nil
	if _, err := Models(context.Background(), prov); err == nil {
		t.Fatal("auth failure swallowed")
	}
}
