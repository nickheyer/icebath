package approvals

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	approvalsconfig "github.com/nickheyer/icebath/internal/approvals/config"
	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Answers a prompt takes
const (
	Once    = "once"
	Session = "session"
	Always  = "always"
	Deny    = "deny"
)

// Choice the user can make, with the words the prompt shows for it
type Choice struct {
	Value string
	Label string
}

// Prompt waiting for the user's answer, as the windows of its session show it
type Prompt struct {
	ID string
	// What would run or change, its secrets masked and anything hidden in it made visible
	Command string
	// What it was flagged as
	Description string
	// Keys an answer that lasts allows
	Keys []string
	// Answers it takes, deny last
	Choices []Choice
	// The smart reviewer judged it dangerous, so allowing it overrides that verdict
	SmartDenied bool
	// When it was asked, and when it stops waiting
	Created  time.Time
	Deadline time.Time
	// Heading of a prompt that is no approval of a command, such as the confirmation of a slash command; empty for the usual one
	Title string
	// Asked of the person for a command of theirs, such as a slash command's confirmation, which the approval and human input hooks do not
	// hear of
	quiet bool
}

// Hook points that hear of the prompt, none for a quiet one
func (a *Approvals) heard(p Prompt) *hooks.Hooks {
	if p.quiet {
		return nil
	}
	return a.hooks
}

// One prompt and how it ended
type prompt struct {
	Prompt
	done chan struct{}
	once sync.Once
	// Answer given, empty while none was
	choice string
	reason string
	// Why it ended with nobody answering
	cancelled string
	expired   bool
}

func (p *prompt) finish() { p.once.Do(func() { close(p.done) }) }

// Answer to the prompts of a session
type Answer struct {
	// Prompt answered, the oldest when empty
	ID     string
	Choice string
	// Answers every prompt waiting
	All bool
	// What the user said when denying
	Reason string
}

// Starts telling notify whenever the prompts of the session change, which is how the user sees them
func (a *Approvals) Watch(id string, notify func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.session(id).notify = notify
}

// Stops telling the session's windows, the prompts still waiting withdrawn with the cause given
func (a *Approvals) Unwatch(id, cause string) {
	a.mu.Lock()
	s := a.sessions[id]
	if s == nil {
		a.mu.Unlock()
		return
	}
	queue := s.queue
	s.queue, s.notify = nil, nil
	for _, p := range queue {
		p.cancelled = cause
	}
	a.mu.Unlock()
	for _, p := range queue {
		p.finish()
	}
	if len(queue) > 0 {
		a.tell(id, nil)
	}
}

// Starts telling fn the prompts waiting in a session whenever they change, in any session, such as for a client answering them from
// elsewhere; the function returned stops it
func (a *Approvals) Observe(fn func(session string, prompts []Prompt)) (stop func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.observers == nil {
		a.observers = map[int]func(string, []Prompt){}
	}
	a.observed++
	key := a.observed
	a.observers[key] = fn
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		delete(a.observers, key)
	}
}

// Prompts waiting in every session, by session
func (a *Approvals) Waiting() map[string][]Prompt {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string][]Prompt{}
	for id, s := range a.sessions {
		for _, p := range s.queue {
			out[id] = append(out[id], p.Prompt)
		}
	}
	return out
}

// Tells the session's windows, when notify is theirs, and every observer that its prompts changed
func (a *Approvals) tell(id string, notify func()) {
	if notify != nil {
		notify()
	}
	a.mu.Lock()
	observers := slices.Collect(maps.Values(a.observers))
	a.mu.Unlock()
	if len(observers) == 0 {
		return
	}
	prompts := a.Pending(id)
	for _, observe := range observers {
		observe(id, prompts)
	}
}

// Prompts of the session waiting for an answer, oldest first
func (a *Approvals) Pending(id string) []Prompt {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []Prompt
	if s := a.sessions[id]; s != nil {
		for _, p := range s.queue {
			out = append(out, p.Prompt)
		}
	}
	return out
}

