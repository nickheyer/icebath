package provider

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

const aliasCredHost = "https://theta.example.com/v1"
const aliasCredDefaultSecret = "sk-or-DEFAULT-PROVIDER-SECRET"

type aliasProbe struct {
	called          bool
	apiKey, baseURL string
}

// Switches to the theta alias from the given session with the probe credential recorded
func switchToThetaAlias(t *testing.T, sessionProvider, sessionModel, sessionBaseURL, sessionKey string, alias map[string]string) (ModelSwitchResult, *aliasProbe) {
	t.Helper()
	keys := make([]string, 0, len(alias))
	for k := range alias {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "model:\n  default: %s\n  provider: %s\nmodel_aliases:\n  theta:\n", sessionModel, sessionProvider)
	for _, k := range keys {
		fmt.Fprintf(&b, "    %s: %q\n", k, alias[k])
	}
	kept := map[string]string{}
	for _, k := range seedableEnvVars {
		if v := os.Getenv(k); v != "" {
			kept[k] = v
		}
	}
	ctx, _ := switchAliasHome(t, b.String())
	for k, v := range kept {
		t.Setenv(k, v)
	}
	fakeSwitchPipeline(t, false)
	probe := &aliasProbe{}
	switchValidateModelFn = func(_ context.Context, _, _ string, opts ValidateModelOptions) ModelVerdict {
		probe.called, probe.apiKey, probe.baseURL = true, opts.APIKey, opts.BaseURL
		return switchAccepted
	}
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "theta", CurrentProvider: sessionProvider,
		CurrentBaseURL: sessionBaseURL, CurrentAPIKey: sessionKey})
	return res, probe
}

func switchFromOpenRouterToTheta(t *testing.T, alias map[string]string) (ModelSwitchResult, *aliasProbe) {
	t.Helper()
	t.Setenv("OPENROUTER_API_KEY", aliasCredDefaultSecret)
	return switchToThetaAlias(t, "openrouter", "gpt-4", "https://openrouter.ai/api/v1", aliasCredDefaultSecret, alias)
}

func TestModelSwitchUsesAliasCredential(t *testing.T) {
	newTestHome(t)
	t.Run("alias api key is sent to alias host", func(t *testing.T) {
		res, probe := switchFromOpenRouterToTheta(t, map[string]string{"model": "theta-1", "provider": "custom",
			"base_url": aliasCredHost, "api_key": "sk-theta-ALIAS-SECRET"})
		if res.BaseURL != aliasCredHost || res.APIKey != "sk-theta-ALIAS-SECRET" || probe.apiKey != "sk-theta-ALIAS-SECRET" {
			t.Fatalf("result %+v probe %+v", res, probe)
		}
	})
	t.Run("alias key env is sent to alias host", func(t *testing.T) {
		t.Setenv("THETA_API_KEY", "sk-theta-FROM-ENV")
		res, _ := switchFromOpenRouterToTheta(t, map[string]string{"model": "theta-1", "provider": "custom",
			"base_url": aliasCredHost, "key_env": "THETA_API_KEY"})
		if res.APIKey != "sk-theta-FROM-ENV" {
			t.Fatalf("result %+v", res)
		}
	})
	t.Run("default provider key never reaches the alias host", func(t *testing.T) {
		res, probe := switchFromOpenRouterToTheta(t, map[string]string{"model": "theta-1", "provider": "custom", "base_url": aliasCredHost})
		if res.BaseURL != aliasCredHost || res.APIKey == aliasCredDefaultSecret || probe.apiKey == aliasCredDefaultSecret || probe.baseURL != aliasCredHost {
			t.Fatalf("result %+v probe %+v", res, probe)
		}
	})
	t.Run("same host alias still resolves that host key", func(t *testing.T) {
		res, _ := switchFromOpenRouterToTheta(t, map[string]string{"model": "theta-1", "provider": "custom", "base_url": "https://openrouter.ai/api/v1"})
		if res.APIKey != aliasCredDefaultSecret {
			t.Fatalf("result %+v", res)
		}
	})
	t.Run("ollama cloud alias resolves ollama api key", func(t *testing.T) {
		t.Setenv("OLLAMA_API_KEY", "sk-ollama-KEY")
		res, _ := switchFromOpenRouterToTheta(t, map[string]string{"model": "qwen3.5:397b", "provider": "custom", "base_url": "https://ollama.com/v1"})
		if res.APIKey != "sk-ollama-KEY" {
			t.Fatalf("result %+v", res)
		}
	})
}

