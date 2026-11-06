package plugins

import "testing"

// Covers agent_plugins liveness_for returning a copy and None after a clear
func TestPortableLivenessReturnsACopyAndNoneWhenCleared(t *testing.T) {
	t.Cleanup(func() { clearPortableLiveness("live-srv") })
	if got := PortableLiveness("live-srv"); got != nil {
		t.Fatalf("undeclared server = %v, want nil", got)
	}
	setPortableLiveness("live-srv", map[string]any{"kind": "server_json", "path": "run/server.json"})
	got := PortableLiveness("live-srv")
	if got["kind"] != "server_json" || got["path"] != "run/server.json" || len(got) != 2 {
		t.Fatalf("declared liveness = %v", got)
	}
	got["kind"] = "static"
	if again := PortableLiveness("live-srv"); again["kind"] != "server_json" {
		t.Fatalf("caller mutation leaked into the registry: %v", again)
	}
	clearPortableLiveness("live-srv")
	if got := PortableLiveness("live-srv"); got != nil {
		t.Fatalf("cleared server = %v, want nil", got)
	}
}
