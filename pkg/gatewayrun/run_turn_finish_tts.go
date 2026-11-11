package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Evicts the cached agent when a fallback model served a successful run, failed runs left cached
func (r *GatewayRunner) runAgentEvictOnFallback(ctx context.Context, tc *TurnContext) error {
	sessionKey, a, resultForFallback := tc.sessionKey, tc.currentAgent(), tc.result()
	if a == nil || (len(resultForFallback) > 0 && pyrepr.Truthy(resultForFallback["failed"])) {
		return nil
	}
	if !a.ProviderFallbackActive() {
		policy, err := gateway.LocalPolicyForSource(ctx, r, tc.source)
		if err != nil {
			return err
		}
		policyModel := ""
		if policy != nil && policy.Model != nil && *policy.Model != "" {
			authority, err := gateway.ActiveAuthority(ctx, r)
			if err != nil {
				return err
			}
			current, err := policy.CurrentModel(ctx, authority)
			if err != nil {
				return err
			}
			if current != nil {
				policyModel = *current
			}
		}
		cfgModel := r.fallbackBaselineModel(ctx, sessionKey, tc.source, a, policyModel)
		if a.Model() == cfgModel || r.isIntentionalModelSwitch(sessionKey, a, cfgModel) {
			return nil
		}
	}
	a.MarkEvictedOnFallback(sessionKey)
	r.evictCachedAgent(ctx, sessionKey, false, false)
	return nil
}

// Finalizes the streaming TTS consumer on the loop, aborting it when the drain times out
func (r *GatewayRunner) runAgentFinalizeStreamingTTS(tc *TurnContext, adapter gateway.PlatformAdapter) {
	consumer := tc.streamingTTSConsumer()
	if consumer == nil {
		return
	}
	consumer.Finish()
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug(fmt.Sprintf("streaming TTS wait_complete error: %v", rec))
			}
		}()
		consumer.WaitComplete(10.0)
	}()
	if !consumer.Done() {
		consumer.Abort("streaming TTS finalisation timeout")
		consumer.WaitComplete(2.0)
	}
	if consumer.SuppressWholeFile() && adapter != nil {
		var marker any
		if tc.runGeneration != nil {
			marker = *tc.runGeneration
		}
		gateway.MarkStreamingTTSCompletedTurn(adapter, tc.sessionKey, marker)
	}
}
