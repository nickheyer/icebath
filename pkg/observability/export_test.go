package observability

import (
	"context"
	"fmt"
	"sort"
	"testing"
)

// Enabled profile home with the fake relay binding for external test files
func DirectRuntimeForTest(t *testing.T) context.Context {
	_, ctx := directRuntime(t)
	return ctx
}

// Dispatches one lifecycle hook with its merged keyword arguments
func InvokeHookForTest(ctx context.Context, name string, kw map[string]any, extra ...map[string]any) {
	invokeHook(ctx, name, kw, extra...)
}

// Lifecycle observers then the hard close of one relay conversation
func FinalizeSessionForTest(ctx context.Context, sessionID string) { finalizeSession(ctx, sessionID) }

// One completed cli turn with its prompt tokens against a 200k window
func AttributionTurnForTest(ctx context.Context, sessionID, taskID, provider, model string, promptTokens int, start map[string]any) {
	attributionTurn(ctx, sessionID, taskID, provider, model, promptTokens, start)
}

// Flushes every live runtime then lists one metric of the context profile as sorted dimension keys and values
func StoredRowsForTest(t *testing.T, ctx context.Context, metric string) []string {
	t.Helper()
	settleStores(t, ctx)
	var out []string
	for _, v := range storedValues(t, ctx, metric) {
		out = append(out, fmt.Sprintf("%s|%d", dimsKey(v.dims), v.value))
	}
	sort.Strings(out)
	return out
}

// One completed user turn on the platform with its todo tool calls
func EngagementTurnForTest(ctx context.Context, sessionID, taskID, platform string, tools int) {
	engTurn(ctx, sessionID, taskID, engTurnOpts{platform: platform, tools: tools})
}

// Reports whether the active runtime holds no lineage, lineage link or session
func RuntimeHoldsNothingForTest(ctx context.Context) bool {
	rt := liveRuntime(ctx)
	rt.lineageMu.Lock()
	lineages := rt.lineages.Len() + rt.lineageOf.Len()
	rt.lineageMu.Unlock()
	rt.sessionsMu.Lock()
	defer rt.sessionsMu.Unlock()
	return lineages == 0 && rt.sessions.Len() == 0
}

// Shuts every runtime down then lists the counter rows the context profile store holds
func CounterRowsAfterShutdownForTest(t *testing.T, ctx context.Context) []CounterRow {
	t.Helper()
	ShutdownRuntimes(ctx)
	return storedRows(t, ctx)
}

// Installed version reader that reports the branch with no commit date until the test ends
func WithVersionBranchForTest(t *testing.T, branch *string) { withVersion(t, nil, branch) }

// Code root reader that reports the directory until the test ends
func WithProjectRootForTest(t *testing.T, root string) { withProjectRoot(t, root) }
