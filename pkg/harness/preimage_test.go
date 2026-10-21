package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"
)

func TestPreImageCapturesTargetsBeforeTheyChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	os.WriteFile(path, []byte("before"), 0o644)
	p := &PreImage{Max: 2}
	ctx := context.Background()
	args, _ := json.Marshal(map[string]string{"file_path": path, "content": "after"})
	adv, err := p.BeforeTool(ctx, protocol.ToolCall{ID: "w1", Name: "Write", Args: args})
	if err != nil || adv.Call.ID != "w1" {
		t.Fatal(err)
	}
	img, ok := p.Get("w1")
	if !ok || img.Content != "before" || img.Missing || img.Path != path {
		t.Fatalf("%+v %v", img, ok)
	}
	missing, _ := json.Marshal(map[string]string{"file_path": filepath.Join(dir, "nope.txt")})
	p.BeforeTool(ctx, protocol.ToolCall{ID: "e1", Name: "Edit", Args: missing})
	if img, ok := p.Get("e1"); !ok || !img.Missing || img.Content != "" {
		t.Fatalf("%+v", img)
	}
	p.BeforeTool(ctx, protocol.ToolCall{ID: "r1", Name: "Read", Args: args})
	if _, ok := p.Get("r1"); ok {
		t.Fatal("read must not be captured")
	}
	p.BeforeTool(ctx, protocol.ToolCall{ID: "b1", Name: "Write", Args: json.RawMessage(`{bad`)})
	p.BeforeTool(ctx, protocol.ToolCall{ID: "b2", Name: "Write", Args: json.RawMessage(`{}`)})
	if _, ok := p.Get("b1"); ok {
		t.Fatal("bad args captured")
	}
	p.BeforeTool(ctx, protocol.ToolCall{ID: "w2", Name: "Write", Args: args})
	if _, ok := p.Get("w1"); ok {
		t.Fatal("oldest image must be evicted past the limit")
	}
	if _, ok := p.Get("w2"); !ok {
		t.Fatal("newest image missing")
	}
	small := &PreImage{MaxBytes: 2, Tools: []string{"Custom"}}
	small.BeforeTool(ctx, protocol.ToolCall{ID: "c1", Name: "Custom", Args: args})
	if img, ok := small.Get("c1"); !ok || img.Content != "" || img.Missing {
		t.Fatalf("oversize files must be recorded without content: %+v", img)
	}
	if _, ok := small.Get("w1"); ok {
		t.Fatal("custom tool list must replace the default")
	}
	if (&PreImage{}).limit() != DefaultPreImages || (&PreImage{}).maxBytes() != 8<<20 {
		t.Fatal("defaults")
	}
}
