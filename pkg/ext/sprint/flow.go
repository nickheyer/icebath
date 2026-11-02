package sprint

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/protocol"
)

// Workflow that runs one sprint from a request
func workflow() harness.Workflow {
	return harness.Workflow{
		Name:        Name,
		Title:       "Sprint",
		Description: "a director settles the request, an architect cuts it into tickets, builders build them and a reviewer checks every handoff, all of it shown in the arena",
		Arg:         "request",
		Run:         run,
	}
}

// One sprint going through its stages
type runner struct {
	cfg Config
	f   *harness.Flow
	s   *Sprint
	// Reviews go one at a time through the one reviewer
	reviewMu sync.Mutex
	// The reviewer has read the brief and the plan
	briefed bool
	mu      sync.Mutex
	// Moment the whole sprint runs out of time
	deadline time.Time
}

// Runs a sprint from the request, or picks an ended one of the same flow up where it stopped
func run(ctx context.Context, f *harness.Flow, request string) error {
	cfg, err := Load(f.App.Config)
	if err != nil {
		return err
	}
	for _, k := range cfg.Kinds() {
		if err := f.App.PutKind(k); err != nil {
			return err
		}
	}
	s := sprintByID(f.ID)
	if s == nil {
		s = newSprint(f.ID, request, time.Now(), f.App.Announce)
		register(s)
	} else {
		s.setAnnounce(f.App.Announce)
		if snap := s.Snapshot(); snap.Running() {
			return fmt.Errorf("%s is still running", s.ID)
		}
		from := s.restart()
		s.say(MsgStage, "", "", "", "restarting at the "+string(from)+" stage")
	}
	r := &runner{cfg: cfg, f: f, s: s, deadline: time.Now().Add(cfg.Limits.Total)}
	f.Panel(ArenaKey)
	f.App.Announce(ShowMsg{ID: s.ID})
	err = r.stages(ctx)
	s.finish(err)
	switch {
	case err == nil:
		done, total := s.Snapshot().Progress()
		f.Notify(harness.Success, fmt.Sprintf("%s done, %d of %d tickets shipped", s.ID, done, total))
	case errors.Is(err, context.Canceled):
		f.Notify(harness.Warn, s.ID+" stopped, it can be restarted from the arena")
	default:
		f.Notify(harness.Error, s.ID+" failed: "+err.Error())
	}
	return err
}

