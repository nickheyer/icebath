package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

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
