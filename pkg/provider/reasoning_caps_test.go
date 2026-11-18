package provider

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
)

func TestParseOpenRouterReasoningCapabilities(t *testing.T) {
	caps, ok := ParseOpenRouterReasoningCapabilities(map[string]any{
		"id": "nvidia/nemotron-3-ultra", "supported_parameters": []any{"temperature", "tools", "reasoning"},
		"reasoning": map[string]any{"mandatory": false, "supported_efforts": []any{"low", "medium", "high"}},
	})
	if !ok || !caps.SupportsReasoning || caps.Mandatory || !modelsEqual(caps.SupportedEfforts, []string{"low", "medium", "high"}) {
		t.Fatalf("supported with efforts: %+v %v", caps, ok)
	}
	caps, ok = ParseOpenRouterReasoningCapabilities(map[string]any{"supported_parameters": []any{"reasoning", "tools"}, "reasoning": map[string]any{}})
	if !ok || !caps.SupportsReasoning || caps.SupportedEfforts != nil || caps.Mandatory {
		t.Fatalf("every effort accepted when the field is omitted: %+v", caps)
	}
	caps, ok = ParseOpenRouterReasoningCapabilities(map[string]any{"supported_parameters": []any{"reasoning"}})
	if !ok || !caps.SupportsReasoning || caps.SupportedEfforts != nil {
		t.Fatalf("supported_parameters alone is authoritative: %+v", caps)
	}
	caps, _ = ParseOpenRouterReasoningCapabilities(map[string]any{"supported_parameters": []any{"reasoning"}, "reasoning": map[string]any{"mandatory": true, "supported_efforts": nil}})
	if !caps.Mandatory {
		t.Fatal("mandatory flag")
	}
	caps, ok = ParseOpenRouterReasoningCapabilities(map[string]any{"supported_parameters": []any{"temperature", "tools"}, "reasoning": map[string]any{"supported_efforts": []any{"high"}}})
	if !ok || caps.SupportsReasoning || caps.SupportedEfforts != nil {
		t.Fatalf("reasoning object untrusted without the parameter: %+v", caps)
	}
	for _, item := range []any{map[string]any{"id": "x"}, map[string]any{"supported_parameters": "bad"}, "not-a-dict"} {
		if _, ok := ParseOpenRouterReasoningCapabilities(item); ok {
			t.Fatalf("%v is unknown", item)
		}
	}
	caps, _ = ParseOpenRouterReasoningCapabilities(map[string]any{"supported_parameters": []any{"reasoning"}, "reasoning": map[string]any{"supported_efforts": []any{" High ", "high", "LOW", "", 3}}})
	if !modelsEqual(caps.SupportedEfforts, []string{"high", "low", "3"}) {
		t.Fatalf("efforts normalized and deduped: %v", caps.SupportedEfforts)
	}
}

var reasoningCapsTestCatalog = map[string]any{"data": []any{
	map[string]any{"id": "deepseek/deepseek-v4-pro", "supported_parameters": []any{"reasoning", "tools"}, "reasoning": map[string]any{"mandatory": false, "supported_efforts": []any{"xhigh", "high"}}},
	map[string]any{"id": "arcee-ai/trinity-large-thinking", "supported_parameters": []any{"reasoning"}, "reasoning": map[string]any{"mandatory": true}},
}}

// Cold process with no mirror on disk and background warms run inline
func reasoningCapsColdProcess(t *testing.T) context.Context {
	t.Helper()
	ctx := modelsTestHome(t)
	ResetReasoningCapsCatalogs()
	os.Remove(reasoningCapsDiskPath())
	prevSpawn := reasoningCapsSpawn
	reasoningCapsSpawn = func(refresh func()) { refresh() }
	t.Cleanup(func() {
		ResetReasoningCapsCatalogs()
		reasoningCapsSpawn = prevSpawn
	})
	return ctx
}

func reasoningCapsServe(payload map[string]any, seen *[]*http.Request) {
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		if seen != nil {
			*seen = append(*seen, req)
		}
		return modelsJSONResponse(req, 200, payload), nil
	})
}

func reasoningCapsOffline(t *testing.T) {
	modelsTransport = modelsFakeTransport(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("must not fetch: %s", req.URL)
		return nil, nil
	})
}

func TestOpenRouterModelReasoningCapabilities(t *testing.T) {
	ctx := reasoningCapsColdProcess(t)
	reasoningCapsServe(map[string]any{"data": []any{map[string]any{"id": "a/b", "supported_parameters": []any{"reasoning"}}}}, nil)
	if _, ok := OpenRouterModelReasoningCapabilities(ctx, "a/b", 0, true); !ok {
		t.Fatal("fetched catalog answers")
	}
	if _, ok := OpenRouterModelReasoningCapabilities(ctx, "private/custom", 0, false); ok {
		t.Fatal("unlisted model is unknown")
	}
	if _, ok := OpenRouterModelReasoningCapabilities(ctx, "", 0, false); ok {
		t.Fatal("empty model is unknown")
	}
	ctx = reasoningCapsColdProcess(t)
	calls := 0
	modelsTransport = modelsFakeTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("offline")
	})
	if _, ok := OpenRouterModelReasoningCapabilities(ctx, "a/b", 0, true); ok {
		t.Fatal("unreachable is unknown")
	}
	if _, ok := OpenRouterModelReasoningCapabilities(ctx, "a/b", 0, true); ok || calls != 1 {
		t.Fatalf("second call inside the failure TTL must not refetch: calls=%d", calls)
	}
	ctx = reasoningCapsColdProcess(t)
	reasoningCapsOffline(t)
	if _, ok := OpenRouterModelReasoningCapabilities(ctx, "a/b", 0, false); ok {
		t.Fatal("cache only by default never fetches")
	}
}
