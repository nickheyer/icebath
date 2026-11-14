package media

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

type fakeVideoProvider struct {
	name        string
	unavailable bool
	models      []map[string]any
	caps        map[string]any
	generate    func(req VideoGenRequest) (map[string]any, error)
	mu          sync.Mutex
	calls       []VideoGenRequest
}

func (p *fakeVideoProvider) Name() string        { return p.name }
func (p *fakeVideoProvider) DisplayName() string { return pystr.Title(p.name) }
func (p *fakeVideoProvider) GetSetupSchema() map[string]any {
	return map[string]any{"name": p.DisplayName(), "badge": "", "tag": "", "env_vars": []any{}}
}
func (p *fakeVideoProvider) IsAvailable() bool            { return !p.unavailable }
func (p *fakeVideoProvider) ListModels() []map[string]any { return p.models }
func (p *fakeVideoProvider) DefaultModel() string {
	if len(p.models) == 0 {
		return ""
	}
	id, _ := p.models[0]["id"].(string)
	return id
}
func (p *fakeVideoProvider) Capabilities() map[string]any {
	if p.caps != nil {
		return p.caps
	}
	return defaultVideoCapabilities()
}
func (p *fakeVideoProvider) Generate(_ context.Context, _ *tools.Env, req VideoGenRequest) (map[string]any, error) {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	p.mu.Unlock()
	if p.generate != nil {
		return p.generate(req)
	}
	return map[string]any{"success": true, "video": p.name + "://" + req.Prompt}, nil
}

func resetVideoRegistry(t *testing.T) {
	videoGenRegistry.ResetForTests()
	t.Cleanup(videoGenRegistry.ResetForTests)
}

func TestRegisterProviderRejectsEmptyName(t *testing.T) {
	resetVideoRegistry(t)
	if err := RegisterVideoGenProvider(&fakeVideoProvider{name: ""}, ""); err == nil {
		t.Fatal("empty name accepted")
	}
}

func TestRegisterProviderListIsSorted(t *testing.T) {
	resetVideoRegistry(t)
	RegisterVideoGenProvider(&fakeVideoProvider{name: "zeta"}, "")
	RegisterVideoGenProvider(&fakeVideoProvider{name: "alpha"}, "")
	var names []string
	for _, p := range ListVideoGenProviders(context.Background(), "") {
		names = append(names, p.Name())
	}
	if !reflect.DeepEqual(names, []string{"alpha", "zeta"}) {
		t.Fatal(names)
	}
}

func TestGetActiveProviderSingleAvailableAmongManyAutoresolves(t *testing.T) {
	resetVideoRegistry(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	RegisterVideoGenProvider(&fakeVideoProvider{name: "fal", unavailable: true}, "")
	RegisterVideoGenProvider(&fakeVideoProvider{name: "xai", unavailable: true}, "")
	RegisterVideoGenProvider(&fakeVideoProvider{name: "deepinfra"}, "")
	active := GetActiveVideoGenProvider(context.Background(), nil)
	if active == nil || active.Name() != "deepinfra" {
		t.Fatal(active)
	}
}

func TestGetActiveProviderUnknownExplicitConfigFailsClosed(t *testing.T) {
	resetVideoRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := config.Save(h, config.Tree{"video_gen": map[string]any{"provider": "ghost"}}); err != nil {
		t.Fatal(err)
	}
	RegisterVideoGenProvider(&fakeVideoProvider{name: "only"}, "")
	if p := GetActiveVideoGenProvider(context.Background(), nil); p != nil {
		t.Fatal(p.Name())
	}
}

// OpenAI videos API double recording the create body, client options and download
type fakeOpenAIVideos struct {
	mu           sync.Mutex
	srv          *httptest.Server
	options      []openAIClientOptions
	create       map[string]any
	status       string
	data         []any
	download     []byte
	downloadedID string
}

// Fake videos server with the DeepInfra job shape, succeeded with one delivery URL by default
func newFakeOpenAIVideos(t *testing.T) *fakeOpenAIVideos {
	t.Helper()
	f := &fakeOpenAIVideos{status: "succeeded", data: []any{map[string]any{"url": "https://cdn.example/out.mp4"}},
		download: []byte("\x00\x00mp4bytes")}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		job := func(id string) {
			body, _ := pyjson.Dumps(map[string]any{"id": id, "status": f.status, "error": nil, "data": f.data})
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, body)
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/videos"):
			raw, _ := io.ReadAll(r.Body)
			parsed, _ := pyjson.Loads(string(raw))
			f.create, _ = pyjson.Plain(parsed).(map[string]any)
			job("vid_123")
		case strings.HasSuffix(r.URL.Path, "/content"):
			f.downloadedID = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path[strings.Index(r.URL.Path, "/videos/"):], "/videos/"), "/content")
			w.Write(f.download)
		default:
			job(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		}
	}))
	t.Cleanup(f.srv.Close)
	prev := newOpenAIClientHook
	newOpenAIClientHook = func(env *tools.Env, o openAIClientOptions) (*openAIClient, error) {
		f.mu.Lock()
		f.options = append(f.options, o)
		f.mu.Unlock()
		c, err := newOpenAIClient(env, o)
		if c != nil {
			c.BaseURL = f.srv.URL + "/v1"
		}
		return c, err
	}
	t.Cleanup(func() { newOpenAIClientHook = prev })
	return f
}

