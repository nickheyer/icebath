package sprint

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/protocol"
)

// Time between looks at whether a session has finished the run going on it
const pollInterval = 500 * time.Millisecond

// Longest the sprint waits for interrupted runs to finish their turn once its whole limit has passed
const windDown = 5 * time.Minute

// Error of a stage once the whole sprint ran past its limit
var errExpired = errors.New("sprint ran past its limit")

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
	pm   Member
	lead Member
	// Engineers by name, kept across review rounds
	engineers map[string]Member
	expired   atomic.Bool
}

func run(ctx context.Context, f *harness.Flow, arg string) error {
	cfg := Current()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("sprint settings: %w", err)
	}
	s := &Sprint{ID: f.ID, Prompt: arg, Started: f.Started, Stage: StageRefine, Status: "starting"}
	register(s)
	whole, cancel := context.WithCancel(ctx)
	defer cancel()
	r := &runner{cfg: cfg, f: f, s: s, engineers: map[string]Member{}}
	clock := time.AfterFunc(time.Duration(cfg.Limits.Total), func() { r.expire(cancel) })
	defer clock.Stop()
	f.Panel(BoardKey)
	err := r.run(whole, ctx)
	if err != nil {
		s.finish(err, "")
		r.close(ctx, err)
		return err
	}
	accepted, unresolved := r.outcome()
	s.finish(nil, "")
	text := fmt.Sprintf("sprint %s done, %d of %d tickets accepted", s.ID, len(accepted), len(accepted)+len(unresolved))
	f.Notify(harness.Success, text)
	r.tell(harness.Success, text+". "+r.leftOpen())
	return nil
}

// Stages in order, each told to wrap up at its own limit inside the whole
func (r *runner) run(ctx, parent context.Context) error {
	if err := r.refine(ctx); err != nil {
		return r.failed(parent, StageRefine, err)
	}
	if err := r.plan(ctx); err != nil {
		return r.failed(parent, StagePlan, err)
	}
	for round := 0; ; round++ {
		if err := r.build(ctx, round); err != nil {
			return r.failed(parent, StageBuild, err)
		}
		rework, err := r.review(ctx, round)
		if err != nil {
			return r.failed(parent, StageReview, err)
		}
		if !rework || round >= r.cfg.Team.MaxRework {
			break
		}
	}
	r.stage(StageReport, "reporting to the person")
	if err := r.report(ctx, nil); err != nil {
		return r.failed(parent, StageReport, err)
	}
	return nil
}

// Error of a stage with the stage named, the plain stop when the person stopped the sprint
func (r *runner) failed(parent context.Context, stage Stage, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if r.expired.Load() || errors.Is(err, errExpired) {
		return fmt.Errorf("sprint ran past its limit of %s during the %s stage", r.cfg.Limits.Total, stage)
	}
	return fmt.Errorf("%s stage: %w", stage, err)
}

// Stops every run of the team after its current turn once the whole limit passed, then cancels the sprint
func (r *runner) expire(cancel context.CancelFunc) {
	r.expired.Store(true)
	r.status(fmt.Sprintf("past its limit of %s, stopping after the current turns", r.cfg.Limits.Total))
	r.tell(harness.Error, fmt.Sprintf("Sprint %s ran past its limit of %s. Every run stops after the turn it is in.", r.s.ID, span(time.Duration(r.cfg.Limits.Total))))
	for _, m := range r.s.Snapshot().Members {
		for _, run := range r.f.App.Control.Active() {
			if run.Parent == "" && m.Live != nil && run.Session == m.Live.Session {
				r.f.Interrupt(run)
			}
		}
	}
	deadline := time.Now().Add(windDown)
	for time.Now().Before(deadline) && r.working() {
		time.Sleep(pollInterval)
	}
	cancel()
}

// Reports whether a run goes on any session of the team
func (r *runner) working() bool {
	for _, m := range r.s.Snapshot().Members {
		if m.Live != nil && r.f.App.Busy(m.Live) {
			return true
		}
	}
	return false
}

