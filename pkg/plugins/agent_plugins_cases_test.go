package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/tools"
)

func apWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func apManifest(overrides map[string]any) map[string]any {
	out := map[string]any{"$schema": PluginSchemaV1, "name": "portable.test"}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// Writes skills/<dir>/SKILL.md with name and description first, then the fields as JSON flow values
func apWriteSkill(t *testing.T, root, dir string, fields ...[2]any) string {
	t.Helper()
	skillDir := filepath.Join(root, "skills", dir)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := [][2]any{{"name", dir}, {"description", "Summarizes reports."}}
	for _, f := range fields {
		replaced := false
		for i := range meta {
			if meta[i][0] == f[0] {
				meta[i][1], replaced = f[1], true
			}
		}
		if !replaced {
			meta = append(meta, f)
		}
	}
	var b strings.Builder
	b.WriteString("---\n")
	for _, kv := range meta {
		raw, _ := json.Marshal(kv[1])
		b.WriteString(kv[0].(string) + ": " + string(raw) + "\n")
	}
	b.WriteString("---\nInstructions.\n")
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return skillDir
}

func apResolve(t *testing.T, p string) string {
	t.Helper()
	r, err := pypath.Resolve(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func apMustLoad(t *testing.T, root, data string) *AgentPluginPackage {
	t.Helper()
	pkg, err := LoadAgentPlugin(root, data)
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}

func apAgentError(t *testing.T, err error, pattern string) {
	t.Helper()
	var agentErr *AgentPluginError
	if !errors.As(err, &agentErr) || !regexp.MustCompile(pattern).MatchString(err.Error()) {
		t.Fatalf("error %v, want AgentPluginError matching %q", err, pattern)
	}
}

func apScopes(pkg *AgentPluginPackage) []string {
	var out []string
	for _, d := range pkg.Diagnostics {
		out = append(out, d.Scope)
	}
	return out
}

func apServerNames(pkg *AgentPluginPackage) []string {
	return sortedKeys(pkg.MCPServers)
}

func apVendor(servers map[string]any) map[string]any {
	return map[string]any{"extensions": map[string]any{portableExtension: map[string]any{"servers": servers}}}
}

func apRemotePackage(t *testing.T, root string, trust any) {
	t.Helper()
	servers := map[string]any{}
	if trust != nil {
		servers["trade"] = map[string]any{"trust": trust}
	}
	apWriteJSON(t, filepath.Join(root, "plugin.json"), apManifest(apVendor(servers)))
	apWriteJSON(t, filepath.Join(root, "mcp.json"), map[string]any{"$schema": MCPSchemaV1, "mcpServers": map[string]any{
		"trade": map[string]any{"type": "streamable-http", "url": "https://mcp.example.com/mcp"}}})
}

func TestLoadsManifestSkillAndStdioServer(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "plugin")
	apWriteJSON(t, filepath.Join(root, "plugin.json"), apManifest(map[string]any{"version": "1.2.3"}))
	skillDir := apWriteSkill(t, root, "summarize")
	apWriteJSON(t, filepath.Join(root, "mcp.json"), map[string]any{"$schema": MCPSchemaV1, "mcpServers": map[string]any{
		"worker": map[string]any{"type": "stdio", "command": "python", "args": []any{"${PLUGIN_ROOT}/server.py", "${UNKNOWN}"},
			"env": map[string]any{"CACHE": "${PLUGIN_DATA}/cache"}}}})
	pkg := apMustLoad(t, root, filepath.Join(tmp, "data"))
	if pkg.Name != "portable.test" || pkg.Version != "1.2.3" || pkg.Skills[0].Root != apResolve(t, skillDir) {
		t.Fatalf("package %s %s %+v", pkg.Name, pkg.Version, pkg.Skills)
	}
	server := pkg.MCPServers["worker"]
	rootR, dataR := apResolve(t, root), apResolve(t, filepath.Join(tmp, "data"))
	env := server["env"].(map[string]any)
	if server["command"] != "python" || !reflect.DeepEqual(server["args"], []any{rootR + "/server.py", "${UNKNOWN}"}) ||
		server["cwd"] != rootR || env["PLUGIN_ROOT"] != rootR || env["PLUGIN_DATA"] != dataR || env["CACHE"] != dataR+"/cache" {
		t.Fatalf("server %v", server)
	}
	if st, err := os.Stat(filepath.Join(tmp, "data")); err != nil || !st.IsDir() {
		t.Fatalf("data root not created: %v", err)
	}
}

func TestRejectsInvalidManifests(t *testing.T) {
	for i, manifest := range []any{
		[]any{},
		map[string]any{"name": "valid-name"},
		map[string]any{"$schema": "https://example.test/schema.json", "name": "valid-name"},
		map[string]any{"$schema": PluginSchemaV1},
		map[string]any{"$schema": PluginSchemaV1, "name": "Bad_Name"},
		map[string]any{"$schema": PluginSchemaV1, "name": "a--b"},
		map[string]any{"$schema": PluginSchemaV1, "name": "a", "keywords": []any{1}},
		map[string]any{"$schema": PluginSchemaV1, "name": "a", "author": map[string]any{"handle": "x"}},
	} {
		tmp := t.TempDir()
		apWriteJSON(t, filepath.Join(tmp, "plugin.json"), manifest)
		if _, err := LoadAgentPlugin(tmp, filepath.Join(tmp, "data")); !errors.As(err, new(*AgentPluginError)) {
			t.Errorf("manifest %d: %v, want AgentPluginError", i, err)
		}
	}
}

func TestServerDeclarationJoinsMcpAndPreservesLiveness(t *testing.T) {
	tmp := t.TempDir()
	app := filepath.Join(tmp, "example-app")
	if err := os.WriteFile(app, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	apWriteJSON(t, filepath.Join(tmp, "plugin.json"), apManifest(apVendor(map[string]any{"worker": map[string]any{
		"app":      map[string]any{"darwin": map[string]any{"presence": "executable", "location": app}},
		"requires": map[string]any{"app": true}, "liveness": map[string]any{"kind": "static"}}})))
	apWriteJSON(t, filepath.Join(tmp, "mcp.json"), map[string]any{"$schema": MCPSchemaV1, "mcpServers": map[string]any{
		"worker": map[string]any{"type": "stdio", "command": "python"}}})
	pkg := apMustLoad(t, tmp, filepath.Join(tmp, "data"))
	server := pkg.ServerDeclarations["worker"]
	decl, err := tools.ParsePlatformDeclaration("worker", server.App, server.Requires, "plugin.json extension server 'worker'")
	if err != nil || !decl.RequiresApp() {
		t.Fatalf("declaration %+v %v", decl, err)
	}
	if !reflect.DeepEqual(server.Liveness, map[string]any{"kind": "static"}) {
		t.Fatalf("liveness %v", server.Liveness)
	}
}

func TestOrphanServerDeclarationDisablesPackage(t *testing.T) {
	tmp := t.TempDir()
	apWriteJSON(t, filepath.Join(tmp, "plugin.json"), apManifest(apVendor(map[string]any{"orphan": map[string]any{"requires": map[string]any{"app": false}}})))
	_, err := LoadAgentPlugin(tmp, filepath.Join(tmp, "data"))
	apAgentError(t, err, "orphan.*no matching mcp.json server")
}

func TestLivenessWithoutDeclarationDisablesPackage(t *testing.T) {
	tmp := t.TempDir()
	apWriteJSON(t, filepath.Join(tmp, "plugin.json"), apManifest(apVendor(map[string]any{"worker": map[string]any{"liveness": map[string]any{"kind": "static"}}})))
	apWriteJSON(t, filepath.Join(tmp, "mcp.json"), map[string]any{"$schema": MCPSchemaV1, "mcpServers": map[string]any{
		"worker": map[string]any{"type": "stdio", "command": "python"}}})
	_, err := LoadAgentPlugin(tmp, filepath.Join(tmp, "data"))
	apAgentError(t, err, "liveness without app or requires")
}

func TestPackageCanAskForItsServerToBeGatedButNeverWidened(t *testing.T) {
	for _, c := range []struct{ declared, expected any }{{"untrusted", "untrusted"}, {"full", nil}, {nil, nil}} {
		tmp := t.TempDir()
		apRemotePackage(t, tmp, c.declared)
		pkg := apMustLoad(t, tmp, filepath.Join(tmp, "data"))
		if got := pkg.MCPServers["trade"]["trust"]; got != c.expected {
			t.Errorf("declared %v: trust %v, want %v", c.declared, got, c.expected)
		}
		if _, ok := pkg.ServerDeclarations["trade"]; ok {
			t.Errorf("declared %v: trust alone declared an application", c.declared)
		}
	}
}

func TestUnknownTrustValueDisablesPackage(t *testing.T) {
	tmp := t.TempDir()
	apRemotePackage(t, tmp, "trusted")
	_, err := LoadAgentPlugin(tmp, filepath.Join(tmp, "data"))
	apAgentError(t, err, "trust must be 'untrusted' or 'full'")
}

func TestServerTimeoutIsOptInAndScopedToItsServer(t *testing.T) {
	for _, transport := range []string{"stdio", "streamable-http"} {
		tmp := t.TempDir()
		server := map[string]any{"type": "stdio", "command": "python"}
		if transport != "stdio" {
			server = map[string]any{"type": "streamable-http", "url": "https://mcp.example.com/mcp"}
		}
		apWriteJSON(t, filepath.Join(tmp, "mcp.json"), map[string]any{"$schema": MCPSchemaV1, "mcpServers": map[string]any{"slow": server, "ordinary": server}})
		for _, settings := range []map[string]any{{}, {"timeout": 1000, "trust": "untrusted"}, {}} {
			overrides := map[string]any{}
			if len(settings) > 0 {
				overrides = apVendor(map[string]any{"slow": settings})
			}
			apWriteJSON(t, filepath.Join(tmp, "plugin.json"), apManifest(overrides))
			pkg := apMustLoad(t, tmp, filepath.Join(tmp, "data"))
			if _, ok := pkg.MCPServers["ordinary"]["timeout"]; ok {
				t.Fatalf("%s: ordinary server got a timeout", transport)
			}
			if _, ok := pkg.ServerDeclarations["slow"]; ok {
				t.Fatalf("%s: settings declared an application", transport)
			}
			slow := pkg.MCPServers["slow"]
			if len(settings) > 0 {
				if slow["timeout"] != float64(1000) || slow["trust"] != "untrusted" {
					t.Fatalf("%s: slow server %v", transport, slow)
				}
			} else if _, ok := slow["timeout"]; ok {
				t.Fatalf("%s: undeclared timeout %v", transport, slow)
			}
		}
	}
}

func TestServerTimeoutRejectsInvalidDeadlines(t *testing.T) {
	for _, timeout := range []string{"true", "null", `"1000"`, "0", "-1", "Infinity", "NaN"} {
		tmp := t.TempDir()
		apRemotePackage(t, tmp, nil)
		doc := `{"$schema": "` + PluginSchemaV1 + `", "name": "portable.test", "extensions": {"` + portableExtension +
			`": {"servers": {"trade": {"timeout": ` + timeout + `}}}}}`
		if err := os.WriteFile(filepath.Join(tmp, "plugin.json"), []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadAgentPlugin(tmp, filepath.Join(tmp, "data"))
		apAgentError(t, err, "timeout must be")
	}
}

func TestUnknownFieldsAndNonObjectExtensionsAreNonfatal(t *testing.T) {
	tmp := t.TempDir()
	apWriteJSON(t, filepath.Join(tmp, "plugin.json"), apManifest(map[string]any{"unknown": true, "extensions": "ignored"}))
	if pkg := apMustLoad(t, tmp, filepath.Join(tmp, "data")); len(pkg.Diagnostics) != 2 {
		t.Fatalf("diagnostics %v", pkg.Diagnostics)
	}
}

func TestUnknownServerDeclarationKeyIsIgnoredAndKnownKeysStillApply(t *testing.T) {
	for _, declared := range []map[string]any{{"trust": "untrusted", "future_key": 5}, {"future_key": 5}} {
		tmp := t.TempDir()
		apRemotePackage(t, tmp, nil)
		apWriteJSON(t, filepath.Join(tmp, "plugin.json"), apManifest(apVendor(map[string]any{"trade": declared})))
		pkg := apMustLoad(t, tmp, filepath.Join(tmp, "data"))
		if got := pkg.MCPServers["trade"]["trust"]; got != declared["trust"] {
			t.Errorf("trust %v, want %v", got, declared["trust"])
		}
		if _, ok := pkg.ServerDeclarations["trade"]; ok {
			t.Error("unknown key declared an application")
		}
		if !reflect.DeepEqual(apScopes(pkg), []string{"server:trade"}) || !strings.Contains(pkg.Diagnostics[0].Message, "future_key") {
			t.Errorf("diagnostics %v", pkg.Diagnostics)
		}
	}
}

func TestInvalidSkillDoesNotHideValidSibling(t *testing.T) {
	tmp := t.TempDir()
	apWriteJSON(t, filepath.Join(tmp, "plugin.json"), apManifest(nil))
	apWriteSkill(t, tmp, "valid-skill", [2]any{"license", "MIT"}, [2]any{"compatibility", "Linux"})
	apWriteSkill(t, tmp, "wrong-dir", [2]any{"name", "different"})
	pkg := apMustLoad(t, tmp, filepath.Join(tmp, "data"))
	if len(pkg.Skills) != 1 || pkg.Skills[0].Name != "valid-skill" || !slices.Contains(apScopes(pkg), "skill:wrong-dir") {
		t.Fatalf("skills %+v diagnostics %v", pkg.Skills, pkg.Diagnostics)
	}
}

func TestRejectsInvalidOptionalSkillFields(t *testing.T) {
	for _, field := range [][2]any{{"license", []any{"MIT"}}, {"compatibility", ""}, {"compatibility", 1}, {"metadata", []any{}},
		{"allowed-tools", []any{"terminal"}}} {
		tmp := t.TempDir()
		apWriteJSON(t, filepath.Join(tmp, "plugin.json"), apManifest(nil))
		apWriteSkill(t, tmp, "bad-skill", field)
		if pkg := apMustLoad(t, tmp, filepath.Join(tmp, "data")); len(pkg.Skills) != 0 {
			t.Errorf("%v: skills %+v", field, pkg.Skills)
		}
	}
}

func TestSymlinkEscapeIsIsolatedToComponent(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "plugin")
	apWriteJSON(t, filepath.Join(root, "plugin.json"), apManifest(nil))
	apWriteSkill(t, root, "summarize")
	outside := filepath.Join(tmp, "outside.json")
	apWriteJSON(t, outside, map[string]any{"$schema": MCPSchemaV1, "mcpServers": map[string]any{}})
	if err := os.Symlink(outside, filepath.Join(root, "mcp.json")); err != nil {
		t.Fatal(err)
	}
	pkg := apMustLoad(t, root, filepath.Join(tmp, "data"))
	if len(pkg.Skills) != 1 || len(pkg.MCPServers) != 0 || !slices.Contains(apScopes(pkg), "mcp") {
		t.Fatalf("skills %d servers %v diagnostics %v", len(pkg.Skills), pkg.MCPServers, pkg.Diagnostics)
	}
}

func TestStdioCommandAndDataCwdContainment(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "plugin")
	apWriteJSON(t, filepath.Join(root, "plugin.json"), apManifest(nil))
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "server"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	apWriteJSON(t, filepath.Join(root, "mcp.json"), map[string]any{"$schema": MCPSchemaV1, "mcpServers": map[string]any{
		"valid":          map[string]any{"type": "stdio", "command": "./bin/server", "cwd": "${PLUGIN_DATA}/state"},
		"opaque-command": map[string]any{"type": "stdio", "command": "./${PLUGIN_ROOT}"},
		"escape":         map[string]any{"type": "stdio", "command": "./../outside"},
		"mixed-root":     map[string]any{"type": "stdio", "command": "python", "cwd": "./${PLUGIN_DATA}/state"},
	}})
	pkg := apMustLoad(t, root, filepath.Join(tmp, "data"))
	if got := apServerNames(pkg); !reflect.DeepEqual(got, []string{"opaque-command", "valid"}) {
		t.Fatalf("servers %v diagnostics %v", got, pkg.Diagnostics)
	}
	if pkg.MCPServers["valid"]["cwd"] != apResolve(t, filepath.Join(tmp, "data", "state")) {
		t.Fatalf("valid cwd %v", pkg.MCPServers["valid"]["cwd"])
	}
	if st, err := os.Stat(filepath.Join(tmp, "data", "state")); err != nil || !st.IsDir() {
		t.Fatalf("data cwd not created: %v", err)
	}
	if _, ok := pkg.MCPServers["valid"]["agent_plugin"]; ok {
		t.Fatal("translated server carries agent_plugin")
	}
	if pkg.MCPServers["opaque-command"]["command"] != apResolve(t, filepath.Join(root, "${PLUGIN_ROOT}")) {
		t.Fatalf("opaque command %v", pkg.MCPServers["opaque-command"]["command"])
	}
}

func TestStdioCwdDirectoryFailureIsolatedToServer(t *testing.T) {
	tmp := t.TempDir()
	apWriteJSON(t, filepath.Join(tmp, "plugin.json"), apManifest(nil))
	apWriteJSON(t, filepath.Join(tmp, "mcp.json"), map[string]any{"$schema": MCPSchemaV1, "mcpServers": map[string]any{
		"broken": map[string]any{"type": "stdio", "command": "python", "cwd": "${PLUGIN_DATA}/broken"},
		"valid":  map[string]any{"type": "stdio", "command": "python", "cwd": "${PLUGIN_DATA}/valid"},
	}})
	dataRoot := apResolve(t, filepath.Join(tmp, "data"))
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "broken"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg := apMustLoad(t, tmp, dataRoot)
	if got := apServerNames(pkg); !reflect.DeepEqual(got, []string{"valid"}) {
		t.Fatalf("servers %v", got)
	}
	if st, err := os.Stat(filepath.Join(dataRoot, "valid")); err != nil || !st.IsDir() || !slices.Contains(apScopes(pkg), "mcp:broken") {
		t.Fatalf("valid cwd %v diagnostics %v", err, pkg.Diagnostics)
	}
}

func TestMalformedSkillYamlIsSkipped(t *testing.T) {
	tmp := t.TempDir()
	apWriteJSON(t, filepath.Join(tmp, "plugin.json"), apManifest(nil))
	skill := filepath.Join(tmp, "skills", "broken")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: broken\ndescription: [unterminated\n---\nBody.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg := apMustLoad(t, tmp, filepath.Join(tmp, "data"))
	if len(pkg.Skills) != 0 || !slices.Contains(apScopes(pkg), "skill:broken") {
		t.Fatalf("skills %+v diagnostics %v", pkg.Skills, pkg.Diagnostics)
	}
}

func TestDataDirectoryFailurePreservesValidSkills(t *testing.T) {
	tmp := t.TempDir()
	apWriteJSON(t, filepath.Join(tmp, "plugin.json"), apManifest(nil))
	apWriteSkill(t, tmp, "summarize")
	apWriteJSON(t, filepath.Join(tmp, "mcp.json"), map[string]any{"$schema": MCPSchemaV1, "mcpServers": map[string]any{
		"worker": map[string]any{"type": "stdio", "command": "python"}}})
	dataRoot := filepath.Join(tmp, "data")
	if err := os.WriteFile(dataRoot, []byte("read-only profile"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg := apMustLoad(t, tmp, dataRoot)
	if len(pkg.Skills) != 1 || len(pkg.MCPServers) != 0 || !slices.Contains(apScopes(pkg), "mcp:worker") {
		t.Fatalf("skills %d servers %v diagnostics %v", len(pkg.Skills), pkg.MCPServers, pkg.Diagnostics)
	}
}

func TestInvalidEntriesAndUnsupportedRemotePreserveValidStdio(t *testing.T) {
	tmp := t.TempDir()
	apWriteJSON(t, filepath.Join(tmp, "plugin.json"), apManifest(nil))
	apWriteJSON(t, filepath.Join(tmp, "mcp.json"), map[string]any{"$schema": MCPSchemaV1, "mcpServers": map[string]any{
		"valid":         map[string]any{"type": "stdio", "command": "python"},
		"invalid":       map[string]any{"type": "stdio", "command": "python", "extra": true},
		"multi-token":   map[string]any{"type": "stdio", "command": "python -m worker"},
		"reserved-root": map[string]any{"type": "stdio", "command": "python", "env": map[string]any{"PLUGIN_ROOT": "override"}},
		"reserved-data": map[string]any{"type": "stdio", "command": "python", "env": map[string]any{"PLUGIN_DATA": "override"}},
		"remote": map[string]any{"type": "streamable-http", "url": "https://example.test/mcp",
			"headers": map[string]any{"X-Tenant": "a", "x-tenant": "b"}},
	}})
	pkg := apMustLoad(t, tmp, filepath.Join(tmp, "data"))
	if got := apServerNames(pkg); !reflect.DeepEqual(got, []string{"valid"}) {
		t.Fatalf("servers %v", got)
	}
	for _, scope := range []string{"mcp:invalid", "mcp:multi-token", "mcp:reserved-root", "mcp:reserved-data", "mcp:remote"} {
		if !slices.Contains(apScopes(pkg), scope) {
			t.Errorf("missing diagnostic %s in %v", scope, pkg.Diagnostics)
		}
	}
}

func TestInvalidMcpTopLevelPreservesSkills(t *testing.T) {
	tmp := t.TempDir()
	apWriteJSON(t, filepath.Join(tmp, "plugin.json"), apManifest(nil))
	apWriteSkill(t, tmp, "summarize")
	apWriteJSON(t, filepath.Join(tmp, "mcp.json"), map[string]any{"$schema": "https://example.test/mcp.json", "mcpServers": map[string]any{}})
	if pkg := apMustLoad(t, tmp, filepath.Join(tmp, "data")); len(pkg.Skills) != 1 || len(pkg.MCPServers) != 0 {
		t.Fatalf("skills %d servers %v", len(pkg.Skills), pkg.MCPServers)
	}
}

func TestStreamableHttpTranslatesToNativeRemoteConfig(t *testing.T) {
	tmp := t.TempDir()
	apWriteJSON(t, filepath.Join(tmp, "plugin.json"), apManifest(nil))
	apWriteJSON(t, filepath.Join(tmp, "mcp.json"), map[string]any{"$schema": MCPSchemaV1, "mcpServers": map[string]any{
		"deploy": map[string]any{"type": "streamable-http", "url": "https://deploy.example.test/mcp", "headers": map[string]any{"X-Tenant": "public-tenant"}},
		"local":  map[string]any{"type": "streamable-http", "url": "http://127.0.0.1:8000/mcp"},
		"bare":   map[string]any{"type": "streamable-http", "url": "https://bare.example.test/mcp"},
	}})
	pkg := apMustLoad(t, tmp, filepath.Join(tmp, "data"))
	if got := apServerNames(pkg); !reflect.DeepEqual(got, []string{"bare", "deploy", "local"}) {
		t.Fatalf("servers %v", got)
	}
	want := map[string]any{"url": "https://deploy.example.test/mcp", "headers": map[string]any{"X-Tenant": "public-tenant"}, "strict_redirect_headers": true}
	if !reflect.DeepEqual(pkg.MCPServers["deploy"], want) {
		t.Fatalf("deploy %v", pkg.MCPServers["deploy"])
	}
	if !reflect.DeepEqual(pkg.MCPServers["bare"], map[string]any{"url": "https://bare.example.test/mcp", "strict_redirect_headers": true}) {
		t.Fatalf("bare %v", pkg.MCPServers["bare"])
	}
}

func TestStreamableHttpRejectsInvalidUrls(t *testing.T) {
	for _, u := range []string{"", "ftp://example.test/mcp", "https://user:pass@example.test/mcp", "https://example.test/mcp#fragment",
		"http://example.test/mcp", "http://10.0.0.5/mcp", "https:///mcp"} {
		tmp := t.TempDir()
		apWriteJSON(t, filepath.Join(tmp, "plugin.json"), apManifest(nil))
		apWriteJSON(t, filepath.Join(tmp, "mcp.json"), map[string]any{"$schema": MCPSchemaV1, "mcpServers": map[string]any{
			"remote": map[string]any{"type": "streamable-http", "url": u}, "valid": map[string]any{"type": "stdio", "command": "python"}}})
		pkg := apMustLoad(t, tmp, filepath.Join(tmp, "data"))
		if got := apServerNames(pkg); !reflect.DeepEqual(got, []string{"valid"}) || !slices.Contains(apScopes(pkg), "mcp:remote") {
			t.Errorf("%q: servers %v diagnostics %v", u, got, pkg.Diagnostics)
		}
	}
}

func TestStreamableHttpAcceptsLoopbackHttpAndHttps(t *testing.T) {
	for _, u := range []string{"http://localhost:8000/mcp", "http://127.0.0.1/mcp", "http://[::1]:9000/mcp", "https://remote.example.test/mcp"} {
		tmp := t.TempDir()
		apWriteJSON(t, filepath.Join(tmp, "plugin.json"), apManifest(nil))
		apWriteJSON(t, filepath.Join(tmp, "mcp.json"), map[string]any{"$schema": MCPSchemaV1, "mcpServers": map[string]any{
			"remote": map[string]any{"type": "streamable-http", "url": u}}})
		pkg := apMustLoad(t, tmp, filepath.Join(tmp, "data"))
		if got := apServerNames(pkg); !reflect.DeepEqual(got, []string{"remote"}) || pkg.MCPServers["remote"]["url"] != u {
			t.Errorf("%q: servers %v diagnostics %v", u, pkg.MCPServers, pkg.Diagnostics)
		}
	}
}

func TestSseRemainsUnsupported(t *testing.T) {
	tmp := t.TempDir()
	apWriteJSON(t, filepath.Join(tmp, "plugin.json"), apManifest(nil))
	apWriteJSON(t, filepath.Join(tmp, "mcp.json"), map[string]any{"$schema": MCPSchemaV1, "mcpServers": map[string]any{
		"legacy": map[string]any{"type": "sse", "url": "https://legacy.example.test/sse"}}})
	pkg := apMustLoad(t, tmp, filepath.Join(tmp, "data"))
	found := false
	for _, d := range pkg.Diagnostics {
		found = found || (d.Scope == "mcp:legacy" && strings.Contains(d.Message, "not supported"))
	}
	if len(pkg.MCPServers) != 0 || !found {
		t.Fatalf("servers %v diagnostics %v", pkg.MCPServers, pkg.Diagnostics)
	}
}

// Home holding one enabled portable package with the given mcp.json servers and an empty bundled tree
func apProbeHome(t *testing.T, servers map[string]any, nativeYAML bool) context.Context {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	plugin := filepath.Join(h, "plugins", "portable")
	apWriteJSON(t, filepath.Join(plugin, "plugin.json"), apManifest(nil))
	if nativeYAML {
		if err := os.WriteFile(filepath.Join(plugin, "plugin.yaml"), []byte("name: native\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	apWriteJSON(t, filepath.Join(plugin, "mcp.json"), map[string]any{"$schema": MCPSchemaV1, "mcpServers": servers})
	bundled := filepath.Join(tmp, "bundled")
	if err := os.MkdirAll(bundled, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", bundled)
	return home.WithOverride(context.Background(), h)
}

func apEnabled(names ...string) map[string]any {
	return map[string]any{"plugins": map[string]any{"enabled": stringsAny(names)}}
}

func TestEnabledPortableMcpProbeDoesNotLoadPlugins(t *testing.T) {
	ctx := apProbeHome(t, map[string]any{"worker": map[string]any{"type": "stdio", "command": "python"}}, false)
	if !HasEnabledAgentPluginMCP(ctx, apEnabled("portable.test")) {
		t.Fatal("enabled portable package with a stdio server not probed")
	}
	disabled := map[string]any{"plugins": map[string]any{"enabled": []any{"portable.test"}, "disabled": []any{"portable.test"}}}
	if HasEnabledAgentPluginMCP(ctx, disabled) {
		t.Fatal("disabled portable package counted")
	}
}

func TestPortableMcpProbeCountsStreamableHttpConfig(t *testing.T) {
	ctx := apProbeHome(t, map[string]any{"remote": map[string]any{"type": "streamable-http", "url": "https://example.test/mcp"}}, false)
	if !HasEnabledAgentPluginMCP(ctx, apEnabled("portable.test")) {
		t.Fatal("streamable-http server not counted")
	}
}

func TestPortableMcpProbeIgnoresUnsupportedOnlyConfig(t *testing.T) {
	ctx := apProbeHome(t, map[string]any{"remote": map[string]any{"type": "sse", "url": "https://example.test/sse"}}, false)
	if HasEnabledAgentPluginMCP(ctx, apEnabled("portable.test")) {
		t.Fatal("an sse only package counted")
	}
}

func TestPortableMcpProbeHonorsNativePrecedence(t *testing.T) {
	ctx := apProbeHome(t, map[string]any{}, true)
	if HasEnabledAgentPluginMCP(ctx, apEnabled("portable.test")) {
		t.Fatal("a native plugin.yaml package counted as portable")
	}
}
