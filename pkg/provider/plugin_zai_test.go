package provider

import (
	"reflect"
	"testing"
)

var (
	zaiThinkingOn  = map[string]any{"thinking": map[string]any{"type": "enabled", "clear_thinking": false}}
	zaiThinkingOff = map[string]any{"thinking": map[string]any{"type": "disabled"}}
)

func zaiExtras(t *testing.T, p Profile, r *Reasoning, model string) (map[string]any, map[string]any) {
	t.Helper()
	return p.BuildAPIKwargsExtras(r, map[string]any{"model": model})
}

func zaiEffort(effort string) *Reasoning { return &Reasoning{Enabled: bptr(true), Effort: effort} }

func TestZaiPluginProfileMatchesUpstreamRegistration(t *testing.T) {
	p := bundledPluginProfile(t, "zai")
	assertProfileFields(t, p, map[string]any{"name": "zai", "aliases": []string{"glm", "z-ai", "z.ai", "zhipu"},
		"env_vars": []string{"GLM_API_KEY", "ZAI_API_KEY", "Z_AI_API_KEY"}, "display_name": "Z.AI (GLM)",
		"description": "Z.AI / GLM — Zhipu AI models", "signup_url": "https://z.ai/",
		"fallback_models": []string{"glm-5.2", "glm-5", "glm-4-9b"}, "base_url": "https://api.z.ai/api/paas/v4",
		"default_aux_model": "glm-4.5-flash", "api_mode": "chat_completions", "base_url_env_var": ""})
}

func TestZaiThinkingWireShape(t *testing.T) {
	p := bundledPluginProfile(t, "zai")
	for _, c := range []struct {
		r          *Reasoning
		extra, top map[string]any
	}{
		{nil, map[string]any{}, map[string]any{}},
		{zaiEffort("medium"), zaiThinkingOn, map[string]any{}},
		{&Reasoning{Enabled: bptr(false)}, zaiThinkingOff, map[string]any{}},
	} {
		extra, top := zaiExtras(t, p, c.r, "glm-5")
		if !reflect.DeepEqual(extra, c.extra) || !reflect.DeepEqual(top, c.top) {
			t.Fatalf("%+v: %v %v", c.r, extra, top)
		}
	}
}

func TestZaiGLM52ReasoningEffort(t *testing.T) {
	p := bundledPluginProfile(t, "zai")
	for effort, want := range map[string]string{"high": "high", "low": "high", "medium": "high", "minimal": "high", "xhigh": "max", "max": "max"} {
		extra, top := zaiExtras(t, p, zaiEffort(effort), "glm-5.2")
		if !reflect.DeepEqual(extra, zaiThinkingOn) || !reflect.DeepEqual(top, map[string]any{"reasoning_effort": want}) {
			t.Fatalf("%s: %v %v", effort, extra, top)
		}
	}
	extra, top := zaiExtras(t, p, &Reasoning{Enabled: bptr(false), Effort: "high"}, "glm-5.2")
	if !reflect.DeepEqual(extra, zaiThinkingOff) || len(top) != 0 {
		t.Fatalf("disabled: %v %v", extra, top)
	}
	for _, model := range []string{"z-ai/glm-5.2", "glm-5-2", "glm-5p2", "accounts/fireworks/models/glm-5p2", "zai-org-glm-5-2"} {
		if _, top := zaiExtras(t, p, zaiEffort("max"), model); !reflect.DeepEqual(top, map[string]any{"reasoning_effort": "max"}) {
			t.Fatalf("%s: %v", model, top)
		}
	}
	for _, model := range []string{"glm-5.1", "glm-5", "glm-4.7", "glm-4-9b", ""} {
		if _, top := zaiExtras(t, p, zaiEffort("high"), model); len(top) != 0 {
			t.Fatalf("%s: %v", model, top)
		}
	}
	if _, top := p.BuildAPIKwargsExtras(zaiEffort("high"), map[string]any{"model": nil}); len(top) != 0 {
		t.Fatalf("None model: %v", top)
	}
}

func TestZaiGLM53ReasoningEffort(t *testing.T) {
	p := bundledPluginProfile(t, "zai")
	for effort, want := range map[string]string{"low": "low", "medium": "medium", "high": "high", "max": "max", "xhigh": "max", "minimal": "low"} {
		extra, top := zaiExtras(t, p, zaiEffort(effort), "glm-5.3")
		if !reflect.DeepEqual(extra, zaiThinkingOn) || !reflect.DeepEqual(top, map[string]any{"reasoning_effort": want}) {
			t.Fatalf("%s: %v %v", effort, extra, top)
		}
	}
	for _, model := range []string{"z-ai/glm-5.3", "glm-5-3", "glm-5p3", "zai-org-glm-5-3"} {
		if _, top := zaiExtras(t, p, zaiEffort("low"), model); !reflect.DeepEqual(top, map[string]any{"reasoning_effort": "low"}) {
			t.Fatalf("%s: %v", model, top)
		}
	}
	if _, top := zaiExtras(t, p, zaiEffort("low"), "glm-5.2"); !reflect.DeepEqual(top, map[string]any{"reasoning_effort": "high"}) {
		t.Fatalf("glm-5.2 low: %v", top)
	}
	for _, model := range []string{"glm-5.3", "glm-5.3-flash", "z-ai/glm-5.3", "z-ai/glm-5.3-flash", "glm-5-3", "glm-5p3"} {
		extra, top := zaiExtras(t, p, &Reasoning{Enabled: bptr(false)}, model)
		if !reflect.DeepEqual(extra, zaiThinkingOn) || !reflect.DeepEqual(top, map[string]any{"reasoning_effort": "low"}) {
			t.Fatalf("%s: %v %v", model, extra, top)
		}
	}
}

func TestZaiModelGating(t *testing.T) {
	p := bundledPluginProfile(t, "zai")
	for _, model := range []string{"glm-4.5", "glm-4.5-air", "glm-4.5-flash", "glm-4.6", "glm-5", "glm-5.2", "GLM-5", "glm-٥"} {
		if extra, _ := zaiExtras(t, p, &Reasoning{Enabled: bptr(false)}, model); !reflect.DeepEqual(extra, zaiThinkingOff) {
			t.Fatalf("%s: %v", model, extra)
		}
	}
	for _, model := range []string{"glm-4", "glm-4.4", "glm-4-9b", "gpt-4o", "glm-٤.٤", "glm-\U0001D7D2"} {
		if extra, _ := zaiExtras(t, p, &Reasoning{Enabled: bptr(false)}, model); len(extra) != 0 {
			t.Fatalf("%s: %v", model, extra)
		}
	}
}
