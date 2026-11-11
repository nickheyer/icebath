package gateway

import (
	"context"
	"encoding/json"
	"testing"
)

// Slack mrkdwn descriptor of the format hint cases with the overrides applied
func relayHintsDesc(mod func(d *CapabilityDescriptor)) CapabilityDescriptor {
	d := NewCapabilityDescriptor(1, "slack", "Slack", 4000, false, true, true, "mrkdwn", "chars")
	if mod != nil {
		mod(&d)
	}
	return d
}

func relayBlocks(d *CapabilityDescriptor) { d.SupportsBlockFormatting = true }

func relayDiscordHints(capable bool) func(d *CapabilityDescriptor) {
	return func(d *CapabilityDescriptor) {
		d.Platform, d.Label, d.MarkdownDialect, d.MaxMessageLength, d.SupportsBlockFormatting = "discord", "Discord", "markdown", 2000, capable
	}
}

func relayDraftBlocks(d *CapabilityDescriptor) {
	d.SupportsBlockFormatting, d.SupportsDraftStreaming, d.SupportedOps = true, true, []string{"send", "edit", "draft"}
}

// Enabled adapter with the extras over a capture transport, a slack only one when none is given
func relayHintsAdapter(t *testing.T, extra map[string]any, desc CapabilityDescriptor, c *relayCapture) (*RelayAdapter, *relayCapture) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if c == nil {
		c = &relayCapture{byPlatform: map[string]CapabilityDescriptor{}, identities: [][2]string{{"slack", "icebath"}}}
	}
	c.result = func(map[string]any) (map[string]any, error) {
		return map[string]any{"success": true, "message_id": "1.2"}, nil
	}
	cfg := NewPlatformConfig()
	cfg.Enabled, cfg.Extra = true, extra
	return NewRelayAdapter(cfg, desc, c), c
}

// Transport negotiating a slack and a discord descriptor
func relayTwoPlatformHints() *relayCapture {
	return &relayCapture{byPlatform: map[string]CapabilityDescriptor{"slack": relayHintsDesc(relayBlocks), "discord": relayHintsDesc(relayDiscordHints(false))},
		identities: [][2]string{{"slack", "icebath"}, {"discord", "icebath"}}}
}

// Format hints of the last frame, nil when none
func relayLastHints(t *testing.T, c *relayCapture) any {
	t.Helper()
	frame, _ := c.last(t)
	md, _ := frame["metadata"].(map[string]any)
	return md["format_hints"]
}

func relaySlackKnobs(knobs map[string]any) map[string]any { return map[string]any{"slack": knobs} }

func TestDescriptorBitFromJSONReadsFlag(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"contract_version": 1, "platform": "slack", "label": "Slack", "max_message_length": 4000,
		"supports_draft_streaming": false, "supports_edit": true, "supports_threads": true, "markdown_dialect": "mrkdwn", "len_unit": "chars",
		"supports_block_formatting": true})
	if d, err := CapabilityDescriptorFromJSON(string(payload)); err != nil || !d.SupportsBlockFormatting {
		t.Fatalf("descriptor %+v err %v", d, err)
	}
}

func TestFormatHintsStampingHintsStampedWhenCapableAndEnabled(t *testing.T) {
	a, c := relayHintsAdapter(t, relaySlackKnobs(map[string]any{"rich_blocks": true, "markdown_blocks": true}), relayHintsDesc(relayBlocks), nil)
	a.Send(context.Background(), "D01", "# Report\n\n| a | b |\n|---|---|\n| 1 | 2 |", "", nil)
	if hints := relayLastHints(t, c); !relayMetaEqual(hints, map[string]any{"rich_blocks": true, "markdown_blocks": true}) {
		t.Fatalf("hints %v", hints)
	}
}

func TestFormatHintsStampingNoHintsWhenConnectorLacksCapability(t *testing.T) {
	a, c := relayHintsAdapter(t, relaySlackKnobs(map[string]any{"rich_blocks": true}), relayHintsDesc(nil), nil)
	a.Send(context.Background(), "D01", "text", "", nil)
	if hints := relayLastHints(t, c); hints != nil {
		t.Fatalf("hints %v", hints)
	}
}

func TestFormatHintsStampingNoHintsWhenKnobsOff(t *testing.T) {
	a, c := relayHintsAdapter(t, map[string]any{}, relayHintsDesc(relayBlocks), nil)
	a.Send(context.Background(), "D01", "text", "", nil)
	if hints := relayLastHints(t, c); hints != nil {
		t.Fatalf("hints %v", hints)
	}
}

func TestFormatHintsStampingQuotedFalseKnobStaysOff(t *testing.T) {
	a, c := relayHintsAdapter(t, relaySlackKnobs(map[string]any{"rich_blocks": "false", "markdown_blocks": "false"}), relayHintsDesc(relayBlocks), nil)
	a.Send(context.Background(), "D01", "text", "", nil)
	if hints := relayLastHints(t, c); hints != nil {
		t.Fatalf("hints %v", hints)
	}
}

