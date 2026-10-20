package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
)

func TestProfileAndModels(t *testing.T) {
	p, ok := profiles.Lookup(Name)
	if !ok || p.Vendor != Vendor || p.Key != profiles.Env(KeyEnv) || p.AuthName != "x-api-key" {
		t.Fatalf("%+v %v", p, ok)
	}
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") != version {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Query().Get("after_id") {
		case "":
			io.WriteString(w, `{"data":[{"id":"claude-a","display_name":"Claude A","created_at":"2025-02-19T00:00:00Z","type":"model"}],"has_more":true,"first_id":"claude-a","last_id":"claude-a"}`)
		default:
			io.WriteString(w, `{"data":[{"id":"claude-b","display_name":"Claude B"}],"has_more":false}`)
		}
	}))
	defer srv.Close()
	p.BaseURL = srv.URL
	prov, err := p.Build("k")
	if err != nil {
		t.Fatal(err)
	}
	models, err := p.ListModels(context.Background(), prov)
	if err != nil || len(models) != 2 || models[0].ID != "claude-a" || models[0].Name != "Claude A" || models[0].Created != 1739923200 || models[1].Created != 0 {
		t.Fatalf("%v %+v", err, models)
	}
	if len(paths) != 2 || paths[1] != "/v1/models?after_id=claude-a&limit=1000" {
		t.Fatalf("%v", paths)
	}
	prov.Auth = nil
	if _, err := Models(context.Background(), prov); err == nil {
		t.Fatal("auth failure swallowed")
	}
}

func TestKnobsAndCountTokens(t *testing.T) {
	if string(Thinking(2048)["thinking"]) != `{"type":"enabled","budget_tokens":2048}` {
		t.Fatal(Thinking(2048))
	}
	if Beta("a", "b").Get("anthropic-beta") != "a,b" {
		t.Fatal(Beta("a", "b"))
	}
	var got map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		io.WriteString(w, `{"input_tokens":42}`)
	}))
	defer srv.Close()
	p := Profile()
	p.BaseURL = srv.URL
	prov, _ := p.Build("k")
	temp := 0.5
	n, err := CountTokens(context.Background(), prov, protocol.Request{Model: "claude-a", System: "sys", Temperature: &temp, Stream: true, Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "hi"}}}}})
	if err != nil || n != 42 {
		t.Fatalf("%v %d", err, n)
	}
	for _, k := range []string{"max_tokens", "stream", "temperature"} {
		if _, has := got[k]; has {
			t.Fatalf("%s must be stripped", k)
		}
	}
	if string(got["system"]) != `"sys"` || string(got["model"]) != `"claude-a"` {
		t.Fatalf("%v", got)
	}
	if _, err := CountTokens(context.Background(), prov, protocol.Request{Model: "m", Messages: []protocol.Message{{Role: "ghost", Content: []protocol.Part{protocol.Text{Text: "x"}}}}}); err == nil {
		t.Fatal("unencodable request accepted")
	}
}
