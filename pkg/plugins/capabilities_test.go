package plugins

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Home with an empty config.yaml, the hermes_home fixture of the capability tests
func capHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	config.ResetCache()
	writeFile(t, filepath.Join(h, "config.yaml"), "{}\n")
	return h
}

func capCfg(t *testing.T, h string) map[string]any {
	t.Helper()
	config.ResetCache()
	cfg, err := loadConfig(h)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func capEntry(t *testing.T, h, plugin string) map[string]any {
	t.Helper()
	return rmConfig(t, h)["plugins"].(map[string]any)["entries"].(map[string]any)[plugin].(map[string]any)
}

func capRecord(t *testing.T, h, plugin string, granted, declared []string) {
	t.Helper()
	if err := RecordConsent(context.Background(), h, plugin, granted, declared); err != nil {
		t.Fatal(err)
	}
}

func capSet(ids ...string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func TestEveryCapabilityHasLegacyGate(t *testing.T) {
	for _, spec := range CapabilityRegistry {
		if (len(spec.LegacyPath) > 0) != (spec.ID != "llm.side_question") || spec.Description == "" {
			t.Fatalf("%+v", spec)
		}
	}
}

func TestDeclarationParsing(t *testing.T) {
	cases := []struct {
		raw  any
		want []string
	}{
		{[]any{"tools.override", "root.everything"}, []string{"tools.override"}},
		{"tools.override", []string{}},
		{map[string]any{"a": 1}, []string{}},
		{nil, []string{}},
		{[]any{1, nil, "tools.override"}, []string{"tools.override"}},
		{[]any{"llm.model_override", "tools.override", "llm.model_override"}, []string{"llm.model_override", "tools.override"}},
	}
	for _, tc := range cases {
		if got := ParseDeclaredCapabilities(tc.raw, "capplug"); !slices.Equal(got, tc.want) {
			t.Fatalf("%v: %v", tc.raw, got)
		}
	}
	dir := filepath.Join(t.TempDir(), "capplug")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "plugin.yaml"), "name: capplug\nversion: '1.0'\ncapabilities:\n  - tools.override\n  - bogus.capability\n")
	if mf := ParseManifestFile(filepath.Join(dir, "plugin.yaml"), dir, "user", ""); mf == nil || !slices.Equal(mf.Capabilities, []string{"tools.override"}) {
		t.Fatalf("%+v", mf)
	}
	plain := filepath.Join(t.TempDir(), "plainplug")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(plain, "plugin.yaml"), "name: plainplug\n")
	if mf := ParseManifestFile(filepath.Join(plain, "plugin.yaml"), plain, "user", ""); mf == nil || len(mf.Capabilities) != 0 {
		t.Fatalf("%+v", mf)
	}
}

func TestConsentPersistence(t *testing.T) {
	h := capHome(t)
	capRecord(t, h, "capplug", []string{"tools.override"}, []string{"tools.override"})
	entry := capEntry(t, h, "capplug")
	consent, _ := entry["capabilities_consent"].(map[string]any)
	if !reflect.DeepEqual(entry["granted_capabilities"], []any{"tools.override"}) || consent["hash"] != CapabilitySetHash([]string{"tools.override"}) ||
		!config.PyTruthy(consent["granted_at"]) || entry["allow_tool_override"] != true {
		t.Fatalf("%v", entry)
	}
	capRecord(t, h, "capplug2", []string{"llm.model_override"}, []string{"llm.model_override"})
	if llm, _ := capEntry(t, h, "capplug2")["llm"].(map[string]any); llm["allow_model_override"] != true {
		t.Fatalf("%v", capEntry(t, h, "capplug2"))
	}
	capRecord(t, h, "capplug", []string{"llm.model_override"}, []string{"tools.override", "llm.model_override"})
	if got := GrantedCapabilities(capCfg(t, h), "capplug"); !reflect.DeepEqual(got, capSet("tools.override", "llm.model_override")) {
		t.Fatalf("%v", got)
	}
	cfg := capCfg(t, h)
	if CapabilityGranted(cfg, "declined", "tools.override") || !slices.Equal(PendingCapabilities(cfg, "declined", []string{"tools.override"}), []string{"tools.override"}) {
		t.Fatal("a plugin without consent must stay off")
	}
}

func TestUpdateReconsent(t *testing.T) {
	h := capHome(t)
	capRecord(t, h, "capplug", []string{"tools.override"}, []string{"tools.override"})
	v2 := []string{"tools.override", "llm.model_override"}
	cfg := capCfg(t, h)
	if !slices.Equal(PendingCapabilities(cfg, "capplug", v2), []string{"llm.model_override"}) || !DeclaredSetChanged(cfg, "capplug", v2) {
		t.Fatal("added capability must be pending")
	}
	if CapabilityGranted(cfg, "capplug", "llm.model_override") || !CapabilityGranted(cfg, "capplug", "tools.override") {
		t.Fatal("grant state after update")
	}
	if len(PendingCapabilities(cfg, "capplug", []string{"tools.override"})) != 0 || DeclaredSetChanged(cfg, "capplug", []string{"tools.override"}) {
		t.Fatal("unchanged set needs no reconsent")
	}
	if CapabilitySetHash([]string{"tools.override", "llm.model_override"}) != CapabilitySetHash([]string{"llm.model_override", "tools.override"}) {
		t.Fatal("hash must ignore order")
	}
	if !DeclaredSetChanged(cfg, "nobody", []string{"tools.override"}) {
		t.Fatal("no consent record counts as changed")
	}
	capRecord(t, h, "capplug", PendingCapabilities(cfg, "capplug", v2), v2)
	cfg = capCfg(t, h)
	if !CapabilityGranted(cfg, "capplug", "llm.model_override") || DeclaredSetChanged(cfg, "capplug", v2) {
		t.Fatal("reconsent must grant the addition")
	}
}

