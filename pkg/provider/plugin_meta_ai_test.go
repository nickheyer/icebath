package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestMetaAIPluginProfileMatchesUpstreamRegistration(t *testing.T) {
	t.Setenv("META_BASE_URL", "")
	p := bundledPluginProfile(t, "meta-ai")
	assertProfileFields(t, p, map[string]any{"name": "meta-ai", "aliases": []string{"meta", "muse", "muse-spark", "model-api", "msl"},
		"display_name": "Meta Model API", "description": "Meta Muse Spark family (Meta Superintelligence Labs)",
		"signup_url": "https://developer.meta.com/ai/", "env_vars": []string{"MODEL_API_KEY", "META_API_KEY", "META_MODEL_API_KEY", "META_BASE_URL"},
		"base_url": "https://api.meta.ai/v1", "auth_type": "api_key", "api_mode": "codex_responses", "supports_vision": true,
		"supports_vision_tool_messages": false, "default_aux_model": "muse-spark-1.2-contributor", "default_max_tokens": 16384,
		"fallback_models": []string{"muse-spark-1.2"}, "base_url_env_var": ""})
	t.Setenv("META_BASE_URL", "  https://relay.example/v1 ")
	if got := metaAIPluginProfiles()[0].BaseURL; got != "https://relay.example/v1" {
		t.Fatalf("env base url = %q", got)
	}
}

func TestMetaAILiveCatalogFiltersNonChatModels(t *testing.T) {
	p := bundledPluginProfile(t, "meta-ai")
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data": [{"id": "muse-voice-transcribe-1.0"}, {"id": "muse-spark-latest"}, {"id": "muse-image-1.0-eval"}, {"id": "muse-nova-test"}]}`))
	}))
	defer srv.Close()
	got, ok := p.FetchModels(context.Background(), "k", srv.URL+"/v1", 5*time.Second)
	if !ok || !reflect.DeepEqual(got, []string{"muse-spark-latest", "muse-nova-test"}) || hits == 0 {
		t.Fatalf("models = %v %v hits %d", got, ok, hits)
	}
}

func TestMetaAIReasoningEffort(t *testing.T) {
	p := bundledPluginProfile(t, "meta-ai")
	effort := func(r *Reasoning) any {
		extra, top := p.BuildAPIKwargsExtras(r, map[string]any{})
		if len(extra) != 0 {
			t.Fatalf("extra body used = %v", extra)
		}
		return top["reasoning_effort"]
	}
	for _, c := range []struct {
		r    *Reasoning
		want string
	}{
		{nil, "medium"}, {&Reasoning{Enabled: bptr(true), Effort: "high"}, "high"}, {&Reasoning{Enabled: bptr(true), Effort: "low"}, "low"},
		{&Reasoning{Enabled: bptr(true), Effort: "max"}, "xhigh"}, {&Reasoning{Enabled: bptr(false)}, "minimal"}, {&Reasoning{Effort: "none"}, "minimal"},
		{&Reasoning{Enabled: bptr(true), Effort: "bespoke"}, "medium"},
	} {
		if got := effort(c.r); got != c.want {
			t.Fatalf("%+v = %v, want %s", c.r, got, c.want)
		}
	}
}
