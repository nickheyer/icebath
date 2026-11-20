package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const moaPresetHomeConfig = `moa:
  default_preset: default
  presets:
    default:
      enabled: true
      reference_models:
      - provider: xai
        model: grok-4-fast
      aggregator:
        provider: xai
        model: grok-4.6
`

// Temp home whose config.yaml holds a real MoA preset named default
func writeMoaHome(t *testing.T) *tools.Env {
	t.Helper()
	env := promptTestEnv(t)
	if err := os.WriteFile(filepath.Join(env.Home, "config.yaml"), []byte(moaPresetHomeConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return env
}

// Fails unless the agent wears the MoA preset identity over the in process facade
func assertBoundToMoaPreset(t *testing.T, a *AIAgent, preset string) {
	t.Helper()
	if got := [3]string{a.provider, a.requestedProvider, a.model}; got != [3]string{"moa", "moa", preset} {
		t.Fatalf("identity %q", got)
	}
	if a.baseURL != "moa://local" || a.apiMode != "chat_completions" {
		t.Fatalf("route %q %q", a.baseURL, a.apiMode)
	}
	if !reflect.DeepEqual(a.clientKwargs, map[string]any{}) {
		t.Fatalf("client kwargs %#v", a.clientKwargs)
	}
	if !moaClientConsumesPreparedRequest(a.client) {
		t.Fatalf("client %T is not the facade", a.client)
	}
}

func TestMoaPresetFallback(t *testing.T) {
	caseHome(t)
	aggregator := isolationClient{apiKey: "xai-key", baseURL: "https://api.x.ai/v1/"}
	t.Run("test_runtime_fallback_to_moa_preset_binds_the_facade", func(t *testing.T) {
		env := writeMoaHome(t)
		a := newFixtureAgent(t, AIAgentOptions{BaseURL: "https://openrouter.ai/api/v1", Env: env, EnabledToolsets: []string{},
			FallbackModel: []map[string]any{{"provider": "moa", "model": "default"}}})
		t.Cleanup(func() { a.Close() })
		recordIsolationResolves(t, aggregator, "grok-4.6")
		if !a.tryActivateFallback(tools.WithToolEnv(context.Background(), env), "") {
			t.Fatal("fallback did not activate")
		}
		assertBoundToMoaPreset(t, a, "default")
		if c, ok := a.client.(isolationClient); ok && c == aggregator {
			t.Fatal("agent bound the aggregator client")
		}
		if a.providerFallbackRoute != [2]string{"default", "moa"} {
			t.Fatalf("route %q", a.providerFallbackRoute)
		}
	})
	t.Run("test_init_time_fallback_to_moa_preset_binds_the_facade", func(t *testing.T) {
		env := writeMoaHome(t)
		installInitLadder(t, func(name, _ string) (provider.AuxClient, string, error) {
			if name == "moa" {
				return aggregator, "grok-4.6", nil
			}
			return nil, "", nil
		}, nil)
		a, err := NewAIAgent(tools.WithToolEnv(context.Background(), env), AIAgentOptions{Model: "anthropic/claude-sonnet-4.5", Provider: "openrouter",
			QuietMode: true, SkipContextFiles: true, SkipMemory: true, Env: env, EnabledToolsets: []string{},
			FallbackModel: []map[string]any{{"provider": "moa", "model": "default"}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { a.Close() })
		assertBoundToMoaPreset(t, a, "default")
		if !a.fallbackActivated {
			t.Fatal("fallback not marked activated")
		}
	})
}
