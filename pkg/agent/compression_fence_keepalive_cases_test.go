package agent

import (
	"context"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Chat completion chunk carrying one delta
func fenceChunk(delta map[string]any) map[string]any {
	return map[string]any{"id": "c1", "model": "m1", "choices": []any{map[string]any{"delta": delta, "finish_reason": nil}}, "usage": nil}
}

func TestContentBearingProgressKeepaliveChunksDoNotResetTheCompressionFence(t *testing.T) {
	fence := NewCompressionCommitFence()
	time.Sleep(20 * time.Millisecond)
	ctx := provider.AuxProgressHook(context.Background(), func() error { fence.TouchProgress(); return nil })
	acc := provider.NewAuxChatStreamAccumulator(ctx, "", nil)
	keepalive := map[string]any{"id": nil, "model": nil, "choices": []any{}, "usage": nil}
	emptyRole := fenceChunk(map[string]any{"content": "", "reasoning": ""})
	for i := 0; i < 5; i++ {
		for _, chunk := range []map[string]any{keepalive, emptyRole} {
			if _, err := acc.Feed(chunk); err != nil {
				t.Fatal(err)
			}
		}
	}
	if fence.SecondsSinceProgress() <= 0 {
		t.Fatal("content-free frames must not refresh the fence")
	}
	if _, err := acc.Feed(fenceChunk(map[string]any{"content": "token"})); err != nil {
		t.Fatal(err)
	}
	if since := fence.SecondsSinceProgress(); since >= 0.05 {
		t.Fatalf("content chunk left the fence stale for %v", since)
	}
}
