package sprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
)

// Reminders a member gets to call its tool after ending turns without it before it counts as stalled
const maxReminders = 2

// Times a run that failed for a reason that may pass is started again before the stage counts as stalled
const maxRetries = 3

// Waits before each retry of a failed run
var backoffs = [maxRetries]time.Duration{5 * time.Second, 20 * time.Second, time.Minute}

// Shortest grace a member gets between the wrap up steer and the interrupt
const minGrace = 2 * time.Minute

// Pause before a start is tried again once a run started elsewhere on the session
const raceWait = 200 * time.Millisecond

// Times a text that never landed in a run of its member is handed to it again
const maxResends = 2

// Error of a stage once the whole sprint ran past its limit
var errExpired = errors.New("the sprint ran past its time limit")

// Error of a member that ended its turns without handing off
var errStalled = errors.New("finished its turn without a handoff")

// Error that ends a run once a role has spent its token budget
var errBudget = errors.New("token budget spent")

// Error of a director whose turn held no report
var errNoReport = errors.New("ended its turn without writing the report")

// Opens every role endpoint before the brief, a missing profile or model failing the sprint at once
func (r *runner) checkTeam(ctx context.Context) error {
	app := r.f.App
	seen := map[harness.Target]bool{}
	for _, k := range r.cfg.Kinds(app.Primary.Profile.Name) {
		role, _ := RoleOfKind(k.Name)
		var err error
		switch {
		case k.Model == nil && app.Primary.Provider == nil:
			err = harness.ErrNoProfile
		case k.Model == nil, seen[*k.Model]:
			continue
		default:
			seen[*k.Model] = true
			_, err = app.OpenEndpoint(ctx, k.Model.Profile, k.Model.Model)
		}
		if err == nil {
			continue
		}
		err = fmt.Errorf("%s: %w", r.cfg.NameOf(role, 0), err)
		r.s.say(MsgFailure, "", Person, "", err.Error())
		return err
	}
	return nil
}

// Session labels naming the sprint and the role of a member session
const (
	sprintLabel = "sprint"
	roleLabel   = "role"
)

// Member of a role at an index, back on its stored session when it has one, else on a fresh one
func (r *runner) member(ctx context.Context, role Role, index int) (*Member, error) {
	s := r.s
	app := r.f.App
	name := r.cfg.NameOf(role, index)
	m := s.member(name)
	if m != nil {
		live, err := s.rejoin(ctx, app, m)
		if live != nil {
			return m, nil
		}
		if err != nil {
			s.say(MsgNote, name, "", "", "session "+m.SessionID()+" could not be opened, "+err.Error()+", starting a new one")
		}
	}
	live, err := app.CreateKind(ctx, KindName(role))
	if err != nil {
		err = fmt.Errorf("%s: %w", name, err)
		s.say(MsgFailure, name, Person, "", err.Error())
		r.f.Notify(harness.Error, s.ID+": "+err.Error())
		return nil, err
	}
	live.Session.SetTitle(name + " of " + s.ID)
	if m == nil {
		m = &Member{Role: role, Name: name, Index: index, Joined: time.Now()}
	}
	s.bind(app, m, live)
	return m, nil
}

// Binds an open session to a member, labeling it with the sprint and the role, and records the member
func (s *Sprint) bind(app *harness.App, m *Member, live *harness.Live) {
	live.Session.Label(sprintLabel, s.ID)
	live.Session.Label(roleLabel, string(m.Role))
	if err := app.SetKindOn(live, KindName(m.Role)); err != nil {
		app.Announce(harness.NoticeMsg{Level: harness.Error, Text: s.ID + ": " + err.Error(), Flow: s.ID})
	}
	s.mu.Lock()
	m.Live, m.Session = live, live.Session.ID()
	s.mu.Unlock()
	s.putMember(m)
}

