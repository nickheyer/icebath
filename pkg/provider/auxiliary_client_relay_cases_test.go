package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Chat client answering the queued responses or errors in order
func auxRelayAttemptsClient(outcomes ...any) *auxCallFakeClient {
	pending := outcomes
	c := &auxCallFakeClient{}
	c.create = func(context.Context, map[string]any) (any, error) {
		next := pending[0]
		pending = pending[1:]
		if err, ok := next.(error); ok {
			return nil, err
		}
		return next, nil
	}
	return c
}

// Chat completion dict naming its model and holding one assistant message
func auxRelayAnswer(model, content string) map[string]any {
	return map[string]any{"model": model, "choices": []any{map[string]any{"message": map[string]any{"content": content}}}}
}

// Restores the relay execute and completion seams after the test
func auxSwapRelaySeams(t *testing.T) {
	t.Helper()
	if os.Getenv("ICEBATH_HOME") == "" {
		t.Setenv("ICEBATH_HOME", t.TempDir())
	}
	exec, asyncExec, complete := relayExecuteCurrentFn, relayExecuteCurrentAsyncFn, relayCompleteLogicalCallFn
	t.Cleanup(func() {
		relayExecuteCurrentFn, relayExecuteCurrentAsyncFn, relayCompleteLogicalCallFn = exec, asyncExec, complete
	})
}

// Records every logical call completion as its argument tuple
func auxRecordLogicalCompletions() *[][]any {
	var completions [][]any
	relayCompleteLogicalCallFn = func(_ context.Context, requestID, outcome, modelName, providerName, responseModelName, errorClass string) error {
		completions = append(completions, []any{requestID, outcome, modelName, providerName, responseModelName, errorClass})
		return nil
	}
	return &completions
}

// Records the kwargs of one relayed attempt the way the hermes execute_current double does
func auxRelayAttemptRecord(name, modelName string, metadata map[string]any, deferLogical bool) map[string]any {
	return map[string]any{"name": name, "model_name": modelName, "metadata": metadata, "defer_logical_completion": deferLogical}
}

// Runs one completion whose validation must fail as an invalid response
func auxExpectInvalidResponse(ctx context.Context, client AuxClient, kwargs map[string]any, task string) error {
	resp, err := auxRelaySyncCompletion(ctx, client, kwargs, "", "", nil)
	if err != nil {
		return err
	}
	if _, err := auxValidateLLMResponse(ctx, resp, task, "", ""); err == nil || !strings.Contains(err.Error(), "invalid response") {
		return fmt.Errorf("invalid first response was accepted: %v", err)
	}
	return nil
}

// Completion followed by validation under the bound relay call
func auxRelayValidatedCompletion(ctx context.Context, client AuxClient, kwargs map[string]any, task string) (any, error) {
	resp, err := auxRelaySyncCompletion(ctx, client, kwargs, "", "", nil)
	if err != nil {
		return nil, err
	}
	return auxValidateLLMResponse(ctx, resp, task, "", "")
}

// Pop outputs recorded since the index, a missing output reading as an empty dict
func auxRelayPopOutputs(f *relayLLMFake, from int) []any {
	outs := f.popOutputs()[from:]
	for i, o := range outs {
		if o == nil {
			outs[i] = map[string]any{}
		}
	}
	return outs
}

// Whether both values are the same map instance
func auxSameMap(got, want any) bool {
	g, w := reflect.ValueOf(got), reflect.ValueOf(want)
	return g.Kind() == reflect.Map && w.Kind() == reflect.Map && g.UnsafePointer() == w.UnsafePointer()
}

