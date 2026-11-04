package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

func jsonMap(raw string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		panic(err)
	}
	return m
}

func sampleRegistry() map[string]any {
	return jsonMap(`{
	 "anthropic": {"id": "anthropic", "name": "Anthropic", "models": {
	   "claude-opus-4-6": {"id": "claude-opus-4-6", "limit": {"context": 1000000, "output": 128000}},
	   "claude-sonnet-4-6": {"id": "claude-sonnet-4-6", "limit": {"context": 1000000, "output": 64000}},
	   "claude-sonnet-4-0": {"id": "claude-sonnet-4-0", "limit": {"context": 200000, "output": 64000}}}},
	 "github-copilot": {"id": "github-copilot", "name": "GitHub Copilot", "models": {
	   "claude-opus-4.6": {"id": "claude-opus-4.6", "limit": {"context": 128000, "output": 32000}}}},
	 "xai": {"id": "xai", "name": "xAI", "models": {
	   "grok-build-0.1": {"id": "grok-build-0.1", "limit": {"context": 256000, "output": 64000}}}},
	 "kilo": {"id": "kilo", "name": "Kilo Gateway", "models": {
	   "anthropic/claude-sonnet-4.6": {"id": "anthropic/claude-sonnet-4.6", "limit": {"context": 1000000, "output": 128000}}}},
	 "deepseek": {"id": "deepseek", "name": "DeepSeek", "models": {
	   "deepseek-chat": {"id": "deepseek-chat", "limit": {"context": 128000, "output": 8192}}}},
	 "audio-only": {"id": "audio-only", "models": {
	   "tts-model": {"id": "tts-model", "limit": {"context": 0, "output": 0}}}}}`)
}

func capsRegistry() map[string]any {
	return jsonMap(`{
	 "google": {"id": "google", "models": {
	   "gemma-4-31b-it": {"id": "gemma-4-31b-it", "attachment": false, "tool_call": true, "modalities": {"input": ["text", "image"]}, "limit": {"context": 128000, "output": 8192}},
	   "gemma-3-1b": {"id": "gemma-3-1b", "tool_call": true, "limit": {"context": 32000, "output": 8192}},
	   "text-only-with-stale-attachment": {"id": "text-only-with-stale-attachment", "attachment": true, "tool_call": true, "modalities": {"input": ["text"]}, "limit": {"context": 128000, "output": 8192}}}},
	 "anthropic": {"id": "anthropic", "models": {
	   "claude-sonnet-4": {"id": "claude-sonnet-4", "attachment": true, "tool_call": true, "limit": {"context": 200000, "output": 64000}}}}}`)
}

type modelsDevFixture struct {
	t         *testing.T
	home      string
	ctx       context.Context
	mu        sync.Mutex
	calls     []map[string]string
	urls      []string
	responder func(headers map[string]string) (int, any, string, error)
	logs      *bytes.Buffer
}

// Isolated home with reset registry state, a recording download seam and a captured log
func newModelsDevFixture(t *testing.T) *modelsDevFixture {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv(home.EnvHome, tmp)
	f := &modelsDevFixture{t: t, home: tmp, ctx: home.WithOverride(context.Background(), tmp), logs: &bytes.Buffer{}}
	ResetModelsDevCache()
	config.ResetCache()
	prevDownload, prevClock, prevLog := downloadModelsDev, modelsDevClock, modelsDevLog
	modelsDevLog = slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	downloadModelsDev = func(ctx context.Context, url string, headers map[string]string) (int, any, string, error) {
		f.mu.Lock()
		f.calls = append(f.calls, headers)
		f.urls = append(f.urls, url)
		responder := f.responder
		f.mu.Unlock()
		if responder == nil {
			t.Errorf("unexpected network download of %s", url)
			return 0, nil, "", errors.New("network")
		}
		return responder(headers)
	}
	t.Cleanup(func() {
		WaitModelsDevRefresh()
		downloadModelsDev, modelsDevClock, modelsDevLog = prevDownload, prevClock, prevLog
		ResetModelsDevCache()
	})
	return f
}

func (f *modelsDevFixture) respond(status int, data any, etag string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responder = func(map[string]string) (int, any, string, error) { return status, data, etag, err }
}

func (f *modelsDevFixture) downloads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *modelsDevFixture) setMemory(reg map[string]any, at time.Time) {
	modelsDevStateMu.Lock()
	defer modelsDevStateMu.Unlock()
	modelsDevCache = reg
	modelsDevCacheTime = at
}

func (f *modelsDevFixture) writeDisk(reg map[string]any, age time.Duration) {
	f.t.Helper()
	path := modelsDevCachePath(f.home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatal(err)
	}
	raw, _ := json.Marshal(reg)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		f.t.Fatal(err)
	}
	if age != 0 {
		at := time.Now().Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *modelsDevFixture) writeConfig(yaml string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.home, "config.yaml"), []byte(yaml), 0o600); err != nil {
		f.t.Fatal(err)
	}
	config.ResetCache()
}

func modelsDevState() (map[string]any, time.Time, time.Time) {
	modelsDevStateMu.Lock()
	defer modelsDevStateMu.Unlock()
	return modelsDevCache, modelsDevCacheTime, modelsDevRetryAfter
}

func TestProviderMappingXAIOAuthUsesXAICatalog(t *testing.T) {
	if providerToModelsDev["xai"] != "xai" || providerToModelsDev["xai-oauth"] != "xai" {
		t.Fatal("xai mapping")
	}
	if id, ok := ModelsDevIDFor("alibaba-cn"); !ok || id != "alibaba" {
		t.Fatalf("profile declared id: %q %v", id, ok)
	}
	if _, ok := ModelsDevIDFor("solstice"); ok {
		t.Fatal("a profile without a declared models.dev id must not map")
	}
}

func TestExtractContext(t *testing.T) {
	if extractModelsDevContext(map[string]any{"limit": map[string]any{"context": float64(128000)}}) != 128000 {
		t.Fatal("valid entry")
	}
	if extractModelsDevLimit("not a dict", "context") != 0 {
		t.Fatal("non dict")
	}
	if extractModelsDevLimit(map[string]any{"limit": map[string]any{"context": true}}, "context") != 0 {
		t.Fatal("bool is not a limit")
	}
}