// Runs the stages from the one the sprint is in to the report, a short build still reported on
func (r *runner) stages(ctx context.Context) error {
	stage := r.s.Snapshot().Stage
	for {
		r.f.SetStatus(string(stage))
		var err error
		switch stage {
		case StageBrief:
			err, stage = r.brief(ctx), StagePlan
		case StagePlan:
			err, stage = r.plan(ctx), StageBuild
		case StageBuild:
			err, stage = r.build(ctx), StageReport
		case StageReport:
			return r.report(ctx, nil)
		default:
			return fmt.Errorf("sprint cannot run from the %s stage", stage)
		}
		if err == nil {
			continue
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || stage != StageReport {
			return err
		}
		if rerr := r.report(ctx, err); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}
}

// Director settles the brief with the person
func (r *runner) brief(ctx context.Context) error {
	s := r.s
	s.enter(StageBrief, "settling the brief")
	d, err := r.member(ctx, RoleDirector, 0)
	if err != nil {
		return err
	}
	s.task(d, "", "settling the brief")
	defer s.task(d, "", "")
	v, _, err := r.persist(ctx, d, func() (any, protocol.Response, error) {
		return r.exchange(ctx, d, directorOpening(s.Snapshot().Request, r.cfg.Interactive), ToolBrief, r.cfg.Limits.Brief, nil)
	})
	if err != nil {
		return fmt.Errorf("brief stage: %w", err)
	}
	b := v.(Brief)
	s.setBrief(b)
	s.say(MsgNote, d.Name, "", "", "brief settled: "+firstLine(b.Objective))
	return nil
}

// Architect cuts the brief into tickets
func (r *runner) plan(ctx context.Context) error {
	s := r.s
	s.enter(StagePlan, "cutting the plan")
	a, err := r.member(ctx, RoleArchitect, 0)
	if err != nil {
		return err
	}
	s.task(a, "", "cutting the plan")
	defer s.task(a, "", "")
	snap := s.Snapshot()
	v, _, err := r.persist(ctx, a, func() (any, protocol.Response, error) {
		return r.exchange(ctx, a, architectOpening(snap.Brief, r.cfg.Team.MaxTickets, r.cfg.Team.Builders), ToolPlan, r.cfg.Limits.Plan, nil)
	})
	if err != nil {
		return fmt.Errorf("plan stage: %w", err)
	}
	in := v.(planInput)
	tickets := make([]*Ticket, 0, len(in.Tickets))
	for i := range in.Tickets {
		t := in.Tickets[i]
		tickets = append(tickets, &t)
	}
	s.setPlan(in.Plan, tickets)
	s.say(MsgNote, a.Name, "", "", fmt.Sprintf("plan cut into %d tickets: %s", len(tickets), in.Title))
	return nil
}

// Ticket a builder finished with, terminal or cut short
type buildResult struct {
	m   *Member
	err error
}

// Builders take ready tickets as they free up, each handoff going through the reviewer, until every ticket is settled
func (r *runner) build(ctx context.Context) error {
	s := r.s
	s.enter(StageBuild, "building")
	if _, err := r.member(ctx, RoleReviewer, 0); err != nil {
		return err
	}
	n := r.cfg.Team.Builders
	if !r.cfg.Team.Parallel {
		n = 1
	}
	n = min(n, max(len(s.Snapshot().Tickets), 1))
	idle := make([]*Member, 0, n)
	for i := range n {
		m, err := r.member(ctx, RoleBuilder, i)
		if err != nil {
			return err
		}
		idle = append(idle, m)
	}
	results := make(chan buildResult, n)
	active := 0
	var failures []error
	for {
		if err := r.gate(ctx); err != nil {
			return drain(results, active, err)
		}
		s.blockDependents()
		ready := s.ready()
		for len(idle) > 0 && len(ready) > 0 {
			m, t := idle[0], ready[0]
			idle, ready = idle[1:], ready[1:]
			active++
			go func() { results <- r.buildTicket(ctx, m, t) }()
		}
		if s.settled() && active == 0 {
			break
		}
		if active == 0 {
			if len(ready) > 0 {
				return errors.New("build stage: no builder is free though tickets are ready")
			}
			s.strand()
			continue
		}
		done, total := s.Snapshot().Progress()
		s.setStatus(fmt.Sprintf("building, %d of %d tickets done, %d in hand", done, total, active))
		select {
		case res := <-results:
			active--
			idle = append(idle, res.m)
			if res.err != nil {
				failures = append(failures, res.err)
				if ctx.Err() != nil || errors.Is(res.err, context.Canceled) {
					return drain(results, active, res.err)
				}
			}
		case <-ctx.Done():
			return drain(results, active, ctx.Err())
		}
	}
	done, total := s.Snapshot().Progress()
	s.setStatus(fmt.Sprintf("built %d of %d tickets", done, total))
	if done == 0 && total > 0 {
		return errors.Join(append([]error{errors.New("build stage: no ticket was accepted")}, failures...)...)
	}
	return nil
}

// Waits for the builders still working once the stage ends early and hands back why it ended
func drain(results chan buildResult, active int, err error) error {
	for ; active > 0; active-- {
		<-results
	}
	return err
}

// Builds one ticket through handoffs and verdicts until the reviewer accepts or fails it
func (r *runner) buildTicket(ctx context.Context, m *Member, t *Ticket) buildResult {
	s := r.s
	reviewer := s.member(r.cfg.NameOf(RoleReviewer, 0))
	s.update(t, func(t *Ticket) { t.Status, t.Builder, t.Started, t.Err = TicketActive, m.Name, time.Now(), "" })
	s.task(m, t.ID, "building "+t.ID)
	defer s.task(m, "", "")
	s.say(MsgAssign, reviewer.Name, m.Name, t.ID, "take "+t.ID+" "+t.Title)
	snap := s.Snapshot()
	text := builderOpening(snap.Brief, snap.Plan, snap.Tickets, snapTicket(snap, t.ID))
	if m.Live.Session.Len() > 0 {
		text = builderNext(snapTicket(snap, t.ID), snap.Tickets)
	}
	check := func(v any) error {
		if h := v.(Handoff); h.Ticket != t.ID {
			return fmt.Errorf("your ticket is %s, hand that one off", t.ID)
		}
		return nil
	}
	for {
		v, _, err := r.persist(ctx, m, func() (any, protocol.Response, error) {
			return r.exchange(ctx, m, text, ToolHandoff, r.cfg.Limits.Build, check)
		})
		if err != nil {
			return r.cutShort(m, t, fmt.Errorf("%s on %s: %w", m.Name, t.ID, err))
		}
		h := v.(Handoff)
		s.update(t, func(t *Ticket) { t.Status, t.Report = TicketReview, h.Summary })
		s.task(m, t.ID, "waiting for the verdict on "+t.ID)
		s.say(MsgHandoff, m.Name, reviewer.Name, t.ID, string(h.Status)+": "+firstLine(h.Summary))
		d, err := r.review(ctx, reviewer, m, t, h)
		if err != nil {
			return r.cutShort(m, t, fmt.Errorf("review of %s: %w", t.ID, err))
		}
		s.say(MsgVerdict, reviewer.Name, m.Name, t.ID, string(d.Verdict)+": "+firstLine(d.Notes))
		switch d.Verdict {
		case VerdictAccepted:
			s.update(t, func(t *Ticket) { t.Status, t.Finished, t.Err = TicketDone, time.Now(), "" })
			s.task(m, t.ID, "closing out "+t.ID)
			r.tell(ctx, m, verdictText(d))
			return buildResult{m: m}
		case VerdictFailed:
			s.update(t, func(t *Ticket) {
				t.Status, t.Finished, t.Err = TicketFailed, time.Now(), "failed by the reviewer: "+d.Notes
			})
			r.tell(ctx, m, verdictText(d))
			return buildResult{m: m}
		case VerdictRework:
			s.update(t, func(t *Ticket) { t.Status, t.Round = TicketActive, t.Round+1; t.Notes = append(t.Notes, d.Notes) })
		default:
			s.update(t, func(t *Ticket) { t.Status = TicketActive; t.Notes = append(t.Notes, d.Notes) })
		}
		s.task(m, t.ID, "building "+t.ID)
		text = verdictText(d)
	}
}

// Ends a ticket a builder could not finish, failed unless the sprint stopped, then back to the queue
func (r *runner) cutShort(m *Member, t *Ticket, err error) buildResult {
	s := r.s
	if errors.Is(err, context.Canceled) {
		s.update(t, func(t *Ticket) { t.Status = TicketTodo })
		return buildResult{m: m, err: err}
	}
	s.update(t, func(t *Ticket) { t.Status, t.Finished, t.Err = TicketFailed, time.Now(), err.Error() })
	s.say(MsgFailure, m.Name, "", t.ID, err.Error())
	return buildResult{m: m, err: err}
}

// Reviewer judges one handoff, verdicts going one at a time through its session
func (r *runner) review(ctx context.Context, reviewer, builder *Member, t *Ticket, h Handoff) (Decision, error) {
	r.reviewMu.Lock()
	defer r.reviewMu.Unlock()
	s := r.s
	s.task(reviewer, t.ID, "reviewing "+t.ID)
	defer s.task(reviewer, "", "")
	snap := s.Snapshot()
	text := reviewOpening(snap.Brief, snap.Plan, snapTicket(snap, t.ID), h, builder.Name, !r.briefed)
	r.briefed = true
	check := func(v any) error {
		d := v.(Decision)
		switch {
		case d.Ticket != t.ID:
			return fmt.Errorf("the handoff under review is %s, give the verdict on it", t.ID)
		case d.Verdict == VerdictAccepted && h.Status != HandoffDone:
			return fmt.Errorf("%s reported %s, not done, give guidance or fail it", builder.Name, h.Status)
		case d.Verdict == VerdictRework && s.ticket(t.ID).Round >= r.cfg.Team.MaxRework:
			return fmt.Errorf("%s was sent back %d times, the most the sprint allows, accept it or fail it", t.ID, r.cfg.Team.MaxRework)
		}
		return nil
	}
	v, _, err := r.persist(ctx, reviewer, func() (any, protocol.Response, error) {
		return r.exchange(ctx, reviewer, text, ToolVerdict, r.cfg.Limits.Review, check)
	})
	if err != nil {
		return Decision{}, err
	}
	return v.(Decision), nil
}

// Director writes the report, the error that cut the build short told to it when there was one
func (r *runner) report(ctx context.Context, cause error) error {
	s := r.s
	s.enter(StageReport, "writing the report")
	d, err := r.member(ctx, RoleDirector, 0)
	if err != nil {
		return err
	}
	s.task(d, "", "writing the report")
	defer s.task(d, "", "")
	snap := s.Snapshot()
	_, resp, err := r.persist(ctx, d, func() (any, protocol.Response, error) {
		return r.exchange(ctx, d, reportOpening(snap.Brief, snap.Plan, snap.Tickets, cause), "", r.cfg.Limits.Brief, nil)
	})
	text := strings.TrimSpace(resp.Text())
	if err != nil && text == "" {
		return fmt.Errorf("report stage: %w", err)
	}
	s.setSummary(text)
	r.f.Note(d.Live, harness.Info, text)
	s.say(MsgNote, d.Name, Person, "", "report: "+firstLine(text))
	return nil
}

// Ticket of a snapshot by id, a bare one for an unknown id
func snapTicket(snap Snapshot, id string) Ticket {
	for _, t := range snap.Tickets {
		if t.ID == id {
			return t
		}
	}
	return Ticket{ID: id}
}