func TestAuxiliaryRetriesShareLogicalRelayIdentity(t *testing.T) {
	auxSwapRelaySeams(t)
	var attempts []map[string]any
	relayExecuteCurrentFn = func(ctx context.Context, request map[string]any, callback func(context.Context, map[string]any) (any, error), name, modelName string, metadata map[string]any, deferLogical bool) (any, error) {
		attempts = append(attempts, auxRelayAttemptRecord(name, modelName, metadata, deferLogical))
		return callback(ctx, request)
	}
	completions := auxRecordLogicalCompletions()
	client := auxRelayAttemptsClient(map[string]any{"choices": []any{}}, auxCallOK("ok"))
	kwargs := func() map[string]any { return map[string]any{"model": "test-model", "messages": []any{}} }
	result, err := auxRelayAuxiliaryCall(context.Background(), "compression", func(ctx context.Context) (any, error) {
		auxSetRelayAuxiliaryRoute(ctx, "openrouter", "test-model", "chat_completions")
		if err := auxExpectInvalidResponse(ctx, client, kwargs(), "compression"); err != nil {
			return nil, err
		}
		return auxRelayValidatedCompletion(ctx, client, kwargs(), "compression")
	})
	if err != nil || ExtractContentOrReasoning(result, -1) != "ok" {
		t.Fatalf("result %v %v", result, err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts %v", attempts)
	}
	meta := func(i int) map[string]any { return attempts[i]["metadata"].(map[string]any) }
	if meta(0)["api_request_id"] != meta(1)["api_request_id"] || meta(0)["call_role"] != "auxiliary:compression" {
		t.Fatalf("attempt metadata %v %v", meta(0), meta(1))
	}
	if fmt.Sprint(meta(0)["retry_count"], meta(1)["retry_count"]) != "0 1" {
		t.Fatalf("retry counts %v %v", meta(0)["retry_count"], meta(1)["retry_count"])
	}
	for _, attempt := range attempts {
		if attempt["defer_logical_completion"] != true {
			t.Fatalf("attempt did not defer logical completion %v", attempt)
		}
	}
	relayLLMEqual(t, *completions, [][]any{{meta(0)["api_request_id"], "success", "test-model", "openrouter", "", "unknown"}})
}

func TestAuxiliaryProviderFallbackClosesOneRealLogicalCall(t *testing.T) {
	f, ctx, turn := relayLLMTurn(t)
	consumer := "test.auxiliary-provider-fallback"
	if err := turn.Lease.Host.RetainManagedExecution(consumer); err != nil {
		t.Fatal(err)
	}
	defer turn.Lease.Host.ReleaseManagedExecution(consumer)
	before := len(f.popOutputs())
	client := auxRelayAttemptsClient(map[string]any{"choices": []any{}}, auxCallOK("recovered"))
	result, err := auxRelayAuxiliaryCall(ctx, "compression", func(ctx context.Context) (any, error) {
		auxSetRelayAuxiliaryRoute(ctx, "nvidia", "nvidia/test-model", "chat_completions")
		if err := auxExpectInvalidResponse(ctx, client, map[string]any{"model": "nvidia/test-model", "messages": []any{}}, "compression"); err != nil {
			return nil, err
		}
		if open := relayLLMLogicalCalls(turn); len(open) != 1 {
			return nil, fmt.Errorf("logical calls after the failed attempt %v", open)
		}
		auxSetRelayAuxiliaryRoute(ctx, "openrouter", "openrouter/test-model", "chat_completions")
		return auxRelayValidatedCompletion(ctx, client, map[string]any{"model": "openrouter/test-model", "messages": []any{}}, "compression")
	})
	if err != nil || ExtractContentOrReasoning(result, -1) != "recovered" {
		t.Fatalf("result %v %v", result, err)
	}
	relayLLMEqual(t, relayLLMLogicalCalls(turn), map[string]any{})
	relayLLMEqual(t, auxRelayPopOutputs(f, before), []any{map[string]any{
		"error_class": "unknown", "model": "openrouter/test-model", "outcome": "success", "provider": "openrouter"}})
}

// Runs the body inside the turn with a real shared metrics subscriber and returns the stored counter rows
func auxRouteRows(t *testing.T, f *relayLLMFake, ctx context.Context, turn *tools.RelayTurnContext, name string, run func()) []observability.CounterRow {
	t.Helper()
	dir := t.TempDir()
	store, err := observability.NewSharedMetricsStore(ctx, filepath.Join(dir, "metrics.sqlite3"), filepath.Join(dir, "outbox"))
	if err != nil {
		t.Fatal(err)
	}
	runtimeID := relayLLMRuntime(turn).RuntimeID
	subscriber, err := observability.NewSharedMetricsSubscriber(ctx, store, "test-version", &runtimeID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.relay.Subscribers.Register(name, func(e *tools.RelayEventView) {
		subscriber.Call(ctx, &observability.RelayEvent{Kind: e.Kind, Name: e.Name, Category: e.Category, ScopeCategory: e.ScopeCategory,
			CategoryProfile: e.CategoryProfile, Metadata: e.Metadata, Data: e.Data})
	}); err != nil {
		t.Fatal(err)
	}
	if err := turn.Lease.Host.RetainManagedExecution(name); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			turn.Lease.Host.ReleaseManagedExecution(name)
			_ = f.relay.Subscribers.Deregister(name)
		}()
		run()
		if err := f.relay.Subscribers.Flush(); err != nil {
			t.Fatal(err)
		}
	}()
	rows, err := store.CounterSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Dimension map of one counter row
func auxRowDims(row observability.CounterRow) map[string]any {
	out := map[string]any{}
	v := reflect.ValueOf(row.Dimensions)
	if v.Kind() != reflect.Map {
		return out
	}
	for _, k := range v.MapKeys() {
		out[fmt.Sprint(k.Interface())] = v.MapIndex(k).Interface()
	}
	return out
}

// Sorted outcome, error class and value triples of the model route rows
func auxRouteTuples(rows []observability.CounterRow) []string {
	var out []string
	for _, r := range rows {
		if r.MetricName != observability.ModelRouteMetric {
			continue
		}
		d := auxRowDims(r)
		out = append(out, fmt.Sprintf("%v|%v|%d", d["outcome"], d["error_class"], r.Value))
	}
	sort.Strings(out)
	return out
}

func TestAuxiliaryProviderFallbackRecordsOneTerminalModelRoute(t *testing.T) {
	f, ctx, turn := relayLLMTurn(t)
	client := auxRelayAttemptsClient(map[string]any{"model": "failed/model", "choices": []any{}}, auxRelayAnswer("Accepted/Model", "recovered"))
	rows := auxRouteRows(t, f, ctx, turn, "test.auxiliary-model-route", func() {
		result, err := auxRelayAuxiliaryCall(ctx, "compression", func(ctx context.Context) (any, error) {
			auxSetRelayAuxiliaryRoute(ctx, "nvidia", "failed/configured-model", "chat_completions")
			if err := auxExpectInvalidResponse(ctx, client, map[string]any{"model": "failed/configured-model", "messages": []any{}}, "compression"); err != nil {
				return nil, err
			}
			auxSetRelayAuxiliaryRoute(ctx, "OpenRouter", "fallback/configured-model", "chat_completions")
			return auxRelayValidatedCompletion(ctx, client, map[string]any{"model": "fallback/configured-model", "messages": []any{}}, "compression")
		})
		if err != nil || ExtractContentOrReasoning(result, -1) != "recovered" {
			t.Fatalf("result %v %v", result, err)
		}
	})
	if len(rows) != 1 || rows[0].MetricName != observability.ModelRouteMetric || rows[0].Resource["icebath_version"] != "test-version" {
		t.Fatalf("rows %#v", rows)
	}
	relayLLMEqual(t, auxRowDims(rows[0]), map[string]any{"call_role": "auxiliary", "error_class": "unknown", "model": "accepted/model",
		"outcome": "success", "provider": "openrouter", "ttft_bucket": "unknown"})
	if rows[0].Value != 1 || rows[0].PackagedValue != 0 {
		t.Fatalf("row values %d %d", rows[0].Value, rows[0].PackagedValue)
	}
}

// The hermes _aux_call helper, retrying Exceptions before the attempts run out and letting aborts through
func auxRelayCall(ctx context.Context, task string, client AuxClient, attempts int) (any, error) {
	return auxRelayAuxiliaryCall(ctx, task, func(ctx context.Context) (any, error) {
		auxSetRelayAuxiliaryRoute(ctx, "openrouter", "m", "chat_completions")
		kwargs := func() map[string]any { return map[string]any{"model": "m", "messages": []any{}} }
		for i := 0; i < attempts-1; i++ {
			out, err := auxRelayValidatedCompletion(ctx, client, kwargs(), task)
			if err == nil {
				return out, nil
			}
			if auxIsBaseException(err) {
				return nil, err
			}
		}
		return auxRelayValidatedCompletion(ctx, client, kwargs(), task)
	})
}

func TestAuxiliaryModelRoutesCarryClassifiedErrorClass(t *testing.T) {
	f, ctx, turn := relayLLMTurn(t)
	rows := auxRouteRows(t, f, ctx, turn, "test.auxiliary-route-rows", func() {
		if _, err := auxRelayCall(ctx, "title_generation", auxRelayAttemptsClient(&AuxSDKError{StatusCode: 429, Message: "Rate limit exceeded"}, auxRelayAnswer("m", "ok")), 2); err != nil {
			t.Fatal(err)
		}
		_, err := auxRelayCall(ctx, "title_generation", auxRelayAttemptsClient(&AuxSDKError{StatusCode: 404, Message: "The model `m` does not exist"}), 1)
		var sdk *AuxSDKError
		if !errors.As(err, &sdk) || sdk.StatusCode != 404 {
			t.Fatalf("second call %v", err)
		}
	})
	relayLLMEqual(t, auxRouteTuples(rows), []string{"failed|model_not_found|1", "success|rate_limit|1"})
}

func TestAbortedAuxiliaryCallIsCancelledNotFailed(t *testing.T) {
	aborts := []struct {
		name string
		err  error
	}{
		{"explicit_cancellation", &AuxiliaryExplicitCancellation{}},
		{"interrupted_error", &InterruptedError{Message: "aux stream interrupted"}},
		{"keyboard_interrupt", tools.ErrKeyboardInterrupt},
	}
	for _, abort := range aborts {
		t.Run(abort.name, func(t *testing.T) {
			f, ctx, turn := relayLLMTurn(t)
			rows := auxRouteRows(t, f, ctx, turn, "test.auxiliary-route-rows", func() {
				_, err := auxRelayCall(ctx, "compression", auxRelayAttemptsClient(abort.err), 1)
				if !errors.Is(err, abort.err) {
					t.Fatalf("abort outcome %v", err)
				}
			})
			relayLLMEqual(t, auxRouteTuples(rows), []string{"cancelled|none|1"})
		})
	}
}

func TestAuxiliaryCallFinishingUnderTheTurnsLiveScopeIsCounted(t *testing.T) {
	f, ctx, turn := relayLLMTurn(t)
	rt, session := relayLLMRuntime(turn), turn.Lease.Session
	var turnWork any
	rows := auxRouteRows(t, f, ctx, turn, "test.auxiliary-route-rows", func() {
		_, err := auxRelayAuxiliaryCall(ctx, "title_generation", func(ctx context.Context) (any, error) {
			auxSetRelayAuxiliaryRoute(ctx, "openrouter", "m", "chat_completions")
			response, err := auxRelaySyncCompletion(ctx, auxRelayAttemptsClient(auxRelayAnswer("m", "A title")), map[string]any{"model": "m", "messages": []any{}}, "", "", nil)
			if err != nil {
				return nil, err
			}
			handle, err := rt.RunInSession(ctx, session, func(c context.Context) (any, error) {
				return f.relay.Scope.Push(c, "test.turn_work", f.relay.ScopeType.Function, map[string]any{"handle": turn.Handle, "input": map[string]any{}})
			}, false, nil)
			if err != nil {
				return nil, err
			}
			turnWork = handle
			return auxValidateLLMResponse(ctx, response, "title_generation", "", "")
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rt.RunInSession(ctx, session, func(c context.Context) (any, error) {
			return tools.PopRelayScope(c, f.relay, turnWork, map[string]any{}, nil, nil)
		}, false, nil); err != nil {
			t.Fatal(err)
		}
		tools.RelayCoordinator.EndTurn(ctx, turn, "success")
	})
	relayLLMEqual(t, auxRouteTuples(rows), []string{"success|none|1"})
}

func TestAsyncAuxiliaryAttemptUsesInheritedRelayAdapter(t *testing.T) {
	auxSwapRelaySeams(t)
	var captured map[string]any
	relayExecuteCurrentAsyncFn = func(ctx context.Context, request map[string]any, callback func(context.Context, map[string]any) (any, error), name, modelName string, metadata map[string]any, deferLogical bool) (any, error) {
		captured = auxRelayAttemptRecord(name, modelName, metadata, deferLogical)
		return callback(ctx, request)
	}
	completions := auxRecordLogicalCompletions()
	client := &auxCallFakeClient{}
	client.create = func(_ context.Context, kwargs map[string]any) (any, error) {
		return map[string]any{"request": kwargs, "choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}}}, nil
	}
	result, err := auxRelayAuxiliaryCall(context.Background(), "title_generation", func(ctx context.Context) (any, error) {
		auxSetRelayAuxiliaryRoute(ctx, "anthropic", "claude-test", "chat_completions")
		resp, err := auxRelayAsyncCompletion(ctx, client, map[string]any{"model": "claude-test", "messages": []any{}}, "", "", nil)
		if err != nil {
			return nil, err
		}
		return auxValidateLLMResponse(ctx, resp, "title_generation", "", "")
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := result.(map[string]any)["request"].(map[string]any)
	metadata, _ := captured["metadata"].(map[string]any)
	if request["model"] != "claude-test" || captured["name"] != "anthropic" || metadata["call_role"] != "auxiliary:title_generation" || captured["defer_logical_completion"] != true {
		t.Fatalf("result %v captured %v", result, captured)
	}
	relayLLMEqual(t, *completions, [][]any{{metadata["api_request_id"], "success", "claude-test", "anthropic", "", "none"}})
}

// Provider stream yielding one chunk and then failing
type auxRelayPartialStream struct {
	chunk  any
	err    error
	served bool
}

func (s *auxRelayPartialStream) Next(context.Context) (any, error) {
	if !s.served {
		s.served = true
		return s.chunk, nil
	}
	return nil, s.err
}

func (s *auxRelayPartialStream) Close() error { return nil }

func TestPartialAuxiliaryStreamFailureClosesBeforeRecovery(t *testing.T) {
	f, ctx, turn := relayLLMTurn(t)
	consumer := "test.partial-auxiliary-stream-failure"
	if err := turn.Lease.Host.RetainManagedExecution(consumer); err != nil {
		t.Fatal(err)
	}
	defer turn.Lease.Host.ReleaseManagedExecution(consumer)
	before := len(f.popOutputs())
	providerErr := errors.New("stream failed")
	partialChunk := map[string]any{"model": "test-model", "usage": nil,
		"choices": []any{map[string]any{"delta": map[string]any{"content": "partial", "tool_calls": nil}, "finish_reason": nil}}}
	streamClient := &auxCallFakeClient{}
	streamClient.create = func(context.Context, map[string]any) (any, error) {
		return &auxRelayPartialStream{chunk: partialChunk, err: providerErr}, nil
	}
	recoveryClient := auxRelayAttemptsClient(auxCallOK("recovered"))
	started, err := auxRelayAuxiliaryCall(ctx, "moa", func(ctx context.Context) (any, error) {
		auxSetRelayAuxiliaryRoute(ctx, "openrouter", "test-model", "chat_completions")
		return auxRelaySyncStream(ctx, streamClient, map[string]any{"model": "test-model", "messages": []any{}, "stream": true}, "", "")
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, ok := started.(interface {
		Next(context.Context) (any, error)
	})
	if !ok {
		t.Fatalf("stream %T", started)
	}
	first, err := stream.Next(ctx)
	if err != nil || !auxSameMap(first, partialChunk) {
		t.Fatalf("first chunk %v %v", first, err)
	}
	if _, err := stream.Next(ctx); !errors.Is(err, providerErr) {
		t.Fatalf("second chunk error %v", err)
	}
	failed := map[string]any{"error_class": "unknown", "model": "test-model", "outcome": "failed", "provider": "openrouter"}
	relayLLMEqual(t, auxRelayPopOutputs(f, before), []any{failed})
	relayLLMEqual(t, relayLLMLogicalCalls(turn), map[string]any{})
	result, err := auxRelayAuxiliaryCall(ctx, "moa", func(ctx context.Context) (any, error) {
		auxSetRelayAuxiliaryRoute(ctx, "openrouter", "test-model", "chat_completions")
		return auxRelayValidatedCompletion(ctx, recoveryClient, map[string]any{"model": "test-model", "messages": []any{}}, "moa")
	})
	if err != nil || ExtractContentOrReasoning(result, -1) != "recovered" {
		t.Fatalf("recovery %v %v", result, err)
	}
	relayLLMEqual(t, auxRelayPopOutputs(f, before), []any{failed,
		map[string]any{"error_class": "none", "model": "test-model", "outcome": "success", "provider": "openrouter"}})
	relayLLMEqual(t, relayLLMLogicalCalls(turn), map[string]any{})
}

// Completed aggregator response an Anthropic protocol client returns for stream true
func auxRelayCompletedAggregate() map[string]any {
	return map[string]any{"model": "kimi-k3",
		"choices": []any{map[string]any{"message": map[string]any{"content": "aggregated"}, "finish_reason": "stop"}}}
}

func TestAuxiliaryStreamUnwrapsCompletedResponse(t *testing.T) {
	_, ctx, _ := relayLLMTurn(t)
	completed := auxRelayCompletedAggregate()
	client := auxRelayAttemptsClient(completed)
	result, err := auxRelayAuxiliaryCall(ctx, "moa_aggregator", func(ctx context.Context) (any, error) {
		auxSetRelayAuxiliaryRoute(ctx, "kimi-coding", "kimi-k3", "chat_completions")
		return auxRelaySyncStream(ctx, client, map[string]any{"model": "kimi-k3", "messages": []any{}, "stream": true}, "", "")
	})
	if err != nil || !auxSameMap(result, completed) {
		t.Fatalf("result %v %v", result, err)
	}
}

func TestCallLlmStreamUnwrapsCompletedResponse(t *testing.T) {
	_, ctx, _ := relayLLMTurn(t)
	completed := auxRelayCompletedAggregate()
	client := &auxCallFakeClient{base: "https://api.kimi.com/coding/v1"}
	client.create = func(context.Context, map[string]any) (any, error) { return completed, nil }
	prev := auxGetCachedClientFn
	auxGetCachedClientFn = func(context.Context, auxCachedClientRequest) (AuxClient, string, error) {
		return client, "kimi-k3", nil
	}
	t.Cleanup(func() { auxGetCachedClientFn = prev })
	result, err := CallLLM(ctx, map[string]any{"task": "moa_aggregator", "provider": "kimi-coding", "model": "kimi-k3", "api_key": "sk-test",
		"messages": []any{map[string]any{"role": "user", "content": "q"}}, "stream": true, "stream_options": map[string]any{"include_usage": true}})
	if err != nil || !auxSameMap(result, completed) {
		t.Fatalf("result %v %v", result, err)
	}
	client.mu.Lock()
	captured := client.sent[len(client.sent)-1]
	client.mu.Unlock()
	if captured["stream"] != true || !reflect.DeepEqual(captured["stream_options"], map[string]any{"include_usage": true}) {
		t.Fatalf("captured %v", captured)
	}
}

// The hermes _MESSAGES fixture
func auxRelayNativeMessages() []any {
	return []any{map[string]any{"role": "system", "content": "advise"}, map[string]any{"role": "user", "content": "hi"}}
}

// Chat client answering advice from gpt-6-astra
func auxRelayChatClient() *auxCallFakeClient {
	return auxRelayAttemptsClient(map[string]any{"model": "gpt-6-astra", "choices": []any{map[string]any{"index": 0,
		"message": map[string]any{"role": "assistant", "content": "advice"}, "finish_reason": "stop"}}})
}

// Content of every message in a sent request
func auxSentMessageContents(kwargs map[string]any) []any {
	var out []any
	v := reflect.ValueOf(kwargs["messages"])
	if v.Kind() != reflect.Slice {
		return out
	}
	for i := 0; i < v.Len(); i++ {
		m := reflect.ValueOf(v.Index(i).Interface())
		if m.Kind() == reflect.Map {
			out = append(out, m.MapIndex(reflect.ValueOf("content")).Interface())
		}
	}
	return out
}

func TestNativeApiModeRequestIsDecodedAsChat(t *testing.T) {
	calls := []struct {
		name string
		call func(ctx context.Context, client AuxClient) (any, error)
	}{
		{"codex_mode_sync", func(ctx context.Context, client AuxClient) (any, error) {
			return auxRelayAuxiliaryCall(ctx, "moa_reference", func(ctx context.Context) (any, error) {
				return auxRelaySyncCompletion(ctx, client, map[string]any{"model": "gpt-6-astra", "messages": auxRelayNativeMessages()}, "openai-codex", "codex_responses", nil)
			})
		}},
		{"codex_mode_async", func(ctx context.Context, client AuxClient) (any, error) {
			return auxRelayAuxiliaryCall(ctx, "moa_reference", func(ctx context.Context) (any, error) {
				return auxRelayAsyncCompletion(ctx, client, map[string]any{"model": "gpt-6-astra", "messages": auxRelayNativeMessages()}, "openai-codex", "codex_responses", nil)
			})
		}},
		{"codex_mode_stream", func(ctx context.Context, client AuxClient) (any, error) {
			return auxRelayAuxiliaryCall(ctx, "moa_reference", func(ctx context.Context) (any, error) {
				return auxRelaySyncStream(ctx, client, map[string]any{"model": "gpt-6-astra", "messages": auxRelayNativeMessages(), "stream": true}, "openai-codex", "codex_responses")
			})
		}},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			_, ctx, turn := relayLLMTurn(t)
			client := auxRelayChatClient()
			if err := turn.Lease.Host.RetainManagedExecution("test.native-api-mode"); err != nil {
				t.Fatal(err)
			}
			result, err := c.call(ctx, client)
			turn.Lease.Host.ReleaseManagedExecution("test.native-api-mode")
			if err != nil || ExtractContentOrReasoning(result, -1) != "advice" {
				t.Fatalf("result %v %v", result, err)
			}
			client.mu.Lock()
			sent := append([]map[string]any(nil), client.sent...)
			client.mu.Unlock()
			var contents [][]any
			for _, kwargs := range sent {
				contents = append(contents, auxSentMessageContents(kwargs))
			}
			relayLLMEqual(t, contents, [][]any{{"advise", "hi"}})
		})
	}
}