func TestCapabilitiesFailClosed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nonexistent")
	t.Setenv("ICEBATH_HOME", missing)
	if cfg := capCfg(t, missing); CapabilityGranted(cfg, "capplug", "tools.override") || len(GrantedCapabilities(cfg, "capplug")) != 0 {
		t.Fatal("missing config must grant nothing")
	}
	h := capHome(t)
	for _, body := range []string{
		"plugins:\n  entries:\n    capplug:\n      granted_capabilities: not-a-list\n",
		"plugins:\n  entries:\n    capplug: 42\n",
		"plugins:\n  entries:\n    capplug:\n      granted_capabilities: [root.everything, 42]\n",
	} {
		writeFile(t, filepath.Join(h, "config.yaml"), body)
		cfg := capCfg(t, h)
		if CapabilityGranted(cfg, "capplug", "tools.override") || len(GrantedCapabilities(cfg, "capplug")) != 0 {
			t.Fatalf("granted under %q", body)
		}
	}
	writeFile(t, filepath.Join(h, "config.yaml"), "{}\n")
	capRecord(t, h, "capplug", []string{"tools.override"}, []string{"tools.override"})
	if CapabilityGranted(capCfg(t, h), "capplug", "root.everything") {
		t.Fatal("unknown capability granted")
	}
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  entries:\n    capplug:\n      capabilities_consent: broken\n")
	cfg := capCfg(t, h)
	if ConsentHash(cfg, "capplug") != "" || !DeclaredSetChanged(cfg, "capplug", []string{"tools.override"}) {
		t.Fatal("corrupt consent hash counts as changed")
	}
}

func capContext(name, source string) *Context {
	return newContext(&Manifest{Name: name, Source: source, Key: name}, NewManager(os.Getenv("ICEBATH_HOME")))
}

func TestLegacyGateCompat(t *testing.T) {
	h := capHome(t)
	ctx := homeCtx(h)
	for _, tc := range []struct {
		body, plugin, capability string
		want                     bool
	}{
		{"plugins:\n  entries:\n    oldplug:\n      allow_tool_override: true\n", "oldplug", "tools.override", true},
		{"plugins:\n  entries:\n    oldplug:\n      llm:\n        allow_model_override: true\n", "oldplug", "llm.model_override", true},
		{"plugins:\n  entries:\n    newplug:\n      llm:\n        allow_side_question: true\n", "newplug", "llm.side_question", false},
		{"plugins:\n  entries:\n    oldplug:\n      allow_tool_override: false\n", "oldplug", "tools.override", false},
	} {
		writeFile(t, filepath.Join(h, "config.yaml"), tc.body)
		if got := CapabilityGranted(capCfg(t, h), tc.plugin, tc.capability); got != tc.want {
			t.Fatalf("%q: %v", tc.body, got)
		}
	}
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  entries:\n    oldplug:\n      allow_tool_override: true\n")
	config.ResetCache()
	if !capContext("oldplug", "user").toolOverrideAllowed("write_file") {
		t.Fatal("legacy key must open the override gate")
	}
	writeFile(t, filepath.Join(h, "config.yaml"), "{}\n")
	config.ResetCache()
	if capContext("capplug", "user").toolOverrideAllowed("write_file") {
		t.Fatal("override gate open without a grant")
	}
	capRecord(t, h, "freshplug", []string{"llm.side_question"}, []string{"llm.side_question"})
	entry := capEntry(t, h, "freshplug")
	if _, hasLLM := entry["llm"]; hasLLM || !reflect.DeepEqual(entry["granted_capabilities"], []any{"llm.side_question"}) ||
		!CapabilityGranted(capCfg(t, h), "freshplug", "llm.side_question") {
		t.Fatalf("%v", entry)
	}
	capRecord(t, h, "capplug", []string{"tools.override"}, []string{"tools.override"})
	config.ResetCache()
	if !capContext("capplug", "user").toolOverrideAllowed("write_file") {
		t.Fatal("canonical grant must open the override gate")
	}
	bundled := capContext("bplug", "bundled")
	if !bundled.toolOverrideAllowed("write_file") || !bundled.HasCapability(ctx, "tools.override") {
		t.Fatal("bundled plugins are trusted")
	}
}

func TestHasCapabilityProbeGranted(t *testing.T) {
	h := capHome(t)
	capRecord(t, h, "capplug", []string{"llm.model_override"}, []string{"llm.model_override"})
	config.ResetCache()
	c := capContext("capplug", "user")
	ctx := homeCtx(h)
	if !c.HasCapability(ctx, "llm.model_override") || c.HasCapability(ctx, "tools.override") || c.HasCapability(ctx, "nonsense.capability") {
		t.Fatal("capability probe")
	}
}
