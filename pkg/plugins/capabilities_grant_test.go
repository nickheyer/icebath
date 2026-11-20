package plugins

import (
	"context"
	"strings"
	"testing"
)

// Granted ids are stripped before the registry filter so a padded grant still opens its gate
func TestGrantedCapabilitiesStripBeforeKnownFilter(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	entry := func(v any) map[string]any {
		return map[string]any{"plugins": map[string]any{"entries": map[string]any{"p": map[string]any{GrantedCapabilitiesKey: v}}}}
	}
	cfg := entry([]any{" gateway.platform_actions\n", int64(7), "bogus", "gateway.platform_actions"})
	if got := GrantedCapabilities(cfg, "p"); len(got) != 1 || !got[PlatformActionCapability] {
		t.Fatalf("granted %v", got)
	}
	if !CapabilityGranted(cfg, "p", PlatformActionCapability) || CapabilityGranted(cfg, "p", "tools.override") {
		t.Fatal("gate decisions")
	}
	if got := GrantedCapabilities(entry("gateway.platform_actions"), "p"); len(got) != 0 {
		t.Fatalf("non list grant %v", got)
	}
	if CapabilityGranted(cfg, "p", "no.such.capability") {
		t.Fatal("unknown capability granted")
	}
	if got := ParseDeclaredCapabilities("", "p"); len(got) != 0 {
		t.Fatalf("falsy capabilities %v", got)
	}
}

// A recorded consent stamps granted_at as isoformat with a +00:00 offset like datetime.now(UTC)
func TestRecordConsentStampsIsoformatOffset(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := RecordConsent(context.Background(), h, "p", []string{PlatformActionCapability}, []string{PlatformActionCapability}); err != nil {
		t.Fatal(err)
	}
	cfg, err := readUserConfig(h)
	if err != nil {
		t.Fatal(err)
	}
	consent := asMap(pluginEntry(cfg, "p")[CapabilitiesConsentKey])
	stamp, _ := consent["granted_at"].(string)
	if !strings.HasSuffix(stamp, "+00:00") || len(stamp) != len("2006-01-02T15:04:05+00:00") {
		t.Fatalf("granted_at %q", stamp)
	}
}
