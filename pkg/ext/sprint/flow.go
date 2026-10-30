package sprint

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/protocol"
)

// Time between looks at whether a session has finished the run going on it
const pollInterval = 500 * time.Millisecond

// Workflow that runs one sprint from a request
func workflow() harness.Workflow {
	return harness.Workflow{
		Name:        Name,
		Title:       "Sprint",
		Description: "settle a request with the project manager, plan it into tickets and build it with a sprint team",
		Arg:         "request",
		Run:         run,
	}
}

// One sprint going through its stages
type runner struct {
	cfg  Config
	f    *harness.Flow
	s    *Sprint
	pm   *harness.Live
	lead *harness.Live
	// Engineers by name, kept across review rounds
	engineers map[string]*harness.Live
}

func run(ctx context.Context, f *harness.Flow, arg string) error {
	cfg := Current()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("sprint settings: %w", err)
	}
	s := &Sprint{ID: f.ID, Prompt: arg, Started: f.Started, Stage: StageRefine, Status: "starting"}
	register(s)
	whole, cancel := context.WithTimeout(ctx, time.Duration(cfg.Limits.Total))
	defer cancel()
	r := &runner{cfg: cfg, f: f, s: s, engineers: map[string]*harness.Live{}}
	err := r.run(whole, ctx)
	if err != nil {
		s.finish(err, "")
		f.Notify(harness.Error, "sprint "+s.ID+": "+err.Error())
		return err
	}
	accepted, unresolved := r.outcome()
	s.finish(nil, "")
	f.Notify(harness.Success, fmt.Sprintf("sprint %s done, %d of %d tickets accepted", s.ID, len(accepted), len(accepted)+len(unresolved)))
	return nil
}

// Stages in order, each under its own limit inside the whole
func (r *runner) run(ctx, parent context.Context) error {
	if err := r.refine(ctx); err != nil {
		return r.failed(parent, StageRefine, r.cfg.Limits.Refine, err)
	}
	if err := r.plan(ctx); err != nil {
		return r.failed(parent, StagePlan, r.cfg.Limits.Plan, err)
	}
	for round := 0; ; round++ {
		if err := r.build(ctx, round); err != nil {
			return r.failed(parent, StageBuild, r.cfg.Limits.Build, err)
		}
		rework, err := r.review(ctx, round)
		if err != nil {
			return r.failed(parent, StageReview, r.cfg.Limits.Review, err)
		}
		if !rework || round >= r.cfg.Team.MaxRework {
			break
		}
	}
	if err := r.report(ctx); err != nil {
		return r.failed(parent, StageReport, r.cfg.Limits.Refine, err)
	}
	return nil
}

// Error of a stage with the stage and the limit it ran past named, the plain stop when the sprint was cancelled
func (r *runner) failed(parent context.Context, stage Stage, limit Duration, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		if r.s.Snapshot().Elapsed() >= time.Duration(r.cfg.Limits.Total) {
			return fmt.Errorf("sprint ran past its limit of %s during the %s stage", r.cfg.Limits.Total, stage)
		}
		return fmt.Errorf("%s stage ran past its limit of %s", stage, limit)
	}
	return fmt.Errorf("%s stage: %w", stage, err)
}

// Moves to a stage, telling the sprint and the workflow list
func (r *runner) stage(stage Stage, status string) {
	r.s.enter(stage, status)
	r.f.SetStatus(string(stage) + " " + status)
}

func (r *runner) status(text string) {
	r.s.setStatus(text)
	r.f.SetStatus(string(r.s.Snapshot().Stage) + " " + text)
}

// Opens a session for a role under a name and adds it to the team
func (r *runner) open(ctx context.Context, role, name string) (*harness.Live, error) {
	live, err := r.f.Open(ctx, KindName(role))
	if err != nil {
		return nil, err
	}
	live.Session.SetTitle("sprint " + r.s.ID + " " + name)
	r.s.addMember(Member{Role: role, Name: name, Live: live, Started: time.Now()})
	return live, nil
}

func parts(text string) []protocol.Part { return []protocol.Part{protocol.Text{Text: text}} }

