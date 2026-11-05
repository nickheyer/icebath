package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func auxReplayHistory() []map[string]any {
	return []map[string]any{
		{"role": "user", "content": "hi"},
		{"role": "assistant", "content": "ok", "reasoning_details": []any{map[string]any{"type": "reasoning.text", "text": "x", "signature": "E"}}},
		{"role": "user", "content": "again"},
	}
}

func TestAuxiliaryWireDropsReasoningDetailsOnlyForNonReplayingRoutes(t *testing.T) {
	history := auxReplayHistory()
	groq := &auxOpenAIClient{apiKey: "k", baseURL: "https://api.groq.com/openai/v1"}
	kwargs := auxPrepareChatMessages(groq, map[string]any{"model": "qwen/qwen3.6-27b", "messages": history})
	for _, m := range kwargs["messages"].([]map[string]any) {
		if _, ok := m["reasoning_details"]; ok {
			t.Fatalf("groq kept reasoning_details: %v", m)
		}
	}
	if _, ok := history[1]["reasoning_details"]; !ok {
		t.Fatal("durable history was mutated")
	}
	or := &auxOpenAIClient{apiKey: "k", baseURL: "https://openrouter.ai/api/v1"}
	kwargs = auxPrepareChatMessages(or, map[string]any{"model": "m", "messages": history})
	kept := false
	for _, m := range kwargs["messages"].([]map[string]any) {
		if _, ok := m["reasoning_details"]; ok {
			kept = true
		}
	}
	if !kept {
		t.Fatal("openrouter lost reasoning_details")
	}
}

func TestAuxiliaryWireLeavesAdaptersAndMessagelessKwargs(t *testing.T) {
	kwargs := map[string]any{"model": "m", "messages": auxReplayHistory()}
	if got := auxPrepareChatMessages(nil, kwargs); fmt.Sprint(got) != fmt.Sprint(kwargs) {
		t.Fatalf("non SDK client changed kwargs: %v", got)
	}
	meta := auxRelayChatMetadata(map[string]any{"api_mode": "codex_responses", "task": "title"})
	if meta["api_mode"] != "chat_completions" || meta["task"] != "title" {
		t.Fatalf("relay metadata %v", meta)
	}
}

func auxSDKStreamError(body any) error {
	return &AuxSDKError{Message: "stream error", Body: body}
}

func TestStatuslessStructuredErrorDetection(t *testing.T) {
	if !auxIsStatuslessStructuredProviderError(auxSDKStreamError(map[string]any{"type": "server_error", "code": "overloaded", "message": "busy"})) {
		t.Fatal("structured dict body not detected")
	}
	if !auxIsStatuslessStructuredProviderError(auxSDKStreamError("service unavailable")) {
		t.Fatal("string body not detected")
	}
	if auxIsStatuslessStructuredProviderError(auxSDKStreamError(map[string]any{"metadata": map[string]any{}})) {
		t.Fatal("metadata only body detected")
	}
	if auxIsStatuslessStructuredProviderError(errors.New("stream_error: mid_stream_failure")) {
		t.Fatal("plain error detected")
	}
	coded := &AuxSDKError{Message: "unavailable", StatusCode: 503, Body: map[string]any{"type": "server_error"}}
	if auxIsStatuslessStructuredProviderError(coded) {
		t.Fatal("status coded error detected")
	}
	wrapped := auxSDKStreamError(map[string]any{"error": map[string]any{"code": "overloaded"}})
	if !auxIsStatuslessStructuredProviderError(wrapped) {
		t.Fatal("error wrapper not accepted")
	}
}

func TestAuxErrorShapesReadStatusAndInvalidResponse(t *testing.T) {
	throttle := &AWSServiceError{StatusCode: 429, Code: "ThrottlingException"}
	if s, c := auxBotocoreError(throttle); s != 429 || c != "ThrottlingException" {
		t.Fatalf("botocore shape %d %s", s, c)
	}
	if auxExcHTTPStatus(throttle) != 429 || auxExcHTTPStatus(&ccHTTPError{Status: 503}) != 503 {
		t.Fatal("status readers")
	}
	if auxExcHTTPStatus(errors.New("x")) != 0 {
		t.Fatal("plain error carries no status")
	}
	invalid := &AuxRuntimeError{Message: "Auxiliary title: LLM returned invalid response (no choices[0].message)"}
	if !auxIsInvalidAuxResponseError(invalid) || auxIsInvalidAuxResponseError(errors.New(invalid.Message)) {
		t.Fatal("invalid response detection")
	}
}

type auxTestExplicitCancel struct{}

func (auxTestExplicitCancel) Error() string     { return "auxiliary request explicitly cancelled by host" }
func (auxTestExplicitCancel) auxBaseException() {}

type auxTestInterrupted struct{}

