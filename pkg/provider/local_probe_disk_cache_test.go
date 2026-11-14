package provider

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Ages every entry of the local probe disk memo past its TTL
func ageLocalProbeDisk(t *testing.T, ctx context.Context) {
	t.Helper()
	path := localProbeDiskCachePath(ctx)
	data := loadJSONDict(path)
	for _, v := range data {
		if entry, ok := v.(map[string]any); ok {
			entry["ts"] = float64(wallNow().Unix()) - localProbeDiskTTL.Seconds() - 1
		}
	}
	if err := atomicJSONWrite(path, data); err != nil {
		t.Fatal(err)
	}
}

// Ages the in memory detection verdict of a server past the given TTL
func ageDetectionVerdict(url string, ttl time.Duration) {
	server := probeServerRoot(localhostToIPv4(normalizeBaseURL(url)))
	endpointProbePathMu.Lock()
	defer endpointProbePathMu.Unlock()
	verdict := endpointProbePathCache[server]
	verdict.at = monotonicNow().Add(-ttl - time.Second)
	endpointProbePathCache[server] = verdict
}

func TestDiskHelpersExpiredEntryIsMiss(t *testing.T) {
	ctx, _, _ := metaTest(t)
	localProbeDiskPut(ctx, "server_type", "http://127.0.0.1:11434", "ollama")
	ageLocalProbeDisk(t, ctx)
	if got := localProbeDiskGet(ctx, "server_type", "http://127.0.0.1:11434"); got != nil {
		t.Fatalf("expired entry served %v", got)
	}
}

func TestDiskHelpersCorruptedFileIsMiss(t *testing.T) {
	ctx, _, _ := metaTest(t)
	path := localProbeDiskCachePath(ctx)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := localProbeDiskGet(ctx, "server_type", "anything"); got != nil {
		t.Fatalf("corrupted file served %v", got)
	}
	localProbeDiskPut(ctx, "server_type", "k", "vllm")
	if got := localProbeDiskGet(ctx, "server_type", "k"); got != "vllm" {
		t.Fatalf("put did not recover the file: %v", got)
	}
}

func TestDetectLocalServerTypeDiskL2DiskHitSkipsHttpEntirely(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	localProbeDiskPut(ctx, "server_type", "http://127.0.0.1:9999", "ollama")
	if got := DetectLocalServerType(ctx, "http://127.0.0.1:9999/v1", ""); got != "ollama" || ft.count() != 0 {
		t.Fatalf("got %q after %d requests", got, ft.count())
	}
}

func TestDetectLocalServerTypeDiskL2SuccessfulProbePersistsToDisk(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(*http.Request) (*http.Response, error) { return textResponse(404, ""), nil }
	ft.on("GET", "http://127.0.0.1:8888/api/tags", 200, map[string]any{"models": []any{}})
	if got := DetectLocalServerType(ctx, "http://127.0.0.1:8888/v1", ""); got != "ollama" {
		t.Fatalf("got %q", got)
	}
	if got := localProbeDiskGet(ctx, "server_type", "http://127.0.0.1:8888"); got != "ollama" {
		t.Fatalf("disk entry %v", got)
	}
}

func TestDetectLocalServerTypeDiskL2FailedProbeNotPersisted(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") }
	if got := DetectLocalServerType(ctx, "http://127.0.0.1:7777/v1", ""); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := localProbeDiskGet(ctx, "server_type", "http://127.0.0.1:7777"); got != nil {
		t.Fatalf("a failed probe pinned %v", got)
	}
}

func TestOllamaNumCtxDiskL2DiskHitSkipsApiShow(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	localProbeDiskPut(ctx, "server_type", "http://127.0.0.1:11434", "ollama")
	localProbeDiskPut(ctx, "ollama_num_ctx", "http://127.0.0.1:11434|llama3", 131072)
	n, ok := QueryOllamaNumCtx(ctx, "llama3", "http://127.0.0.1:11434/v1", "")
	if !ok || n != 131072 || ft.count() != 0 {
		t.Fatalf("got %d %t after %d requests", n, ok, ft.count())
	}
}