// Tells every member session the sprint ended, then has the project manager report the failure to the person
func (r *runner) close(parent context.Context, err error) {
	what := "Sprint " + r.s.ID + " failed: " + err.Error()
	if parent.Err() != nil {
		what = "Sprint " + r.s.ID + " was stopped"
	}
	r.f.Notify(harness.Error, what)
	for _, m := range r.s.Snapshot().Members {
		if m.Role != RolePM {
			r.note(m, harness.Error, what+". Nothing waits for the sprint tools on this session any more.")
		}
	}
	r.tell(harness.Error, what+". "+r.leftOpen())
	if parent.Err() != nil || r.pm.Live == nil {
		return
	}
	if rerr := r.report(parent, err); rerr != nil {
		r.tell(harness.Error, "The project manager could not report the outcome: "+rerr.Error())
	}
}

// Words telling the person where the team's sessions are once the sprint is over
func (r *runner) leftOpen() string {
	return "The team's sessions stay open in workspace " + r.s.ID + " until you close them."
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

// Writes a note for the person into the project manager's transcript, their window onto the sprint
func (r *runner) tell(level harness.Level, text string) {
	r.note(r.pm, level, text)
}

// Writes a note for the person into the transcript of a member
func (r *runner) note(m Member, level harness.Level, text string) {
	if m.Live != nil {
		r.f.Note(m.Live, level, text)
	}
}

// Opens a session for a role under a name and adds it to the team
func (r *runner) open(ctx context.Context, role, name string) (Member, error) {
	live, err := r.f.Open(ctx, KindName(role))
	if err != nil {
		return Member{}, err
	}
	live.Session.SetTitle(name + " " + r.s.ID)
	m := Member{Role: role, Name: name, Live: live, Started: time.Now()}
	r.s.addMember(m)
	return m, nil
}

func parts(text string) []protocol.Part { return []protocol.Part{protocol.Text{Text: text}} }

// Starts a run with the text and waits for the tool the stage needs, telling the role to wrap up once the limit passes,
// reminding a role that ended without the call once unless it converses with the person, and in an interactive sprint
// waiting for the person to drive the session to the call instead of failing
func (r *runner) handoff(ctx context.Context, m Member, toolName, text string, limit time.Duration, converses bool) (any, error) {
	live := m.Live
	opening := text
	w := await(live.Session.ID(), toolName)
	defer func() { w.release(live.Session.ID()) }()
	clock := time.NewTimer(limit)
	defer clock.Stop()
	late := false
	for attempt := 0; ; attempt++ {
		run, err := r.f.Start(ctx, live, "", parts(text))
		if err != nil {
			return nil, err
		}
		done, _ := r.f.App.Control.Done(run.ID)
	running:
		for {
			select {
			case v := <-w.ch:
				return v, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-clock.C:
				late = true
				r.late(m, run, toolName, limit)
			case <-done:
				break running
			}
		}
		select {
		case v := <-w.ch:
			return v, nil
		default:
		}
		if r.expired.Load() {
			return nil, errExpired
		}
		err = run.Err()
		switch {
		case err == nil && !converses && attempt == 0:
			text = nudge(toolName)
			continue
		case !r.cfg.Interactive && err != nil:
			return nil, err
		case !r.cfg.Interactive:
			return nil, fmt.Errorf("the run ended without calling %s", toolName)
		}
		r.hold(m, toolName, err, converses && err == nil && !late)
		select {
		case v := <-w.ch:
			r.s.unpause()
			return v, nil
		case <-r.s.resumed():
			r.s.unpause()
			text = goOn(toolName)
			if fresh, moved := r.reopen(ctx, m); moved {
				w.release(live.Session.ID())
				m, live = fresh, fresh.Live
				w = await(live.Session.ID(), toolName)
				text = opening
			}
		case <-ctx.Done():
			r.s.unpause()
			return nil, ctx.Err()
		}
	}
}

// Opens a fresh session for a member whose role now names another profile than its session runs on, so a profile
// changed in the settings while the sprint waited takes effect, reporting whether it did
func (r *runner) reopen(ctx context.Context, m Member) (Member, bool) {
	want := r.f.App.Primary.Profile.Name
	if k, ok := r.f.App.Kind(KindName(m.Role)); ok && k.Model != nil && k.Model.Profile != "" {
		want = k.Model.Profile
	}
	if m.Live == nil || m.Live.Endpoint.Profile.Name == want {
		return m, false
	}
	fresh, err := r.open(ctx, m.Role, m.Name)
	if err != nil {
		r.tell(harness.Error, fmt.Sprintf("%s could not move to profile %s: %s", m.Name, want, err))
		return m, false
	}
	r.note(m, harness.Info, fmt.Sprintf("%s goes on in a fresh session on profile %s.", m.Name, want))
	r.tell(harness.Info, fmt.Sprintf("%s goes on in a fresh session on profile %s.", m.Name, want))
	switch m.Role {
	case RolePM:
		r.pm = fresh
	case RoleLead:
		r.lead = fresh
	case RoleEngineer:
		r.engineers[m.Name] = fresh
	}
	return fresh, true
}

// Tells a role whose stage ran past its limit to wrap up, and the person that it was told
func (r *runner) late(m Member, run *harness.Run, toolName string, limit time.Duration) {
	r.f.Steer(run, parts(wrapUp(toolName, limit))...)
	r.status(m.Name + " past its limit of " + span(limit) + ", told to wrap up")
	r.tell(harness.Warn, fmt.Sprintf("%s ran past its limit of %s and was told to wrap up and call %s.", m.Name, span(limit), toolName))
}

// Marks the sprint as waiting for the person on the session of a member, saying why and what lifts it unless the role
// simply asked them something
func (r *runner) hold(m Member, toolName string, err error, quiet bool) {
	r.s.setWaiting(m.Name)
	if quiet {
		r.status("waiting for the person on " + m.Name)
		return
	}
	why := m.Name + " ended its run without calling " + toolName
	fix := fmt.Sprintf("tell %s to go on and call %s in its session, or press r on the board (/%s) to send it on", m.Name, toolName, BoardKey)
	switch stuck := protocol.StuckError(err); {
	case stuck != nil:
		why = fmt.Sprintf("%s cannot run, its endpoint is %s: %s", m.Name, stuck.Why(), stuck.Message)
		fix = fmt.Sprintf("fix the account, or set another profile for the %s in the sprint settings (/%s), then press r on the board (/%s) to go on", m.Role, SettingsKey, BoardKey)
	case errors.Is(err, errBudget):
		why = m.Name + " " + err.Error()
		fix = fmt.Sprintf("raise its budget in the sprint settings (/%s), then press r on the board (/%s) to go on", SettingsKey, BoardKey)
	case err != nil:
		why = m.Name + " " + ended(err) + ": " + err.Error()
	}
	r.s.pause(why)
	r.status("paused, " + why)
	r.note(m, harness.Warn, fmt.Sprintf("%s. Sprint %s waits here: %s, or stop it with x on the board.", why, r.s.ID, fix))
	if m.Role != RolePM {
		r.tell(harness.Warn, fmt.Sprintf("%s. Sprint %s waits in the session \"%s\": %s, or stop it with x on the board.", why, r.s.ID, m.Name+" "+r.s.ID, fix))
	}
}

// Holds a build round whose engineers cannot run until the person fixes their endpoint and tells the sprint to go on
func (r *runner) holdBuild(ctx context.Context, stuck *protocol.Error) error {
	why := fmt.Sprintf("the engineers cannot run, their endpoint is %s: %s", stuck.Why(), stuck.Message)
	r.s.pause(why)
	r.status("paused, " + why)
	r.tell(harness.Warn, fmt.Sprintf("Sprint %s paused: %s. Fix the account, or set another profile for the engineer in the sprint settings (/%s), then press r on the board (/%s) to go on, or x to stop.", r.s.ID, why, SettingsKey, BoardKey))
	select {
	case <-r.s.resumed():
		r.s.unpause()
		for name, m := range r.engineers {
			if fresh, moved := r.reopen(ctx, m); moved {
				r.engineers[name] = fresh
			}
		}
		return nil
	case <-ctx.Done():
		r.s.unpause()
		return ctx.Err()
	}
}

// Verb for how a run came to its end, stopped when something cut it short and failed otherwise
func ended(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, agent.ErrInterrupted) {
		return "was stopped"
	}
	return "failed"
}

