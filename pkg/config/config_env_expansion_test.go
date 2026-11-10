package config

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestExpandEnvVars(t *testing.T) {
	t.Run("test_non_string_values_untouched", func(t *testing.T) {
		for _, v := range []any{42, 3.14, true, nil} {
			if got := Expand(v, EnvRefLookupContext(context.Background())); got != v {
				t.Fatalf("%#v became %#v", v, got)
			}
		}
	})
}

func TestLoadConfigExpansion(t *testing.T) {
	t.Run("test_load_config_expands_env_vars", func(t *testing.T) {
		ctx := context.Background()
		h, _ := facadeHome(t, "model:\n  api_key: ${GOOGLE_API_KEY}\nplatforms:\n  telegram:\n    token: ${TELEGRAM_BOT_TOKEN}\nplain: no-substitution\n")
		t.Setenv("GOOGLE_API_KEY", "gsk-test-key")
		t.Setenv("TELEGRAM_BOT_TOKEN", "1234567:ABC-token")
		cfg, err := LoadContext(ctx, h, EnvRefLookupContext(ctx))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Get("model.api_key") != "gsk-test-key" || cfg.Get("platforms.telegram.token") != "1234567:ABC-token" || cfg.Get("plain") != "no-substitution" {
			t.Fatalf("%v %v %v", cfg.Get("model.api_key"), cfg.Get("platforms.telegram.token"), cfg.Get("plain"))
		}
	})
}

func TestLoadConfigCacheEnvStaleness(t *testing.T) {
	t.Run("test_env_var_appearing_after_first_load_invalidates_cache", func(t *testing.T) {
		ctx := context.Background()
		h, _ := facadeHome(t, "auxiliary:\n  vision:\n    api_key: ${LATE_DOTENV_KEY_58514}\n")
		unsetEnv(t, "LATE_DOTENV_KEY_58514")
		cfg, err := LoadContext(ctx, h, EnvRefLookupContext(ctx))
		if err != nil || cfg.Get("auxiliary.vision.api_key") != "${LATE_DOTENV_KEY_58514}" {
			t.Fatalf("first load %v %v", err, cfg.Get("auxiliary.vision.api_key"))
		}
		t.Setenv("LATE_DOTENV_KEY_58514", "nvapi-real")
		cfg, err = LoadContext(ctx, h, EnvRefLookupContext(ctx))
		if err != nil || cfg.Get("auxiliary.vision.api_key") != "nvapi-real" {
			t.Fatalf("after dotenv %v %v", err, cfg.Get("auxiliary.vision.api_key"))
		}
	})
	t.Run("test_unchanged_env_still_serves_cache", func(t *testing.T) {
		ctx := context.Background()
		h, _ := facadeHome(t, "providers:\n  mistral:\n    api_key: ${STABLE_KEY_58514}\n")
		t.Setenv("STABLE_KEY_58514", "key-stable")
		if _, err := LoadContext(ctx, h, EnvRefLookupContext(ctx)); err != nil {
			t.Fatal(err)
		}
		first, err := LoadReadonly(ctx, h, EnvRefLookupContext(ctx))
		if err != nil {
			t.Fatal(err)
		}
		second, _ := LoadReadonly(ctx, h, EnvRefLookupContext(ctx))
		if reflect.ValueOf(first).Pointer() != reflect.ValueOf(second).Pointer() {
			t.Fatal("readonly loads rebuilt instead of serving the cached object")
		}
		if first.Get("providers.mistral.api_key") != "key-stable" {
			t.Fatalf("%v", first.Get("providers.mistral.api_key"))
		}
	})
}

func TestValueContainingColonIsNotASourceRef(t *testing.T) {
	unsetEnv(t, "MY:WEIRD")
	if got := ExpandString("${MY:WEIRD}", EnvRefLookupContext(context.Background())); got != "${MY:WEIRD}" {
		t.Fatalf("got %q", got)
	}
	if isNonEnvSecretRef("MY:WEIRD") || !isNonEnvSecretRef("bitwarden:FOO") {
		t.Fatal("only a lowercase ident prefix is a source")
	}
}