func TestLookupModelsDevContext(t *testing.T) {
	f := newModelsDevFixture(t)
	f.setMemory(sampleRegistry(), time.Now())
	if n, ok := LookupModelsDevContext(f.ctx, "anthropic", "claude-opus-4-6", false); !ok || n != 1000000 {
		t.Fatalf("exact match %d %v", n, ok)
	}
	tts := sampleRegistry()["audio-only"].(map[string]any)["models"].(map[string]any)["tts-model"].(map[string]any)
	if extractModelsDevContext(tts) != 0 {
		t.Fatal("zero context must be filtered")
	}
	if lookupModelsDevContext(f.ctx, "anthropic", "nope") != 0 {
		t.Fatal("unknown model has no context")
	}
}

func TestStaleDiskCacheReturnsWithoutForegroundNetwork(t *testing.T) {
	f := newModelsDevFixture(t)
	f.writeDisk(sampleRegistry(), modelsDevCacheTTL+time.Minute)
	started := make(chan struct{})
	release := make(chan struct{})
	f.mu.Lock()
	f.responder = func(map[string]string) (int, any, string, error) {
		close(started)
		<-release
		return 200, sampleRegistry(), "", nil
	}
	f.mu.Unlock()
	result := FetchModelsDev(f.ctx, false)
	if _, ok := result["anthropic"]; !ok {
		t.Fatal("stale disk cache not served")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("background refresh never started")
	}
	close(release)
	WaitModelsDevRefresh()
	if f.downloads() != 1 {
		t.Fatalf("downloads %d", f.downloads())
	}
}

func TestStaleCacheFailureEntersBackoffAndSuppressesRetry(t *testing.T) {
	f := newModelsDevFixture(t)
	f.respond(0, nil, "", errors.New("models.dev unreachable"))
	f.setMemory(sampleRegistry(), time.Now().Add(-modelsDevCacheTTL-time.Second))
	first := FetchModelsDev(f.ctx, false)
	WaitModelsDevRefresh()
	if !reflect.DeepEqual(first, sampleRegistry()) {
		t.Fatal("stale memory not served")
	}
	_, _, retryAfter := modelsDevState()
	if !retryAfter.After(time.Now()) || f.downloads() != 1 {
		t.Fatalf("backoff not armed: %v downloads %d", retryAfter, f.downloads())
	}
	modelsDevRefreshMu.Lock()
	inflight := modelsDevRefreshInFlight
	modelsDevRefreshMu.Unlock()
	if inflight {
		t.Fatal("refresh still marked in flight")
	}
	f.setMemory(sampleRegistry(), time.Now().Add(-modelsDevCacheTTL-time.Second))
	second := FetchModelsDev(f.ctx, false)
	WaitModelsDevRefresh()
	if !reflect.DeepEqual(second, sampleRegistry()) || f.downloads() != 1 {
		t.Fatalf("inside the backoff window a second refresh ran: %d", f.downloads())
	}
}

func TestBackgroundRefreshSuccessCommitsRegistry(t *testing.T) {
	f := newModelsDevFixture(t)
	f.respond(200, sampleRegistry(), `"abc123"`, nil)
	f.setMemory(map[string]any{"stale": map[string]any{}}, time.Time{})
	modelsDevStateMu.Lock()
	modelsDevRetryAfter = time.Now().Add(-time.Second)
	modelsDevStateMu.Unlock()
	modelsDevRefreshMu.Lock()
	modelsDevRefreshInFlight = true
	modelsDevRefreshMu.Unlock()
	modelsDevRefreshWait.Add(1)
	backgroundModelsDevRefresh(f.home)
	cache, cacheTime, retryAfter := modelsDevState()
	if !reflect.DeepEqual(cache, sampleRegistry()) || cacheTime.IsZero() || !retryAfter.IsZero() {
		t.Fatalf("commit state wrong: %v %v", cacheTime, retryAfter)
	}
	if got := loadModelsDevETag(f.home); got != `"abc123"` {
		t.Fatalf("etag sidecar %q", got)
	}
	if disk := loadModelsDevDiskCache(f.home); !reflect.DeepEqual(disk, sampleRegistry()) {
		t.Fatal("disk cache not written")
	}
	modelsDevRefreshMu.Lock()
	inflight := modelsDevRefreshInFlight
	modelsDevRefreshMu.Unlock()
	if inflight {
		t.Fatal("in flight flag not cleared")
	}
}

func TestConcurrentRefreshesShareOneNetworkRequest(t *testing.T) {
	f := newModelsDevFixture(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	f.mu.Lock()
	f.responder = func(map[string]string) (int, any, string, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return 200, sampleRegistry(), "", nil
	}
	f.mu.Unlock()
	results := make([]map[string]any, 6)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = FetchModelsDev(f.ctx, false)
		}(i)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request never started")
	}
	close(release)
	wg.Wait()
	for _, r := range results {
		if !reflect.DeepEqual(r, sampleRegistry()) {
			t.Fatalf("result %v", r)
		}
	}
	if f.downloads() != 1 {
		t.Fatalf("downloads %d", f.downloads())
	}
}

func TestForceRefreshBypassesFailureBackoff(t *testing.T) {
	f := newModelsDevFixture(t)
	attempt := 0
	f.mu.Lock()
	f.responder = func(map[string]string) (int, any, string, error) {
		attempt++
		if attempt == 1 {
			return 0, nil, "", errors.New("models.dev unreachable")
		}
		return 200, sampleRegistry(), "", nil
	}
	f.mu.Unlock()
	if got := FetchModelsDev(f.ctx, false); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if got := FetchModelsDev(f.ctx, true); !reflect.DeepEqual(got, sampleRegistry()) {
		t.Fatalf("force refresh %v", got)
	}
	_, _, retryAfter := modelsDevState()
	if f.downloads() != 2 || !retryAfter.IsZero() {
		t.Fatalf("downloads %d retry %v", f.downloads(), retryAfter)
	}
}

