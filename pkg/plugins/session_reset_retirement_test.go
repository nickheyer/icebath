package plugins

import "testing"

func TestOnlyTimedModesCountAsRetiredPolicy(t *testing.T) {
	cases := []struct {
		cfg        any
		path, mode string
		ok         bool
	}{
		{map[string]any{"session_reset": map[string]any{"mode": "both"}}, "session_reset", "both", true},
		{map[string]any{"gateway": map[string]any{"session_reset": map[string]any{"mode": "Daily"}}}, "gateway.session_reset", "daily", true},
		{map[string]any{"session_reset": map[string]any{"mode": "none"}}, "", "", false},
		{map[string]any{"session_reset": map[string]any{"idle_minutes": 60}}, "", "", false},
		{map[string]any{}, "", "", false},
	}
	for _, c := range cases {
		path, mode, ok := RetiredResetPolicy(c.cfg)
		if path != c.path || mode != c.mode || ok != c.ok {
			t.Fatalf("RetiredResetPolicy(%v) = %q %q %v", c.cfg, path, mode, ok)
		}
	}
}
