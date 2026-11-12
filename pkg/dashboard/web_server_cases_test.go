package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const immutableCacheControl = "public, max-age=31536000, immutable"

// Copies the testdata SPA bundle into a fresh web_dist, leaving index.html out when asked
func copySpaBundle(t *testing.T, withIndex bool) string {
	t.Helper()
	dist := filepath.Join(t.TempDir(), "web_dist")
	src := filepath.Join("testdata", "spa_bundle")
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dist, rel), 0o755)
		}
		if rel == "index.html" && !withIndex {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dist, rel), raw, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dist
}

// Points the module level WEB_DIST at dist for the test
func patchModuleWebDist(t *testing.T, dist string) {
	t.Helper()
	webStateMu.Lock()
	saved := webDist
	webDist = dist
	webStateMu.Unlock()
	t.Cleanup(func() {
		webStateMu.Lock()
		webDist = saved
		webStateMu.Unlock()
	})
}

// SPA app mounted over the module WEB_DIST with headless mode cleared
func spaModuleClient(t *testing.T, dist string) *testClient {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_SERVE_HEADLESS", "")
	patchModuleWebDist(t, dist)
	app := NewFastAPI("", "", nil, nil)
	mountSPA(app)
	return newTestClient(t, app)
}

// Asserts a JSON 404 whose error field is a non empty string
func expectJSONError404(t *testing.T, resp *testResponse) {
	t.Helper()
	if resp.Status != 404 {
		t.Fatalf("status %d %q", resp.Status, resp.Body)
	}
	obj, ok := resp.JSON(t).(*pyjson.Object)
	if !ok {
		t.Fatalf("not an object %q", resp.Body)
	}
	if msg, _ := obj.Vals["error"].(string); msg == "" {
		t.Fatalf("no error in %q", resp.Body)
	}
}

func TestServeIndexMissingIndexMissingIndexInsideExistingDistReturnsJSON404(t *testing.T) {
	client := spaModuleClient(t, copySpaBundle(t, false))
	for _, route := range []string{"/", "/chat"} {
		expectJSONError404(t, client.get(route))
	}
}

func TestServeIndexMissingIndexIndexDeletedAfterMountReturnsJSON404(t *testing.T) {
	dist := copySpaBundle(t, true)
	client := spaModuleClient(t, dist)
	if resp := client.get("/chat"); resp.Status != 200 {
		t.Fatalf("healthy first %d %q", resp.Status, resp.Body)
	}
	if err := os.Remove(filepath.Join(dist, "index.html")); err != nil {
		t.Fatal(err)
	}
	expectJSONError404(t, client.get("/chat"))
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("<html><head></head><body>SPA-rebuilt</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := client.get("/chat")
	if resp.Status != 200 || !strings.Contains(string(resp.Body), "SPA-rebuilt") {
		t.Fatalf("%d %q", resp.Status, resp.Body)
	}
}

func TestHashedAssetCacheHeadersHashedJsAssetIsImmutable(t *testing.T) {
	resp := spaModuleClient(t, copySpaBundle(t, true)).get("/assets/index-abc123.js")
	if resp.Status != 200 || resp.Headers.Get("cache-control") != immutableCacheControl {
		t.Fatalf("%d %q", resp.Status, resp.Headers.Get("cache-control"))
	}
}

func TestHashedAssetCacheHeadersServeCssIsImmutableAndKeepsPrefixRewrites(t *testing.T) {
	client := spaModuleClient(t, copySpaBundle(t, true))
	resp := client.get("/assets/index-abc123.css")
	if resp.Status != 200 || resp.Headers.Get("cache-control") != immutableCacheControl {
		t.Fatalf("%d %q", resp.Status, resp.Headers.Get("cache-control"))
	}
	prefixed := client.get("/assets/index-abc123.css", "X-Forwarded-Prefix", "/hermes")
	if prefixed.Status != 200 || prefixed.Headers.Get("cache-control") != immutableCacheControl {
		t.Fatalf("%d %q", prefixed.Status, prefixed.Headers.Get("cache-control"))
	}
	for _, want := range []string{"url(/hermes/ds-assets/bg.png)", "url(/hermes/fonts-terminal/x.woff2)"} {
		if !strings.Contains(string(prefixed.Body), want) {
			t.Fatalf("missing %s in %q", want, prefixed.Body)
		}
	}
}

func TestHashedAssetCacheHeadersIndexHtmlStaysNoStore(t *testing.T) {
	client := spaModuleClient(t, copySpaBundle(t, true))
	for _, route := range []string{"/", "/chat"} {
		resp := client.get(route)
		cacheControl := resp.Headers.Get("cache-control")
		if resp.Status != 200 || !strings.Contains(cacheControl, "no-store") || strings.Contains(cacheControl, "immutable") {
			t.Fatalf("%s: %d %q", route, resp.Status, cacheControl)
		}
	}
}

func TestHashedAssetCacheHeadersMissingAssetIsNotMarkedImmutable(t *testing.T) {
	resp := spaModuleClient(t, copySpaBundle(t, true)).get("/assets/nope-000000.js")
	if resp.Status != 404 || strings.Contains(resp.Headers.Get("cache-control"), "immutable") {
		t.Fatalf("%d %q", resp.Status, resp.Headers.Get("cache-control"))
	}
}

func TestMountSpaDynamicWebDistRecheck(t *testing.T) {
	dist := filepath.Join(t.TempDir(), "web_dist")
	client := spaModuleClient(t, dist)
	expectJSONError404(t, client.get("/"))
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("<html><body>Test</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := client.get("/")
	if resp.Status != 200 || !strings.Contains(string(resp.Body), "Test") {
		t.Fatalf("%d %q", resp.Status, resp.Body)
	}
}
