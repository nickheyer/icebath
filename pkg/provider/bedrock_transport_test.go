package provider

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AWS and icebath variables a Bedrock test must not inherit from the developer's shell
var bedrockTestEnvVars = []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_SECURITY_TOKEN",
	"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_BEARER_TOKEN_BEDROCK", "AWS_ENDPOINT_URL",
	"AWS_ENDPOINT_URL_BEDROCK_RUNTIME", "AWS_ENDPOINT_URL_BEDROCK", "AWS_IGNORE_CONFIGURED_ENDPOINT_URLS",
	"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN",
	"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_ROLE_SESSION_NAME",
	"ANTHROPIC_BEDROCK_BASE_URL", "AWS_RETRY_MODE", "AWS_MAX_ATTEMPTS", "AWS_USE_FIPS_ENDPOINT", "AWS_USE_DUALSTACK_ENDPOINT",
	"AWS_CREDENTIAL_EXPIRATION", "AWS_EC2_METADATA_SERVICE_ENDPOINT", "AWS_EC2_METADATA_SERVICE_ENDPOINT_MODE"}

// Isolated home, AWS files and caches, returning the icebath home directory
func bedrockIsolatedEnv(t *testing.T) string {
	dir := t.TempDir()
	for _, k := range bedrockTestEnvVars {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	h := filepath.Join(dir, "icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("HOME", dir)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "aws-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "aws-credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	ResetBedrockClientCache()
	ResetBedrockDiscoveryCache()
	t.Cleanup(ResetBedrockClientCache)
	return h
}

// Writes the icebath config.yaml of the isolated home
func bedrockWriteConfig(t *testing.T, h, yaml string) {
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	ResetBedrockClientCache()
}

func TestBedrockTransportStreamUnavailableShapes(t *testing.T) {
	tr, _ := bedrockTransport(Route{})
	he := &ccHTTPError{Status: 403, Body: `{"message":"not authorized to perform: bedrock:InvokeModelWithResponseStream"}`,
		Headers: map[string][]string{"X-Amzn-Errortype": {"AccessDeniedException:http://internal.amazon.com/coral/com.amazon.bedrock/"}}}
	if !tr.StreamUnavailable(he) {
		t.Fatal("access denied stream shape not detected")
	}
	he.Headers = map[string][]string{"X-Amzn-Errortype": {"ThrottlingException"}}
	if tr.StreamUnavailable(he) || tr.StreamUnavailable(nil) || !tr.StreamUnavailable(errors.New("streaming is not supported here")) {
		t.Fatal("stream unavailable classification")
	}
}

func TestBedrockTransportBetasCarryContext1M(t *testing.T) {
	tr, _ := bedrockTransport(Route{})
	got := strings.Join(tr.Betas(false), ",")
	for _, want := range []string{"context-1m-2025-08-07", "interleaved-thinking-2025-05-14", "fine-grained-tool-streaming-2025-05-14"} {
		if !strings.Contains(got, want) {
			t.Fatalf("betas %s lack %s", got, want)
		}
	}
	if strings.Contains(strings.Join(tr.Betas(true), ","), "context-1m") {
		t.Fatal("dropped 1M beta still sent")
	}
	for _, u := range []string{"https://api.minimax.io/anthropic", "https://api.minimaxi.com/anthropic"} {
		b := strings.Join(anthCommonBetasForBaseURL(u, false), ",")
		if strings.Contains(b, "context-1m") || !strings.Contains(b, "interleaved-thinking-2025-05-14") {
			t.Fatalf("minimax betas %s", b)
		}
	}
}

func TestAnthropicResponseGuardrailIntervenedReadsTheActionField(t *testing.T) {
	if !AnthropicResponseGuardrailIntervened(map[string]any{BedrockGuardrailActionField: "intervened"}) {
		t.Fatal("an INTERVENED action must read as a guardrail substitution")
	}
	if AnthropicResponseGuardrailIntervened(map[string]any{BedrockGuardrailActionField: "NONE"}) || AnthropicResponseGuardrailIntervened(nil) {
		t.Fatal("a reply without the INTERVENED action is ordinary model output")
	}
}
