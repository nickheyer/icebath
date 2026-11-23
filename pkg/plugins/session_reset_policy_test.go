package plugins

import "testing"

func TestOnlyTimedModesCountAsATimedSessionReset(t *testing.T) {
	cases := []struct {
		cfg  any
		mode string
		ok   bool
	}{
		{map[string]any{"session_reset": map[string]any{"mode": "both"}}, "both", true},
		{map[string]any{"session_reset": map[string]any{"mode": " Daily "}}, "daily", true},
		{map[string]any{"session_reset": map[string]any{"mode": "none"}}, "", false},
		{map[string]any{"session_reset": map[string]any{"idle_minutes": 60}}, "", false},
		{map[string]any{}, "", false},
	}
	for _, c := range cases {
		mode, ok := TimedSessionReset(c.cfg)
		if mode != c.mode || ok != c.ok {
			t.Fatalf("TimedSessionReset(%v) = %q %v", c.cfg, mode, ok)
		}
	}
}