func TestOllamaNumCtxDiskL2SuccessfulShowPersists(t *testing.T) {
	ctx, _, _ := metaTest(t)
	localProbeDiskPut(ctx, "server_type", "http://127.0.0.1:11434", "ollama")
	ft := metadataTransport.(*fakeTransport)
	ft.on("POST", "http://127.0.0.1:11434/api/show", 200, map[string]any{"parameters": "", "model_info": map[string]any{"llama.context_length": 8192}})
	n, ok := QueryOllamaNumCtx(ctx, "llama3", "http://127.0.0.1:11434/v1", "")
	if !ok || n != 8192 {
		t.Fatalf("got %d %t", n, ok)
	}
	if hit, ok := intValue(localProbeDiskGet(ctx, "ollama_num_ctx", "http://127.0.0.1:11434|llama3")); !ok || hit != 8192 {
		t.Fatalf("disk entry %v", localProbeDiskGet(ctx, "ollama_num_ctx", "http://127.0.0.1:11434|llama3"))
	}
}

func TestOllamaApiShowCachingFailureNeverMemoized(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(*http.Request) (*http.Response, error) { return textResponse(404, ""), nil }
	if queryOllamaAPIShow(ctx, "llama3", "http://127.0.0.1:11434", "") != 0 || queryOllamaAPIShow(ctx, "llama3", "http://127.0.0.1:11434", "") != 0 {
		t.Fatal("a down server has no context length")
	}
	if ft.count() != 2 {
		t.Fatalf("%d probes, the failure was memoized", ft.count())
	}
}

// Ollama shaped transport answering /api/tags and nothing else
func ollamaTagsTransport(ft *fakeTransport, root string) {
	ft.handle = func(*http.Request) (*http.Response, error) { return textResponse(404, ""), nil }
	ft.on("GET", root+"/api/tags", 200, map[string]any{"models": []any{}})
}

func TestDetectLocalServerTypeCacheTtlExpiryAllowsServerSwapRedetection(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ollamaTagsTransport(ft, "http://127.0.0.1:11434")
	if got := DetectLocalServerType(ctx, "http://127.0.0.1:11434", ""); got != "ollama" {
		t.Fatalf("got %q", got)
	}
	ageDetectionVerdict("http://127.0.0.1:11434", endpointProbeTTL)
	ageLocalProbeDisk(t, ctx)
	swapped := newFakeTransport()
	swapped.handle = func(*http.Request) (*http.Response, error) { return textResponse(404, ""), nil }
	swapped.onFunc("GET", lmstudioServerRoot("http://127.0.0.1:11434")+"/api/v1/models", func(*http.Request) *http.Response {
		return jsonResponse(nil, 200, map[string]any{"data": []any{}}, nil)
	})
	metadataTransport = swapped
	if got := DetectLocalServerType(ctx, "http://127.0.0.1:11434", ""); got != "lm-studio" {
		t.Fatalf("after the swap got %q", got)
	}
}

func TestLocalhostIPv4SiblingSitesRewriteIsHostOnlyNotSubstring(t *testing.T) {
	proxied := "https://proxy.example.com/route?upstream=http://localhost:11434"
	if localhostToIPv4(proxied) != proxied {
		t.Fatal("an embedded localhost must not be rewritten")
	}
	if localhostToIPv4("http://localhost.example.com/v1") != "http://localhost.example.com/v1" {
		t.Fatal("localhost.example.com is not localhost")
	}
}

func TestLocalhostIPv4SiblingSitesOllamaApiShowProbesIpv4(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(*http.Request) (*http.Response, error) {
		return jsonResponse(nil, 200, map[string]any{"model_info": map[string]any{"llama.context_length": 131072}, "parameters": ""}, nil), nil
	}
	queryOllamaAPIShow(ctx, "llama3", "http://localhost:11434", "")
	urls := ft.urls()
	if len(urls) == 0 || !strings.HasPrefix(urls[0], "http://127.0.0.1:11434") {
		t.Fatalf("probe urls %v", urls)
	}
}

