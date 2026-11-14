package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Exception with an optional status code attribute, zero meaning the attribute is absent
func auxExc(msg string, status int) error {
	return &AuxSDKError{Class: "Exception", Message: msg, StatusCode: status}
}

type ReadTimeout struct{ msg string }

func (e *ReadTimeout) Error() string { return e.msg }

func TestAuxIsPaymentErrorCases(t *testing.T) {
	if !auxIsPaymentError(auxExc("Payment Required", 402)) {
		t.Fatal("402 is payment")
	}
	for _, spelling := range []string{"RESOURCE_EXHAUSTED", "ResourceExhausted", "resource-exhausted"} {
		for _, status := range []int{0, 429} {
			if !auxIsPaymentError(auxExc(spelling+": Worker local total request limit reached (32/32)", status)) {
				t.Fatalf("%s %d is payment", spelling, status)
			}
		}
	}
	if !auxIsPaymentError(auxExc("this model requires a subscription, upgrade for access: https://ollama.com/upgrade", 403)) {
		t.Fatal("403 subscription is payment")
	}
	if auxIsPaymentError(auxExc("Not Found", 404)) {
		t.Fatal("generic 404 is not payment")
	}
	if auxIsPaymentError(auxExc("credits exhausted", 500)) {
		t.Fatal("a 500 status never reads the body")
	}
}

func TestAuxIsModelNotFoundAndIncompatibleError(t *testing.T) {
	if !auxIsModelNotFoundError(auxExc("Model 'gpt-5.4-mini' not found. The requested model does not exist in our configuration or OpenRouter catalog.", 404)) {
		t.Fatal("catalog 404 is not found")
	}
	billing := auxExc("Model 'gpt-5' is not available on the free tier. Upgrade.", 404)
	if auxIsModelNotFoundError(billing) || !auxIsPaymentError(billing) {
		t.Fatal("billing 404 belongs to payment")
	}
	if auxIsModelNotFoundError(auxExc("Your API key is blocked or out of funds. model_not_found", 404)) {
		t.Fatal("billing keyword wins")
	}
	if !auxIsModelIncompatibleError(auxExc("Error code: 400 - {'detail': \"The 'glm-5.2' model is not supported when using Codex with a ChatGPT account.\"}", 400)) {
		t.Fatal("codex gating is incompatible")
	}
	notFound := auxExc("openrouter/foo/bar is not a valid model ID", 400)
	if auxIsModelIncompatibleError(notFound) || !auxIsModelNotFoundError(notFound) {
		t.Fatal("not found is not incompatible")
	}
	if auxIsModelIncompatibleError(auxExc("insufficient credits: model is not supported on free tier", 400)) {
		t.Fatal("payment 400 is not incompatible")
	}
	if auxIsModelIncompatibleError(auxExc("model is not supported", 404)) || auxIsModelNotFoundError(auxExc("model not found", 500)) {
		t.Fatal("status gates both predicates")
	}
}

func TestAuxIsRateLimitTimeoutConnectionError(t *testing.T) {
	if !auxIsRateLimitError(auxExc("Rate limit exceeded, try again in 2 seconds", 429)) {
		t.Fatal("429 rate limit")
	}
	if auxIsRateLimitError(auxExc("insufficient credits", 429)) || !auxIsRateLimitError(auxExc("slow down", 429)) {
		t.Fatal("billing 429 is not a rate limit, a bare 429 is")
	}
	if !auxIsRateLimitError(&AuxSDKError{Class: "RateLimitError", Message: "x"}) || !auxIsRateLimitError(&AWSServiceError{Code: "ThrottlingException", StatusCode: 400}) {
		t.Fatal("class and Bedrock throttle are rate limits")
	}
	if !auxIsTimeoutError(errors.New("Request timed out.")) || !auxIsTimeoutError(&ReadTimeout{"slow"}) {
		t.Fatal("timeout by wording and class name")
	}
	if !auxIsTimeoutError(&AuxSDKError{Class: "APITimeoutError", Message: "x"}) || !auxIsTimeoutError(&AuxTransportError{Class: "ReadTimeout", Message: "x"}) {
		t.Fatal("SDK and transport timeouts")
	}
	if !auxIsConnectionError(errors.New("Connection refused")) {
		t.Fatal("connection refused")
	}
	if auxIsConnectionError(auxExc("Bad Request: invalid model", 400)) {
		t.Fatal("a 400 is not a connection error")
	}
	if !auxIsConnectionError(&AuxSDKError{Class: "APIConnectionError", Message: "Connection error."}) || !auxIsConnectionError(errors.New("peer closed connection without sending complete message body (incomplete chunked read)")) {
		t.Fatal("SDK connection class and premature close")
	}
	if !auxIsTransientTransportError(auxExc("bad gateway", 502)) || !auxIsTransientTransportError(auxExc("req timeout", 408)) || auxIsTransientTransportError(auxExc("bad", 400)) {
		t.Fatal("5xx and 408 are transient, 400 is not")
	}
}

