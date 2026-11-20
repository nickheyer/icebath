package tools

import (
	"strings"
	"testing"
)

func TestStripLegacyBotProtocolKeepsNeighbouringSections(t *testing.T) {
	legacy := "# Me\n\nBe terse.\n\n## Messaging other agents\n\nold plugin text\n- `bot-a`\n"
	soul := StripLegacyBotProtocol(legacy + "\n## Symbols\n\nuse ◆\n")
	if strings.Contains(soul, "Messaging other agents") || strings.Contains(soul, "old plugin text") {
		t.Fatalf("%q", soul)
	}
	if !strings.Contains(soul, "Be terse.") || !strings.Contains(soul, "use ◆") {
		t.Fatalf("%q", soul)
	}
	if got := StripLegacyBotProtocol(legacy); got != "# Me\n\nBe terse.\n" {
		t.Fatalf("%q", got)
	}
	if got := StripLegacyBotProtocol("# Worker\n\n## Messaging other agents\nx\n"); got != "# Worker\n" {
		t.Fatalf("%q", got)
	}
	if StripLegacyBotProtocol(soul) != soul {
		t.Fatal("not idempotent")
	}
	if plain := "# Me\nno section"; StripLegacyBotProtocol(plain) != plain {
		t.Fatal("text without the heading must stay byte identical")
	}
}
