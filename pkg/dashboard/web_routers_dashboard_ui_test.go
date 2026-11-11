package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

const exampleDashboardFixture = "../../references/projects/hermes-agent/tests/fixtures/plugins/example-dashboard"

// Copies the example dashboard fixture into the user plugins dir of home and enables it, the launch cache dropped
func installExamplePlugin(t *testing.T, h string) {
	t.Helper()
	resetDashboardPluginCache(t)
	dst := filepath.Join(h, "plugins", "example-dashboard", "dashboard")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manifest.json", "plugin_api.py"} {
		raw, err := os.ReadFile(filepath.Join(exampleDashboardFixture, "dashboard", name))
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(dst, name), string(raw))
	}
	writeTestFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled:\n  - example\n")
}

// Unauthenticated client over the test application with the example plugin installed and enabled
func assetClient(t *testing.T) *testClient {
	t.Helper()
	app := dashboardTestApp(t)
	installExamplePlugin(t, os.Getenv("ICEBATH_HOME"))
	return newTestClient(t, app)
}

func TestDashboardPluginStaticAssetAllowlistPythonSourceIs404(t *testing.T) {
	if resp := assetClient(t).get("/dashboard-plugins/example/plugin_api.py"); resp.Status != 404 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
}

func TestDashboardPluginStaticAssetAllowlistManifestJsonStillServed(t *testing.T) {
	resp := assetClient(t).get("/dashboard-plugins/example/manifest.json")
	if resp.Status != 200 || !strings.HasPrefix(resp.Headers.Get("content-type"), "application/json") {
		t.Fatalf("status %d headers %v body %s", resp.Status, resp.Raw, resp.Body)
	}
	if jsonObject(t, resp)["name"] != "example" {
		t.Fatalf("body %s", resp.Body)
	}
}

func TestDashboardPluginStaticAssetAllowlistPathTraversalStillBlocked(t *testing.T) {
	resp := assetClient(t).get("/dashboard-plugins/example/..%2Fplugin_api.py")
	if resp.Status != 403 && resp.Status != 404 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
}

