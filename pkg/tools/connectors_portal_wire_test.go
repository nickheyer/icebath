package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"
)

type outcomeLog struct {
	mu   sync.Mutex
	recs []string
}

func (h *outcomeLog) Enabled(context.Context, slog.Level) bool { return true }
func (h *outcomeLog) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *outcomeLog) WithGroup(string) slog.Handler            { return h }

// Records each connector outcome as its level and the upstream log line
func (h *outcomeLog) Handle(_ context.Context, r slog.Record) error {
	a := map[string]string{}
	r.Attrs(func(at slog.Attr) bool { a[at.Key] = at.Value.String(); return true })
	h.mu.Lock()
	h.recs = append(h.recs, fmt.Sprintf("%s connector %s %s %s request_id=%s dispatch=%s", r.Level, a["route"], a["status"], a["code"], a["request_id"], a["dispatch"]))
	h.mu.Unlock()
	return nil
}

type portalReply struct {
	status    int
	body      string
	requestID string
	etag      string
}

type portalSent struct {
	method, url string
	headers     map[string]string
	body        any
}

// Portal client over a scripted transport answering replies in order
func scriptedPortal(env *Env, replies ...portalReply) (*PortalConnectorClient, *[]portalSent) {
	var sent []portalSent
	transport := func(_ context.Context, method, rawURL string, headers map[string]string, body any, _ time.Duration) (*http.Response, error) {
		sent = append(sent, portalSent{method, rawURL, headers, body})
		r := replies[0]
		replies = replies[1:]
		h := http.Header{"X-Request-Id": {r.requestID}}
		if r.etag != "" {
			h.Set("Etag", r.etag)
		}
		return &http.Response{StatusCode: r.status, Header: h, Body: io.NopCloser(bytes.NewReader([]byte(r.body)))}, nil
	}
	return NewPortalConnectorClient(context.Background(), env, PortalOptions{Transport: transport,
		EndpointResolver: func() (string, error) { return "https://portal.test", nil },
		HeaderProvider:   func(string) map[string]string { return map[string]string{"Authorization": "Bearer t"} }}), &sent
}

func TestPortalTools304IsASuccessAndABadBodyAFailure(t *testing.T) {
	logs := &outcomeLog{}
	env := &Env{Home: t.TempDir(), Log: slog.New(logs)}
	client, sent := scriptedPortal(env, portalReply{304, "", "same-id", ""}, portalReply{200, "[]", "bad-id", ""})
	got, err := client.Tools("gmail", "etag-1")
	if _, ok := got.(PortalNotModified); !ok || err != nil {
		t.Fatalf("tools %v %v", got, err)
	}
	if (*sent)[0].headers["If-None-Match"] != "etag-1" {
		t.Fatalf("headers %v", (*sent)[0].headers)
	}
	_, err = client.Tools("gmail", "")
	var gerr *ToolGatewayError
	if !errors.As(err, &gerr) || gerr.Kind != PortalKindToolsUnavailable || gerr.RequestID != "bad-id" {
		t.Fatalf("error %#v", err)
	}
	want := []string{"DEBUG connector portal.connectors.tools 304 - request_id=same-id dispatch=-",
		"WARN connector portal.connectors.tools 200 INVALID_RESPONSE request_id=bad-id dispatch=-"}
	if !reflect.DeepEqual(logs.recs, want) {
		t.Fatalf("outcomes %q", logs.recs)
	}
}

