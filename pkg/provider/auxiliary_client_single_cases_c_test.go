package provider

import (
	"context"
	"testing"
)

func TestVisionFallbackSkipsTextOnlyMainModel(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	prevProvider, prevModel, prevVision, prevResolve := auxReadMainProvider, auxReadMainModel, auxMainModelSupportsVision, auxResolveProviderClientFn
	t.Cleanup(func() {
		auxReadMainProvider, auxReadMainModel, auxMainModelSupportsVision, auxResolveProviderClientFn = prevProvider, prevModel, prevVision, prevResolve
	})
	auxReadMainProvider = func(context.Context) string { return "zai" }
	auxReadMainModel = func(context.Context) string { return "glm-5.3" }
	auxMainModelSupportsVision = func(context.Context, string, string) bool { return false }
	auxResolveProviderClientFn = func(context.Context, string, string, ProviderClientOptions) (AuxClient, string, error) {
		t.Fatal("must not build a client for a text-only model")
		return nil, "", nil
	}
	client, model, label, err := auxTryMainAgentModelFallback(ctx, "nous", "vision", "rate limit", "", "", FailureScope(""))
	if err != nil || client != nil || model != "" || label != "" {
		t.Fatalf("got (%v, %q, %q, %v)", client, model, label, err)
	}
}

func TestRateLimitAfterParameterStripFallsThroughToLaterRungs(t *testing.T) {
	auxFreshFloors(t)
	rateLimited := &AuxSDKError{Class: "RateLimitError", StatusCode: 429, Message: "Error code: 429 - Rate limit exceeded",
		Body: map[string]any{"error": map[string]any{"message": "Rate limit exceeded"}}}
	client := auxScriptedCallClient("https://api.example/v1", "k", rateLimited)
	route := auxLadderRoute{Client: client, Task: "title_generation"}
	kwargs := map[string]any{"model": "m", "messages": []any{}, "max_tokens": 64}
	firstErr := &AuxSDKError{StatusCode: 400, Message: "Error code: 400 - Unsupported parameter: 'max_tokens' is not supported with this model."}
	var finalKwargs map[string]any
	var narrowed error
	resp, raised := auxDriveLadder(context.Background(), func(yield auxLadderYield) (any, error) {
		resp, n, kw, raised := auxLadderParameterRungs(yield, firstErr, route, kwargs, 64)
		finalKwargs, narrowed = kw, n
		return resp, raised
	}, func(ctx context.Context, step auxLadderStep) (any, error) {
		return step.Args[0].(AuxClient).CreateChatCompletion(ctx, step.Args[1].(map[string]any))
	})
	if raised != nil || resp != nil || narrowed != error(rateLimited) {
		t.Fatalf("ladder (%v, %v) narrowed %v", resp, raised, narrowed)
	}
	if _, ok := finalKwargs["max_tokens"]; ok {
		t.Fatalf("stripped kwargs still carry max_tokens %v", finalKwargs)
	}
}
