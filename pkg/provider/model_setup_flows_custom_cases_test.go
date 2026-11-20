package provider

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

func TestBlankContextLengthReportsDetectionOutcome(t *testing.T) {
	for _, tc := range []struct {
		resolved int
		expect   string
	}{{200_000, "auto-detected"}, {0, "using the default"}} {
		t.Run(tc.expect, func(t *testing.T) {
			h := newTestHome(t)
			r, out := newFlowRun(t, h)
			resolved := tc.resolved
			if resolved == 0 {
				resolved = DefaultFallbackContext
			}
			swapFlowSeam(t, &customContextLengthFn, func(context.Context, ContextLengthRequest) (int, error) { return resolved, nil })
			reportContextLengthDetection(r, "some-model", "http://localhost:8000/v1", "k")
			if !strings.Contains(out.String(), tc.expect) {
				t.Fatalf("output %q", out.String())
			}
		})
	}
}

func TestProbeFailureNeverBlocksTheSave(t *testing.T) {
	h := newTestHome(t)
	r, out := newFlowRun(t, h)
	swapFlowSeam(t, &customContextLengthFn, func(context.Context, ContextLengthRequest) (int, error) { return 0, errors.New("boom") })
	reportContextLengthDetection(r, "some-model", "http://x/v1", "")
	if out.String() != "" {
		t.Fatalf("output %q", out.String())
	}
}

// Gateway shape, multiplex on with this profile's .env installed as the secret scope and a different process env value
func scopedProfileHome(t *testing.T) (string, context.Context) {
	t.Helper()
	h := newTestHome(t)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("model: old-model\ncustom_providers: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("EXAMPLE_PROVIDER_API_KEY=sk-from-profile-dotenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXAMPLE_PROVIDER_API_KEY", "sk-other-profile-process-env")
	config.ResetCache()
	SetMultiplexActive(true)
	t.Cleanup(func() { SetMultiplexActive(false) })
	ctx := home.WithOverride(context.Background(), h)
	ctx = WithSecretScope(ctx, BuildProfileSecretScope(h), h)
	return h, ctx
}

func TestNamedCustomFlowProbesWithScopedKeyEnv(t *testing.T) {
	h, ctx := scopedProfileHome(t)
	env := testEnv(t, h)
	env.Ctx = ctx
	r := &ModelFlowRun{Ctx: ctx, Env: env, Out: io.Discard, Config: map[string]any{}}
	providerInfo := map[string]any{"name": "Example Provider", "base_url": "https://api.example-provider.test/v1", "api_key": "",
		"key_env": "EXAMPLE_PROVIDER_API_KEY", "model": "qwen3.6-35b-fast"}
	var probedKeys []string
	swapFlowSeam(t, &flowFetchAPIModelsFn, func(_ context.Context, apiKey, _ string, _ time.Duration, _ string, _ map[string]string) ([]string, bool) {
		probedKeys = append(probedKeys, apiKey)
		return []string{"qwen3.6-35b-fast"}, true
	})
	swapFlowSeam(t, &flowRadiolistFn, func(string, [][]PickerSegment, PickerRadiolistOptions) (int, error) {
		return 0, errors.New("ImportError")
	})
	swapFlowSeam(t, &flowRawInputFn, func(io.Writer, string) (string, error) { return "1", nil })
	if err := modelFlowNamedCustom(r, providerInfo); err != nil {
		t.Fatal(err)
	}
	if len(probedKeys) == 0 || probedKeys[0] != "sk-from-profile-dotenv" {
		t.Fatalf("probed keys %q", probedKeys)
	}
}
