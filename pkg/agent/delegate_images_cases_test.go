package agent

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

const delegateImagePNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYGBgAAAABQABh6FO1AAAAABJRU5ErkJggg=="

func delegateImagePNG(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(delegateImagePNGBase64)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Writes the one pixel PNG under dir and returns its path
func writeDelegatePNG(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, delegateImagePNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Child of test_delegate_images.py _FakeChild on the openrouter vision route
func imageFakeChild(env *tools.Env) *AIAgent {
	return &AIAgent{provider: "openrouter", model: "some/vision-model", env: env}
}

// Forces the image input mode decision, a panic standing in for the hermes RuntimeError
func forceImageInputMode(t *testing.T, fn func() string) {
	t.Helper()
	prev := decideImageInputModeFn
	decideImageInputModeFn = func(context.Context, *tools.Env, string, string, string) string { return fn() }
	t.Cleanup(func() { decideImageInputModeFn = prev })
}

func imageURLsOf(msg any) ([]any, []string) {
	parts, _ := msg.([]any)
	var urls []string
	for _, p := range parts {
		m, _ := p.(map[string]any)
		if m["type"] == "image_url" {
			urls = append(urls, m["image_url"].(map[string]any)["url"].(string))
		}
	}
	return parts, urls
}

func TestNativeModeBuildsImagePartsForEverySourceKind(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	dir := t.TempDir()
	path := writeDelegatePNG(t, dir, "shot.png")
	url := "https://example.test/mock.png"
	dataURL := "data:image/png;base64," + delegateImagePNGBase64
	forceImageInputMode(t, func() string { return "native" })
	msg := buildChildGoalMessage(context.Background(), "Inspect the mock", []string{path, url, dataURL}, imageFakeChild(env))
	parts, images := imageURLsOf(msg)
	if len(parts) == 0 {
		t.Fatalf("message %v", msg)
	}
	first := parts[0].(map[string]any)
	text, _ := first["text"].(string)
	if first["type"] != "text" || !strings.Contains(text, "Inspect the mock") {
		t.Fatalf("first part %v", first)
	}
	dataCount, hasURL := 0, false
	for _, u := range images {
		if u == dataURL {
			dataCount++
		}
		hasURL = hasURL || u == url
	}
	if len(images) != 3 || !hasURL || dataCount != 2 || strings.Contains(text, dataURL) {
		t.Fatalf("images %v text %q", images, text)
	}
	if got := buildChildGoalMessage(context.Background(), "Goal text", []string{filepath.Join(dir, "nope.png")}, imageFakeChild(env)); got != "Goal text" {
		t.Fatalf("unreadable sources gave %v", got)
	}
}

func TestTextModeHintsAndFailureDegrade(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	dir := t.TempDir()
	path := writeDelegatePNG(t, dir, "shot.png")
	url := "https://example.test/a.png"
	forceImageInputMode(t, func() string { return "text" })
	msg, ok := buildChildGoalMessage(context.Background(), "Goal", []string{path, url, filepath.Join(dir, "gone.png")}, imageFakeChild(env)).(string)
	if !ok {
		t.Fatalf("message %v", msg)
	}
	if !strings.Contains(msg, "[Image attached at: "+path+"]") || !strings.Contains(msg, "[Image attached: "+url+"]") || !strings.Contains(msg, "vision_analyze") {
		t.Fatalf("message %q", msg)
	}
	if strings.Contains(msg, "gone.png") {
		t.Fatalf("missing image forwarded %q", msg)
	}
	forceImageInputMode(t, func() string { panic("boom") })
	if got := buildChildGoalMessage(context.Background(), "Plain goal", []string{"/tmp/x.png"}, imageFakeChild(env)); got != "Plain goal" {
		t.Fatalf("failure gave %v", got)
	}
}

func TestChildRunSendsMultimodalGoalTurn(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	path := writeDelegatePNG(t, t.TempDir(), "shot.png")
	child := imageFakeChild(env)
	child.delegateImages = []string{path}
	parent := &AIAgent{env: env}
	var seen any
	prevRun := runChildConversationFn
	runChildConversationFn = func(_ context.Context, _ *AIAgent, userMessage any, _ RunConversationOptions) (map[string]any, error) {
		seen = userMessage
		return map[string]any{"final_response": "ok", "completed": true}, nil
	}
	prevWorktree := createIsolatedWorktreeFn
	createIsolatedWorktreeFn = func(context.Context, *AIAgent, string, string) map[string]string { return nil }
	t.Cleanup(func() { runChildConversationFn, createIsolatedWorktreeFn = prevRun, prevWorktree })
	forceImageInputMode(t, func() string { return "native" })
	run := &childRun{ctx: context.Background(), child: child, parent: parent, taskIndex: 0, goal: "Describe it", heartbeat: newChildHeartbeat(child, parent, 0)}
	run.seedWorkspace()
	result, failure, _ := run.awaitChild()
	if failure != nil || result["final_response"] != "ok" {
		t.Fatalf("result %v failure %v", result, failure)
	}
	content, images := imageURLsOf(seen)
	if len(content) < 2 || content[0].(map[string]any)["type"] != "text" || len(images) != 1 || !strings.HasPrefix(images[0], "data:image/png;base64,") {
		t.Fatalf("content %v", seen)
	}
	if content[1].(map[string]any)["image_url"].(map[string]any)["url"] != images[0] {
		t.Fatalf("content %v", seen)
	}
}

func TestImagesAdvertisedPerTaskOnly(t *testing.T) {
	props := tools.DelegateTaskSchema["parameters"].(map[string]any)["properties"].(map[string]any)
	item := props["tasks"].(map[string]any)["items"].(map[string]any)
	images := item["properties"].(map[string]any)["images"].(map[string]any)
	required, _ := item["required"].([]any)
	for _, r := range required {
		if r == "images" {
			t.Fatal("images required")
		}
	}
	if _, top := props["images"]; images["type"] != "array" || top {
		t.Fatalf("images %v top level %v", images, top)
	}
}
