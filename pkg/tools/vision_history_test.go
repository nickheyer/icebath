package tools

import (
	"container/list"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Clears the process wide embed counters around a test
func freshRepeatCounts(t *testing.T) {
	t.Helper()
	clear := func() {
		repeatCounts.Lock()
		repeatCounts.order.Init()
		repeatCounts.index = map[repeatKey]*list.Element{}
		repeatCounts.count = map[repeatKey]int{}
		repeatCounts.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// PNG fixture, solid or seeded noise
func writeFixturePNG(t *testing.T, path string, w, h int, noisy bool) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewSource(7))
	for i := 0; i < len(img.Pix); i += 4 {
		if noisy {
			img.Pix[i], img.Pix[i+1], img.Pix[i+2] = uint8(rnd.Intn(256)), uint8(rnd.Intn(256)), uint8(rnd.Intn(256))
		} else {
			img.Pix[i], img.Pix[i+1], img.Pix[i+2] = 200, 40, 40
		}
		img.Pix[i+3] = 255
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	return path
}

// Decoded tool result, nil when it is not JSON
func decodeResult(t *testing.T, result string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(result), &m); err != nil {
		t.Fatalf("result is not JSON: %q", result)
	}
	return m
}

// Reports whether a result is the multimodal envelope
func embeddedResult(t *testing.T, result string) bool {
	t.Helper()
	return IsMultimodalToolResult(decodeResult(t, result))
}

// Length of the embedded image data URL
func embedLen(t *testing.T, result string) int {
	t.Helper()
	m := decodeResult(t, result)
	for _, p := range m["content"].([]any) {
		part := p.(map[string]any)
		if part["type"] == "image_url" {
			return len(part["image_url"].(map[string]any)["url"].(string))
		}
	}
	t.Fatal("no image part")
	return 0
}

// Native load of one image for a session
func nativeLoad(env *Env, session, image string, region any) string {
	return visionAnalyzeNative(context.Background(), env, session, image, "q", "", region)
}

func TestRepeatCapDelegatedSubagent(t *testing.T) {
	freshRepeatCounts(t)
	env := isrcEnv(t, "local")
	env.Values.Store("session.delegated_child", true)
	dir := t.TempDir()
	shot := writeFixturePNG(t, filepath.Join(dir, "shot.png"), 16, 16, false)
	if !embeddedResult(t, nativeLoad(env, "child-session", shot, nil)) ||
		!embeddedResult(t, nativeLoad(env, "child-session", shot, []any{0.0, 0.0, 8.0, 8.0})) ||
		!embeddedResult(t, nativeLoad(env, "child-session", shot, nil)) {
		t.Fatal("first three loads must embed")
	}
	refused := decodeResult(t, nativeLoad(env, "child-session", shot, []any{4.0, 4.0, 12.0, 12.0}))
	if refused["success"] != false {
		t.Fatalf("fourth load not refused: %v", refused)
	}
	msg := refused["error"].(string)
	if !strings.Contains(msg, "already been loaded") || !strings.Contains(msg, "max_calls_per_image") {
		t.Fatalf("refusal text %q", msg)
	}
	other := writeFixturePNG(t, filepath.Join(dir, "other.png"), 16, 16, false)
	if !embeddedResult(t, nativeLoad(env, "child-session", other, nil)) {
		t.Fatal("a different image is not affected")
	}
}

func TestRepeatCapParallelBatchCannotOvershoot(t *testing.T) {
	freshRepeatCounts(t)
	env := isrcEnv(t, "local")
	env.Values.Store("session.delegated_child", true)
	shot := writeFixturePNG(t, filepath.Join(t.TempDir(), "shot.png"), 16, 16, false)
	results := make([]bool, 6)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var m map[string]any
			json.Unmarshal([]byte(nativeLoad(env, "child-parallel", shot, nil)), &m)
			results[i] = IsMultimodalToolResult(m)
		}(i)
	}
	wg.Wait()
	n := 0
	for _, ok := range results {
		if ok {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("embeds %d, want 3", n)
	}
	if c := repeatCounts.count[repeatKey{"child-parallel", visionImageKey(env, shot)}]; c != 3 {
		t.Fatalf("count %d", c)
	}
}

func TestRepeatCapFailedEmbedReleasesSlot(t *testing.T) {
	freshRepeatCounts(t)
	env := isrcEnv(t, "local")
	env.Values.Store("session.delegated_child", true)
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.png")
	for i := 0; i < 3; i++ {
		if decodeResult(t, nativeLoad(env, "child-release", missing, nil))["success"] != false {
			t.Fatal("missing file must fail")
		}
	}
	shot := writeFixturePNG(t, filepath.Join(dir, "shot.png"), 16, 16, false)
	for i := 0; i < 3; i++ {
		if !embeddedResult(t, nativeLoad(env, "child-release", shot, nil)) {
			t.Fatalf("load %d refused after failed loads", i)
		}
	}
	if _, ok := repeatCounts.count[repeatKey{"child-release", visionImageKey(env, missing)}]; ok {
		t.Fatal("failed loads kept their slot")
	}
}