func (auxTestInterrupted) Error() string   { return "interrupted" }
func (auxTestInterrupted) auxInterrupted() {}

func TestAuxCallOutcomeCancellationAndEmbeddedError(t *testing.T) {
	if !auxIsCancellation(context.Canceled) || !auxIsCancellation(auxTestExplicitCancel{}) || !auxIsCancellation(auxTestInterrupted{}) {
		t.Fatal("cancellation kinds")
	}
	if auxIsCancellation(errors.New("boom")) {
		t.Fatal("plain error is not a cancellation")
	}
	if auxErrorClass(auxTestExplicitCancel{}, "p", "m") != "unknown" {
		t.Fatal("base exception classifies as unknown")
	}
	if got := auxEmbeddedProviderError(map[string]any{"error": map[string]any{"message": "upstream 429", "code": 429}}); got["message"] != "upstream 429" {
		t.Fatalf("embedded %v", got)
	}
	if auxEmbeddedProviderError(map[string]any{"error": map[string]any{}}) != nil || auxEmbeddedProviderError(map[string]any{"choices": []any{}}) != nil {
		t.Fatal("empty or absent error object")
	}
	embedded := &AuxRuntimeError{Message: "Auxiliary x: LLM returned invalid response", ProviderError: map[string]any{"message": "Rate limit exceeded", "code": 429}}
	if got := auxErrorClass(embedded, "openrouter", "m"); got == "unknown" || got == "" {
		t.Fatalf("embedded provider error class %q", got)
	}
}

func TestRotateRuntimeMainAPIKeyMatchesOnlyTheOldKey(t *testing.T) {
	if !auxSameValue("a", "a") || auxSameValue("a", nil) || !auxSameValue(nil, nil) || auxSameValue(map[string]any{}, "a") {
		t.Fatal("python equality")
	}
}

func TestNoStateFileMeansNoManagedEndpoint(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	auxResetManagedLocalCache()
	t.Cleanup(auxResetManagedLocalCache)
	if auxIsManagedLocalEndpoint("http://127.0.0.1:18080/v1") {
		t.Fatal("absent state file reported a managed endpoint")
	}
}