func TestOpenrouterLimit403IsBillingForMainLoopAndAuxLadder(t *testing.T) {
	for _, body := range []string{
		"Error code: 403 - {'error': {'message': 'Budget limit exceeded (monthly limit). Contact your org admin.', 'code': 403}}",
		"Error code: 403 - {'error': {'message': 'Key limit exceeded (total limit). Contact your org admin.', 'code': 403}}",
	} {
		exc := auxExc(body, 403)
		if r := ClassifyError(auxClassifyInput(exc, "", "")).Reason; r != ReasonBilling {
			t.Fatalf("main classifier reason %s", r)
		}
		if !auxIsPaymentError(exc) {
			t.Fatal("aux payment")
		}
	}
	for _, p := range billingPatterns {
		if !slices.Contains(auxPaymentKeywords, p) {
			t.Fatalf("payment keywords miss %q", p)
		}
	}
}

func TestAbsentCredentialsQuarantineIsDebugAndNamesTheRealReason(t *testing.T) {
	ctx := home.WithOverride(context.Background(), t.TempDir())
	auxResetAuxUnhealthyCache()
	t.Cleanup(auxResetAuxUnhealthyCache)
	logs := auxCaptureLogs(t)
	auxMarkProviderUnhealthy(ctx, "openrouter", 60, "", "OPENROUTER_API_KEY not set", slog.LevelDebug)
	auxLogSkipUnhealthy(ctx, "openrouter", "title_generation", "")
	auxMarkProviderUnhealthy(ctx, "nous", auxUnhealthyTTLSeconds, "", auxUnhealthyPaymentReason, slog.LevelWarn)
	marks := logs.matching("marking")
	if len(marks) != 2 || marks[0].level != slog.LevelDebug || strings.Contains(marks[0].msg, "payment") || marks[1].level != slog.LevelWarn || !strings.Contains(marks[1].msg, "payment") {
		t.Fatalf("marks %+v", marks)
	}
	if !strings.Contains(marks[0].msg, "OPENROUTER_API_KEY not set") {
		t.Fatal(marks[0].msg)
	}
	skip := logs.matching("skipping")
	if len(skip) == 0 || !strings.Contains(skip[0].msg, "OPENROUTER_API_KEY not set") || strings.Contains(skip[0].msg, "payment") {
		t.Fatalf("skip %+v", skip)
	}
}

func TestAuxProfileScopedUnhealthyMarksFollowRoutedHome(t *testing.T) {
	a := home.WithOverride(context.Background(), t.TempDir())
	b := home.WithOverride(context.Background(), t.TempDir())
	auxResetAuxUnhealthyCache()
	t.Cleanup(auxResetAuxUnhealthyCache)
	auxMarkProviderUnhealthy(a, "openrouter", auxUnhealthyTTLSeconds, "", auxUnhealthyPaymentReason, slog.LevelWarn)
	if !auxIsProviderUnhealthy(a, "openrouter", "") || auxIsProviderUnhealthy(b, "openrouter", "") {
		t.Fatal("unhealthy marks are keyed by the routed profile home")
	}
}

