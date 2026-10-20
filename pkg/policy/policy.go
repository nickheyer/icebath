// Decides whether a tool call may run, asking a person when the rules do not settle it
package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

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

type PolicyFunc func(call protocol.ToolCall) Decision

func (f PolicyFunc) Decide(call protocol.ToolCall) Decision { return f(call) }

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
	d, _ := s.Explain(call)
	return d
}

// Strictest decision across the policies with the reason of the policy that set it
func (s strictest) Explain(call protocol.ToolCall) (Decision, string) {
	out, why := Allow, "policy"
	for _, p := range s {
		if d, r := Explain(p, call); d.rank() > out.rank() {
			out, why = d, r
		}
	}
	return out, why
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
	d, _ := r.Explain(call)
	return d
}

// Decision with the index of the rule that matched or the word default
func (r Rules) Explain(call protocol.ToolCall) (Decision, string) {
	for i, rule := range r.Rules {
		if rule.matches(call) {
			return rule.Decision, fmt.Sprintf("rule %d for %s", i+1, rule.Tool)
		}
	}
	if r.Default == "" {
		return Ask, "default"
	}
	return r.Default, "default"
}

// Policy holder whose policy may be swapped while runs are in flight
type Switch struct {
	v atomic.Pointer[Policy]
}

// Replaces the policy for every later decision
func (s *Switch) Set(p Policy) {
	if p == nil {
		p = AskAll
	}
	s.v.Store(&p)
}

// Policy currently held, asking for everything when none was set
func (s *Switch) Get() Policy {
	if p := s.v.Load(); p != nil {
		return *p
	}
	return AskAll
}

func (s *Switch) Decide(call protocol.ToolCall) Decision { return s.Get().Decide(call) }

func (s *Switch) Explain(call protocol.ToolCall) (Decision, string) { return Explain(s.Get(), call) }

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

// Everything a person or a reviewer needs to judge one call
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
	// Opinion of an automated reviewer that asked for a person
	Review *Review
}

// What a reviewer concluded before handing the call on
type Review struct {
	By       string
	Decision Decision
	Reason   string
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
	// Who decided, such as person, memory or judge
	By string
	// What the person chose when a person decided
	Verdict Verdict
	Reason  string
}

// Prompts a person and reports what they chose, including whether to remember it
type Asker interface {
	Ask(ctx context.Context, req Request) (Verdict, error)
}

type AskerFunc func(ctx context.Context, req Request) (Verdict, error)

func (f AskerFunc) Ask(ctx context.Context, req Request) (Verdict, error) {
	return f(ctx, req)
}

// Answers the Ask decision, usually by prompting in the UI
type Approver interface {
	Approve(ctx context.Context, req Request) (Answer, error)
}

type ApproverFunc func(ctx context.Context, req Request) (Answer, error)

func (f ApproverFunc) Approve(ctx context.Context, req Request) (Answer, error) {
	return f(ctx, req)
}

type askerApprover struct{ a Asker }

func (p askerApprover) Approve(ctx context.Context, req Request) (Answer, error) {
	v, err := p.a.Ask(ctx, req)
	if err != nil {
		return Answer{}, err
	}
	return Answer{Allowed: v.Allowed(), By: "person", Verdict: v}, nil
}

// Approver that asks a person directly without remembering anything
func FromAsker(a Asker) Approver { return askerApprover{a} }

type approverAsker struct{ a Approver }

func (p approverAsker) Ask(ctx context.Context, req Request) (Verdict, error) {
	ans, err := p.a.Approve(ctx, req)
	var denied *Denied
	if err != nil {
		if asDenied(err, &denied) {
			return No, nil
		}
		return No, err
	}
	if ans.Allowed {
		return Yes, nil
	}
	return No, nil
}

// Asker that answers yes or no from an approver so it can sit behind a Memory
func ToAsker(a Approver) Asker { return approverAsker{a} }

