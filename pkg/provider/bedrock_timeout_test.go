package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

const (
	bedrockTimeoutClaude = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	bedrockTimeoutNova   = "us.amazon.nova-pro-v1:0"
	bedrockTimeoutLlama  = "us.meta.llama4-maverick-17b-instruct-v1:0"
	bedrockOverridesYAML = "providers:\n  bedrock:\n    request_timeout_seconds: 600\n    models:\n      " + bedrockTimeoutClaude +
		":\n        timeout_seconds: 300\n      " + bedrockTimeoutLlama + ":\n        timeout_seconds: 120\n"
)

func bedrockTimeoutClient(t *testing.T, ctx context.Context, model string) *awsServiceClient {
	c, err := bedrockRuntimeClient(ctx, "us-east-1", model)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Response header timeout of the client's transport, the botocore connect bound fixed at 60 seconds
func bedrockClientTimeouts(c *awsServiceClient) (time.Duration, time.Duration) {
	return c.readTimeout, awsDefaultConnectTimeout
}

func TestBedrockUnsetConfigBuildsTheBotocoreDefaultClient(t *testing.T) {
	bedrockIsolatedEnv(t)
	read, connect := bedrockClientTimeouts(bedrockTimeoutClient(t, context.Background(), bedrockTimeoutClaude))
	if read != 60*time.Second || connect != 60*time.Second {
		t.Fatalf("default timeouts %v %v", read, connect)
	}
}

func TestBedrockConfiguredTimeoutIsTheReadTimeout(t *testing.T) {
	for model, want := range map[string]time.Duration{bedrockTimeoutNova: 600 * time.Second, bedrockTimeoutClaude: 300 * time.Second} {
		h := bedrockIsolatedEnv(t)
		bedrockWriteConfig(t, h, bedrockOverridesYAML)
		read, connect := bedrockClientTimeouts(bedrockTimeoutClient(t, context.Background(), model))
		if read != want || connect != 60*time.Second {
			t.Fatalf("%s timeouts %v %v", model, read, connect)
		}
	}
}

func TestBedrockInvalidTimeoutValuesBehaveLikeUnset(t *testing.T) {
	for _, pair := range [][2]string{{"soon", "-5"}, {"0", "later"}, {`""`, "null"}} {
		h := bedrockIsolatedEnv(t)
		bedrockWriteConfig(t, h, "providers:\n  bedrock:\n    request_timeout_seconds: "+pair[0]+"\n    models:\n      "+bedrockTimeoutClaude+":\n        timeout_seconds: "+pair[1]+"\n")
		c := bedrockTimeoutClient(t, context.Background(), bedrockTimeoutClaude)
		if c.readTimeout != 60*time.Second || c != bedrockTimeoutClient(t, context.Background(), "") {
			t.Fatalf("%v: invalid values must share the unset client (read %v)", pair, c.readTimeout)
		}
	}
}

func TestBedrockModelsWithDifferentOverridesNeverShareAClient(t *testing.T) {
	h := bedrockIsolatedEnv(t)
	bedrockWriteConfig(t, h, bedrockOverridesYAML)
	ctx := context.Background()
	claude, nova, llama := bedrockTimeoutClient(t, ctx, bedrockTimeoutClaude), bedrockTimeoutClient(t, ctx, bedrockTimeoutNova), bedrockTimeoutClient(t, ctx, bedrockTimeoutLlama)
	if claude.readTimeout != 300*time.Second || nova.readTimeout != 600*time.Second || llama.readTimeout != 120*time.Second {
		t.Fatalf("timeouts %v %v %v", claude.readTimeout, nova.readTimeout, llama.readTimeout)
	}
	if claude == nova || nova == llama || claude == llama {
		t.Fatal("distinct timeouts shared a client")
	}
	if bedrockTimeoutClient(t, ctx, bedrockTimeoutClaude) != claude || bedrockTimeoutClient(t, ctx, "us.amazon.nova-lite-v1:0") != nova {
		t.Fatal("same region and timeout must reuse one client")
	}
	if err := os.Remove(filepath.Join(h, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	unset := bedrockTimeoutClient(t, ctx, bedrockTimeoutClaude)
	if unset == claude || unset == nova || unset == llama || unset.readTimeout != 60*time.Second {
		t.Fatalf("unset collided with a configured slot (read %v)", unset.readTimeout)
	}
}

func TestBedrockProfileScopedClientsCarryTheirOwnTimeouts(t *testing.T) {
	bedrockIsolatedEnv(t)
	profile := filepath.Join(t.TempDir(), "profile-home")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	bedrockWriteConfig(t, profile, "providers:\n  bedrock:\n    request_timeout_seconds: 600\n    models:\n      "+bedrockTimeoutClaude+":\n        timeout_seconds: 300\n")
	pctx := WithMultiplexContext(WithSecretScope(home.WithOverride(context.Background(), profile),
		map[string]string{"AWS_ACCESS_KEY_ID": "AKIA-PROFILE", "AWS_SECRET_ACCESS_KEY": "profile-secret"}, profile), true)
	claude, nova, again := bedrockTimeoutClient(t, pctx, bedrockTimeoutClaude), bedrockTimeoutClient(t, pctx, bedrockTimeoutNova), bedrockTimeoutClient(t, pctx, bedrockTimeoutClaude)
	if claude.readTimeout != 300*time.Second || nova.readTimeout != 600*time.Second || claude != again || claude == nova {
		t.Fatalf("profile clients %v %v", claude.readTimeout, nova.readTimeout)
	}
	creds, err := claude.creds.Retrieve(pctx)
	if err != nil || creds.AccessKeyID != "AKIA-PROFILE" {
		t.Fatalf("profile signer creds %#v %v", creds, err)
	}
	ambient := bedrockTimeoutClient(t, context.Background(), bedrockTimeoutClaude)
	if ambient == claude || ambient.readTimeout != 60*time.Second {
		t.Fatalf("launch context client %v", ambient.readTimeout)
	}
	if !InvalidateBedrockRuntimeClient(pctx, "us-east-1") {
		t.Fatal("profile eviction reported nothing evicted")
	}
	if bedrockTimeoutClient(t, pctx, bedrockTimeoutClaude) == claude || bedrockTimeoutClient(t, context.Background(), bedrockTimeoutClaude) != ambient {
		t.Fatal("eviction must drop the profile's slots and keep the launch context's")
	}
}

func TestBedrockClaudeBindingTakesTheModelsTimeout(t *testing.T) {
	h := bedrockIsolatedEnv(t)
	bind := func(model string) time.Duration {
		b, err := BindBedrockRuntime(context.Background(), "https://bedrock-runtime.us-east-1.amazonaws.com", ModeAnthropicMessages, model)
		if err != nil {
			t.Fatal(err)
		}
		return b.Client.Route().Timeout
	}
	if got := bind(bedrockTimeoutClaude); got != 0 {
		t.Fatalf("unset config must keep the client default, got %v", got)
	}
	bedrockWriteConfig(t, h, bedrockOverridesYAML)
	if a, b := bind(bedrockTimeoutClaude), bind("us.anthropic.claude-haiku-4-5-20251001-v1:0"); a != 300*time.Second || b != 600*time.Second {
		t.Fatalf("binding timeouts %v %v", a, b)
	}
}