func TestNetworkDisabledNeverFetches(t *testing.T) {
	cases := []struct {
		name     string
		cache    map[string]any
		at       time.Time
		expected map[string]any
	}{
		{"fresh-memory", sampleRegistry(), time.Now(), sampleRegistry()},
		{"stale-memory", sampleRegistry(), time.Now().Add(-modelsDevCacheTTL - time.Second), sampleRegistry()},
		{"missing", map[string]any{}, time.Time{}, map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newModelsDevFixture(t)
			f.setMemory(tc.cache, tc.at)
			got := ModelsDevOffline(f.ctx)
			if len(got) != len(tc.expected) || (len(got) > 0 && !reflect.DeepEqual(got, tc.expected)) {
				t.Fatalf("got %v", got)
			}
			if f.downloads() != 0 {
				t.Fatal("network touched")
			}
		})
	}
}

func TestETagSentWhenCached(t *testing.T) {
	f := newModelsDevFixture(t)
	f.respond(200, sampleRegistry(), `"v2"`, nil)
	f.setMemory(sampleRegistry(), time.Time{})
	saveModelsDevETag(f.home, `"v1"`)
	FetchModelsDev(f.ctx, true)
	if f.calls[0]["If-None-Match"] != `"v1"` {
		t.Fatalf("headers %v", f.calls[0])
	}
	if got := loadModelsDevETag(f.home); got != `"v2"` {
		t.Fatalf("new etag %q", got)
	}
}

func Test304ReconfirmsCacheFreshness(t *testing.T) {
	f := newModelsDevFixture(t)
	f.respond(304, nil, "", nil)
	f.setMemory(sampleRegistry(), time.Time{})
	modelsDevStateMu.Lock()
	modelsDevRetryAfter = time.Now().Add(100 * time.Second)
	modelsDevStateMu.Unlock()
	saveModelsDevETag(f.home, `"v1"`)
	modelsDevRefreshMu.Lock()
	modelsDevRefreshInFlight = true
	modelsDevRefreshMu.Unlock()
	modelsDevRefreshWait.Add(1)
	backgroundModelsDevRefresh(f.home)
	cache, cacheTime, retryAfter := modelsDevState()
	if !reflect.DeepEqual(cache, sampleRegistry()) || cacheTime.IsZero() || !retryAfter.IsZero() {
		t.Fatalf("state after 304: %v %v", cacheTime, retryAfter)
	}
}

func TestForeground304ReturnsExistingCache(t *testing.T) {
	f := newModelsDevFixture(t)
	f.respond(304, nil, "", nil)
	f.setMemory(sampleRegistry(), time.Time{})
	saveModelsDevETag(f.home, `"v1"`)
	if got := FetchModelsDev(f.ctx, true); !reflect.DeepEqual(got, sampleRegistry()) {
		t.Fatalf("got %v", got)
	}
	if _, cacheTime, _ := modelsDevState(); cacheTime.IsZero() {
		t.Fatal("freshness not advanced")
	}
}

func TestNoETagHeaderSentWithoutCachedETag(t *testing.T) {
	f := newModelsDevFixture(t)
	f.respond(200, sampleRegistry(), "", nil)
	FetchModelsDev(f.ctx, false)
	if _, ok := f.calls[0]["If-None-Match"]; ok {
		t.Fatalf("headers %v", f.calls[0])
	}
}

func TestValidateRegistry(t *testing.T) {
	if validateModelsDevRegistry(map[string]any{}) || validateModelsDevRegistry("not a dict") || validateModelsDevRegistry(nil) || validateModelsDevRegistry([]any{}) {
		t.Fatal("invalid registry accepted")
	}
	if !validateModelsDevRegistry(map[string]any{"anthropic": map[string]any{}}) {
		t.Fatal("populated registry rejected")
	}
}

func TestCorruptJSONOnDiskRejectedWithWarning(t *testing.T) {
	f := newModelsDevFixture(t)
	path := modelsDevCachePath(f.home)
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("not json{{{"), 0o600)
	if got := loadModelsDevDiskCache(f.home); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if !strings.Contains(f.logs.String(), "disk cache") {
		t.Fatalf("no warning: %s", f.logs.String())
	}
}

func TestEmptyDictOnDiskRejectedWithWarning(t *testing.T) {
	f := newModelsDevFixture(t)
	path := modelsDevCachePath(f.home)
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("{}"), 0o600)
	if got := loadModelsDevDiskCache(f.home); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if !strings.Contains(f.logs.String(), "corrupt or empty") {
		t.Fatalf("no warning: %s", f.logs.String())
	}
}

func TestCorruptCacheClearsETagSidecar(t *testing.T) {
	f := newModelsDevFixture(t)
	path := modelsDevCachePath(f.home)
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("corrupt!!"), 0o600)
	saveModelsDevETag(f.home, "stale-etag")
	if got := loadModelsDevDiskCache(f.home); len(got) != 0 {
		t.Fatal("served corrupt cache")
	}
	if _, err := os.Stat(modelsDevETagPath(f.home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("etag sidecar survived")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("corrupt cache not quarantined")
	}
	if _, err := os.Stat(strings.TrimSuffix(path, ".json") + ".json.corrupt"); err != nil {
		t.Fatal("quarantine file missing")
	}
}

func TestConditionalGetSkippedWithoutServableCache(t *testing.T) {
	f := newModelsDevFixture(t)
	f.respond(200, map[string]any{"anthropic": map[string]any{"models": map[string]any{}}}, "fresh", nil)
	saveModelsDevETag(f.home, "stale-etag")
	f.setMemory(map[string]any{}, time.Time{})
	data, etag, err := fetchModelsDevFromNetwork(f.home, false)
	if err != nil || etag != "fresh" || !reflect.DeepEqual(data, map[string]any{"anthropic": map[string]any{"models": map[string]any{}}}) {
		t.Fatalf("%v %q %v", data, etag, err)
	}
	if _, ok := f.calls[0]["If-None-Match"]; ok {
		t.Fatal("conditional header sent without a servable cache")
	}
}

