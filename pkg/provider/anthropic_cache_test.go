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