const (
	catalogValidSha = "38fe0fb53eff98d477f807432e965429e665ca33"
	catalogOtherSha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// Session client whose in-tree plugin catalog is a directory holding alpha-plugin and a kill list naming bad-plugin
func catalogClient(t *testing.T) sessionClient {
	t.Helper()
	c := newSessionClient(t)
	dir := filepath.Join(t.TempDir(), "catalog")
	writeTestFile(t, filepath.Join(dir, "alpha-plugin.yaml"), "name: alpha-plugin\nrepo: https://github.com/example/alpha-plugin\n"+
		"sha: "+catalogValidSha+"\ndescription: d\nmaintainer: Example\ntier: official\n"+
		"capabilities:\n  provides_tools:\n  - tool_a\n  requires_env:\n  - EXAMPLE_API_KEY\n")
	writeTestFile(t, filepath.Join(dir, "removed.yaml"), "removed:\n- name: bad-plugin\n  repo: https://github.com/evil/bad-plugin\n"+
		"  reason: exfiltrated env vars\n")
	t.Cleanup(plugins.SwapCatalogDir(dir))
	return c
}

// User dir plugin whose installer record carries the catalog provenance of a sidecar
func installCatalogPlugin(t *testing.T, name, catalogName, sha, tier string) {
	t.Helper()
	h := os.Getenv("ICEBATH_HOME")
	writeTestFile(t, filepath.Join(h, "plugins", name, "plugin.yaml"), "name: "+name+"\nversion: '1.0'\ndescription: x\n")
	metadata, err := plugins.ReadInstallMetadata(h)
	if err != nil {
		t.Fatal(err)
	}
	metadata[name] = map[string]any{"pinned": true, "revision": sha, "source": "https://github.com/example/alpha-plugin.git",
		"catalog": map[string]any{"name": catalogName, "sha": sha, "tier": tier}}
	if err := plugins.WriteInstallMetadata(context.Background(), h, metadata); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogEndpointMergesInstalledStateFromSidecar(t *testing.T) {
	c := catalogClient(t)
	if resp := c.testClient.get("/api/dashboard/plugins/catalog"); resp.Status != 401 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	installCatalogPlugin(t, "alpha", "alpha-plugin", catalogOtherSha, "official")
	data := jsonObject(t, c.get("/api/dashboard/plugins/catalog"))
	entries := data["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entries %v", entries)
	}
	entry := entries[0].(map[string]any)
	if entry["name"] != "alpha-plugin" || entry["sha_short"] != catalogValidSha[:7] ||
		!reflect.DeepEqual(entry["capabilities"].(map[string]any)["provides_tools"], []any{"tool_a"}) {
		t.Fatalf("entry %v", entry)
	}
	if summary, _ := entry["capability_summary"].(string); !strings.Contains(summary, "tool_a") {
		t.Fatalf("summary %q", summary)
	}
	if entry["installed"] != true || entry["installed_sha"] != catalogOtherSha || entry["update_available"] != true ||
		entry["runtime_status"] != "inactive" {
		t.Fatalf("entry %v", entry)
	}
	if data["removed"].([]any)[0].(map[string]any)["reason"] != "exfiltrated env vars" {
		t.Fatalf("removed %v", data["removed"])
	}
}

func TestInstallEndpointRefusesRemovedPluginsWithNoBypass(t *testing.T) {
	c := catalogClient(t)
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	for _, body := range []map[string]any{
		{"identifier": "https://github.com/evil/bad-plugin.git"},
		{"identifier": "", "catalog_name": "bad-plugin"},
		{"identifier": "evil/bad-plugin"},
	} {
		resp := c.send("POST", "/api/dashboard/agent-plugins/install", body)
		if resp.Status != 400 {
			t.Fatalf("%v: status %d %s", body, resp.Status, resp.Body)
		}
		detail, _ := jsonObject(t, resp)["detail"].(string)
		if !strings.Contains(detail, "removed") && !strings.Contains(detail, "not in the Icebath plugin catalog") {
			t.Fatalf("%v: detail %q", body, detail)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(os.Getenv("ICEBATH_HOME"), "plugins")); err == nil && len(entries) != 0 {
		t.Fatalf("a kill listed install left %v", entries)
	}
	if resp := c.send("POST", "/api/dashboard/agent-plugins/install", map[string]any{"identifier": ""}); resp.Status != 400 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
}

// Session client whose launch home holds a GitHub token, the process hosting a profile beside its launch one
func multiplexedPluginClient(t *testing.T) sessionClient {
	t.Helper()
	c := newSessionClient(t)
	writeTestFile(t, filepath.Join(os.Getenv("ICEBATH_HOME"), ".env"), "GITHUB_TOKEN=ghp_launch_token\n")
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	return c
}

// Reads the git credential the way the clone path does and records it
func readGitCredential(t *testing.T, ctx context.Context, seen map[string]any) {
	t.Helper()
	token, _, err := provider.GetSecret(ctx, "GITHUB_TOKEN")
	if err != nil {
		t.Errorf("credential read failed: %v", err)
	}
	seen["token"] = token
}

// Stands in for the install action, reading the credential and answering result or a plain success
func stubInstallAction(t *testing.T, seen map[string]any, result map[string]any) {
	t.Helper()
	real := uiDashboardInstallPlugin
	uiDashboardInstallPlugin = func(ctx context.Context, _ plugins.DashboardInstallOptions) (map[string]any, error) {
		readGitCredential(t, ctx, seen)
		if result != nil {
			return result, nil
		}
		return map[string]any{"ok": true, "name": "dashboard_install_plugin"}, nil
	}
	t.Cleanup(func() { uiDashboardInstallPlugin = real })
}

func TestInstallRouteReadsTheGitCredentialInProfileScope(t *testing.T) {
	c := multiplexedPluginClient(t)
	seen := map[string]any{}
	stubInstallAction(t, seen, nil)
	resp := c.send("POST", "/api/dashboard/agent-plugins/install", map[string]any{"identifier": "owner/repo"})
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	if seen["token"] != "ghp_launch_token" {
		t.Fatalf("token %v", seen["token"])
	}
}

func TestUpdateRouteReadsTheGitCredentialInProfileScope(t *testing.T) {
	c := multiplexedPluginClient(t)
	seen := map[string]any{}
	real := uiDashboardUpdateUserPlugin
	uiDashboardUpdateUserPlugin = func(ctx context.Context, _ string, _, _ bool) (map[string]any, error) {
		readGitCredential(t, ctx, seen)
		return map[string]any{"ok": true, "name": "dashboard_update_user_plugin"}, nil
	}
	t.Cleanup(func() { uiDashboardUpdateUserPlugin = real })
	resp := c.send("POST", "/api/dashboard/agent-plugins/probe/update", nil)
	if resp.Status != 200 {
		t.Fatalf("status %d %s", resp.Status, resp.Body)
	}
	if seen["token"] != "ghp_launch_token" {
		t.Fatalf("token %v", seen["token"])
	}
}

func TestFailureAndRequestShapeErrorsAreUnchanged(t *testing.T) {
	c := multiplexedPluginClient(t)
	seen := map[string]any{}
	stubInstallAction(t, seen, map[string]any{"ok": false, "error": "Install failed: no such plugin."})
	failed := c.send("POST", "/api/dashboard/agent-plugins/install", map[string]any{"identifier": "owner/repo"})
	if detail, _ := jsonObject(t, failed)["detail"].(string); failed.Status != 400 || !strings.Contains(detail, "no such plugin") {
		t.Fatalf("status %d %s", failed.Status, failed.Body)
	}
	malformed := c.send("POST", "/api/dashboard/agent-plugins/install", map[string]any{"identifier": ""})
	if detail, _ := jsonObject(t, malformed)["detail"].(string); malformed.Status != 400 || !strings.Contains(detail, "catalog_name") {
		t.Fatalf("status %d %s", malformed.Status, malformed.Body)
	}
}