func TestManagedLocalEndpointMatchesStateFileNetlocWithTTL(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	auxResetManagedLocalCache()
	t.Cleanup(auxResetManagedLocalCache)
	now := time.Unix(1000, 0)
	auxNow = func() time.Time { return now }
	t.Cleanup(func() { auxNow = time.Now })
	path := localRuntimeStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("\xef\xbb\xbf{\"base_url\": \"http://127.0.0.1:18080/v1\"}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !auxIsManagedLocalEndpoint("http://127.0.0.1:18080/v1/") || auxIsManagedLocalEndpoint("http://127.0.0.1:9999/v1") || auxIsManagedLocalEndpoint("") {
		t.Fatal("managed endpoint match")
	}
	if err := os.WriteFile(path, []byte(`["not an object"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !auxIsManagedLocalEndpoint("http://127.0.0.1:18080/v1") {
		t.Fatal("cached netloc dropped inside the TTL")
	}
	now = now.Add(16 * time.Second)
	if auxIsManagedLocalEndpoint("http://127.0.0.1:18080/v1") {
		t.Fatal("non object state file kept a managed endpoint after the TTL")
	}
}

func TestBareLlamacppEndpointSkipsExplicitURLsAndOtherProviders(t *testing.T) {
	ctx := context.Background()
	if _, _, ok, err := auxBareLlamacppEndpoint(ctx, "llamacpp", "http://x/v1", ""); ok || err != nil {
		t.Fatal("explicit base url is not a bare alias")
	}
	if _, _, ok, err := auxBareLlamacppEndpoint(ctx, "openrouter", "", ""); ok || err != nil {
		t.Fatal("non llamacpp provider is not a bare alias")
	}
}

func TestAsyncRebuildKeepsKeyProviderAndCopiesHeaders(t *testing.T) {
	provider := func() (string, error) { return "minted", nil }
	sdk := &auxOpenAIClient{apiKey: "static", apiKeyProvider: provider, customHeaders: map[string]string{"X-A": "1"}}
	key, ok := auxAsyncAPIKey(sdk).(func() (string, error))
	if !ok {
		t.Fatal("key provider not passed through")
	}
	if v, _ := key(); v != "minted" {
		t.Fatalf("provider returned %q", v)
	}
	headers := auxConfiguredDefaultHeaders(sdk)
	headers["X-A"] = "2"
	if sdk.customHeaders["X-A"] != "1" {
		t.Fatal("headers not copied")
	}
	if !auxTruthyKey(provider) || auxTruthyKey("") || auxTruthyKey(nil) {
		t.Fatal("key truthiness")
	}
}

func TestAuxRateLimitWaitHonoursDeclaredCooldownAndCap(t *testing.T) {
	short := &AuxSDKError{StatusCode: 429, Message: "slow down", Headers: http.Header{"Retry-After": []string{"2"}}}
	if d, ok := auxRateLimitWait(short, 1); !ok || d != 2*time.Second {
		t.Fatalf("declared wait %v %v", d, ok)
	}
	long := &AuxSDKError{StatusCode: 429, Message: "slow down", Body: map[string]any{"error": map[string]any{"retry_after": 30.0}}}
	if _, ok := auxRateLimitWait(long, 1); ok {
		t.Fatal("cooldown above the cap must not be waited")
	}
	throttle := &AWSServiceError{StatusCode: 400, Code: "ThrottlingException", Headers: http.Header{"Retry-After": []string{"3"}}}
	if d, ok := auxRateLimitRetryAfter(throttle); !ok || d != 3*time.Second {
		t.Fatalf("bedrock header cooldown %v %v", d, ok)
	}
	d, ok := auxRateLimitWait(&AuxSDKError{StatusCode: 429, Message: "x"}, 1)
	if !ok || d < time.Second || d > 1500*time.Millisecond {
		t.Fatalf("first jittered wait %v", d)
	}
}

func TestAuxFallbackCandidateQuarantineTTL(t *testing.T) {
	for _, label := range []string{"rate limit", "connection error", "invalid provider response"} {
		if d, ok := auxFallbackCandidateQuarantineTTL(label); !ok || d != 60*time.Second {
			t.Fatalf("%s ttl %v", label, d)
		}
	}
	if _, ok := auxFallbackCandidateQuarantineTTL("payment error"); ok {
		t.Fatal("payment keeps the long default")
	}
	if _, ok := auxFallbackCandidateQuarantineTTL(""); ok {
		t.Fatal("stale credential keeps the long default")
	}
}

func TestSendWithParameterRungsPassesSuccessAndCancellationThrough(t *testing.T) {
	calls := 0
	send := func(ctx context.Context, client AuxClient, kwargs map[string]any) (any, error) {
		calls++
		return map[string]any{"ok": true}, nil
	}
	resp, err := SendWithParameterRungs(context.Background(), send, nil, map[string]any{"model": "m"}, "title")
	if err != nil || calls != 1 || resp.(map[string]any)["ok"] != true {
		t.Fatalf("success path %v %v %d", resp, err, calls)
	}
	cancelled := func(ctx context.Context, client AuxClient, kwargs map[string]any) (any, error) {
		return nil, context.Canceled
	}
	if _, err := SendWithParameterRungsAsync(context.Background(), cancelled, nil, map[string]any{}, "title"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation must propagate without the ladder, got %v", err)
	}
}

type auxUsageRow struct {
	session, task string
	usage         AuxiliaryUsage
}

type auxUsageRecorderFake struct{ rows []auxUsageRow }

func (f *auxUsageRecorderFake) RecordAuxiliaryUsage(ctx context.Context, sessionID, task string, usage AuxiliaryUsage) error {
	f.rows = append(f.rows, auxUsageRow{sessionID, task, usage})
	return nil
}

func auxUsageResponse(model string) map[string]any {
	return map[string]any{"model": model, "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120}}
}

func TestRecordAuxUsageWritesThroughContext(t *testing.T) {
	db := &auxUsageRecorderFake{}
	ctx := SetAccountingContext(context.Background(), db, "s1")
	RecordAuxUsage(ctx, auxUsageResponse("aux-m"), "vision", "gemini", "")
	if len(db.rows) != 1 {
		t.Fatalf("rows %v", db.rows)
	}
	row := db.rows[0]
	if row.session != "s1" || row.task != "vision" || row.usage.Model != "aux-m" || row.usage.InputTokens != 100 || row.usage.OutputTokens != 20 {
		t.Fatalf("row %+v", row)
	}
	RecordAuxUsage(context.Background(), auxUsageResponse("aux-m"), "vision", "gemini", "")
	RecordAuxUsage(SetAccountingContext(ctx, nil, "s1"), auxUsageResponse("aux-m"), "vision", "gemini", "")
	if len(db.rows) != 1 {
		t.Fatal("recorded outside an accounting scope")
	}
}

func TestRecordAuxUsageExcludesMoATasks(t *testing.T) {
	db := &auxUsageRecorderFake{}
	ctx := SetAccountingContext(context.Background(), db, "s1")
	RecordAuxUsage(ctx, auxUsageResponse("m"), "moa_reference", "", "")
	RecordAuxUsage(ctx, auxUsageResponse("m"), "moa_aggregator", "", "")
	RecordAuxUsage(ctx, map[string]any{"model": "m"}, "vision", "", "")
	if len(db.rows) != 0 {
		t.Fatalf("rows %v", db.rows)
	}
}
