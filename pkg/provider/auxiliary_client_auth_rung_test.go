package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const (
	auxRungModel    = "z-ai/glm-5.3-flash"
	auxRungNousBase = "https://inference-api.nousresearch.com/v1"
)

// Ladder seams kept off the network with the provider fallback rung recorded
func auxHermeticLadder(t *testing.T) *[]error {
	t.Helper()
	calls := &[]error{}
	prevFallback, prevPool, prevPaid := auxLadderProviderFallbackFn, auxRecoverablePoolProviderFn, auxNousFreshPaidAccessFn
	t.Cleanup(func() {
		auxLadderProviderFallbackFn, auxRecoverablePoolProviderFn, auxNousFreshPaidAccessFn = prevFallback, prevPool, prevPaid
	})
	auxLadderProviderFallbackFn = func(_ context.Context, _ auxLadderYield, firstErr error, _ auxLadderRoute) (any, error) {
		*calls = append(*calls, firstErr)
		return "chain-response", nil
	}
	auxRecoverablePoolProviderFn = func(context.Context, string, AuxClient, map[string]any) (string, error) { return "", nil }
	auxNousFreshPaidAccessFn = func(context.Context) bool { return false }
	return calls
}

func TestPostRefreshRetryOwnsTheLadderOutcome(t *testing.T) {
	for _, tc := range []struct {
		rung    string
		succeed bool
	}{{"nous", false}, {"nous", true}, {"provider_credential", false}} {
		t.Run(fmt.Sprintf("test_post_refresh_retry_owns_the_ladder_outcome[%s-%v]", tc.rung, tc.succeed), func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			ctx := context.Background()
			chain := auxHermeticLadder(t)
			expectedStep, expectedBase, provider := "call", auxRungNousBase, "nous"
			if tc.rung == "nous" {
				prev := auxRefreshNousAuxiliaryClientFn
				t.Cleanup(func() { auxRefreshNousAuxiliaryClientFn = prev })
				auxRefreshNousAuxiliaryClientFn = func(context.Context, auxNousRefreshRequest) (AuxClient, string, error) {
					return &auxCallFakeClient{base: auxRungNousBase, key: "sk-test"}, auxRungModel, nil
				}
			} else {
				prevRoute, prevRefresh, prevEvict := auxAuthRefreshProviderForRouteFn, auxRefreshProviderCredentialsFn, auxEvictCachedClientsFn
				t.Cleanup(func() {
					auxAuthRefreshProviderForRouteFn, auxRefreshProviderCredentialsFn, auxEvictCachedClientsFn = prevRoute, prevRefresh, prevEvict
				})
				auxAuthRefreshProviderForRouteFn = func(context.Context, string, string, string) string { return "codex" }
				auxRefreshProviderCredentialsFn = func(context.Context, string, string) bool { return true }
				auxEvictCachedClientsFn = func(context.Context, string) {}
				expectedStep, expectedBase, provider = "retry_same_provider", "https://openrouter.ai/api/v1", "openrouter"
			}
			authErr := &AuxSDKError{Class: "APIStatusError", Message: "Error code: 401 - Unauthorized", StatusCode: 401}
			ladder := auxRecoveryLadder(ctx, authErr, auxPreparedAuxRequest{
				Client: &auxCallFakeClient{base: auxRungNousBase, key: "sk-test"}, FinalModel: auxRungModel,
				Kwargs: map[string]any{"model": auxRungModel}, ResolvedProvider: provider, ResolvedModel: auxRungModel, BaseInfo: expectedBase,
			}, "compression", false, nil, nil, map[string]string{})
			failure := &AuxSDKError{Class: "APIStatusError", StatusCode: 404, Message: "Error code: 404 - Model '" + auxRungModel +
				"' requires available credits. Your account balance is too low to use paid models."}
			var performed []string
			result, err := auxDriveLadder(ctx, ladder, func(_ context.Context, step auxLadderStep) (any, error) {
				performed = append(performed, step.Kind)
				if tc.succeed {
					return "refreshed-client-response", nil
				}
				return nil, failure
			})
			if tc.succeed {
				if err != nil || result != "refreshed-client-response" || len(*chain) != 0 {
					t.Fatalf("a successful retry must not reach the fallback chain: %v %v %v", result, err, *chain)
				}
				return
			}
			var sdkErr *AuxSDKError
			if errors.As(err, &sdkErr) {
				t.Fatalf("the ladder let %v escape instead of falling through to the configured fallback chain (steps performed: %v)", err, performed)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(performed) != 1 || performed[0] != expectedStep {
				t.Fatalf("the post-refresh retry is the only request %v", performed)
			}
			if len(*chain) == 0 || !strings.Contains((*chain)[0].Error(), "requires available credits") {
				t.Fatalf("the configured fallback chain must be consulted with the retry error %v", *chain)
			}
			if result != "chain-response" {
				t.Fatalf("result %v", result)
			}
		})
	}
}

