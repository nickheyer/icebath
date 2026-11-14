package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func clearActualEnv(t *testing.T) {
	for _, k := range []string{"ACTUAL_API_KEY", "ACTUAL_BASE_URL", "ICEBATH_CA_BUNDLE", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"} {
		t.Setenv(k, "")
	}
}

func TestActualPluginProfileMatchesUpstreamRegistration(t *testing.T) {
	clearActualEnv(t)
	bundledPluginProfile(t, "actual")
	p, ok := GetProviderProfile(context.Background(), "actual-computer")
	if !ok {
		t.Fatal("alias actual-computer unresolved")
	}
	assertProfileFields(t, p, map[string]any{
		"name": "actual", "aliases": []string{"actual-computer", "actualcomputer", "aci"}, "display_name": "Actual Computer",
		"description": "Actual Computer - hosted inference via api.actual.inc, or local offline inference via model.base_url in config.yaml",
		"signup_url":  "https://actual.inc", "env_vars": []string{"ACTUAL_API_KEY"}, "base_url": "https://api.actual.inc/v1",
		"auth_type": "api_key", "api_mode": "chat_completions", "base_url_env_var": "",
	})
}

func TestActualProfileTranslatesExplicitReasoningControls(t *testing.T) {
	p := bundledPluginProfile(t, "actual")
	eb, tl := p.BuildAPIKwargsExtras(nil, map[string]any{})
	assertExtras(t, "nil", eb, tl, map[string]any{}, map[string]any{})
	if got, ok, err := p.SupportedReasoningEfforts(context.Background(), "zai-org/GLM-5.3"); err != nil || !ok || !reflect.DeepEqual(got, []string{"none", "low", "medium", "high", "max"}) {
		t.Fatalf("efforts %v %v", got, ok)
	}
	for _, c := range []struct {
		r              *Reasoning
		toggle, effort string
	}{
		{effortCfg("high"), "enabled", "high"}, {effortCfg("xhigh"), "enabled", "high"}, {effortCfg("ultra"), "enabled", "max"},
		{&Reasoning{Enabled: bptr(false), Effort: "high"}, "disabled", ""}, {effortCfg("none"), "disabled", "none"},
	} {
		eb, tl := p.BuildAPIKwargsExtras(c.r, map[string]any{"model": "zai-org/GLM-5.3"})
		if !reflect.DeepEqual(eb, map[string]any{"thinking": map[string]any{"type": c.toggle}}) {
			t.Fatalf("%s extra body %#v", c.r.Effort, eb)
		}
		if got, ok := tl["reasoning_effort"]; c.effort == "" && ok || c.effort != "" && got != c.effort {
			t.Fatalf("%s top level %#v", c.r.Effort, tl)
		}
	}
}

func TestActualProfileFetchModelsNormalizesEnvBaseURL(t *testing.T) {
	clearActualEnv(t)
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data": [{"id": "actual/local-model"}]}`))
	}))
	defer srv.Close()
	p := bundledPluginProfile(t, "actual")
	t.Setenv("ACTUAL_BASE_URL", srv.URL)
	got, ok := p.FetchModels(context.Background(), "", "", 0)
	if !ok || !reflect.DeepEqual(got, []string{"actual/local-model"}) {
		t.Fatalf("models %v %v", got, ok)
	}
	if want := mustURLPath(t, normalizeActualRuntimeBaseURL(srv.URL)) + "/models"; path != want || auth != "" {
		t.Fatalf("path %s want %s auth %q", path, want, auth)
	}
}

func mustURLPath(t *testing.T, raw string) string {
	t.Helper()
	u, err := parseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Path
}

func TestActualProfileFetchModelsSendsCredentialOnlyToOriginalOrigin(t *testing.T) {
	clearActualEnv(t)
	var targetAuth []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetAuth = append(targetAuth, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"data": [{"id": "redirected"}]}`))
	}))
	defer target.Close()
	var sourceAuth []string
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceAuth = append(sourceAuth, r.Header.Get("Authorization"))
		http.Redirect(w, r, target.URL+"/v1/models", http.StatusFound)
	}))
	defer source.Close()
	p := bundledPluginProfile(t, "actual")
	got, ok := p.FetchModels(context.Background(), "secret-key", source.URL+"/v1", 0)
	if !ok || !reflect.DeepEqual(got, []string{"redirected"}) {
		t.Fatalf("models %v %v", got, ok)
	}
	if !reflect.DeepEqual(sourceAuth, []string{"Bearer secret-key"}) || !reflect.DeepEqual(targetAuth, []string{""}) {
		t.Fatalf("source auth %v target auth %v", sourceAuth, targetAuth)
	}
}

func TestActualHostedClientUsesTheCABundleOnMacOS(t *testing.T) {
	clearActualEnv(t)
	saved := actualGOOS
	actualGOOS = "darwin"
	t.Cleanup(func() { actualGOOS = saved })
	p := bundledPluginProfile(t, "actual")
	want := map[string]any{}
	if bundle := DefaultCABundlePath(); bundle != "" {
		want["ssl_ca_cert"] = bundle
	}
	if got := p.BuildClientKwargsExtras(map[string]any{"base_url": "https://api.actual.inc/v1"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("hosted %v want %v", got, want)
	}
	if got := p.BuildClientKwargsExtras(map[string]any{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("profile base %v want %v", got, want)
	}
	if got := p.BuildClientKwargsExtras(map[string]any{"base_url": "http://127.0.0.1:8080/v1"}); len(got) != 0 {
		t.Fatalf("local %v", got)
	}
	t.Setenv("REQUESTS_CA_BUNDLE", "/etc/custom.pem")
	if got := p.BuildClientKwargsExtras(map[string]any{"base_url": "https://api.actual.inc/v1"}); len(got) != 0 {
		t.Fatalf("explicit CA override %v", got)
	}
	actualGOOS = "linux"
	t.Setenv("REQUESTS_CA_BUNDLE", "")
	if got := p.BuildClientKwargsExtras(map[string]any{"base_url": "https://api.actual.inc/v1"}); len(got) != 0 {
		t.Fatalf("non macOS %v", got)
	}
}
