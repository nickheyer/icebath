package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
)

// Runs fn and swallows a panic it raises, the contextlib suppress of one builder
func suppressPanic(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// End of turn session info for a local viewer, a one turn route evicted and reported as the policy route
func publishSettledInfo(ctx context.Context, t *TurnRunner, a *agent.AIAgent) {
	if a == nil {
		return
	}
	if err := publishSettledInfoBody(ctx, t, a); err != nil {
		runLog().Debug(fmt.Sprintf("settled session info failed: %s", err))
	}
}

func publishSettledInfoBody(ctx context.Context, t *TurnRunner, a *agent.AIAgent) error {
	if t.approvalOwner == nil {
		return &pyTypeError{"cannot unpack non-iterable NoneType object"}
	}
	authority, sessionID := t.approvalOwner.authority, t.approvalOwner.sessionID
	var messageID any
	if t.ctx != nil {
		if _, inboundID, _ := t.runner.turnReplyAnchors(t.ctx); inboundID != nil {
			messageID = *inboundID
		}
	}
	oneTurn := pyTruthy(gateway.CLITurn(ctx, messageID).GetOr("model_override", nil))
	var live *gateway.LiveSession
	if oneTurn {
		if found, ok := authority.Live(sessionID); ok {
			live = found
		}
		if live != nil && t.runner != nil {
			t.runner.evictCachedAgent(ctx, live.Route, false, true)
		}
	}
	local, err := gateway.LocalSessionInfo(ctx, authority, gateway.SessionRef{ProfileID: authority.ProfileID, SessionID: sessionID})
	if err != nil {
		return err
	}
	info := gateway.AgentInventory(ctx, a)
	for _, key := range local.Keys {
		info.Set(key, local.Vals[key])
	}
	if oneTurn && live != nil {
		var policy *gateway.LocalSessionPolicy
		var runner gateway.AuthorityRunner = t.runner
		if authority.Runner != nil {
			runner = authority.Runner
		}
		found, perr := gateway.LocalPolicyForSource(ctx, runner, live.Source)
		if perr != nil {
			runLog().Debug(fmt.Sprintf("one-turn policy route unavailable: %s", perr))
		} else {
			policy = found
		}
		if policy != nil {
			cfg, model, eerr := policy.EffectiveFor(ctx, authority, false)
			if eerr != nil {
				return eerr
			}
			if model != nil {
				info.Set("model", *model)
			} else {
				info.Set("model", nil)
			}
			if section, ok := cfg["model"].(map[string]any); ok {
				if p := section["provider"]; pyTruthy(p) {
					info.Set("provider", p)
				}
			}
		}
	}
	t.publishExecution("session.info", info.Vals)
	return nil
}