// Explicit vertex route seams with every gated fallback failing the test when reached
func auxExplicitAuthSeams(t *testing.T) {
	t.Helper()
	prevRoute, prevRefresh, prevPool := auxAuthRefreshProviderForRouteFn, auxRefreshProviderCredentialsFn, auxRecoverablePoolProviderFn
	prevPay, prevMain, prevAgent, prevChain := auxTryPaymentFallbackFn, auxTryMainFallbackChainFn, auxTryMainAgentModelFallbackFn, auxTryConfiguredFallbackChainFn
	t.Cleanup(func() {
		auxAuthRefreshProviderForRouteFn, auxRefreshProviderCredentialsFn, auxRecoverablePoolProviderFn = prevRoute, prevRefresh, prevPool
		auxTryPaymentFallbackFn, auxTryMainFallbackChainFn, auxTryMainAgentModelFallbackFn, auxTryConfiguredFallbackChainFn = prevPay, prevMain, prevAgent, prevChain
	})
	auxAuthRefreshProviderForRouteFn = func(context.Context, string, string, string) string { return "vertex" }
	auxRefreshProviderCredentialsFn = func(context.Context, string, string) bool { return false }
	auxRecoverablePoolProviderFn = func(context.Context, string, AuxClient, map[string]any) (string, error) { return "", nil }
	auxTryPaymentFallbackFn = func(context.Context, string, string, string, string, FailureScope, map[string]any) (AuxClient, string, string, error) {
		t.Error("_try_payment_fallback must stay gated for explicit auth")
		return nil, "", "", nil
	}
	auxTryMainFallbackChainFn = func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		t.Error("_try_main_fallback_chain must stay gated for explicit auth")
		return nil, "", "", nil
	}
	auxTryMainAgentModelFallbackFn = func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		t.Error("explicit auth must not use the main-agent fallback")
		return nil, "", "", nil
	}
}

// Recovery ladder for a 401 on the explicit vertex route
func auxExplicitVertexLadder(ctx context.Context, authErr error, routeInfo map[string]string) func(yield auxLadderYield) (any, error) {
	return auxRecoveryLadder(ctx, authErr, auxPreparedAuxRequest{
		Client: &auxCallFakeClient{base: "https://vertex.example/v1", key: "stale-key"}, FinalModel: auxRungModel,
		Kwargs: map[string]any{"model": auxRungModel}, ResolvedProvider: "vertex", ResolvedModel: auxRungModel, BaseInfo: "https://vertex.example/v1",
	}, "compression", false, nil, nil, routeInfo)
}