// Settles the objective with the project manager
func (r *runner) refine(ctx context.Context) error {
	r.stage(StageRefine, "settling the objective")
	pm, err := r.open(ctx, RolePM, RolePM)
	if err != nil {
		return err
	}
	r.pm = pm
	r.tell(harness.Info, fmt.Sprintf("Sprint %s started. The team's sessions open in workspace %s and the board is /%s. Talk to the project manager here until it hands the objective to the planner.", r.s.ID, r.s.ID, BoardKey))
	v, err := r.handoff(ctx, pm, ToolResolve, pmOpening(r.s.Prompt, r.cfg.Interactive), time.Duration(r.cfg.Limits.Refine), r.cfg.Interactive)
	if err != nil {
		return err
	}
	r.s.setObjective(v.(Objective))
	return nil
}

// Has the planner size the objective and cut it into tickets
func (r *runner) plan(ctx context.Context) error {
	r.stage(StagePlan, "sizing and cutting tickets")
	planner, err := r.open(ctx, RolePlanner, RolePlanner)
	if err != nil {
		return err
	}
	limit := time.Duration(r.cfg.Limits.Plan)
	r.tell(harness.Info, fmt.Sprintf("The planner is studying the repository and cutting tickets in the session \"%s\", limit %s.", RolePlanner+" "+r.s.ID, span(limit)))
	v, err := r.handoff(ctx, planner, ToolPlan, plannerOpening(r.s.Snapshot().Objective, r.cfg.Team.MaxTickets), limit, false)
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
	r.tell(harness.Success, "## Plan\n\n"+planText(in.Story, in.Tickets))
	return nil
}

