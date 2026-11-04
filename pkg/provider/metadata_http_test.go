package provider

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestRedirectCredentialsAndProxyRoutingArePreserved(t *testing.T) {
	ctx, _, _ := metaTest(t)
	metadataTransport = nil
	var mu sync.Mutex
	var seen []struct {
		path    string
		headers http.Header
	}
	sinkURL := ""
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, struct {
			path    string
			headers http.Header
		}{r.RequestURI, r.Header.Clone()})
		mu.Unlock()
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, sinkURL+"/models", http.StatusFound)
			return
		}
		w.Header().Set("Content-Length", "11")
		w.WriteHeader(200)
		w.Write([]byte(`{"data":[]}`))
	})
	source := httptest.NewServer(handler)
	defer source.Close()
	sink := httptest.NewServer(handler)
	defer sink.Close()
	sinkURL = sink.URL
	t.Setenv("NO_PROXY", "127.0.0.1")
	t.Setenv("HTTP_PROXY", "")
	headers := map[string]string{"Authorization": "Bearer dummy", "X-Custom-Auth": "dummy", "Accept": "application/json"}
	resp, err := newProbeClient(ctx, "", headers, 5*time.Second, 5*time.Second).Get(source.URL+"/redirect", nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("redirect fetch: %v %v", err, resp)
	}
	mustEqual(t, seen[0].headers.Get("Authorization"), "Bearer dummy", "first hop keeps auth")
	last := seen[len(seen)-1].headers
	if last.Get("Authorization") != "" || last.Get("X-Custom-Auth") != "" {
		t.Fatal("cross origin redirect must drop credentials")
	}
	mustEqual(t, last.Get("Accept"), "application/json", "accept survives")
	t.Setenv("HTTP_PROXY", source.URL)
	t.Setenv("NO_PROXY", "")
	resp, err = newProbeClient(ctx, "", nil, 5*time.Second, 5*time.Second).Get("http://probe.invalid/models", nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("proxied fetch: %v %v", err, resp)
	}
	mustEqual(t, seen[len(seen)-1].path, "http://probe.invalid/models", "proxy sees the absolute uri")
}

func TestMetadataAuthFailureClosesWithoutReadingBody(t *testing.T) {
	for _, status := range []int{401, 403, 404} {
		ctx, _, _ := metaTest(t)
		metadataTransport = nil
		var mu sync.Mutex
		var requests []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			requests = append(requests, r.URL.Path)
			mu.Unlock()
			if status == 404 && r.URL.Path == "/models" {
				body := `{"data":[{"id":"test/model","context_length":32768}]}`
				w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				w.WriteHeader(200)
				w.Write([]byte(body))
				return
			}
			w.Header().Set("Content-Length", "1000000")
			w.WriteHeader(status)
		}))
		t.Setenv("NO_PROXY", "127.0.0.1")
		seedServerType(server.URL+"/v1", "")
		started := time.Now()
		result := FetchEndpointModelMetadata(ctx, server.URL+"/v1", "", true)
		if status == 404 {
			mustEqual(t, intOrDefault(result["test/model"]["context_length"], 0), 32768, "alternate candidate")
			mustEqual(t, strings.Join(requests, ","), "/v1/models,/models", "paths")
		} else {
			if len(result) != 0 || len(FetchEndpointModelMetadata(ctx, server.URL+"/v1", "", false)) != 0 {
				t.Fatalf("status %d must yield empty metadata", status)
			}
			mustEqual(t, strings.Join(requests, ","), "/v1/models", "stops after the rejection")
		}
		if time.Since(started) > 2*time.Second {
			t.Fatalf("status %d waited for an absent body", status)
		}
		server.CloseClientConnections()
		server.Close()
	}
}

