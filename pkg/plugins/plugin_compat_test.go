package plugins

import (
	"testing"
)

// old_updater_surface.json plugin_compat rows: the three frozen exports stay inert
func TestPluginCompatFrozenExports(t *testing.T) {
	if r := CompatReport(nil, true); len(r) != 0 || r == nil {
		t.Fatalf("report %v", r)
	}
	if !RemovalInEffect(nil) || len(SummaryLines(map[string][]any{}, nil)) != 0 {
		t.Fatal("removal and summary")
	}
}

const legacyFixtureInit = `"""Frozen legacy plugin used by the behavior compatibility suite."""

received_sessions = []


def _on_session_start(*, session_id):
    received_sessions.append(session_id)
    return {"legacy_session_id": session_id, "received": list(received_sessions)}


def register(ctx):
    ctx.register_hook("on_session_start", _on_session_start)
`
