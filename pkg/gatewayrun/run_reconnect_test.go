package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Home under root whose config sets the reconnect attention threshold
func homeWithThreshold(t *testing.T, root, name, seconds string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("agent:\n  reconnect_attention_after: "+seconds+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestThresholdFollowsBoundProfileScopeABA(t *testing.T) {
	root := t.TempDir()
	homeA := homeWithThreshold(t, root, "a", "10")
	homeB := homeWithThreshold(t, root, "b", "100000")
	t.Setenv("ICEBATH_HOME", homeA)
	ctx := context.Background()
	now := pythread.Monotonic()
	queued := func() map[string]any { return map[string]any{"queued_at": now - 20} }
	if !reconnectNeedsAttention(ctx, queued(), now) {
		t.Fatal("launch profile threshold 10s should escalate a 20s queue")
	}
	if reconnectNeedsAttention(gateway.ProfileRuntimeScope(ctx, homeB, false), queued(), now) {
		t.Fatal("profile b threshold should not escalate")
	}
	if !reconnectNeedsAttention(ctx, queued(), now) {
		t.Fatal("back in profile a the threshold is 10s again")
	}
	if err := os.WriteFile(filepath.Join(homeA, "config.yaml"), []byte("agent:\n  reconnect_attention_after: 100000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if reconnectNeedsAttention(ctx, queued(), now) {
		t.Fatal("a live config edit takes effect on the next call")
	}
}
