package provider

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type auxFiredHook struct {
	name    string
	payload map[string]any
}

// Subscribes a recorder to the auxiliary and turn request hooks through the lifecycle seams
func auxRecordHooks(t *testing.T, invoke func(name string, payload map[string]any)) *[]auxFiredHook {
	t.Helper()
	fired := &[]auxFiredHook{}
	prevHas, prevInvoke, prevTurn := auxHasHookFn, auxInvokeHookFn, auxCurrentTurnIdentityFn
	auxHasHookFn = func(context.Context, string) bool { return true }
	auxInvokeHookFn = func(_ context.Context, name string, payload map[string]any) []any {
		*fired = append(*fired, auxFiredHook{name, payload})
		if invoke != nil {
			invoke(name, payload)
		}
		return nil
	}
	auxCurrentTurnIdentityFn = func(context.Context) (string, string, string, string, bool) { return "", "", "", "", false }
	t.Cleanup(func() { auxHasHookFn, auxInvokeHookFn, auxCurrentTurnIdentityFn = prevHas, prevInvoke, prevTurn })
	return fired
}

func auxHookTitleResponse() map[string]any {
	return map[string]any{
		"model":   "mock-model",
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "A title", "tool_calls": nil}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
	}
}

func auxHookArgs(task string, kwargs map[string]any) auxCallHookArgs {
	return auxCallHookArgs{AuxTask: task, Metadata: map[string]any{"api_request_id": "aux-1234", "retry_count": 0},
		Client: &auxProbeClient{baseURL: "https://openrouter.ai/api/v1"}, Kwargs: kwargs,
		Provider: "openrouter", Model: "mock-model", APIMode: "chat_completions"}
}

func TestAuxiliaryHooksEmitThePairWithTheRequestAndReplyView(t *testing.T) {
	fired := auxRecordHooks(t, nil)
	want := auxHookTitleResponse()
	messages := []any{map[string]any{"role": "user", "content": "hello"}}
	kwargs := map[string]any{"model": "mock-model", "messages": messages, "timeout": 30.0}
	got, err := runWithAuxHooks(context.Background(), func() (any, error) { return want, nil }, auxHookArgs("title_generation", kwargs))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("response %v, %v", got, err)
	}
	if len(*fired) != 2 || (*fired)[0].name != PreAuxiliaryCall || (*fired)[1].name != PostAuxiliaryCall {
		t.Fatalf("fired %v", *fired)
	}
	pre, post := (*fired)[0].payload, (*fired)[1].payload
	if pre["aux_task"] != "title_generation" || post["aux_task"] != "title_generation" {
		t.Fatalf("aux_task %v %v", pre["aux_task"], post["aux_task"])
	}
	if !strings.HasPrefix(pre["api_request_id"].(string), "aux-") || post["api_request_id"] != pre["api_request_id"] {
		t.Fatalf("request ids %v %v", pre["api_request_id"], post["api_request_id"])
	}
	if pre["provider"] != "openrouter" || pre["model"] != "mock-model" || pre["api_call_count"] != 1 || pre["message_count"] != 1 {
		t.Fatalf("pre %v", pre)
	}
	if !reflect.DeepEqual(pre["request_messages"], messages) {
		t.Fatalf("request_messages %v", pre["request_messages"])
	}
	body := pre["request"].(map[string]any)["body"].(map[string]any)
	if !reflect.DeepEqual(body["messages"], messages) {
		t.Fatalf("request body %v", body)
	}
	if _, ok := body["timeout"]; ok {
		t.Fatal("timeout is not part of the request body")
	}
	if post["finish_reason"] != "stop" || post["error"] != nil {
		t.Fatalf("post %v", post)
	}
	usage := post["usage"].(map[string]any)
	if usage["input_tokens"] != 10 || usage["output_tokens"] != 5 {
		t.Fatalf("usage %v", usage)
	}
	if post["response"].(map[string]any)["assistant_message"].(map[string]any)["content"] != "A title" {
		t.Fatalf("response %v", post["response"])
	}
	if post["assistant_content_chars"] != 7 || post["assistant_tool_call_count"] != 0 {
		t.Fatalf("counts %v %v", post["assistant_content_chars"], post["assistant_tool_call_count"])
	}
}

func TestAuxiliaryHooksRaisingSubscriberDoesNotBreakTheCall(t *testing.T) {
	auxRecordHooks(t, func(string, map[string]any) { panic("observer exploded") })
	logs := auxCaptureLogs(t)
	want := auxHookTitleResponse()
	got, err := runWithAuxHooks(context.Background(), func() (any, error) { return want, nil },
		auxHookArgs("compression", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "summarize"}}}))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("response %v, %v", got, err)
	}
	if len(logs.matching("plugin hook failed for aux_task=compression; continuing")) != 2 {
		t.Fatalf("records %+v", logs.records)
	}
}

func TestAuxiliaryHooksReportTheErrorAndReturnIt(t *testing.T) {
	fired := auxRecordHooks(t, nil)
	boom := &AuxTimeoutError{Message: "Auxiliary chat stream stalled"}
	_, err := runWithAuxHooks(context.Background(), func() (any, error) { return nil, boom }, auxHookArgs("compression", map[string]any{}))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	post := (*fired)[1].payload
	if post["error"] != "TimeoutError: Auxiliary chat stream stalled" || post["error_type"] != "TimeoutError" || post["response"] != nil {
		t.Fatalf("post %v", post)
	}
}

func TestAuxiliaryHooksStayQuietWithoutSubscribers(t *testing.T) {
	auxRecordHooks(t, nil)
	auxHasHookFn = func(context.Context, string) bool { return false }
	if startAuxCallHooks(context.Background(), auxHookArgs("x", map[string]any{})) != nil {
		t.Fatal("no subscriber means no handle")
	}
}

func TestAuxiliaryHooksBadRetryCountSkipsThePair(t *testing.T) {
	fired := auxRecordHooks(t, nil)
	a := auxHookArgs("x", map[string]any{})
	a.Metadata["retry_count"] = "two"
	if startAuxCallHooks(context.Background(), a) != nil || len(*fired) != 0 {
		t.Fatalf("payload build failure is fail open, fired %v", *fired)
	}
}

func TestAuxiliaryHooksExportedPairFiresPreAndPost(t *testing.T) {
	fired := auxRecordHooks(t, nil)
	hooks := StartAuxCallHooks(context.Background(), auxHookArgs("plugin_side_question", map[string]any{}))
	if hooks == nil || len(*fired) != 1 || (*fired)[0].name != "pre_auxiliary_call" {
		t.Fatalf("pre %v", *fired)
	}
	FinishAuxCallHooks(context.Background(), hooks, nil, &InterruptedError{Message: "side question cancelled"}, nil)
	if len(*fired) != 2 || (*fired)[1].payload["error_type"] != "InterruptedError" {
		t.Fatalf("post %v", *fired)
	}
}
