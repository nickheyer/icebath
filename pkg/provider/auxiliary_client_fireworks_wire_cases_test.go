package provider

import (
	"reflect"
	"testing"
)

func TestFireworksReasoningAuxiliaryReasoningWireShape(t *testing.T) {
	registeredProfile(t, "fireworks")
	ctx := auxMainTestHome(t, "")
	cases := []struct {
		provider      string
		reasoning     map[string]any
		expectTop     map[string]any
		expectGeneric bool
	}{
		{"fireworks", map[string]any{"enabled": false}, map[string]any{"reasoning_effort": "none"}, false},
		{"fireworks", map[string]any{"enabled": true, "effort": "low"}, map[string]any{"reasoning_effort": "low"}, false},
		{"unregistered-gateway", map[string]any{"enabled": false}, map[string]any{}, true},
	}
	for _, tc := range cases {
		kwargs, err := auxBuildCallKwargs(ctx, tc.provider, "accounts/fireworks/models/glm-5p2",
			[]any{map[string]any{"role": "user", "content": "Generate a title"}},
			auxCallKwargsOptions{ReasoningConfig: tc.reasoning, BaseURL: "https://api.fireworks.ai/inference/v1", Task: "title_generation"})
		if err != nil {
			t.Fatal(err)
		}
		top := map[string]any{}
		if v, ok := kwargs["reasoning_effort"]; ok {
			top["reasoning_effort"] = v
		}
		if !reflect.DeepEqual(top, tc.expectTop) {
			t.Fatalf("%s top level %v, want %v", tc.provider, top, tc.expectTop)
		}
		extra, _ := kwargs["extra_body"].(map[string]any)
		if _, generic := extra["reasoning"]; generic != tc.expectGeneric {
			t.Fatalf("%s extra_body %v generic %v", tc.provider, extra, generic)
		}
	}
}
