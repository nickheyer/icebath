package media

import (
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestPluginCommonSizeAndCatalogRows(t *testing.T) {
	if sizeFor("portrait") != "1024x1536" || sizeFor("wide") != "1024x1024" {
		t.Fatal("size mapping")
	}
	rows := catalogRows(imageModelCatalog{order: []string{"b", "a"}, meta: map[string]map[string]any{"b": {"speed": "1s"}, "a": {"display": "A"}}}, nil, nil)
	if !jsonEqual(rows, []map[string]any{{"id": "b", "display": "b", "speed": "1s", "strengths": "", "price": ""}, {"id": "a", "display": "A", "speed": "", "strengths": "", "price": ""}}) {
		t.Fatalf("rows %v", rows)
	}
	price := "$0.04"
	rows = catalogRows(gptImage2Tiers, []string{"display"}, &price)
	if len(rows) != 3 || rows[0]["id"] != "gpt-image-2-low" || rows[2]["price"] != "$0.04" || rows[1]["display"] != "GPT Image 2 (Medium)" {
		t.Fatalf("tiers %v", rows)
	}
	s := apiKeySetupSchema(apiKeySetup{Name: "N", Badge: "paid", Tag: "t", Key: "K", Prompt: "p", URL: "u"})
	if !jsonEqual(s, map[string]any{"name": "N", "badge": "paid", "tag": "t", "env_vars": []any{map[string]any{"key": "K", "prompt": "p", "url": "u"}}}) {
		t.Fatalf("setup %v", s)
	}
}

func TestPluginCommonResolveStaticModel(t *testing.T) {
	env := imageEnv(t, config.Tree{"image_gen": map[string]any{"model": "gpt-image-2-high", "openai": map[string]any{"model": "gpt-image-2-low"}}})
	o := staticModelOptions{EnvVar: "TEST_IMAGE_MODEL", ConfigKey: "openai"}
	if id, _ := resolveStaticModel(env, gptImage2Tiers, gptImage2Default, o); id != "gpt-image-2-low" {
		t.Fatalf("scoped %s", id)
	}
	o.Explicit = " gpt-image-2-high "
	if id, _ := resolveStaticModel(env, gptImage2Tiers, gptImage2Default, o); id != "gpt-image-2-high" {
		t.Fatalf("explicit %s", id)
	}
	o.Explicit = ""
	t.Setenv("TEST_IMAGE_MODEL", "gpt-image-2-medium")
	if id, _ := resolveStaticModel(env, gptImage2Tiers, gptImage2Default, o); id != "gpt-image-2-medium" {
		t.Fatalf("env %s", id)
	}
	t.Setenv("TEST_IMAGE_MODEL", "")
	setImageConfig(t, env, config.Tree{"image_gen": map[string]any{"model": "gpt-image-2-high", "openai": map[string]any{"model": "custom-gw-model"}}})
	if id, _ := resolveStaticModel(env, gptImage2Tiers, gptImage2Default, o); id != "gpt-image-2-high" {
		t.Fatalf("top level %s", id)
	}
	o.ExcludeTopLevel = true
	if id, _ := resolveStaticModel(env, gptImage2Tiers, gptImage2Default, o); id != gptImage2Default {
		t.Fatalf("default %s", id)
	}
	o.Passthrough = true
	id, meta := resolveStaticModel(env, gptImage2Tiers, gptImage2Default, o)
	if id != "custom-gw-model" || !jsonEqual(meta, map[string]any{"display": "custom-gw-model", "api_model": "custom-gw-model", "quality": nil}) {
		t.Fatalf("passthrough %s %v", id, meta)
	}
	o.Config = map[string]any{"model": "other-provider-model"}
	if id, _ := resolveStaticModel(env, gptImage2Tiers, gptImage2Default, o); id != gptImage2Default {
		t.Fatalf("top level never passes through %s", id)
	}
}

func TestPluginCommonCollectSourcesAndErrors(t *testing.T) {
	if got := collectSourceImages(" a ", []any{"b", " ", "c"}, -1); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("%v", got)
	}
	if got := collectSourceImages("", []any{"b", "c"}, 1); strings.Join(got, ",") != "b" {
		t.Fatalf("%v", got)
	}
	model := "m2"
	e := errorFactory("p", "square", "m", "draw")("boom", "api_error", errorOverride{Model: &model})
	if e["provider"] != "p" || e["model"] != "m2" || e["prompt"] != "draw" || e["aspect_ratio"] != "square" || e["error"] != "boom" || e["error_type"] != "api_error" {
		t.Fatalf("%v", e)
	}
	if e := promptRequiredError("p", "portrait"); e["error"] != promptRequired || e["error_type"] != "invalid_argument" {
		t.Fatalf("%v", e)
	}
	recordTokenUsage(context.Background(), nil, "m", "p", "")
	recordTokenUsage(context.Background(), map[string]any{"prompt_tokens": 3}, "m", "p", "")
}

