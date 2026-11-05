package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
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

const bedrockTestClaude = "us.anthropic.claude-opus-4-6-v1"

// Messages client on the Bedrock wire the way BindBedrockRuntime builds it
func bedrockTestClient(t *testing.T, mutate func(*Route)) *anthropicClient {
	route := Route{Provider: "bedrock", Model: bedrockTestClaude, BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com",
		APIKey: "aws-sdk", Mode: ModeAnthropicMessages, Auth: AuthAWSSigV4, Region: "us-east-1"}
	if mutate != nil {
		mutate(&route)
	}
	c, err := newAnthropicClient(route)
	if err != nil {
		t.Fatal(err)
	}
	ac := c.(*anthropicClient)
	if ac.variant != "bedrock" {
		t.Fatalf("variant %q", ac.variant)
	}
	return ac
}

func bedrockTestRequest(model string) *Request {
	return &Request{Model: model, Messages: []*state.Message{{Role: "user", Content: "hi"}}, MaxTokens: 64}
}

// Decoded request body of a record, the inner InvokeModel body for CountTokens
func bedrockRecordBody(t *testing.T, rec *bedrockFakeRecord) map[string]any {
	var body map[string]any
	if err := json.Unmarshal(rec.Raw, &body); err != nil {
		t.Fatal(err)
	}
	if rec.Op != "CountTokens" {
		return body
	}
	data, err := base64.StdEncoding.DecodeString(body["input"].(map[string]any)["invokeModel"].(map[string]any)["body"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var inner map[string]any
	if err := json.Unmarshal(data, &inner); err != nil {
		t.Fatal(err)
	}
	return inner
}

func bedrockBetas(body map[string]any) map[string]bool {
	out := map[string]bool{}
	list, _ := body["anthropic_beta"].([]any)
	for _, b := range list {
		out[stringOf(b)] = true
	}
	return out
}

func TestBedrockInvokeSignedShapedAndNormalized(t *testing.T) {
	bedrockIsolatedEnv(t)
	fake := newBedrockFake(t, bedrockSeq(newBedrockTurn(bedrockTextBlock("hello from bedrock"))))
	fake.setEnv(t)
	resp, err := bedrockTestClient(t, nil).Complete(context.Background(), bedrockTestRequest(bedrockTestClaude))
	if err != nil {
		t.Fatal(err)
	}
	if got := stringOf(resp.Message.Content); !strings.Contains(got, "hello from bedrock") || resp.FinishReason != "stop" {
		t.Fatalf("response %q %q", got, resp.FinishReason)
	}
	recs := fake.snapshot()
	if len(recs) != 1 || recs[0].Op != "InvokeModel" || recs[0].Rejected != "" || recs[0].Model != bedrockTestClaude {
		t.Fatalf("records %#v", recs[0])
	}
	if recs[0].Auth["region"] != "us-east-1" || recs[0].Auth["key"] != bedrockFakeAccessKey {
		t.Fatalf("auth scope %#v", recs[0].Auth)
	}
	body := bedrockRecordBody(t, recs[0])
	if body["anthropic_version"] != "bedrock-2023-05-31" || body["model"] != nil || body["stream"] != nil {
		t.Fatalf("invoke body %#v", body)
	}
	betas := bedrockBetas(body)
	for _, want := range []string{"context-1m-2025-08-07", "interleaved-thinking-2025-05-14", "fine-grained-tool-streaming-2025-05-14"} {
		if !betas[want] {
			t.Fatalf("beta %s missing from %v", want, betas)
		}
	}
	if _, has := recs[0].Headers["anthropic-version"]; has {
		t.Fatal("anthropic-version header reached Bedrock")
	}
}

func TestBedrockStreamDecodesInvokeEventStream(t *testing.T) {
	bedrockIsolatedEnv(t)
	turn := newBedrockTurn(bedrockTextBlock("streamed over the aws event stream"), &bedrockToolUse{Name: "read_file", Input: map[string]any{"path": "a.txt"}})
	fake := newBedrockFake(t, bedrockSeq(turn))
	fake.setEnv(t)
	var text strings.Builder
	resp, err := bedrockTestClient(t, nil).Stream(context.Background(), bedrockTestRequest(bedrockTestClaude), func(ev StreamEvent) error {
		if ev.Kind == EventTextDelta {
			text.WriteString(ev.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "streamed over the aws event stream" || resp.FinishReason != "tool_calls" || len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("stream result %q %q %#v", text.String(), resp.FinishReason, resp.Message.ToolCalls)
	}
	if resp.Message.ToolCalls[0].Function.Name != "read_file" || resp.Usage.OutputTokens != 30 {
		t.Fatalf("tool call %#v usage %#v", resp.Message.ToolCalls[0], resp.Usage)
	}
	if recs := fake.snapshot(); recs[0].Op != "InvokeModelWithResponseStream" || bedrockRecordBody(t, recs[0])["stream"] != nil {
		t.Fatalf("stream record %#v", recs[0])
	}
}

func TestBedrockStreamExceptionIsClassifiedByModelledStatus(t *testing.T) {
	bedrockIsolatedEnv(t)
	fake := newBedrockFake(t, bedrockSeq(newBedrockStreamException(newBedrockTurn(bedrockTextBlock("partial")), "throttlingException", "Too many requests, please wait before trying again.")))
	fake.setEnv(t)
	_, err := bedrockTestClient(t, nil).Stream(context.Background(), bedrockTestRequest(bedrockTestClaude), func(StreamEvent) error { return nil })
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 429 || !strings.Contains(err.Error(), "Too many requests") {
		t.Fatalf("throttle exception: %#v", err)
	}
}

func TestBedrockDroppedStreamFails(t *testing.T) {
	bedrockIsolatedEnv(t)
	fake := newBedrockFake(t, bedrockSeq(newBedrockDrop(newBedrockTurn(bedrockTextBlock("cut off mid stream")))))
	fake.setEnv(t)
	_, err := bedrockTestClient(t, nil).Stream(context.Background(), bedrockTestRequest(bedrockTestClaude), func(StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("dropped stream succeeded")
	}
}

func TestBedrockStreamingAccessDeniedLatchesNonStreaming(t *testing.T) {
	bedrockIsolatedEnv(t)
	denied := &bedrockHTTPError{Code: "AccessDeniedException", Message: "User: arn:aws:iam::123456789012:user/dev is not authorized to perform: bedrock:InvokeModelWithResponseStream on resource: arn:aws:bedrock:us-east-1::foundation-model/x"}
	fake := newBedrockFake(t, func(rec *bedrockFakeRecord) bedrockReply {
		if rec.Op == "InvokeModelWithResponseStream" {
			return denied
		}
		return newBedrockTurn(bedrockTextBlock("non streamed answer"))
	})
	fake.setEnv(t)
	c := bedrockTestClient(t, nil)
	for i := 0; i < 2; i++ {
		var text strings.Builder
		if _, err := c.Stream(context.Background(), bedrockTestRequest(bedrockTestClaude), func(ev StreamEvent) error {
			if ev.Kind == EventTextDelta {
				text.WriteString(ev.Text)
			}
			return nil
		}); err != nil || text.String() != "non streamed answer" {
			t.Fatalf("call %d: %v %q", i, err, text.String())
		}
	}
	var ops []string
	for _, r := range fake.snapshot() {
		ops = append(ops, r.Op)
	}
	if strings.Join(ops, ",") != "InvokeModelWithResponseStream,InvokeModel,InvokeModel" {
		t.Fatalf("ops %v", ops)
	}
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

func TestBedrockClaudeRouteCarriesGuardrailHeadersAndFlagsIntervention(t *testing.T) {
	h := bedrockIsolatedEnv(t)
	bedrockWriteConfig(t, h, "bedrock:\n  guardrail:\n    guardrail_identifier: gr-1\n    guardrail_version: DRAFT\n    trace: enabled\n")
	blocked := newBedrockTurn(bedrockTextBlock("BLOCKED_BY_GUARDRAIL_INPUT"))
	blocked.GuardrailAction = "INTERVENED"
	fake := newBedrockFake(t, bedrockSeq(blocked, newBedrockTurn(bedrockTextBlock("hi"))))
	fake.setEnv(t)
	c := bedrockTestClient(t, nil)
	resp, err := c.Complete(context.Background(), bedrockTestRequest(bedrockTestClaude))
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != "content_filter" {
		t.Fatalf("guardrail finish %q", resp.FinishReason)
	}
	if resp, err = c.Complete(context.Background(), bedrockTestRequest(bedrockTestClaude)); err != nil || resp.FinishReason != "stop" {
		t.Fatalf("clean finish %v %v", err, resp)
	}
	hdr := fake.snapshot()[0].Headers
	if hdr["x-amzn-bedrock-guardrailidentifier"] != "gr-1" || hdr["x-amzn-bedrock-guardrailversion"] != "DRAFT" || hdr["x-amzn-bedrock-trace"] != "ENABLED" {
		t.Fatalf("guardrail headers %#v", hdr)
	}
	if hdr["anthropic-beta"] == "" {
		t.Fatal("guardrail headers displaced the beta header")
	}
	c = bedrockTestClient(t, func(r *Route) { r.Headers = map[string]string{"X-Amzn-Bedrock-GuardrailVersion": "7"} })
	if _, err := c.Complete(context.Background(), bedrockTestRequest(bedrockTestClaude)); err != nil {
		t.Fatal(err)
	}
	if got := fake.snapshot()[2].Headers["x-amzn-bedrock-guardrailversion"]; got != "7" {
		t.Fatalf("route header override lost: %q", got)
	}
}

func TestBedrockCountTokensReadsInputTokens(t *testing.T) {
	bedrockIsolatedEnv(t)
	turn := newBedrockTurn()
	turn.InputTokens = 4321
	fake := newBedrockFake(t, bedrockSeq(turn))
	fake.setEnv(t)
	n, err := bedrockTestClient(t, nil).CountTokens(context.Background(), bedrockTestRequest(bedrockTestClaude))
	if err != nil || n != 4321 {
		t.Fatalf("count tokens %d %v", n, err)
	}
	rec := fake.snapshot()[0]
	inner := bedrockRecordBody(t, rec)
	if rec.Op != "CountTokens" || inner["anthropic_version"] != "bedrock-2023-05-31" || inner["model"] != nil || inner["messages"] == nil {
		t.Fatalf("count body %#v", inner)
	}
}

func TestBedrockBearerTokenAuth(t *testing.T) {
	bedrockIsolatedEnv(t)
	fake := newBedrockFake(t, bedrockSeq(newBedrockTurn(bedrockTextBlock("ok"))))
	fake.bearer = "ABSKbedrockapikey"
	t.Setenv("AWS_ENDPOINT_URL_BEDROCK_RUNTIME", fake.endpoint())
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "ABSKbedrockapikey")
	if _, err := bedrockTestClient(t, nil).Complete(context.Background(), bedrockTestRequest(bedrockTestClaude)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	ResetBedrockClientCache()
	if _, err := bedrockTestClient(t, func(r *Route) { r.APIKey = "ABSKbedrockapikey" }).Complete(context.Background(), bedrockTestRequest(bedrockTestClaude)); err != nil {
		t.Fatal(err)
	}
	for _, rec := range fake.snapshot() {
		if rec.Auth["bearer"] != "ABSKbedrockapikey" || rec.Headers["authorization"] != "Bearer ABSKbedrockapikey" {
			t.Fatalf("bearer auth %#v", rec.Auth)
		}
	}
}

func TestBedrockBaseURLOverrideAndRouteStaticKeys(t *testing.T) {
	bedrockIsolatedEnv(t)
	fake := newBedrockFake(t, bedrockSeq(newBedrockTurn(bedrockTextBlock("ok"))))
	t.Setenv("ANTHROPIC_BEDROCK_BASE_URL", fake.endpoint())
	c := bedrockTestClient(t, func(r *Route) {
		r.Region, r.AccessKeyID, r.SecretAccessKey = "eu-west-1", bedrockFakeAccessKey, bedrockFakeSecretKey
	})
	if _, err := c.Complete(context.Background(), bedrockTestRequest(bedrockTestClaude)); err != nil {
		t.Fatal(err)
	}
	if rec := fake.snapshot()[0]; rec.Auth["region"] != "eu-west-1" || rec.Rejected != "" {
		t.Fatalf("static key signing %#v", rec)
	}
}

func TestBedrockApplicationProfileARNPathIsEscapedAndSigned(t *testing.T) {
	bedrockIsolatedEnv(t)
	arn := "arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/abc123"
	fake := newBedrockFake(t, bedrockSeq(newBedrockTurn(bedrockTextBlock("ok"))))
	fake.setEnv(t)
	if _, err := bedrockTestClient(t, func(r *Route) { r.Model = arn }).Complete(context.Background(), bedrockTestRequest(arn)); err != nil {
		t.Fatal(err)
	}
	rec := fake.snapshot()[0]
	if rec.Model != arn || !strings.Contains(rec.Path, "application-inference-profile%2Fabc123/invoke") {
		t.Fatalf("arn path %q model %q", rec.Path, rec.Model)
	}
}

func TestBedrockHTTPErrorKeepsAWSErrorShape(t *testing.T) {
	bedrockIsolatedEnv(t)
	fake := newBedrockFake(t, bedrockSeq(&bedrockHTTPError{Code: "ValidationException", Message: "The model returned the following errors: bad"}))
	fake.setEnv(t)
	_, err := bedrockTestClient(t, nil).Complete(context.Background(), bedrockTestRequest(bedrockTestClaude))
	var apiErr *APIError
	var he *ccHTTPError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 || !errors.As(err, &he) || awsErrorTypeHeader(he.Headers) != "ValidationException" {
		t.Fatalf("validation error %#v", err)
	}
	if !strings.Contains(err.Error(), "The model returned the following errors") {
		t.Fatalf("message lost: %v", err)
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
