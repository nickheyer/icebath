// Decides whether a tool call may run, asking a person when the rules do not settle it
package policy

import (
	"context"
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

type Decision string

const (
	Allow Decision = "allow"
	Deny  Decision = "deny"
	Ask   Decision = "ask"
)

// Orders decisions by strictness, unknown values ranking above deny
func (d Decision) rank() int {
	switch d {
	case Allow:
		return 0
	case Ask:
		return 1
	case Deny:
		return 2
	}
	return 3
}

// Picks the stricter of two decisions, an empty one losing to anything
func Stricter(a, b Decision) Decision {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	case b.rank() > a.rank():
		return b
	}
	return a
}

// Maps a tool call to a decision without side effects
type Policy interface {
	Decide(call protocol.ToolCall) Decision
}

// Policy that can name the rule or layer behind its decision
type Explainer interface {
	Explain(call protocol.ToolCall) (Decision, string)
}

// Decision of p with its reason, which is the word policy for policies that cannot explain
func Explain(p Policy, call protocol.ToolCall) (Decision, string) {
	if e, ok := p.(Explainer); ok {
		return e.Explain(call)
	}
	return p.Decide(call), "policy"
}

// Policy that answers the same decision for every call
type Static Decision

func (s Static) Decide(protocol.ToolCall) Decision { return Decision(s) }

var (
	AllowAll Policy = Static(Allow)
	DenyAll  Policy = Static(Deny)
	AskAll   Policy = Static(Ask)
)

// One thing a call operates on, drawn from its arguments
type Subject struct {
	// What the text is, such as command, path, domain or plain
	Kind string
	Text string
	// Hides further commands behind substitution, so no allow pattern can vouch for it
	Unsafe bool
	// Whole command line that only a wildcard free pattern may match
	Exact bool
}

// Policy that can name the rule an always answer should remember for a call
type Keyer interface {
	Key(call protocol.ToolCall) string
}

// Policy that can list what a call operates on
type Subjecter interface {
	SubjectsOf(call protocol.ToolCall) []Subject
}

// Everything a person needs to decide one call
type Request struct {
	Call protocol.ToolCall
	// Agent that wants to run the call
	Agent string
	// Why the gate is asking rather than deciding
	Reason string
	// Rule text an always or never answer will remember
	Key      string
	Subjects []Subject
	// Definition of the tool when the gate knows its registry
	Tool  *protocol.Tool
	Hints tool.Annotations
}

// Answer a person gives to one approval prompt
type Verdict int

const (
	No Verdict = iota
	Yes
	Never
	Always
)

func (v Verdict) Allowed() bool { return v == Yes || v == Always }

func (v Verdict) String() string {
	switch v {
	case Yes:
		return "yes"
	case Never:
		return "never"
	case Always:
		return "always"
	}
	return "no"
}

// Outcome of an approval with who settled it
type Answer struct {
	Allowed bool
	// Who decided, person or memory
	By string
	// What the person chose when a person decided
	Verdict Verdict
	Reason  string
}

// Prompts a person and reports what they chose, including whether to remember it
type Asker interface {
	Ask(ctx context.Context, req Request) (Verdict, error)
}

// Answers the Ask decision, usually by prompting in the UI
type Approver interface {
	Approve(ctx context.Context, req Request) (Answer, error)
}

// Approver that remembers always and never answers under a key derived from the call
type Memory struct {
	Ask Asker
	// Groups calls that share one remembered answer, the request key or tool name when nil
	Key     func(call protocol.ToolCall) string
	mu      sync.Mutex
	allowed map[string]bool
	denied  map[string]bool
}

func (m *Memory) key(req Request) string {
	if m.Key != nil {
		return m.Key(req.Call)
	}
	if req.Key != "" {
		return req.Key
	}
	return req.Call.Name
}

