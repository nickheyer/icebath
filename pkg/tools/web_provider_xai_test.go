package tools

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func xaiP() *xaiWebProvider { return webProvider(nil, "xai").(*xaiWebProvider) }

// xAI env keyed by XAI_API_KEY whose base url is the fake server
func xaiKeyedEnv(t *testing.T, base string, settings map[string]any) *Env {
	t.Helper()
	t.Setenv("XAI_BASE_URL", "")
	os.Unsetenv("XAI_BASE_URL")
	return httpxProviderEnv(t, "XAI_API_KEY=xai-test-key\nXAI_BASE_URL="+base+"\n", settings)
}

func usePools(t *testing.T, p CredentialPoolSource) {
	prev := CredentialPools
	CredentialPools = p
	t.Cleanup(func() { CredentialPools = prev })
}

// Responses API answer whose single message carries the text and annotations
func xaiAnswer(text string, annotations string) string {
	if annotations == "" {
		annotations = "[]"
	}
	return `{"output":[{"type":"reasoning"},{"type":"message","content":[{"type":"output_text","text":` + jsonCompact(text) + `,"annotations":` + annotations + `}]}]}`
}

func TestXAIProviderIsAvailable(t *testing.T) {
	env := httpxProviderEnv(t, "", nil)
	if xaiP().Available(env) {
		t.Fatal("available without credentials")
	}
	os.MkdirAll(filepath.Dir(home.AuthPath(env.Home)), 0o700)
	os.WriteFile(home.AuthPath(env.Home), []byte("{corrupt"), 0o600)
	if xaiP().Available(env) {
		t.Fatal("corrupted store counted")
	}
	os.WriteFile(home.AuthPath(env.Home), []byte(`{"providers":{"xai-oauth":{"tokens":{"access_token":"tok"}}}}`), 0o600)
	pools := &fakePools{}
	usePools(t, pools)
	if !xaiP().Available(env) || len(pools.asked) != 0 {
		t.Fatalf("store tokens ignored or resolver called %v", pools.asked)
	}
	if !xaiP().Available(httpxProviderEnv(t, "XAI_API_KEY=k\n", nil)) {
		t.Fatal("env key ignored")
	}
}