func Test304WithEmptyCacheArmsBackoffAndClearsETag(t *testing.T) {
	f := newModelsDevFixture(t)
	saveModelsDevETag(f.home, "stale")
	f.setMemory(map[string]any{}, time.Time{})
	confirmModelsDevNotModified(f.home, "test")
	if _, err := os.Stat(modelsDevETagPath(f.home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("etag sidecar survived")
	}
	if _, _, retryAfter := modelsDevState(); !retryAfter.After(time.Now().Add(-time.Second)) {
		t.Fatal("backoff not armed")
	}
}

func TestMirrorURLUsedWhenConfigured(t *testing.T) {
	f := newModelsDevFixture(t)
	f.writeConfig("models_dev:\n  url: https://mirror.example.com/api.json\n")
	f.respond(200, sampleRegistry(), "", nil)
	FetchModelsDev(f.ctx, false)
	if !strings.Contains(f.urls[0], "mirror.example.com") {
		t.Fatalf("url %s", f.urls[0])
	}
}

func TestEmptyURLFallsBackToDefault(t *testing.T) {
	f := newModelsDevFixture(t)
	f.writeConfig("models_dev:\n  url: \"\"\n")
	f.respond(200, sampleRegistry(), "", nil)
	FetchModelsDev(f.ctx, false)
	if !strings.Contains(f.urls[0], "models.dev") {
		t.Fatalf("url %s", f.urls[0])
	}
}

func TestFetchDeadlineAbandonsATricklingRegistry(t *testing.T) {
	f := newModelsDevFixture(t)
	f.mu.Lock()
	f.responder = func(map[string]string) (int, any, string, error) {
		time.Sleep(200 * time.Millisecond)
		return 200, sampleRegistry(), "", nil
	}
	f.mu.Unlock()
	prevDeadline := modelsDevFetchDeadlineFor
	modelsDevFetchDeadlineFor = func() time.Duration { return 20 * time.Millisecond }
	t.Cleanup(func() { modelsDevFetchDeadlineFor = prevDeadline })
	started := time.Now()
	_, _, err := fetchModelsDevFromNetwork(f.home, false)
	var deadline *ModelsDevFetchDeadlineError
	if !errors.As(err, &deadline) || time.Since(started) > 150*time.Millisecond {
		t.Fatalf("err %v after %v", err, time.Since(started))
	}
	if _, _, err := fetchModelsDevFromNetwork(f.home, false); err == nil || !strings.Contains(err.Error(), "abandoned") {
		t.Fatalf("second attempt must fail fast while the slot is held: %v", err)
	}
}

func TestHotPathsNeverFetch(t *testing.T) {
	f := newModelsDevFixture(t)
	f.setMemory(capsRegistry(), time.Now())
	ModelsDevCapabilities(f.ctx, "anthropic", "claude-sonnet-4", false)
	ModelsDevProfile(f.ctx, "anthropic", "claude-sonnet-4")
	LookupModelsDevContext(f.ctx, "anthropic", "claude-sonnet-4", false)
	if f.downloads() != 0 {
		t.Fatal("hot path touched the network")
	}
}

func boolPtrEq(p *bool, want bool) bool { return p != nil && *p == want }

func TestVisionFromAttachmentFlag(t *testing.T) {
	f := newModelsDevFixture(t)
	f.setMemory(capsRegistry(), time.Now())
	caps, ok := ModelsDevCapabilities(f.ctx, "anthropic", "claude-sonnet-4", false)
	if !ok || !boolPtrEq(caps.SupportsVision, true) {
		t.Fatalf("%+v", caps)
	}
	gemma, _ := ModelsDevCapabilities(f.ctx, "gemini", "gemma-4-31b-it", false)
	if !boolPtrEq(gemma.SupportsVision, true) {
		t.Fatal("modalities must beat the attachment flag")
	}
	stale, _ := ModelsDevCapabilities(f.ctx, "google", "text-only-with-stale-attachment", false)
	if !boolPtrEq(stale.SupportsVision, false) {
		t.Fatal("stale attachment flag must lose to explicit modalities")
	}
}

func TestModalitiesNonDictHandled(t *testing.T) {
	f := newModelsDevFixture(t)
	f.setMemory(jsonMap(`{"google": {"id": "google", "models": {"weird-model": {"id": "weird-model", "modalities": "text", "limit": {"context": 200000, "output": 8192}}}}}`), time.Now())
	caps, ok := ModelsDevCapabilities(f.ctx, "gemini", "weird-model", false)
	if !ok || !boolPtrEq(caps.SupportsVision, false) {
		t.Fatalf("%+v %v", caps, ok)
	}
}

func TestMetadataOnlyOverrideOnCustomProviderLeavesCapabilitiesUnknown(t *testing.T) {
	f := newModelsDevFixture(t)
	cfg := map[string]any{"model_overrides": map[string]any{"925llm": map[string]any{"deepseek-v4.1-flash": map[string]any{"context_window": 1_000_000}}}}
	caps, ok := modelsDevCapabilitiesIn(f.home, "925llm", "deepseek-v4.1-flash", false, cfg)
	info, infoOK := modelsDevModelInfoIn(f.home, "925llm", "deepseek-v4.1-flash", false, cfg)
	if !ok || caps.ContextWindow != 1_000_000 || caps.MaxOutputTokens != 0 || caps.SupportsVision != nil || caps.SupportsReasoning != nil {
		t.Fatalf("%+v", caps)
	}
	if !infoOK || info.MaxOutput != 0 {
		t.Fatalf("%+v", info)
	}
}

func TestCatalogProviderAliasReachesBuiltinAndCatalogMetadata(t *testing.T) {
	f := newModelsDevFixture(t)
	cfg := map[string]any{"providers": map[string]any{"925llm": map[string]any{"api": "http://gw.internal/v1", "catalog_provider": "deepseek"}}}
	f.setMemory(jsonMap(`{"deepseek": {"id": "deepseek", "models": {"deepseek-chat": {"id": "deepseek-chat", "limit": {"context": 128000, "output": 8000}, "tool_call": true, "reasoning": false, "modalities": {"input": ["text"], "output": ["text"]}}}}}`), time.Now())
	builtin, ok := modelsDevCapabilitiesIn(f.home, "925llm", "deepseek-v4.1-flash", false, cfg)
	prefixed, _ := modelsDevCapabilitiesIn(f.home, "custom:925llm", "deepseek-v4.1-flash", false, cfg)
	catalog, _ := modelsDevCapabilitiesIn(f.home, "925llm", "deepseek-chat", false, cfg)
	info, _ := modelsDevModelInfoIn(f.home, "925llm", "deepseek-chat", false, cfg)
	if !ok || !boolPtrEq(builtin.SupportsVision, true) || !reflect.DeepEqual(prefixed, builtin) {
		t.Fatalf("builtin %+v prefixed %+v", builtin, prefixed)
	}
	if !boolPtrEq(catalog.SupportsVision, false) || catalog.MaxOutputTokens != 8000 {
		t.Fatalf("catalog %+v", catalog)
	}
	if info.ProviderID != "deepseek" || info.ContextWindow != 128000 {
		t.Fatalf("info %+v", info)
	}
	f.writeConfig("providers:\n  925llm:\n    api: http://gw.internal/v1\n    catalog_provider: deepseek\n")
	if n, ok := LookupModelsDevContext(f.ctx, "925llm", "deepseek-chat", false); !ok || n != 128000 {
		t.Fatalf("ctx %d %v", n, ok)
	}
}

func TestMistypedAliasWarnsOnceAndKeepsTheConfiguredSlug(t *testing.T) {
	f := newModelsDevFixture(t)
	cfg := map[string]any{
		"providers":       map[string]any{"925llm": map[string]any{"api": "http://gw.internal/v1", "catalog_provider": "deepsek"}},
		"model_overrides": map[string]any{"925llm": map[string]any{"deepseek-v4.1-flash": map[string]any{"context_window": 1000000}}},
	}
	f.setMemory(jsonMap(`{"deepseek": {"models": {}}}`), time.Now())
	info, ok := modelsDevModelInfoIn(f.home, "925llm", "deepseek-v4.1-flash", false, cfg)
	caps, _ := modelsDevCapabilitiesIn(f.home, "925llm", "deepseek-v4.1-flash", false, cfg)
	modelsDevModelInfoIn(f.home, "925llm", "deepseek-v4.1-flash", false, cfg)
	if !ok || info.ProviderID != "925llm" || caps.SupportsVision != nil {
		t.Fatalf("%+v %+v", info, caps)
	}
	warned := strings.Count(f.logs.String(), "catalog_provider")
	if warned != 1 || !strings.Contains(f.logs.String(), "deepsek") || !strings.Contains(f.logs.String(), "925llm") {
		t.Fatalf("warnings %d: %s", warned, f.logs.String())
	}
}

func TestWithoutAliasCustomProviderStaysUnknown(t *testing.T) {
	f := newModelsDevFixture(t)
	cfg := map[string]any{
		"providers":        map[string]any{"925llm": map[string]any{"api": "http://gw.internal/v1"}},
		"custom_providers": []any{map[string]any{"name": "legacy-gw", "base_url": "http://x/v1", "catalog_provider": "deepseek"}},
	}
	if _, ok := modelsDevCapabilitiesIn(f.home, "925llm", "deepseek-v4.1-flash", false, cfg); ok {
		t.Fatal("unaliased custom provider inherited vendor metadata")
	}
	legacy, ok := modelsDevCapabilitiesIn(f.home, "legacy-gw", "deepseek-v4.1-flash", false, cfg)
	if !ok || !boolPtrEq(legacy.SupportsVision, true) {
		t.Fatalf("%+v %v", legacy, ok)
	}
}

func overridesCfg(overrides map[string]any) map[string]any {
	return map[string]any{"model_overrides": overrides}
}

func TestExplicitModelOverrideResolution(t *testing.T) {
	cfg := overridesCfg(map[string]any{"upstage": map[string]any{"solar-pro4": map[string]any{"context_window": 524288}}})
	if got := explicitModelOverride("upstage", "solar-pro4", cfg); got == nil || got["context_window"] != 524288 {
		t.Fatalf("%v", got)
	}
	cfg = overridesCfg(map[string]any{"upstage": map[string]any{"Solar-Pro4": map[string]any{"context_window": 524288}}})
	if got := explicitModelOverride("upstage", "solar-pro4", cfg); got == nil || got["context_window"] != 524288 {
		t.Fatalf("case insensitive %v", got)
	}
	cfg = overridesCfg(map[string]any{"fireworks": map[string]any{"my-model": map[string]any{"context_window": 111111}}})
	if got := explicitModelOverride("fireworks-ai", "my-model", cfg); got == nil || got["context_window"] != 111111 {
		t.Fatalf("models.dev id caller %v", got)
	}
	cfg = overridesCfg(map[string]any{"fireworks-ai": map[string]any{"my-model": map[string]any{"context_window": 222222}}})
	if got := explicitModelOverride("fireworks", "my-model", cfg); got == nil || got["context_window"] != 222222 {
		t.Fatalf("hermes id caller %v", got)
	}
	if explicitModelOverride("anthropic", "claude-sonnet-4", overridesCfg(map[string]any{})) != nil || defaultModelOverride("anthropic", overridesCfg(map[string]any{})) != nil {
		t.Fatal("empty overrides produced a value")
	}
}

func TestDefaultOverrideSemantics(t *testing.T) {
	cfg := overridesCfg(map[string]any{"upstage": map[string]any{"_default": map[string]any{"context_window": 128000}}})
	if got := overrideFor("upstage", "unknown-model", false, cfg); got == nil || got["context_window"] != 128000 {
		t.Fatalf("fill gap %v", got)
	}
	cfg = overridesCfg(map[string]any{"upstage": map[string]any{"_default": map[string]any{"context_window": 128000}}, "_default": map[string]any{"context_window": 65536}})
	if overrideFor("upstage", "known-model", true, cfg) != nil {
		t.Fatal("default clamped a catalog known model")
	}
	if got := defaultModelOverride("unknown-provider", overridesCfg(map[string]any{"_default": map[string]any{"context_window": 65536}})); got == nil || got["context_window"] != 65536 {
		t.Fatalf("global default %v", got)
	}
	cfg = overridesCfg(map[string]any{"upstage": map[string]any{"solar-pro4": map[string]any{"context_window": 524288}, "_default": map[string]any{"context_window": 128000}}})
	if got := overrideFor("upstage", "solar-pro4", false, cfg); got["context_window"] != 524288 {
		t.Fatalf("explicit beats default %v", got)
	}
	cfg = overridesCfg(map[string]any{"upstage": map[string]any{"_default": map[string]any{"context_window": 128000}}, "_default": map[string]any{"context_window": 65536}})
	if got := defaultModelOverride("upstage", cfg); got["context_window"] != 128000 {
		t.Fatalf("per provider default beats global %v", got)
	}
}

func TestOverrideContextWindow(t *testing.T) {
	f := newModelsDevFixture(t)
	if n, ok := overrideContextWindow("upstage", "syn-pro", overridesCfg(map[string]any{"upstage": map[string]any{"syn-pro": map[string]any{"context_window": 65536}}})); !ok || n != 65536 {
		t.Fatalf("%d %v", n, ok)
	}
	if _, ok := overrideContextWindow("upstage", "syn-pro", overridesCfg(map[string]any{})); ok {
		t.Fatal("missing returned a value")
	}
	if _, ok := overrideContextWindow("upstage", "bad-model", overridesCfg(map[string]any{"upstage": map[string]any{"bad-model": map[string]any{"context_window": 0}}})); ok {
		t.Fatal("zero accepted")
	}
	if _, ok := overrideContextWindow("upstage", "syn-pro", overridesCfg(map[string]any{"upstage": map[string]any{"_default": map[string]any{"context_window": 128000}}})); ok {
		t.Fatal("default preempted the explicit only lookup")
	}
	bad := overridesCfg(map[string]any{"upstage": map[string]any{"bad-model": map[string]any{"context_window": "512k"}}})
	overrideContextWindow("upstage", "bad-model", bad)
	overrideContextWindow("upstage", "bad-model", bad)
	if strings.Count(f.logs.String(), "512k") != 1 {
		t.Fatalf("warnings: %s", f.logs.String())
	}
	f.writeConfig("model_overrides:\n  upstage:\n    solar-pro4:\n      context_window: 524288\n")
	if n, ok := OverrideContextWindow(f.ctx, "upstage", "solar-pro4"); !ok || n != 524288 {
		t.Fatalf("real config %d %v", n, ok)
	}
	if n, ok := LookupModelsDevContext(f.ctx, "upstage", "solar-pro4", false); !ok || n != 524288 {
		t.Fatalf("e2e config %d %v", n, ok)
	}
}

func TestCapabilitiesWithOverrides(t *testing.T) {
	f := newModelsDevFixture(t)
	cfg := overridesCfg(map[string]any{"custom:my-vllm": map[string]any{"my-llava-model": map[string]any{"context_window": 8192, "supports_vision": true, "supports_reasoning": false, "supports_tools": true}}})
	caps, ok := modelsDevCapabilitiesIn(f.home, "custom:my-vllm", "my-llava-model", false, cfg)
	if !ok || caps.ContextWindow != 8192 || !boolPtrEq(caps.SupportsVision, true) || !boolPtrEq(caps.SupportsReasoning, false) || !caps.SupportsTools {
		t.Fatalf("unknown model override %+v", caps)
	}
	cfg = overridesCfg(map[string]any{"custom-gateway": map[string]any{"upstream-model": map[string]any{"context_window": 1_000_000}, "text-model": map[string]any{"context_window": 1_000_000, "supports_vision": false, "supports_reasoning": false}}})
	unknown, _ := modelsDevCapabilitiesIn(f.home, "custom-gateway", "upstream-model", false, cfg)
	explicit, _ := modelsDevCapabilitiesIn(f.home, "custom-gateway", "text-model", false, cfg)
	if unknown.ContextWindow != 1_000_000 || unknown.SupportsVision != nil || unknown.SupportsReasoning != nil {
		t.Fatalf("context only %+v", unknown)
	}
	if !boolPtrEq(explicit.SupportsVision, false) || !boolPtrEq(explicit.SupportsReasoning, false) {
		t.Fatalf("explicit false %+v", explicit)
	}
	f.setMemory(capsRegistry(), time.Now())
	cfg = overridesCfg(map[string]any{"anthropic": map[string]any{"claude-sonnet-4": map[string]any{"context_window": 500000}}})
	patched, _ := modelsDevCapabilitiesIn(f.home, "anthropic", "claude-sonnet-4", false, cfg)
	if patched.ContextWindow != 500000 || !boolPtrEq(patched.SupportsVision, true) || !patched.SupportsTools {
		t.Fatalf("patched %+v", patched)
	}
	cfg = overridesCfg(map[string]any{"anthropic": map[string]any{"_default": map[string]any{"context_window": 1000}}})
	if clamped, _ := modelsDevCapabilitiesIn(f.home, "anthropic", "claude-sonnet-4", false, cfg); clamped.ContextWindow == 1000 {
		t.Fatal("default clamped a catalog model")
	}
	f.setMemory(map[string]any{}, time.Time{})
	if _, ok := modelsDevCapabilitiesIn(f.home, "anthropic", "unknown-model", false, overridesCfg(map[string]any{})); ok {
		t.Fatal("no override no catalog returned caps")
	}
	cfg = overridesCfg(map[string]any{"custom:my-vllm": map[string]any{"_default": map[string]any{"context_window": 32768, "supports_tools": true}}})
	if d, ok := modelsDevCapabilitiesIn(f.home, "custom:my-vllm", "some-new-model", false, cfg); !ok || d.ContextWindow != 32768 || !d.SupportsTools {
		t.Fatalf("default caps %+v", d)
	}
}

func contextIn(f *modelsDevFixture, provider, model string, cfg map[string]any) (int, bool) {
	if n, ok := overrideContextWindow(provider, model, cfg); ok {
		return n, true
	}
	if models := modelsDevProviderModels(f.home, provider, false, cfg); models != nil {
		for _, m := range iterModelsDevEntries(models, model, true, provider) {
			if n := extractModelsDevContext(m.entry); n > 0 {
				return n, true
			}
		}
	}
	return defaultOverrideContext(provider, cfg)
}

func TestContextLookupWithOverrides(t *testing.T) {
	f := newModelsDevFixture(t)
	f.setMemory(sampleRegistry(), time.Now())
	if n, _ := contextIn(f, "anthropic", "claude-opus-4-6", overridesCfg(map[string]any{"anthropic": map[string]any{"claude-opus-4-6": map[string]any{"context_window": 500000}}})); n != 500000 {
		t.Fatalf("override wins %d", n)
	}
	if n, _ := contextIn(f, "upstage", "solar-pro4", overridesCfg(map[string]any{"upstage": map[string]any{"solar-pro4": map[string]any{"context_window": 524288}}})); n != 524288 {
		t.Fatalf("unknown provider %d", n)
	}
	if n, _ := contextIn(f, "anthropic", "model-not-in-catalog", overridesCfg(map[string]any{"anthropic": map[string]any{"_default": map[string]any{"context_window": 77777}}})); n != 77777 {
		t.Fatalf("default fills miss %d", n)
	}
	if n, _ := contextIn(f, "anthropic", "claude-opus-4-6", overridesCfg(map[string]any{"anthropic": map[string]any{"_default": map[string]any{"context_window": 1000}}})); n != 1000000 {
		t.Fatalf("default clamped %d", n)
	}
}

func TestModelInfoWithOverrides(t *testing.T) {
	f := newModelsDevFixture(t)
	cfg := overridesCfg(map[string]any{"custom:my-vllm": map[string]any{"my-llava-model": map[string]any{"model_family": "llava", "supports_reasoning": false, "supports_tools": true, "context_window": 8192, "max_output_tokens": 4096}}})
	info, ok := modelsDevModelInfoIn(f.home, "custom:my-vllm", "my-llava-model", false, cfg)
	if !ok || info.Family != "llava" || info.ContextWindow != 8192 || info.MaxOutput != 0 || !info.ToolCall || info.Reasoning {
		t.Fatalf("%+v", info)
	}
	f.setMemory(sampleRegistry(), time.Now())
	merged, _ := modelsDevModelInfoIn(f.home, "anthropic", "claude-sonnet-4-6", false, overridesCfg(map[string]any{"anthropic": map[string]any{"claude-sonnet-4-6": map[string]any{"context_window": 500000}}}))
	if merged.ContextWindow != 500000 || merged.MaxOutput != 64000 || merged.Name != "claude-sonnet-4-6" {
		t.Fatalf("merged %+v", merged)
	}
	if kept, _ := modelsDevModelInfoIn(f.home, "anthropic", "claude-sonnet-4-6", false, overridesCfg(map[string]any{"anthropic": map[string]any{"_default": map[string]any{"context_window": 1000}}})); kept.ContextWindow != 1000000 {
		t.Fatalf("default clamped %+v", kept)
	}
	f.setMemory(jsonMap(`{"ollama-cloud": {"id": "ollama-cloud", "models": {"kimi-k2.6:cloud": {"id": "kimi-k2.6:cloud", "tool_call": true, "limit": {"context": 262144, "output": 8192}}}}}`), time.Now())
	suffix, _ := modelsDevCapabilitiesIn(f.home, "ollama-cloud", "kimi-k2.6", false, overridesCfg(map[string]any{"ollama-cloud": map[string]any{"_default": map[string]any{"context_window": 1000, "supports_tools": false}}}))
	if suffix.ContextWindow != 262144 || !suffix.SupportsTools {
		t.Fatalf("suffix keyed hit %+v", suffix)
	}
	f.setMemory(map[string]any{}, time.Time{})
	safe, _ := modelsDevModelInfoIn(f.home, "custom:my-vllm", "my-model", false, overridesCfg(map[string]any{"custom:my-vllm": map[string]any{"my-model": map[string]any{"supports_reasoning": true}}}))
	if safe.ContextWindow != 200000 || safe.MaxOutput != 0 || !safe.ToolCall || !safe.Reasoning {
		t.Fatalf("safe defaults %+v", safe)
	}
	vision, _ := modelsDevModelInfoIn(f.home, "custom:my-vllm", "my-model", false, overridesCfg(map[string]any{"custom:my-vllm": map[string]any{"my-model": map[string]any{"supports_vision": true, "context_window": 8192}}}))
	if !containsString(vision.InputModalities, "image") || !vision.Attachment {
		t.Fatalf("vision override %+v", vision)
	}
}

func TestOpenRouterRoutingVariantCatalogLookup(t *testing.T) {
	f := newModelsDevFixture(t)
	f.setMemory(jsonMap(`{"openrouter": {"id": "openrouter", "models": {
	 "z-ai/glm-5.3-flash": {"id": "z-ai/glm-5.3-flash", "limit": {"context": 1310720, "output": 131072}, "tool_call": true, "reasoning": true},
	 "z-ai/glm-5.2": {"id": "z-ai/glm-5.2", "limit": {"context": 1048576, "output": 131072}},
	 "z-ai/glm-5.2:free": {"id": "z-ai/glm-5.2:free", "limit": {"context": 256000, "output": 131072}}}}}`), time.Now())
	for _, suffix := range []string{"nitro", "floor", "exacto", "online"} {
		routed := "z-ai/glm-5.3-flash:" + suffix
		if n, _ := LookupModelsDevContext(f.ctx, "openrouter", routed, false); n != 1310720 {
			t.Fatalf("%s context %d", routed, n)
		}
		base, _ := ModelsDevCapabilities(f.ctx, "openrouter", "z-ai/glm-5.3-flash", false)
		rc, _ := ModelsDevCapabilities(f.ctx, "openrouter", routed, false)
		if rc.ContextWindow != base.ContextWindow || rc.SupportsTools != base.SupportsTools {
			t.Fatalf("%s caps %+v", routed, rc)
		}
		if info, _ := ModelsDevModelInfo(f.ctx, "openrouter", routed, false); info.ContextWindow != 1310720 {
			t.Fatalf("%s info %+v", routed, info)
		}
		if _, ok := LookupModelsDevContext(f.ctx, "anthropic", "claude-x:"+suffix, false); ok {
			t.Fatal("other providers must keep exact match semantics")
		}
	}
	if n, _ := LookupModelsDevContext(f.ctx, "openrouter", "z-ai/glm-5.2:free", false); n != 256000 {
		t.Fatalf("free sku %d", n)
	}
	if _, ok := LookupModelsDevContext(f.ctx, "openrouter", "z-ai/glm-5.3-flash:free", false); ok {
		t.Fatal("absent free sku must miss")
	}
}

func TestOpencodeRelayVisionMarker(t *testing.T) {
	f := newModelsDevFixture(t)
	for _, provider := range []string{"opencode-go", "opencode-zen"} {
		caps, ok := ModelsDevCapabilities(f.ctx, provider, "deepseek-v4-flash-vision-exp", false)
		if !ok || !boolPtrEq(caps.SupportsVision, true) || caps.SupportsReasoning != nil {
			t.Fatalf("%s %+v %v", provider, caps, ok)
		}
	}
	if _, ok := ModelsDevCapabilities(f.ctx, "opencode-go", "deepseek-v4-flash", false); ok {
		t.Fatal("marker needs the vision token")
	}
	if _, ok := ModelsDevCapabilities(f.ctx, "deepseek", "some-vision-model", false); ok {
		t.Fatal("marker needs the opencode family")
	}
	f.setMemory(jsonMap(`{"opencode-go": {"id": "opencode-go", "models": {"deepseek-v4-flash-vision-exp": {"id": "deepseek-v4-flash-vision-exp", "modalities": {"input": ["text"]}, "limit": {"context": 500000}}}}}`), time.Now())
	caps, _ := ModelsDevCapabilities(f.ctx, "opencode-go", "deepseek-v4-flash-vision-exp", false)
	if !boolPtrEq(caps.SupportsVision, false) || caps.ContextWindow != 500000 {
		t.Fatalf("catalog must stay authoritative %+v", caps)
	}
}

func TestRegisteredModelCapabilitiesReachEveryConsumerAndUserOverrideWins(t *testing.T) {
	f := newModelsDevFixture(t)
	RegisterModelCapabilities("acme", map[string]map[string]any{"acme-vision": {"supports_vision": true, "context_window": 64000, "supports_reasoning": true}})
	t.Cleanup(func() { RegisterModelCapabilities("acme", nil) })
	caps, ok := ModelsDevCapabilities(f.ctx, "acme", "acme-vision", false)
	if !ok || !boolPtrEq(caps.SupportsVision, true) || caps.ContextWindow != 64000 || !boolPtrEq(caps.SupportsReasoning, true) {
		t.Fatalf("%+v %v", caps, ok)
	}
	if n, ok := LookupModelsDevContext(f.ctx, "acme", "acme-vision", false); !ok || n != 64000 {
		t.Fatalf("declared context %d %v", n, ok)
	}
	user := overridesCfg(map[string]any{"acme": map[string]any{"acme-vision": map[string]any{"supports_vision": false}}})
	over, _ := modelsDevCapabilitiesIn(f.home, "acme", "acme-vision", false, user)
	if !boolPtrEq(over.SupportsVision, false) || over.ContextWindow != 64000 {
		t.Fatalf("user override must win over the declaration %+v", over)
	}
	partial, _ := ModelsDevCapabilities(f.ctx, "acme", "acme-other", false)
	if partial.SupportsVision != nil {
		t.Fatal("undeclared models stay unknown")
	}
}

func TestListModelsAndAgenticModels(t *testing.T) {
	f := newModelsDevFixture(t)
	f.setMemory(jsonMap(`{"google": {"id": "google", "models": {
	 "gemini-3.8-flash": {"tool_call": true}, "gemma-3-1b": {"tool_call": true}, "gemini-embedding-001": {"tool_call": true},
	 "gemini-2.5-flash-preview-tts": {"tool_call": true}, "gemini-3-image": {"tool_call": false}}}}`), time.Now())
	if got := ListModelsDevModels(f.ctx, "google", false); !reflect.DeepEqual(got, []string{"gemini-2.5-flash-preview-tts", "gemini-3-image", "gemini-3.8-flash", "gemini-embedding-001"}) {
		t.Fatalf("list %v", got)
	}
	if got := ListAgenticModels(f.ctx, "gemini", false); !reflect.DeepEqual(got, []string{"gemini-3.8-flash"}) {
		t.Fatalf("agentic %v", got)
	}
	if got := ListModelsDevModels(f.ctx, "no-such", false); len(got) != 0 {
		t.Fatalf("unknown provider %v", got)
	}
}

func TestProviderInfoAndProfileSynthesis(t *testing.T) {
	f := newModelsDevFixture(t)
	f.setMemory(jsonMap(`{"acme": {"id": "acme", "name": "Acme Cloud", "env": ["ACME_API_KEY"], "api": "https://api.acme.test/v1", "doc": "https://acme.test/docs", "models": {"acme-1": {}}}}`), time.Now())
	info, ok := ModelsDevProviderInfo(f.ctx, "acme", false)
	if !ok || info.Name != "Acme Cloud" || info.ModelCount != 1 || !reflect.DeepEqual(info.Env, []string{"ACME_API_KEY"}) {
		t.Fatalf("%+v %v", info, ok)
	}
	prof, ok := modelsDevProfileOffline("acme")
	if !ok || prof.DisplayName != "Acme Cloud" || prof.BaseURL != "https://api.acme.test/v1" || prof.SignupURL != "https://acme.test/docs" || prof.KeyEnv[0] != "ACME_API_KEY" || prof.Mode != ModeChatCompletions {
		t.Fatalf("%+v %v", prof, ok)
	}
	if _, ok := modelsDevProfileOffline("unknown-vendor"); ok {
		t.Fatal("unknown provider synthesized")
	}
	entry, ok := ModelsDevProfile(f.ctx, "acme", "acme-1")
	if !ok || entry.ProviderID != "acme" || entry.FormatCapabilities() != "basic" {
		t.Fatalf("%+v %v", entry, ok)
	}
}

func TestParseModelsDevEntryCapabilities(t *testing.T) {
	raw := jsonMap(`{"name": "Model X", "family": "x", "reasoning": true, "tool_call": true, "attachment": false, "open_weights": true,
	 "modalities": {"input": ["text", "image", "pdf", "audio"], "output": ["text"]}, "limit": {"context": 100, "output": 10, "input": 90},
	 "cost": {"input": 1.5, "output": 3, "cache_read": 0.1}, "knowledge": "2026-01", "release_date": "2026-02-01", "status": "beta", "interleaved": {"field": "reasoning_content"}}`)
	e := parseModelsDevEntry("x-1", raw, "acme")
	if e.FormatCapabilities() != "reasoning, tools, vision, PDF, audio, open weights" {
		t.Fatalf("caps %q", e.FormatCapabilities())
	}
	if !e.HasCostData() || e.CostCacheRead == nil || *e.CostCacheRead != 0.1 || e.CostCacheWrite != nil || e.MaxInput != 90 || e.Status != "beta" {
		t.Fatalf("%+v", e)
	}
	bare := parseModelsDevEntry("y", map[string]any{}, "acme")
	if bare.Name != "y" || bare.Interleaved != false || bare.HasCostData() {
		t.Fatalf("%+v", bare)
	}
}
