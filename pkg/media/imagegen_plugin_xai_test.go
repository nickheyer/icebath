package media

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One request the fake xAI API received
type xaiSeenRequest struct {
	Method, Path string
	Header       http.Header
	JSON         map[string]any
}

// Fake xAI REST API answering images calls with the status and body the test sets
type xaiFakeAPI struct {
	srv    *httptest.Server
	mu     sync.Mutex
	seen   []xaiSeenRequest
	status int
	body   string
	delay  time.Duration
}

func (f *xaiFakeAPI) requests() []xaiSeenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]xaiSeenRequest(nil), f.seen...)
}

func (f *xaiFakeAPI) set(status int, body string) {
	f.mu.Lock()
	f.status, f.body = status, body
	f.mu.Unlock()
}

// Temp home with XAI_API_KEY set, the base URL on a fake API and the live catalog offline
func xaiTestEnv(t *testing.T) (*tools.Env, *xaiFakeAPI, func(context.Context, *tools.Env, *xaiImageCreds) (xaiLiveCatalog, error)) {
	t.Helper()
	env := openAIHomeEnv(t, nil)
	for _, k := range []string{"XAI_IMAGE_MODEL", "XAI_OAUTH_ACCESS_TOKEN", "ICEBATH_ALLOW_PRIVATE_URLS"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("XAI_API_KEY", "test-key-12345")
	f := &xaiFakeAPI{status: 200, body: `{"data": [{"b64_json": "dGVzdA=="}]}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.seen = append(f.seen, xaiSeenRequest{r.Method, r.URL.Path, r.Header.Clone(), body})
		status, resp, delay := f.status, f.body, f.delay
		f.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		if strings.HasSuffix(r.URL.Path, ".jpeg") || strings.HasSuffix(r.URL.Path, ".png") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, resp)
	}))
	t.Cleanup(f.srv.Close)
	t.Setenv("XAI_BASE_URL", f.srv.URL+"/v1")
	prev := fetchXAILiveImageModels
	fetchXAILiveImageModels = func(context.Context, *tools.Env, *xaiImageCreds) (xaiLiveCatalog, error) {
		return xaiLiveCatalog{}, errors.New("offline (test)")
	}
	resetXAILiveCache()
	t.Cleanup(func() { fetchXAILiveImageModels = prev; resetXAILiveCache() })
	return env, f, prev
}

func resetXAILiveCache() {
	xaiLiveCache.Lock()
	xaiLiveCache.process, xaiLiveCache.byCredential = nil, nil
	xaiLiveCache.Unlock()
}

// Live catalog seam answering with the given ids and modalities in order
func setXAILive(t *testing.T, ids []string, mods map[string][]any) {
	t.Helper()
	fetchXAILiveImageModels = func(context.Context, *tools.Env, *xaiImageCreds) (xaiLiveCatalog, error) {
		out := xaiLiveCatalog{order: ids, models: map[string]map[string]any{}}
		for _, id := range ids {
			out.models[id] = map[string]any{"input_modalities": mods[id], "aliases": []any{}}
		}
		return out, nil
	}
	resetXAILiveCache()
}

func TestXAIAvailabilityAndSetupSchema(t *testing.T) {
	xaiTestEnv(t)
	t.Setenv("XAI_API_KEY", "sk-xxx")
	p := newXAIImageGenProvider()
	if !p.IsAvailable() {
		t.Fatal("unavailable with XAI_API_KEY")
	}
	schema := p.GetSetupSchema()
	if envVars, _ := schema["env_vars"].([]any); envVars == nil || len(envVars) != 0 || schema["post_setup"] != "xai_grok" {
		t.Fatal(schema)
	}
}

func TestXAIModelResolutionCallerEnvAndUnknown(t *testing.T) {
	env, _, _ := xaiTestEnv(t)
	t.Setenv("XAI_IMAGE_MODEL", "grok-imagine-image")
	if id, _, _ := resolveXAIImageModel(context.Background(), env, "grok-imagine-image-quality"); id != "grok-imagine-image-quality" {
		t.Fatal(id)
	}
	if id, _, _ := resolveXAIImageModel(context.Background(), env, "not-a-real-model"); id != "grok-imagine-image" {
		t.Fatal(id)
	}
	if _, _, err := resolveXAIImageModel(context.Background(), env, []any{"x"}); err == nil || err.Error() != "unhashable type: 'list'" {
		t.Fatal(err)
	}
}

func TestXAIModelKwargForwardedToGenerate(t *testing.T) {
	env, f, _ := xaiTestEnv(t)
	res, err := newXAIImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "test", Kwargs: map[string]any{"model": "grok-imagine-image-quality"}})
	if err != nil || res["success"] != true || res["model"] != "grok-imagine-image-quality" {
		t.Fatal(res, err)
	}
	if seen := f.requests(); len(seen) != 1 || seen[0].JSON["model"] != "grok-imagine-image-quality" || seen[0].Path != "/v1/images/generations" {
		t.Fatal(seen)
	}
}

func TestXAILiveCatalogMergeFallbackAndEditModel(t *testing.T) {
	env, _, _ := xaiTestEnv(t)
	ctx := context.Background()
	if cat := xaiImageCatalog(ctx, env); strings.Join(cat.order, ",") != strings.Join(xaiImageModels.order, ",") {
		t.Fatal("offline catalog is not the static table", cat.order)
	}
	both := []any{"text", "image"}
	setXAILive(t, []string{"grok-imagine-image", "grok-imagine-image-3.0"}, map[string][]any{"grok-imagine-image": both, "grok-imagine-image-3.0": both})
	cat := xaiImageCatalog(ctx, env)
	if _, ok := cat.meta["grok-imagine-image-3.0"]; !ok || cat.meta["grok-imagine-image"]["display"] != xaiImageModels.meta["grok-imagine-image"]["display"] {
		t.Fatal(cat)
	}
	t.Setenv("XAI_IMAGE_MODEL", "grok-imagine-image-3.0")
	if id, _, _ := resolveXAIImageModel(ctx, env, nil); id != "grok-imagine-image-3.0" {
		t.Fatal(id)
	}
	setXAILive(t, []string{"grok-imagine-image-2.0", "grok-imagine-image-quality"}, map[string][]any{"grok-imagine-image-2.0": both, "grok-imagine-image-quality": both})
	t.Setenv("XAI_IMAGE_MODEL", "grok-imagine-image-2.0")
	if id, _ := resolveXAIEditModel(ctx, env, nil); id != "grok-imagine-image-2.0" {
		t.Fatal(id)
	}
	os.Unsetenv("XAI_IMAGE_MODEL")
	if id, _ := resolveXAIEditModel(ctx, env, "grok-imagine-image-2.0"); id != "grok-imagine-image-2.0" {
		t.Fatal(id)
	}
	setXAILive(t, []string{"grok-imagine-image-2.0", "grok-imagine-image-quality"}, map[string][]any{"grok-imagine-image-2.0": {"text"}, "grok-imagine-image-quality": both})
	if id, _ := resolveXAIEditModel(ctx, env, "grok-imagine-image-2.0"); id != "grok-imagine-image-quality" {
		t.Fatal(id)
	}
}

func TestXAILiveFetchParsesEndpointAndCachesPerCredential(t *testing.T) {
	env, f, realFetch := xaiTestEnv(t)
	f.set(200, `{"models": [{"id": " grok-x ", "input_modalities": ["text", "image"]}, {"name": "grok-y"}, {"id": ""}, "junk"]}`)
	live, err := realFetch(context.Background(), env, nil)
	if err != nil || strings.Join(live.order, ",") != "grok-x,grok-y" || len(live.models["grok-y"]["input_modalities"].([]any)) != 0 {
		t.Fatal(live, err)
	}
	seen := f.requests()
	if seen[0].Path != "/v1/image-generation-models" || seen[0].Header.Get("Authorization") != "Bearer test-key-12345" ||
		!strings.HasPrefix(seen[0].Header.Get("User-Agent"), "Icebath-Agent/") {
		t.Fatal(seen[0])
	}
	calls := 0
	fetchXAILiveImageModels = func(context.Context, *tools.Env, *xaiImageCreds) (xaiLiveCatalog, error) {
		calls++
		return xaiLiveCatalog{order: []string{"grok-z"}, models: map[string]map[string]any{"grok-z": {}}}, nil
	}
	ctx := home.WithOverride(context.Background(), env.Home)
	xaiLiveImageModels(ctx, env)
	xaiLiveImageModels(ctx, env)
	t.Setenv("XAI_API_KEY", "other-profile-key")
	xaiLiveImageModels(ctx, env)
	if calls != 2 {
		t.Fatal("per credential cache fetched", calls)
	}
}

func TestXAIGenerateOutcomes(t *testing.T) {
	env, f, _ := xaiTestEnv(t)
	ctx := context.Background()
	p := newXAIImageGenProvider()
	f.set(200, `{"data": [{"b64_json": "dGVzdC1pbWFnZS1kYXRh"}]}`)
	res, _ := p.Generate(ctx, env, ImageGenRequest{Prompt: "A cat playing piano"})
	saved, _ := res["image"].(string)
	if raw, err := os.ReadFile(saved); err != nil || string(raw) != "test-image-data" || res["provider"] != "xai" || res["model"] != "grok-imagine-image" || res["success"] != true {
		t.Fatal(res, err)
	}
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "1")
	gone := f.srv.URL + "/xai-tmp-imgen-already-404.jpeg"
	f.set(200, `{"data": [{"url": "`+gone+`"}]}`)
	if res, _ := p.Generate(ctx, env, ImageGenRequest{Prompt: "A cat playing piano"}); res["success"] != true || res["image"] != gone {
		t.Fatal(res)
	}
	f.set(401, `{"error": {"message": "Invalid API key"}}`)
	if res, _ := p.Generate(ctx, env, ImageGenRequest{Prompt: "test"}); res["success"] != false || res["error_type"] != "api_error" {
		t.Fatal(res)
	}
	f.set(200, `{"data": []}`)
	if res, _ := p.Generate(ctx, env, ImageGenRequest{Prompt: "test"}); res["success"] != false || res["error_type"] != "empty_response" {
		t.Fatal(res)
	}
	prev := xaiImageRequestTimeout
	xaiImageRequestTimeout = 50 * time.Millisecond
	f.mu.Lock()
	f.delay = 300 * time.Millisecond
	f.mu.Unlock()
	res, _ = p.Generate(ctx, env, ImageGenRequest{Prompt: "test"})
	xaiImageRequestTimeout = prev
	f.mu.Lock()
	f.delay = 0
	f.mu.Unlock()
	if res["success"] != false || res["error_type"] != "timeout" {
		t.Fatal(res)
	}
	os.Unsetenv("XAI_API_KEY")
	res, _ = p.Generate(ctx, env, ImageGenRequest{Prompt: "test"})
	if errText, _ := res["error"].(string); res["success"] != false || !strings.Contains(errText, "XAI_API_KEY") {
		t.Fatal(res)
	}
}

func TestXAIWirePayloadHeadersAndStorage(t *testing.T) {
	env, f, _ := xaiTestEnv(t)
	f.set(200, `{"data": [{"b64_json": "dGVzdA=="}]}`)
	newXAIImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "test"})
	seen := f.requests()[0]
	if !strings.Contains(seen.Header.Get("Authorization"), "Bearer test-key-12345") || !strings.Contains(seen.Header.Get("User-Agent"), "Icebath-Agent") {
		t.Fatal(seen.Header)
	}
	if r := seen.JSON["resolution"]; r != "1k" && r != "2k" {
		t.Fatalf("resolution %#v", r)
	}
	so, _ := seen.JSON["storage_options"].(map[string]any)
	filename, _ := so["filename"].(string)
	if so["public_url"] != true || so["expires_after"] != nil || !strings.HasSuffix(filename, ".png") {
		t.Fatal(so)
	}
}

func TestXAIEditRejectsBareFileIDInputs(t *testing.T) {
	env, f, _ := xaiTestEnv(t)
	p := newXAIImageGenProvider()
	for _, req := range []ImageGenRequest{
		{Prompt: "make the robot red", ImageURL: "file_03eb65b1-aa97-482f-9ef0-b04f9172ea00"},
		{Prompt: "combine these robots into one product shot", ImageURL: "file_03eb65b1-aa97-482f-9ef0-b04f9172ea00",
			ReferenceImageURLs: []string{"file_54b48d6d-28ad-4982-9d72-bd3ac677c9bc", "file_aa11bb22-cc33-44dd-88ee-ff0011223344"}},
	} {
		if res, _ := p.Generate(context.Background(), env, req); res["success"] != false || res["error_type"] != "invalid_image_url" {
			t.Fatal(res)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Fatal("posted", n)
	}
}

func TestXAIPublicURLFileOutputWinsOverTemporaryURL(t *testing.T) {
	env, f, _ := xaiTestEnv(t)
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "1")
	f.set(200, `{"data": [{"url": "`+f.srv.URL+`/xai-tmp-imgen-test.jpeg", "file_output": {"file_id": "file-123", "filename": "stored.png", `+
		`"public_url": "https://xai-files.example/stored.png", "public_url_expires_at": 1234567890}}]}`)
	res, _ := newXAIImageGenProvider().Generate(context.Background(), env, ImageGenRequest{Prompt: "A cat playing piano"})
	if res["success"] != true || res["image"] != "https://xai-files.example/stored.png" || res["public_url"] != "https://xai-files.example/stored.png" || res["filename"] != "stored.png" {
		t.Fatal(res)
	}
	if _, ok := res["file_id"]; ok {
		t.Fatal(res)
	}
	for _, s := range f.requests() {
		if s.Method == http.MethodGet {
			t.Fatal("temporary URL downloaded", s.Path)
		}
	}
}

func TestXAIRegisterWiresProviderIntoRegistry(t *testing.T) {
	resetImageRegistry(t)
	env := imageEnv(t, config.Tree{})
	t.Setenv("ICEBATH_HOME", env.Home)
	plugins.InstallServices(&plugins.Services{ProviderRegistries: map[string]plugins.ScopedRegistry{"image_gen_provider": ImageGenScopedRegistry()}})
	t.Cleanup(func() { plugins.InstallServices(nil); plugins.ResetPluginManagersForTests() })
	plugins.ResetPluginManagersForTests()
	ctx := envContext(context.Background(), env)
	plugins.GetPluginManager(ctx).DiscoverAndLoad(ctx, true)
	if p, ok := GetImageGenProvider(ctx, "xai", "").(*xaiImageGenProvider); !ok || p.Name() != "xai" {
		t.Fatalf("registered %T", GetImageGenProvider(ctx, "xai", ""))
	}
}

func TestXAIImageFieldExpandsUserHomeAndBlocksCredentialStore(t *testing.T) {
	env, _, _ := xaiTestEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.WriteFile(filepath.Join(dir, "pic.png"), []byte("\x89PNG\r\n\x1a\n"), 0o644)
	field, err := xaiImageField(env, "~/pic.png")
	if url, _ := field["url"].(string); err != nil || field["type"] != "image_url" || !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatal(field, err)
	}
	h := filepath.Join(t.TempDir(), ".icebath")
	os.MkdirAll(h, 0o755)
	t.Setenv("ICEBATH_HOME", h)
	auth := filepath.Join(h, "auth.json")
	os.WriteFile(auth, []byte(`{"api_key":"sk-secret"}`), 0o600)
	if _, err := xaiImageField(&tools.Env{Home: h}, auth); err == nil || !strings.Contains(err.Error(), "credential store") {
		t.Fatal(err)
	}
}
