package plugins

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Granted ids are stripped before the registry filter so a padded grant still opens its gate
func TestGrantedCapabilitiesStripBeforeKnownFilter(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	entry := func(grants string) context.Context {
		h := t.TempDir()
		writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  entries:\n    p:\n      "+GrantedCapabilitiesKey+": "+grants+"\n")
		return homeCtx(h)
	}
	ctx := entry(`[" gateway.platform_actions\n", 7, bogus, gateway.platform_actions]`)
	if got := GrantedCapabilities(ctx, "p"); len(got) != 1 || !got[PlatformActionCapability] {
		t.Fatalf("granted %v", got)
	}
	if !CapabilityGranted(ctx, "p", PlatformActionCapability) || CapabilityGranted(ctx, "p", "tools.override") {
		t.Fatal("gate decisions")
	}
	if got := GrantedCapabilities(entry("gateway.platform_actions"), "p"); len(got) != 0 {
		t.Fatalf("non list grant %v", got)
	}
	if CapabilityGranted(ctx, "p", "no.such.capability") {
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
	consent := asMap(pluginEntry(homeCtx(h), "p")[CapabilitiesConsentKey])
	stamp, _ := consent["granted_at"].(string)
	if !strings.HasSuffix(stamp, "+00:00") || len(stamp) != len("2006-01-02T15:04:05+00:00") {
		t.Fatalf("granted_at %q", stamp)
	}
}
