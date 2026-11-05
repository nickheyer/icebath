package provider

import (
	"context"
	"testing"
)

const (
	bedrockTestMantle    = "https://bedrock-mantle.us-east-1.api.aws/openai/v1"
	bedrockTestRuntimeEU = "https://bedrock-runtime.eu-west-1.amazonaws.com"
)

// Static keys in the environment and no Bedrock API key, the aws_env fixture
func bedrockAWSEnvFixture(t *testing.T) {
	bedrockIsolatedEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAFAKEFAKEFAKEFAKE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake-secret")
}

func TestBedrockSwitchToMantleInstallsSigV4Transport(t *testing.T) {
	bedrockAWSEnvFixture(t)
	route, err := ConfigureBedrockOpenAIRoute(context.Background(), Route{Provider: "bedrock", Model: "openai.gpt-5.6-terra", APIKey: "aws-sdk",
		BaseURL: bedrockTestMantle, Mode: ModeCodexResponses}, 0)
	if err != nil {
		t.Fatal(err)
	}
	rt, ok := route.Transport.(*sigV4RoundTripper)
	if !ok {
		t.Fatalf("switched route would send Bearer aws-sdk (transport=%T)", route.Transport)
	}
	if rt.region != "us-east-1" {
		t.Fatalf("mantle region %q", rt.region)
	}
}

func TestBedrockSwitchToRuntimeBindsRegionAndWire(t *testing.T) {
	for _, c := range []struct {
		model  string
		mode   APIMode
		claude bool
	}{{"us.anthropic.claude-opus-4-6-v1", ModeAnthropicMessages, true}, {"us.amazon.nova-pro-v1:0", modeBedrockConverse, false}} {
		bedrockAWSEnvFixture(t)
		b, err := BindBedrockRuntime(context.Background(), bedrockTestRuntimeEU, c.mode, c.model)
		if err != nil {
			t.Fatal(err)
		}
		if b.Region != "eu-west-1" || b.APIKey != "aws-sdk" || b.BaseURL != bedrockTestRuntimeEU {
			t.Fatalf("%s binding %#v", c.model, b)
		}
		if !c.claude {
			if b.Client != nil {
				t.Fatalf("converse wire got a Messages client %T", b.Client)
			}
			continue
		}
		ac, ok := b.Client.(*anthropicClient)
		if !ok || ac.variant != "bedrock" {
			t.Fatalf("claude wire client %T", b.Client)
		}
		if got := ac.transport.(*anthBedrockTransport).region(context.Background()); got != "eu-west-1" {
			t.Fatalf("transport region %q", got)
		}
	}
}

func TestBedrockFallbackBindsRuntimeNotGenericClient(t *testing.T) {
	for _, mode := range []APIMode{ModeAnthropicMessages, modeBedrockConverse} {
		bedrockAWSEnvFixture(t)
		b, err := BindBedrockRuntime(context.Background(), bedrockTestRuntimeEU, mode, "us.amazon.nova-pro-v1:0")
		if err != nil {
			t.Fatal(err)
		}
		if b.Region != "eu-west-1" || (mode == ModeAnthropicMessages) != (b.Client != nil) {
			t.Fatalf("%s fallback binding %#v", mode, b)
		}
	}
}