// Starts a run with the text and waits for the tool the stage needs, reminding the role once when it ends without it
// and, when the person may answer, waiting through their turns on the session as well
func (r *runner) handoff(ctx context.Context, live *harness.Live, toolName, text string, withPerson bool) (any, error) {
	w := await(live.Session.ID(), toolName)
	defer w.release(live.Session.ID())
	for attempt := 0; ; attempt++ {
		run, err := r.f.Start(ctx, live, "", parts(text))
		if err != nil {
			return nil, err
		}
		done, _ := r.f.App.Control.Done(run.ID)
		select {
		case v := <-w.ch:
			return v, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
		}
		select {
		case v := <-w.ch:
			return v, nil
		default:
		}
		if err := run.Err(); err != nil {
			return nil, err
		}
		if withPerson {
			r.status("waiting for the person")
			select {
			case v := <-w.ch:
				return v, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if attempt > 0 {
			return nil, fmt.Errorf("the run ended without calling %s", toolName)
		}
		text = nudge(toolName)
	}
}

// Settles the objective with the project manager
func (r *runner) refine(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.cfg.Limits.Refine))
	defer cancel()
	r.stage(StageRefine, "settling the objective")
	pm, err := r.open(ctx, RolePM, RolePM)
	if err != nil {
		return err
	}
	r.pm = pm
	v, err := r.handoff(ctx, pm, ToolResolve, pmOpening(r.s.Prompt, r.cfg.Interactive), r.cfg.Interactive)
	if err != nil {
		return err
	}
	r.s.setObjective(v.(Objective))
	return nil
}

// Has the planner size the objective and cut it into tickets
func (r *runner) plan(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.cfg.Limits.Plan))
	defer cancel()
	r.stage(StagePlan, "sizing and cutting tickets")
	planner, err := r.open(ctx, RolePlanner, RolePlanner)
	if err != nil {
		return err
	}
	v, err := r.handoff(ctx, planner, ToolPlan, plannerOpening(r.s.Snapshot().Objective, r.cfg.Team.MaxTickets), false)
	if err != nil {
		return err
	}
	in := v.(planInput)
	tickets := make([]*Ticket, 0, len(in.Tickets))
	for i := range in.Tickets {
		t := in.Tickets[i]
		tickets = append(tickets, &t)
	}
	r.s.setPlan(in.Story, tickets)
	return nil
}

// Builds every ticket still to do, engineers taking ready tickets as they free up, their count following the ratio
func (r *runner) build(ctx context.Context, round int) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.cfg.Limits.Build))
	defer cancel()
	snap := r.s.Snapshot()
	var pending []*Ticket
	for _, t := range snap.Tickets {
		if t.Status == TicketTodo || t.Status == TicketRework {
			pending = append(pending, r.s.ticket(t.ID))
		}
	}
	if len(pending) == 0 {
		return nil
	}
	want := int(math.Ceil(float64(len(pending)) / r.cfg.Team.TicketsPerEngineer))
	want = min(max(want, 1), r.cfg.Team.MaxEngineers)
	for len(r.engineers) < want {
		name := fmt.Sprintf("%s-%d", RoleEngineer, len(r.engineers)+1)
		live, err := r.open(ctx, RoleEngineer, name)
		if err != nil {
			return err
		}
		r.engineers[name] = live
	}
	limit := len(r.engineers)
	if !r.cfg.Team.Parallel {
		limit = 1
	}
	r.stage(StageBuild, fmt.Sprintf("round %d, %d tickets across %d engineers", round+1, len(pending), len(r.engineers)))
	busy := map[string]bool{}
	claimed := map[string]bool{}
	results := make(chan string)
	active, finished := 0, 0
	for finished < len(pending) {
		for active < limit {
			t := r.ready(pending, claimed)
			if t == nil {
				break
			}
			name := r.free(busy, t.Engineer)
			if name == "" {
				break
			}
			claimed[t.ID], busy[name] = true, true
			active++
			go func(name string, t *Ticket) {
				r.ticket(ctx, name, t)
				results <- name
			}(name, t)
		}
		if active == 0 {
			r.block(pending, claimed)
			break
		}
		select {
		case name := <-results:
			active--
			finished++
			busy[name] = false
			done, total := r.s.Snapshot().Progress()
			r.status(fmt.Sprintf("round %d, %d of %d tickets built", round+1, done, total))
		case <-ctx.Done():
			for active > 0 {
				<-results
				active--
			}
			return ctx.Err()
		}
	}
	return nil
}

// Next unclaimed ticket whose dependencies are all built, nil when none can start
func (r *runner) ready(pending []*Ticket, claimed map[string]bool) *Ticket {
	for _, t := range pending {
		if claimed[t.ID] {
			continue
		}
		if r.depsBuilt(t) {
			return t
		}
	}
	return nil
}

// Reports whether every ticket this one depends on is done
func (r *runner) depsBuilt(t *Ticket) bool {
	snap := r.s.Snapshot()
	for _, id := range t.DependsOn {
		for _, d := range snap.Tickets {
			if d.ID == id && d.Status != TicketDone {
				return false
			}
		}
	}
	return true
}