func TestSnapshotDetectsRotationForEnvPrefixed(t *testing.T) {
	lookup := EnvRefLookupContext(context.Background())
	t.Setenv("PARITY_ROT", "before")
	snap1 := RefSnapshot(map[string]any{"k": "${env:PARITY_ROT}"}, lookup)
	t.Setenv("PARITY_ROT", "after")
	snap2 := RefSnapshot(map[string]any{"k": "${env:PARITY_ROT}"}, lookup)
	if reflect.DeepEqual(snap1, snap2) || *snap1["PARITY_ROT"] != "before" || *snap2["PARITY_ROT"] != "after" {
		t.Fatalf("snapshots %v %v", snap1, snap2)
	}
}

// Scope hooks route a bound scope through the scoped reader and everything else through the process env
func TestEnvRefLookupContextFollowsTheBoundScope(t *testing.T) {
	type scopeKey struct{}
	prev := hooks()
	h := prev
	h.SecretScopeBound = func(ctx context.Context) bool { return ctx.Value(scopeKey{}) != nil }
	h.ScopedSecret = func(ctx context.Context, key string) (string, bool, error) {
		v, ok := ctx.Value(scopeKey{}).(map[string]string)[key]
		return v, ok, nil
	}
	RegisterConfigFacade(h)
	t.Cleanup(func() { RegisterConfigFacade(prev) })
	t.Setenv("MATRIX_ACCESS_TOKEN", "default-token")
	scoped := context.WithValue(context.Background(), scopeKey{}, map[string]string{"OTHER_KEY": "x"})
	if got := ExpandString("${MATRIX_ACCESS_TOKEN}", EnvRefLookupContext(scoped)); got != "${MATRIX_ACCESS_TOKEN}" {
		t.Fatalf("scoped miss read the process env %q", got)
	}
	if got := ExpandString("${MATRIX_ACCESS_TOKEN}", EnvRefLookupContext(context.Background())); got != "default-token" {
		t.Fatalf("unscoped %q", got)
	}
}

// Removes an env var for the test, restoring it afterwards
func unsetEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	os.Unsetenv(name)
}

func TestSaveConfigPreservesUnresolvedEnvRefs(t *testing.T) {
	ctx := context.Background()
	h, path := facadeHome(t, "custom_providers:\n  - name: unresolved\n    api_key: ${MISSING_SECRET}\n    model: claude-opus-4-6\nmodel:\n  default: claude-opus-4-6\n")
	unsetEnv(t, "MISSING_SECRET")
	cfg, err := LoadContext(ctx, h, EnvRefLookupContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	cfg["display"].(map[string]any)["compact"] = true
	if err := SaveConfig(ctx, h, map[string]any(cfg), SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	if text, _ := os.ReadFile(path); !strings.Contains(string(text), "api_key: ${MISSING_SECRET}") {
		t.Fatalf("saved %s", text)
	}
}

func TestSaveConfigAllowsIntentionalSecretValueChange(t *testing.T) {
	ctx := context.Background()
	h, path := facadeHome(t, "custom_providers:\n  - name: tuzi\n    api_key: ${TU_ZI_API_KEY}\n    model: claude-opus-4-6\nmodel:\n  default: claude-opus-4-6\n")
	t.Setenv("TU_ZI_API_KEY", "sk-old-secret")
	cfg, err := LoadContext(ctx, h, EnvRefLookupContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	cfg["custom_providers"].([]any)[0].(map[string]any)["api_key"] = "sk-new-secret"
	if err := SaveConfig(ctx, h, map[string]any(cfg), SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(path)
	if !strings.Contains(string(text), "api_key: sk-new-secret") || strings.Contains(string(text), "${TU_ZI_API_KEY}") {
		t.Fatalf("saved %s", text)
	}
}

// Fails until every unported config.py test case listed under rvconfiga_s24 in dev/notes/config.md has a Go test
func TestRemainingConfigPyCases(t *testing.T) { portRemainingConfigPyCases(t) }
