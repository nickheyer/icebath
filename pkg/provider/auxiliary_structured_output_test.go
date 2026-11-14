package provider

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

const auxDeepseek400 = "Error code: 400 - {'error': {'message': 'This response_format type is unavailable now', " +
	"'type': 'invalid_request_error', 'param': None, 'code': 'invalid_request_error'}}"

func auxJSONSchemaFormat() map[string]any {
	return map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "t", "strict": true, "schema": map[string]any{"type": "object"}}}
}

func auxTitleResponseFormat() map[string]any {
	return map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "session_title", "strict": true,
		"schema": map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string"}},
			"required": []any{"title"}, "additionalProperties": false}}}
}

// Fresh rejection memo per test as the agent test directory fixture provides
func auxFreshStructuredMemo(t *testing.T) context.Context {
	t.Helper()
	auxResetStructuredOutputRejections()
	t.Cleanup(auxResetStructuredOutputRejections)
	return modelsTestHome(t)
}

func auxStrippedFormat(t *testing.T, ctx context.Context, provider, model, baseURL string, format map[string]any) bool {
	t.Helper()
	extra := map[string]any{"response_format": format}
	got, err := WithoutUnsupportedResponseFormat(ctx, extra, provider, baseURL, model, "title_generation")
	if err != nil {
		t.Fatal(err)
	}
	rf, kept := got["response_format"]
	if kept && !reflect.DeepEqual(rf, format) {
		t.Fatalf("response_format rewritten to %v", rf)
	}
	return !kept
}

func TestKnownUnsupportedRouteSkipsResponseFormatBeforeFirstRequest(t *testing.T) {
	ctx := auxFreshStructuredMemo(t)
	if !auxStrippedFormat(t, ctx, "deepseek", "deepseek-flash", "https://api.deepseek.com/v1", auxJSONSchemaFormat()) {
		t.Fatal("deepseek json_schema must be dropped")
	}
	if auxStrippedFormat(t, ctx, "deepseek", "deepseek-flash", "https://api.deepseek.com/v1", map[string]any{"type": "json_object"}) {
		t.Fatal("deepseek json_object is kept")
	}
	if auxStrippedFormat(t, ctx, "openai", "gpt-5-mini", "https://api.openai.com/v1", auxJSONSchemaFormat()) {
		t.Fatal("openai json_schema is kept")
	}
	err := RememberStructuredOutputRejection("custom", "http://127.0.0.1:1234/v1",
		map[string]any{"model": "m", "extra_body": map[string]any{"response_format": auxJSONSchemaFormat()}}, errors.New(auxDeepseek400))
	if err != nil {
		t.Fatal(err)
	}
	if !auxStrippedFormat(t, ctx, "custom", "m", "http://127.0.0.1:1234/v1", auxJSONSchemaFormat()) {
		t.Fatal("remembered endpoint drops the field")
	}
	if auxStrippedFormat(t, ctx, "custom", "m", "http://127.0.0.1:11434/v1", auxJSONSchemaFormat()) {
		t.Fatal("another local port keeps the field")
	}
	if !auxStrippedFormat(t, ctx, "custom", "deepseek-chat", "https://api.deepseek.com/v1", auxJSONSchemaFormat()) {
		t.Fatal("custom route on the deepseek host gets the deepseek profile")
	}
}

func TestRejectionMemoIsPerModelAndIgnoresSchemaValidationErrors(t *testing.T) {
	ctx := auxFreshStructuredMemo(t)
	openrouter := "https://openrouter.ai/api/v1"
	err := RememberStructuredOutputRejection("openrouter", openrouter,
		map[string]any{"model": "some/chat-only-model", "extra_body": map[string]any{"response_format": auxJSONSchemaFormat()}},
		errors.New("Error code: 400 - {'error': {'message': 'response_format is not supported by this model'}}"))
	if err != nil {
		t.Fatal(err)
	}
	if !auxStrippedFormat(t, ctx, "openrouter", "some/chat-only-model", openrouter, auxJSONSchemaFormat()) {
		t.Fatal("rejected model drops the field")
	}
	if auxStrippedFormat(t, ctx, "openrouter", "openai/gpt-5-mini", openrouter, auxJSONSchemaFormat()) {
		t.Fatal("sibling model on the host keeps the field")
	}
	validation := errors.New("Error code: 400 - Invalid schema for response_format 'json_schema': In context=(), " +
		"'additionalProperties' is required to be supplied and to be false.")
	if IsCapabilityRejection(validation) {
		t.Fatal("schema validation is not a capability rejection")
	}
	if err := RememberStructuredOutputRejection("openai", "https://api.openai.com/v1",
		map[string]any{"model": "gpt-5-mini", "extra_body": map[string]any{"response_format": auxJSONSchemaFormat()}}, validation); err != nil {
		t.Fatal(err)
	}
	if auxStrippedFormat(t, ctx, "openai", "gpt-5-mini", "https://api.openai.com/v1", auxJSONSchemaFormat()) {
		t.Fatal("schema validation never feeds the memo")
	}
}