func TestPluginCommonMaterializeImage(t *testing.T) {
	env := imageEnv(t, config.Tree{})
	o := materializeOptions{Prefix: "t", Label: "Test", Provider: "p", Model: "m", Prompt: "x", Aspect: "square"}
	path, failure := materializeImage(context.Background(), env, base64.StdEncoding.EncodeToString(png1px), "", o)
	if failure != nil || !strings.HasPrefix(path, env.Home) {
		t.Fatalf("%s %v", path, failure)
	}
	if b, _ := os.ReadFile(path); string(b) != string(png1px) {
		t.Fatal("cached bytes differ")
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	var seen error
	o.OnURLFail = func(err error) { seen = err }
	if ref, failure := materializeImage(context.Background(), env, "", srv.URL+"/x.png", o); ref != srv.URL+"/x.png" || failure != nil || seen == nil {
		t.Fatalf("%s %v %v", ref, failure, seen)
	}
	if _, failure := materializeImage(context.Background(), env, "", "", o); failure["error"] != "Test response contained neither b64_json nor URL" || failure["error_type"] != "empty_response" {
		t.Fatalf("%v", failure)
	}
	blocker := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocker, nil, 0o644)
	if _, failure := materializeImage(context.Background(), &tools.Env{Home: filepath.Join(blocker, "home")}, "aGk=", "", o); failure["error_type"] != "io_error" ||
		!strings.HasPrefix(failure["error"].(string), "Could not save image to cache: ") {
		t.Fatalf("%v", failure)
	}
}

func TestPluginCommonRequestsErrorMessage(t *testing.T) {
	long := strings.Repeat("é", 400)
	for body, want := range map[string]string{
		`{"error": {"message": "bad key"}}`: "bad key",
		`{"error": {"message": 7}}`:         "7",
		`{"error": {"code": 1}}`:            `{"error": {"code": 1}}`,
		`{"error": "flat"}`:                 `{"error": "flat"}`,
		`[1]`:                               `[1]`,
		long:                                strings.Repeat("é", 300),
	} {
		if got := defaultErrorMessage(400, []byte(body)); got != want {
			t.Fatalf("%.30q -> %.30q", body, got)
		}
	}
}

func TestPluginCommonPostJSONFailureKinds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte(`[{"a": 1}]`))
		case "/bad":
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error": {"message": "nope"}}`))
		case "/html":
			_, _ = w.Write([]byte(`<html>`))
		case "/slow":
			time.Sleep(300 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)
	ctx, to := context.Background(), requestsTimeout{Connect: time.Second, Read: time.Second}
	if body, f, err := postJSON(ctx, srv.URL+"/ok", nil, map[string]any{"x": 1}, to, "Test", nil, false); err != nil || f != nil || !jsonEqual(body, []any{map[string]any{"a": 1}}) {
		t.Fatalf("%v %v %v", body, f, err)
	}
	if _, f, _ := postJSON(ctx, srv.URL+"/bad", nil, nil, to, "Test", nil, false); f == nil || f.Kind != "http" || f.Status != 401 || f.Error != "Test image generation failed (401): nope" || f.ErrorType != "api_error" {
		t.Fatalf("%+v", f)
	}
	if _, f, _ := postJSON(ctx, srv.URL+"/html", nil, nil, to, "Test", nil, false); f == nil || f.Kind != "invalid_json" || f.ErrorType != "invalid_response" {
		t.Fatalf("%+v", f)
	}
	if _, f, _ := postJSON(ctx, srv.URL+"/slow", nil, nil, requestsTimeout{Connect: time.Second, Read: 100 * time.Millisecond}, "Test", nil, false); f == nil || f.Kind != "timeout" || f.Error != "Test image generation timed out (0s)" {
		t.Fatalf("%+v", f)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + l.Addr().String()
	l.Close()
	if _, f, _ := postJSON(ctx, dead, nil, nil, to, "Test", nil, false); f == nil || f.Kind != "unreachable" || f.ErrorType != "connection_error" {
		t.Fatalf("%+v", f)
	}
	if _, f, err := postJSON(ctx, "nota://x", nil, nil, to, "Test", nil, false); err == nil || f != nil {
		t.Fatalf("uncaught request error %v %v", f, err)
	}
	if _, f, err := postJSON(ctx, "nota://x", nil, nil, to, "Test", nil, true); err != nil || f == nil || f.Kind != "request" || !strings.HasPrefix(f.Error, "Test request failed: ") {
		t.Fatalf("caught request error %+v %v", f, err)
	}
}