func TestSessionKeyIsHostScoped(t *testing.T) {
	newTestHome(t)
	alias := map[string]string{"model": "theta-1", "provider": "custom", "base_url": "https://myhost.test/v1"}
	if res, _ := switchToThetaAlias(t, "ollama-launch", "m", "https://myhost.test/v1", "sk-session-KEY", alias); res.APIKey != "sk-session-KEY" {
		t.Fatalf("same host alias dropped the session key %+v", res)
	}
	if res, _ := switchToThetaAlias(t, "ollama-launch", "m", "https://elsewhere.test/v1", "sk-session-KEY", alias); res.APIKey == "sk-session-KEY" {
		t.Fatalf("different host alias kept the session key %+v", res)
	}
}

func TestBuiltinProviderKeysDoNotLeak(t *testing.T) {
	newTestHome(t)
	for _, tc := range []struct{ provider, envVar, sessionBaseURL string }{
		{"anthropic", "ANTHROPIC_API_KEY", "https://api.anthropic.com"},
		{"openai", "OPENAI_API_KEY", "https://api.openai.com/v1"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			secret := "sk-" + tc.provider + "-SECRET"
			t.Setenv(tc.envVar, secret)
			res, probe := switchToThetaAlias(t, tc.provider, "m", tc.sessionBaseURL, secret,
				map[string]string{"model": "theta-1", "provider": "custom", "base_url": aliasCredHost})
			if res.BaseURL != aliasCredHost || res.APIKey == secret || probe.apiKey == secret {
				t.Fatalf("result %+v probe %+v", res, probe)
			}
		})
	}
}

func TestProviderLabelCannotSelectAKeyForAnArbitraryHost(t *testing.T) {
	newTestHome(t)
	for _, tc := range []struct{ provider, envVar string }{
		{"anthropic", "ANTHROPIC_API_KEY"}, {"openai", "OPENAI_API_KEY"}, {"openrouter", "OPENROUTER_API_KEY"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			secret := "sk-" + tc.provider + "-SECRET"
			t.Setenv(tc.envVar, secret)
			res, probe := switchToThetaAlias(t, "openrouter", "m", "https://openrouter.ai/api/v1", "sk-session",
				map[string]string{"model": "c", "provider": tc.provider, "base_url": "https://evil.test/v1"})
			if res.APIKey == secret || (probe.called && probe.apiKey == secret) {
				t.Fatalf("result %+v probe %+v", res, probe)
			}
		})
	}
	t.Run("authoritative host still resolves its vendor key", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "sk-anthropic-SECRET")
		res, _ := switchToThetaAlias(t, "openrouter", "m", "https://openrouter.ai/api/v1", "sk-session",
			map[string]string{"model": "c", "provider": "anthropic", "base_url": "https://api.anthropic.com/v1"})
		if res.APIKey != "sk-anthropic-SECRET" {
			t.Fatalf("result %+v", res)
		}
	})
}

