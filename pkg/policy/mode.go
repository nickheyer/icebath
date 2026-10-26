package policy

import (
	"fmt"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

type Mode string

const (
	// Base policy decides everything
	ModeDefault Mode = "default"
	// Read only tools follow the base policy and every other tool is denied
	ModePlan Mode = "plan"
	// File editing tools run without asking unless the base policy denies them
	ModeAcceptEdits Mode = "acceptEdits"
	// Everything the base policy does not deny runs
	ModeBypass Mode = "bypassPermissions"
)

// Every mode in the order a user cycles through them
func Modes() []Mode { return []Mode{ModeDefault, ModeAcceptEdits, ModePlan, ModeBypass} }

// Mode after m in the cycle order
func (m Mode) Next() Mode {
	all := Modes()
	for i, x := range all {
		if x == m {
			return all[(i+1)%len(all)]
		}
	}
	return ModeDefault
}

// Short label for display
func (m Mode) Label() string {
	switch m {
	case ModePlan:
		return "plan"
	case ModeAcceptEdits:
		return "accept edits"
	case ModeBypass:
		return "bypass"
	}
	return "default"
}

// Parses a mode name
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeDefault, ModePlan, ModeAcceptEdits, ModeBypass:
		return Mode(s), nil
	}
	return "", fmt.Errorf("mode: unknown mode %q", s)
}

// Tools AcceptEdits lets through without asking
var DefaultEdits = []string{"Write", "Edit"}

// Base policy wrapped by a mode
type moded struct {
	Mode Mode
	// Consulted when the mode defers, asking for everything when nil
	Base Policy
	// Reports whether a tool only reads, every tool counting as mutating when nil
	ReadOnly func(name string) bool
}

// Reports read only tools out of a registry by their annotations
func ReadOnlyOf(r *tool.Registry) func(string) bool {
	return func(name string) bool {
		t, ok := r.Lookup(name)
		return ok && tool.Describe(t).ReadOnly
	}
}

func (p moded) base(call protocol.ToolCall) (Decision, string) {
	if p.Base == nil {
		return Ask, "policy"
	}
	return Explain(p.Base, call)
}

func (p moded) Decide(call protocol.ToolCall) Decision {
	d, _ := p.Explain(call)
	return d
}

// Decision with the mode or base rule that produced it
func (p moded) Explain(call protocol.ToolCall) (Decision, string) {
	switch p.Mode {
	case ModeBypass:
		if d, why := p.base(call); d == Deny {
			return Deny, why
		}
		return Allow, "bypass mode"
	case ModePlan:
		if p.ReadOnly != nil && p.ReadOnly(call.Name) {
			return p.base(call)
		}
		return Deny, "plan mode denies tools that change anything"
	case ModeAcceptEdits:
		for _, name := range DefaultEdits {
			if name == call.Name {
				if d, why := p.base(call); d == Deny {
					return Deny, why
				}
				return Allow, "accept edits mode"
			}
		}
	}
	return p.base(call)
}

// Policy whose mode may be changed while runs are in flight
type ModeSwitch struct {
	mode atomic.Pointer[Mode]
	// Consulted when the mode defers, asking for everything when nil
	Base Policy
	// Reports whether a tool only reads
	ReadOnly func(name string) bool
}

// Current mode, Default when none was set
func (s *ModeSwitch) Mode() Mode {
	if m := s.mode.Load(); m != nil {
		return *m
	}
	return ModeDefault
}

// Changes the mode for every later decision
func (s *ModeSwitch) Set(m Mode) { s.mode.Store(&m) }

func (s *ModeSwitch) policy() moded {
	return moded{Mode: s.Mode(), Base: s.Base, ReadOnly: s.ReadOnly}
}

func (s *ModeSwitch) Decide(call protocol.ToolCall) Decision { return s.policy().Decide(call) }

func (s *ModeSwitch) Explain(call protocol.ToolCall) (Decision, string) {
	return s.policy().Explain(call)
}

// Policy raising an allow the base reached by default to ask for tools annotated destructive or open world
type Guard struct {
	// Consulted first, asking for everything when nil
	Base Policy
	// Registry whose annotations are read, every tool counting as destructive and open world when nil
	Tools *tool.Registry
}

func (g Guard) Decide(call protocol.ToolCall) Decision {
	d, _ := g.Explain(call)
	return d
}

// Decision with the guard reason when it raised a default allow to ask
func (g Guard) Explain(call protocol.ToolCall) (Decision, string) {
	base, why := Ask, "policy"
	if g.Base != nil {
		base, why = Explain(g.Base, call)
	}
	if base != Allow || why != "default" && why != "policy" {
		return base, why
	}
	if g.Tools == nil {
		return Ask, "guard asks for tools it cannot describe"
	}
	t, ok := g.Tools.Lookup(call.Name)
	if !ok {
		return Ask, "guard asks for unknown tool " + call.Name
	}
	switch hints := tool.Describe(t); {
	case hints.Destructive:
		return Ask, "guard asks before destructive tool " + call.Name
	case hints.OpenWorld:
		return Ask, "guard asks before open world tool " + call.Name
	}
	return Allow, why
}

// Standard stack, a switchable mode over a guard over the rules, with the guard respecting explicit rules
func Compose(start Mode, rules Policy, tools *tool.Registry) *ModeSwitch {
	s := &ModeSwitch{Base: Guard{Base: rules, Tools: tools}, ReadOnly: ReadOnlyOf(tools)}
	s.mode.Store(&start)
	return s
}