func TestExplicitProviderAuthUsesItsConfiguredTaskFallback(t *testing.T) {
	for _, spareSurvives := range []bool{true, false} {
		t.Run(fmt.Sprintf("spare_survives=%v", spareSurvives), func(t *testing.T) {
			ctx := context.Background()
			h := t.TempDir()
			t.Setenv("ICEBATH_HOME", h)
			writeConfig(t, h, map[string]any{"auxiliary": map[string]any{"compression": map[string]any{
				"fallback_chain": []any{map[string]any{"provider": "custom:dead"}, map[string]any{"provider": "custom:backup"}}}}})
			auxExplicitAuthSeams(t)
			deadClient := &auxCallFakeClient{base: "https://vertex.example/v1", key: "stale-key"}
			fallbackClient := &auxCallFakeClient{base: auxRungNousBase, key: "sk-test"}
			type entry struct {
				label  string
				client AuxClient
			}
			chain := []entry{{"fallback_chain[0](custom:dead)", deadClient}}
			if spareSurvives {
				chain = append(chain, entry{"fallback_chain[1](custom:backup)", fallbackClient})
			}
			auxTryConfiguredFallbackChainFn = func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
				if len(chain) == 0 {
					return nil, "", "", nil
				}
				next := chain[0]
				chain = chain[1:]
				return next.client, "fallback-model", next.label, nil
			}
			routeInfo := map[string]string{"provider": "vertex", "model": auxRungModel}
			authErr := &AuxSDKError{Class: "APIStatusError", Message: "Error code: 401 - Unauthorized", StatusCode: 401}
			result, err := auxDriveLadder(ctx, auxExplicitVertexLadder(ctx, authErr, routeInfo), func(_ context.Context, step auxLadderStep) (any, error) {
				if step.Kind != "fallback" {
					t.Fatalf("step %s", step.Kind)
				}
				if step.Args[0] == AuxClient(deadClient) {
					return nil, nil
				}
				if step.Args[0] != AuxClient(fallbackClient) || step.Args[1] != "fallback-model" || step.Args[2] != "fallback_chain[1](custom:backup)" {
					t.Fatalf("fallback step args %v", step.Args)
				}
				return "fallback-response", nil
			})
			if spareSurvives {
				if err != nil || result != "fallback-response" {
					t.Fatalf("result %v err %v", result, err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "Unauthorized") {
					t.Fatalf("an exhausted chain must raise the primary error, got %v %v", result, err)
				}
				if len(routeInfo) != 2 || routeInfo["provider"] != "vertex" || routeInfo["model"] != auxRungModel {
					t.Fatalf("route_info must name the primary route %v", routeInfo)
				}
			}
			if len(chain) != 0 {
				t.Fatalf("chain entries left %v", chain)
			}
		})
	}
}

