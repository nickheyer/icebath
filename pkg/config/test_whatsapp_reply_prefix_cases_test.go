package config

import "testing"

func TestConfigVersionCoverageDefaultConfigVersionCoversEnvVarVersions(t *testing.T) {
	highest, seen := 0, false
	for version := range EnvVarsByVersion {
		if !seen || version > highest {
			highest, seen = version, true
		}
	}
	if !seen {
		t.Fatal("max() of an empty ENV_VARS_BY_VERSION")
	}
	version, ok := Defaults()["_config_version"].(int)
	if !ok {
		t.Fatalf("_config_version %#v is no int", Defaults()["_config_version"])
	}
	if version < highest {
		t.Fatalf("_config_version %d below the highest ENV_VARS_BY_VERSION key %d", version, highest)
	}
}
