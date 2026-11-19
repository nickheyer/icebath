package provider

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func ident(provider, model, baseURL string) BackendIdentity {
	return BuildBackendIdentity(provider, model, baseURL)
}

// First class predicate over a fixed provider set, distinct known labels only
func firstClassSet(names ...string) func(a, b BackendIdentity) bool {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return func(a, b BackendIdentity) bool {
		return a.Provider != "" && b.Provider != "" && a.Provider != b.Provider && set[a.Provider] && set[b.Provider]
	}
}

// Skip test of the live core with the first class predicate injected
func skipCandidate(candidate, failed BackendIdentity, scope FailureScope, firstClass func(a, b BackendIdentity) bool) bool {
	return shouldSkipCandidate(candidate, failed, scope, func(a, b BackendIdentity) bool { return sameDeployment(a, b, firstClass) })
}

func TestSameDeployment(t *testing.T) {
	none := firstClassSet()
	failed := ident("custom", "zai-org/glm-5.2", "")
	sibling := ident("custom", "mindai/macaron-v1-venti", "")
	if sameDeployment(sibling, failed, none) || skipCandidate(sibling, failed, ScopeModel, none) {
		t.Fatal("sibling models are independent deployments")
	}
	a := ident("custom", "qwen-3", "http://box1:1234/v1")
	b := ident("custom", "qwen-3", "http://box2:1234/v1")
	if sameDeployment(a, b, none) || skipCandidate(a, b, ScopeModel, none) {
		t.Fatal("same model on two explicit urls is a pool")
	}
	a = ident("claude-cli", "claude-opus-4.7", "http://127.0.0.1:7891/v1")
	b = ident("claude-cli-alt", "claude-opus-4.7", "http://127.0.0.1:7891/v1")
	if !sameDeployment(a, b, none) {
		t.Fatal("shim aliases at one url with one model are the same deployment")
	}
	xai := firstClassSet("xai", "xai-oauth")
	a = ident("xai-oauth", "grok-4.5", "https://api.x.ai/v1")
	b = ident("xai", "grok-4.5", "https://api.x.ai/v1")
	if sameDeployment(a, b, xai) || skipCandidate(a, b, ScopeModel, xai) {
		t.Fatal("first class pair on one host is not the same")
	}
}

func TestSameCredentialSurface(t *testing.T) {
	failed := ident("custom", "zai-org/glm-5.2", "")
	sibling := ident("custom", "mindai/macaron-v1-venti", "")
	if !SameCredentialSurface(sibling, failed) || !skipCandidate(sibling, failed, ScopeCredential, firstClassSet()) {
		t.Fatal("same provider label shares the credential")
	}
	xai := firstClassSet("xai", "xai-oauth")
	a := ident("xai", "grok-4.5", "https://api.x.ai/v1")
	b := ident("xai-oauth", "grok-4.5", "https://api.x.ai/v1")
	if SameCredentialSurface(a, b) || skipCandidate(a, b, ScopeCredential, xai) {
		t.Fatal("distinct first class providers differ")
	}
	if SameCredentialSurface(ident("proxy-a", "m", "http://gw:9000/v1"), ident("proxy-b", "m", "http://gw:9000/v1")) {
		t.Fatal("distinct custom labels on one url are not assumed shared")
	}
}

func TestSameEndpoint(t *testing.T) {
	none := firstClassSet()
	a := ident("a", "m1", "http://host:8000/v1/")
	b := ident("b", "m2", "http://HOST:8000/v1")
	if !SameEndpoint(a, b) || !skipCandidate(a, b, ScopeEndpoint, none) {
		t.Fatal("trailing slash and host case are the same endpoint")
	}
	failed := ident("custom", "m", "https://Example.test/API/v1?token=AbC")
	if skipCandidate(ident("custom", "m", "https://example.test/api/v1?token=AbC"), failed, ScopeEndpoint, none) {
		t.Fatal("path case distinguishes endpoints")
	}
	if skipCandidate(ident("custom", "m", "https://example.test/API/v1?token=abc"), failed, ScopeEndpoint, none) {
		t.Fatal("query case distinguishes endpoints")
	}
	if SameEndpoint(ident("a", "m", "http://h1/v1"), ident("a2", "m", "http://h2/v1")) {
		t.Fatal("different urls differ")
	}
	if !SameEndpoint(ident("openrouter", "m1", ""), ident("openrouter", "m2", "")) || SameEndpoint(ident("openrouter", "m", ""), ident("deepseek", "m", "")) {
		t.Fatal("unknown url falls back to the provider default")
	}
}

func TestUnknownAxesNeverStrand(t *testing.T) {
	none := firstClassSet()
	failed := ident("custom", "glm", "http://h/v1")
	for _, scope := range []FailureScope{ScopeModel, ScopeCredential, ScopeEndpoint} {
		if skipCandidate(ident("", "", ""), failed, scope, none) {
			t.Fatalf("empty candidate skipped under %s", scope)
		}
	}
	if skipCandidate(ident("custom", "some-model", ""), ident("custom", "", ""), ScopeModel, none) {
		t.Fatal("model scope requires model evidence")
	}
}

func writeConfig(t *testing.T, h string, doc map[string]any) {
	t.Helper()
	if err := config.SaveUser(context.Background(), h, doc); err != nil {
		t.Fatal(err)
	}
}

func readConfig(t *testing.T, h string) map[string]any {
	t.Helper()
	doc, err := config.ReadUser(h)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestFallbackReasonTextNamesTheFailure(t *testing.T) {
	if FallbackReasonText(ReasonBilling) != "billing or quota exhausted" || FallbackReasonText("custom_thing") != "custom thing" || FallbackReasonText("") != "provider failure" {
		t.Fatal("reason text")
	}
}