func TestExplicitProviderAuthNeverUsesAnUnconfiguredFallback(t *testing.T) {
	ctx := context.Background()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeConfig(t, h, map[string]any{})
	auxExplicitAuthSeams(t)
	authErr := &AuxSDKError{Class: "APIStatusError", Message: "Error code: 401 - Unauthorized", StatusCode: 401}
	_, err := auxDriveLadder(ctx, auxExplicitVertexLadder(ctx, authErr, map[string]string{}), func(context.Context, auxLadderStep) (any, error) {
		t.Fatal("no fallback request expected")
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("err %v", err)
	}
}

func TestAuthRefreshRetryFailureReachesTheConfiguredChainOverHTTP(t *testing.T) {
	ctx := context.Background()
	const nousHost = "inference-api.nousresearch.com"
	ShutdownCachedClients(ctx)
	auxClearUnhealthy(t)
	auxRedirectHostToLoopback(t, nousHost)
	t.Setenv("NO_PROXY", "127.0.0.1,localhost,"+nousHost)
	var mu sync.Mutex
	var seen []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		send := func(status int, body any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
		}
		mu.Lock()
		if r.URL.Path == "/v1/chat/completions" {
			seen = append(seen, payload["model"])
		}
		auxCalls := 0
		for _, m := range seen {
			if m == auxRungModel {
				auxCalls++
			}
		}
		mu.Unlock()
		model, ok := payload["model"]
		if !ok || model == nil {
			send(400, map[string]any{"error": map[string]any{"message": "unexpected payload without model"}})
			return
		}
		if model == auxRungModel {
			if auxCalls == 1 {
				send(401, map[string]any{"error": map[string]any{"message": "Unauthorized", "type": "authentication_error"}})
				return
			}
			send(404, map[string]any{"error": map[string]any{"message": "Model '" + auxRungModel + "' requires available credits. Your account " +
				"balance is too low to use paid models.", "type": "invalid_request_error", "code": "insufficient_credits"}})
			return
		}
		send(200, map[string]any{"id": "chatcmpl-ladder", "created": 1, "model": model, "object": "chat.completion",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "The task is complete."}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
	}))
	t.Cleanup(func() {
		ShutdownCachedClients(ctx)
		srv.Close()
	})
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	hostURL, localURL := "http://"+nousHost+":"+port, "http://127.0.0.1:"+port
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("AUX_FB_KEY", "fallback-test-key")
	writeConfig(t, h, map[string]any{
		"model":     map[string]any{"provider": "nous", "default": auxRungModel},
		"providers": map[string]any{"aux-fb": map[string]any{"base_url": localURL + "/v1", "key_env": "AUX_FB_KEY"}},
		"auxiliary": map[string]any{"compression": map[string]any{"provider": "custom", "model": auxRungModel, "base_url": hostURL + "/v1", "timeout": 20,
			"fallback_chain": []any{map[string]any{"provider": "custom:aux-fb", "model": "fallback-model", "base_url": localURL + "/v1", "timeout": 20}}}},
	})
	prevPaid, prevNous := auxNousFreshPaidAccessFn, auxResolveNousRuntimeAPIFn
	t.Cleanup(func() { auxNousFreshPaidAccessFn, auxResolveNousRuntimeAPIFn = prevPaid, prevNous })
	auxNousFreshPaidAccessFn = func(context.Context) bool { return false }
	auxResolveNousRuntimeAPIFn = func(context.Context, bool, string) (string, string, bool) {
		return "fresh-nous-key", hostURL + "/v1", true
	}
	resp, err := CallLLM(ctx, map[string]any{"task": "compression",
		"messages": []any{map[string]any{"role": "user", "content": "Summarize the conversation so far."}}, "timeout": 20.0})
	if err != nil {
		t.Fatal(err)
	}
	if got := auxActualContent(t, resp); got != "The task is complete." {
		t.Fatalf("content %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(seen) != fmt.Sprint([]any{auxRungModel, auxRungModel, "fallback-model"}) {
		t.Fatalf("401, then the refreshed retry fails on credits, then the configured chain: %v", seen)
	}
}

func TestExhaustedLadderRaisesTheNarrowedError(t *testing.T) {
	ctx := context.Background()
	recorded := auxHermeticLadder(t)
	prevRefresh := auxRefreshNousAuxiliaryClientFn
	t.Cleanup(func() { auxRefreshNousAuxiliaryClientFn = prevRefresh })
	auxRefreshNousAuxiliaryClientFn = func(context.Context, auxNousRefreshRequest) (AuxClient, string, error) {
		return &auxCallFakeClient{base: auxRungNousBase, key: "sk-test"}, auxRungModel, nil
	}
	auxLadderProviderFallbackFn = func(_ context.Context, _ auxLadderYield, firstErr error, _ auxLadderRoute) (any, error) {
		*recorded = append(*recorded, firstErr)
		return nil, nil
	}
	authErr := &AuxSDKError{Class: "APIStatusError", Message: "Error code: 401 - Unauthorized", StatusCode: 401}
	ladder := auxRecoveryLadder(ctx, authErr, auxPreparedAuxRequest{
		Client: &auxCallFakeClient{base: auxRungNousBase, key: "sk-test"}, FinalModel: auxRungModel,
		Kwargs: map[string]any{"model": auxRungModel}, ResolvedProvider: "nous", ResolvedModel: auxRungModel, BaseInfo: auxRungNousBase,
	}, "compression", false, nil, nil, map[string]string{})
	failure := &AuxSDKError{Class: "APIStatusError", StatusCode: 404, Message: "Error code: 404 - Model '" + auxRungModel +
		"' requires available credits. Your account balance is too low to use paid models."}
	_, err := auxDriveLadder(ctx, ladder, func(context.Context, auxLadderStep) (any, error) { return nil, failure })
	if err != error(failure) {
		t.Fatalf("the ladder must surface the actionable retry failure, got %v", err)
	}
	if len(*recorded) != 1 || (*recorded)[0] != error(failure) {
		t.Fatalf("provider fallback consulted with %v", *recorded)
	}
}
