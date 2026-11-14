package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Isolated home and AWS environment holding the example key pair and the given region
func bedrockAuxEnv(t *testing.T, region string) context.Context {
	t.Helper()
	h := bedrockIsolatedEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	if region != "" {
		t.Setenv("AWS_REGION", region)
	}
	return home.WithOverride(context.Background(), h)
}

func TestModelCatalogBedrockHasCuratedModels(t *testing.T) {
	if len(staticCatalogModels("bedrock")) == 0 {
		t.Fatal("bedrock needs a static model fallback list")
	}
}

func TestResolveProviderExplicitBedrockResolves(t *testing.T) {
	ctx := bedrockAuxEnv(t, "")
	if got, err := resolveProvider(PoolEnv{Ctx: ctx}, "bedrock"); err != nil || got != "bedrock" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestResolveProviderAwsAliasResolvesToBedrock(t *testing.T) {
	ctx := bedrockAuxEnv(t, "")
	if got, err := resolveProvider(PoolEnv{Ctx: ctx}, "aws"); err != nil || got != "bedrock" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestResolveProviderAutoDetectWithAwsCredentials(t *testing.T) {
	ctx := bedrockAuxEnv(t, "")
	for _, name := range []string{"OPENAI_API_KEY", "OPENROUTER_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "GOOGLE_API_KEY", "DEEPSEEK_API_KEY"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	if got, err := resolveProvider(PoolEnv{Ctx: ctx}, "auto"); err != nil || got != "bedrock" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestRuntimeProviderBedrockRuntimeNoCredentialsRaisesOnAutoDetect(t *testing.T) {
	bedrockIsolatedEnv(t)
	r := testResolver(t, map[string]any{"model": map[string]any{"provider": "bedrock"}}, &fakeCreds{}, "")
	_, err := r.resolveBedrockRuntime("auto", map[string]any{"provider": "bedrock"}, "")
	var ae *AuthError
	if !errors.As(err, &ae) || !strings.Contains(err.Error(), "No AWS credentials") {
		t.Fatalf("err %v", err)
	}
}

func TestRuntimeProviderBedrockRuntimeExplicitSkipsCredentialCheck(t *testing.T) {
	bedrockIsolatedEnv(t)
	r := testResolver(t, map[string]any{"model": map[string]any{"provider": "bedrock"}}, &fakeCreds{}, "")
	rt, err := r.resolveBedrockRuntime("bedrock", map[string]any{"provider": "bedrock"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if rt.Provider != "bedrock" || rt.APIMode != modeBedrockConverse {
		t.Fatalf("runtime %+v", rt)
	}
}

func TestRuntimeProviderBedrockOpenaiModelsRouteToMantleResponses(t *testing.T) {
	bedrockAuxEnv(t, "us-east-2")
	if len(BedrockOpenAIResponsesModelIDs) == 0 {
		t.Fatal("the Mantle allowlist is empty")
	}
	for _, modelID := range BedrockOpenAIResponsesModelIDs {
		r := testResolver(t, map[string]any{"model": map[string]any{"provider": "bedrock", "default": modelID}}, &fakeCreds{}, "")
		rt, err := r.resolveBedrockRuntime("bedrock", map[string]any{"provider": "bedrock", "default": modelID}, "")
		if err != nil {
			t.Fatal(err)
		}
		if rt.APIMode != ModeCodexResponses || rt.Model != modelID || rt.BaseURL != "https://bedrock-mantle.us-east-2.api.aws/openai/v1" || rt.APIKey != "aws-sdk" || !rt.BedrockOpenAI {
			t.Fatalf("%s runtime %+v", modelID, rt)
		}
	}
}

func TestRuntimeProviderBedrockOpenaiProfileIdsRouteToRuntimeConverse(t *testing.T) {
	bedrockAuxEnv(t, "us-east-2")
	for _, modelID := range BedrockOpenAIResponsesModelIDs {
		for _, profileID := range []string{"us." + modelID, "global." + modelID} {
			r := testResolver(t, map[string]any{"model": map[string]any{"provider": "bedrock", "default": profileID}}, &fakeCreds{}, "")
			rt, err := r.resolveBedrockRuntime("bedrock", map[string]any{"provider": "bedrock", "default": profileID}, "")
			if err != nil {
				t.Fatal(err)
			}
			if rt.APIMode != modeBedrockConverse || rt.BaseURL != "https://bedrock-runtime.us-east-2.amazonaws.com" || rt.BedrockOpenAI {
				t.Fatalf("%s runtime %+v", profileID, rt)
			}
		}
	}
}

func TestRuntimeProviderBedrockOpenaiModelsDoNotFallBackToDefaultContext(t *testing.T) {
	ctx := bedrockAuxEnv(t, "us-east-2")
	for _, modelID := range BedrockOpenAIResponsesModelIDs {
		if got := BedrockContextLength(ctx, modelID, "us-east-2", false); got <= BedrockDefaultContextLength {
			t.Fatalf("%s context %d", modelID, got)
		}
	}
}

func TestProvidersModuleDetermineApiModeFromBedrockUrl(t *testing.T) {
	ctx := bedrockAuxEnv(t, "")
	if got := switchDetermineAPIMode(ctx, "unknown", "https://bedrock-runtime.us-east-1.amazonaws.com", ""); got != "bedrock_converse" {
		t.Fatalf("api mode %q", got)
	}
}

func TestErrorClassifierBedrockBedrockThrottlingClassifiedAsRateLimit(t *testing.T) {
	err := errors.New("An error occurred (ThrottlingException) when calling the ConverseStream operation: Too many requests, please wait before trying again.")
	if got := ClassifyError(ClassifyInput{Err: err, Provider: "bedrock"}).Reason; got != ReasonRateLimit {
		t.Fatalf("reason %q", got)
	}
}

func TestErrorClassifierBedrockBedrockInputTooLongClassifiedAsContextOverflow(t *testing.T) {
	err := errors.New("An error occurred (ValidationException) when calling the ConverseStream operation: Input is too long for requested model.")
	if got := ClassifyError(ClassifyInput{Err: err, Provider: "bedrock"}).Reason; got != ReasonContextOverflow {
		t.Fatalf("reason %q", got)
	}
}

func TestBedrockModelNameNormalizationBedrockPrefixPreservedWithoutPreserveDots(t *testing.T) {
	if got := normalizeModelName("global.anthropic.claude-opus-4-7", false); got != "global.anthropic.claude-opus-4-7" {
		t.Fatalf("got %q", got)
	}
}

func TestBedrockBuildAnthropicKwargsEndToEndBedrockModelPreservedWithoutPreserveDots(t *testing.T) {
	kwargs, err := anthBuildKwargs(anthBuildInput{model: "global.anthropic.claude-opus-4-7",
		messages: []map[string]any{{"role": "user", "content": "hi"}}, maxTokens: 1024, preserveDots: false})
	if err != nil {
		t.Fatal(err)
	}
	if kwargs["model"] != "global.anthropic.claude-opus-4-7" {
		t.Fatalf("model %v", kwargs["model"])
	}
}

func TestBedrockModelIdDetectionBareBedrockIdPreservedWithoutFlag(t *testing.T) {
	if got := normalizeModelName("anthropic.claude-opus-4-7", false); got != "anthropic.claude-opus-4-7" {
		t.Fatalf("got %q", got)
	}
}

func TestAuxiliaryClientBedrockResolutionBedrockReturnsClientWithCredentials(t *testing.T) {
	ctx := bedrockAuxEnv(t, "us-west-2")
	client, model, err := ResolveProviderClient(ctx, "bedrock", "", ProviderClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil {
		t.Fatal("resolve_provider_client('bedrock') returned None — aws_sdk auth type is not handled")
	}
	if _, ok := client.(*AnthropicAuxiliaryClient); !ok {
		t.Fatalf("client %T", client)
	}
	if model == "" || client.APIKey() != "aws-sdk" || !strings.Contains(client.BaseURL(), "us-west-2") {
		t.Fatalf("model %q key %q base %q", model, client.APIKey(), client.BaseURL())
	}
}

func TestAuxiliaryClientBedrockResolutionBedrockReturnsNoneWithoutCredentials(t *testing.T) {
	ctx := bedrockAuxEnv(t, "")
	prev := auxHasAWSCredentialsFn
	auxHasAWSCredentialsFn = func(bool) bool { return false }
	t.Cleanup(func() { auxHasAWSCredentialsFn = prev })
	client, model, err := ResolveProviderClient(ctx, "bedrock", "", ProviderClientOptions{})
	if err != nil || client != nil || model != "" {
		t.Fatalf("got %v %q %v", client, model, err)
	}
}

func TestAuxiliaryClientBedrockResolutionBedrockShimUncappedWhenCallerOmitsMaxTokens(t *testing.T) {
	ctx := bedrockAuxEnv(t, "")
	client := NewBedrockAuxiliaryClient("us-east-1", "openai.gpt-oss-20b-1:0")
	var captured bedrockConverseInput
	prev := callBedrockConverseFn
	callBedrockConverseFn = func(_ context.Context, _ string, in bedrockConverseInput) (*bedrockConverseResult, error) {
		captured = in
		return &bedrockConverseResult{Content: "", FinishReason: "stop", Model: in.Model}, nil
	}
	t.Cleanup(func() { callBedrockConverseFn = prev })
	if _, err := client.CreateChatCompletion(ctx, map[string]any{"model": "openai.gpt-oss-20b-1:0",
		"messages": []any{map[string]any{"role": "user", "content": "describe"}}, "temperature": 0.1}); err != nil {
		t.Fatal(err)
	}
	inference, _ := buildBedrockConverseKwargs(captured)["inferenceConfig"].(map[string]any)
	if _, has := inference["maxTokens"]; has {
		t.Fatalf("inferenceConfig %v", inference)
	}
	if _, err := client.CreateChatCompletion(ctx, map[string]any{"model": "openai.gpt-oss-20b-1:0",
		"messages": []any{map[string]any{"role": "user", "content": "describe"}}, "max_tokens": 1234}); err != nil {
		t.Fatal(err)
	}
	inference, _ = buildBedrockConverseKwargs(captured)["inferenceConfig"].(map[string]any)
	if inference["maxTokens"] != 1234 {
		t.Fatalf("inferenceConfig %v", inference)
	}
}

func TestAuxiliaryClientBedrockResolutionBedrockMantleConfigRegionBeatsEnvRegion(t *testing.T) {
	ctx := bedrockAuxEnv(t, "eu-central-1")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "test-bearer")
	if err := os.WriteFile(filepath.Join(home.Dir(ctx), "config.yaml"), []byte("bedrock:\n  region: us-west-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	client, model, err := ResolveProviderClient(ctx, "bedrock", "openai.gpt-5.6-sol", ProviderClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || model != "openai.gpt-5.6-sol" {
		t.Fatalf("client %v model %q", client, model)
	}
	if !strings.Contains(client.BaseURL(), "us-west-2") {
		t.Fatalf("Mantle auxiliary base_url ignored config.yaml bedrock.region: %q", client.BaseURL())
	}
}

func TestAuxiliaryClientBedrockResolutionBedrockOpenaiAuxUsesResponsesClient(t *testing.T) {
	ctx := bedrockAuxEnv(t, "us-east-2")
	client, model, err := ResolveProviderClient(ctx, "bedrock", "openai.gpt-5.5", ProviderClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if model != "openai.gpt-5.5" {
		t.Fatalf("model %q", model)
	}
	codex, ok := client.(*CodexAuxiliaryClient)
	if !ok {
		t.Fatalf("client %T", client)
	}
	real, ok := codex.realClient.(*auxOpenAIClient)
	if !ok {
		t.Fatalf("real client %T", codex.realClient)
	}
	if real.APIKey() != "aws-sdk" || strings.TrimRight(real.BaseURL(), "/") != "https://bedrock-mantle.us-east-2.api.aws/openai/v1" || real.httpClient == nil {
		t.Fatalf("key %q base %q http client %v", real.APIKey(), real.BaseURL(), real.httpClient)
	}
}

func TestAuxiliaryClientBedrockResolutionBedrockOpenaiProfileAuxUsesRuntimeConverse(t *testing.T) {
	for _, profileID := range []string{"us.openai.gpt-6.1-sol", "global.openai.gpt-6-astra", "us.openai.gpt-5.6-terra"} {
		t.Run(profileID, func(t *testing.T) {
			ctx := bedrockAuxEnv(t, "us-east-2")
			client, model, err := ResolveProviderClient(ctx, "bedrock", profileID, ProviderClientOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if model != profileID {
				t.Fatalf("model %q", model)
			}
			if _, ok := client.(*BedrockAuxiliaryClient); !ok {
				t.Fatalf("client %T", client)
			}
			if client.BaseURL() != "https://bedrock-runtime.us-east-2.amazonaws.com" {
				t.Fatalf("base %q", client.BaseURL())
			}
		})
	}
}