func TestRepeatCapMainAgentUnlimitedUnlessConfigured(t *testing.T) {
	freshRepeatCounts(t)
	env := isrcEnv(t, "local")
	shot := writeFixturePNG(t, filepath.Join(t.TempDir(), "shot.png"), 16, 16, false)
	for i := 0; i < 5; i++ {
		if !embeddedResult(t, nativeLoad(env, "main", shot, nil)) {
			t.Fatal("main agent must be unlimited")
		}
	}
	env.Config.Set("vision.max_calls_per_image", 1)
	freshRepeatCounts(t)
	if !embeddedResult(t, nativeLoad(env, "main", shot, nil)) {
		t.Fatal("first configured load must embed")
	}
	if decodeResult(t, nativeLoad(env, "main", shot, nil))["success"] != false {
		t.Fatal("second configured load must be refused")
	}
}

func TestRepeatCountsEvictOldestPairs(t *testing.T) {
	freshRepeatCounts(t)
	repeatCounts.Lock()
	for i := 0; i < repeatCountsMaxKeys+5; i++ {
		recordEmbedLocked(repeatKey{session: "s", image: string(rune('a'+i%26)) + strconv.Itoa(i)})
	}
	size := len(repeatCounts.count)
	_, firstKept := repeatCounts.count[repeatKey{session: "s", image: "a0"}]
	repeatCounts.Unlock()
	if size != repeatCountsMaxKeys || firstKept {
		t.Fatalf("size %d first kept %v", size, firstKept)
	}
}

func TestEmbedTargetBytesFollowsConfig(t *testing.T) {
	freshRepeatCounts(t)
	env := isrcEnv(t, "local")
	dense := writeFixturePNG(t, filepath.Join(t.TempDir(), "dense.png"), 400, 400, true)
	if n := embedLen(t, nativeLoad(env, "s", dense, nil)); n <= 65536 || n > defaultEmbedTargetBytes {
		t.Fatalf("default embed %d", n)
	}
	env.Config.Set("vision.embed_target_bytes", 65536)
	if n := embedLen(t, nativeLoad(env, "s", dense, nil)); n > 65536 {
		t.Fatalf("configured embed %d", n)
	}
	for raw, want := range map[any]int{"not-a-number": defaultEmbedTargetBytes, true: defaultEmbedTargetBytes, 1: minEmbedTargetBytes, 64 * 1024 * 1024: maxEmbedTargetBytes} {
		env.Config.Set("vision.embed_target_bytes", raw)
		if got := resolveEmbedTargetBytes(env); got != want {
			t.Errorf("%v: got %d want %d", raw, got, want)
		}
	}
}

func TestNativeTurnDedupe(t *testing.T) {
	freshRepeatCounts(t)
	env := isrcEnv(t, "local")
	dir := t.TempDir()
	same := writeFixturePNG(t, filepath.Join(dir, "same.png"), 16, 16, false)
	other := writeFixturePNG(t, filepath.Join(dir, "other.png"), 16, 16, true)
	parts, skipped, err := BuildNativeContentParts(env, "look", []string{same}, nil)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("native parts %v %v", skipped, err)
	}
	restore := NativeTurnImages(env, parts)
	result := nativeLoad(env, "s", same, nil)
	if embeddedResult(t, result) || decodeResult(t, result)["already_in_context"] != true {
		t.Fatalf("duplicate embedded: %q", result)
	}
	if !embeddedResult(t, nativeLoad(env, "s", same, []any{0.0, 0.0, 8.0, 8.0})) || !embeddedResult(t, nativeLoad(env, "s", other, nil)) {
		t.Fatal("crop and other image must embed")
	}
	restore()
	if !embeddedResult(t, nativeLoad(env, "s", same, nil)) {
		t.Fatal("scope ended with the turn")
	}
	textOnly := []any{map[string]any{"type": "text", "text": "[Image attached at: " + same + "]"}}
	restore = NativeTurnImages(env, textOnly)
	defer restore()
	if !embeddedResult(t, nativeLoad(env, "s", same, nil)) {
		t.Fatal("handles without native image parts do not dedupe")
	}
}

func TestVisionImageKey(t *testing.T) {
	env := isrcEnv(t, "local")
	t.Setenv("HOME", env.Cwd)
	if visionImageKey(env, "~/a.png") != "file:"+resolveNonStrict(filepath.Join(env.Cwd, "a.png")) {
		t.Fatalf("tilde key %q", visionImageKey(env, "~/a.png"))
	}
	if visionImageKey(env, "file://"+filepath.Join(env.Cwd, "a.png")) != visionImageKey(env, filepath.Join(env.Cwd, "a.png")) {
		t.Fatal("file URI and path keys differ")
	}
	if visionImageKey(env, "https://x.test/a.png#frag") != "url:https://x.test/a.png" {
		t.Fatal("fragment kept")
	}
	if k := visionImageKey(env, "data:image/png;base64,AAAA"); len(k) != len("data:")+32 {
		t.Fatalf("data key %q", k)
	}
}
