package plugins

import "testing"

func TestEnsureBasicAuthPluginEnabledNoopWhenNotDisabled(t *testing.T) {
	cfg := map[string]any{"plugins": map[string]any{"disabled": []any{"other-plugin"}}}
	if EnsureBasicAuthPluginEnabledInConfig(cfg) {
		t.Fatal("a config that does not disable the basic plugin was reported as changed")
	}
}
