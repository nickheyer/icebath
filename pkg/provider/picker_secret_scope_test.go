package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPickerKeyEnvScope(t *testing.T) {
	newTestHome(t)
	ctx := context.Background()
	read := func(t *testing.T, ctx context.Context, name string) string {
		t.Helper()
		v, err := scopedKeyEnv(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	t.Run("test_unscoped_read_matches_the_process_environment", func(t *testing.T) {
		t.Setenv("ACME_KEY", "from-environment")
		if got := read(t, ctx, "ACME_KEY"); got != "from-environment" {
			t.Fatalf("unscoped %q", got)
		}
	})
	t.Run("test_installed_scope_wins_over_the_process_environment", func(t *testing.T) {
		t.Setenv("ACME_KEY", "other-profile-key")
		if got := read(t, WithSecretScope(ctx, map[string]string{"ACME_KEY": "this-profile-key"}, ""), "ACME_KEY"); got != "this-profile-key" {
			t.Fatalf("installed scope %q", got)
		}
		if got := read(t, ctx, "ACME_KEY"); got != "other-profile-key" {
			t.Fatalf("after scope reset %q", got)
		}
	})
	t.Run("test_absent_key_and_empty_name_resolve_empty", func(t *testing.T) {
		unsetEnvForTest(t, "ACME_KEY")
		if read(t, ctx, "ACME_KEY") != "" || read(t, ctx, "") != "" {
			t.Fatal("absent key or empty name resolved")
		}
	})
	t.Run("test_value_is_stripped", func(t *testing.T) {
		t.Setenv("ACME_KEY", "  padded  ")
		if got := read(t, ctx, "ACME_KEY"); got != "padded" {
			t.Fatalf("stripped %q", got)
		}
	})
}

// Switches to the acme user provider recording the key handed to runtime resolution and the probe
func switchToAcme(t *testing.T, ctx context.Context, userCfg map[string]any) (runtimeKey, probeKey string) {
	t.Helper()
	fakeSwitchPipeline(t, false)
	switchValidateModelFn = func(_ context.Context, _, _ string, opts ValidateModelOptions) ModelVerdict {
		probeKey = opts.APIKey
		return switchAccepted
	}
	saved := switchResolveRuntimeFn
	t.Cleanup(func() { switchResolveRuntimeFn = saved })
	switchResolveRuntimeFn = func(_ context.Context, _, explicitAPIKey, explicitBaseURL, _ string) (*Runtime, error) {
		runtimeKey = explicitAPIKey
		return &Runtime{APIKey: explicitAPIKey, BaseURL: explicitBaseURL}, nil
	}
	mustSwitch(t, ctx, SwitchModelOptions{RawInput: "some-model", CurrentProvider: "openrouter", ExplicitProvider: "acme",
		UserProviders: map[string]any{"acme": userCfg}})
	return runtimeKey, probeKey
}

func TestSwitchModelKeyEnvScope(t *testing.T) {
	scopedSwitch := func(t *testing.T, cfg map[string]any) {
		t.Helper()
		ctx, _ := switchAliasHome(t, "")
		t.Setenv("ACME_KEY", "other-profile-key")
		scoped := WithSecretScope(ctx, map[string]string{"ACME_KEY": "this-profile-key"}, "")
		if key, _ := switchToAcme(t, scoped, cfg); key != "this-profile-key" {
			t.Fatalf("runtime key %q", key)
		}
	}
	t.Run("test_key_env_read_honors_installed_scope", func(t *testing.T) {
		scopedSwitch(t, map[string]any{"base_url": "https://api.acme.test/v1", "key_env": "ACME_KEY"})
	})
	t.Run("test_dollar_var_expansion_honors_installed_scope", func(t *testing.T) {
		scopedSwitch(t, map[string]any{"base_url": "https://api.acme.test/v1", "api_key": "${ACME_KEY}"})
	})
}

func writeAcmeRelayDotenv(t *testing.T, h, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("ACME_RELAY_KEY="+value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSwitchProbeUsesDotenvKeyOverStaleProcessEnv(t *testing.T) {
	ctx, h := switchAliasHome(t, "")
	writeAcmeRelayDotenv(t, h, "fresh-dotenv")
	t.Setenv("ACME_RELAY_KEY", "stale-process")
	if _, probe := switchToAcme(t, ctx, map[string]any{"base_url": "https://api.acme.test/v1", "key_env": "ACME_RELAY_KEY"}); probe != "fresh-dotenv" {
		t.Fatalf("probe key %q", probe)
	}
}

func TestMultiplexScopedMissNeverBorrowsDotenvOrProcessEnv(t *testing.T) {
	h := newTestHome(t)
	writeAcmeRelayDotenv(t, h, "default-profile-key")
	t.Setenv("ACME_RELAY_KEY", "other-profile-key")
	SetMultiplexActive(true)
	t.Cleanup(func() { SetMultiplexActive(false) })
	if v, err := scopedKeyEnv(context.Background(), "ACME_RELAY_KEY"); v != "" || err != nil {
		t.Fatalf("no scope installed must fail closed, got %q %v", v, err)
	}
	if v, err := scopedKeyEnv(WithSecretScope(context.Background(), map[string]string{"OTHER": "x"}, ""), "ACME_RELAY_KEY"); v != "" || err != nil {
		t.Fatalf("scoped miss fell through to %q %v", v, err)
	}
}
