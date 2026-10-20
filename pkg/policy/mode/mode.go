// Permission modes that relax or tighten a base policy the way agentic harnesses expect
package mode

import (
	"fmt"
	"sync/atomic"

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
	// Everything the base policy does not deny runs
	Bypass Mode = "bypassPermissions"
)

// Every mode in the order a user cycles through them
func All() []Mode { return []Mode{Default, AcceptEdits, Plan, Bypass} }

// Mode after m in the cycle order
func (m Mode) Next() Mode {
	all := All()
	for i, x := range all {
		if x == m {
			return all[(i+1)%len(all)]
		}
	}
	return Default
}

// Short label for display
func (m Mode) Label() string {
	switch m {
	case Plan:
		return "plan"
	case AcceptEdits:
		return "accept edits"
	case Bypass:
		return "bypass"
	}
	return "default"
}

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

func (p Policy) base(call protocol.ToolCall) (policy.Decision, string) {
	if p.Base == nil {
		return policy.Ask, "policy"
	}
	return policy.Explain(p.Base, call)
}

func (p Policy) Decide(call protocol.ToolCall) policy.Decision {
	d, _ := p.Explain(call)
	return d
}

// Decision with the mode or base rule that produced it
func (p Policy) Explain(call protocol.ToolCall) (policy.Decision, string) {
	switch p.Mode {
	case Bypass:
		if d, why := p.base(call); d == policy.Deny {
			return policy.Deny, why
		}
		return policy.Allow, "bypass mode"
	case Plan:
		if p.ReadOnly != nil && p.ReadOnly(call.Name) {
			return p.base(call)
		}
		return policy.Deny, "plan mode denies tools that change anything"
	case AcceptEdits:
		edits := p.Edits
		if edits == nil {
			edits = DefaultEdits
		}
		for _, name := range edits {
			if name == call.Name {
				if d, why := p.base(call); d == policy.Deny {
					return policy.Deny, why
				}
				return policy.Allow, "accept edits mode"
			}
		}
	}
	return p.base(call)
}

// Policy whose mode may be changed while runs are in flight
type Switch struct {
	mode atomic.Pointer[Mode]
	// Consulted when the mode defers, asking for everything when nil
	Base policy.Policy
	// Reports whether a tool only reads
	ReadOnly func(name string) bool
	// Tool names AcceptEdits allows, DefaultEdits when nil
	Edits []string
	// Called with the new mode after every change
	OnChange func(Mode)
}

// Current mode, Default when none was set
func (s *Switch) Mode() Mode {
	if m := s.mode.Load(); m != nil {
		return *m
	}
	return Default
}

// Changes the mode for every later decision
func (s *Switch) Set(m Mode) {
	s.mode.Store(&m)
	if s.OnChange != nil {
		s.OnChange(m)
	}
}

// Moves to the next mode in the cycle and returns it
func (s *Switch) Cycle() Mode {
	next := s.Mode().Next()
	s.Set(next)
	return next
}

func (s *Switch) policy() Policy {
	return Policy{Mode: s.Mode(), Base: s.Base, ReadOnly: s.ReadOnly, Edits: s.Edits}
}

func (s *Switch) Decide(call protocol.ToolCall) policy.Decision { return s.policy().Decide(call) }

func (s *Switch) Explain(call protocol.ToolCall) (policy.Decision, string) {
	return s.policy().Explain(call)
}

// Policy raising an allow the base reached by default to ask for tools annotated destructive or open world
type Guard struct {
	// Consulted first, asking for everything when nil
	Base policy.Policy
	// Registry whose annotations are read, every tool counting as destructive and open world when nil
	Tools *tool.Registry
}

func (g Guard) Decide(call protocol.ToolCall) policy.Decision {
	d, _ := g.Explain(call)
	return d
}

// Decision with the guard reason when it raised a default allow to ask
func (g Guard) Explain(call protocol.ToolCall) (policy.Decision, string) {
	base, why := policy.Ask, "policy"
	if g.Base != nil {
		base, why = policy.Explain(g.Base, call)
	}
	if base != policy.Allow || why != "default" && why != "policy" {
		return base, why
	}
	if g.Tools == nil {
		return policy.Ask, "guard asks for tools it cannot describe"
	}
	t, ok := g.Tools.Lookup(call.Name)
	if !ok {
		return policy.Ask, "guard asks for unknown tool " + call.Name
	}
	switch hints := tool.Describe(t); {
	case hints.Destructive:
		return policy.Ask, "guard asks before destructive tool " + call.Name
	case hints.OpenWorld:
		return policy.Ask, "guard asks before open world tool " + call.Name
	}
	return policy.Allow, why
}

// Standard stack, a switchable mode over a guard over the rules, with the guard respecting explicit rules
func Compose(start Mode, rules policy.Policy, tools *tool.Registry) *Switch {
	s := &Switch{Base: Guard{Base: rules, Tools: tools}, ReadOnly: ReadOnlyOf(tools)}
	s.mode.Store(&start)
	return s
}
