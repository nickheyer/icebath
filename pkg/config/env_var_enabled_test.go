package config

import "testing"

func TestEnvVarEnabledTruthyAndFalsyValues(t *testing.T) {
	const name = "ICEBATH_TEST_ENV_VAR_ENABLED"
	for _, v := range []string{"1", "true", "TRUE", "Yes", "on", " on "} {
		t.Setenv(name, v)
		if !EnvVarEnabled(name) {
			t.Fatalf("%q should enable", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "enabled", "2"} {
		t.Setenv(name, v)
		if EnvVarEnabled(name) {
			t.Fatalf("%q should not enable", v)
		}
	}
}
