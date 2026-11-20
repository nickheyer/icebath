package provider

import (
	"strings"
	"testing"
)

func TestIsStructuredOutputRejectionDoesNotMatchNon400Statuses(t *testing.T) {
	if auxIsStructuredOutputRejection(auxExc("output_config: Extra inputs are not permitted", 500)) {
		t.Fatal("a 500 carrying the schema wording is not a structured output rejection")
	}
}

// One structured output rejection then a dummy answer, asserting the single retry without response_format
func auxRetriesOnceWithoutResponseFormatCase(t *testing.T, asyncMode bool, message string) {
	t.Helper()
	ctx, client := auxTemperatureFixture(t, auxTaskRoute{Provider: "openai-codex", Model: "gpt-5.5"}, "https://api.openai.com/v1",
		[]auxScriptedResult{{nil, auxExc(message, 400)}, {auxDummyResponse(), nil}})
	call := CallLLM
	if asyncMode {
		call = AsyncCallLLM
	}
	result, err := call(ctx, map[string]any{"task": "title_generation", "messages": auxConcurrencyMessages("hi"), "max_tokens": 64,
		"extra_body": map[string]any{"response_format": auxTitleResponseFormat()}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := result.(map[string]any); got["ok"] != true {
		t.Fatalf("result %v", result)
	}
	if n := client.callCount(); n != 2 {
		t.Fatalf("create called %d times", n)
	}
	first, retry := client.calls[0], client.calls[1]
	if _, ok := auxExtraBody(first)["response_format"]; !ok {
		t.Fatalf("first call lost response_format %v", first)
	}
	if _, ok := auxExtraBody(retry)["response_format"]; ok {
		t.Fatalf("retry kept response_format in extra_body %v", retry)
	}
	if _, ok := retry["response_format"]; ok {
		t.Fatalf("retry kept top level response_format %v", retry)
	}
	if retry["model"] != first["model"] {
		t.Fatalf("retry model %v first %v", retry["model"], first["model"])
	}
}

// A 400 that is not a structured output rejection surfaces after exactly one create
func auxUnrelated400Case(t *testing.T, asyncMode bool, message string, extraBody map[string]any) {
	t.Helper()
	ctx, client := auxTemperatureFixture(t, auxTaskRoute{Provider: "openai-codex", Model: "gpt-5.5"}, "https://api.openai.com/v1",
		[]auxScriptedResult{{nil, auxExc(message, 400)}})
	auxSwapPaymentFallbackNone(t)
	call := CallLLM
	if asyncMode {
		call = AsyncCallLLM
	}
	kwargs := map[string]any{"task": "title_generation", "messages": auxConcurrencyMessages("x"), "max_tokens": 64}
	if extraBody != nil {
		kwargs["extra_body"] = extraBody
	}
	_, err := call(ctx, kwargs)
	if err == nil || !strings.Contains(err.Error(), message) {
		t.Fatalf("error %v", err)
	}
	if n := client.callCount(); n != 1 {
		t.Fatalf("create called %d times", n)
	}
}

func TestCallLlmStructuredOutputRetryRetriesOnceWithoutResponseFormat(t *testing.T) {
	for _, message := range []string{
		"Error code: 400 - guided_grammar has compile_grammar_error: No module named 'xgrammar'",
		"HTTP 400: This response_format type is unavailable now",
		"HTTP 400: output_config: Extra inputs are not permitted",
	} {
		auxRetriesOnceWithoutResponseFormatCase(t, false, message)
	}
}

func TestCallLlmStructuredOutputRetryUnrelated400DoesNotStripResponseFormat(t *testing.T) {
	auxUnrelated400Case(t, false, "HTTP 400: Invalid value: 'tool'. Supported values are: 'assistant'", map[string]any{"response_format": auxTitleResponseFormat()})
}

func TestCallLlmStructuredOutputRetryNoRetryWhenNoResponseFormatWasSent(t *testing.T) {
	auxUnrelated400Case(t, false, "HTTP 400: output_config: Extra inputs are not permitted", nil)
}

func TestAsyncCallLlmStructuredOutputRetryAsyncRetriesOnceWithoutResponseFormat(t *testing.T) {
	auxRetriesOnceWithoutResponseFormatCase(t, true, "Error code: 400 - guided_grammar has compile_grammar_error: No module named 'xgrammar'")
}

func TestAsyncCallLlmStructuredOutputRetryAsyncUnrelated400DoesNotRetry(t *testing.T) {
	auxUnrelated400Case(t, true, "HTTP 400: Invalid value: 'tool'", map[string]any{"response_format": auxTitleResponseFormat()})
}
