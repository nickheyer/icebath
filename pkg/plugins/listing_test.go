package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Isolated home with an empty bundled tree, no entry point path and the active context
func listWorld(t *testing.T) (context.Context, string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	bundled := filepath.Join(t.TempDir(), "bundled")
	if err := os.MkdirAll(bundled, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", bundled)
	t.Setenv("PYTHONPATH", "")
	installActivationServices(t, &Services{DeclaresModelProviderKind: func(string) bool { return false }, RequiresAuthNotice: func(map[string]any) string { return "" }})
	if err := os.MkdirAll(home.PluginsDir(h), 0o755); err != nil {
		t.Fatal(err)
	}
	return home.WithOverride(context.Background(), h), h
}

func TestFilterPluginEntriesEnabledOnly(t *testing.T) {
	_, h := listWorld(t)
	entries := []PluginEntry{
		{Name: "disk-cleanup", Version: "2.0.0", Description: "Bundled", Source: "bundled", Key: "disk-cleanup"},
		{Name: "web-search-plus", Version: "2.2.0", Description: "Search", Source: "git", Key: "web-search-plus"},
		{Name: "old-plugin", Version: "1.0.0", Description: "Old", Source: "user", Key: "old-plugin"},
	}
	filtered, err := filterPluginEntries(h, entries, ListOptions{Enabled: true},
		map[string]bool{"disk-cleanup": true, "web-search-plus": true}, map[string]bool{"old-plugin": true})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range filtered {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"disk-cleanup", "web-search-plus"}) {
		t.Fatalf("%v", names)
	}
}

func TestCmdListPlainCompactOutput(t *testing.T) {
	ctx, h := listWorld(t)
	writePluginDir(t, filepath.Join(os.Getenv("ICEBATH_BUNDLED_PLUGINS"), "disk-cleanup"), "name: disk-cleanup\nversion: 2.0.0\ndescription: Bundled\n")
	writePluginDir(t, filepath.Join(h, "plugins", "web-search-plus"), "name: web-search-plus\nversion: 2.2.0\ndescription: Search\n")
	_ = os.MkdirAll(filepath.Join(h, "plugins", "web-search-plus", ".git"), 0o755)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("plugins:\n  enabled: [web-search-plus]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := CmdList(ctx, ListOptions{Plain: true, NoBundled: true}, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "web-search-plus") || !strings.Contains(text, "enabled") || strings.Contains(text, "disk-cleanup") || strings.Contains(text, "Search") {
		t.Fatalf("%q", text)
	}
}

// Site directory holding one distribution with the given entry points
func writeEntryPointSite(t *testing.T, dist, version, summary, entryPoints string) string {
	t.Helper()
	site := t.TempDir()
	info := filepath.Join(site, dist+"-"+version+".dist-info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := "Metadata-Version: 2.1\nName: " + dist + "\nVersion: " + version + "\nSummary: " + summary + "\n"
	if err := os.WriteFile(filepath.Join(info, "METADATA"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info, "entry_points.txt"), []byte(entryPoints), 0o644); err != nil {
		t.Fatal(err)
	}
	return site
}

func TestEntrypointSpecDoesNotAbortPluginStatus(t *testing.T) {
	_, h := listWorld(t)
	status, err := pluginStatus(h, PluginEntry{Name: "mnemosyne_icebath", Key: "mnemosyne_icebath", Source: "entrypoint", Dir: "mnemosyne_icebath:register"},
		map[string]bool{}, map[string]bool{}, nil, true)
	if err != nil || status != "not enabled" {
		t.Fatalf("%q %v", status, err)
	}
}

func TestUnreadablePluginDirIsSkippedByEveryManifestScan(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	ctx, h := listWorld(t)
	user := home.PluginsDir(h)
	for _, name := range []string{"denied", "good"} {
		writePluginDir(t, filepath.Join(user, name), "name: "+name+"\nversion: 1.0.0\n")
	}
	if err := os.Chmod(filepath.Join(user, "denied"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(user, "denied"), 0o700) })
	logs := captureLogs(t)
	var loader []string
	for _, m := range ScanDirectory(user, "user", nil) {
		loader = append(loader, m.Name)
	}
	entries, err := discoverAllPlugins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, e := range entries {
		listed = append(listed, e.Name)
	}
	if !slices.Equal(loader, []string{"good"}) || !slices.Equal(listed, []string{"good"}) {
		t.Fatalf("%v %v", loader, listed)
	}
	if n := strings.Count(logs.String(), "Skipping unreadable plugin directory"); n != 2 {
		t.Fatalf("%d\n%s", n, logs.String())
	}
}

func TestCmdShowPrintsDetailsAndFailsForUnknownPlugins(t *testing.T) {
	ctx, h := listWorld(t)
	writePluginDir(t, filepath.Join(h, "plugins", "events"), "name: events\nversion: 1.0.0\ndescription: Emitter\nemits: [a.b, c.d]\n")
	var out bytes.Buffer
	console := &Console{Out: &out, Width: 200}
	if err := cmdShow(ctx, "events", console); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"events v1.0.0", "Emitter", "Status: not enabled", "Source: user", "Key: events", "Runs in: Icebath process", "Emits: a.b, c.d", "Listens: (none)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	out.Reset()
	err := cmdShow(ctx, "nope", console)
	if exit, ok := err.(*CommandExit); !ok || exit.Code != 1 || !strings.Contains(out.String(), "Plugin 'nope' not found.") {
		t.Fatalf("%v %q", err, out.String())
	}
}

func TestEnabledFilterUsesKey(t *testing.T) {
	_, h := listWorld(t)
	entries := []PluginEntry{
		{Name: "web-keenable", Version: "1.0.0", Description: "search", Source: "user", Dir: t.TempDir(), Key: "web/keenable"},
		{Name: "disk-cleanup", Version: "1.0.0", Description: "cleanup", Source: "bundled", Dir: t.TempDir(), Key: "disk-cleanup"},
	}
	got, err := filterPluginEntries(h, entries, ListOptions{Enabled: true}, map[string]bool{"web/keenable": true}, map[string]bool{})
	if err != nil || len(got) != 1 || got[0].Key != "web/keenable" {
		t.Fatalf("%+v %v", got, err)
	}
}

// Decoded JSON rows of icebath plugins list --json
func listJSON(t *testing.T, ctx context.Context) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := CmdList(ctx, ListOptions{JSON: true}, &out); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("%v %q", err, out.String())
	}
	return rows
}

func TestJSONOutputIncludesCategoryPlugins(t *testing.T) {
	ctx, h := listWorld(t)
	writePluginDir(t, filepath.Join(h, "plugins", "web", "keenable"), "name: web-keenable\nversion: 1.0.0\ndescription: search\n")
	writePluginDir(t, filepath.Join(h, "plugins", "disk-cleanup"), "name: disk-cleanup\nversion: 2.0.0\ndescription: cleanup\n")
	var names []string
	for _, row := range listJSON(t, ctx) {
		names = append(names, row["name"].(string))
	}
	if !slices.Contains(names, "web-keenable") || !slices.Contains(names, "disk-cleanup") {
		t.Fatalf("%v", names)
	}
}

func TestJSONStatusUsesKey(t *testing.T) {
	ctx, h := listWorld(t)
	writePluginDir(t, filepath.Join(h, "plugins", "web", "keenable"), "name: web-keenable\nversion: 1.0.0\n")
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("plugins:\n  enabled: [web/keenable]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows := listJSON(t, ctx)
	if len(rows) != 1 || rows[0]["status"] != "enabled" || rows[0]["removed"] != nil {
		t.Fatalf("%v", rows)
	}
}
