package media

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Clean process home and Meta env so every case starts from no credentials
func metaHomeEnv(t *testing.T) *tools.Env {
	t.Helper()
	env := openAIHomeEnv(t, nil)
	for _, k := range []string{"MODEL_API_KEY", "META_API_KEY", "META_MODEL_API_KEY", "META_BASE_URL", "META_IMAGE_MODEL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	return env
}

// Adds a catalog entry for the test and removes it afterwards
func addMetaTestModel(t *testing.T, id string) {
	t.Helper()
	metaImageModels.meta[id] = metaImageModels.meta[metaDefaultModel]
	t.Cleanup(func() { delete(metaImageModels.meta, id) })
}

func TestMetaCatalogAvailabilityAndResolution(t *testing.T) {
	metaHomeEnv(t)
	for _, row := range newMetaImageGenProvider().ListModels() {
		for _, k := range []string{"display", "speed", "strengths", "price"} {
			if !pyTruthy(row[k]) {
				t.Fatal(row)
			}
		}
	}
	if newMetaImageGenProvider().IsAvailable() {
		t.Fatal("available without a key")
	}
	for _, name := range metaAPIKeyEnvs {
		t.Setenv(name, "test")
		if !newMetaImageGenProvider().IsAvailable() {
			t.Fatal(name)
		}
		os.Unsetenv(name)
	}
	t.Setenv("META_MODEL_API_KEY", "third")
	t.Setenv("META_API_KEY", "second")
	t.Setenv("MODEL_API_KEY", "first")
	if got := resolveMetaAPIKey(context.Background()); got != "first" {
		t.Fatal(got)
	}
	t.Setenv("META_BASE_URL", "https://proxy.internal/v1")
	if got := resolveMetaBaseURL(context.Background()); got != "https://proxy.internal/v1" {
		t.Fatal(got)
	}
}

func TestMetaBaseURLFollowsTheProfileSecretScope(t *testing.T) {
	metaHomeEnv(t)
	t.Setenv("META_BASE_URL", "https://launch.example/v1")
	provider.SetMultiplexActive(true)
	defer provider.SetMultiplexActive(false)
	for _, c := range []struct {
		scope map[string]string
		want  string
	}{{map[string]string{"META_BASE_URL": "https://profile-b.example/v1"}, "https://profile-b.example/v1"}, {map[string]string{}, metaDefaultBaseURL}} {
		ctx := provider.WithSecretScope(context.Background(), c.scope, "")
		if got := resolveMetaBaseURL(ctx); got != c.want {
			t.Fatal(got, c.want)
		}
	}
}

func TestMetaModelResolution(t *testing.T) {
	env := metaHomeEnv(t)
	t.Setenv("META_IMAGE_MODEL", "not-a-real-model")
	if id, _ := resolveMetaImageModel(env, ""); id != metaDefaultModel {
		t.Fatal(id)
	}
	addMetaTestModel(t, "muse-image-test")
	t.Setenv("META_IMAGE_MODEL", "muse-image-1.0")
	if id, _ := resolveMetaImageModel(env, "muse-image-test"); id != "muse-image-test" {
		t.Fatal(id)
	}
	os.Unsetenv("META_IMAGE_MODEL")
	if id, _ := resolveMetaImageModel(env, "not-a-real-model"); id != metaDefaultModel {
		t.Fatal(id)
	}
}

func TestMetaGenerate(t *testing.T) {
	f := newFakeOpenAIImages(t)
	env := metaHomeEnv(t)
	ctx := context.Background()
	p := newMetaImageGenProvider()
	if res, _ := p.Generate(ctx, env, ImageGenRequest{Prompt: "a cat"}); res["success"] != false || res["error_type"] != "auth_required" {
		t.Fatal(res)
	}
	t.Setenv("META_MODEL_API_KEY", "test-key")
	if res, _ := p.Generate(ctx, env, ImageGenRequest{Prompt: "", AspectRatio: "square"}); res["error_type"] != "invalid_argument" || res["provider"] != "meta-ai" {
		t.Fatal(res)
	}
	addMetaTestModel(t, "muse-image-test")
	if res, _ := p.Generate(ctx, env, ImageGenRequest{Prompt: "a cat", Kwargs: map[string]any{"model": "muse-image-test"}}); res["success"] != true || f.last(t).JSON["model"] != "muse-image-test" {
		t.Fatal(res, f.last(t).JSON)
	}
	res, _ := p.Generate(ctx, env, ImageGenRequest{Prompt: "a cat", AspectRatio: "landscape"})
	saved, _ := res["image"].(string)
	if res["success"] != true || res["model"] != metaDefaultModel || res["aspect_ratio"] != "landscape" || res["provider"] != "meta-ai" || res["modality"] != "text" ||
		filepath.Dir(saved) != filepath.Join(env.Home, "cache", "generated", "images") || !strings.HasSuffix(saved, ".webp") {
		t.Fatal(res)
	}
	if data, _ := os.ReadFile(saved); string(data) != string(openAITestPNG) {
		t.Fatal("cached bytes differ")
	}
	body := f.last(t).JSON
	if body["model"] != metaDefaultModel || body["size"] != "1536x1024" || f.last(t).Header.Get("Authorization") != "Bearer test-key" {
		t.Fatal(body)
	}
	if o := f.options[len(f.options)-1]; *o.BaseURL != metaDefaultBaseURL || o.DefaultHeaders != nil || o.HTTPClient != nil {
		t.Fatalf("%+v", o)
	}
	t.Setenv("META_BASE_URL", "https://proxy.internal/v1")
	p.Generate(ctx, env, ImageGenRequest{Prompt: "a cat"})
	if o := f.options[len(f.options)-1]; *o.BaseURL != "https://proxy.internal/v1" {
		t.Fatal(*o.BaseURL)
	}
	for aspect, size := range map[string]string{"landscape": "1536x1024", "square": "1024x1024", "portrait": "1024x1536"} {
		p.Generate(ctx, env, ImageGenRequest{Prompt: "a cat", AspectRatio: aspect})
		if f.last(t).JSON["size"] != size {
			t.Fatal(aspect)
		}
	}
	png := base64.StdEncoding.EncodeToString(openAITestPNG)
	f.reply = func(*http.Request) (int, string) {
		return 200, `{"data": [{"b64_json": "` + png + `", "revised_prompt": "A photo of a cat"}]}`
	}
	if res, _ := p.Generate(ctx, env, ImageGenRequest{Prompt: "a cat"}); res["revised_prompt"] != "A photo of a cat" {
		t.Fatal(res)
	}
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(openAITestPNG) }))
	defer imgSrv.Close()
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "1")
	f.reply = func(*http.Request) (int, string) {
		return 200, `{"data": [{"b64_json": null, "url": "` + imgSrv.URL + `/img.webp"}]}`
	}
	res, _ = p.Generate(ctx, env, ImageGenRequest{Prompt: "a cat"})
	img, _ := res["image"].(string)
	if res["success"] != true || !strings.HasPrefix(filepath.Base(img), "meta_") || strings.Contains(img, "127.0.0.1") {
		t.Fatal(res)
	}
	for _, body := range []string{`{"data": [{"b64_json": null, "url": null}]}`, `{"data": []}`, `"not an object"`} {
		f.reply = func(*http.Request) (int, string) { return 200, body }
		if res, _ := p.Generate(ctx, env, ImageGenRequest{Prompt: "a cat"}); res["success"] != false || res["error_type"] != "empty_response" {
			t.Fatal(body, res)
		}
	}
	f.reply = func(*http.Request) (int, string) { return 400, `{"error": {"message": "boom"}}` }
	if res, _ := p.Generate(ctx, env, ImageGenRequest{Prompt: "a cat"}); res["success"] != false || res["error_type"] != "api_error" || !strings.Contains(res["error"].(string), "boom") {
		t.Fatal(res)
	}
}
