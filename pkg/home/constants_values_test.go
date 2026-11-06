package home

import "testing"

func TestOpenRouterVariantBase(t *testing.T) {
	for in, want := range map[string]string{
		"x-ai/grok-4:nitro": "x-ai/grok-4", "x-ai/grok-4:NITRO": "x-ai/grok-4", "x-ai/grok-4:free": "",
		"x-ai/grok-4": "", ":nitro": "", "": "", "a:b:online": "a:b", "x:NİTRO": "",
	} {
		if got := OpenRouterVariantBase(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	if OpenRouterModelsURL != "https://openrouter.ai/api/v1/models" || DefaultIndicatorStyle != "kaomoji" || WebappAttachmentMaxBytes != 16<<20 {
		t.Fatal("constants")
	}
}