// Open session of a member, its stored one opened again while closed, nil while it never had one
func (s *Sprint) rejoin(ctx context.Context, app *harness.App, m *Member) (*harness.Live, error) {
	s.reachMu.Lock()
	defer s.reachMu.Unlock()
	s.mu.Lock()
	live, stored := m.Live, m.Session
	s.mu.Unlock()
	if live != nil && app.LiveOf(live.Session) != nil {
		return live, nil
	}
	if stored == "" {
		return nil, nil
	}
	live, err := app.Open(ctx, stored)
	if err != nil {
		return nil, err
	}
	s.bind(app, m, live)
	s.say(MsgNote, m.Name, "", "", "resumed session "+stored)
	return live, nil
}

// Runs a unit of work on a member until it passes, failures pausing an interactive sprint for the person
func (r *runner) persist(ctx context.Context, m *Member, unit func() (any, protocol.Response, error)) (any, protocol.Response, error) {
	for {
		v, resp, err := unit()
		if err == nil {
			return v, resp, nil
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return nil, resp, err
		}
		if serr := r.stall(ctx, m, err); serr != nil {
			return nil, resp, serr
		}
	}
}

// Pauses the sprint on a member failure until the person resumes it, failing it when nobody is there
func (r *runner) stall(ctx context.Context, m *Member, err error) error {
	s := r.s
	if !r.cfg.Interactive {
		return err
	}
	why := fmt.Sprintf("%s stalled, %v. Message %s from the arena, then resume to retry, or stop the sprint", m.Name, err, m.Name)
	if errors.Is(err, errExpired) {
		why = fmt.Sprintf("the sprint ran past its limit of %s, resume grants another %s", r.cfg.Limits.Total, r.cfg.Limits.Total)
	}
	s.say(MsgFailure, m.Name, Person, s.ticketOf(m), err.Error())
	s.pause(why)
	s.wait(m.Name)
	if gerr := r.gate(ctx); gerr != nil {
		s.unpause()
		return gerr
	}
	if errors.Is(err, errExpired) {
		r.renew()
	}
	return nil
}

