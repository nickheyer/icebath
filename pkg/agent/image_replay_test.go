package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

var replayPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

func retainImage(t *testing.T, ctx context.Context, name string, raw []byte) string {
	t.Helper()
	root, err := NativeInputsRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	dir := filepath.Join(root, hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReplayFrontierMovesInWholeBatches(t *testing.T) {
	for _, c := range [][3]int{{0, 3, 0}, {3, 3, 0}, {4, 3, 0}, {5, 3, 0}, {6, 3, 3}, {8, 3, 3}, {9, 3, 6}} {
		if got := ReplayFrontier(c[0], c[1]); got != c[2] {
			t.Errorf("frontier(%d, %d) = %d, want %d", c[0], c[1], got, c[2])
		}
	}
}

func TestResolveReplayRecentImagesRejectsUnusableValues(t *testing.T) {
	cases := []struct {
		cfg  map[string]any
		want int
	}{
		{nil, 3}, {map[string]any{"vision": map[string]any{"replay_recent_images": nil}}, 3},
		{map[string]any{"vision": map[string]any{"replay_recent_images": 5}}, 5},
		{map[string]any{"vision": map[string]any{"replay_recent_images": -1}}, 0},
		{map[string]any{"vision": map[string]any{"replay_recent_images": true}}, 0},
		{map[string]any{"vision": map[string]any{"replay_recent_images": "2"}}, 0},
	}
	for _, c := range cases {
		if got := ResolveReplayRecentImages(c.cfg); got != c.want {
			t.Errorf("%v = %d, want %d", c.cfg, got, c.want)
		}
	}
}

func TestReplayRecentImagesRebuildsRetainedBytesInPlace(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ctx := home.WithOverride(context.Background(), h)
	a := &AIAgent{}
	kept := retainImage(t, ctx, "photo.png", replayPNG)
	tampered := retainImage(t, ctx, "changed.png", replayPNG[:20])
	if err := os.WriteFile(tampered, []byte("changed bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{
		{"role": "user", "content": "look\n[screenshot detail=low]\n[Image attached at: " + kept + "]"},
		{"role": "user", "content": "[screenshot]\n[Image attached at: " + tampered + "]"},
		{"role": "user", "content": "[screenshot]\n[Image attached: https://cdn.example/x.png]"},
		{"role": "user", "content": "typed [screenshot] marker\n[screenshot]"},
		{"role": "assistant", "content": "[screenshot]\n[Image attached at: " + kept + "]"},
	}
	if n := a.replayRecentImages(ctx, rows, 3); n != 1 {
		t.Fatalf("attached %d", n)
	}
	parts, ok := rows[0]["content"].([]any)
	if !ok || len(parts) != 3 {
		t.Fatalf("rebuilt %v", rows[0]["content"])
	}
	image := parts[1].(map[string]any)["image_url"].(map[string]any)
	if parts[0].(map[string]any)["text"] != "look" || image["detail"] != "low" || !strings.HasPrefix(image["url"].(string), "data:image/png;base64,") {
		t.Fatalf("parts %v", parts)
	}
	for i := 1; i < len(rows); i++ {
		if _, isText := rows[i]["content"].(string); !isText {
			t.Fatalf("row %d must keep its text, got %v", i, rows[i]["content"])
		}
	}
}

func TestReplayForAgentSkipsRequestsWithoutMarkers(t *testing.T) {
	a := &AIAgent{}
	rows := []map[string]any{{"role": "user", "content": "plain"}}
	replayForAgent(context.Background(), a, rows)
	if a.imageReplayWindow != nil {
		t.Fatal("a request without screenshot markers must not resolve the replay window")
	}
}
