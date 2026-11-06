package mcp

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Managed client seam of a card retry, the tests swap in a scripted gateway
var reissueManagedClient = func(env *tools.Env, createsGuest bool, initiator string, call *tools.CallRef) (tools.ManagedClient, error) {
	client, err := tools.NewManagedClient(env, createsGuest, initiator, call)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// Re-runs the named failed or expired rows, the refusal key when the card retry cannot run
func Reissue(ctx context.Context, env *tools.Env, op *tools.ConnectionOperation, names []string, initiator string) (string, error) {
	targets := make([]*tools.ConnTarget, 0, len(names))
	for _, name := range names {
		t := op.Target(name)
		if t == nil {
			return tools.ConnUnknownTarget, nil
		}
		targets = append(targets, t)
	}
	kinds := map[string]bool{}
	for _, t := range targets {
		kinds[t.Kind] = true
	}
	if len(kinds) != 1 {
		return tools.ConnMixedKinds, nil
	}
	var stale []string
	for _, t := range targets {
		if s := op.StateOf(t); s == tools.TargetFailed || s == tools.TargetExpired {
			stale = append(stale, t.Name)
		}
	}
	if len(stale) != len(targets) {
		return tools.ConnLinkStillValid, nil
	}
	if op.Settled() {
		return tools.ConnSettled, nil
	}
	for _, t := range targets {
		if tools.AllowedTransition(t.Kind, op.StateOf(t), tools.TargetInitiated) == "" {
			return tools.ConnNotAllowed, nil
		}
	}
	if targets[0].Kind == "connector" {
		if initiator == "" {
			initiator = tools.InitiatorCardRetry
		}
		call, _ := op.Call.(*tools.CallRef)
		client, err := reissueManagedClient(env, true, initiator, call)
		if err != nil {
			return "", err
		}
		return "", tools.Mint(ctx, env, client, op, stale, true, tools.ActorUser, "")
	}
	var refusal string
	var err error
	if CatalogOwns(op.OpID) {
		refusal, err = CatalogRetry(ctx, op, stale)
	} else {
		refusal, err = Retry(ctx, op, stale)
	}
	if err != nil {
		return "", err
	}
	if refusal != "" {
		return tools.ConnRefused, nil
	}
	return "", nil
}

// Hands the card's answer to the display, catalog or MCP module running the operation
func ApplyConnAnswer(ctx context.Context, op *tools.ConnectionOperation, raw string) error {
	if op.Display != nil {
		return tools.DisplayApplyAnswer(op, raw)
	}
	if CatalogOwns(op.OpID) {
		return CatalogApplyAnswer(ctx, op, raw)
	}
	return ApplyAnswer(ctx, op, raw)
}

// Applies the card's answer, settles once every row resolved and closes a settled operation
func Respond(ctx context.Context, op *tools.ConnectionOperation, raw string) (bool, error) {
	if err := ApplyConnAnswer(ctx, op, raw); err != nil {
		return false, err
	}
	if !op.Settled() && op.AllResolved() {
		op.Settle(tools.SettleAllResolved)
	}
	if op.Settled() {
		tools.LiveClose(op)
	}
	return op.Settled(), nil
}
