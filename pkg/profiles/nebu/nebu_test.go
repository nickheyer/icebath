package nebu

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
)

func TestProfileAsksThroughNebuHeaders(t *testing.T) {
	p, ok := profiles.Lookup(Name)
	if !ok || p.Protocol != protocol.OpenAICompletions || p.Vendor != "openai" || p.Key != profiles.Env(KeyEnv) || p.Ghost.Header.Get(GhostHeader) != "1" || p.Bare.Header.Get(BareHeader) != "1" {
		t.Fatalf("%+v", p)
	}
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"object":"list","data":[{"id":"freddie","object":"model"}]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c","object":"chat.completion","model":"freddie","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	p.BaseURL = srv.URL + "/v1"
	prov, err := p.Build("k")
	if err != nil {
		t.Fatal(err)
	}
	models, err := p.ListModels(context.Background(), prov)
	if err != nil || len(models) != 1 || models[0].ID != "freddie" || got.Get(GhostHeader) != "" {
		t.Fatalf("models are listed without directives %v %+v", err, models)
	}
	ctx := protocol.WithDirectives(context.Background(), func() protocol.Directives { return protocol.Directives{Ghost: true, Bare: true} })
	if _, err := prov.Complete(ctx, protocol.Request{Model: "freddie", Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "hi"}}}}}); err != nil {
		t.Fatal(err)
	}
	if got.Get(GhostHeader) != "1" || got.Get(BareHeader) != "1" || got.Get("Authorization") != "Bearer k" {
		t.Fatalf("a turn asking for both carries both headers %v", got)
	}
	if _, err := prov.Complete(context.Background(), protocol.Request{Model: "freddie"}); err != nil || got.Get(GhostHeader) != "" || got.Get(BareHeader) != "" {
		t.Fatalf("a turn asking for neither carries neither %v %v", err, got)
	}
}