// Replaces the delivery URL download with a double that records the URL and returns a cache path
func mockURLDownload(t *testing.T, captured *string, raise error) {
	t.Helper()
	prev := saveURLVideo
	saveURLVideo = func(_ context.Context, _ *tools.Env, rawURL string, o videoURLOptions) (string, error) {
		*captured = rawURL
		if raise != nil {
			return "", raise
		}
		prefix := o.Prefix
		if prefix == "" {
			prefix = "video"
		}
		return "/home/x/.icebath/cache/videos/" + prefix + "_test.mp4", nil
	}
	t.Cleanup(func() { saveURLVideo = prev })
}

// Temp home with the DeepInfra key set and an empty catalog cache
func deepInfraVideoIsolation(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resetDeepInfraCatalogCache(t)
	t.Setenv("DEEPINFRA_API_KEY", "test-key")
	t.Setenv("DEEPINFRA_BASE_URL", "")
	os.Unsetenv("DEEPINFRA_BASE_URL")
}

func TestAvailabilityFollowsAPIKey(t *testing.T) {
	deepInfraVideoIsolation(t)
	p := newDeepInfraVideoGenProvider()
	if !p.IsAvailable() {
		t.Fatal("unavailable with key")
	}
	os.Unsetenv("DEEPINFRA_API_KEY")
	if p.IsAvailable() {
		t.Fatal("available without key")
	}
}

func TestGenerateUsesEnvOnlyProxyHTTPClient(t *testing.T) {
	deepInfraVideoIsolation(t)
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("DEEPINFRA_BASE_URL", "http://localhost:18081/v1")
	f := newFakeOpenAIVideos(t)
	var downloaded string
	mockURLDownload(t, &downloaded, nil)
	res, err := newDeepInfraVideoGenProvider().Generate(context.Background(), nil, VideoGenRequest{Prompt: "a cube", Model: "vendor/x"})
	if err != nil || res["success"] != true {
		t.Fatal(res, err)
	}
	o := f.options[0]
	if o.BaseURL == nil || *o.BaseURL != "http://localhost:18081/v1" {
		t.Fatal(o.BaseURL)
	}
	want := provider.BuildKeepaliveHTTPClient("http://localhost:18081/v1", false, provider.TLSVerify{})
	if o.HTTPClient == nil || reflect.TypeOf(o.HTTPClient.Transport) != reflect.TypeOf(want.Transport) {
		t.Fatalf("handed %v, keepalive client %v", o.HTTPClient, want)
	}
	if tr, ok := o.HTTPClient.Transport.(*http.Transport); ok && tr.Proxy != nil {
		target, _ := url.Parse("http://localhost:18081/v1/videos")
		if px, _ := tr.Proxy(&http.Request{URL: target}); px != nil {
			t.Fatal("proxy mounted for the custom endpoint", px)
		}
	}
}

func TestGenerateTextToVideoDownloadsURLAndSavesLocally(t *testing.T) {
	deepInfraVideoIsolation(t)
	f := newFakeOpenAIVideos(t)
	var downloaded string
	mockURLDownload(t, &downloaded, nil)
	five := 5
	res, err := newDeepInfraVideoGenProvider().Generate(context.Background(), nil,
		VideoGenRequest{Prompt: "a red cube rotating", Model: "vendor/test-vid", Duration: &five})
	if err != nil || res["success"] != true || res["modality"] != "text" {
		t.Fatal(res, err)
	}
	video, _ := res["video"].(string)
	if !strings.HasSuffix(video, ".mp4") || !strings.Contains(video, filepath.Join("cache", "videos")) {
		t.Fatal(video)
	}
	if downloaded != "https://cdn.example/out.mp4" {
		t.Fatal(downloaded)
	}
	o := f.options[0]
	if !strings.Contains(*o.BaseURL, "deepinfra") || *o.APIKey != "test-key" {
		t.Fatal(*o.BaseURL, *o.APIKey)
	}
	if f.create["model"] != "vendor/test-vid" || f.create["seconds"] != "5" {
		t.Fatal(f.create)
	}
	if _, ok := f.create["image_url"]; ok {
		t.Fatal("image_url sent for text-to-video", f.create)
	}
}

func TestCredentialsFollowTheProfileSecretScope(t *testing.T) {
	deepInfraVideoIsolation(t)
	t.Setenv("DEEPINFRA_BASE_URL", "https://launch.example/v1")
	p := newDeepInfraVideoGenProvider()
	f := newFakeOpenAIVideos(t)
	var downloaded string
	mockURLDownload(t, &downloaded, nil)
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	ctx := provider.WithSecretScope(context.Background(),
		map[string]string{"DEEPINFRA_API_KEY": "profile-b-key", "DEEPINFRA_BASE_URL": "https://profile-b.example/v1"}, "")
	res, err := p.Generate(ctx, nil, VideoGenRequest{Prompt: "a cube", Model: "vendor/x"})
	if err != nil || res["success"] != true {
		t.Fatal(res, err)
	}
	if p.keyAvailable(provider.WithSecretScope(context.Background(), map[string]string{}, "")) {
		t.Fatal("empty profile scope borrowed the launch key")
	}
	o := f.options[0]
	if *o.APIKey != "profile-b-key" || *o.BaseURL != "https://profile-b.example/v1" {
		t.Fatal(*o.APIKey, *o.BaseURL)
	}
}
