package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
)

func TestSPAAssetsKeepWebContentTypesOverAPoisonedHostMap(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_SERVE_HEADLESS", "")
	db := pymimetypes.Default()
	for _, ext := range []string{".js", ".mjs", ".css"} {
		db.AddType("text/plain", ext, true)
	}
	t.Cleanup(normalizeWebAssetMIMETypes)
	if typ, _, _ := pymimetypes.GuessType("a.js", true); typ != "text/plain" {
		t.Fatalf("poisoned guess = %q", typ)
	}
	normalizeWebAssetMIMETypes()
	normalizeWebAssetMIMETypes()
	for _, e := range webAssetMIMETypes {
		if typ, _, _ := pymimetypes.GuessType("file"+e.ext, true); typ != e.mime {
			t.Errorf("guess %s = %q, want %q", e.ext, typ, e.mime)
		}
	}
	if typ, _, _ := pymimetypes.GuessType("bundle.js.map", true); typ != "application/json" {
		t.Errorf("source map = %q", typ)
	}
	dist := filepath.Join(t.TempDir(), "web_dist")
	if err := os.MkdirAll(filepath.Join(dist, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"index.html": "<html><head><title>t</title></head><body>SPA</body></html>",
		"assets/index-CqUa8pQa.js": "export const boot = () => {};", "assets/worker-Bd0k2mZx.mjs": "export const worker = true;"}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dist, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	app := NewFastAPI("t", "1", nil, nil)
	app.State.Set("web_dist", dist)
	mountSPA(app)
	c := newTestClient(t, app)
	for _, asset := range []string{"/assets/index-CqUa8pQa.js", "/assets/worker-Bd0k2mZx.mjs"} {
		resp := c.get(asset)
		got := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Headers.Get("content-type"), ";", 2)[0]))
		if resp.Status != 200 || got != "text/javascript" {
			t.Errorf("%s = %d %q", asset, resp.Status, resp.Headers.Get("content-type"))
		}
	}
}
