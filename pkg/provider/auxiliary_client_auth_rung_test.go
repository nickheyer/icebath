package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
