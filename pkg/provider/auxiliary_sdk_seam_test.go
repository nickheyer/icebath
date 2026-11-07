package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestAuxiliaryCompletionPathHandsTheSDKOnlyThePlaceholder(t *testing.T) {
	var seen []map[string]any
	prev := auxSDKBuildRequestFn
	auxSDKBuildRequestFn = func(c *auxOpenAIClient, kwargs map[string]any) (*auxSDKRequest, error) {
		seen = append(seen, kwargs)
		return prev(c, kwargs)
	}
	t.Cleanup(func() { auxSDKBuildRequestFn = prev })
	srv, _ := auxSDKServer(t, auxSDKJSON(200, map[string]any{"id": "c", "object": "chat.completion", "created": 1, "model": "m",
		"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "ok"}}}}, nil))
	client := newAuxOpenAIClient("k", srv.URL, auxOpenAIOptions{MaxRetries: auxSDKRetries(0)})
	messages := []any{map[string]any{"role": "user", "content": strings.Repeat("x", 4096)}}
	body := map[string]any{"model": "m", "messages": messages, "max_tokens": 8}
	if _, err := auxRelaySyncCompletion(context.Background(), client, body, "", "", nil); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("sdk create calls %d", len(seen))
	}
	if got, _ := auxListValue(seen[0]["messages"]); len(got) != 0 {
		t.Fatalf("typed messages %v", seen[0]["messages"])
	}
	extra, _ := seen[0]["extra_body"].(map[string]any)
	if !reflect.DeepEqual(auxMessageList(extra["messages"]), auxMessageList(messages)) {
		t.Fatalf("extra_body messages %v", extra["messages"])
	}
}
