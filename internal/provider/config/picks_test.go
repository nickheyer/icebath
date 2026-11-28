package config

import "testing"

func TestBesideKeepsNames(t *testing.T) {
	for key, want := range map[string]string{
		"agent.model":                      "agent.provider",
		"auxiliary.vision.fallback_model":  "auxiliary.vision.fallback_provider",
		"agent.aliases.Fast.model":         "agent.aliases.Fast.provider",
		"tools.mcp.servers.GitHub.account": "tools.mcp.servers.GitHub.provider",
		"providers.acme.default_model":     "",
		"agent.max_turns":                  "",
	} {
		if got := Beside(key); got != want {
			t.Fatalf("beside %s is %q, not %q", key, got, want)
		}
	}
}