// Waits while the sprint is paused, until the person resumes it or the sprint stops
func (r *runner) gate(ctx context.Context) error {
	for r.s.paused() {
		select {
		case <-r.s.resumed():
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ctx.Err()
}

// Gives the sprint another full time limit
func (r *runner) renew() {
	r.mu.Lock()
	r.deadline = time.Now().Add(r.cfg.Limits.Total)
	r.mu.Unlock()
}

// Channel that fires once the whole sprint runs out of time
func (r *runner) expiry() <-chan time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.After(time.Until(r.deadline))
}

// Sends text to a member and waits for its tool call or run end, resending lost text, retrying and reminding
func (r *runner) exchange(ctx context.Context, m *Member, text, toolName string, limit time.Duration, check func(any) error) (any, protocol.Response, error) {
	s := r.s
	reminders, retries, resent := 0, 0, 0
	for {
		if err := r.gate(ctx); err != nil {
			return nil, protocol.Response{}, err
		}
		var w *handoffWait
		if toolName != "" {
			w = s.await(m.SessionID(), toolName, check)
		}
		mark := lastSeq(m.Live.Session)
		run, err := r.send(ctx, m, text)
		if err != nil {
			s.release(m.SessionID(), w)
			if transient(err) && retries < maxRetries {
				if err := sleep(ctx, backoffs[retries]); err != nil {
					return nil, protocol.Response{}, err
				}
				retries++
				continue
			}
			return nil, protocol.Response{}, err
		}
		v, resp, err := r.follow(ctx, run, w, toolName, limit)
		lost := err == nil && v == nil && !carried(m.Live.Session, mark, text)
		if err == nil && v == nil && toolName != "" && !lost {
			v, resp, err = r.converse(ctx, m, w, mark, toolName, limit, resp)
		}
		s.release(m.SessionID(), w)
		switch {
		case err == nil && v != nil:
			return v, resp, nil
		case lost && resent < maxResends:
			resent++
		case err == nil && toolName == "":
			return nil, resp, nil
		case err == nil && chatted(m.Live.Session, mark):
			text = resume(toolName)
		case err == nil, errors.Is(err, agent.ErrRounds):
			if reminders >= maxReminders {
				return nil, resp, fmt.Errorf("%s %w", m.Name, errStalled)
			}
			reminders++
			text = reminder(toolName)
		case ctx.Err() != nil:
			return nil, resp, ctx.Err()
		case transient(err) && retries < maxRetries:
			if err := sleep(ctx, backoffs[retries]); err != nil {
				return nil, resp, err
			}
			retries++
		default:
			return nil, resp, err
		}
	}
}

// Starts a run on the member with the text or steers the one going, retrying once after a race
func (r *runner) send(ctx context.Context, m *Member, text string) (*harness.Run, error) {
	parts := []protocol.Part{protocol.Text{Text: text}}
	start := func() (*harness.Run, error) { return r.f.Start(ctx, m.Live, "", parts) }
	run, err := r.s.reach(r.f.App, m.Live, parts, start)
	if !runGoing(err) {
		return run, err
	}
	if err := sleep(ctx, raceWait); err != nil {
		return nil, err
	}
	return r.s.reach(r.f.App, m.Live, parts, start)
}

// Steers the root run going on the session with the parts or starts one, one caller at a time
func (s *Sprint) reach(app *harness.App, live *harness.Live, parts []protocol.Part, start func() (*harness.Run, error)) (*harness.Run, error) {
	s.reachMu.Lock()
	defer s.reachMu.Unlock()
	steer := func() *harness.Run {
		if run := rootRun(app, live); run != nil && app.Control.Steer(run.ID, parts...) == nil {
			return run
		}
		return nil
	}
	if run := steer(); run != nil {
		return run, nil
	}
	run, err := start()
	if runGoing(err) {
		if run := steer(); run != nil {
			return run, nil
		}
	}
	return run, err
}

// Starts a run on the open session under the context, refused while a root run goes on it
func startOn(ctx context.Context, app *harness.App, live *harness.Live, parts []protocol.Part) (*harness.Run, error) {
	if app.Busy(live) {
		return nil, fmt.Errorf("harness: a run is going on %s", live.Session.ID())
	}
	return app.RunOn(ctx, live, "", parts)
}

// Reports whether a start was refused because a run goes on the session
func runGoing(err error) bool {
	return err != nil && strings.Contains(err.Error(), "a run is going on")
}

// Sends text to a member without waiting on it, a closing word the member need not answer
func (r *runner) tell(ctx context.Context, m *Member, text string) {
	if run, err := r.send(ctx, m, text); err == nil && run != nil {
		r.f.SetStatus(r.s.Snapshot().Status)
	}
}

// Root run going on the session of a member, nil while none is
func (r *runner) active(m *Member) *harness.Run { return rootRun(r.f.App, m.Live) }

// Root run going on the open session, nil while none is
func rootRun(app *harness.App, live *harness.Live) *harness.Run {
	for _, run := range app.Control.Active() {
		if run.Parent == "" && run.Session == live.Session {
			return run
		}
	}
	return nil
}

// Follows further runs of a member whose stage run ended without the tool, waiting for replies it asked for
func (r *runner) converse(ctx context.Context, m *Member, w *handoffWait, mark int, toolName string, limit time.Duration, resp protocol.Response) (any, protocol.Response, error) {
	for {
		knock := r.s.knocked(m.Name)
		next := r.active(m)
		if next == nil {
			if !asking(m.Live.Session, mark) {
				return nil, resp, nil
			}
			v, err := r.reply(ctx, w, knock, max(limit/4, minGrace))
			if v != nil || err != nil {
				return v, resp, err
			}
			if next = r.active(m); next == nil {
				return nil, resp, nil
			}
		}
		v, out, err := r.follow(ctx, next, w, toolName, limit)
		if err != nil || v != nil {
			return v, out, err
		}
		resp = out
	}
}

// Waits for a chat to reach the member, its tool call to arrive or the wait to pass
func (r *runner) reply(ctx context.Context, w *handoffWait, knock <-chan struct{}, wait time.Duration) (any, error) {
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-knock:
		return nil, nil
	case v := <-w.ch:
		return v, nil
	case <-t.C:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Newest sequence number among the entries of the session, minus one while it holds none
func lastSeq(sess *session.Session) int {
	last := -1
	for _, e := range sess.Entries() {
		last = max(last, e.Seq)
	}
	return last
}

// Entries of the session added after the mark, in transcript order
func since(sess *session.Session, mark int) []session.Entry {
	var out []session.Entry
	for _, e := range sess.Entries() {
		if e.Seq > mark {
			out = append(out, e)
		}
	}
	return out
}

// Reports whether a user message added after the mark carries the text
func carried(sess *session.Session, mark int, text string) bool {
	for _, e := range since(sess, mark) {
		if e.Role == protocol.RoleUser && strings.Contains(protocol.Response{Content: e.Content}.Text(), text) {
			return true
		}
	}
	return false
}

// Reports whether a message opening with the brief ever reached the session
func briefed(sess *session.Session) bool {
	for _, e := range sess.Entries() {
		if e.Role == protocol.RoleUser && opensWithBrief(protocol.Response{Content: e.Content}.Text()) {
			return true
		}
	}
	return false
}

// Reports whether a chat from a teammate reached the session after the mark
func chatted(sess *session.Session, mark int) bool {
	for _, e := range since(sess, mark) {
		if e.Role != protocol.RoleUser {
			continue
		}
		for _, p := range e.Content {
			if t, ok := p.(protocol.Text); ok && strings.HasPrefix(t.Text, chatHead) {
				return true
			}
		}
	}
	return false
}

// Reports whether the session sent a teammate a message after the mark and no user message came after it
func asking(sess *session.Session, mark int) bool {
	asked := false
	calls := map[string]bool{}
	for _, e := range since(sess, mark) {
		switch e.Role {
		case protocol.RoleAssistant:
			for _, p := range e.Content {
				if c, ok := p.(protocol.ToolCall); ok && c.Name == ToolMessage && !toPerson(c.Args) {
					calls[c.ID] = true
				}
			}
		case protocol.RoleTool:
			for _, p := range e.Content {
				if res, ok := p.(protocol.ToolResult); ok && calls[res.CallID] && !res.IsError {
					asked = true
				}
			}
		case protocol.RoleUser:
			asked = false
		}
	}
	return asked
}

// Reports whether message arguments address the person, who answers by steering rather than by message
func toPerson(args json.RawMessage) bool {
	var in messageInput
	return json.Unmarshal(args, &in) == nil && strings.EqualFold(strings.TrimSpace(in.To), Person)
}

// Hands a chat to a member and watches until it lands, handing it again while runs end without it
func (s *Sprint) post(app *harness.App, from, to *Member, live *harness.Live, text string) error {
	ctx := s.lifeCtx()
	if ctx == nil || ctx.Err() != nil {
		return fmt.Errorf("%s could not be reached, %s is not running in this process", to.Name, s.ID)
	}
	parts := []protocol.Part{protocol.Text{Text: text}}
	mark := lastSeq(live.Session)
	run, err := s.reach(app, live, parts, func() (*harness.Run, error) { return startOn(ctx, app, live, parts) })
	if err != nil {
		return fmt.Errorf("%s could not be reached, %w", to.Name, err)
	}
	s.knock(to.Name)
	go s.watch(ctx, app, from, to, live, run, mark, text)
	return nil
}

// Follows the run a chat went to, handing it again until it lands, telling the sender when it never does
func (s *Sprint) watch(ctx context.Context, app *harness.App, from, to *Member, live *harness.Live, run *harness.Run, mark int, text string) {
	parts := []protocol.Part{protocol.Text{Text: text}}
	for tries := 0; ; tries++ {
		done, ok := app.Control.Done(run.ID)
		if !ok {
			s.undelivered(app, from, to, "its run "+run.ID+" is unknown to the harness")
			return
		}
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
		if carried(live.Session, mark, text) || ctx.Err() != nil {
			return
		}
		why := run.Err()
		switch {
		case why != nil && !transient(why):
			s.undelivered(app, from, to, "its run ended with "+why.Error())
			return
		case tries == maxResends:
			s.undelivered(app, from, to, fmt.Sprintf("it was handed over %d times and never landed", tries+1))
			return
		case why != nil:
			if sleep(ctx, backoffs[min(tries, len(backoffs)-1)]) != nil {
				return
			}
		}
		mark = lastSeq(live.Session)
		next, err := s.reach(app, live, parts, func() (*harness.Run, error) { return startOn(ctx, app, live, parts) })
		if err != nil {
			s.undelivered(app, from, to, err.Error())
			return
		}
		s.knock(to.Name)
		run = next
	}
}

// Logs a chat that never reached its member and tells the sender so it can send it again
func (s *Sprint) undelivered(app *harness.App, from, to *Member, why string) {
	s.say(MsgFailure, to.Name, from.Name, "", "message from "+from.Name+" did not reach "+to.Name+", "+why)
	s.mu.Lock()
	live := from.Live
	ctx := s.life
	s.mu.Unlock()
	if live == nil || app.LiveOf(live.Session) == nil || ctx == nil || ctx.Err() != nil {
		return
	}
	parts := []protocol.Part{protocol.Text{Text: chatLost(to.Name, why)}}
	if _, err := s.reach(app, live, parts, func() (*harness.Run, error) { return startOn(ctx, app, live, parts) }); err != nil {
		s.say(MsgFailure, from.Name, "", "", "could not be told its message to "+to.Name+" was lost, "+err.Error())
	}
}

// Waits for the tool call or run end, steering a wrap up at the limit and interrupting after a grace
func (r *runner) follow(ctx context.Context, run *harness.Run, w *handoffWait, toolName string, limit time.Duration) (any, protocol.Response, error) {
	done, ok := r.f.App.Control.Done(run.ID)
	if !ok {
		return nil, protocol.Response{}, fmt.Errorf("harness: no run %s", run.ID)
	}
	var arrived <-chan any
	if w != nil {
		arrived = w.ch
	}
	wrap := time.NewTimer(limit)
	defer wrap.Stop()
	var grace <-chan time.Time
	expired := false
	for {
		select {
		case v := <-arrived:
			return v, protocol.Response{}, nil
		case <-done:
			if w != nil {
				select {
				case v := <-w.ch:
					return v, run.Response(), nil
				default:
				}
			}
			if expired {
				return nil, run.Response(), errExpired
			}
			return nil, run.Response(), run.Err()
		case <-ctx.Done():
			return nil, protocol.Response{}, ctx.Err()
		case <-wrap.C:
			if toolName == "" {
				r.f.Interrupt(run)
				continue
			}
			r.f.Steer(run, protocol.Text{Text: wrapUp(toolName, limit)})
			grace = time.After(max(limit/4, minGrace))
		case <-grace:
			r.f.Interrupt(run)
			grace = nil
		case <-r.expiry():
			expired = true
			r.f.Interrupt(run)
		}
	}
}

// Waits for the duration unless the context ends first
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reports whether a run failure may pass on a retry, as endpoint hiccups and dropped streams do
func transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, agent.ErrInterrupted) || errors.Is(err, agent.ErrRounds) || errors.Is(err, errBudget) || errors.Is(err, errExpired) || errors.Is(err, errStalled) || errors.Is(err, errNoReport) {
		return false
	}
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return pe.Transient && !pe.Stuck()
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, hint := range []string{"stream ended", "connection", "eof", "timeout", "timed out", "a run is going on", "temporarily", "overloaded", "rate limit"} {
		if strings.Contains(text, hint) {
			return true
		}
	}
	return false
}

// Hook recording the tokens a member spent and ending its run past the role budget, nil for other agents
func budgetHook(info harness.AgentInfo) agent.Hook {
	role, ok := RoleOfKind(info.Kind.Name)
	if !ok {
		return nil
	}
	cfg, err := Load(info.App.Config)
	if err != nil {
		return agent.Hooks{BeforeRunFn: func(context.Context, []protocol.Part) ([]protocol.Part, error) { return nil, err }}
	}
	budget := cfg.Roles[role].Budget
	sess := info.Session
	return agent.Hooks{AfterTurnFn: func(ctx context.Context, resp protocol.Response) error {
		u := sess.Usage()
		spent := Spent(u)
		if s, m, ok := sprintOfSession(sess.ID()); ok {
			s.spend(m.Name, u)
		}
		if budget > 0 && spent > budget {
			return fmt.Errorf("%w: %s spent %d tokens, past its budget of %d", errBudget, role, spent, budget)
		}
		return nil
	}}
}