func TestXAIProviderSearchJSONPath(t *testing.T) {
	var answer string
	srv, calls := recordingServer(t, func(r recordedRequest, w http.ResponseWriter) { writeVendorJSON(w, 200, answer) })
	env := xaiKeyedEnv(t, srv.URL+"/", nil)
	answer = xaiAnswer(`{"results":[{"title":" A ","url":"https://a.example","description":"first"},{"title":"B","url":"https://b.example","description":"second"}]}`, "")
	resp, err := xaiP().Search(context.Background(), env, "q", 5)
	if err != nil || !resp.Success || len(resp.Data.Web) != 2 || resp.Data.Web[0].Title != "A" || resp.Data.Web[1].Position != 2 {
		t.Fatalf("%+v %v", resp, err)
	}
	c := calls()[0]
	if c.path != "/responses" || c.header.Get("Authorization") != "Bearer xai-test-key" || c.header.Get("User-Agent") != icebathXAIUserAgent() {
		t.Fatalf("%+v", c)
	}
	tools := c.body["tools"].([]any)
	if c.body["model"] != xaiWebDefaultModel || tools[0].(map[string]any)["type"] != "web_search" || tools[0].(map[string]any)["filters"] != nil || c.body["include"].([]any)[0] != "no_inline_citations" {
		t.Fatalf("%v", c.body)
	}
	prompt := c.body["input"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(prompt, "Return at most 5 results") || !strings.HasSuffix(prompt, "Query: q") {
		t.Fatal(prompt)
	}
	answer = xaiAnswer("Sure, here you go:\n"+`{"results":[{"title":"P","url":"https://p.example","description":"d"}]}`+"\nDone.", "")
	if resp, _ := xaiP().Search(context.Background(), env, "q", 5); len(resp.Data.Web) != 1 || resp.Data.Web[0].URL != "https://p.example" {
		t.Fatalf("%+v", resp)
	}
	answer = xaiAnswer(`{"results":[{"title":"no url"},"junk",{"title":"K","url":"https://k.example"}]}`, "")
	if resp, _ := xaiP().Search(context.Background(), env, "q", 5); len(resp.Data.Web) != 1 || resp.Data.Web[0].Position != 1 || resp.Data.Web[0].URL != "https://k.example" {
		t.Fatalf("%+v", resp)
	}
}

func TestXAIProviderSearchFallbacks(t *testing.T) {
	var answer string
	srv, _ := recordingServer(t, func(r recordedRequest, w http.ResponseWriter) { writeVendorJSON(w, 200, answer) })
	env := xaiKeyedEnv(t, srv.URL, nil)
	text := "Rust is a systems language. See the book."
	answer = xaiAnswer(text, `[{"type":"url_citation","url":"https://rust.example","start_index":27,"end_index":40},{"type":"url_citation","url":"https://rust.example"},{"type":"url_citation","url":" https://two.example ","start_index":0,"end_index":999}]`)
	resp, _ := xaiP().Search(context.Background(), env, "q", 5)
	if len(resp.Data.Web) != 2 || resp.Data.Web[0].Description != "Rust is a systems language." || resp.Data.Web[1].URL != "https://two.example" || resp.Data.Web[1].Description != "" {
		t.Fatalf("%+v", resp.Data.Web)
	}
	answer = `{"output":[],"citations":["https://c1.example",5,"  ","https://c4.example"]}`
	resp, _ = xaiP().Search(context.Background(), env, "q", 5)
	if len(resp.Data.Web) != 2 || resp.Data.Web[1].Position != 4 {
		t.Fatalf("%+v", resp.Data.Web)
	}
	answer = `{"output":[]}`
	if resp, _ := xaiP().Search(context.Background(), env, "q", 5); !resp.Success || len(resp.Data.Web) != 0 {
		t.Fatalf("%+v", resp)
	}
}

func TestXAIProviderRequestShapeAndConfig(t *testing.T) {
	srv, calls := recordingServer(t, func(r recordedRequest, w http.ResponseWriter) { writeVendorJSON(w, 200, `{"output":[]}`) })
	env := xaiKeyedEnv(t, srv.URL, map[string]any{"web.xai": map[string]any{"model": " grok-x ", "allowed_domains": []any{"a.com", " ", "b.com", 3, "c.com", "d.com", "e.com", "f.com"}}})
	xaiP().Search(context.Background(), env, "q", 500)
	c := calls()[0]
	filters := c.body["tools"].([]any)[0].(map[string]any)["filters"].(map[string]any)
	if c.body["model"] != "grok-x" || len(filters["allowed_domains"].([]any)) != 5 || filters["excluded_domains"] != nil {
		t.Fatalf("%v", c.body)
	}
	if prompt := c.body["input"].([]any)[0].(map[string]any)["content"].(string); !strings.Contains(prompt, "at most 100 results") {
		t.Fatal(prompt)
	}
	both := xaiKeyedEnv(t, srv.URL, map[string]any{"web.xai": map[string]any{"allowed_domains": []any{"a.com"}, "excluded_domains": []any{"b.com"}}})
	if resp, _ := xaiP().Search(context.Background(), both, "q", 5); resp.Error != "web.xai.allowed_domains and web.xai.excluded_domains cannot both be set (xAI restriction)." || len(calls()) != 1 {
		t.Fatalf("%+v", resp)
	}
	if resp, _ := xaiP().Search(context.Background(), httpxProviderEnv(t, "", nil), "q", 5); resp.Success || !strings.Contains(resp.Error, "No xAI credentials found") {
		t.Fatalf("%+v", resp)
	}
	if xaiP().SetupSchema()["post_setup"] != "xai_grok" {
		t.Fatal("setup schema")
	}
}

func TestXAIProviderSearchErrors(t *testing.T) {
	var status int
	var body string
	srv, calls := recordingServer(t, func(r recordedRequest, w http.ResponseWriter) {
		if r.header.Get("Authorization") == "Bearer fresh-token" {
			writeVendorJSON(w, 200, xaiAnswer(`{"results":[{"title":"R","url":"https://r.example"}]}`, ""))
			return
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	})
	status, body = 401, "unauthorized"
	pools := &fakePools{token: "stale-token", base: srv.URL, refreshed: "fresh-token"}
	usePools(t, pools)
	oauth := httpxProviderEnv(t, "", nil)
	resp, _ := xaiP().Search(context.Background(), oauth, "q", 5)
	if !resp.Success || len(calls()) != 2 {
		t.Fatalf("%+v %v", resp, pools.asked)
	}
	if !containsString(pools.asked, "refresh:stale-token") {
		t.Fatalf("no forced refresh %v", pools.asked)
	}
	usePools(t, nil)
	env := xaiKeyedEnv(t, srv.URL, nil)
	before := len(calls())
	if resp, _ := xaiP().Search(context.Background(), env, "q", 5); resp.Error != "xAI web search returned HTTP 401: unauthorized" || len(calls()) != before+1 {
		t.Fatalf("%+v", resp)
	}
	status, body = 200, `{"error":{"code":"overloaded"}}`
	if resp, _ := xaiP().Search(context.Background(), env, "q", 5); resp.Error != "xAI returned an error: overloaded" {
		t.Fatalf("%+v", resp)
	}
	status, body = 200, "not json"
	if resp, _ := xaiP().Search(context.Background(), env, "q", 5); resp.Error != "Could not parse xAI Responses API reply as JSON" {
		t.Fatalf("%+v", resp)
	}
	status, body = 503, strings.Repeat("x", 400)+"   "
	if resp, _ := xaiP().Search(context.Background(), env, "q", 5); resp.Error != "xAI web search returned HTTP 503: "+strings.Repeat("x", 300) {
		t.Fatalf("%+v", resp)
	}
	if resp, _ := xaiP().Search(context.Background(), xaiKeyedEnv(t, "http://127.0.0.1:1", nil), "q", 5); !strings.HasPrefix(resp.Error, "Could not reach xAI: [") {
		t.Fatalf("%+v", resp)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if resp, _ := xaiP().Search(ctx, env, "q", 5); resp.Error != "Interrupted" {
		t.Fatalf("%+v", resp)
	}
}

func TestXAIBackendWiring(t *testing.T) {
	pools := &fakePools{}
	usePools(t, pools)
	env := httpxProviderEnv(t, "XAI_API_KEY=xai-test-key\n", map[string]any{"web.keyless_fallback": false})
	if !isWebBackendAvailable(env, "xai") || len(pools.asked) != 0 {
		t.Fatalf("availability called the resolver %v", pools.asked)
	}
	if got := getWebBackend(env); got == "xai" {
		t.Fatal("stray XAI_API_KEY rerouted web search")
	}
	if !CheckWebAPIKey(httpxProviderEnv(t, "XAI_API_KEY=k\n", map[string]any{"web.backend": "xai"})) {
		t.Fatal("configured xai did not light the gate")
	}
	if CheckWebAPIKey(httpxProviderEnv(t, "", map[string]any{"web.backend": nil, "web.keyless_fallback": false})) {
		t.Fatal("null backend lit the gate")
	}
}

func TestXAITryParseAndCoercions(t *testing.T) {
	if xaiTryParseJSONResults(`{"results": "nope"}`, 5) != nil || xaiTryParseJSONResults("no json here", 5) != nil {
		t.Fatal("parsed garbage")
	}
	if got := xaiTryParseJSONResults(`{"results":[{"url":"https://1.example"},{"url":"https://2.example"}]}`, 1); len(got) != 1 {
		t.Fatalf("%v", got)
	}
	if f, err := coerceFloat("abc", 90); err != nil || f != 90 {
		t.Fatal(f, err)
	}
	if f, err := coerceFloat("12.5", 90); err != nil || f != 12.5 {
		t.Fatal(f, err)
	}
	if f, _ := coerceFloat(nil, 90); f != 90 {
		t.Fatal(f)
	}
}