func TestReasoningEffortNoneUnsupportedReversedWording(t *testing.T) {
	bad := func(msg string) error { return auxExc(msg, 400) }
	if !auxIsReasoningFieldRejection(bad("Error code: 400 - reasoning_effort 'none' unsupported; use minimal|low|medium|high|xhigh")) {
		t.Fatal("reversed wording fires")
	}
	if auxIsReasoningFieldRejection(bad("The model kimi-k2-thinking is not supported when using this account")) || auxIsReasoningFieldRejection(bad("reasoning models: tool_choice 'required' is unsupported")) {
		t.Fatal("route gating and far adjectives do not fire")
	}
	if auxIsReasoningFieldRejection(auxExc("reasoning_effort 'none' unsupported", 500)) {
		t.Fatal("a 500 is never a field rejection")
	}
}

func TestAuxIsStructuredOutputRejection(t *testing.T) {
	runtime := func(msg string) error { return &AuxSDKError{Class: "RuntimeError", Message: msg} }
	for _, msg := range []string{
		"Error code: 400 - {'error': {'message': 'guided_grammar '{\"additionalProperties\":false}' has compile_grammar_error: No module named 'xgrammar'', 'type': 'invalid_request_error'}}",
		"HTTP 400: This response_format type is unavailable now",
		"HTTP 400: response_format: Extra inputs are not permitted",
		"HTTP 400: output_config: Extra inputs are not permitted",
		"Unsupported parameter: response_format",
		"output_config is not supported",
		"HTTP 422: {\"detail\":[{\"loc\":[\"body\",\"response_format\",\"json_schema\"],\"msg\":\"str type expected\",\"type\":\"type_error.str\"}]}",
		"Gemini HTTP 400 (INVALID_ARGUMENT): Function calling with a response mime type: 'application/json' is unsupported",
		"Gemini HTTP 400 (INVALID_ARGUMENT): Invalid JSON payload received. Unknown name \"response_json_schema\" at 'generation_config'",
		"Gemini HTTP 400 (INVALID_ARGUMENT): Invalid value at 'generation_config.response_schema.properties[0].value.type'",
	} {
		if !auxIsStructuredOutputRejection(runtime(msg)) {
			t.Fatalf("should match %q", msg)
		}
	}
	for _, msg := range []string{
		"HTTP 400: Invalid value: 'tool'. Supported values are: 'assistant'",
		"HTTP 400: Unsupported parameter: temperature",
		"max_tokens is too large for this model",
		"Rate limit exceeded",
		"Connection reset by peer",
		"messages: Extra inputs are not permitted",
	} {
		if auxIsStructuredOutputRejection(runtime(msg)) {
			t.Fatalf("should not match %q", msg)
		}
	}
	if auxIsStructuredOutputRejection(&AuxSDKError{Class: "RuntimeError", Message: "output_config: Extra inputs are not permitted", StatusCode: 500}) {
		t.Fatal("non 400 statuses never match")
	}
}

func TestAuxIsUnsupportedTemperatureError(t *testing.T) {
	t.Run("test_matches_real_provider_messages", func(t *testing.T) {
		for _, msg := range []string{
			"HTTP 400: Unsupported parameter: temperature",
			"Error code: 400 - {'error': {'message': \"Unsupported parameter: 'temperature'\"}}",
			"Error code: 400 - {'error': {'code': 'unsupported_parameter', 'param': 'temperature'}}",
			"Provider returned error: temperature is not supported for this model",
			"this model does not support temperature",
			"temperature: unknown parameter",
			"unrecognized request argument supplied: temperature",
		} {
			if !auxIsUnsupportedParameterError(errors.New(msg), "temperature") {
				t.Fatalf("should match %q", msg)
			}
		}
	})
	t.Run("test_does_not_match_unrelated_errors", func(t *testing.T) {
		for _, msg := range []string{
			"HTTP 400: Invalid value: 'tool'. Supported values are: 'assistant'...",
			"max_tokens is too large for this model",
			"Rate limit exceeded",
			"Connection reset by peer",
			"temperature must be between 0 and 2",
		} {
			if auxIsUnsupportedParameterError(errors.New(msg), "temperature") {
				t.Fatalf("should not match %q", msg)
			}
		}
	})
	if auxIsUnsupportedParameterError(errors.New("unsupported"), "") {
		t.Fatal("an empty parameter never matches")
	}
}

