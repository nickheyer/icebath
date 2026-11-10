package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pytoml"
)

func codexMigrationEntry(pairs ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i < len(pairs); i += 2 {
		o.Set(pairs[i].(string), pairs[i+1])
	}
	return o
}

func codexMigrationLeftovers(t *testing.T, dirs ...string) []string {
	t.Helper()
	var out []string
	for _, dir := range dirs {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".config.toml.") {
				out = append(out, e.Name())
			}
		}
	}
	return out
}

func codexMigrationRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func codexMigrationPatchQuery(t *testing.T, fn func(codexHome string, timeout float64, codexBin string) ([]map[string]any, string, error)) {
	t.Helper()
	prev := codexQueryCodexPlugins
	codexQueryCodexPlugins = fn
	t.Cleanup(func() { codexQueryCodexPlugins = prev })
}

func codexMigrateNoExtras(t *testing.T, cfg map[string]any, home string) *CodexMigrationReport {
	t.Helper()
	report, err := MigrateCodexRuntimePlugins(context.Background(), cfg, CodexMigrateOptions{CodexHome: home, SkipPluginDiscovery: true, SkipToolsMCPServer: true})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestCodexMigrationTranslateOneServer(t *testing.T) {
	t.Run("test_stdio_basic", func(t *testing.T) {
		out, skipped, err := codexTranslateOneServer("filesystem", map[string]any{
			"command": "npx",
			"args":    []any{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
			"env":     map[string]any{"FOO": "bar"},
		})
		if err != nil || len(skipped) != 0 {
			t.Fatalf("skipped %v err %v", skipped, err)
		}
		got, _ := pyjson.Dumps(out)
		if got != `{"command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"], "env": {"FOO": "bar"}}` {
			t.Fatalf("entry %s", got)
		}
	})
	t.Run("test_unknown_keys_warned", func(t *testing.T) {
		out, skipped, _ := codexTranslateOneServer("x", map[string]any{"command": "y", "totally_made_up_key": "value"})
		if _, ok := out.Get("totally_made_up_key"); ok {
			t.Fatal("unknown key copied")
		}
		if !slices.ContainsFunc(skipped, func(s string) bool { return strings.Contains(s, "totally_made_up_key") }) {
			t.Fatalf("skipped %v", skipped)
		}
	})
	t.Run("url transport timeouts and flags", func(t *testing.T) {
		cfg := pyjson.NewObject()
		cfg.Set("url", "https://h/mcp")
		cfg.Set("headers", map[string]any{"Authorization": "Bearer x", "N": 1})
		cfg.Set("transport", "sse")
		cfg.Set("timeout", "12")
		cfg.Set("connect_timeout", "soon")
		cfg.Set("enabled", false)
		cfg.Set("sampling", map[string]any{"x": 1})
		out, skipped, err := codexTranslateOneServer("h", cfg)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := pyjson.Dumps(out)
		if got != `{"url": "https://h/mcp", "http_headers": {"Authorization": "Bearer x", "N": "1"}, "tool_timeout_sec": 12.0, "enabled": false}` {
			t.Fatalf("entry %s", got)
		}
		want := []string{"transport=sse (codex auto-negotiates)", "connect_timeout (not numeric)", "sampling (no codex equivalent)"}
		if !slices.Equal(skipped, want) {
			t.Fatalf("skipped %q", skipped)
		}
	})
	t.Run("command wins over url and odd shapes", func(t *testing.T) {
		out, skipped, _ := codexTranslateOneServer("s", map[string]any{"command": "c", "url": "u", "args": "ab", "cwd": "/w", "timeout": nil})
		got, _ := pyjson.Dumps(out)
		if got != `{"command": "c", "args": ["a", "b"], "cwd": "/w"}` || !slices.Equal(skipped, []string{"url (both command and url set; preferring stdio)", "timeout (not numeric)"}) {
			t.Fatalf("entry %s skipped %q", got, skipped)
		}
		if out, skipped, _ := codexTranslateOneServer("s", map[string]any{"description": "d"}); out != nil || !slices.Equal(skipped, []string{"no command or url field"}) {
			t.Fatalf("no transport %v %v", out, skipped)
		}
		if out, skipped, _ := codexTranslateOneServer("s", []any{"x"}); out != nil || len(skipped) != 0 {
			t.Fatalf("non dict %v %v", out, skipped)
		}
		if _, _, err := codexTranslateOneServer("s", map[string]any{"command": "c", "env": []any{"A=1"}}); err == nil || err.Error() != "'list' object has no attribute 'items'" {
			t.Fatalf("env list error %v", err)
		}
		if _, _, err := codexTranslateOneServer("s", map[string]any{"command": "c", "args": 3}); err == nil || err.Error() != "'int' object is not iterable" {
			t.Fatalf("args int error %v", err)
		}
	})
}

func TestCodexMigrationTOMLValueFormatter(t *testing.T) {
	t.Run("test_atomic_write_no_temp_leak_on_success", func(t *testing.T) {
		home := t.TempDir()
		report, err := MigrateCodexRuntimePlugins(context.Background(), map[string]any{"mcp_servers": map[string]any{"x": map[string]any{"command": "y"}}},
			CodexMigrateOptions{CodexHome: home, SkipPluginDiscovery: true, SkipToolsMCPServer: true, NoDefaultPermissionProfile: true})
		if err != nil || !report.Written {
			t.Fatalf("report %+v err %v", report, err)
		}
		if _, err := os.Stat(filepath.Join(home, "config.toml")); err != nil {
			t.Fatal(err)
		}
		if left := codexMigrationLeftovers(t, home); len(left) != 0 {
			t.Fatalf("temp file leaked after migration: %v", left)
		}
	})
	t.Run("test_atomic_write_cleanup_on_rename_failure", func(t *testing.T) {
		home := t.TempDir()
		locked := filepath.Join(t.TempDir(), "locked")
		if err := os.Mkdir(locked, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(locked, 0o755) })
		if err := os.Symlink(filepath.Join(locked, "config.toml"), filepath.Join(home, "config.toml")); err != nil {
			t.Fatal(err)
		}
		report, err := MigrateCodexRuntimePlugins(context.Background(), map[string]any{"mcp_servers": map[string]any{"x": map[string]any{"command": "y"}}},
			CodexMigrateOptions{CodexHome: home, SkipPluginDiscovery: true, SkipToolsMCPServer: true, NoDefaultPermissionProfile: true})
		if err != nil {
			t.Fatal(err)
		}
		if report.Written || !slices.ContainsFunc(report.Errors, func(e string) bool { return strings.Contains(e, "Permission denied") }) {
			t.Fatalf("rename failure not surfaced %+v", report)
		}
		if left := codexMigrationLeftovers(t, home, locked); len(left) != 0 {
			t.Fatalf("temp files leaked: %v", left)
		}
	})
	t.Run("scalar list and table forms", func(t *testing.T) {
		cases := map[string]any{
			"true": true, "7": 7, "7.0": 7.0, "1e+16": 1e16, `"a\\b\"c\b\t\n\f\r"`: "a\\b\"c\b\t\n\f\r",
			`["x", 2]`: []any{"x", 2}, "{}": map[string]any{}, `{ a-b_1 = "v", "k y" = false, "q\"\\" = 1 }`: codexMigrationEntry("a-b_1", "v", "k y", false, "q\"\\", 1),
		}
		for want, value := range cases {
			if got, err := codexFormatTOMLValue(value); err != nil || got != want {
				t.Fatalf("format %#v = %q %v, want %q", value, got, err, want)
			}
		}
		if _, err := codexFormatTOMLValue(nil); err == nil || err.Error() != "Unsupported TOML value type: NoneType" {
			t.Fatalf("None error %v", err)
		}
		if codexQuoteTOMLKey("") != `""` || codexQuoteTOMLKey("é1") != "é1" || codexQuoteTOMLKey("a@b") != `"a@b"` {
			t.Fatal("key quoting")
		}
	})
}

func TestCodexMigrationRenderTOML(t *testing.T) {
	t.Run("test_servers_sorted_alphabetically", func(t *testing.T) {
		out, err := RenderCodexTOMLSection(map[string]*pyjson.Object{
			"zoo": codexMigrationEntry("command", "z"), "alpha": codexMigrationEntry("command", "a"), "middle": codexMigrationEntry("command", "m"),
		}, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		a, m, z := strings.Index(out, "[mcp_servers.alpha]"), strings.Index(out, "[mcp_servers.middle]"), strings.Index(out, "[mcp_servers.zoo]")
		if !(0 < a && a < m && m < z) {
			t.Fatalf("order %d %d %d in\n%s", a, m, z, out)
		}
	})
	t.Run("test_server_with_args_and_env", func(t *testing.T) {
		out, _ := RenderCodexTOMLSection(map[string]*pyjson.Object{
			"fs": codexMigrationEntry("command", "npx", "args", []any{"-y", "filesystem"}, "env", codexMigrationEntry("PATH", "/usr/bin")),
		}, nil, "")
		for _, want := range []string{"[mcp_servers.fs]", `command = "npx"`, `args = ["-y", "filesystem"]`, `env = { PATH = "/usr/bin" }`} {
			if !strings.Contains(out, want) {
				t.Fatalf("missing %s in\n%s", want, out)
			}
		}
	})
	t.Run("empty block permissions and plugins", func(t *testing.T) {
		empty, _ := RenderCodexTOMLSection(nil, nil, "")
		if empty != CodexMigrationMarker+"\n# (no MCP servers, plugins, or permissions configured by Icebath)\n"+CodexMigrationEndMarker+"\n" {
			t.Fatalf("empty %q", empty)
		}
		out, _ := RenderCodexTOMLSection(nil, []map[string]any{
			{"name": "b", "marketplace": "m", "enabled": false}, {"name": "a"}, {"name": "c", "marketplace": nil, "enabled": 1},
		}, "workspace-write")
		want := CodexMigrationMarker + "\n\ndefault_permissions = \":workspace-write\"\n\n[plugins.\"a@openai-curated\"]\nenabled = true\n\n[plugins.\"b@m\"]\nenabled = false\n\n[plugins.\"c@openai-curated\"]\nenabled = true\n\n" + CodexMigrationEndMarker + "\n"
		if out != want {
			t.Fatalf("render\n%s\nwant\n%s", out, want)
		}
	})
}

func TestCodexMigrationStripExistingManagedBlock(t *testing.T) {
	t.Run("test_no_managed_block_unchanged", func(t *testing.T) {
		text := "[other]\nfoo = 1\n"
		if got := codexStripExistingManagedBlock(text); got != text {
			t.Fatalf("changed %q", got)
		}
	})
	t.Run("test_preserves_user_content_above_managed_block", func(t *testing.T) {
		out := codexStripExistingManagedBlock("[model]\nname = \"gpt-5.5\"\n\n" + CodexMigrationMarker + "\n[mcp_servers.fs]\ncommand = \"x\"\n")
		if !strings.Contains(out, "[model]") || !strings.Contains(out, `name = "gpt-5.5"`) || strings.Contains(out, "mcp_servers.fs") {
			t.Fatalf("stripped %q", out)
		}
	})
	t.Run("end marker and legacy block", func(t *testing.T) {
		text := "a = 1\n" + CodexMigrationMarker + "\nx = 2\n[mcp_servers.m]\n" + CodexMigrationEndMarker + "\n[mcp_servers.user]\ncommand = \"u\"\n"
		if got := codexStripExistingManagedBlock(text); got != "a = 1\n[mcp_servers.user]\ncommand = \"u\"\n" {
			t.Fatalf("bounded %q", got)
		}
		legacy := CodexMigrationMarker + "\n[permissions]\nx = 1\n[plugins.\"p@m\"]\nenabled = true\n[features]\ny = 2\n"
		if got := codexStripExistingManagedBlock(legacy); got != "[features]\ny = 2\n" {
			t.Fatalf("legacy %q", got)
		}
	})
	t.Run("insert at top level", func(t *testing.T) {
		block := "B\n"
		for in, want := range map[string]string{
			"  \n":                          block,
			"model = \"m\"\n\n":             "model = \"m\"\n\n" + block,
			"[features]\nx = 1\n":           block + "\n[features]\nx = 1\n",
			"model = \"m\"\n\n[f]\nx = 1\n": "model = \"m\"\n\n" + block + "\n[f]\nx = 1\n",
		} {
			if got := codexInsertManagedBlockAtTopLevel(in, block); got != want {
				t.Fatalf("insert %q = %q, want %q", in, got, want)
			}
		}
	})
}

func TestCodexMigrationMigrate(t *testing.T) {
	t.Run("test_plugin_discovery_writes_plugin_blocks", func(t *testing.T) {
		seen := ""
		codexMigrationPatchQuery(t, func(_ string, _ float64, codexBin string) ([]map[string]any, string, error) {
			seen = codexBin
			return []map[string]any{
				{"name": "google-calendar", "marketplace": "openai-curated", "enabled": true},
				{"name": "github", "marketplace": "openai-curated", "enabled": true},
			}, "", nil
		})
		home := t.TempDir()
		report, err := MigrateCodexRuntimePlugins(context.Background(), map[string]any{"model": map[string]any{"codex_bin": "/opt/codex-app/codex"}}, CodexMigrateOptions{CodexHome: home})
		if err != nil {
			t.Fatal(err)
		}
		if seen != "/opt/codex-app/codex" {
			t.Fatalf("codex_bin %q", seen)
		}
		text := codexMigrationRead(t, filepath.Join(home, "config.toml"))
		for _, want := range []string{`[plugins."github@openai-curated"]`, `[plugins."google-calendar@openai-curated"]`, "enabled = true"} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %s in\n%s", want, text)
			}
		}
		if !slices.Contains(report.MigratedPlugins, "google-calendar@openai-curated") || !slices.Contains(report.MigratedPlugins, "github@openai-curated") {
			t.Fatalf("migrated plugins %v", report.MigratedPlugins)
		}
	})
	t.Run("test_plugin_discovery_failure_non_fatal", func(t *testing.T) {
		codexMigrationPatchQuery(t, func(string, float64, string) ([]map[string]any, string, error) {
			return []map[string]any{}, "codex CLI not available", nil
		})
		report, err := MigrateCodexRuntimePlugins(context.Background(), map[string]any{"mcp_servers": map[string]any{"x": map[string]any{"command": "y"}}},
			CodexMigrateOptions{CodexHome: t.TempDir(), SkipToolsMCPServer: true})
		if err != nil || !report.Written || !slices.Equal(report.Migrated, []string{"x"}) || report.PluginQueryError != "codex CLI not available" || len(report.MigratedPlugins) != 0 {
			t.Fatalf("report %+v err %v", report, err)
		}
	})
	t.Run("test_full_migration_round_trip", func(t *testing.T) {
		installCodexEnvSources(t)
		home := t.TempDir()
		report, err := MigrateCodexRuntimePlugins(context.Background(), map[string]any{"mcp_servers": map[string]any{
			"filesystem": map[string]any{"command": "npx", "args": []any{"-y", "@modelcontextprotocol/server-filesystem"}},
			"github":     map[string]any{"url": "https://api.github.com/mcp", "headers": map[string]any{"Authorization": "Bearer x"}},
		}}, CodexMigrateOptions{CodexHome: home, SkipToolsMCPServer: true})
		if err != nil || !report.Written {
			t.Fatalf("report %+v err %v", report, err)
		}
		text := codexMigrationRead(t, filepath.Join(home, "config.toml"))
		for _, want := range []string{"[mcp_servers.filesystem]", "[mcp_servers.github]", `command = "npx"`, `url = "https://api.github.com/mcp"`} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %s in\n%s", want, text)
			}
		}
	})
	t.Run("test_preserves_user_mcp_server_outside_managed_block", func(t *testing.T) {
		home := t.TempDir()
		target := filepath.Join(home, "config.toml")
		os.WriteFile(target, []byte("[mcp_servers.user-above]\ncommand = \"/usr/bin/above-server\"\nargs = [\"--above\"]\n"), 0o644)
		cfg := map[string]any{"mcp_servers": map[string]any{"icebath-mcp": map[string]any{"command": "npx"}}}
		codexMigrateNoExtras(t, cfg, home)
		text := codexMigrationRead(t, target)
		if !strings.Contains(text, "user-above") || !strings.Contains(text, `command = "/usr/bin/above-server"`) {
			t.Fatalf("user MCP server above managed block got nuked\n%s", text)
		}
		os.WriteFile(target, []byte(text+"\n[mcp_servers.user-below]\ncommand = \"below-server\"\n"), 0o644)
		codexMigrateNoExtras(t, cfg, home)
		final := codexMigrationRead(t, target)
		if !strings.Contains(final, "user-above") || !strings.Contains(final, "user-below") || !strings.Contains(final, "[mcp_servers.icebath-mcp]") {
			t.Fatalf("final\n%s", final)
		}
		if _, err := pytoml.Loads(final); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("report errors and refusals", func(t *testing.T) {
		home := t.TempDir()
		report := codexMigrateNoExtras(t, map[string]any{"mcp_servers": []any{"x"}}, home)
		if !slices.Equal(report.Errors, []string{"mcp_servers in Icebath config is not a dict; cannot migrate."}) {
			t.Fatalf("not a dict %v", report.Errors)
		}
		report = codexMigrateNoExtras(t, map[string]any{"mcp_servers": map[string]any{"a": "", "b": 5, "c": map[string]any{"cwd": "/"}, "d": map[string]any{"command": "x\x01"}}}, home)
		want := []string{"server 'a' skipped: no command or url field", "server 'b' skipped: no transport configured", "server 'c' skipped: no command or url field"}
		if !slices.Equal(report.Errors[:3], want) || !strings.HasPrefix(report.Errors[3], "refusing to write "+filepath.Join(home, "config.toml")+": rendered config is not valid TOML (") || report.Written {
			t.Fatalf("errors %q", report.Errors)
		}
		if _, err := os.Stat(filepath.Join(home, "config.toml")); err == nil {
			t.Fatal("refused config written")
		}
		os.WriteFile(filepath.Join(home, "config.toml"), []byte("[a]\nx = 1\n[a]\n"), 0o644)
		codexMigrationPatchQuery(t, func(string, float64, string) ([]map[string]any, string, error) {
			return []map[string]any{}, "boom", nil
		})
		report, _ = MigrateCodexRuntimePlugins(context.Background(), nil, CodexMigrateOptions{CodexHome: home, SkipToolsMCPServer: true})
		if report.PluginQueryError != "boom; existing config.toml was unloadable — re-run `icebath codex-runtime migrate` to migrate plugins" {
			t.Fatalf("plugin error %q", report.PluginQueryError)
		}
		os.Remove(filepath.Join(home, "config.toml"))
		os.Mkdir(filepath.Join(home, "config.toml"), 0o755)
		report = codexMigrateNoExtras(t, nil, home)
		if len(report.Errors) != 1 || !strings.HasPrefix(report.Errors[0], "could not read "+filepath.Join(home, "config.toml")+": [Errno 21] Is a directory") {
			t.Fatalf("read error %q", report.Errors)
		}
	})
	t.Run("summary", func(t *testing.T) {
		r := &CodexMigrationReport{TargetPath: "/c/config.toml", Written: true, Migrated: []string{"a", "b"}, SkippedKeysPerServer: map[string][]string{"a": {"x (unknown Icebath key)", "sampling (no codex equivalent)"}},
			PluginQueryError: "down", WrotePermissionsDefault: ":workspace", PreservedUserServers: []string{"g"}, Errors: []string{"e1"}}
		want := "Wrote /c/config.toml\nMigrated 2 MCP server(s):\n  - a (skipped: x (unknown Icebath key), sampling (no codex equivalent))\n  - b\nCodex plugin discovery skipped: down\nWrote default_permissions = ':workspace'\nKept 1 user-owned MCP server(s) already in config.toml (Icebath projection skipped): g\n⚠ e1"
		if got := r.Summary(); got != want {
			t.Fatalf("summary\n%s", got)
		}
		r = &CodexMigrationReport{TargetPath: "/c/config.toml", DryRun: true, MigratedPlugins: []string{"p@m"}, PluginQueryError: "hidden"}
		if got := r.Summary(); got != "(dry run) Would write /c/config.toml\nNo MCP servers found in Icebath config.\nMigrated 1 native Codex plugin(s):\n  - p@m" {
			t.Fatalf("dry summary\n%s", got)
		}
	})
}

func TestCodexMigrationStripUnmanagedPluginTables(t *testing.T) {
	t.Run("test_strips_plugin_tables_outside_managed_block", func(t *testing.T) {
		stripped := codexStripUnmanagedPluginTables("model = \"gpt-5.5\"\n\n[mcp_servers.user-thing]\ncommand = \"x\"\n\n[plugins.\"tasks@openai-curated\"]\nenabled = true\n\n[plugins.\"web-search@openai-curated\"]\nenabled = true\n\n[features]\nterminal_resize_reflow = true\n")
		if strings.Contains(stripped, "[plugins.") || !strings.Contains(stripped, "[mcp_servers.user-thing]") || !strings.Contains(stripped, "[features]") || !strings.Contains(stripped, "terminal_resize_reflow = true") {
			t.Fatalf("stripped %q", stripped)
		}
	})
	t.Run("test_multi_line_array_in_plugin_table_does_not_leak", func(t *testing.T) {
		stripped := codexStripUnmanagedPluginTables("[plugins.\"tasks@openai-curated\"]\nallowed = [\n  \"a\",\n  [\"nested\"],\n]\n[features]\nx = 1\n")
		if strings.Contains(stripped, `["nested"]`) || strings.Contains(stripped, "allowed") || !strings.Contains(stripped, "[features]") || !strings.Contains(stripped, "x = 1") {
			t.Fatalf("stripped %q", stripped)
		}
		if _, err := pytoml.Loads(stripped); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("test_migrate_dedups_codex_owned_plugin_tables", func(t *testing.T) {
		home := t.TempDir()
		target := filepath.Join(home, "config.toml")
		os.WriteFile(target, []byte("[mcp_servers.user-server]\ncommand = \"x\"\n\n[plugins.\"tasks@openai-curated\"]\nenabled = true\n"), 0o644)
		codexMigrationPatchQuery(t, func(string, float64, string) ([]map[string]any, string, error) {
			return []map[string]any{{"name": "tasks", "marketplace": "openai-curated", "enabled": true}}, "", nil
		})
		if _, err := MigrateCodexRuntimePlugins(context.Background(), map[string]any{}, CodexMigrateOptions{CodexHome: home, SkipToolsMCPServer: true}); err != nil {
			t.Fatal(err)
		}
		text := codexMigrationRead(t, target)
		header := `[plugins."tasks@openai-curated"]`
		if strings.Count(text, header) != 1 {
			t.Fatalf("headers in\n%s", text)
		}
		start, end, idx := strings.Index(text, CodexMigrationMarker), strings.Index(text, CodexMigrationEndMarker), strings.Index(text, header)
		if !(start < idx && idx < end) {
			t.Fatalf("plugin outside managed block\n%s", text)
		}
		if _, err := pytoml.Loads(text); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("user server names by header scan", func(t *testing.T) {
		names, err := codexUnmanagedMCPServerNames("[mcp_servers.a]\nx = [\n[mcp_servers.\"b c\".env]\n  [ mcp_servers.d.env ] # c\nk = [x]\n[[mcp_servers.e]]\n")
		if err != nil || len(names) != 2 || !names["a"] || !names["b c"] {
			t.Fatalf("names %v %v", names, err)
		}
		if _, err := codexUnmanagedMCPServerNames("[mcp_servers.\"open]\n"); err == nil {
			t.Fatal("unterminated quoted name parsed")
		}
		if codexLooksLikeTableHeader("key = [x]") || !codexLooksLikeTableHeader("[a] # c") || codexLooksLikeTableHeader("[a = b]") || codexLooksLikeTableHeader("[\"x\",") {
			t.Fatal("header detection")
		}
	})
}

func TestCodexMigrationPluginQuery(t *testing.T) {
	prev := codexPluginQueryClient
	t.Cleanup(func() { codexPluginQueryClient = prev })
	var opened CodexAppServerOptions
	fake := &fakeCodexClient{}
	codexPluginQueryClient = func(opts CodexAppServerOptions) (CodexRPCClient, error) {
		opened = opts
		return fake, nil
	}
	fake.handler = func(method string, _ any) (any, error) {
		return map[string]any{"marketplaces": []any{
			"junk",
			map[string]any{"plugins": []any{
				map[string]any{"name": "tasks", "installed": true},
				map[string]any{"name": "tasks", "installed": true, "enabled": false},
				map[string]any{"name": "off", "installed": false},
				map[string]any{"name": "broken", "installed": true, "availability": "needs_auth"},
				map[string]any{"name": "ok", "installed": true, "availability": "available", "enabled": false},
				map[string]any{"installed": true},
			}},
			map[string]any{"name": "team", "plugins": "nope"},
			map[string]any{"name": "team", "plugins": []any{map[string]any{"name": "tasks", "installed": 1}}},
		}}, nil
	}
	plugins, reason, err := codexQueryCodexPlugins("/ch", 8, "/bin/codex")
	if err != nil || reason != "" || opened.CodexBin != "/bin/codex" || opened.CodexHome != "/ch" || fake.requests[0].method != "plugin/list" || !fake.closed {
		t.Fatalf("query %v %q %v %+v %v", plugins, reason, err, opened, fake.closed)
	}
	got, _ := pyjson.Dumps(plugins, pyjson.SortKeys(true))
	if got != `[{"enabled": true, "marketplace": "openai-curated", "name": "tasks"}, {"enabled": false, "marketplace": "openai-curated", "name": "ok"}, {"enabled": true, "marketplace": "team", "name": "tasks"}]` {
		t.Fatalf("plugins %s", got)
	}
	fake.handler = func(string, any) (any, error) { return map[string]any{"marketplaces": "x"}, nil }
	if _, reason, _ = codexQueryCodexPlugins("", 8, "codex"); reason != "plugin/list response missing 'marketplaces'" {
		t.Fatalf("reason %q", reason)
	}
	fake.handler = func(string, any) (any, error) { return nil, errors.New("rpc down") }
	if _, reason, _ = codexQueryCodexPlugins("", 8, "codex"); reason != "plugin/list query failed: rpc down" {
		t.Fatalf("reason %q", reason)
	}
	fake.handler = func(string, any) (any, error) { return nil, nil }
	if _, _, err = codexQueryCodexPlugins("", 8, "codex"); err == nil || err.Error() != "'NoneType' object has no attribute 'get'" {
		t.Fatalf("non object err %v", err)
	}
	codexPluginQueryClient = func(CodexAppServerOptions) (CodexRPCClient, error) { return nil, errors.New("no such binary") }
	if _, reason, _ = codexQueryCodexPlugins("", 8, "codex"); reason != "plugin/list query failed: no such binary" {
		t.Fatalf("spawn reason %q", reason)
	}
}

func TestCodexMigrationIcebathHomeLeakGuard(t *testing.T) {
	t.Run("test_real_hermes_home_propagates", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", "/Users/alice/.icebath")
		env, _ := codexBuildToolsMCPEntry().Get("env")
		if v, _ := env.(*pyjson.Object).Get("ICEBATH_HOME"); v != "/Users/alice/.icebath" {
			t.Fatalf("env %v", env)
		}
	})
	t.Run("test_unset_hermes_home_omits_env_key", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", "")
		os.Unsetenv("ICEBATH_HOME")
		env, _ := codexBuildToolsMCPEntry().Get("env")
		if _, ok := env.(*pyjson.Object).Get("ICEBATH_HOME"); ok {
			t.Fatalf("ICEBATH_HOME should not be set when env var is unset, got %v", env)
		}
	})
	t.Run("tempdir guard and entry shape", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", "/tmp/pytest-of-nick/pytest-3/home")
		t.Setenv("PYTHONPATH", "/wt")
		exe, _ := os.Executable()
		got, _ := pyjson.Dumps(codexBuildToolsMCPEntry())
		want, _ := pyjson.Dumps(codexMigrationEntry("command", exe, "args", []any{CodexToolsMCPServerEntry},
			"env", codexMigrationEntry("PYTHONPATH", "/wt", "ICEBATH_QUIET", "1", "ICEBATH_REDACT_SECRETS", "true"),
			"startup_timeout_sec", 30.0, "tool_timeout_sec", 600.0))
		if got != want {
			t.Fatalf("entry %s", got)
		}
		if !codexLooksLikeTestTempdir("/private/var/folders/x/T") || codexLooksLikeTestTempdir("") || codexLooksLikeTestTempdir("/home/a/.icebath") {
			t.Fatal("tempdir needles")
		}
	})
}

func TestCodexMigrationSameNameUserMCPTable(t *testing.T) {
	cfg := map[string]any{"mcp_servers": map[string]any{"gbrain": map[string]any{"command": "projected-gbrain"}, "other": map[string]any{"command": "o"}}}
	opts := func(home string) CodexMigrateOptions {
		return CodexMigrateOptions{CodexHome: home, SkipPluginDiscovery: true, SkipToolsMCPServer: true, NoDefaultPermissionProfile: true}
	}
	t.Run("test_user_table_wins_and_output_stays_valid_toml", func(t *testing.T) {
		home := t.TempDir()
		target := filepath.Join(home, "config.toml")
		os.WriteFile(target, []byte("[mcp_servers.gbrain]\ncommand = \"existing-gbrain\"\n"), 0o644)
		report, err := MigrateCodexRuntimePlugins(context.Background(), cfg, opts(home))
		if err != nil {
			t.Fatal(err)
		}
		text := codexMigrationRead(t, target)
		parsed, err := pytoml.Loads(text)
		if err != nil {
			t.Fatal(err)
		}
		servers := parsed["mcp_servers"].(map[string]any)
		if strings.Count(text, "[mcp_servers.gbrain]") != 1 || servers["gbrain"].(map[string]any)["command"] != "existing-gbrain" || servers["other"] == nil {
			t.Fatalf("text\n%s", text)
		}
		if !slices.Equal(report.PreservedUserServers, []string{"gbrain"}) || !slices.Equal(report.Migrated, []string{"other"}) || !strings.Contains(report.Summary(), "gbrain") {
			t.Fatalf("report %+v", report)
		}
	})
	t.Run("test_inline_table_user_server_is_preserved", func(t *testing.T) {
		home := t.TempDir()
		target := filepath.Join(home, "config.toml")
		os.WriteFile(target, []byte("[mcp_servers]\ngbrain = { command = \"existing-gbrain\" }\n"), 0o644)
		report, err := MigrateCodexRuntimePlugins(context.Background(), cfg, opts(home))
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := pytoml.Loads(codexMigrationRead(t, target))
		if err != nil {
			t.Fatal(err)
		}
		servers := parsed["mcp_servers"].(map[string]any)
		if !report.Written || len(report.Errors) != 0 || !slices.Equal(report.PreservedUserServers, []string{"gbrain"}) ||
			servers["gbrain"].(map[string]any)["command"] != "existing-gbrain" || servers["other"].(map[string]any)["command"] != "o" {
			t.Fatalf("report %+v parsed %v", report, servers)
		}
	})
	t.Run("test_cli_migrate_dry_run_json_reports_without_writing", func(t *testing.T) {
		tmp := t.TempDir()
		codexHome := filepath.Join(tmp, "alt-codex")
		os.Mkdir(codexHome, 0o755)
		target := filepath.Join(codexHome, "config.toml")
		original := "[mcp_servers.gbrain]\ncommand = \"existing-gbrain\"\n"
		os.WriteFile(target, []byte(original), 0o644)
		t.Setenv("CODEX_HOME", codexHome)
		t.Setenv("HOME", tmp)
		icebathHome := filepath.Join(tmp, "icebath-home")
		os.Mkdir(icebathHome, 0o755)
		os.WriteFile(filepath.Join(icebathHome, "config.yaml"), []byte("mcp_servers:\n  gbrain:\n    command: projected\n  other:\n    command: o\n"), 0o644)
		var out strings.Builder
		rc, err := CodexRuntimeCommand(t.Context(), icebathHome, map[string]any{"dry_run": true, "json": true}, &out)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(out.String()), &payload); err != nil {
			t.Fatalf("output is not JSON: %v\n%s", err, out.String())
		}
		if rc != 0 {
			t.Fatalf("rc = %d, want 0", rc)
		}
		if payload["dry_run"] != true || payload["written"] != false {
			t.Fatalf("dry_run %v written %v", payload["dry_run"], payload["written"])
		}
		if kept, _ := payload["preserved_user_servers"].([]any); len(kept) != 1 || kept[0] != "gbrain" {
			t.Fatalf("preserved_user_servers %v", payload["preserved_user_servers"])
		}
		if payload["target_path"] != target {
			t.Fatalf("target_path %v, want %s", payload["target_path"], target)
		}
		if codexMigrationRead(t, target) != original {
			t.Fatal("dry run wrote config.toml")
		}
		want := "{\n  \"dry_run\": true,\n  \"errors\": [],\n  \"migrated\": [\n    \"other\",\n    \"" + CodexToolsMCPServerName + "\"\n  ],\n  \"migrated_plugins\": [],\n  \"plugin_query_error\": null,\n  \"preserved_user_servers\": [\n    \"gbrain\"\n  ],\n  \"skipped_keys_per_server\": {},\n  \"target_path\": \"" + target + "\",\n  \"written\": false,\n  \"wrote_permissions_default\": \":workspace\"\n}\n"
		if out.String() != want {
			t.Fatalf("output\n%s\nwant\n%s", out.String(), want)
		}
	})
}

func TestCodexRuntimeCommandPrintsTheSummaryAndExitsOneOnErrors(t *testing.T) {
	tmp := t.TempDir()
	codexHome := filepath.Join(tmp, "alt-codex")
	os.Mkdir(codexHome, 0o755)
	target := filepath.Join(codexHome, "config.toml")
	original := "[mcp_servers.gbrain]\ncommand = \"existing-gbrain\"\n"
	os.WriteFile(target, []byte(original), 0o644)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("HOME", tmp)
	good := filepath.Join(tmp, "good")
	bad := filepath.Join(tmp, "bad")
	os.Mkdir(good, 0o755)
	os.Mkdir(bad, 0o755)
	os.WriteFile(filepath.Join(good, "config.yaml"), []byte("mcp_servers:\n  gbrain:\n    command: projected\n  other:\n    command: o\n"), 0o644)
	os.WriteFile(filepath.Join(bad, "config.yaml"), []byte("mcp_servers:\n- x\n"), 0o644)
	cases := []struct {
		name string
		home string
		ns   map[string]any
		rc   int
		want string
	}{
		{"summary", good, map[string]any{"dry_run": true, "json": false}, 0,
			"(dry run) Would write " + target + "\nMigrated 2 MCP server(s):\n  - other\n  - " + CodexToolsMCPServerName + "\nWrote default_permissions = ':workspace'\nKept 1 user-owned MCP server(s) already in config.toml (Icebath projection skipped): gbrain\n"},
		{"errors as json", bad, map[string]any{"dry_run": true, "json": true}, 1,
			"{\n  \"dry_run\": true,\n  \"errors\": [\n    \"mcp_servers in Icebath config is not a dict; cannot migrate.\"\n  ],\n  \"migrated\": [],\n  \"migrated_plugins\": [],\n  \"plugin_query_error\": null,\n  \"preserved_user_servers\": [],\n  \"skipped_keys_per_server\": {},\n  \"target_path\": \"" + target + "\",\n  \"written\": false,\n  \"wrote_permissions_default\": null\n}\n"},
		{"errors as summary", bad, map[string]any{"dry_run": true, "json": false}, 1,
			"(dry run) Would write " + target + "\nNo MCP servers found in Icebath config.\n⚠ mcp_servers in Icebath config is not a dict; cannot migrate.\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out strings.Builder
			rc, err := CodexRuntimeCommand(t.Context(), c.home, c.ns, &out)
			if err != nil {
				t.Fatal(err)
			}
			if rc != c.rc {
				t.Fatalf("rc = %d, want %d", rc, c.rc)
			}
			if out.String() != c.want {
				t.Fatalf("output\n%q\nwant\n%q", out.String(), c.want)
			}
		})
	}
	if codexMigrationRead(t, target) != original {
		t.Fatal("dry run wrote config.toml")
	}
}

