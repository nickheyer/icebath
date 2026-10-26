package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"
)

func TestDirectivesLayOverStaticHeadersAndRequestFields(t *testing.T) {
	var header http.Header
	var body map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		body = nil
		json.Unmarshal(raw, &body)
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	p := &Provider{
		Name: "t", Protocol: extraProtocol{}, BaseURL: srv.URL,
		Header: http.Header{"X-Bare": {"0"}},
		Extra:  map[string]json.RawMessage{"provider": json.RawMessage(`{"order":["a"]}`)},
		Ghost:  Directive{Extra: map[string]json.RawMessage{"store": json.RawMessage("false"), "provider": json.RawMessage(`{"zdr":true}`)}},
		Bare:   Directive{Header: http.Header{"X-Bare": {"1"}}},
	}
	req := protocol.Request{Model: "m", Extra: map[string]json.RawMessage{"store": json.RawMessage("true")}}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if header.Get("X-Bare") != "0" || string(body["store"]) != "true" || string(body["provider"]) != `{"order":["a"]}` {
		t.Fatalf("without directives the request goes as configured %v %v", header, body)
	}
	ctx := protocol.WithDirectives(context.Background(), func() protocol.Directives { return protocol.Directives{Ghost: true, Bare: true} })
	if _, err := p.Complete(ctx, req); err != nil {
		t.Fatal(err)
	}
	var routing map[string]any
	json.Unmarshal(body["provider"], &routing)
	if header.Get("X-Bare") != "1" || string(body["store"]) != "false" || routing["zdr"] != true || routing["order"] == nil {
		t.Fatalf("directives win over static headers and request fields, merging into nested ones %v %v", header, body)
	}
	if string(req.Extra["store"]) != "true" || len(p.Extra) != 1 {
		t.Fatal("the request and the provider extras are left as they were")
	}
	bare := &Provider{Name: "b", Protocol: extraProtocol{}, BaseURL: srv.URL, Ghost: Directive{Extra: map[string]json.RawMessage{"store": json.RawMessage("false")}}}
	if _, err := bare.Complete(ctx, protocol.Request{Model: "m"}); err != nil || string(body["store"]) != "false" {
		t.Fatalf("directive fields reach a request with no extras of its own %v %v", err, body)
	}
}

func TestDirectiveCopiesShareNothing(t *testing.T) {
	d := Directive{Header: http.Header{"X-Nebu-Ghost": {"1"}}, Extra: map[string]json.RawMessage{"store": json.RawMessage("false")}}
	c := d.Clone()
	c.Header.Set("X-Nebu-Ghost", "0")
	c.Extra["store"][0] = 't'
	if d.Header.Get("X-Nebu-Ghost") != "1" || string(d.Extra["store"]) != "false" {
		t.Fatal("a copy shares nothing")
	}
}