func TestFormatHintsStampingPartialKnobsStampOnlyEnabled(t *testing.T) {
	a, c := relayHintsAdapter(t, relaySlackKnobs(map[string]any{"markdown_blocks": true}), relayHintsDesc(relayBlocks), nil)
	a.Send(context.Background(), "D01", "text", "", nil)
	if hints := relayLastHints(t, c); !relayMetaEqual(hints, map[string]any{"markdown_blocks": true}) {
		t.Fatalf("hints %v", hints)
	}
}

func TestFormatHintsStampingEditLaneCarriesHintsToo(t *testing.T) {
	a, c := relayHintsAdapter(t, relaySlackKnobs(map[string]any{"rich_blocks": true}), relayHintsDesc(relayBlocks), nil)
	a.EditMessage(context.Background(), "D01", "1.2", "updated **content**", false)
	if hints := relayLastHints(t, c); !relayMetaEqual(hints, map[string]any{"rich_blocks": true}) {
		t.Fatalf("hints %v", hints)
	}
}

func TestFormatHintsStampingDraftInterimAndSealFramesCarryHints(t *testing.T) {
	a, c := relayHintsAdapter(t, relaySlackKnobs(map[string]any{"markdown_blocks": true}), relayHintsDesc(relayDraftBlocks), nil)
	ctx := context.Background()
	if _, err := a.SendDraft(ctx, "D01", 7, "```bash\necho", nil); err != nil {
		t.Fatal(err)
	}
	interim, _ := c.last(t)
	if interim["op"] != "draft" || interim["final"] != false || !relayMetaEqual(relayLastHints(t, c), map[string]any{"markdown_blocks": true}) {
		t.Fatalf("interim %v", interim)
	}
	if _, err := a.sealOpenDraft(ctx, "D01", "```bash\necho hi\n```", nil, relayDraftKey("D01", nil)); err != nil {
		t.Fatal(err)
	}
	final, _ := c.last(t)
	if final["op"] != "draft" || final["final"] != true || !relayMetaEqual(relayLastHints(t, c), map[string]any{"markdown_blocks": true}) {
		t.Fatalf("seal %v", final)
	}
}

func TestFormatHintsStampingDraftFramesNoHintsWhenKnobsOff(t *testing.T) {
	a, c := relayHintsAdapter(t, map[string]any{}, relayHintsDesc(relayDraftBlocks), nil)
	if _, err := a.SendDraft(context.Background(), "D01", 8, "plain text", nil); err != nil {
		t.Fatal(err)
	}
	if hints := relayLastHints(t, c); hints != nil {
		t.Fatalf("hints %v", hints)
	}
}

func TestMultiPlatformResolutionSlackPrimaryDoesNotLeakHintsOntoDiscordChat(t *testing.T) {
	a, c := relayHintsAdapter(t, relaySlackKnobs(map[string]any{"rich_blocks": true}), relayHintsDesc(relayBlocks), relayTwoPlatformHints())
	a.platformByChat["777"] = "discord"
	a.Send(context.Background(), "777", "text", "", nil)
	if hints := relayLastHints(t, c); hints != nil {
		t.Fatalf("Slack-primary adapter stamped Slack format hints on a Discord chat: %v", hints)
	}
}

func TestMultiPlatformResolutionDiscordPrimaryStillStampsHintsForSlackChat(t *testing.T) {
	a, c := relayHintsAdapter(t, relaySlackKnobs(map[string]any{"rich_blocks": true}), relayHintsDesc(relayDiscordHints(false)), relayTwoPlatformHints())
	a.platformByChat["D01"] = "slack"
	a.Send(context.Background(), "D01", "text", "", nil)
	if hints := relayLastHints(t, c); !relayMetaEqual(hints, map[string]any{"rich_blocks": true}) {
		t.Fatalf("Discord-primary adapter suppressed hints for a capable Slack chat: %v", hints)
	}
}

func TestMultiPlatformResolutionSendForPlatformStampsHints(t *testing.T) {
	a, c := relayHintsAdapter(t, relaySlackKnobs(map[string]any{"rich_blocks": true, "markdown_blocks": true}), relayHintsDesc(relayBlocks), relayTwoPlatformHints())
	a.SendForPlatform(context.Background(), PlatformSlack, "C123", "| a |\n|---|", nil)
	if hints := relayLastHints(t, c); !relayMetaEqual(hints, map[string]any{"rich_blocks": true, "markdown_blocks": true}) {
		t.Fatalf("hints %v", hints)
	}
}

func TestMultiPlatformResolutionSendForPlatformNoHintsForIncapablePlatform(t *testing.T) {
	extra := map[string]any{"slack": map[string]any{"rich_blocks": true}, "discord": map[string]any{"rich_blocks": true}}
	a, c := relayHintsAdapter(t, extra, relayHintsDesc(relayBlocks), relayTwoPlatformHints())
	a.SendForPlatform(context.Background(), PlatformDiscord, "777", "text", nil)
	if hints := relayLastHints(t, c); hints != nil {
		t.Fatalf("hints %v", hints)
	}
}
