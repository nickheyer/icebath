package plugins

import "testing"

func TestHooksAreRegistered(t *testing.T) {
	for _, hook := range []string{"on_human_input_request", "on_human_input_resolved"} {
		if !validHooks[hook] {
			t.Fatalf("%s is not a valid hook", hook)
		}
	}
}
