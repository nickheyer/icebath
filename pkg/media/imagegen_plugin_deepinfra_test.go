package media

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Empty DeepInfra catalog cache for the test and afterwards
func resetDeepInfraCatalogCache(t *testing.T) {
	t.Helper()
	reset := func() {
		deepInfraCatalogCache.Lock()
		deepInfraCatalogCache.entries = map[string][]map[string]any{}
		deepInfraCatalogCache.failed = map[string]time.Time{}
		deepInfraCatalogCache.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func TestDeepInfraListModelsFiltersByImageGenTag(t *testing.T) {
	openAIHomeEnv(t, nil)
	resetDeepInfraCatalogCache(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"data": [{"id": "vendor/chat", "metadata": {"tags": ["chat"]}},
			{"id": "vendor/img", "metadata": {"tags": ["image-gen"], "pricing": {"per_image_unit": 0.005}, "default_width": 1024}}]}`)
	}))
	defer srv.Close()
	t.Setenv("DEEPINFRA_API_KEY", "test-key")
	t.Setenv("DEEPINFRA_BASE_URL", srv.URL)
	p := newDeepInfraImageGenProvider()
	rows := p.ListModels()
	if len(rows) != 1 || rows[0]["id"] != "vendor/img" || rows[0]["price"] != "$0.0050/image" || rows[0]["display"] != "img" {
		t.Fatal(rows)
	}
	if w := rows[0]["default_width"]; w == nil || pyStrOf(w) != "1024" {
		t.Fatal(rows[0])
	}
	if p.DefaultModel() != "vendor/img" || !p.IsAvailable() {
		t.Fatal(p.DefaultModel())
	}
	if row := formatDeepInfraCatalogRow(map[string]any{"id": "x", "metadata": map[string]any{"pricing": map[string]any{"per_image_unit": "abc"}}}); row["price"] != nil || row["strengths"] != "" {
		t.Fatal(row)
	}
}

func TestDeepInfraGenerateCallsOpenAIClientWithDeepInfraBaseURL(t *testing.T) {
	f := newFakeOpenAIImages(t)
	env := openAIHomeEnv(t, nil)
	resetDeepInfraCatalogCache(t)
	prev := deepInfraImageLiveModels
	deepInfraImageLiveModels = func(context.Context, *tools.Env) []map[string]any { return nil }
	t.Cleanup(func() { deepInfraImageLiveModels = prev })
	t.Setenv("DEEPINFRA_API_KEY", "test-key")
	os.Unsetenv("DEEPINFRA_BASE_URL")
	p := newDeepInfraImageGenProvider()
	if res, _ := p.Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat", AspectRatio: "square"}); res["error_type"] != "no_model_available" || res["prompt"] != "a cat" {
		t.Fatal(res)
	}
	t.Setenv("DEEPINFRA_IMAGE_MODEL", "vendor/test-img")
	res, err := p.Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat", AspectRatio: "square"})
	if err != nil || res["success"] != true || res["model"] != "vendor/test-img" || !strings.Contains(res["image"].(string), "deepinfra_test-img") {
		t.Fatal(res, err)
	}
	o := f.options[len(f.options)-1]
	if !strings.Contains(*o.BaseURL, "deepinfra") || *o.APIKey != "test-key" || f.last(t).JSON["model"] != "vendor/test-img" || f.last(t).JSON["size"] != "1024x1024" {
		t.Fatalf("%+v %v", o, f.last(t).JSON)
	}
	if res, _ := p.Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat", ImageURL: "https://x/y.png"}); res["error_type"] != "modality_unsupported" {
		t.Fatal(res)
	}
	os.Unsetenv("DEEPINFRA_API_KEY")
	if res, _ := p.Generate(context.Background(), env, ImageGenRequest{Prompt: "a cat"}); res["error_type"] != "auth_required" || p.IsAvailable() {
		t.Fatal(res)
	}
}
