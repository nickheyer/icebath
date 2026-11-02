package sprint

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/protocol"
)

// Reminders a member gets to call its tool after ending turns without it before it counts as stalled
const maxReminders = 2

// Times a run that failed for a reason that may pass is started again before the stage counts as stalled
const maxRetries = 3

// Waits before each retry of a failed run
var backoffs = [maxRetries]time.Duration{5 * time.Second, 20 * time.Second, time.Minute}

// Shortest grace a member gets between the wrap up steer and the interrupt
const minGrace = 2 * time.Minute

// Error of a stage once the whole sprint ran past its limit
var errExpired = errors.New("the sprint ran past its time limit")

// Error of a member that ended its turns without handing off
var errStalled = errors.New("ended its turns without handing off")

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

// Member of a role at an index, its session opened when it has none open yet
func (r *runner) member(ctx context.Context, role Role, index int) (*Member, error) {
	s := r.s
	name := r.cfg.NameOf(role, index)
	if m := s.member(name); m != nil && m.Live != nil && r.f.App.LiveOf(m.Live.Session) != nil {
		return m, nil
	}
	live, err := r.f.App.CreateKind(ctx, KindName(role))
	if err != nil {
		err = fmt.Errorf("%s: %w", name, err)
		s.say(MsgFailure, name, Person, "", err.Error())
		r.f.Notify(harness.Error, s.ID+": "+err.Error())
		return nil, err
	}
	live.Session.SetTitle(name + " of " + s.ID)
	m := &Member{Role: role, Name: name, Index: index, Live: live, Joined: time.Now()}
	s.putMember(m)
	return m, nil
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
	why := fmt.Sprintf("%s stalled, %v. Steer %s in the arena, then resume to try again, or stop the sprint", m.Name, err, m.Name)
	if errors.Is(err, errExpired) {
		why = fmt.Sprintf("the sprint ran past its limit of %s, resume grants another %s", r.cfg.Limits.Total, r.cfg.Limits.Total)
	}
	s.say(MsgFailure, m.Name, Person, s.ticketOf(m), err.Error())
	s.pause(why)
	s.wait(m.Name)
	if gerr := r.gate(ctx); gerr != nil {
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

// Sends text to a member and waits for its tool call or run end, retrying passing failures and reminding
func (r *runner) exchange(ctx context.Context, m *Member, text, toolName string, limit time.Duration, check func(any) error) (any, protocol.Response, error) {
	s := r.s
	reminders, retries := 0, 0
	for {
		if err := r.gate(ctx); err != nil {
			return nil, protocol.Response{}, err
		}
		var w *handoffWait
		if toolName != "" {
			w = s.await(m.SessionID(), toolName, check)
		}
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
		s.release(m.SessionID(), w)
		switch {
		case err == nil && (toolName == "" || v != nil):
			return v, resp, nil
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

// Starts a run on the member with the text, steering a run already going on its session instead
func (r *runner) send(ctx context.Context, m *Member, text string) (*harness.Run, error) {
	parts := []protocol.Part{protocol.Text{Text: text}}
	if run := r.active(m); run != nil {
		if err := r.f.Steer(run, parts...); err == nil {
			return run, nil
		}
	}
	return r.f.Start(ctx, m.Live, "", parts)
}

// Sends text to a member without waiting on it, a closing word the member need not answer
func (r *runner) tell(ctx context.Context, m *Member, text string) {
	if run, err := r.send(ctx, m, text); err == nil && run != nil {
		r.f.SetStatus(r.s.Snapshot().Status)
	}
}

// Root run going on the session of a member, nil while none is
func (r *runner) active(m *Member) *harness.Run {
	for _, run := range r.f.App.Control.Active() {
		if run.Parent == "" && run.Session == m.Live.Session {
			return run
		}
	}
	return nil
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
		spent := sess.Usage().Total()
		if s, m, ok := sprintOfSession(sess.ID()); ok {
			s.spend(m.Name, spent)
		}
		if budget > 0 && spent > budget {
			return fmt.Errorf("%w: %s spent %d tokens, past its budget of %d", errBudget, role, spent, budget)
		}
		return nil
	}}
}
