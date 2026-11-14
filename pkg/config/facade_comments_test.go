package config

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const commentedConfig = `# TOP COMMENT — rationale for this whole file must survive
_config_version: 12
model:
  provider: test   # pinned for eval reproducibility
  default: some-model

plugins:
  # rationale for the enabled list
  enabled:
    - alpha  # kept for the demo account
approvals:
  mode: "off"   # quoted on purpose: PyYAML reads bare off as False
hooks:
  pre_tool_call:
    # why this hook exists
    - command: /bin/echo
      timeout: 10
`

var preservedComments = []string{
	"# TOP COMMENT — rationale for this whole file must survive",
	"# pinned for eval reproducibility",
	"# rationale for the enabled list",
	"# kept for the demo account",
	"# quoted on purpose: PyYAML reads bare off as False",
	"# why this hook exists",
}

// Home seeded with the hand commented config and an empty .env
func commentedHome(t *testing.T) (string, string) {
	t.Helper()
	h, path := facadeHome(t, commentedConfig)
	os.WriteFile(filepath.Join(h, ".env"), nil, 0o600)
	captureFacade(t)
	return h, path
}

// Asserts every comment, the top level key order and the quoted off survived, returning the parsed file
func assertPreserved(t *testing.T, path string) map[string]any {
	t.Helper()
	data, _ := os.ReadFile(path)
	text := string(data)
	for _, c := range preservedComments {
		if !strings.Contains(text, c) {
			t.Fatalf("comment %q destroyed by the write\n---\n%s", c, text)
		}
	}
	doc, err := ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	var top []string
	for _, k := range MappingKeys(doc) {
		switch k {
		case "_config_version", "model", "plugins", "approvals", "hooks":
			top = append(top, k)
		}
	}
	if !reflect.DeepEqual(top, []string{"_config_version", "model", "plugins", "approvals", "hooks"}) {
		t.Fatalf("key order changed %v\n---\n%s", top, text)
	}
	if v, _ := CfgGet(doc, "approvals", "mode"); v != "off" {
		t.Fatalf("approvals.mode %v", v)
	}
	if strings.Contains(text, "── Security ──") {
		t.Fatalf("stock boilerplate appended to an existing file\n%s", text)
	}
	return doc
}

func TestEveryWriterPreservesCommentsConfigSet(t *testing.T) {
	h, path := commentedHome(t)
	if err := SetConfigValue(context.Background(), h, "streaming.enabled", "true", false, pathCommandSources()); err != nil {
		t.Fatal(err)
	}
	if v, _ := CfgGet(assertPreserved(t, path), "streaming", "enabled"); v != true {
		t.Fatalf("streaming.enabled %v", v)
	}
}

func TestEveryWriterPreservesCommentsConfigUnset(t *testing.T) {
	h, path := commentedHome(t)
	if err := UnsetConfigValue(context.Background(), h, "model.default", pathCommandSources()); err != nil {
		t.Fatal(err)
	}
	if _, ok := CfgGet(assertPreserved(t, path), "model", "default"); ok {
		t.Fatal("model.default survived unset")
	}
}

func TestEveryWriterPreservesCommentsSaveConfigPluginEnableAndMemoryProvider(t *testing.T) {
	h, path := commentedHome(t)
	cfg, err := Load(h, facadeLookup)
	if err != nil {
		t.Fatal(err)
	}
	plugins := AsMap(cfg["plugins"])
	plugins["enabled"] = append(plugins["enabled"].([]any), "beta")
	mem, ok := cfg["memory"].(map[string]any)
	if !ok {
		mem = map[string]any{}
		setOrdered(cfg, "memory", mem)
	}
	mem["provider"] = "honcho"
	if err := SaveConfig(context.Background(), h, map[string]any(cfg), SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	doc := assertPreserved(t, path)
	if v, _ := CfgGet(doc, "plugins", "enabled"); !reflect.DeepEqual(v, []any{"alpha", "beta"}) {
		t.Fatalf("plugins.enabled %v", v)
	}
	if v, _ := CfgGet(doc, "memory", "provider"); v != "honcho" {
		t.Fatalf("memory.provider %v", v)
	}
}

func TestEveryWriterPreservesCommentsMigrationVersionBump(t *testing.T) {
	h, path := commentedHome(t)
	registerMigrationFacade(t)
	_, latest, err := CheckConfigVersion(context.Background(), h, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateConfig(context.Background(), h, false, true); err != nil {
		t.Fatal(err)
	}
	if v := assertPreserved(t, path)["_config_version"]; !pyEq(v, latest) {
		t.Fatalf("stamp %v of %d", v, latest)
	}
}

func TestEveryWriterPreservesCommentsAtomicConfigWriteDirect(t *testing.T) {
	h, path := commentedHome(t)
	raw, err := ReadUserConfigRaw(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	AsMap(raw["model"])["provider"] = "auto"
	if err := AtomicConfigWrite(context.Background(), h, path, raw, ""); err != nil {
		t.Fatal(err)
	}
	if v, _ := CfgGet(assertPreserved(t, path), "model", "provider"); v != "auto" {
		t.Fatalf("model.provider %v", v)
	}
}

func TestBoilerplateOnlyOnCreate(t *testing.T) {
	h, path := commentedHome(t)
	os.Remove(path)
	ctx := context.Background()
	if err := SaveConfig(ctx, h, map[string]any{"model": map[string]any{"provider": "test"}}, SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "── Security ──") {
		t.Fatalf("fresh file lacks the commented examples\n%s", data)
	}
	if err := SaveConfig(ctx, h, map[string]any{"model": map[string]any{"provider": "test", "default": "m"}}, SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); strings.Count(string(data), "── Security ──") != 1 {
		t.Fatalf("commented examples repeated\n%s", data)
	}
}
