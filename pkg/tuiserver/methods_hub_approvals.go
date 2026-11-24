package tuiserver

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var hubApprovalsRegistry = &handlerRegistry{}

func init() {
	scopedRPC(hubApprovalsRegistry, tuigateway.ApprovalsSuggest.Name(), 5197, rpcShape{}, approvalsSuggest)
	scopedRPC(hubApprovalsRegistry, tuigateway.ApprovalsAllow.Name(), 5197, rpcShape{}, approvalsAllow)
	scopedRPC(hubApprovalsRegistry, tuigateway.ApprovalsTest.Name(), 5197, rpcShape{}, approvalsTest)
	addLongHandlers(tuigateway.ApprovalsSuggest.Name())
	hubApprovalsRegistry.install()
}

// Tool env of the profile in the context with its config and secrets, the one the approval rules read
func approvalsEnv(ctx context.Context) *tools.Env {
	return tools.EnvForProfileHome(ctx, nil, home.Dir(ctx))
}

// Allowed commands entries proposed from the approvals given in the profile's sessions, the most approved first
func approvalsSuggest(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	p, err := tuigateway.ApprovalsSuggest.Params(raw)
	if err != nil {
		return rpcErr(rid, 4197, err.Error(), nil), nil
	}
	days, minCount, limit := int64(90), p.MinCount, p.Limit
	if p.Days != nil {
		days = max(*p.Days, 0)
	}
	if minCount <= 0 {
		minCount = 2
	}
	if limit <= 0 {
		limit = 20
	}
	records, err := tools.ScanApprovalHistory(state.ResolveDefaultDBPath(ctx), int(days))
	if err != nil {
		return nil, err
	}
	allowed := tools.LoadPermanentAllowlist(approvalsEnv(ctx))
	out := tuigateway.ApprovalsSuggestResult{Days: days, Proposals: []tuigateway.ApprovalProposal{}, Allowed: int64(len(allowed))}
	for _, proposal := range tools.BuildProposals(records, allowed, int(minCount), int(limit)) {
		classes := make([]string, 0, len(proposal.Classes))
		for class := range proposal.Classes {
			classes = append(classes, class)
		}
		sort.Strings(classes)
		out.Proposals = append(out.Proposals, tuigateway.ApprovalProposal{Pattern: proposal.Pattern, Kind: proposal.Kind, Count: int64(proposal.Count),
			Classes: classes, Examples: slices.Clone(proposal.Examples)})
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// Adds patterns to the allowed commands, answering the ones the saved setting holds afterwards
func approvalsAllow(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	p, err := tuigateway.ApprovalsAllow.Params(raw)
	if err != nil {
		return rpcErr(rid, 4197, err.Error(), nil), nil
	}
	var asked []string
	for _, pattern := range p.Patterns {
		if pattern = strings.TrimSpace(pattern); pattern != "" && !slices.Contains(asked, pattern) {
			asked = append(asked, pattern)
		}
	}
	if len(asked) == 0 {
		return rpcErr(rid, 4197, "approvals.allow needs at least one pattern", nil), nil
	}
	proposals, indices := make([]*tools.Proposal, len(asked)), make([]int, len(asked))
	for i, pattern := range asked {
		proposals[i], indices[i] = &tools.Proposal{Pattern: pattern}, i
	}
	if _, err := tools.ApplyProposals(approvalsEnv(ctx), proposals, indices); err != nil {
		return rpcErr(rid, 4198, err.Error(), nil), nil
	}
	saved := settings.AllowedCommands.Strings(ctx)
	out := tuigateway.ApprovalsAllowResult{Added: []string{}, Allowed: int64(len(saved))}
	for _, pattern := range asked {
		if slices.Contains(saved, pattern) {
			out.Added = append(out.Added, pattern)
		}
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// Verdict the approval rules give a command where the profile runs commands, the command never run
func approvalsTest(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	p, err := tuigateway.ApprovalsTest.Params(raw)
	if err != nil {
		return rpcErr(rid, 4197, err.Error(), nil), nil
	}
	command := strings.TrimSpace(p.Command)
	if command == "" {
		return rpcErr(rid, 4197, "approvals.test needs a command", nil), nil
	}
	envType := strings.ToLower(strings.TrimSpace(p.EnvType))
	if envType == "" {
		envType = effectiveTerminalBackend(ctx)
	}
	v := tools.EvaluateCommand(approvalsEnv(ctx), command, envType)
	return ok(rid, tuigateway.Plain(tuigateway.ApprovalsTestResult{Command: v.Command, EnvType: v.EnvType, Verdict: tuigateway.ApprovalVerdict(v.Verdict),
		Rule: v.Rule, Detail: v.Detail, Variants: slices.Clone(v.NormalizedVariants)})), nil
}