func TestAuxIsAuthErrorAndSameProviderRetrySkip(t *testing.T) {
	if !auxIsAuthError(auxExc("nope", 401)) || !auxIsAuthError(errors.New("Error code: 401 - bad key")) || !auxIsAuthError(&AuxSDKError{Class: "AuthenticationError", Message: "x"}) {
		t.Fatal("401 shapes are auth errors")
	}
	if !auxIsAuthError(auxExc("unauthenticated:bad-credentials", 403)) || !auxIsAuthError(errors.New("unauthenticated:bad-credentials")) || auxIsAuthError(auxExc("bad-credentials", 500)) {
		t.Fatal("xAI 403 bad credentials")
	}
	timeout := errors.New("Request timed out.")
	if !auxShouldSkipSameProviderRetry("compression", timeout) || auxShouldSkipSameProviderRetry("web_extract", timeout) || auxShouldSkipSameProviderRetry("", timeout) {
		t.Fatal("only critical path tasks skip")
	}
	if auxShouldSkipSameProviderRetry("vision", errors.New("no-progress timeout after 30s (timed out)")) || auxShouldSkipSameProviderRetry("vision", errors.New("Connection refused")) {
		t.Fatal("fast first token fails and connection drops keep the retry")
	}
}

func TestAuxWithoutReasoningFields(t *testing.T) {
	if got := auxWithoutReasoningFields(map[string]any{"model": "m", "reasoning_effort": nil}); got != nil {
		t.Fatalf("a None effort is no change %v", got)
	}
	in := map[string]any{"model": "m", "reasoning_effort": "none", "_reasoning_config": map[string]any{}, "extra_body": map[string]any{" Thinking ": 1, "top_k": 2}}
	got := auxWithoutReasoningFields(in)
	if fmt.Sprint(got) != "map[extra_body:map[top_k:2] model:m]" || len(in) != 4 {
		t.Fatalf("got %v in %v", got, in)
	}
	got = auxWithoutReasoningFields(map[string]any{"extra_body": map[string]any{"verbosity": "low"}})
	if _, ok := got["extra_body"]; got == nil || ok {
		t.Fatalf("empty extra body is dropped %v", got)
	}
}

func TestAuxTransientRetryCountClampsConfig(t *testing.T) {
	prev := auxLoadConfigFn
	t.Cleanup(func() { auxLoadConfigFn = prev })
	for _, tc := range []struct {
		val  any
		want int
	}{{nil, 2}, {0, 0}, {9, 6}, {-3, 0}, {"4", 4}, {"many", 2}, {3.9, 3}} {
		auxLoadConfigFn = func(context.Context) (config.Tree, error) {
			return config.Tree{"auxiliary": map[string]any{"transient_retries": tc.val}}, nil
		}
		if got := auxTransientRetryCount(context.Background()); got != tc.want {
			t.Fatalf("%v gave %d want %d", tc.val, got, tc.want)
		}
	}
	auxLoadConfigFn = func(context.Context) (config.Tree, error) { return nil, errors.New("broken") }
	if auxTransientRetryCount(context.Background()) != 2 {
		t.Fatal("config failures fall back to the default")
	}
}