// Free engineer, the one that worked the ticket before when it is free, empty when all are busy
func (r *runner) free(busy map[string]bool, prefer string) string {
	if prefer != "" && r.engineers[prefer] != nil && !busy[prefer] {
		return prefer
	}
	for i := 1; i <= len(r.engineers); i++ {
		name := fmt.Sprintf("%s-%d", RoleEngineer, i)
		if !busy[name] {
			return name
		}
	}
	return ""
}

// Fails every unclaimed ticket, which no engineer could start because something it depends on failed
func (r *runner) block(pending []*Ticket, claimed map[string]bool) {
	for _, t := range pending {
		if claimed[t.ID] {
			continue
		}
		r.s.update(t, func(t *Ticket) {
			t.Status = TicketFailed
			t.Err = "blocked, a ticket it depends on was not built"
		})
	}
}

// Runs one ticket on an engineer and records the report or the failure
func (r *runner) ticket(ctx context.Context, name string, t *Ticket) {
	live := r.engineers[name]
	r.s.update(t, func(t *Ticket) { t.Status, t.Engineer, t.Err = TicketActive, name, "" })
	snap := r.s.Snapshot()
	var deps []Ticket
	for _, d := range snap.Tickets {
		if contains(t.DependsOn, d.ID) {
			deps = append(deps, d)
		}
	}
	var mine Ticket
	for _, c := range snap.Tickets {
		if c.ID == t.ID {
			mine = c
		}
	}
	run, err := r.f.Start(ctx, live, "", parts(engineerOpening(snap.Objective, snap.Story, snap.Tickets, mine, deps)))
	if err == nil {
		var resp protocol.Response
		resp, err = r.f.Wait(ctx, run)
		if err == nil {
			r.s.update(t, func(t *Ticket) { t.Status, t.Report = TicketDone, resp.Text() })
			return
		}
	}
	r.s.update(t, func(t *Ticket) { t.Status, t.Err = TicketFailed, err.Error() })
}

// Has the lead review the built tickets, reporting whether any went back for rework
func (r *runner) review(ctx context.Context, round int) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.cfg.Limits.Review))
	defer cancel()
	r.stage(StageReview, fmt.Sprintf("round %d", round+1))
	if r.lead == nil {
		lead, err := r.open(ctx, RoleLead, RoleLead)
		if err != nil {
			return false, err
		}
		r.lead = lead
	}
	if err := r.idle(ctx, r.lead); err != nil {
		return false, err
	}
	snap := r.s.Snapshot()
	v, err := r.handoff(ctx, r.lead, ToolReview, leadOpening(snap.Objective, snap.Story, snap.Tickets, round), false)
	if err != nil {
		return false, err
	}
	in := v.(reviewInput)
	rework := false
	for _, verdict := range in.Verdicts {
		t := r.s.ticket(verdict.Ticket)
		if t == nil {
			continue
		}
		r.s.update(t, func(t *Ticket) {
			t.Round = round + 1
			if verdict.Status == "rework" {
				t.Status = TicketRework
				t.Notes = append(t.Notes, verdict.Notes)
				return
			}
			if t.Status != TicketFailed {
				t.Status = TicketDone
			}
		})
		if verdict.Status == "rework" {
			rework = true
		}
	}
	r.s.mu.Lock()
	r.s.Round, r.s.Summary = round+1, in.Summary
	r.s.mu.Unlock()
	return rework, nil
}

// Tickets accepted and the rest
func (r *runner) outcome() (accepted, unresolved []Ticket) {
	for _, t := range r.s.Snapshot().Tickets {
		if t.Status == TicketDone {
			accepted = append(accepted, t)
		} else {
			unresolved = append(unresolved, t)
		}
	}
	return accepted, unresolved
}

// Tells the project manager how it went once the person's turns on its session are over
func (r *runner) report(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.cfg.Limits.Refine))
	defer cancel()
	r.stage(StageReport, "reporting to the person")
	if err := r.idle(ctx, r.pm); err != nil {
		return err
	}
	accepted, unresolved := r.outcome()
	run, err := r.f.Start(ctx, r.pm, "", parts(pmClosing(r.s.Snapshot().Summary, accepted, unresolved)))
	if err != nil {
		return err
	}
	_, err = r.f.Wait(ctx, run)
	return err
}

// Waits until no run goes on the session, so the next one may start
func (r *runner) idle(ctx context.Context, live *harness.Live) error {
	for r.f.App.Busy(live) {
		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
