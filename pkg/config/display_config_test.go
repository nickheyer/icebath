package config

import "testing"

func TestDisplayOverrideableKeysAreTheGlobalDisplayDefaults(t *testing.T) {
	keys := DisplayOverrideableKeys()
	if len(keys) != 13 || !keys["tool_progress"] || !keys["busy_steer_ack_enabled"] || !keys["live_status"] || keys["webhook"] {
		t.Fatal(keys)
	}
}
