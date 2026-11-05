package provider

import (
	"errors"
	"net/http"
	"reflect"
	"testing"
)

const auxPortal400 = "Error code: 400 - {'status': 400, 'message': 'This request is not valid. Check the model name and " +
	"other parameters. Additional info: Reasoning is mandatory for this endpoint and cannot be disabled.'}"

const auxTokenplan400 = "Error code: 400 - {'error':{'code':'invalid_parameter_error','param':None," +
	"'message':\"'reasoning_effort' must be one of: 'minimal', 'low', 'medium', 'high', " +
	"'xhigh', 'max', 'ultra'\",'type':'invalid_request_error'}}"

var auxFloorCatalog = []any{
	map[string]any{"id": "openai/gpt-oss-20b", "supported_parameters": []any{"reasoning", "tools"},
		"reasoning": map[string]any{"mandatory": true, "supported_efforts": []any{"high", "medium", "low"}}},
	map[string]any{"id": "x-ai/grok-4.3", "supported_parameters": []any{"reasoning", "tools"},
		"reasoning": map[string]any{"mandatory": false, "supported_efforts": []any{"high", "medium", "low", "none"}}},
}

func auxFreshFloors(t *testing.T) {
	auxResetReasoningFloors()
	t.Cleanup(auxResetReasoningFloors)
}

func auxTitleKwargs(model string) map[string]any {
	return map[string]any{
		"model": model, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"reasoning_effort": "none", "extra_body": map[string]any{"response_format": map[string]any{"type": "json_object"}},
	}
}

func auxFloorStepUp(t *testing.T, provider, baseURL, model, body string) {
	t.Helper()
	ctx := reasoningCapsColdProcess(t)
	reasoningCapsOffline(t)
	first := auxTitleKwargs(model)
	if !IsReasoningRequiredRejection(body) {
		t.Fatalf("400 must classify as a reasoning required rejection")
	}
	retry := WithReasoningFloor(first)
	if retry == nil || retry["reasoning_effort"] != ReasoningFloorEffort || first["reasoning_effort"] != "none" {
		t.Fatalf("retry = %v, first = %v", retry, first)
	}
	if got := retry["extra_body"].(map[string]any)["response_format"]; !reflect.DeepEqual(got, map[string]any{"type": "json_object"}) {
		t.Fatalf("response_format = %v", got)
	}
	if retry["model"] != first["model"] {
		t.Fatalf("model changed %v", retry["model"])
	}
	if err := RememberReasoningFloor(provider, baseURL, first, errors.New(body)); err != nil {
		t.Fatal(err)
	}
	upfront, err := KnownReasoningFloor(ctx, map[string]any{"enabled": false}, provider, baseURL, model, "title_generation")
	if err != nil || !reflect.DeepEqual(upfront, map[string]any{"enabled": true, "effort": ReasoningFloorEffort}) {
		t.Fatalf("upfront = %v, %v", upfront, err)
	}
}

func TestReasoningRequired400StepsEffortUpToTheFloorAndRemembersTheRoute(t *testing.T) {
	auxFreshFloors(t)
	auxFloorStepUp(t, "custom", "http://127.0.0.1:8765/v1", "openai/gpt-6-astra", auxPortal400)
	if !auxReasoningFloored("127.0.0.1:8765", "openai/gpt-6-astra") {
		t.Fatal("route memo is keyed by the endpoint netloc")
	}
}

