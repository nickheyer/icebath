package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
)

func TestProfilesAndModels(t *testing.T) {
	p, ok := profiles.Lookup(Name)
	chat, ok2 := profiles.Lookup(ChatName)
	if !ok || !ok2 || p.Protocol != protocol.OpenAIResponses || chat.Protocol != protocol.OpenAICompletions || p.Vendor != Vendor || chat.Vendor != Vendor || p.Key != profiles.Env(KeyEnv) {
		t.Fatalf("%+v %+v", p, chat)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"object":"list","data":[{"id":"gpt-5","object":"model","created":1715367049,"owned_by":"system"},{"id":"gpt-5-mini","created":1}]}`)
	}))
	defer srv.Close()
	chat.BaseURL = srv.URL + "/v1"
	prov, _ := chat.Build("k")
	models, err := chat.ListModels(context.Background(), prov)
	if err != nil || len(models) != 2 || models[0].ID != "gpt-5" || models[0].Created != 1715367049 || models[1].ID != "gpt-5-mini" {
		t.Fatalf("%v %+v", err, models)
	}
	prov.Auth = nil
	if _, err := Models(context.Background(), prov); err == nil {
		t.Fatal("auth failure swallowed")
	}
}

func TestOptions(t *testing.T) {
	if string(Reasoning("high")["reasoning"]) != `{"effort":"high"}` || string(ChatReasoning("low")["reasoning_effort"]) != `"low"` {
		t.Fatal(Reasoning("high"), ChatReasoning("low"))
	}
	h := Organization("org-1", "proj-1")
	if h.Get("OpenAI-Organization") != "org-1" || h.Get("OpenAI-Project") != "proj-1" || len(Organization("", "")) != 0 {
		t.Fatal(h)
	}
}