// Answers the prompt named, the oldest, or every one waiting, giving how many were answered
func (a *Approvals) Resolve(id string, answer Answer) int {
	a.mu.Lock()
	s := a.sessions[id]
	if s == nil || len(s.queue) == 0 {
		a.mu.Unlock()
		return 0
	}
	var targets, rest []*prompt
	switch {
	case answer.ID != "":
		for _, p := range s.queue {
			if p.ID == answer.ID {
				targets = append(targets, p)
			} else {
				rest = append(rest, p)
			}
		}
	case answer.All:
		targets = s.queue
	default:
		targets, rest = s.queue[:1], s.queue[1:]
	}
	for _, p := range targets {
		choice := answer.Choice
		if !slices.ContainsFunc(p.Choices, func(c Choice) bool { return c.Value == choice }) {
			// An answer the prompt doesn't offer counts as the nearest it does, never as more than it allows
			choice = narrowest(p.Choices, choice)
		}
		p.choice, p.reason = choice, answer.Reason
	}
	s.queue = rest
	notify := s.notify
	a.mu.Unlock()
	for _, p := range targets {
		p.finish()
	}
	if len(targets) > 0 {
		a.tell(id, notify)
	}
	return len(targets)
}

// Choice the prompt offers in place of one it doesn't: always falls to session, session to once, anything else to deny
func narrowest(choices []Choice, choice string) string {
	offered := func(value string) bool {
		return slices.ContainsFunc(choices, func(c Choice) bool { return c.Value == value })
	}
	for _, step := range []struct{ from, to string }{{Always, Session}, {Session, Once}} {
		if choice == step.from {
			choice = step.to
			if offered(choice) {
				return choice
			}
		}
	}
	if offered(choice) {
		return choice
	}
	return Deny
}

// How a wait for the user ended
type outcome struct {
	// Answer given, empty when there was none
	choice string
	reason string
	// Why nobody answered, empty when someone did or the time ran out
	cancelled string
}

// Whether the session has a window to show prompts in
func (a *Approvals) watched(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id] != nil && a.sessions[id].notify != nil
}

// Puts the prompt before the user and waits until they answer, the time runs out or the turn stops; an identical prompt already waiting
// is waited on instead, and its answer taken unless it was for that one time only
func (a *Approvals) await(ctx context.Context, c Context, p Prompt) outcome {
	limit := timeout(c.Config)
	for {
		a.mu.Lock()
		s := a.session(c.Session)
		var leader *prompt
		for _, waiting := range s.queue {
			if waiting.Command == p.Command && slices.Equal(waiting.Keys, p.Keys) {
				leader = waiting
				break
			}
		}
		if leader == nil {
			break
		}
		a.mu.Unlock()
		// The approval hooks hear of a prompt that waits on an identical one already asked, and of what that one came to
		asked := approval(ctx, c, p, c.surface())
		asked.Coalesced = true
		a.heard(p).Fire(ctx, c.Config, asked)
		out, again := a.follow(ctx, c, leader)
		if again {
			continue
		}
		a.heard(p).Fire(context.WithoutCancel(ctx), c.Config, hooks.Approved{Approval: asked, Choice: choiceOf(out), Cancelled: out.cancelled})
		return out
	}
	p.ID, p.Created, p.Deadline = strs.Random(16), time.Now(), time.Now().Add(limit)
	entry := &prompt{Prompt: p, done: make(chan struct{})}
	s := a.session(c.Session)
	s.queue = append(s.queue, entry)
	notify := s.notify
	a.mu.Unlock()
	// The human input and approval hooks hear of the prompt put to the person and of how it ended
	human, asked := humanInput(ctx, c, p), approval(ctx, c, p, c.surface())
	a.heard(p).Fire(ctx, c.Config, human)
	a.heard(p).Fire(ctx, c.Config, asked)
	a.tell(c.Session, notify)
	result := a.put(ctx, c, entry, limit)
	choice := choiceOf(result)
	a.heard(p).Fire(context.WithoutCancel(ctx), c.Config, hooks.HumanAnswered{HumanInput: human, Outcome: choice})
	a.heard(p).Fire(context.WithoutCancel(ctx), c.Config, hooks.Approved{Approval: asked, Choice: choice, Cancelled: result.cancelled})
	return result
}

// What a prompt that waits on an identical one already asked comes to, and whether that one was answered for its own time only, so the
// prompt is asked again
func (a *Approvals) follow(ctx context.Context, c Context, leader *prompt) (outcome, bool) {
	state := a.wait(ctx, c, leader, time.Until(leader.Deadline))
	a.mu.Lock()
	choice, reason, expired := leader.choice, leader.reason, leader.expired
	a.mu.Unlock()
	if state == waitSet && choice == "" && expired {
		state = waitTimeout
	}
	switch {
	case state == waitInterrupted:
		return outcome{choice: Deny, cancelled: cause(ctx)}, false
	case state == waitTimeout:
		return outcome{}, false
	case choice == Once:
		return outcome{}, true
	case choice == "":
		return outcome{cancelled: cmp.Or(leader.cancelled, "the turn ended before the prompt was answered")}, false
	}
	return outcome{choice: choice, reason: reason}, false
}

