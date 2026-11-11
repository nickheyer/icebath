package gateway

import (
	"encoding/json"
	"testing"
)

// Slack descriptor of the in channel continuable cases with the overrides applied
func relayContinuableDesc(mod func(d *CapabilityDescriptor)) CapabilityDescriptor {
	d := NewCapabilityDescriptor(1, "slack", "Slack", 4000, false, true, true, "mrkdwn", "chars")
	if mod != nil {
		mod(&d)
	}
	return d
}

func relayContinuableOn(d *CapabilityDescriptor) { d.SupportsInchannelContinuable = true }

func relayContinuableDiscord(d *CapabilityDescriptor) {
	d.Platform, d.Label, d.MarkdownDialect = "discord", "Discord", "markdown"
}

func relayContinuablePayload(withFlag bool) string {
	payload := map[string]any{"contract_version": 1, "platform": "slack", "label": "Slack", "max_message_length": 4000,
		"supports_draft_streaming": false, "supports_edit": true, "supports_threads": true, "markdown_dialect": "mrkdwn", "len_unit": "chars"}
	if withFlag {
		payload["supports_inchannel_continuable"] = true
	}
	text, _ := json.Marshal(payload)
	return string(text)
}

func TestDescriptorCapabilityBitFromJSONReadsFlag(t *testing.T) {
	d, err := CapabilityDescriptorFromJSON(relayContinuablePayload(true))
	if err != nil || !d.SupportsInchannelContinuable {
		t.Fatalf("descriptor %+v err %v", d, err)
	}
}

func TestDescriptorCapabilityBitFromJSONMissingFlagDefaultsFalse(t *testing.T) {
	d, err := CapabilityDescriptorFromJSON(relayContinuablePayload(false))
	if err != nil || d.SupportsInchannelContinuable {
		t.Fatalf("descriptor %+v err %v", d, err)
	}
}

// Enabled adapter with empty extras over a transport negotiating the given descriptors
func relayContinuableAdapter(t *testing.T, primary CapabilityDescriptor, byPlatform map[string]CapabilityDescriptor) *RelayAdapter {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cfg := NewPlatformConfig()
	cfg.Enabled, cfg.Extra = true, map[string]any{}
	if byPlatform == nil {
		return NewRelayAdapter(cfg, primary, nil)
	}
	c := &relayCapture{byPlatform: byPlatform}
	for p := range byPlatform {
		c.identities = append(c.identities, [2]string{p, "icebath"})
	}
	return NewRelayAdapter(cfg, primary, c)
}

func TestRelayAdapterCapabilityMappingRenegotiationUpdatesFlag(t *testing.T) {
	a := relayContinuableAdapter(t, relayContinuableDesc(nil), nil)
	a.applyDescriptor(relayContinuableDesc(relayContinuableOn))
	if !a.Traits.SupportsInchannelContinuable {
		t.Fatal("renegotiated flag not adopted")
	}
}

func relayContinuableFor(t *testing.T, a *RelayAdapter, platform string) bool {
	t.Helper()
	ok, err := a.SupportsInchannelContinuableForPlatform(platform)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestPerPlatformCapabilityPrimaryTrueDoesNotLeakOntoOtherPlatform(t *testing.T) {
	slack, discord := relayContinuableDesc(relayContinuableOn), relayContinuableDesc(relayContinuableDiscord)
	a := relayContinuableAdapter(t, slack, map[string]CapabilityDescriptor{"slack": slack, "discord": discord})
	if !relayContinuableFor(t, a, "slack") || relayContinuableFor(t, a, "discord") {
		t.Fatal("primary bit leaked")
	}
}

func TestPerPlatformCapabilityNonprimaryAdvertisedBitIsHonored(t *testing.T) {
	slack, discord := relayContinuableDesc(relayContinuableOn), relayContinuableDesc(relayContinuableDiscord)
	a := relayContinuableAdapter(t, discord, map[string]CapabilityDescriptor{"slack": slack, "discord": discord})
	if !relayContinuableFor(t, a, "slack") || relayContinuableFor(t, a, "discord") {
		t.Fatal("non primary bit not honored")
	}
}

func TestPerPlatformCapabilityUnknownPlatformFallsBackToScalar(t *testing.T) {
	slack := relayContinuableDesc(relayContinuableOn)
	a := relayContinuableAdapter(t, slack, map[string]CapabilityDescriptor{"slack": slack})
	if !relayContinuableFor(t, a, "matrix") {
		t.Fatal("unknown platform must take the primary scalar")
	}
}
