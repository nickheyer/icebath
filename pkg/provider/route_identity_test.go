package provider

import "testing"

func TestRouteURLNormalizationPreservesPathSlashBeforeQuery(t *testing.T) {
	if NormalizeRouteBaseURL("https://example.com/v1/?tenant=large") == NormalizeRouteBaseURL("https://example.com/v1?tenant=large") {
		t.Fatal("path slash before a query must change the route identity")
	}
}

func TestNormalizeRouteBaseURL(t *testing.T) {
	cases := map[string]string{
		"":                                    "",
		"HTTPS://Example.COM:443/v1/":         "https://example.com/v1",
		"http://example.com:80/v1":            "http://example.com/v1",
		"http://example.com:8080/v1/":         "http://example.com:8080/v1",
		"https://user:pw@Example.com/v1":      "https://user:pw@example.com/v1",
		"https://[::1]:8000/v1":               "https://[::1]:8000/v1",
		"https://example.com/v1?":             "https://example.com/v1?",
		"https://example.com/v1#frag":         "https://example.com/v1",
		"not a url":                           "not a url",
		"localhost:8080":                      "localhost:8080",
		"https://example.com/v1?tenant=large": "https://example.com/v1?tenant=large",
	}
	for raw, want := range cases {
		if got := NormalizeRouteBaseURL(raw); got != want {
			t.Errorf("%q: got %q want %q", raw, got, want)
		}
	}
}

func TestProviderOwnsRoute(t *testing.T) {
	if owned, known := ProviderOwnsRoute("deepseek", "https://api.deepseek.com/v1"); !owned || !known {
		t.Fatalf("own endpoint: %v %v", owned, known)
	}
	if owned, known := ProviderOwnsRoute("custom", "http://10.0.0.5:8000/v1"); !owned || !known {
		t.Fatalf("custom owns any url: %v %v", owned, known)
	}
	if owned, known := ProviderOwnsRoute("local", "http://10.0.0.5:8000/v1"); !owned || !known {
		t.Fatalf("local owns any url: %v %v", owned, known)
	}
	if _, known := ProviderOwnsRoute("deepseek", ""); known {
		t.Fatal("empty url is unknown")
	}
	cfg := map[string]any{"providers": map[string]any{"mygw": map[string]any{"api": "https://gw.example/v1"}}}
	if owned, known := providerOwnsRouteIn("mygw", "https://gw.example/v1", cfg); !owned || !known {
		t.Fatalf("user entry own url: %v %v", owned, known)
	}
	if owned, known := providerOwnsRouteIn("mygw", "https://other.example/v1", cfg); owned || !known {
		t.Fatalf("user entry other url: %v %v", owned, known)
	}
	if owned, known := ProviderOwnsRoute("never-heard-of", "https://somewhere.example/v1"); !owned || !known {
		t.Fatalf("unknown provider and no entry is true: %v %v", owned, known)
	}
	if owned, known := ProviderOwnsRoute("anthropic", "https://api.deepseek.com/v1"); owned || !known {
		t.Fatalf("another provider's stock endpoint: %v %v", owned, known)
	}
	if _, known := ProviderOwnsRoute("anthropic", "https://proxy.lan/v1"); known {
		t.Fatal("unknown host is None")
	}
}

func TestDropStaleModelRoute(t *testing.T) {
	cfg := map[string]any{"base_url": "https://api.deepseek.com/v1", "api_mode": "chat_completions"}
	popped, unverified := DropStaleModelRoute(cfg, "deepseek", nil)
	if len(popped) != 0 || unverified {
		t.Fatalf("owned route kept: %v %v", popped, unverified)
	}
	cfg = map[string]any{"base_url": "https://proxy.lan/v1", "api_mode": "chat_completions"}
	popped, unverified = DropStaleModelRoute(cfg, "anthropic", nil)
	if len(popped) != 0 || !unverified {
		t.Fatalf("unknown route is unverified: %v %v", popped, unverified)
	}
	cfg = map[string]any{"api_mode": "codex_responses", "default": "m"}
	popped, unverified = DropStaleModelRoute(cfg, "anthropic", nil)
	if unverified || popped["api_mode"] != "codex_responses" {
		t.Fatalf("api mode alone goes: %v %v", popped, unverified)
	}
	if _, has := cfg["api_mode"]; has {
		t.Fatal("api_mode not popped")
	}
}

func TestContextRouteMismatch(t *testing.T) {
	if !ContextRouteMismatch("https://a.example/v1", "https://b.example/v1", "x", "x") {
		t.Fatal("different urls mismatch")
	}
	if ContextRouteMismatch("https://A.example/v1/", "https://a.example/v1", "x", "y") {
		t.Fatal("same normalized url matches")
	}
	if ContextRouteMismatch("", "", "", "x") {
		t.Fatal("no configured provider never mismatches")
	}
	if ContextRouteMismatch("", "https://api.deepseek.com/v1", "deepseek", "deepseek") {
		t.Fatal("default route of the provider matches")
	}
	if !ContextRouteMismatch("", "https://relay.example/v1", "deepseek", "deepseek") {
		t.Fatal("foreign active route mismatches a catalog provider")
	}
	if ContextRouteMismatch("", "https://relay.example/v1", "custom:mine", "custom:mine") {
		t.Fatal("named provider without defaults matches on identity")
	}
	if !ContextRouteMismatch("", "", "deepseek", "anthropic") {
		t.Fatal("different providers mismatch")
	}
	if ContextRouteMismatch("", "https://generativelanguage.googleapis.com/v1beta/openai", "gemini", "gemini") {
		t.Fatal("gemini openai compatible route is a default route")
	}
}

func TestShouldClearContextPin(t *testing.T) {
	if !ShouldClearContextPin("m1", "m2", "", "", "", "") {
		t.Fatal("model change clears")
	}
	if ShouldClearContextPin("m1", "m1", "https://a.example/v1", "https://a.example/v1", "x", "x") {
		t.Fatal("same route keeps the pin")
	}
}
