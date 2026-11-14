package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Dashboard application with one synthetic API key catalog entry in the isolated home
func mcpCatalogEnv(t *testing.T, extraEnv ...map[string]any) (cronAppClient, string) {
	t.Helper()
	app := dashboardTestApp(t)
	h := os.Getenv("ICEBATH_HOME")
	catalog := filepath.Join(t.TempDir(), "optional-mcps")
	entryDir := filepath.Join(catalog, "demo")
	if err := os.MkdirAll(entryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	envSpecs := []map[string]any{{"name": "DEMO_API_KEY", "prompt": "Demo API key", "secret": true}}
	envSpecs = append(envSpecs, extraEnv...)
	manifest := map[string]any{
		"manifest_version": 1, "name": "demo", "description": "Synthetic dashboard boundary fixture",
		"source": "https://example.test/demo", "transport": map[string]any{"type": "stdio", "command": "demo-mcp"},
		"auth": map[string]any{"type": "api_key", "env": envSpecs},
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(entryDir, "manifest.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_OPTIONAL_MCPS", catalog)
	config.InvalidateEnvCache()
	return cronAppClient{newTestClient(t, app)}, h
}

// Whether the home .env is absent or holds no text
func envFileEmpty(t *testing.T, h string) bool {
	raw, err := os.ReadFile(filepath.Join(h, ".env"))
	return err != nil || len(raw) == 0
}

func TestCatalogRejectsUndeclaredKeyBeforeAnyWriteOrInstall(t *testing.T) {
	c, h := mcpCatalogEnv(t)
	r := c.send("POST", "/api/mcp/catalog/install", map[string]any{"name": "demo", "env": map[string]any{
		"DEMO_API_KEY": "valid-demo-value", "UNRELATED_SETTING": "must-not-land"}})
	if r.Status != 400 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	detail := cronObj(t, r).GetOr("detail", "").(string)
	if !strings.Contains(detail, "UNRELATED_SETTING") || strings.Contains(detail, "valid-demo-value") || strings.Contains(detail, "must-not-land") {
		t.Fatalf("detail %q", detail)
	}
	if !envFileEmpty(t, h) {
		t.Fatal(".env written")
	}
	if servers, _ := os.ReadFile(filepath.Join(h, "config.yaml")); strings.Contains(string(servers), "demo") {
		t.Fatal("installed")
	}
}

func TestCatalogCannotDeclareReservedControlKey(t *testing.T) {
	c, h := mcpCatalogEnv(t, map[string]any{"name": "ICEBATH_YOLO_MODE", "prompt": "Unsafe control", "secret": false})
	r := c.send("POST", "/api/mcp/catalog/install", map[string]any{"name": "demo", "env": map[string]any{"ICEBATH_YOLO_MODE": "1"}})
	if r.Status != 400 || !strings.Contains(cronObj(t, r).GetOr("detail", "").(string), "denylist") {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	raw, _ := os.ReadFile(filepath.Join(h, ".env"))
	if strings.Contains(string(raw), "ICEBATH_YOLO_MODE") {
		t.Fatal(".env holds the control key")
	}
	if servers, _ := os.ReadFile(filepath.Join(h, "config.yaml")); strings.Contains(string(servers), "demo") {
		t.Fatal("installed")
	}
}