func TestCodexMigrationWorkerOverridesTargetTheMigratedServer(t *testing.T) {
	installCodexEnvSources(t)
	tmp := t.TempDir()
	t.Setenv("ICEBATH_NON_DISPATCHER_OWNED_CONTEXT", "")
	t.Setenv("ICEBATH_DELEGATED_CHILD_CONTEXT", "")
	t.Setenv("ICEBATH_KANBAN_TASK", "11111111-1111-4111-8111-111111111111")
	t.Setenv("ICEBATH_KANBAN_RUN_ID", "42")
	t.Setenv("ICEBATH_KANBAN_DB", filepath.Join(tmp, "board", "kanban.db"))
	argv, _, _ := codexAppServerSpawn(CodexAppServerOptions{CodexBin: "codex", CodexHome: filepath.Join(tmp, "codex")})
	var overrides []string
	for _, arg := range argv {
		if strings.HasPrefix(arg, "mcp_servers.") {
			overrides = append(overrides, arg)
		}
	}
	if len(overrides) == 0 {
		t.Fatal("dispatcher-owned worker must scope the managed MCP endpoint")
	}
	targeted := map[string]bool{}
	for _, arg := range overrides {
		targeted[strings.TrimPrefix(strings.SplitN(arg, ".env.", 2)[0], "mcp_servers.")] = true
	}
	codexHome := filepath.Join(tmp, "codex")
	report, err := MigrateCodexRuntimePlugins(context.Background(), map[string]any{"mcp_servers": map[string]any{}}, CodexMigrateOptions{CodexHome: codexHome, SkipPluginDiscovery: true})
	if err != nil || len(report.Errors) != 0 {
		t.Fatalf("report %+v err %v", report, err)
	}
	parsed, err := pytoml.Loads(codexMigrationRead(t, filepath.Join(codexHome, "config.toml")))
	if err != nil {
		t.Fatal(err)
	}
	migrated := parsed["mcp_servers"].(map[string]any)
	var first string
	for name := range targeted {
		if _, ok := migrated[name]; !ok {
			t.Fatalf("overrides target %s, which codex has no transport for", name)
		}
		first = name
	}
	if !slices.ContainsFunc(overrides, func(a string) bool { return strings.HasPrefix(a, "mcp_servers."+first+".env.ICEBATH_KANBAN_TASK=") }) {
		t.Fatalf("overrides %v", overrides)
	}
}
