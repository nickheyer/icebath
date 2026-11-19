package provider

import (
	"reflect"
	"testing"
)

func TestApplyAnthropicCacheControlWrapsUnexported(t *testing.T) {
	build := func() []map[string]any {
		return []map[string]any{{"role": "system", "content": "sys"}, {"role": "user", "content": "hi"}}
	}
	got := ApplyAnthropicCacheControl(build(), "5m", true, "", true)
	want := applyAnthropicCacheControl(build(), "5m", true, "", true)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// Wire visible cache markers in a request local plan
func countCacheMarkers(messages []map[string]any, tools []map[string]any) int {
	n := 0
	for _, m := range messages {
		if _, has := m["cache_control"]; has {
			n++
		}
		if list, ok := asList(m["content"]); ok {
			for _, p := range list {
				if pm, ok := asMap(p); ok {
					if _, has := pm["cache_control"]; has {
						n++
					}
				}
			}
		}
	}
	for _, t := range tools {
		if _, has := t["cache_control"]; has {
			n++
		}
	}
	return n
}