func TestLocalhostIPv4SiblingSitesEndpointAndPropsFollowupUseIpv4(t *testing.T) {
	for _, llamacpp := range []bool{false, true} {
		ctx, ft, _ := metaTest(t)
		settleDetection("http://localhost:8000/v1", "")
		models := []any{}
		if llamacpp {
			models = []any{map[string]any{"id": "llama-3-8b", "owned_by": "llamacpp"}}
		}
		ft.on("GET", "http://127.0.0.1:8000/v1/models", 200, map[string]any{"data": models})
		ft.on("GET", "http://127.0.0.1:8000/v1/props", 200, map[string]any{"default_generation_settings": map[string]any{"n_ctx": 32768}, "model_alias": "llama-3-8b"})
		result := FetchEndpointModelMetadata(ctx, "http://localhost:8000/v1", "", true)
		urls := ft.urls()
		want := 1
		if llamacpp {
			want = 2
		}
		if len(urls) != want {
			t.Fatalf("llamacpp=%t requests %v", llamacpp, urls)
		}
		for _, u := range urls {
			if !strings.HasPrefix(u, "http://127.0.0.1:8000") {
				t.Fatalf("request to %s did not use IPv4", u)
			}
		}
		if llamacpp {
			if !strings.HasSuffix(urls[1], "/v1/props") {
				t.Fatalf("props follow up %v", urls)
			}
			if n, _ := intValue(result["llama-3-8b"]["context_length"]); n != 32768 {
				t.Fatalf("context length %v", result["llama-3-8b"])
			}
		}
	}
}

func TestContextCacheKeyNormalizationTrailingSlashVariantsShareOneEntry(t *testing.T) {
	ctx, _, _ := metaTest(t)
	SaveContextLength(ctx, "m1", "http://host/v1/", 200_000, "")
	for _, base := range []string{"http://host/v1", "http://host/v1/"} {
		if n, ok := GetCachedContextLength(ctx, "m1", base, false); !ok || n != 200_000 {
			t.Fatalf("%s -> %d %t", base, n, ok)
		}
	}
	cache, _ := loadContextCacheDocument(ctx)
	if len(cache) != 1 || cache["m1@http://host/v1"] != 200_000 {
		t.Fatalf("cache rows %v", cache)
	}
}

func TestContextCacheKeyNormalizationInvalidateClearsBothKeyShapes(t *testing.T) {
	ctx, _, _ := metaTest(t)
	path := contextCachePath(ctx)
	raw, err := yaml.Marshal(map[string]any{"context_lengths": map[string]any{"m1@http://host/v1": 128_000, "m1@http://host/v1/": 64_000}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	invalidateCachedContextLength(ctx, "m1", "http://host/v1/")
	cache, _ := loadContextCacheDocument(ctx)
	for _, key := range []string{"m1@http://host/v1", "m1@http://host/v1/"} {
		if _, has := cache[key]; has {
			t.Fatalf("%s survived the invalidation: %v", key, cache)
		}
	}
}

func TestDetectServerTypeNegativeCachingNegativeVerdictIsCachedInMemory(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(*http.Request) (*http.Response, error) { return textResponse(401, ""), nil }
	if DetectLocalServerType(ctx, "http://remote:8080/v1", "") != "" {
		t.Fatal("401s give no server type")
	}
	afterFirst := ft.count()
	if DetectLocalServerType(ctx, "http://remote:8080/v1", "") != "" {
		t.Fatal("401s give no server type")
	}
	if afterFirst == 0 || ft.count() != afterFirst {
		t.Fatalf("waterfall ran %d then %d probes", afterFirst, ft.count())
	}
}

func TestDetectServerTypeNegativeCachingNegativeVerdictExpiresQuickly(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	ft.handle = func(*http.Request) (*http.Response, error) { return textResponse(401, ""), nil }
	if DetectLocalServerType(ctx, "http://remote3:8080/v1", "") != "" {
		t.Fatal("401s give no server type")
	}
	afterFirst := ft.count()
	ageDetectionVerdict("http://remote3:8080/v1", endpointProbeFailureTTL)
	if DetectLocalServerType(ctx, "http://remote3:8080/v1", "") != "" {
		t.Fatal("401s give no server type")
	}
	if ft.count() != 2*afterFirst {
		t.Fatalf("waterfall ran %d then %d probes", afterFirst, ft.count())
	}
}
