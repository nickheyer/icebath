package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

var shadowedBuiltins = [][2]string{{"deepseek", "DEEPSEEK_API_KEY"}, {"anthropic", "ANTHROPIC_API_KEY"}}

// Profile whose providers entry is keyed like a built in, with both the entry and built in keys exported
func shadowHome(t *testing.T, name, builtinEnv string) string {
	t.Helper()
	h := newTestHome(t)
	raw, err := yaml.Marshal(shadowConfig(name, nil))
	if err != nil {
		t.Fatal(err)
	}
	writeConfigYAML(t, h, string(raw))
	auth, _ := json.Marshal(map[string]any{"version": 1, "providers": map[string]any{}, "credential_pool": map[string]any{}})
	if err := os.WriteFile(filepath.Join(h, "auth.json"), auth, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(builtinEnv, shadowBuiltin)
	t.Setenv("ENTRY_KEY_VAR", shadowEntryKey)
	return h
}

// Updates the model section and the deepseek entry of the profile config
func shadowEditConfig(t *testing.T, h string, model, entry map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	for k, v := range model {
		cfg["model"].(map[string]any)[k] = v
	}
	for k, v := range entry {
		cfg["providers"].(map[string]any)["deepseek"].(map[string]any)[k] = v
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	writeConfigYAML(t, h, string(out))
}

func TestFrozenRuntimeConfigConcurrentScopesStayApart(t *testing.T) {
	h := newTestHome(t)
	writeConfigYAML(t, h, "model:\n  default: live-model\n")
	base := context.Background()
	if FrozenRuntimeConfig(base) != nil {
		t.Fatal("a bare context carries a frozen config")
	}
	var ready, done sync.WaitGroup
	ready.Add(2)
	release := make(chan struct{})
	errs := make(chan string, 8)
	for _, model := range []string{"frozen-a", "frozen-b"} {
		done.Add(1)
		go func(model string) {
			defer done.Done()
			ctx := WithFrozenRuntimeConfig(base, map[string]any{"model": map[string]any{"default": model}})
			ready.Done()
			<-release
			for i := 0; i < 200; i++ {
				section, _ := FrozenRuntimeConfig(ctx)["model"].(map[string]any)
				if section["default"] != model {
					errs <- "scope " + model + " saw " + cfgString(section["default"])
					return
				}
				loaded, _ := LoadConfigSafe(ctx, h)["model"].(map[string]any)
				if loaded["default"] != model {
					errs <- "LoadConfigSafe in scope " + model + " saw " + cfgString(loaded["default"])
					return
				}
				if _, ok := FrozenRuntimeConfig(ctx)["checkpoints"].(map[string]any); !ok {
					errs <- "scope " + model + " is not merged over the defaults"
					return
				}
			}
		}(model)
	}
	ready.Wait()
	close(release)
	done.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
	live, _ := LoadConfigSafe(base, h)["model"].(map[string]any)
	if live["default"] != "live-model" {
		t.Fatalf("outside every scope the live config is read, got %v", live["default"])
	}
}

func TestShadowingEntryFrozenPolicySendsItsKeyEnv(t *testing.T) {
	for _, tc := range shadowedBuiltins {
		t.Run(tc[0], func(t *testing.T) {
			shadowHome(t, tc[0], tc[1])
			ctx := context.Background()
			frozen := modelsConfig(ctx)
			resolved, err := modelsResolver(ctx).resolveNamedCustomRuntime("custom:"+tc[0], "", "", "", frozen)
			if err != nil || resolved == nil {
				t.Fatalf("named custom runtime: %v %v", resolved, err)
			}
			if resolved.BaseURL != shadowEndpoint || resolved.APIKey != shadowEntryKey {
				t.Fatalf("named rung got (%s, %s)", resolved.BaseURL, resolved.APIKey)
			}
			rt, err := ResolveRuntimeProvider(WithFrozenRuntimeConfig(ctx, frozen), "", "custom:"+tc[0], "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			if rt.BaseURL != shadowEndpoint || rt.APIKey != shadowEntryKey {
				t.Fatalf("frozen scope got (%s, %s)", rt.BaseURL, rt.APIKey)
			}
		})
	}
}

func TestUrlMatchedBareCustomFrozenRouteSendsTheEntryKey(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "model_base_url"
		if explicit {
			name = "explicit_base_url"
		}
		t.Run(name, func(t *testing.T) {
			h := shadowHome(t, "deepseek", "DEEPSEEK_API_KEY")
			shadowEditConfig(t, h, map[string]any{"provider": "custom", "base_url": shadowEndpoint}, nil)
			ctx := context.Background()
			frozen := modelsConfig(ctx)
			shadowEditConfig(t, h, nil, map[string]any{"key_env": "OTHER_UNSET_VAR"})
			explicitURL := ""
			if explicit {
				explicitURL = shadowEndpoint
			}
			rt, err := ResolveRuntimeProvider(WithFrozenRuntimeConfig(ctx, frozen), "", "custom", "", explicitURL, "")
			if err != nil {
				t.Fatal(err)
			}
			if rt.BaseURL != shadowEndpoint || rt.APIKey != shadowEntryKey {
				t.Fatalf("frozen route got (%s, %s)", rt.BaseURL, rt.APIKey)
			}
		})
	}
}