func TestSessionCredentialIsScopedToTheOrigin(t *testing.T) {
	newTestHome(t)
	sw := func(t *testing.T, aliasURL, sessionURL string) ModelSwitchResult {
		res, _ := switchToThetaAlias(t, "my-endpoint", "m", sessionURL, "sk-SESSION-SECRET",
			map[string]string{"model": "m2", "provider": "custom", "base_url": aliasURL})
		return res
	}
	for _, tc := range [][3]string{
		{"http://api.example.com/v1", "https://api.example.com/v1", "https to http"},
		{"https://api.example.com:8443/v1", "https://api.example.com/v1", "port change"},
		{"http://api.example.com:8080/v1", "https://api.example.com/v1", "scheme and port"},
		{"https://other.example.com/v1", "https://api.example.com/v1", "cross host"},
	} {
		t.Run(tc[2], func(t *testing.T) {
			if res := sw(t, tc[0], tc[1]); res.APIKey == "sk-SESSION-SECRET" {
				t.Fatalf("session credential followed the origin change %+v", res)
			}
		})
	}
	for _, url := range []string{"https://api.example.com/v1", "http://127.0.0.1:11434/v1", "http://localhost:8080/v1"} {
		t.Run(url, func(t *testing.T) {
			if res := sw(t, url, url); res.APIKey != "sk-SESSION-SECRET" {
				t.Fatalf("same origin dropped the session credential %+v", res)
			}
		})
	}
	t.Run("default port and explicit port", func(t *testing.T) {
		if res := sw(t, "https://api.example.com:443/v1", "https://api.example.com/v1"); res.APIKey != "sk-SESSION-SECRET" {
			t.Fatalf("result %+v", res)
		}
	})
}

func TestSwitchModelSurfacesAmbiguityMessage(t *testing.T) {
	ctx, _ := switchAliasHome(t, "")
	fakeSwitchPipeline(t, true)
	prevList, prevStatic := listProviderModelsFn, staticCatalogFn
	t.Cleanup(func() { listProviderModelsFn, staticCatalogFn = prevList, prevStatic })
	staticCatalogFn = func(context.Context, string) []string { return nil }
	listProviderModelsFn = func(context.Context, string) []string {
		return []string{"claude-opus-4-8", "claude-opus-4-20250514"}
	}
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "opus", CurrentProvider: "anthropic"})
	if res.Success || !strings.Contains(res.ErrorMessage, "claude-opus-4-8") || !strings.Contains(res.ErrorMessage, "claude-opus-4-20250514") {
		t.Fatalf("result %+v", res)
	}
}

func TestSwitchModelDirectAliasOverride(t *testing.T) {
	emptyRuntime := &Runtime{Provider: "custom", APIMode: "openai_compat"}
	t.Run("uses alias base url", func(t *testing.T) {
		ctx, _ := switchAliasHome(t, "")
		fakeSwitchPipeline(t, true)
		fakeSwitchRuntime(t, emptyRuntime, nil)
		seedDirectAliases(NamedDirectAlias{"qwen", DirectAlias{Model: "qwen3.5:397b", Provider: "custom", BaseURL: "https://ollama.com/v1"}})
		res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "qwen", CurrentProvider: "openrouter"})
		if !res.Success || res.BaseURL != "https://ollama.com/v1" || res.NewModel != "qwen3.5:397b" {
			t.Fatalf("result %+v", res)
		}
	})
	t.Run("alias without api key gets the no key marker", func(t *testing.T) {
		ctx, _ := switchAliasHome(t, "")
		fakeSwitchPipeline(t, true)
		fakeSwitchRuntime(t, emptyRuntime, nil)
		seedDirectAliases(NamedDirectAlias{"local", DirectAlias{Model: "local-model", Provider: "custom", BaseURL: "http://localhost:11434/v1"}})
		res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "local", CurrentProvider: "openrouter"})
		if !res.Success || res.APIKey != "no-key-required" || res.BaseURL != "http://localhost:11434/v1" {
			t.Fatalf("result %+v", res)
		}
	})
}

const providerABConfig = "model:\n  provider: provider-a\n  default: old-model\nproviders:\n" +
	"  provider-a:\n    base_url: https://api-a.example.com/v1\n" +
	"  provider-b:\n    base_url: https://api-b.example.com/v1\n    key_env: PROVIDER_B_KEY\n"

// Runs a switch against a real config with the given direct aliases and offline validation
func switchSharedModel(t *testing.T, configYAML string, opts SwitchModelOptions, aliases ...NamedDirectAlias) ModelSwitchResult {
	t.Helper()
	ctx, h := switchAliasHome(t, configYAML)
	fakeSwitchPipeline(t, true)
	seedDirectAliases(aliases...)
	cfg := readConfigYAMLMap(t, h)
	opts.RawInput = "shared-model"
	opts.UserProviders, _ = cfg["providers"].(map[string]any)
	opts.CustomProviders, _ = cfg["custom_providers"].([]any)
	return mustSwitch(t, ctx, opts)
}