func asDenied(err error, target **Denied) bool {
	for err != nil {
		if d, ok := err.(*Denied); ok {
			*target = d
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Approver that remembers always and never answers under a key derived from the call
type Memory struct {
	Ask Asker
	// Groups calls that share one remembered answer, the request key or tool name when nil
	Key func(call protocol.ToolCall) string
	// Called after every remembered or forgotten answer so callers can persist or mirror it
	OnChange func(key string, allow, remembered bool)
	mu       sync.Mutex
	allowed  map[string]bool
	denied   map[string]bool
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
	fn := m.OnChange
	m.mu.Unlock()
	if fn != nil {
		fn(key, allow, true)
	}
}

func (m *Memory) Forget(key string) {
	m.mu.Lock()
	allow := m.allowed[key]
	delete(m.allowed, key)
	delete(m.denied, key)
	fn := m.OnChange
	m.mu.Unlock()
	if fn != nil {
		fn(key, allow, false)
	}
}

// Keys remembered as always allowed, sorted
func (m *Memory) Allowed() []string { return m.keys(true) }

// Keys remembered as never allowed, sorted
func (m *Memory) Denied() []string { return m.keys(false) }

// Every remembered key with whether it is allowed
func (m *Memory) Export() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]bool, len(m.allowed)+len(m.denied))
	for k := range m.allowed {
		out[k] = true
	}
	for k := range m.denied {
		out[k] = false
	}
	return out
}

// Loads remembered keys, keeping any already held
func (m *Memory) Import(keys map[string]bool) {
	for k, allow := range keys {
		m.Remember(k, allow)
	}
}

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
	Agent    string          `json:"agent,omitempty"`
	Tool     string          `json:"tool"`
	CallID   string          `json:"call_id"`
	Args     json.RawMessage `json:"args"`
	Decision Decision        `json:"decision"`
	Allowed  bool            `json:"allowed"`
	Reason   string          `json:"reason"`
	// Who answered an ask, empty when the policy decided alone
	By string `json:"by,omitempty"`
	// What the person chose when a person answered
	Verdict string `json:"verdict,omitempty"`
	// Rule text an always answer remembers
	Key string `json:"key,omitempty"`
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
	Recorder Recorder
	// Describes tools on approval requests when set
	Tools Lookup
	// Called with every request handed to the approver
	OnAsk func(Request)
	// Called with the answer or error the approver gave
	OnAnswer func(Request, Answer, error)
}

// Returns nil when the call may run and a Denied error otherwise
func (g Gate) Check(ctx context.Context, call protocol.ToolCall) error {
	return g.Resolve(ctx, call, "")
}

// Checks the call with a decision settled ahead of the policy, replacing its allow or ask but never its deny
func (g Gate) Resolve(ctx context.Context, call protocol.ToolCall, settled Decision) error {
	e, err := g.check(ctx, call, settled)
	if g.Recorder != nil {
		g.Recorder.Record(e)
	}
	return err
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

func (g Gate) check(ctx context.Context, call protocol.ToolCall, settled Decision) (Entry, error) {
	e := Entry{Time: time.Now().UTC(), Agent: AgentFrom(ctx), Tool: call.Name, CallID: call.ID, Args: call.Args}
	decision, by := Ask, "policy"
	if g.Policy != nil {
		decision, by = Explain(g.Policy, call)
	}
	if settled != "" && decision != Deny {
		decision, by = settled, "settled decision"
	}
	e.Decision = decision
	switch decision {
	case Allow:
		e.Allowed, e.Reason = true, "allowed by "+by
		return e, nil
	case Deny:
		d := &Denied{Call: call, Reason: "denied by " + by}
		e.Reason = d.Reason
		return e, d
	case Ask:
		if g.Approver == nil {
			d := &Denied{Call: call, Reason: "approval required and no approver is configured"}
			e.Reason = d.Reason
			return e, d
		}
		req := g.request(ctx, call, by)
		e.Key = req.Key
		if g.OnAsk != nil {
			g.OnAsk(req)
		}
		ans, err := g.Approver.Approve(ctx, req)
		if g.OnAnswer != nil {
			g.OnAnswer(req, ans, err)
		}
		e.By = ans.By
		if ans.By == "person" {
			e.Verdict = ans.Verdict.String()
		}
		if err != nil {
			e.Reason = err.Error()
			return e, err
		}
		if !ans.Allowed {
			d := &Denied{Call: call, Reason: rejected(ans)}
			e.Reason = d.Reason
			return e, d
		}
		e.Allowed, e.Reason = true, approved(ans)
		return e, nil
	}
	d := &Denied{Call: call, Reason: fmt.Sprintf("unknown decision %q", decision)}
	e.Reason = d.Reason
	return e, d
}

func approved(ans Answer) string {
	if ans.By == "" {
		return "approved"
	}
	return "approved by " + ans.By
}

func rejected(ans Answer) string {
	if ans.By == "" {
		return "rejected by the approver"
	}
	return "rejected by " + ans.By
}