func TestWithoutStructuredOutputFormatRemovesExtraBodyEntryAndKeepsSiblings(t *testing.T) {
	kwargs := map[string]any{"model": "m", "extra_body": map[string]any{"response_format": auxTitleResponseFormat(), "metadata": map[string]any{"user_id": "u1"}}}
	result := auxWithoutStructuredOutputFormat(kwargs)
	if result == nil || !reflect.DeepEqual(result["extra_body"], map[string]any{"metadata": map[string]any{"user_id": "u1"}}) {
		t.Fatalf("result = %v", result)
	}
	if _, ok := kwargs["extra_body"].(map[string]any)["response_format"]; !ok {
		t.Fatal("input is not mutated")
	}
}

func TestWithoutStructuredOutputFormatDropsExtraBodyEntirelyWhenItBecomesEmpty(t *testing.T) {
	result := auxWithoutStructuredOutputFormat(map[string]any{"model": "m", "extra_body": map[string]any{"response_format": auxTitleResponseFormat()}})
	if result == nil {
		t.Fatal("changed request expected")
	}
	if _, ok := result["extra_body"]; ok {
		t.Fatalf("extra_body kept: %v", result)
	}
}

func TestWithoutStructuredOutputFormatRemovesTopLevelKwarg(t *testing.T) {
	result := auxWithoutStructuredOutputFormat(map[string]any{"model": "m", "response_format": auxTitleResponseFormat()})
	if result == nil {
		t.Fatal("changed request expected")
	}
	if _, ok := result["response_format"]; ok {
		t.Fatalf("response_format kept: %v", result)
	}
}

func TestWithoutStructuredOutputFormatReturnsNoneWhenNothingToRemove(t *testing.T) {
	if auxWithoutStructuredOutputFormat(map[string]any{"model": "m"}) != nil {
		t.Fatal("nothing to remove")
	}
	if auxWithoutStructuredOutputFormat(map[string]any{"model": "m", "extra_body": map[string]any{"metadata": map[string]any{}}}) != nil {
		t.Fatal("nothing to remove from extra_body")
	}
	if auxWithoutStructuredOutputFormat(map[string]any{"model": "m", "response_format": nil}) != nil {
		t.Fatal("a None response_format is no change")
	}
}

// Response format the chat completions transport put on the wire for one request
func auxTransportResponseFormat(t *testing.T, ctx context.Context, route Route, model string, format map[string]any) (any, error) {
	t.Helper()
	b := newCCRoute(route).build(ctx, &Request{Model: model, Messages: []*state.Message{cm("user", "hi")}, ResponseFormat: format}, wireSettings{})
	if b.err != nil {
		return nil, b.err
	}
	rf, _ := b.body.Get("response_format")
	return rf, nil
}

func TestTransportAndAuxiliaryShareOneRejectionMemo(t *testing.T) {
	ctx := auxFreshStructuredMemo(t)
	local := Route{Provider: "custom", BaseURL: "http://127.0.0.1:1234/v1", Model: "m", Mode: ModeChatCompletions}
	if rf, err := auxTransportResponseFormat(t, ctx, local, "m", auxJSONSchemaFormat()); err != nil || rf == nil {
		t.Fatalf("unrejected route must carry response_format, got %v %v", rf, err)
	}
	err := RememberStructuredOutputRejection("custom", "http://127.0.0.1:1234/v1",
		map[string]any{"model": "m", "response_format": auxJSONSchemaFormat()}, errors.New(auxDeepseek400))
	if err != nil {
		t.Fatal(err)
	}
	if rf, err := auxTransportResponseFormat(t, ctx, local, "m", auxJSONSchemaFormat()); err != nil || rf != nil {
		t.Fatalf("transport must read the memo the ladder wrote, got %v %v", rf, err)
	}
	if !auxStrippedFormat(t, ctx, "custom", "m", "http://127.0.0.1:1234/v1", auxJSONSchemaFormat()) {
		t.Fatal("auxiliary path must read the same memo")
	}
	if rf, _ := auxTransportResponseFormat(t, ctx, local, "other-model", auxJSONSchemaFormat()); rf == nil {
		t.Fatal("memo is per model on the transport path")
	}
	otherPort := Route{Provider: "custom", BaseURL: "http://127.0.0.1:1235/v1", Model: "m", Mode: ModeChatCompletions}
	if rf, _ := auxTransportResponseFormat(t, ctx, otherPort, "m", auxJSONSchemaFormat()); rf == nil {
		t.Fatal("memo key is host and port, a different local port is another route")
	}
	deepseek := Route{Provider: "custom", BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-flash", Mode: ModeChatCompletions}
	if rf, _ := auxTransportResponseFormat(t, ctx, deepseek, "deepseek-flash", auxJSONSchemaFormat()); rf != nil {
		t.Fatal("custom route on the deepseek host takes the deepseek profile")
	}
	if rf, _ := auxTransportResponseFormat(t, ctx, deepseek, "deepseek-flash", map[string]any{"type": "json_object"}); rf == nil {
		t.Fatal("json_object stays on deepseek")
	}
	bad := Route{Provider: "custom", BaseURL: "http://[::1/v1", Model: "m", Mode: ModeChatCompletions}
	if _, err := auxTransportResponseFormat(t, ctx, bad, "m", auxJSONSchemaFormat()); err == nil || err.Error() != "Invalid IPv6 URL" {
		t.Fatalf("an unparseable base URL raises as urlparse does, got %v", err)
	}
}