func explicitSwitchToProviderB(t *testing.T, explicit, extraCfg string, aliases ...NamedDirectAlias) ModelSwitchResult {
	t.Helper()
	t.Setenv("PROVIDER_B_KEY", "sk-provider-b")
	return switchSharedModel(t, providerABConfig+extraCfg, SwitchModelOptions{CurrentProvider: "provider-a",
		CurrentBaseURL: "https://api-a.example.com/v1", CurrentAPIKey: "sk-provider-a", ExplicitProvider: explicit}, aliases...)
}

func TestExplicitProviderAliasOwnership(t *testing.T) {
	aAlias := NamedDirectAlias{"a-alias", DirectAlias{Model: "shared-model", Provider: "custom", BaseURL: "https://alias-host.example.com/v1", APIKey: "sk-alias-host"}}
	t.Run("never adopts alias bound to another provider", func(t *testing.T) {
		res := explicitSwitchToProviderB(t, "provider-b", "", aAlias)
		if !res.Success || res.TargetProvider != "provider-b" || res.BaseURL != "https://api-b.example.com/v1" || res.APIKey != "sk-provider-b" || res.ResolvedViaAlias != "" {
			t.Fatalf("result %+v", res)
		}
	})
	t.Run("prefers its own alias for a shared model", func(t *testing.T) {
		res := explicitSwitchToProviderB(t, "provider-b", "", aAlias,
			NamedDirectAlias{"b-alias", DirectAlias{Model: "shared-model", Provider: "Provider-B", BaseURL: "https://api-b.example.com/v2"}})
		if !res.Success || res.ResolvedViaAlias != "b-alias" || res.BaseURL != "https://api-b.example.com/v2" || res.APIKey == "sk-alias-host" {
			t.Fatalf("result %+v", res)
		}
	})
	corpAlias := NamedDirectAlias{"corp-alias", DirectAlias{Model: "shared-model", Provider: "corp-llm", BaseURL: "https://corp.example.com/v2", APIKey: "sk-corp-alias"}}
	providerAAlias := NamedDirectAlias{"a-alias", DirectAlias{Model: "shared-model", Provider: "provider-a", BaseURL: "https://alias-host.example.com/v1", APIKey: "sk-alias-host"}}
	corpEntry := "custom_providers:\n  - name: corp-llm\n    base_url: https://corp.example.com/v1\n    api_key: sk-corp\n"
	t.Run("keeps alias owned by legacy custom provider", func(t *testing.T) {
		res := explicitSwitchToProviderB(t, "corp-llm", corpEntry, providerAAlias, corpAlias)
		if !res.Success || res.TargetProvider != "custom:corp-llm" || res.ResolvedViaAlias != "corp-alias" || res.BaseURL != "https://corp.example.com/v2" || res.APIKey != "sk-corp-alias" {
			t.Fatalf("result %+v", res)
		}
	})
	t.Run("implicit switch prefers alias of current legacy custom provider", func(t *testing.T) {
		cfg := "model:\n  provider: custom:corp-llm\n  default: old-model\nproviders:\n  provider-a:\n    base_url: https://api-a.example.com/v1\n" + corpEntry
		res := switchSharedModel(t, cfg, SwitchModelOptions{CurrentProvider: "custom:corp-llm",
			CurrentBaseURL: "https://corp.example.com/v1", CurrentAPIKey: "sk-corp"}, providerAAlias, corpAlias)
		if !res.Success || res.ResolvedViaAlias != "corp-alias" || res.BaseURL != "https://corp.example.com/v2" || res.APIKey != "sk-corp-alias" {
			t.Fatalf("result %+v", res)
		}
	})
}

func readConfigYAMLMap(t *testing.T, h string) map[string]any {
	t.Helper()
	return LoadConfigSafe(h)
}