func TestAuxNousPortalAccountHasFreshPaidAccess(t *testing.T) {
	prev := auxNousPortalAccountInfoFn
	t.Cleanup(func() { auxNousPortalAccountInfoFn = prev })
	yes, no := true, false
	for _, tc := range []struct {
		info *NousPortalAccountInfo
		want bool
	}{{&NousPortalAccountInfo{PaidServiceAccess: &yes}, true}, {&NousPortalAccountInfo{PaidServiceAccess: &no}, false}, {&NousPortalAccountInfo{}, false}, {nil, false}} {
		var forced bool
		auxNousPortalAccountInfoFn = func(_ context.Context, force bool) *NousPortalAccountInfo { forced = force; return tc.info }
		if got := auxNousPortalAccountHasFreshPaidAccess(context.Background()); got != tc.want || !forced {
			t.Fatalf("%+v gave %v forced %v", tc.info, got, forced)
		}
	}
}

// Fresh client cache restored after the test
func auxIsolateClientCache(t *testing.T) {
	t.Helper()
	auxClientCacheMu.Lock()
	prev, prevOrder := auxClientCache, auxClientCacheOrder
	auxClientCache, auxClientCacheOrder = map[auxClientCacheKey]auxCachedClient{}, nil
	auxClientCacheMu.Unlock()
	t.Cleanup(func() {
		auxClientCacheMu.Lock()
		auxClientCache, auxClientCacheOrder = prev, prevOrder
		auxClientCacheMu.Unlock()
	})
}

func TestAuxEvictCachedClientsIsProfileAndProviderScoped(t *testing.T) {
	auxIsolateClientCache(t)
	ctxA := home.WithOverride(context.Background(), t.TempDir())
	ctxB := home.WithOverride(context.Background(), t.TempDir())
	keyA, keyB := home.Key(home.Dir(ctxA)), home.Key(home.Dir(ctxB))
	c := &auxOpenAIClient{}
	auxClientCacheMu.Lock()
	auxClientCacheSetLocked(auxClientCacheKey{Home: keyA, Provider: "openrouter"}, auxCachedClient{Client: c})
	auxClientCacheSetLocked(auxClientCacheKey{Home: keyA, Provider: "nous", Async: true}, auxCachedClient{Client: c})
	auxClientCacheSetLocked(auxClientCacheKey{Home: keyB, Provider: "openrouter"}, auxCachedClient{Client: c})
	auxClientCacheMu.Unlock()
	auxEvictCachedClients(ctxA, "OpenRouter")
	keys := auxClientCacheKeysLocked()
	if len(keys) != 2 || keys[0].Provider != "nous" || keys[1].Home != keyB {
		t.Fatalf("keys %+v", keys)
	}
}

func TestAuxiliaryClientPoisonedCacheEvictionEvictCachedClientInstanceWalksAsyncWrapper(t *testing.T) {
	auxIsolateClientCache(t)
	real := &auxOpenAIClient{}
	sync := NewCodexAuxiliaryClient(real, "gpt-5")
	async := NewAsyncCodexAuxiliaryClient(sync)
	other := &auxOpenAIClient{}
	auxClientCacheMu.Lock()
	auxClientCacheSetLocked(auxClientCacheKey{Provider: "openai-codex"}, auxCachedClient{Client: sync})
	auxClientCacheSetLocked(auxClientCacheKey{Provider: "openai-codex", Async: true}, auxCachedClient{Client: async})
	auxClientCacheSetLocked(auxClientCacheKey{Provider: "custom"}, auxCachedClient{Client: other})
	auxClientCacheMu.Unlock()
	if auxEvictCachedClientInstance(nil) || auxEvictCachedClientInstance((*auxOpenAIClient)(nil)) {
		t.Fatal("nil targets evict nothing")
	}
	if !auxEvictCachedClientInstance(real) {
		t.Fatal("the leaf client evicts its wrappers")
	}
	keys := auxClientCacheKeysLocked()
	if len(keys) != 1 || keys[0].Provider != "custom" || auxEvictCachedClientInstance(real) {
		t.Fatalf("keys %+v", keys)
	}
	if !auxEvictCachedClientInstance(other) || len(auxClientCacheKeysLocked()) != 0 {
		t.Fatal("direct identity evicts")
	}
}