func TestEndpointModelsProbeOnATricklingServerReturnsWithinTheDeadline(t *testing.T) {
	ctx, _, _ := metaTest(t)
	metadataTransport = nil
	stop := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 80; i++ {
			if _, err := w.Write([]byte(" ")); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}))
	defer func() {
		close(stop)
		server.CloseClientConnections()
		server.Close()
	}()
	t.Setenv("NO_PROXY", "127.0.0.1")
	probeTotalDeadline = 500 * time.Millisecond
	seedServerType(server.URL+"/v1", "")
	started := time.Now()
	if got := FetchEndpointModelMetadata(ctx, server.URL+"/v1", "", true); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if time.Since(started) >= 3*time.Second {
		t.Fatal("probe outlived the deadline")
	}
}

func TestConcurrentCacheWritersKeepEachOthersEntries(t *testing.T) {
	ctx, _, _ := metaTest(t)
	SaveContextLength(ctx, "model-a", "https://a.example/v1", 200_000, "")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			SaveContextLength(ctx, fmt.Sprintf("model-%d", i), "https://a.example/v1", 128_000+i, "")
		}(i)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				invalidateCachedContextLength(ctx, "model-a", "https://a.example/v1")
			} else {
				SaveContextLength(ctx, "model-b", "https://b.example/v1", 64_000, "")
			}
		}(i)
	}
	wg.Wait()
	if got, ok := GetCachedContextLength(ctx, "model-b", "https://b.example/v1", false); !ok || got != 64_000 {
		t.Fatal("model-b survives concurrent writers")
	}
	for i := 0; i < 8; i++ {
		if got, ok := GetCachedContextLength(ctx, fmt.Sprintf("model-%d", i), "https://a.example/v1", false); !ok || got != 128_000+i {
			t.Fatalf("model-%d lost", i)
		}
	}
	if _, ok := GetCachedContextLength(ctx, "model-a", "https://a.example/v1", false); ok {
		t.Fatal("model-a was invalidated")
	}
}

func TestInvalidationEvictsInProcessProbeCachesWhenTheLockIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	ctx := home.WithOverride(context.Background(), dir)
	resetMetadataCaches()
	t.Cleanup(resetMetadataCaches)
	contextCacheLockTimeout = 100 * time.Millisecond
	t.Cleanup(func() { contextCacheLockTimeout = 5 * time.Second })
	base := "https://a.example/v1"
	localCtxProbeMu.Lock()
	localCtxProbeCache[localProbeKey{"", "model-a", base}] = localProbeHit{result: 200_000}
	localCtxProbeCache[localProbeKey{"ollama_show", "model-a", base}] = localProbeHit{result: 200_000}
	localCtxProbeMu.Unlock()
	bedrockProbeFailureMu.Lock()
	bedrockProbeFailureCache[bedrockProbeKey{dir, base, "model-a", "us-east-1"}] = time.Time{}
	bedrockProbeFailureMu.Unlock()
	lockPath := filepath.Join(dir, "context_length_cache.yaml.lock")
	f, err := openLockFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := kernelLock(f, true); err != nil {
		t.Fatal(err)
	}
	defer func() { kernelLock(f, false); f.Close() }()
	os.WriteFile(filepath.Join(dir, "context_length_cache.yaml"), []byte("context_lengths:\n  model-a@https://a.example/v1: 200000\n"), 0o600)
	invalidateCachedContextLength(ctx, "model-a", base)
	localCtxProbeMu.Lock()
	_, a := localCtxProbeCache[localProbeKey{"", "model-a", base}]
	_, b := localCtxProbeCache[localProbeKey{"ollama_show", "model-a", base}]
	localCtxProbeMu.Unlock()
	if a || b {
		t.Fatal("in process probe memos must be evicted without the file lock")
	}
	bedrockProbeFailureMu.Lock()
	n := len(bedrockProbeFailureCache)
	bedrockProbeFailureMu.Unlock()
	mustEqual(t, n, 0, "bedrock failure memo evicted")
}

