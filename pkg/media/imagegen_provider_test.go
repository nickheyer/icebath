package media

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Provider double whose availability and generate result tests control
type fakeImageProvider struct {
	name        string
	unavailable bool
	models      []map[string]any
	caps        map[string]any
	generate    func(req ImageGenRequest) (map[string]any, error)
	calls       []ImageGenRequest
}

func (p *fakeImageProvider) Name() string        { return p.name }
func (p *fakeImageProvider) DisplayName() string { return pystr.Title(p.name) }
func (p *fakeImageProvider) GetSetupSchema() map[string]any {
	return map[string]any{"name": p.DisplayName(), "badge": "", "tag": "", "env_vars": []any{}}
}
func (p *fakeImageProvider) IsAvailable() bool            { return !p.unavailable }
func (p *fakeImageProvider) ListModels() []map[string]any { return p.models }
func (p *fakeImageProvider) DefaultModel() string {
	if len(p.models) == 0 {
		return ""
	}
	id, _ := p.models[0]["id"].(string)
	return id
}
func (p *fakeImageProvider) Capabilities() map[string]any {
	if p.caps != nil {
		return p.caps
	}
	return defaultImageCapabilities()
}
func (p *fakeImageProvider) Generate(_ context.Context, _ *tools.Env, req ImageGenRequest) (map[string]any, error) {
	p.calls = append(p.calls, req)
	if p.generate != nil {
		return p.generate(req)
	}
	return imageSuccessResponse("https://x/y.png", "m", req.Prompt, req.AspectRatio, p.name, "", nil), nil
}

func resetImageRegistry(t *testing.T) {
	imageGenRegistry.ResetForTests()
	t.Cleanup(imageGenRegistry.ResetForTests)
}

// Environment whose home config.yaml and in memory config both hold the tree
func imageEnv(t *testing.T, cfg config.Tree) *tools.Env {
	t.Helper()
	env := &tools.Env{Home: t.TempDir()}
	setImageConfig(t, env, cfg)
	return env
}

// Rewrites the environment config on disk and in memory
func setImageConfig(t *testing.T, env *tools.Env, cfg config.Tree) {
	t.Helper()
	if err := config.Save(env.Home, cfg); err != nil {
		t.Fatal(err)
	}
	env.Config = cfg
}

