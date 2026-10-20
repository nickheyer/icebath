// Decides whether a tool call may run, asking a person when the rules do not settle it
package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
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

type PolicyFunc func(call protocol.ToolCall) Decision

func (f PolicyFunc) Decide(call protocol.ToolCall) Decision { return f(call) }

// Answers the Ask decision, usually by prompting in the UI
type Approver interface {
	Approve(ctx context.Context, call protocol.ToolCall) (bool, error)
}

type ApproverFunc func(ctx context.Context, call protocol.ToolCall) (bool, error)

func (f ApproverFunc) Approve(ctx context.Context, call protocol.ToolCall) (bool, error) {
	return f(ctx, call)
}

// Policy that answers the same decision for every call
type Static Decision

func (s Static) Decide(protocol.ToolCall) Decision { return Decision(s) }

var (
	AllowAll Policy = Static(Allow)
	DenyAll  Policy = Static(Deny)
	AskAll   Policy = Static(Ask)
)

type strictest []Policy

func (s strictest) Decide(call protocol.ToolCall) Decision {
	out := Allow
	for _, p := range s {
		if d := p.Decide(call); d.rank() > out.rank() {
			out = d
		}
	}
	return out
}

// Combines policies so deny beats ask and ask beats allow, allowing when given none
func Strictest(policies ...Policy) Policy {
	return strictest(policies)
}

// One rule, matching a tool name or every tool with a star and an optional argument test
type Rule struct {
	Tool     string
	When     func(args json.RawMessage) bool
	Decision Decision
}

func (r Rule) matches(call protocol.ToolCall) bool {
	if r.Tool != "*" && r.Tool != call.Name {
		return false
	}
	return r.When == nil || r.When(call.Args)
}

// First matching rule wins, otherwise the default decision, which is Ask when unset
type Rules struct {
	Rules   []Rule
	Default Decision
}

func (r Rules) Decide(call protocol.ToolCall) Decision {
	for _, rule := range r.Rules {
		if rule.matches(call) {
			return rule.Decision
		}
	}
	if r.Default == "" {
		return Ask
	}
	return r.Default
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

// Prompts a person and reports what they chose, including whether to remember it
type Asker interface {
	Ask(ctx context.Context, call protocol.ToolCall) (Verdict, error)
}

type AskerFunc func(ctx context.Context, call protocol.ToolCall) (Verdict, error)

func (f AskerFunc) Ask(ctx context.Context, call protocol.ToolCall) (Verdict, error) {
	return f(ctx, call)
}

// Approver that remembers always and never answers under a key derived from the call
type Memory struct {
	Ask Asker
	// Groups calls that share one remembered answer, the tool name when nil
	Key     func(call protocol.ToolCall) string
	mu      sync.Mutex
	allowed map[string]bool
	denied  map[string]bool
}

func (m *Memory) key(call protocol.ToolCall) string {
	if m.Key != nil {
		return m.Key(call)
	}
	return call.Name
}

// Answers from memory when it can and asks otherwise, learning always and never
func (m *Memory) Approve(ctx context.Context, call protocol.ToolCall) (bool, error) {
	key := m.key(call)
	m.mu.Lock()
	allowed, denied := m.allowed[key], m.denied[key]
	m.mu.Unlock()
	switch {
	case denied:
		return false, &Denied{Call: call, Reason: "never allowed by an earlier answer"}
	case allowed:
		return true, nil
	case m.Ask == nil:
		return false, &Denied{Call: call, Reason: "approval required and no asker is configured"}
	}
	v, err := m.Ask.Ask(ctx, call)
	if err != nil {
		return false, err
	}
	switch v {
	case Always:
		m.Remember(key, true)
	case Never:
		m.Remember(key, false)
	}
	return v.Allowed(), nil
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

func (m *Memory) Forget(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.allowed, key)
	delete(m.denied, key)
}

// Keys remembered as always allowed, sorted
func (m *Memory) Allowed() []string { return m.keys(true) }

// Keys remembered as never allowed, sorted
func (m *Memory) Denied() []string { return m.keys(false) }

func (m *Memory) keys(allow bool) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.denied
	if allow {
		src = m.allowed
	}
	out := make([]string, 0, len(src))
	for k := range src {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Call that was not allowed to run
type Denied struct {
	Call   protocol.ToolCall
	Reason string
}

func (d *Denied) Error() string {
	return fmt.Sprintf("policy: %s denied, %s", d.Call.Name, d.Reason)
}

// One gate decision as it is recorded
type Entry struct {
	Time     time.Time       `json:"time"`
	Tool     string          `json:"tool"`
	CallID   string          `json:"call_id"`
	Args     json.RawMessage `json:"args"`
	Decision Decision        `json:"decision"`
	Allowed  bool            `json:"allowed"`
	Reason   string          `json:"reason"`
}

// Receives every decision a gate makes
type Recorder interface {
	Record(e Entry)
}

type RecorderFunc func(e Entry)

func (f RecorderFunc) Record(e Entry) { f(e) }

type logger struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (l *logger) Record(e Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enc.Encode(e)
}

// Recorder writing one JSON object per line
func Log(w io.Writer) Recorder {
	return &logger{enc: json.NewEncoder(w)}
}

// Policy and approver together, asking for everything without a policy and denying every ask without an approver
type Gate struct {
	Policy   Policy
	Approver Approver
	Recorder Recorder
}

// Returns nil when the call may run and a Denied error otherwise
func (g Gate) Check(ctx context.Context, call protocol.ToolCall) error {
	return g.Resolve(ctx, call, "")
}

// Checks the call with a decision settled ahead of the policy, replacing its allow or ask but never its deny
func (g Gate) Resolve(ctx context.Context, call protocol.ToolCall, settled Decision) error {
	decision, reason, err := g.check(ctx, call, settled)
	if g.Recorder != nil {
		g.Recorder.Record(Entry{Time: time.Now().UTC(), Tool: call.Name, CallID: call.ID, Args: call.Args, Decision: decision, Allowed: err == nil, Reason: reason})
	}
	return err
}

func (g Gate) check(ctx context.Context, call protocol.ToolCall, settled Decision) (Decision, string, error) {
	decision := Ask
	if g.Policy != nil {
		decision = g.Policy.Decide(call)
	}
	by := "policy"
	if settled != "" && decision != Deny {
		decision, by = settled, "settled decision"
	}
	switch decision {
	case Allow:
		return decision, "allowed by " + by, nil
	case Deny:
		d := &Denied{Call: call, Reason: "denied by " + by}
		return decision, d.Reason, d
	case Ask:
		if g.Approver == nil {
			d := &Denied{Call: call, Reason: "approval required and no approver is configured"}
			return decision, d.Reason, d
		}
		ok, err := g.Approver.Approve(ctx, call)
		if err != nil {
			return decision, err.Error(), err
		}
		if !ok {
			d := &Denied{Call: call, Reason: "rejected by the approver"}
			return decision, d.Reason, d
		}
		return decision, "approved", nil
	}
	d := &Denied{Call: call, Reason: fmt.Sprintf("unknown decision %q", decision)}
	return decision, d.Reason, d
}
