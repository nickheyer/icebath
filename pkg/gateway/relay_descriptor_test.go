package gateway

import (
	"slices"
	"testing"
)

func TestSupportsOpLegacyConnectorAssumesLegacySet(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	d := NewCapabilityDescriptor(RelayContractVersion, "telegram", "Telegram", 4096, false, true, true, "markdown_v2", "utf16")
	d.Emoji = "✈️"
	d.PlatformHint = "You are on Telegram."
	for _, op := range []string{"send", "edit", "typing", "follow_up"} {
		if !d.SupportsOp(op) {
			t.Fatalf("legacy op %s unsupported", op)
		}
	}
	if d.SupportsOp("get_chat_info") {
		t.Fatal("new op assumed on a legacy connector")
	}
}

func TestFromJSONNormalizesMalformedSupportedOps(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	base := `{"contract_version": 1, "platform": "x", "label": "X", "max_message_length": 2000, "supports_draft_streaming": false, ` +
		`"supports_edit": true, "supports_threads": false, "markdown_dialect": "plain", "len_unit": "chars", `
	d, err := CapabilityDescriptorFromJSON(base + `"supported_ops": "send"}`)
	if err != nil || len(d.SupportedOps) != 0 {
		t.Fatalf("ops %v err %v", d.SupportedOps, err)
	}
	d, err = CapabilityDescriptorFromJSON(base + `"supported_ops": ["send", 7, null, ""]}`)
	if err != nil || !slices.Equal(d.SupportedOps, []string{"send"}) {
		t.Fatalf("ops %v err %v", d.SupportedOps, err)
	}
}