// Registry holding only the given providers with plugin discovery disabled
func usePluginProviders(t *testing.T, providers ...ImageGenProvider) {
	t.Helper()
	resetImageRegistry(t)
	prev := ensurePluginsDiscoveredHook
	ensurePluginsDiscoveredHook = func(context.Context, bool) {}
	t.Cleanup(func() { ensurePluginsDiscoveredHook = prev })
	for _, p := range providers {
		if err := RegisterImageGenProvider(p, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func TestImageRegistryRejectsEmptyName(t *testing.T) {
	resetImageRegistry(t)
	if err := RegisterImageGenProvider(&fakeImageProvider{name: ""}, ""); err == nil {
		t.Fatal("empty name accepted")
	}
}

func TestImageRegistryListIsSorted(t *testing.T) {
	resetImageRegistry(t)
	_ = RegisterImageGenProvider(&fakeImageProvider{name: "zeta"}, "")
	_ = RegisterImageGenProvider(&fakeImageProvider{name: "alpha"}, "")
	var names []string
	for _, p := range ListImageGenProviders(context.Background(), "") {
		names = append(names, p.Name())
	}
	if strings.Join(names, ",") != "alpha,zeta" {
		t.Fatalf("names %v", names)
	}
}

func TestImageRegistryExplicitConfigWins(t *testing.T) {
	resetImageRegistry(t)
	env := imageEnv(t, config.Tree{"image_gen": map[string]any{"provider": "openai"}})
	_ = RegisterImageGenProvider(&fakeImageProvider{name: "fal"}, "")
	_ = RegisterImageGenProvider(&fakeImageProvider{name: "openai"}, "")
	if p := GetActiveImageGenProvider(context.Background(), env); p == nil || p.Name() != "openai" {
		t.Fatalf("active %v", p)
	}
}

func TestImageRegistryNoneWhenEmpty(t *testing.T) {
	resetImageRegistry(t)
	if p := GetActiveImageGenProvider(context.Background(), &tools.Env{Home: t.TempDir()}); p != nil {
		t.Fatalf("active %v", p)
	}
}

func TestImageRegistryFallbacks(t *testing.T) {
	resetImageRegistry(t)
	env := imageEnv(t, config.Tree{"image_gen": map[string]any{"provider": "nous"}})
	_ = RegisterImageGenProvider(&fakeImageProvider{name: "fal", unavailable: true}, "")
	_ = RegisterImageGenProvider(&fakeImageProvider{name: "openai"}, "")
	if p := GetActiveImageGenProvider(context.Background(), env); p == nil || p.Name() != "fal" {
		t.Fatalf("nous selection must map to fal even when unavailable, got %v", p)
	}
	setImageConfig(t, env, config.Tree{})
	if p := GetActiveImageGenProvider(context.Background(), env); p == nil || p.Name() != "openai" {
		t.Fatalf("single available provider, got %v", p)
	}
	_ = RegisterImageGenProvider(&fakeImageProvider{name: "xai"}, "")
	if p := GetActiveImageGenProvider(context.Background(), env); p != nil {
		t.Fatalf("two available without fal must be none, got %v", p.Name())
	}
}

func TestImageAspectAndReferenceNormalization(t *testing.T) {
	for in, want := range map[any]string{" Square ": "square", "PORTRAIT": "portrait", "wide": "landscape", 7: "landscape"} {
		if got := resolveAspectRatio(in); got != want {
			t.Fatalf("%v -> %s", in, got)
		}
	}
	if got := normalizeReferenceImages(" a "); len(got) != 1 || got[0] != "a" {
		t.Fatalf("got %v", got)
	}
	if got := normalizeReferenceImages([]any{" ", 3, "b"}); len(got) != 1 || got[0] != "b" {
		t.Fatalf("got %v", got)
	}
	if normalizeReferenceImages([]any{" "}) != nil || normalizeReferenceImages(5) != nil {
		t.Fatal("unusable refs must be nil")
	}
}

func TestImageEnvelopes(t *testing.T) {
	ok := imageSuccessResponse("img", "m", "p", "square", "fal", "", map[string]any{"success": false, "upscaled": true})
	if ok["success"] != true || ok["upscaled"] != true || ok["modality"] != "text" {
		t.Fatalf("success %v", ok)
	}
	bad := imageErrorResponse("boom", "", "fal", "", "", "")
	if bad["image"] != nil || bad["error_type"] != "provider_error" || bad["aspect_ratio"] != "landscape" || bad["success"] != false {
		t.Fatalf("error %v", bad)
	}
}

var png1px, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAAEElEQVR4AWNcDgAAAP7/AwAABgBXv6vUAAAAAElFTkSuQmCC")

// Image server for the cache download tests
func tinyImageServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/img.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(png1px)
		case "/huge.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(make([]byte, 2*1024*1024))
		case "/to-metadata/img.png":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func privateURLEnv(t *testing.T, allow bool) *tools.Env {
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", map[bool]string{true: "1", false: ""}[allow])
	return &tools.Env{Home: t.TempDir(), Config: config.Tree{}}
}

func TestSaveURLImageWritesRealBytesToHomeCache(t *testing.T) {
	srv := tinyImageServer(t)
	env := privateURLEnv(t, true)
	path, err := saveURLImage(context.Background(), env, srv.URL+"/img.png", "test", 5e9, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, filepath.Join(env.Home, "cache", "generated", "images")) || !strings.HasSuffix(path, ".png") {
		t.Fatalf("path %s", path)
	}
	if raw, _ := os.ReadFile(path); string(raw) != string(png1px) {
		t.Fatal("bytes differ")
	}
}

func TestSaveURLImage404Raises(t *testing.T) {
	srv := tinyImageServer(t)
	env := privateURLEnv(t, true)
	if _, err := saveURLImage(context.Background(), env, srv.URL+"/missing.png", "", 5e9, 0); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("got %v", err)
	}
}

func TestSaveURLImageOversizeRaisesAndCleansUp(t *testing.T) {
	srv := tinyImageServer(t)
	env := privateURLEnv(t, true)
	_, err := saveURLImage(context.Background(), env, srv.URL+"/huge.png", "", 5e9, 1024*1024)
	if err == nil || !strings.Contains(err.Error(), "exceeds 1MB cap") {
		t.Fatalf("got %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(env.Home, "cache", "generated", "images", "*"))
	if len(files) != 0 {
		t.Fatalf("partial files left %v", files)
	}
}

func TestSaveURLImageRefusesInternalTargetsBeforeConnect(t *testing.T) {
	hits := 0
	srv := tinyImageServer(t)
	counted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		srv.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(counted.Close)
	env := privateURLEnv(t, false)
	if _, err := saveURLImage(context.Background(), env, counted.URL+"/img.png", "", 5e9, 0); err == nil || hits != 0 {
		t.Fatalf("loopback fetched: %v hits %d", err, hits)
	}
	env = privateURLEnv(t, true)
	if _, err := saveURLImage(context.Background(), env, counted.URL+"/to-metadata/img.png", "", 5e9, 0); err == nil || hits != 1 {
		t.Fatalf("metadata redirect followed: %v hits %d", err, hits)
	}
	files, _ := filepath.Glob(filepath.Join(env.Home, "cache", "generated", "images", "*"))
	if len(files) != 0 {
		t.Fatal("body cached")
	}
}

func TestGeneratedDirIsMountedFromTheWritersLocation(t *testing.T) {
	env := &tools.Env{Home: t.TempDir(), Config: config.Tree{}}
	path, err := saveB64Image(env, base64.StdEncoding.EncodeToString(png1px), "red_test", "png")
	if err != nil {
		t.Fatal(err)
	}
	mounts := map[string]string{}
	for _, m := range tools.GetCacheDirectoryMounts(env, "/root/.icebath") {
		mounts[m.ContainerPath] = m.HostPath
	}
	host, ok := mounts["/root/.icebath/cache/generated"]
	if !ok {
		keys := make([]string, 0, len(mounts))
		for k := range mounts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Fatalf("mounts %v", keys)
	}
	real, _ := filepath.EvalSymlinks(host)
	if rp, _ := filepath.EvalSymlinks(path); !strings.HasPrefix(rp, real+string(filepath.Separator)) {
		t.Fatalf("%s not under %s", rp, real)
	}
}

func TestManagedModelsResolveToTheirOwnGateway(t *testing.T) {
	if managedBackendForModel(nil) != managedFal {
		t.Fatal("unset model")
	}
	for id := range falModels {
		if managedBackendForModel(id) != managedFal {
			t.Fatal(id)
		}
	}
	for id := range kreaModelIDs {
		if managedBackendForModel(id) != managedKrea {
			t.Fatal(id)
		}
	}
	if managedBackendForModel("vendor/some-portal-only-model") != managedPortal {
		t.Fatal("portal")
	}
}

func TestManagedUnionCatalogListsEachModelOnce(t *testing.T) {
	krea := &fakeImageProvider{name: "krea"}
	for id := range kreaModelIDs {
		krea.models = append(krea.models, map[string]any{"id": id, "display": id})
	}
	nous := &fakeImageProvider{name: "nous", models: []map[string]any{
		{"id": "microsoft/mai-image-2.5-pro"}, {"id": "krea/krea-2-medium"}, {"id": "google/gemini-3-pro-image"}, {"id": "vendor/portal-only"}}}
	usePluginProviders(t, krea, nous)
	order, catalog, def := managedImageCatalog(context.Background(), &tools.Env{}, true, true)
	if def != falDefaultModel || len(order) != len(catalog) {
		t.Fatalf("default %s", def)
	}
	var portal []string
	for id, meta := range catalog {
		if meta["backend"] == managedPortal {
			portal = append(portal, id)
		}
		if strings.HasPrefix(id, "fal-ai/krea/") {
			t.Fatal(id)
		}
	}
	if len(portal) != 1 || portal[0] != "vendor/portal-only" || catalog["microsoft/mai-image-2.5-pro"]["backend"] != managedFal {
		t.Fatalf("portal %v", portal)
	}
	for id := range kreaModelIDs {
		if catalog[id] == nil {
			t.Fatal(id)
		}
	}
}

func TestManagedRouteFollowsTheStoredModel(t *testing.T) {
	cases := []struct {
		provider, model any
		want            string
	}{
		{"nous", "krea-2-medium", managedKrea},
		{nil, falDefaultModel, managedFal},
		{"nous", "openai/gpt-image-2", managedPortal},
		{nil, "openai/gpt-image-2", managedFal},
		{"  ", "openai/gpt-image-2", managedFal},
		{"openai", "gpt-image-2", ""},
	}
	for _, c := range cases {
		if got := managedRoute(c.provider, c.model); got != c.want {
			t.Fatalf("%v/%v -> %q", c.provider, c.model, got)
		}
	}
}
