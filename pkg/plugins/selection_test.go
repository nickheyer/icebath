package plugins

import "testing"

func TestUnknownPluginMessageExportedMatchesTheMessage(t *testing.T) {
	for _, name := range []string{"nonexistent", "observability/langfuse"} {
		if UnknownPluginMessage(name, false) != unknownPluginMessage(name, false) || UnknownPluginMessage(name, true) != unknownPluginMessage(name, true) {
			t.Fatal(name)
		}
	}
}