func TestIsConnectTimeoutAndBlackhole(t *testing.T) {
	metaTest(t)
	timeout := &net.OpError{Op: "dial", Err: &timeoutErr{}}
	if !isConnectTimeout(timeout) {
		t.Fatal("dial timeout")
	}
	if isConnectTimeout(&net.OpError{Op: "read", Err: &timeoutErr{}}) {
		t.Fatal("read timeout proves a connection")
	}
	noteIfConnectTimeout(timeout, "http://10.0.0.9:8080/v1")
	if !endpointBlackholed("http://10.0.0.9:8080") || endpointBlackholed("http://10.0.0.9:9090") {
		t.Fatal("blackhole keyed by host and port")
	}
	monotonicNow = func() time.Time { return time.Now().Add(endpointBlackholeTTL + time.Second) }
	if endpointBlackholed("http://10.0.0.9:8080") {
		t.Fatal("blackhole expires")
	}
}

type timeoutErr struct{}

func (*timeoutErr) Error() string   { return "i/o timeout" }
func (*timeoutErr) Timeout() bool   { return true }
func (*timeoutErr) Temporary() bool { return true }

func TestMessageTokenMemo(t *testing.T) {
	metaTest(t)
	row := map[string]any{"role": "user", "content": strings.Repeat("hello world ", 50), "timestamp": "2026-01-01"}
	first := EstimateRowTokensCached(row, 1500)
	row["timestamp"] = "2026-02-02"
	mustEqual(t, EstimateRowTokensCached(row, 1500), first, "persistence fields never miss the memo")
	msgTokensCache.mu.Lock()
	entries := msgTokensCache.order.Len()
	msgTokensCache.mu.Unlock()
	mustEqual(t, entries, 1, "one entry")
	if RetainedBytes() <= 0 {
		t.Fatal("retained bytes tracked")
	}
	image := map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + strings.Repeat("A", 100)}}}}
	withCost := EstimateRowTokensCached(image, 1500)
	repriced := EstimateRowTokensCached(image, 100)
	if withCost-repriced != 1400 {
		t.Fatalf("image count memoized separately from its price: %d %d", withCost, repriced)
	}
	mustEqual(t, ForgetMessageTokenMemo([]any{row}, nil), 1, "forget evicts the pinning entry")
	mustEqual(t, ForgetMessageTokenMemo([]any{row}, []any{row}), 0, "kept strings are not evicted")
}

func TestMessageTokenMemoBounds(t *testing.T) {
	metaTest(t)
	base := time.Now().Add(-4 * activeWindow)
	msgTokensCache.clock = func() time.Time { return base }
	t.Cleanup(func() { msgTokensCache.clock = time.Now })
	for i := 0; i < msgTokensCacheMax; i++ {
		rememberMessageTokens(fmt.Sprint(i), messageTokenEntry{pins: &MemoPins{Strings: []string{fmt.Sprint(i)}}, text: 1})
	}
	base = base.Add(2 * activeWindow)
	for i := msgTokensCacheMax; i < msgTokensCacheMax+10; i++ {
		rememberMessageTokens(fmt.Sprint(i), messageTokenEntry{pins: &MemoPins{Strings: []string{fmt.Sprint(i)}}, text: 1})
	}
	if _, ok := lookupMessageTokens("0"); ok {
		t.Fatal("the oldest idle entries are shed first")
	}
	msgTokensCache.mu.Lock()
	n := msgTokensCache.order.Len()
	msgTokensCache.mu.Unlock()
	mustEqual(t, n, msgTokensCacheMax, "entry bound")
	huge := messageTokenEntry{pins: &MemoPins{Strings: []string{strings.Repeat("x", msgTokensCacheMaxBytes+1)}}}
	rememberMessageTokens("huge", huge)
	if _, ok := lookupMessageTokens("huge"); ok {
		t.Fatal("an entry larger than the budget is never stored")
	}
	msgTokensCache.clock = time.Now
	rememberMessageTokens("active", messageTokenEntry{pins: &MemoPins{Strings: []string{strings.Repeat("y", msgTokensCacheMaxBytes/2)}}})
	rememberMessageTokens("active2", messageTokenEntry{pins: &MemoPins{Strings: []string{strings.Repeat("z", msgTokensCacheMaxBytes/2)}}})
	if _, ok := lookupMessageTokens("active"); !ok {
		t.Fatal("entries inside the active window are never evicted")
	}
}