// Builds every ticket still to do, engineers taking ready tickets as they free up, their count following the ratio
func (r *runner) build(ctx context.Context, round int) error {
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
		m, err := r.open(ctx, RoleEngineer, name)
		if err != nil {
			return err
		}
		r.engineers[name] = m
	}
	limit := len(r.engineers)
	if !r.cfg.Team.Parallel {
		limit = 1
	}
	r.stage(StageBuild, fmt.Sprintf("round %d, %d tickets across %d engineers", round+1, len(pending), len(r.engineers)))
	r.tell(harness.Info, fmt.Sprintf("Build round %d: %d tickets across %d engineers, limit %s.", round+1, len(pending), len(r.engineers), span(time.Duration(r.cfg.Limits.Build))))
	late := make(chan struct{})
	clock := time.AfterFunc(time.Duration(r.cfg.Limits.Build), func() { close(late) })
	defer clock.Stop()
	busy := map[string]bool{}
	claimed := map[string]bool{}
	results := make(chan outcome)
	active := 0
	var stuck *protocol.Error
	for {
		for active < limit && stuck == nil {
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
				results <- outcome{name: name, ticket: t, stuck: r.ticket(ctx, name, t, late)}
			}(name, t)
		}
		if active == 0 {
			if stuck == nil {
				r.block(pending, claimed)
				break
			}
			if !r.cfg.Interactive {
				return stuck
			}
			if err := r.holdBuild(ctx, stuck); err != nil {
				return err
			}
			stuck = nil
			continue
		}
		select {
		case out := <-results:
			active--
			busy[out.name] = false
			if out.stuck != nil {
				stuck = out.stuck
				delete(claimed, out.ticket.ID)
			}
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
	if r.expired.Load() {
		return errExpired
	}
	built, failed := 0, 0
	for _, t := range r.s.Snapshot().Tickets {
		switch t.Status {
		case TicketDone:
			built++
		case TicketFailed:
			failed++
		}
	}
	r.tell(harness.Info, fmt.Sprintf("Build round %d over: %d of %d tickets built, %d failed.", round+1, built, len(pending), failed))
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
	if prefer != "" && r.engineers[prefer].Live != nil && !busy[prefer] {
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

// What one engineer came back with, the refusal of its endpoint when that is what stopped it
type outcome struct {
	name   string
	ticket *Ticket
	stuck  *protocol.Error
}

// Runs one ticket on an engineer and records the report or the failure, handing the ticket back to the queue with
// the refusal when the endpoint refused the account
func (r *runner) ticket(ctx context.Context, name string, t *Ticket, late <-chan struct{}) *protocol.Error {
	m := r.engineers[name]
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
	run, err := r.f.Start(ctx, m.Live, "", parts(engineerOpening(snap.Objective, snap.Story, snap.Tickets, mine, deps)))
	if err == nil {
		var resp protocol.Response
		resp, err = r.watch(ctx, m, run, late)
		if err == nil {
			r.s.update(t, func(t *Ticket) { t.Status, t.Report = TicketDone, resp.Text() })
			return nil
		}
	}
	if stuck := protocol.StuckError(err); stuck != nil {
		r.s.update(t, func(t *Ticket) { t.Status, t.Err = TicketTodo, "" })
		return stuck
	}
	r.s.update(t, func(t *Ticket) { t.Status, t.Err = TicketFailed, ended(err)+": "+err.Error() })
	return nil
}

// Waits for the run of an engineer, telling it to finish and report once the build round ran past its limit
func (r *runner) watch(ctx context.Context, m Member, run *harness.Run, late <-chan struct{}) (protocol.Response, error) {
	done, _ := r.f.App.Control.Done(run.ID)
	for {
		select {
		case <-done:
			return run.Response(), run.Err()
		case <-ctx.Done():
			return protocol.Response{}, ctx.Err()
		case <-late:
			late = nil
			limit := time.Duration(r.cfg.Limits.Build)
			r.f.Steer(run, parts(wrapUpReport(limit))...)
			r.tell(harness.Warn, fmt.Sprintf("%s ran past the build limit of %s and was told to finish and report.", m.Name, span(limit)))
		}
	}
}

// Has the lead review the built tickets, reporting whether any went back for rework
func (r *runner) review(ctx context.Context, round int) (bool, error) {
	r.stage(StageReview, fmt.Sprintf("round %d", round+1))
	if r.lead.Live == nil {
		lead, err := r.open(ctx, RoleLead, RoleLead)
		if err != nil {
			return false, err
		}
		r.lead = lead
	}
	if err := r.idle(ctx, r.lead.Live); err != nil {
		return false, err
	}
	limit := time.Duration(r.cfg.Limits.Review)
	r.tell(harness.Info, fmt.Sprintf("Review round %d: the lead is checking the work in the session \"%s\", limit %s.", round+1, RoleLead+" "+r.s.ID, span(limit)))
	snap := r.s.Snapshot()
	v, err := r.handoff(ctx, r.lead, ToolReview, leadOpening(snap.Objective, snap.Story, snap.Tickets, round), limit, false)
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
	r.tell(harness.Info, fmt.Sprintf("Review round %d over: %d accepted, %d sent back.\n\n", round+1, len(in.Verdicts)-in.rework(), in.rework())+reviewText(in))
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

// Tells the project manager how it went once the person's turns on its session are over, with the failure when one ended it
func (r *runner) report(ctx context.Context, failure error) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.cfg.Limits.Refine))
	defer cancel()
	if err := r.idle(ctx, r.pm.Live); err != nil {
		return err
	}
	accepted, unresolved := r.outcome()
	run, err := r.f.Start(ctx, r.pm.Live, "", parts(pmClosing(r.s.Snapshot().Summary, accepted, unresolved, failure)))
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
