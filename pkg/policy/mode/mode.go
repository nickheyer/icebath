// Permission modes that relax or tighten a base policy the way agentic harnesses expect
package mode

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

type Mode string

const (
	// Base policy decides everything
	Default Mode = "default"
	// Read only tools follow the base policy and every other tool is denied
	Plan Mode = "plan"
	// File editing tools run without asking unless the base policy denies them
	AcceptEdits Mode = "acceptEdits"
	// Everything runs
	Bypass Mode = "bypassPermissions"
)

// Parses a mode name
func Parse(s string) (Mode, error) {
	switch Mode(s) {
	case Default, Plan, AcceptEdits, Bypass:
		return Mode(s), nil
	}
	return "", fmt.Errorf("mode: unknown mode %q", s)
}

// Tools AcceptEdits lets through without asking
var DefaultEdits = []string{"Write", "Edit"}

// Base policy wrapped by a mode
type Policy struct {
	Mode Mode
	// Consulted when the mode defers, asking for everything when nil
	Base policy.Policy
	// Reports whether a tool only reads, every tool counting as mutating when nil
	ReadOnly func(name string) bool
	// Tool names AcceptEdits allows, DefaultEdits when nil
	Edits []string
}

// Reports read only tools out of a registry by their annotations
func ReadOnlyOf(r *tool.Registry) func(string) bool {
	return func(name string) bool {
		t, ok := r.Lookup(name)
		return ok && tool.Describe(t).ReadOnly
	}
}

func (p Policy) base(call protocol.ToolCall) policy.Decision {
	if p.Base == nil {
		return policy.Ask
	}
	return p.Base.Decide(call)
}

func (p Policy) Decide(call protocol.ToolCall) policy.Decision {
	switch p.Mode {
	case Bypass:
		return policy.Allow
	case Plan:
		if p.ReadOnly != nil && p.ReadOnly(call.Name) {
			return p.base(call)
		}
		return policy.Deny
	case AcceptEdits:
		edits := p.Edits
		if edits == nil {
			edits = DefaultEdits
		}
		for _, name := range edits {
			if name == call.Name {
				if p.base(call) == policy.Deny {
					return policy.Deny
				}
				return policy.Allow
			}
		}
	}
	return p.base(call)
}

// Policy raising a base allow to ask for tools annotated destructive or open world
type Guard struct {
	// Consulted first, asking for everything when nil
	Base policy.Policy
	// Registry whose annotations are read, every tool counting as destructive and open world when nil
	Tools *tool.Registry
}

func (g Guard) Decide(call protocol.ToolCall) policy.Decision {
	base := policy.Ask
	if g.Base != nil {
		base = g.Base.Decide(call)
	}
	if base != policy.Allow {
		return base
	}
	if g.Tools == nil {
		return policy.Ask
	}
	t, ok := g.Tools.Lookup(call.Name)
	if !ok {
		return policy.Ask
	}
	if hints := tool.Describe(t); hints.Destructive || hints.OpenWorld {
		return policy.Ask
	}
	return policy.Allow
}