func TestPortalToolsListingParsesStrictRowsAndTakesTheHeaderEtag(t *testing.T) {
	body := `{"connector":"gmail","toolkitVersion":"v2","etag":"body-etag","extra":1,"tools":[` +
		`{"slug":"GMAIL_SEND","name":"Send","description":"d","facet":"exotic","hints":["h"],"categories":["Mail","ÉTÉ"],"noAuth":false,"deprecated":true}]}`
	client, _ := scriptedPortal(&Env{Home: t.TempDir()}, portalReply{200, body, "r1", "header-etag"},
		portalReply{200, `{"connector":"gmail","toolkit_version":"v2","etag":"e","tools":[{"slug":"S","name":"n","description":"d","facet":"read","hints":[],"categories":[],"no_auth":1,"deprecated":false}]}`, "r2", ""})
	got, err := client.Tools("gmail", "")
	if err != nil {
		t.Fatal(err)
	}
	want := ConnectorToolsListing{Connector: "gmail", ToolkitVersion: "v2", Etag: "header-etag", Tools: []ConnectorTool{{Slug: "GMAIL_SEND", Name: "Send",
		Description: "d", Facet: "unclassified", Hints: []string{"h"}, Categories: []string{"mail", "été"}, Deprecated: true}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listing %#v", got)
	}
	if _, err := client.Tools("gmail", ""); err == nil {
		t.Fatal("a non bool no_auth passed the strict model")
	}
	if _, err := client.Tools("Bad Slug", ""); err == nil {
		t.Fatal("invalid slug accepted")
	}
}

func TestPortalToolsMapsGatewayFailuresToToolsUnavailable(t *testing.T) {
	client, _ := scriptedPortal(&Env{Home: t.TempDir()}, portalReply{404, `{"error":{"code":"route_missing","message":"no"}}`, "r1", ""},
		portalReply{401, `{"error":{"code":"unauthorized","message":"no"}}`, "r2", ""})
	_, err := client.Tools("gmail", "")
	var gerr *ToolGatewayError
	if !errors.As(err, &gerr) || gerr.Kind != PortalKindToolsUnavailable || gerr.Message != "portal tools unavailable" {
		t.Fatalf("missing route %#v", err)
	}
	_, err = client.Tools("gmail", "")
	if !errors.As(err, &gerr) || gerr.Kind == PortalKindToolsUnavailable {
		t.Fatalf("auth failure %#v", err)
	}
}

func TestPortalCatalogPublicCatalogAndPolicy(t *testing.T) {
	catalog := `{"connectors":[{"slug":"gmail","name":"Gmail","description":"d","category":"mail","logoUrl":"https://x/l.png"},{"slug":"x","name":"X","description":"","category":"c","logo_url":null}]}`
	policy := `{"layers":[{"kind":"org","body":{"mode":"allow","connectors":["gmail"],"tools":{"gmail":{"disable":["GMAIL_DELETE"]},"x":{}},"tags":{"enable":["safe"]}},"revision":"r1"}],` +
		`"effective":{"mode":"deny","disabledConnectors":["x"],"version":1,"revision":"r2","issuedAtMs":17}}`
	client, sent := scriptedPortal(&Env{Home: t.TempDir()}, portalReply{200, catalog, "c1", ""}, portalReply{200, catalog, "c2", ""},
		portalReply{200, policy, "p1", ""}, portalReply{200, `{"revision":"r3","effective":{"mode":"deny-all","version":1,"revision":"r3","issuedAtMs":18}}`, "p2", ""},
		portalReply{200, `{"revision":"r4","effective":{"mode":"deny-all","version":1,"revision":"r4","issuedAtMs":18.0}}`, "p3", ""},
		portalReply{200, `{"layers":[],"effective":{"mode":"open","version":1,"revision":"r","issuedAtMs":1}}`, "p4", ""})
	logo := "https://x/l.png"
	wantCatalog := ConnectorCatalogResponse{Connectors: []ConnectorCatalogRow{{Slug: "gmail", Name: "Gmail", Description: "d", Category: "mail", LogoURL: &logo},
		{Slug: "x", Name: "X", Category: "c"}}}
	for i, read := range []func() (ConnectorCatalogResponse, error){client.Catalog, client.PublicCatalog} {
		got, err := read()
		if err != nil || !reflect.DeepEqual(got, wantCatalog) {
			t.Fatalf("catalog %d %#v %v", i, got, err)
		}
	}
	if _, auth := (*sent)[1].headers["Authorization"]; auth || (*sent)[1].url != "https://portal.test/api/v1/connectors/catalog/public" || (*sent)[0].headers["Authorization"] != "Bearer t" {
		t.Fatalf("catalog requests %v", *sent)
	}
	got, err := client.Policy()
	wantPolicy := ConnectorPolicyResponse{Layers: []ConnectorPolicyLayer{{Kind: "org", Revision: "r1", Body: PolicyBody{Mode: "allow", Connectors: []string{"gmail"},
		ToolKeys: []string{"gmail", "x"}, Tools: map[string]PolicyToolRule{"gmail": {Disable: []string{"GMAIL_DELETE"}}, "x": {Disable: []string{}}},
		Tags: &PolicyTags{Enable: []string{"safe"}}}}},
		Effective: ConnectorPolicyEffective{PolicyBody: PolicyBody{Mode: "deny", DisabledConnectors: []string{"x"}, ToolKeys: []string{}, Tools: map[string]PolicyToolRule{}},
			Version: 1, Revision: "r2", IssuedAtMs: 17}}
	if err != nil || !reflect.DeepEqual(got, wantPolicy) {
		t.Fatalf("policy %#v %v", got, err)
	}
	wrote, err := client.SetPolicy(map[string]any{"mode": "deny-all"})
	if err != nil || wrote.Revision != "r3" || wrote.Effective.Mode != "deny-all" || wrote.Effective.IssuedAtMs != 18 {
		t.Fatalf("set policy %#v %v", wrote, err)
	}
	if s := (*sent)[3]; s.method != "PUT" || s.url != "https://portal.test/api/v1/connectors/policy" || !reflect.DeepEqual(s.body, map[string]any{"mode": "deny-all"}) {
		t.Fatalf("set policy request %v", s)
	}
	for _, label := range []string{"float issuedAtMs", "unknown mode"} {
		var gerr *ToolGatewayError
		var err error
		if label == "float issuedAtMs" {
			_, err = client.SetPolicy(map[string]any{})
		} else {
			_, err = client.Policy()
		}
		if !errors.As(err, &gerr) || gerr.Code != "INVALID_RESPONSE" || gerr.Message != "portal policy unavailable" {
			t.Fatalf("%s %#v", label, err)
		}
	}
	raw, _ := json.Marshal(wantPolicy.Layers[0].Body.ToolKeys)
	if string(raw) != `["gmail","x"]` {
		t.Fatalf("tool order %s", raw)
	}
}
