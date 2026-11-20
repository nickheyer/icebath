package agent

import (
	"context"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Frozen inline delegation decision and whether it was frozen yet
func (a *AIAgent) DelegationInline() (inline, set bool) {
	return a.delegationInline, a.delegationInlineSet
}

func (a *AIAgent) SetDelegationInline(inline bool) {
	a.delegationInline, a.delegationInlineSet = inline, true
}

// Backend the carried delegate_task was built from and whether one was ever recorded
func (a *AIAgent) PinnedDelegationBackend() (backend string, set bool) {
	return a.delegationBackend, a.delegationBackendSet
}

func (a *AIAgent) setDelegationBackend(backend string) {
	a.delegationBackend, a.delegationBackendSet = backend, true
}

// Delegation rebuild surface over the agent, tool list identity kept while the carried list is unchanged
type delegationAgentView struct {
	a    *AIAgent
	src  unsafe.Pointer
	n    int
	defs []map[string]any
}

func newDelegationAgentView(a *AIAgent) *delegationAgentView { return &delegationAgentView{a: a} }

// Agent the view reads, what the mcp refresh and cache break hooks act on
func (v *delegationAgentView) Unwrap() any { return v.a }

func (v *delegationAgentView) Tools() []map[string]any {
	if v.a.tools == nil {
		v.src, v.n, v.defs = nil, 0, nil
		return nil
	}
	if src := unsafe.Pointer(unsafe.SliceData(v.a.tools)); v.defs == nil || src != v.src || len(v.a.tools) != v.n {
		v.src, v.n, v.defs = src, len(v.a.tools), v.a.ToolDefs()
	}
	return v.defs
}

func (v *delegationAgentView) DelegationBackend() (string, bool) {
	return v.a.PinnedDelegationBackend()
}
func (v *delegationAgentView) SetDelegationBackend(backend string) { v.a.setDelegationBackend(backend) }
func (v *delegationAgentView) TurnOrigin() string                  { return v.a.turnOrigin }
func (v *delegationAgentView) DelegateDepth() int                  { return v.a.delegateDepth }
func (v *delegationAgentView) SessionID() string                   { return v.a.sessionID }
func (v *delegationAgentView) SessionEnv() *tools.Env              { return v.a.env }

// Turn start tool rebuild a delegation backend switch asked for this session, true when the tool list changed
func applyRequestedToolRebuild(ctx context.Context, a *AIAgent) bool {
	return tools.ApplyRequestedToolRebuild(ctx, newDelegationAgentView(a))
}

var (
	_ tools.DelegationInlineAgent  = (*AIAgent)(nil)
	_ tools.DelegationRebuildAgent = (*delegationAgentView)(nil)
)