// Waits on the prompt put to the person until they answer, the time runs out or the turn stops, the prompt taken off the queue after
func (a *Approvals) put(ctx context.Context, c Context, entry *prompt, limit time.Duration) outcome {
	state := a.wait(ctx, c, entry, limit)
	a.mu.Lock()
	removed := false
	var notify func()
	if s := a.sessions[c.Session]; s != nil {
		before := len(s.queue)
		s.queue = slices.DeleteFunc(s.queue, func(q *prompt) bool { return q == entry })
		removed = len(s.queue) != before
		notify = s.notify
	}
	if entry.choice == "" && state == waitTimeout {
		entry.expired = true
	}
	if state == waitInterrupted {
		entry.cancelled = cause(ctx)
	}
	result := outcome{choice: entry.choice, reason: entry.reason, cancelled: entry.cancelled}
	a.mu.Unlock()
	entry.finish()
	if removed {
		a.tell(c.Session, notify)
	}
	switch {
	case state == waitInterrupted:
		return outcome{choice: Deny, cancelled: result.cancelled}
	case result.choice == "" && state == waitSet:
		result.cancelled = cmp.Or(result.cancelled, "the turn ended before the prompt was answered")
	}
	return result
}

const (
	waitSet = iota
	waitTimeout
	waitInterrupted
)

// Waits on the prompt, saying how long it has waited every ten seconds, and that it no longer waits once it is done
func (a *Approvals) wait(ctx context.Context, c Context, p *prompt, limit time.Duration) int {
	deadline := time.NewTimer(max(limit, 0))
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	start, told := time.Now(), false
	defer func() {
		if told {
			c.Activity("")
		}
	}()
	for {
		select {
		case <-p.done:
			return waitSet
		case <-ctx.Done():
			slog.Info("Approval wait interrupted — returning deny for session", "session", c.Session)
			return waitInterrupted
		case <-deadline.C:
			return waitTimeout
		case <-tick.C:
			if c.Activity != nil {
				c.Activity(fmt.Sprintf("waiting for user approval (%ds elapsed)", int(time.Since(start).Seconds())))
				told = true
			}
		}
	}
}

// Why the turn stopped waiting, for the model
func cause(ctx context.Context) string {
	if err := context.Cause(ctx); err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		return err.Error()
	}
	return "turn interrupted"
}

// Choices of a prompt: once, then for the session and always when those may last, then deny
func choices(session, always bool) []Choice {
	out := []Choice{{Once, "Allow once"}}
	if session {
		out = append(out, Choice{Session, "Allow this session"})
	}
	if always {
		out = append(out, Choice{Always, "Always allow"})
	}
	return append(out, Choice{Deny, "Deny"})
}

// Request a transport shows, never changed once made, with a digest over every field the decision must be bound to
type Request struct {
	SchemaVersion  int
	ID             string
	Digest         string
	Command        string
	Description    string
	Keys           []string
	Surface        string
	TimeoutSeconds float64
	Choices        []string
}

// Decision a transport gives for a request
type Decision struct {
	ID     string
	Digest string
	Choice string
}

func newRequest(c Context, p Prompt, surface string) (Request, error) {
	r := Request{SchemaVersion: 1, ID: strs.Random(16), Command: p.Command, Description: p.Description, Keys: slices.Clone(p.Keys), Surface: surface,
		TimeoutSeconds: timeout(c.Config).Seconds()}
	for _, choice := range p.Choices {
		r.Choices = append(r.Choices, choice.Value)
	}
	canonical, err := json.Marshal(map[string]any{"schema_version": r.SchemaVersion, "request_id": r.ID, "command": r.Command,
		"description": r.Description, "pattern_key": strs.First(r.Keys), "pattern_keys": r.Keys, "surface": surface, "timeout_seconds": r.TimeoutSeconds,
		"allowed_choices": r.Choices, "session_key": c.Session})
	if err != nil {
		return Request{}, err
	}
	sum := sha256.Sum256(canonical)
	r.Digest = hex.EncodeToString(sum[:])
	return r, nil
}

// What became of a prompt sent through the chosen transport: not chosen, an answer, or why it failed and whether to fall back to the chat
type sent struct {
	chosen   bool
	choice   string
	failure  string
	fallback bool
	name     string
}

