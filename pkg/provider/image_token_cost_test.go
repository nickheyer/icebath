package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

func imageCostMsg() *state.Message {
	return &state.Message{Role: "user", Content: []any{map[string]any{"type": "text", "text": "look"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + strings.Repeat("A", 4000)}}}}
}

// Points the cache at a fresh home and drops every loaded table
func isolateImageCost(t *testing.T) string {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	reset := func() {
		imageCostTables.mu.Lock()
		imageCostTables.launch, imageCostTables.byHome = nil, nil
		imageCostTables.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
	return dir
}

func TestImageCostResidualTeachesPerImageCost(t *testing.T) {
	isolateImageCost(t)
	ctx := context.Background()
	history := []*state.Message{anchorMsg("user", "start"), anchorMsg("assistant", "ok")}
	anchor := CaptureUsageAnchor(10_000, 5, history)
	history = append(history, imageCostMsg(), anchorMsg("assistant", "looking"), imageCostMsg())
	zero := 0
	textOnly, ok := AnchoredContextTokens(ImageCostContext(ctx, &zero), history, anchor, true)
	if !ok {
		t.Fatal("anchor must match")
	}
	scoped := ImageCostContext(ctx, nil)
	scoped, learned, ok := CalibrateImageTokenCost(scoped, "vision-local", "http://127.0.0.1:8080/v1", anchor, history, textOnly+2*4_000)
	if !ok || learned != 4_000 || CurrentImageTokenCost(scoped) != 4_000 {
		t.Fatalf("learned %d %v current %d", learned, ok, CurrentImageTokenCost(scoped))
	}
	if got := EstimateMessagesTokens([]*state.Message{imageCostMsg()}, CurrentImageTokenCost(scoped), true); got < 4_000 {
		t.Fatalf("estimate %d", got)
	}
	if CurrentImageTokenCost(ctx) != DefaultImageTokenCost {
		t.Fatal("the scope leaked into the parent context")
	}
	imageCostTables.mu.Lock()
	imageCostTables.launch = nil
	imageCostTables.mu.Unlock()
	if got := LearnedImageTokenCost(ctx, "vision-local", "http://127.0.0.1:8080/v1"); got != 4_000 {
		t.Fatalf("persisted %d", got)
	}
	if got := LearnedImageTokenCost(ctx, "other-model", "http://127.0.0.1:8080/v1"); got != DefaultImageTokenCost {
		t.Fatalf("other model %d", got)
	}
	raw, _ := os.ReadFile(imageCostCachePath(ctx))
	if string(raw) != "{\n\"vision-local@127.0.0.1\":4000\n}" {
		t.Fatalf("cache file %q", raw)
	}
	_, again, ok := CalibrateImageTokenCost(scoped, "vision-local", "http://127.0.0.1:8080/v1", anchor, history, textOnly+2*3_001)
	if !ok || again != 3_500 {
		t.Fatalf("ema %d %v", again, ok)
	}
}

func TestImageCostNothingLearnedWithoutImagesOrAnchor(t *testing.T) {
	isolateImageCost(t)
	ctx := context.Background()
	history := []*state.Message{anchorMsg("user", "start"), anchorMsg("assistant", "ok")}
	anchor := CaptureUsageAnchor(10_000, 5, history)
	history = append(history, anchorMsg("user", "no image here"))
	if _, _, ok := CalibrateImageTokenCost(ctx, "vision-local", "http://127.0.0.1:8080/v1", anchor, history, 50_000); ok {
		t.Fatal("text only delta taught a price")
	}
	history = append(history, imageCostMsg())
	if _, _, ok := CalibrateImageTokenCost(ctx, "vision-local", "http://127.0.0.1:8080/v1", nil, history, 50_000); ok {
		t.Fatal("missing anchor taught a price")
	}
	if _, _, ok := CalibrateImageTokenCost(ctx, "vision-local", "http://127.0.0.1:8080/v1", anchor, history, 10_001); ok {
		t.Fatal("implausible residual taught a price")
	}
	if _, _, ok := CalibrateImageTokenCost(ctx, "vision-local", "http://127.0.0.1:8080/v1", anchor, history, "junk"); ok {
		t.Fatal("unparseable prompt tokens taught a price")
	}
	imageCostTables.mu.Lock()
	defer imageCostTables.mu.Unlock()
	if imageCostTables.launch != nil && imageCostTables.launch.Len() != 0 {
		t.Fatalf("table %v", imageCostTables.launch.Keys)
	}
}

func TestImageCostHomeKeyedTablesServeEachProfile(t *testing.T) {
	isolateImageCost(t)
	a, b := t.TempDir(), t.TempDir()
	for dir, body := range map[string]string{a: `{"m@gw.example": 1000, "bad": 1.5e3, "low": 10}`, b: `{"m@gw.example": 3000}`} {
		if err := os.MkdirAll(filepath.Join(dir, "cache"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home.CacheDir(dir), "image_token_costs.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctxA, ctxB := home.WithOverride(context.Background(), a), home.WithOverride(context.Background(), b)
	if got := LearnedImageTokenCost(ctxA, "m", "http://gw.example/v1"); got != 1000 {
		t.Fatalf("home a %d", got)
	}
	if got := LearnedImageTokenCost(ctxB, "m", "http://gw.example/v1"); got != 3000 {
		t.Fatalf("home b %d", got)
	}
	if got := LearnedImageTokenCost(ctxA, "bad", ""); got != DefaultImageTokenCost {
		t.Fatalf("float price admitted %d", got)
	}
	if got := LearnedImageTokenCost(context.Background(), "m", "http://gw.example/v1"); got != DefaultImageTokenCost {
		t.Fatalf("launch table %d", got)
	}
}

func TestBindImageTokenCostSetsInPlace(t *testing.T) {
	isolateImageCost(t)
	scoped := ImageCostContext(context.Background(), nil)
	if CurrentImageTokenCost(scoped) != DefaultImageTokenCost {
		t.Fatal("nil scope must read the default")
	}
	if BindImageTokenCost(scoped, "m", "http://h/v1") != scoped {
		t.Fatal("bind inside a scope must set in place")
	}
	if CountImages([]*state.Message{imageCostMsg(), nil, anchorMsg("user", "x"), imageCostMsg()}) != 2 {
		t.Fatal("count images")
	}
}