func TestFalPluginSurface(t *testing.T) {
	p := newFalImageGenProvider()
	if p.DefaultModel() != falDefaultModel || p.Name() != "fal" || p.DisplayName() != "FAL.ai" {
		t.Fatal("identity")
	}
	rows := p.ListModels()
	if len(rows) != len(falModels) {
		t.Fatalf("%d rows for %d models", len(rows), len(falModels))
	}
	for _, r := range rows {
		if _, ok := falModels[r["id"].(string)]; !ok {
			t.Fatalf("unknown %v", r["id"])
		}
		for _, f := range []string{"id", "display", "speed", "strengths", "price"} {
			if _, ok := r[f]; !ok {
				t.Fatalf("%v lacks %s", r["id"], f)
			}
		}
	}
}

func TestFalPluginGenerateDelegatesToImageGenerateTool(t *testing.T) {
	env := imageEnv(t, config.Tree{"image_gen": map[string]any{"model": "fal-ai/flux-2/klein/9b"}})
	var got imageToolArgs
	prev := imageGenerateToolHook
	imageGenerateToolHook = func(_ context.Context, _ *tools.Env, a imageToolArgs) string {
		got = a
		return `{"success": true, "image": "https://fake/image.png"}`
	}
	t.Cleanup(func() { imageGenerateToolHook = prev })
	res, err := newFalImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "a serene mountain landscape", AspectRatio: "square", Kwargs: map[string]any{"seed": 42}})
	if err != nil || got.Prompt != "a serene mountain landscape" || got.AspectRatio != "square" || got.Seed != 42 {
		t.Fatalf("%+v %v", got, err)
	}
	if got.NumImages != nil || got.GuidanceScale != nil || got.Upscale != nil || got.ImageURL != nil || got.ReferenceImageURLs != nil || got.OutputFormat != nil || got.NumInferenceSteps != nil {
		t.Fatalf("extra kwargs %+v", got)
	}
	if !jsonEqual(res, map[string]any{"success": true, "image": "https://fake/image.png", "provider": "fal", "prompt": "a serene mountain landscape", "aspect_ratio": "square", "model": "fal-ai/flux-2/klein/9b"}) {
		t.Fatalf("%v", res)
	}
}

func TestFalPluginRegisterWiresProviderIntoRegistry(t *testing.T) {
	resetImageRegistry(t)
	env := imageEnv(t, config.Tree{})
	t.Setenv("ICEBATH_HOME", env.Home)
	plugins.InstallServices(&plugins.Services{ProviderRegistries: map[string]plugins.ScopedRegistry{"image_gen_provider": ImageGenScopedRegistry()}})
	t.Cleanup(func() { plugins.InstallServices(nil); plugins.ResetPluginManagersForTests() })
	plugins.ResetPluginManagersForTests()
	ctx := envContext(context.Background(), env)
	plugins.GetPluginManager(ctx).DiscoverAndLoad(ctx, true)
	if _, ok := GetImageGenProvider(ctx, "fal", "").(*falImageGenProvider); !ok {
		t.Fatalf("registered %T", GetImageGenProvider(ctx, "fal", ""))
	}
}
