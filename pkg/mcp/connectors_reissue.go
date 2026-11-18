package mcp

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Re-runs the named failed rows, the refusal key when the card retry cannot run
func Reissue(ctx context.Context, op *tools.ConnectionOperation, names []string) (string, error) {
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
		if op.StateOf(t) == tools.TargetFailed {
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
	refusal, err := Retry(ctx, op, stale)
	if err != nil {
		return "", err
	}
	if refusal != "" {
		return tools.ConnRefused, nil
	}
	return "", nil
}

// Hands the card's answer to the catalog or MCP module running the operation
func ApplyConnAnswer(ctx context.Context, op *tools.ConnectionOperation, raw string) error {
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