func TestFieldRejectionStillStripsInsteadOfSteppingUp(t *testing.T) {
	auxFreshFloors(t)
	ctx := reasoningCapsColdProcess(t)
	reasoningCapsOffline(t)
	body := "Error code: 400 - Unrecognized request argument supplied: reasoning_effort"
	if !IsReasoningFieldRejection(body) || IsReasoningRequiredRejection(body) {
		t.Fatal("unknown field wording is a field rejection")
	}
	got, err := KnownReasoningFloor(ctx, map[string]any{"enabled": false}, "custom", "https://relay.example/v1", "openai/gpt-6-astra", "title_generation")
	if err != nil || !reflect.DeepEqual(got, map[string]any{"enabled": false}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if len(auxFlooredRoutes) != 0 {
		t.Fatalf("nothing remembered, got %v", auxFlooredRoutes)
	}
}

func TestTokenplanLevelRejectionIsAReasoningRejectionNotAFieldRejection(t *testing.T) {
	if !IsReasoningRequiredRejection(auxTokenplan400) {
		t.Fatal("closed level set must classify as required")
	}
	if IsReasoningFieldRejection(auxTokenplan400) {
		t.Fatal("closed level set must not classify as a field rejection")
	}
}

func TestTokenplanLevelRejectionStepsUpToTheFloorAndRemembersTheRoute(t *testing.T) {
	auxFreshFloors(t)
	auxFloorStepUp(t, "custom", "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1", "deepseek-v4.1-flash", auxTokenplan400)
}

func TestWithReasoningFloorLiftsEveryDisabledEncoding(t *testing.T) {
	floor := map[string]any{"enabled": true, "effort": ReasoningFloorEffort}
	if got := WithReasoningFloor(map[string]any{"model": "m", "reasoning_effort": "medium"}); got != nil {
		t.Fatalf("unchanged request yields nil, got %v", got)
	}
	got := WithReasoningFloor(map[string]any{"reasoning_effort": nil, "_reasoning_config": map[string]any{"enabled": false},
		"extra_body": map[string]any{"reasoning": map[string]any{"effort": " Off "}, "keep": 1}})
	if got["reasoning_effort"] != ReasoningFloorEffort || !reflect.DeepEqual(got["_reasoning_config"], floor) {
		t.Fatalf("got %v", got)
	}
	if !reflect.DeepEqual(got["extra_body"], map[string]any{"reasoning": floor, "keep": 1}) {
		t.Fatalf("extra_body = %v", got["extra_body"])
	}
	if got := WithReasoningFloor(map[string]any{"extra_body": map[string]any{"reasoning": map[string]any{"enabled": 0}}}); got != nil {
		t.Fatalf("only a literal false disables, got %v", got)
	}
	if !reflect.DeepEqual(FloorReasoningConfig(map[string]any{"enabled": true}), map[string]any{"enabled": true}) {
		t.Fatal("enabled config passes through")
	}
}

func TestCatalogMandatoryModelGetsTheFloorOnTheFirstRequest(t *testing.T) {
	auxFreshFloors(t)
	ctx := reasoningCapsColdProcess(t)
	reasoningCapsOffline(t)
	if !seedReasoningCaps(openRouterCatalogURL, auxFloorCatalog) {
		t.Fatal("seed failed")
	}
	floor := map[string]any{"enabled": true, "effort": ReasoningFloorEffort}
	if got, _ := KnownReasoningFloor(ctx, map[string]any{"enabled": false}, "openrouter", "", "openai/gpt-oss-20b", "title_generation"); !reflect.DeepEqual(got, floor) {
		t.Fatalf("mandatory model got %v", got)
	}
	if got, _ := KnownReasoningFloor(ctx, map[string]any{"enabled": false}, "openrouter", "", "x-ai/grok-4.3", "title_generation"); !reflect.DeepEqual(got, map[string]any{"enabled": false}) {
		t.Fatalf("optional model got %v", got)
	}
}

func TestColdCatalogIsWarmedSoTheNextProcessStartsAtTheFloor(t *testing.T) {
	auxFreshFloors(t)
	ctx := reasoningCapsColdProcess(t)
	var seen []*http.Request
	reasoningCapsServe(map[string]any{"data": auxFloorCatalog}, &seen)
	warmed := 0
	reasoningCapsSpawn = func(refresh func()) { warmed++; refresh() }
	got, err := KnownReasoningFloor(ctx, map[string]any{"enabled": false}, "", "https://openrouter.ai/api/v1", "openai/gpt-oss-20b", "title_generation")
	if err != nil || !reflect.DeepEqual(got, map[string]any{"enabled": false}) {
		t.Fatalf("cold first request keeps its disable, got %v, %v", got, err)
	}
	if warmed == 0 || len(seen) == 0 {
		t.Fatal("a cold catalog lookup must start the background warm")
	}
	ResetReasoningCapsCatalogs()
	auxResetReasoningFloors()
	reasoningCapsOffline(t)
	got, err = KnownReasoningFloor(ctx, map[string]any{"enabled": false}, "", "https://openrouter.ai/api/v1", "openai/gpt-oss-20b", "title_generation")
	if err != nil || !reflect.DeepEqual(got, map[string]any{"enabled": true, "effort": ReasoningFloorEffort}) {
		t.Fatalf("next process starts at the floor, got %v, %v", got, err)
	}
}
