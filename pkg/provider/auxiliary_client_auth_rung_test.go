package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const (
	auxRungModel = "z-ai/glm-5.3-flash"
	auxRungBase  = "https://inference.example.test/v1"
)

// Ladder seams kept off the network with the provider fallback rung recorded
func auxHermeticLadder(t *testing.T) *[]error {
	t.Helper()
	calls := &[]error{}
	prevFallback, prevPool := auxLadderProviderFallbackFn, auxRecoverablePoolProviderFn
	t.Cleanup(func() {
		auxLadderProviderFallbackFn, auxRecoverablePoolProviderFn = prevFallback, prevPool
	})
	auxLadderProviderFallbackFn = func(_ context.Context, _ auxLadderYield, firstErr error, _ auxLadderRoute) (any, error) {
		*calls = append(*calls, firstErr)
		return "chain-response", nil
	}
	auxRecoverablePoolProviderFn = func(context.Context, string, AuxClient, map[string]any) (string, error) { return "", nil }
	return calls
}

func TestPostRefreshRetryOwnsTheLadderOutcome(t *testing.T) {
	for _, tc := range []struct {
		rung    string
		succeed bool
	}{{"provider_credential", false}, {"provider_credential", true}} {
		t.Run(fmt.Sprintf("test_post_refresh_retry_owns_the_ladder_outcome[%s-%v]", tc.rung, tc.succeed), func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			ctx := context.Background()
			chain := auxHermeticLadder(t)
			prevRoute, prevRefresh, prevEvict := auxAuthRefreshProviderForRouteFn, auxRefreshProviderCredentialsFn, auxEvictCachedClientsFn
			t.Cleanup(func() {
				auxAuthRefreshProviderForRouteFn, auxRefreshProviderCredentialsFn, auxEvictCachedClientsFn = prevRoute, prevRefresh, prevEvict
			})
			auxAuthRefreshProviderForRouteFn = func(context.Context, string, string, string) string { return "codex" }
			auxRefreshProviderCredentialsFn = func(context.Context, string, string) bool { return true }
			auxEvictCachedClientsFn = func(context.Context, string) {}
			expectedStep, expectedBase, provider := "retry_same_provider", "https://openrouter.ai/api/v1", "openrouter"
			authErr := &AuxSDKError{Class: "APIStatusError", Message: "Error code: 401 - Unauthorized", StatusCode: 401}
			ladder := auxRecoveryLadder(ctx, authErr, auxPreparedAuxRequest{
				Client: &auxCallFakeClient{base: auxRungBase, key: "sk-test"}, FinalModel: auxRungModel,
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
			fallbackClient := &auxCallFakeClient{base: auxRungBase, key: "sk-test"}
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