func TestAuxProviderForHostTablesAndCredsHaveAPIKey(t *testing.T) {
	ctx := context.Background()
	if auxProviderForHost(ctx, "https://api.x.ai/v1", auxAuthRefreshProviderByHost) != "xai-oauth" || auxProviderForHost(ctx, "https://chatgpt.com/backend-api/codex", auxPoolProviderByHost) != "openai-codex" {
		t.Fatal("table hosts")
	}
	if auxProviderForHost(ctx, "https://openrouter.ai/api/v1", auxAuthRefreshProviderByHost) == "openrouter" || auxProviderForHost(ctx, "https://api.kimi.com.attacker.invalid", auxPoolProviderByHost) == "kimi-coding" {
		t.Fatal("refresh table has no openrouter row and look alike hosts never match")
	}
	if auxProviderForHost(ctx, "https://unclaimed.invalid/v1", auxPoolProviderByHost) != "" {
		t.Fatal("unclaimed host")
	}
	if auxCredsHaveAPIKey(map[string]any{"api_key": "  "}) || auxCredsHaveAPIKey(map[string]any{"api_key": nil}) || auxCredsHaveAPIKey(map[string]any{}) || !auxCredsHaveAPIKey(map[string]any{"api_key": "sk"}) {
		t.Fatal("api key presence")
	}
}

func TestAuxUnhealthyCacheTtlExpiryEvicts(t *testing.T) {
	ctx := home.WithOverride(context.Background(), t.TempDir())
	auxResetAuxUnhealthyCache()
	t.Cleanup(auxResetAuxUnhealthyCache)
	now := 1000.0
	prev := auxUnhealthyNow
	auxUnhealthyNow = func() float64 { return now }
	t.Cleanup(func() { auxUnhealthyNow = prev })
	auxMarkProviderUnhealthy(ctx, "openrouter", 0.01, "", auxUnhealthyPaymentReason, slog.LevelWarn)
	if !auxIsProviderUnhealthy(ctx, "openrouter", "") {
		t.Fatal("fresh mark")
	}
	now += 0.02
	if auxIsProviderUnhealthy(ctx, "openrouter", "") {
		t.Fatal("expired mark")
	}
	if _, ok := auxUnhealthyUntil[auxUnhealthyCacheKey(ctx, "openrouter", "")]; ok {
		t.Fatal("lazy eviction removes the entry")
	}
	auxMarkProviderUnhealthy(ctx, "custom", auxUnhealthyTTLSeconds, "https://Example.test/API/v1/", auxUnhealthyPaymentReason, slog.LevelWarn)
	if !auxIsProviderUnhealthy(ctx, "custom", "https://example.TEST/API/v1") || auxIsProviderUnhealthy(ctx, "custom", "https://example.test/api/v1") {
		t.Fatal("host case folds, path case stays")
	}
	auxMarkProviderUnhealthy(ctx, "custom", auxUnhealthyTTLSeconds, "https://example.test/API/v1?token=AbC", auxUnhealthyPaymentReason, slog.LevelWarn)
	if auxIsProviderUnhealthy(ctx, "custom", "https://example.test/API/v1?token=abc") {
		t.Fatal("query case stays")
	}
}

func TestAuxRefreshNousRecommendedModelNoDistinctAlternative(t *testing.T) {
	prev := auxGetNousRecommendedAuxModelFn
	auxGetNousRecommendedAuxModelFn = func(context.Context, bool, bool) string { return auxNousModel }
	t.Cleanup(func() { auxGetNousRecommendedAuxModelFn = prev })
	if got := auxRefreshNousRecommendedModel(context.Background(), false, auxNousModel); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestAuxExcTypeNameExposesTheOutermostClass(t *testing.T) {
	if got := AuxExcTypeName(&AuxSDKError{Class: "RateLimitError"}); got != "RateLimitError" {
		t.Fatalf("sdk %q", got)
	}
	if got := AuxExcTypeName(&AuxTransportError{Class: "ConnectTimeout"}); got != "ConnectTimeout" {
		t.Fatalf("transport %q", got)
	}
}