// Answers from memory when it can and asks otherwise, learning always and never
func (m *Memory) Approve(ctx context.Context, req Request) (Answer, error) {
	key := m.key(req)
	if req.Key == "" {
		req.Key = key
	}
	m.mu.Lock()
	allowed, denied := m.allowed[key], m.denied[key]
	m.mu.Unlock()
	switch {
	case denied:
		return Answer{By: "memory", Reason: "never allowed by an earlier answer"}, &Denied{Call: req.Call, Reason: "never allowed by an earlier answer"}
	case allowed:
		return Answer{Allowed: true, By: "memory", Reason: "always allowed by an earlier answer"}, nil
	case m.Ask == nil:
		return Answer{}, &Denied{Call: req.Call, Reason: "approval required and no asker is configured"}
	}
	v, err := m.Ask.Ask(ctx, req)
	if err != nil {
		return Answer{}, err
	}
	switch v {
	case Always:
		m.Remember(key, true)
	case Never:
		m.Remember(key, false)
	}
	return Answer{Allowed: v.Allowed(), By: "person", Verdict: v}, nil
}

// Stores an answer for a key ahead of any prompt
func (m *Memory) Remember(key string, allow bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.allowed == nil {
		m.allowed, m.denied = map[string]bool{}, map[string]bool{}
	}
	delete(m.allowed, key)
	delete(m.denied, key)
	if allow {
		m.allowed[key] = true
	} else {
		m.denied[key] = true
	}
}

// Call that was not allowed to run
type Denied struct {
	Call   protocol.ToolCall
	Reason string
}

func (d *Denied) Error() string {
	return fmt.Sprintf("policy: %s denied, %s", d.Call.Name, d.Reason)
}

// Registry view a gate uses to describe the tool behind a call
type Lookup interface {
	Lookup(name string) (tool.Tool, bool)
}

type agentKey struct{}

// Carries the name of the agent whose calls are being gated
func WithAgent(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, agentKey{}, name)
}

// Agent name carried by the context, empty when none was set
func AgentFrom(ctx context.Context) string {
	name, _ := ctx.Value(agentKey{}).(string)
	return name
}

// Policy and approver together, asking for everything without a policy and denying every ask without an approver
type Gate struct {
	Policy   Policy
	Approver Approver
	// Describes tools on approval requests when set
	Tools Lookup
	// Called with every request handed to the approver
	OnAsk func(Request)
	// Called with the answer or error the approver gave
	OnAnswer func(Request, Answer, error)
}

// Checks the call with a decision settled ahead of the policy, replacing its allow or ask but never its deny
func (g Gate) Resolve(ctx context.Context, call protocol.ToolCall, settled Decision) error {
	decision, by := Ask, "policy"
	if g.Policy != nil {
		decision, by = Explain(g.Policy, call)
	}
	if settled != "" && decision != Deny {
		decision, by = settled, "settled decision"
	}
	switch decision {
	case Allow:
		return nil
	case Deny:
		return &Denied{Call: call, Reason: "denied by " + by}
	case Ask:
		if g.Approver == nil {
			return &Denied{Call: call, Reason: "approval required and no approver is configured"}
		}
		req := g.request(ctx, call, by)
		if g.OnAsk != nil {
			g.OnAsk(req)
		}
		ans, err := g.Approver.Approve(ctx, req)
		if g.OnAnswer != nil {
			g.OnAnswer(req, ans, err)
		}
		if err != nil {
			return err
		}
		if !ans.Allowed {
			return &Denied{Call: call, Reason: rejected(ans)}
		}
		return nil
	}
	return &Denied{Call: call, Reason: fmt.Sprintf("unknown decision %q", decision)}
}

// Builds the request an approver sees for a call
func (g Gate) request(ctx context.Context, call protocol.ToolCall, reason string) Request {
	req := Request{Call: call, Agent: AgentFrom(ctx), Reason: reason}
	if k, ok := g.Policy.(Keyer); ok {
		req.Key = k.Key(call)
	}
	if s, ok := g.Policy.(Subjecter); ok {
		req.Subjects = s.SubjectsOf(call)
	}
	if g.Tools != nil {
		if t, ok := g.Tools.Lookup(call.Name); ok {
			def := t.Definition()
			req.Tool = &def
			req.Hints = tool.Describe(t)
		}
	}
	return req
}

func rejected(ans Answer) string {
	if ans.By == "" {
		return "rejected by the approver"
	}
	return "rejected by " + ans.By
}