// Shows the prompt through the transport approvals.transport names, when it names one
func (a *Approvals) send(ctx context.Context, c Context, p Prompt) sent {
	name := strings.ToLower(strings.TrimSpace(c.Config.GetString(approvalsconfig.Transport)))
	if name == "" || name == approvalsconfig.Builtin {
		return sent{}
	}
	fallback := c.Config.GetString(approvalsconfig.TransportFallback) == approvalsconfig.Builtin
	i := slices.IndexFunc(a.Transports(), func(t Transport) bool { return t.Name == name })
	if i < 0 {
		slog.Warn("Selected approval transport is unavailable", "transport", name)
		return sent{chosen: true, choice: Deny, failure: "unavailable", fallback: fallback, name: name}
	}
	transport := a.Transports()[i]
	request, err := newRequest(c, p, "transport:"+name)
	if err != nil {
		slog.Warn("Could not build redacted plugin approval request")
		return sent{chosen: true, choice: Deny, failure: "error", name: name}
	}
	// The approval and human input hooks hear of the request the transport presents and of what came of it
	// The hooks read the request as the transport is handed it, its secrets masked
	shown := Prompt{Command: request.Command, Description: request.Description, Keys: p.Keys}
	asked, human := approval(ctx, c, shown, request.Surface), humanInput(ctx, c, shown)
	asked.RequestID, asked.Digest = request.ID, request.Digest
	a.heard(p).Fire(ctx, c.Config, asked)
	a.heard(p).Fire(ctx, c.Config, human)
	choice, failure := a.invoke(ctx, transport, request, timeout(c.Config))
	came := choice
	if failure != "" {
		came = "transport_" + failure
	}
	a.heard(p).Fire(context.WithoutCancel(ctx), c.Config, hooks.HumanAnswered{HumanInput: human, Outcome: came})
	a.heard(p).Fire(context.WithoutCancel(ctx), c.Config, hooks.Approved{Approval: asked, Choice: came})
	return sent{chosen: true, choice: choice, failure: failure, fallback: fallback, name: name}
}

// Runs the transport on a worker of its own and checks its decision against the request and the deadline
func (a *Approvals) invoke(ctx context.Context, transport Transport, request Request, limit time.Duration) (string, string) {
	select {
	case a.slots <- struct{}{}:
	default:
		slog.Warn("Approval transport worker capacity exhausted")
		return Deny, "busy"
	}
	type result struct {
		decision Decision
		err      error
		at       time.Time
	}
	results := make(chan result, 1)
	deadline := time.Now().Add(max(limit, 0))
	worker, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	go func() {
		defer func() { <-a.slots }()
		defer func() {
			if p := recover(); p != nil {
				results <- result{err: fmt.Errorf("transport panic: %v", p), at: time.Now()}
			}
		}()
		decision, err := transport.Present(worker, request)
		results <- result{decision, err, time.Now()}
	}()
	timer := time.NewTimer(max(limit, 0))
	defer timer.Stop()
	var got result
	select {
	case got = <-results:
	case <-ctx.Done():
		slog.Info("Approval transport wait interrupted", "request_id", request.ID)
		return Deny, "interrupted"
	case <-timer.C:
		slog.Warn("Approval transport timed out", "request_id", request.ID)
		return Deny, "timeout"
	}
	switch {
	case got.at.After(deadline):
		slog.Warn("Approval transport timed out", "request_id", request.ID)
		return Deny, "timeout"
	case got.err != nil:
		slog.Warn("Approval transport failed", "request_id", request.ID, "error", got.err)
		return Deny, "error"
	case got.decision.ID == "" && got.decision.Digest == "":
		slog.Warn("Approval transport returned an invalid decision type")
		return Deny, "invalid"
	case got.decision.ID != request.ID || got.decision.Digest != request.Digest:
		slog.Warn("Approval transport returned a stale or mismatched decision")
		return Deny, "stale"
	case !slices.Contains(request.Choices, got.decision.Choice):
		slog.Warn("Approval transport returned a disallowed choice")
		return Deny, "invalid"
	}
	return got.decision.Choice, ""
}

// Answer of a transport: the choice, else nothing to fall back to the chat, else the refusal
func (a *Approvals) transported(c Context, s sent) (string, *refusal) {
	if !s.chosen {
		return "", nil
	}
	if s.failure == "" {
		return s.choice, nil
	}
	if s.fallback {
		slog.Warn("Approval transport failed; using explicit builtin fallback", "transport", s.name, "failure", s.failure)
		return "", nil
	}
	message := fmt.Sprintf("BLOCKED: Selected approval transport failed (%s); the user has NOT consented to this action. Do NOT retry this command "+
		"or attempt the same outcome through another route.%s", s.failure, a.breaker(c.Config, c.Session))
	return "", refused(c.Config, message, "transport_"+s.failure, "command", "")
}
