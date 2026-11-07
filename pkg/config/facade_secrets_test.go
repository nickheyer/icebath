package config

import "testing"

func TestSecretConfigKeysExport(t *testing.T) {
	if !SecretConfigKeys["api_key"] || !SecretConfigKeys["jwt"] || SecretConfigKeys["base_url"] {
		t.Fatalf("secret config keys %v", SecretConfigKeys)
	}
}
