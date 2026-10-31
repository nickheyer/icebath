package sprint

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
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

// Times the lead may end a run with reports waiting before the sprint stops nudging it
const maxNudges = 2

// Times a run that failed for a reason that may pass is started again before the sprint pauses or fails
const maxRetries = 3

// Waits before each retry of a failed run
var backoffs = [maxRetries]time.Duration{5 * time.Second, 20 * time.Second, time.Minute}

// Error of a stage once the whole sprint ran past its limit
var errExpired = errors.New("sprint ran past its limit")

// Error of a turn that ended with no text to read
var errSilent = errors.New("the run ended without an answer")

// Workflow that runs one sprint from a request
func workflow() harness.Workflow {
	return harness.Workflow{
		Name:        Name,
		Title:       "Sprint",
		Description: "settle a request with the project manager, plan it into tickets, then build it with engineers reporting to a lead",
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
	// Engineers by name, kept across build rounds
	engineers map[string]Member
	expired   atomic.Bool
	// Clock of the whole sprint, given another full limit when the person resumes an expired sprint
	clock *time.Timer
	// Ends the sprint once an expired sprint nobody can resume has wound down
	cancel context.CancelFunc
	// Error that stopped the lead from directing, failing the build round once its engineers are done
	leadErr error
	leadMu  sync.Mutex
	// The lead goes on in a fresh session and needs the plan again with its next message
	leadFresh bool
}

// Runs one sprint from a request
func run(ctx context.Context, f *harness.Flow, arg string) error {
	cfg := Current()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("sprint settings: %w", err)
	}
	s := newSprint(f.ID, arg, f.Started)
	register(s)
	r := &runner{cfg: cfg, f: f, s: s, engineers: map[string]Member{}}
	return r.drive(ctx, StageRefine)
}

// Starts an ended sprint again under the id of its first run, so it keeps its workspaces
func restart(ctx context.Context, app *harness.App, s *Sprint) error {
	if !s.Snapshot().Restartable() {
		return fmt.Errorf("sprint %s is not ended short of done", s.ID)
	}
	w := workflow()
	w.Run = func(ctx context.Context, f *harness.Flow, _ string) error { return resume(ctx, f, s) }
	_, err := app.Flows.Restart(ctx, s.ID, w, nil, s.Prompt)
	return err
}

// Stage a restarted sprint picks up at, the one it ended in or an earlier one whose outcome it lacks
func restartStage(snap Snapshot) Stage {
	from := snap.Last
	switch from {
	case StageReview, StageFailed, "":
		from = StageBuild
	}
	switch {
	case snap.Objective.Objective == "":
		return StageRefine
	case len(snap.Tickets) == 0:
		return StagePlan
	case snap.Briefing == "" && from != StageReport:
		return StageBrief
	}
	return from
}

// Drives an ended sprint again from the stage it was in, with the members whose sessions are still open
func resume(ctx context.Context, f *harness.Flow, s *Sprint) error {
	cfg := Current()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("sprint settings: %w", err)
	}
	r := &runner{cfg: cfg, f: f, s: s, engineers: map[string]Member{}}
	from := s.restart()
	s.pruneMembers(r.alive)
	for _, m := range s.Snapshot().Members {
		r.keep(m)
	}
	f.Notify(harness.Info, fmt.Sprintf("sprint %s restarts at the %s stage", s.ID, from))
	return r.drive(ctx, from)
}

// Runs the sprint from a stage to its end, reporting the outcome to the person either way
func (r *runner) drive(ctx context.Context, from Stage) error {
	whole, cancel := context.WithCancel(ctx)
	defer cancel()
	r.cancel = cancel
	r.clock = time.AfterFunc(time.Duration(r.cfg.Limits.Total), r.expire)
	defer r.clock.Stop()
	r.f.Panel(BoardKey)
	err := r.run(whole, ctx, from)
	if err != nil {
		r.s.finish(err, "")
		r.close(ctx, err)
		return err
	}
	accepted, unresolved := r.outcome()
	r.s.finish(nil, "")
	text := fmt.Sprintf("sprint %s done, %d of %d tickets accepted", r.s.ID, len(accepted), len(accepted)+len(unresolved))
	r.f.Notify(harness.Success, text)
	r.tell(harness.Success, text+". "+r.leftOpen())
	return nil
}

// Stages from the one given in order, each pausing for the person when it fails rather than ending the sprint
func (r *runner) run(ctx, parent context.Context, from Stage) error {
	steps := []struct {
		stage Stage
		fn    func(context.Context) error
	}{{StageRefine, r.refine}, {StagePlan, r.plan}, {StageBrief, r.brief}, {StageBuild, r.rounds}, {StageReport, r.deliver}}
	start := 0
	for i, step := range steps {
		if step.stage == from {
			start = i
		}
	}
	for _, step := range steps[start:] {
		if err := r.attempt(ctx, parent, step.stage, step.fn); err != nil {
			return err
		}
	}
	return nil
}

// Runs a stage until it passes, pausing for the person on a failure in an interactive sprint and failing otherwise
func (r *runner) attempt(ctx, parent context.Context, stage Stage, fn func(context.Context) error) error {
	for {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		if parent.Err() != nil {
			return parent.Err()
		}
		if !r.cfg.Interactive || ctx.Err() != nil {
			return r.failed(parent, stage, err)
		}
		if !r.await(ctx, stage, err) {
			return r.failed(parent, stage, err)
		}
	}
}

// Pauses the sprint on a failed stage until the person resumes it, reporting whether it may go on
func (r *runner) await(ctx context.Context, stage Stage, err error) bool {
	why, fix := r.reason(stage, err)
	r.s.pause(why)
	r.status("paused, " + why)
	r.tell(harness.Warn, fmt.Sprintf("Sprint %s paused: %s. %s, or press x there to stop it.", r.s.ID, why, fix))
	select {
	case <-r.s.resumed():
		r.lift()
		r.reopenAll(ctx)
		r.status(fmt.Sprintf("resumed at the %s stage", stage))
		return true
	case <-ctx.Done():
		r.s.unpause()
		return false
	}
}

// Why a stage failed and what the person does about it, in words for the pause
func (r *runner) reason(stage Stage, err error) (why, fix string) {
	resume := fmt.Sprintf("press r on the board (/%s) to resume", BoardKey)
	switch stuck := protocol.StuckError(err); {
	case r.expired.Load() || errors.Is(err, errExpired):
		limit := span(time.Duration(r.cfg.Limits.Total))
		return fmt.Sprintf("the sprint ran past its limit of %s during the %s stage", limit, stage), fmt.Sprintf("press r on the board (/%s) to give it another %s", BoardKey, limit)
	case stuck != nil:
		return fmt.Sprintf("the %s stage cannot run, an endpoint is %s: %s", stage, stuck.Why(), stuck.Message), fmt.Sprintf("fix the account, or set another profile in the sprint settings (/%s), then %s", SettingsKey, resume)
	case errors.Is(err, errBudget):
		return fmt.Sprintf("the %s stage stopped, %s", stage, err), fmt.Sprintf("raise the budget in the sprint settings (/%s), then %s", SettingsKey, resume)
	case errors.Is(err, agent.ErrRounds):
		return fmt.Sprintf("the %s stage stopped, %s", stage, err), fmt.Sprintf("raise max rounds in the sprint settings (/%s), then %s", SettingsKey, resume)
	}
	return fmt.Sprintf("the %s stage failed: %s", stage, err), resume + " and try the stage again"
}

// Clears the pause once the person resumed, an expired sprint getting its whole limit afresh
func (r *runner) lift() {
	r.s.unpause()
	if !r.expired.Load() {
		return
	}
	r.expired.Store(false)
	limit := time.Duration(r.cfg.Limits.Total)
	r.clock.Reset(limit)
	r.tell(harness.Info, fmt.Sprintf("Sprint %s has another %s.", r.s.ID, span(limit)))
}

// Opens fresh sessions for every member whose session was closed or whose role moved to another profile
func (r *runner) reopenAll(ctx context.Context) {
	for _, m := range r.s.Snapshot().Members {
		if _, moved, _ := r.ensure(ctx, m, true); moved && m.Role == RoleLead {
			r.leadFresh = true
		}
	}
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

// Reports whether a failed run may pass when started again, which rules out stops, refusals and limits reached
func transient(err error) bool {
	switch {
	case err == nil, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, agent.ErrInterrupted),
		errors.Is(err, agent.ErrRounds), errors.Is(err, errBudget), errors.Is(err, errSilent), errors.Is(err, errExpired), protocol.StuckError(err) != nil:
		return false
	}
	return true
}

// Waits out the backoff after a failed run of a member, reporting whether the run should be started again
func (r *runner) retry(ctx context.Context, m Member, err error, tries *int) bool {
	if !transient(err) || *tries >= maxRetries || ctx.Err() != nil || r.expired.Load() {
		return false
	}
	wait := backoffs[*tries]
	*tries++
	r.status(fmt.Sprintf("%s failed, trying again in %s", m.Name, span(wait)))
	text := fmt.Sprintf("%s failed and is started again in %s, try %d of %d: %s", m.Name, span(wait), *tries, maxRetries, err)
	r.note(m, harness.Warn, text)
	if m.Role != RolePM {
		r.tell(harness.Warn, text)
	}
	select {
	case <-time.After(wait):
		return true
	case <-ctx.Done():
		return false
	}
}

// Stops every run after its turn once the whole limit passed, stages then pausing, or the sprint ending when nobody can resume
func (r *runner) expire() {
	r.expired.Store(true)
	limit := span(time.Duration(r.cfg.Limits.Total))
	r.status(fmt.Sprintf("past its limit of %s, stopping after the current turns", limit))
	r.tell(harness.Error, fmt.Sprintf("Sprint %s ran past its limit of %s. Every run stops after the turn it is in.", r.s.ID, limit))
	for _, m := range r.s.Snapshot().Members {
		for _, run := range r.f.App.Control.Active() {
			if run.Parent == "" && m.Live != nil && run.Session == m.Live.Session {
				r.f.Interrupt(run)
			}
		}
	}
	if r.cfg.Interactive {
		return
	}
	deadline := time.Now().Add(windDown)
	for time.Now().Before(deadline) && r.working() {
		time.Sleep(pollInterval)
	}
	r.cancel()
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

// Tells every member session the sprint ended and how it restarts, then has the project manager report the failure
func (r *runner) close(parent context.Context, err error) {
	what := "Sprint " + r.s.ID + " failed: " + err.Error()
	if parent.Err() != nil {
		what = "Sprint " + r.s.ID + " was stopped"
	}
	r.f.Notify(harness.Error, what)
	again := fmt.Sprintf("Press r on the board (/%s) to restart it at the %s stage.", BoardKey, restartStage(r.s.Snapshot()))
	for _, m := range r.s.Snapshot().Members {
		if m.Role != RolePM {
			r.note(m, harness.Error, what+". Nothing waits for the sprint tools on this session until it restarts. "+again)
		}
	}
	r.tell(harness.Error, what+". "+again+" "+r.leftOpen())
	if parent.Err() != nil || r.pm.Live == nil {
		return
	}
	if rerr := r.report(parent, err); rerr != nil {
		r.tell(harness.Error, "The project manager could not report the outcome: "+rerr.Error())
	}
}

// Words telling the person where the team's sessions are once the sprint is over
func (r *runner) leftOpen() string {
	return "The team's sessions stay open in the workspaces of " + r.s.ID + " until you close them."
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

// Reports whether the session of a member is still open in the app
func (r *runner) alive(m Member) bool {
	return m.Live != nil && r.f.App.LiveOf(m.Live.Session) == m.Live
}

// Records a member as the one of its role the runner works with
func (r *runner) keep(m Member) {
	switch m.Role {
	case RolePM:
		r.pm = m
	case RoleLead:
		r.lead = m
	case RoleEngineer:
		r.engineers[m.Name] = m
	}
}

// Opens a session for a role under a name and makes it the member of that name
func (r *runner) open(ctx context.Context, role, name string) (Member, error) {
	live, err := r.f.Open(ctx, KindName(role))
	if err != nil {
		return Member{}, err
	}
	live.Session.SetTitle(name + " " + r.s.ID)
	m := Member{Role: role, Name: name, Live: live, Started: time.Now()}
	r.s.putMember(m)
	r.keep(m)
	return m, nil
}

// Member of the role under the name, opened when the sprint has none or its session was closed
func (r *runner) member(ctx context.Context, role, name string) (Member, error) {
	var have Member
	switch role {
	case RolePM:
		have = r.pm
	case RoleLead:
		have = r.lead
	case RoleEngineer:
		have = r.engineers[name]
	}
	if r.alive(have) {
		return have, nil
	}
	return r.open(ctx, role, name)
}

// Opens a fresh session for a member whose session was closed, or on resume whose role moved to another profile,
// reporting whether it did and failing when the fresh session cannot open
func (r *runner) ensure(ctx context.Context, m Member, profile bool) (Member, bool, error) {
	if m.Role == "" {
		return m, false, nil
	}
	why := ""
	switch want := r.profileOf(m.Role); {
	case !r.alive(m):
		why = m.Name + "'s session was closed, it goes on in a fresh one"
	case profile && m.Live.Endpoint.Profile.Name != want:
		why = fmt.Sprintf("%s goes on in a fresh session on profile %s", m.Name, want)
	default:
		return m, false, nil
	}
	fresh, err := r.open(ctx, m.Role, m.Name)
	if err != nil {
		r.tell(harness.Error, fmt.Sprintf("%s could not open a fresh session: %s", m.Name, err))
		return m, false, fmt.Errorf("%s could not open a fresh session: %w", m.Name, err)
	}
	r.note(fresh, harness.Info, why+".")
	if m.Role != RolePM {
		r.tell(harness.Info, why+".")
	}
	return fresh, true, nil
}

// Profile the role runs on now, the default profile when its kind names none
func (r *runner) profileOf(role string) string {
	if k, ok := r.f.App.Kind(KindName(role)); ok && k.Model != nil && k.Model.Profile != "" {
		return k.Model.Profile
	}
	return r.f.App.Primary.Profile.Name
}

func parts(text string) []protocol.Part { return []protocol.Part{protocol.Text{Text: text}} }

// Starts a run on the member once no run of the person goes on its session
func (r *runner) start(ctx context.Context, m Member, text string) (*harness.Run, error) {
	if err := r.idle(ctx, m.Live); err != nil {
		return nil, err
	}
	return r.f.Start(ctx, m.Live, "", parts(text))
}

// Runs the role until it calls the stage tool, nudging, retrying, wrapping up late and holding for the person
func (r *runner) handoff(ctx context.Context, m Member, toolName, text string, limit time.Duration, converses bool) (any, error) {
	opening := text
	w := await(m.Live.Session.ID(), toolName)
	defer func() { w.release(m.Live.Session.ID()) }()
	clock := time.NewTimer(limit)
	defer clock.Stop()
	late, tries := false, 0
	for attempt := 0; ; attempt++ {
		fresh, moved, err := r.ensure(ctx, m, false)
		if err != nil {
			return nil, err
		}
		if moved {
			w.release(m.Live.Session.ID())
			m, text = fresh, opening
			w = await(m.Live.Session.ID(), toolName)
		}
		run, err := r.start(ctx, m, text)
		if err != nil {
			if r.retry(ctx, m, err, &tries) {
				continue
			}
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
		case err != nil && r.retry(ctx, m, err, &tries):
			text = goOn(toolName)
			continue
		case !r.cfg.Interactive && err != nil:
			return nil, err
		case !r.cfg.Interactive:
			return nil, fmt.Errorf("the run ended without calling %s", toolName)
		}
		r.hold(m, "call "+toolName, err, converses && err == nil && !late)
		select {
		case v := <-w.ch:
			r.s.unpause()
			return v, nil
		case <-r.s.resumed():
			r.lift()
			text, tries = goOn(toolName), 0
			fresh, moved, err := r.ensure(ctx, m, true)
			if err != nil {
				return nil, err
			}
			if moved {
				w.release(m.Live.Session.ID())
				m, text = fresh, opening
				w = await(m.Live.Session.ID(), toolName)
			}
		case <-ctx.Done():
			r.s.unpause()
			return nil, ctx.Err()
		}
	}
}

// Runs one turn and hands back its text, asking again when silent, retrying, wrapping up late and holding on failure
func (r *runner) turn(ctx context.Context, m Member, text string, limit time.Duration, wrap, want string) (string, error) {
	opening := text
	tries := 0
	for attempt := 0; ; attempt++ {
		fresh, moved, err := r.ensure(ctx, m, false)
		if err != nil {
			return "", err
		}
		if moved {
			m, text = fresh, opening
		}
		run, err := r.start(ctx, m, text)
		var resp protocol.Response
		if err == nil {
			resp, err = r.watchTurn(ctx, m, run, limit, wrap)
		}
		if err == nil && strings.TrimSpace(resp.Text()) == "" {
			err = errSilent
		}
		if err == nil {
			return resp.Text(), nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if r.expired.Load() {
			return "", errExpired
		}
		if errors.Is(err, errSilent) && attempt == 0 {
			text = "You ended your turn without an answer. " + strings.ToUpper(want[:1]) + want[1:] + " now."
			continue
		}
		if r.retry(ctx, m, err, &tries) {
			text = "Go on from where you were and " + want + "."
			continue
		}
		if !r.cfg.Interactive {
			return "", err
		}
		r.hold(m, want, err, false)
		select {
		case <-r.s.resumed():
			r.lift()
			text, tries = "Go on from where you were and "+want+".", 0
			fresh, moved, err := r.ensure(ctx, m, true)
			if err != nil {
				return "", err
			}
			if moved {
				m, text = fresh, opening
			}
		case <-ctx.Done():
			r.s.unpause()
			return "", ctx.Err()
		}
	}
}

// Waits for the run of a member, steering it to wrap up past the limit, and hands back its response
func (r *runner) watchTurn(ctx context.Context, m Member, run *harness.Run, limit time.Duration, wrap string) (protocol.Response, error) {
	done, _ := r.f.App.Control.Done(run.ID)
	clock := time.NewTimer(limit)
	defer clock.Stop()
	for {
		select {
		case <-done:
			return run.Response(), run.Err()
		case <-ctx.Done():
			return protocol.Response{}, ctx.Err()
		case <-clock.C:
			r.f.Steer(run, parts(wrap)...)
			r.status(m.Name + " past its limit of " + span(limit) + ", told to wrap up")
			r.tell(harness.Warn, fmt.Sprintf("%s ran past its limit of %s and was told to wrap up.", m.Name, span(limit)))
		}
	}
}

// Tells a role whose stage ran past its limit to wrap up, and the person that it was told
func (r *runner) late(m Member, run *harness.Run, toolName string, limit time.Duration) {
	r.f.Steer(run, parts(wrapUp(toolName, limit))...)
	r.status(m.Name + " past its limit of " + span(limit) + ", told to wrap up")
	r.tell(harness.Warn, fmt.Sprintf("%s ran past its limit of %s and was told to wrap up and call %s.", m.Name, span(limit), toolName))
}

// Marks the sprint as waiting for the person on the member, saying why and what lifts it unless quiet
func (r *runner) hold(m Member, want string, err error, quiet bool) {
	r.s.setWaiting(m.Name)
	if quiet {
		r.status("waiting for the person on " + m.Name)
		return
	}
	resume := fmt.Sprintf("press r on the board (/%s) to resume", BoardKey)
	why := m.Name + " ended its run and did not " + want
	fix := fmt.Sprintf("tell %s to go on and %s in its session, or %s", m.Name, want, resume)
	switch stuck := protocol.StuckError(err); {
	case stuck != nil:
		why = fmt.Sprintf("%s cannot run, its endpoint is %s: %s", m.Name, stuck.Why(), stuck.Message)
		fix = fmt.Sprintf("fix the account, or set another profile for the %s in the sprint settings (/%s), then %s", m.Role, SettingsKey, resume)
	case errors.Is(err, errBudget):
		why = m.Name + " " + err.Error()
		fix = fmt.Sprintf("raise its budget in the sprint settings (/%s), then %s", SettingsKey, resume)
	case errors.Is(err, agent.ErrRounds):
		why = m.Name + " stopped, " + err.Error()
		fix = fmt.Sprintf("raise its max rounds in the sprint settings (/%s), then %s", SettingsKey, resume)
	case err != nil && !errors.Is(err, errSilent):
		why = m.Name + " " + ended(err) + ": " + err.Error()
		fix = resume + " and it is started again"
	}
	r.s.pause(why)
	r.status("paused, " + why)
	r.note(m, harness.Warn, fmt.Sprintf("%s. Sprint %s waits here: %s, or press x there to stop it.", why, r.s.ID, fix))
	if m.Role != RolePM {
		r.tell(harness.Warn, fmt.Sprintf("%s. Sprint %s waits in the session \"%s\": %s, or press x there to stop it.", why, r.s.ID, m.Name+" "+r.s.ID, fix))
	}
}

// Holds a build round whose engineers cannot run until the person fixes their endpoint and resumes the sprint
func (r *runner) holdBuild(ctx context.Context, stuck *protocol.Error) error {
	why := fmt.Sprintf("the engineers cannot run, their endpoint is %s: %s", stuck.Why(), stuck.Message)
	r.s.pause(why)
	r.status("paused, " + why)
	r.tell(harness.Warn, fmt.Sprintf("Sprint %s paused: %s. Fix the account, or set another profile for the engineer in the sprint settings (/%s), then press r on the board (/%s) to resume, or x there to stop.", r.s.ID, why, SettingsKey, BoardKey))
	select {
	case <-r.s.resumed():
		r.lift()
		for _, m := range r.engineers {
			r.ensure(ctx, m, true)
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
	fresh := !r.alive(r.pm)
	pm, err := r.member(ctx, RolePM, RolePM)
	if err != nil {
		return err
	}
	if fresh {
		r.tell(harness.Info, fmt.Sprintf("Sprint %s started. The team's sessions open in the workspaces of %s and the board is /%s. The project manager asks you here, with choices to pick in this prompt, until it hands the objective to the planner.", r.s.ID, r.s.ID, BoardKey))
	}
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

// Count of engineers the tickets call for under the ratio, within the team limit
func (r *runner) crew(tickets int) int {
	want := int(math.Ceil(float64(tickets) / r.cfg.Team.TicketsPerEngineer))
	return min(max(want, 1), r.cfg.Team.MaxEngineers)
}

// Brings the lead in to study the plan and brief the engineers before any of them starts
func (r *runner) brief(ctx context.Context) error {
	r.stage(StageBrief, "the lead is studying the plan")
	lead, err := r.member(ctx, RoleLead, RoleLead)
	if err != nil {
		return err
	}
	limit := time.Duration(r.cfg.Limits.Review)
	snap := r.s.Snapshot()
	r.tell(harness.Info, fmt.Sprintf("The lead is reading the plan and the repository in the session \"%s\" and will brief the engineers, limit %s.", RoleLead+" "+r.s.ID, span(limit)))
	text, err := r.turn(ctx, lead, leadBriefOpening(snap.Objective, snap.Story, snap.Tickets, r.crew(len(snap.Tickets))), limit, wrapUpBriefing(limit), "finish the briefing")
	if err != nil {
		return err
	}
	r.s.setBriefing(text)
	r.tell(harness.Success, "## Briefing from the lead\n\n"+strings.TrimSpace(text))
	return nil
}

// Build rounds the lead directs and reviews until the review sends nothing back
func (r *runner) rounds(ctx context.Context) error {
	for round := r.s.Snapshot().Round; ; round++ {
		if err := r.build(ctx, round); err != nil {
			return err
		}
		rework, err := r.review(ctx, round)
		if err != nil {
			return err
		}
		if !rework {
			return nil
		}
	}
}

// Builds every ticket still to do, engineers taking ready tickets as they free up and reporting to the lead
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
	if _, err := r.member(ctx, RoleLead, RoleLead); err != nil {
		return err
	}
	for i := 1; i <= r.crew(len(pending)) || i <= len(r.engineers); i++ {
		if _, err := r.member(ctx, RoleEngineer, fmt.Sprintf("%s-%d", RoleEngineer, i)); err != nil {
			return err
		}
	}
	limit := len(r.engineers)
	if !r.cfg.Team.Parallel {
		limit = 1
	}
	r.stage(StageBuild, fmt.Sprintf("round %d, %d tickets across %d engineers", round+1, len(pending), len(r.engineers)))
	r.tell(harness.Info, fmt.Sprintf("Build round %d: %d tickets across %d engineers reporting to the lead, limit %s.", round+1, len(pending), len(r.engineers), span(time.Duration(r.cfg.Limits.Build))))
	late := make(chan struct{})
	clock := time.AfterFunc(time.Duration(r.cfg.Limits.Build), func() { close(late) })
	defer clock.Stop()
	r.s.lateRound.Store(false)
	r.setLeadErr(nil)
	over, overseen := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(overseen)
		r.oversee(ctx, over, late)
	}()
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
				if !r.expired.Load() {
					r.block(pending, claimed)
				}
				break
			}
			if !r.cfg.Interactive {
				close(over)
				<-overseen
				return stuck
			}
			if err := r.holdBuild(ctx, stuck); err != nil {
				close(over)
				<-overseen
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
			close(over)
			<-overseen
			return ctx.Err()
		}
	}
	close(over)
	<-overseen
	if r.expired.Load() {
		return errExpired
	}
	if err := r.leadError(); err != nil {
		return err
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
	r.tell(harness.Info, fmt.Sprintf("Build round %d over: %d of %d tickets accepted by the lead, %d failed.", round+1, built, len(pending), failed))
	return nil
}

func (r *runner) setLeadErr(err error) {
	r.leadMu.Lock()
	r.leadErr = err
	r.leadMu.Unlock()
}

// Error that stopped the lead from directing this round, nil while it directs
func (r *runner) leadError() error {
	r.leadMu.Lock()
	defer r.leadMu.Unlock()
	return r.leadErr
}

// Brings each report to the lead as it comes and keeps the lead answering until the round is over
func (r *runner) oversee(ctx context.Context, over, late <-chan struct{}) {
	limit := time.Duration(r.cfg.Limits.Review)
	var run *harness.Run
	var done <-chan struct{}
	var clock, retry <-chan time.Time
	var queued []*report
	var backlog []string
	nudges, directed, tries := 0, r.s.directions(), 0
	for {
		select {
		case rep := <-r.s.inbox:
			queued = append(queued, rep)
		case <-done:
			err := run.Err()
			run, done, clock = nil, nil, nil
			switch {
			case err == nil:
				tries = 0
			case ctx.Err() != nil || r.expired.Load():
			case r.retry(ctx, r.lead, err, &tries):
				nudges = 0
			case !r.leadFailed(ctx, err):
				return
			default:
				tries, nudges = 0, 0
			}
		case <-clock:
			clock = nil
			r.late(r.lead, run, ToolDirect, limit)
		case <-late:
			late = nil
			r.s.lateRound.Store(true)
			if run != nil {
				r.f.Steer(run, parts(wrapUpDirect(time.Duration(r.cfg.Limits.Build)))...)
			}
		case <-retry:
			retry = nil
		case <-over:
			return
		case <-ctx.Done():
			return
		}
		if len(queued) > 0 {
			backlog = append(backlog, reportsText(queued))
			queued = nil
		}
		if open := r.s.openReports(); len(backlog) == 0 && run == nil && len(open) > 0 {
			if n := r.s.directions(); n != directed {
				nudges, directed = 0, n
			}
			if nudges++; nudges > maxNudges {
				if !r.leadFailed(ctx, fmt.Errorf("%w %d times with %s waiting", errSilent, nudges-1, strings.Join(reportIDs(open), ", "))) {
					return
				}
				nudges = 0
			}
			backlog = append(backlog, nudgeDirect(open))
		}
		if len(backlog) == 0 {
			continue
		}
		text := strings.Join(backlog, "\n\n")
		if run == nil {
			run = r.leadRun()
		}
		if run != nil {
			if err := r.f.Steer(run, parts(text)...); err == nil {
				backlog = nil
				if done == nil {
					done, _ = r.f.App.Control.Done(run.ID)
				}
				continue
			}
			run, done, clock = nil, nil, nil
		}
		if r.s.lateRound.Load() {
			text = wrapUpDirect(time.Duration(r.cfg.Limits.Build)) + "\n\n" + text
		}
		_, moved, err := r.ensure(ctx, r.lead, false)
		if moved || r.leadFresh {
			text = leadContext(r.s.Snapshot()) + "\n\n" + text
			r.leadFresh = false
		}
		var started *harness.Run
		if err == nil {
			started, err = r.f.Start(ctx, r.lead.Live, "", parts(text))
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !r.f.App.Busy(r.lead.Live) && !r.retry(ctx, r.lead, err, &tries) && !r.leadFailed(ctx, err) {
				return
			}
			retry = time.After(pollInterval)
			continue
		}
		backlog = nil
		run = started
		done, _ = r.f.App.Control.Done(run.ID)
		clock = time.After(limit)
	}
}

// Root run going on the session of the lead, nil while none is
func (r *runner) leadRun() *harness.Run {
	for _, run := range r.f.App.Control.Active() {
		if run.Parent == "" && r.lead.Live != nil && run.Session == r.lead.Live.Session {
			return run
		}
	}
	return nil
}

// Holds for the person when the lead cannot direct, else fails the open reports, reporting whether oversight goes on
func (r *runner) leadFailed(ctx context.Context, err error) bool {
	if !r.cfg.Interactive {
		why := "the lead " + ended(err) + ": " + err.Error()
		r.setLeadErr(errors.New(why))
		n := r.s.loseLead(why)
		r.tell(harness.Error, fmt.Sprintf("%s. %d reports waiting on the lead failed with it.", strings.ToUpper(why[:1])+why[1:], n))
		return false
	}
	r.hold(r.lead, "direct the engineers waiting for you", err, false)
	select {
	case <-r.s.resumed():
		r.lift()
		if _, moved, _ := r.ensure(ctx, r.lead, true); moved {
			r.leadFresh = true
		}
		return true
	case <-ctx.Done():
		r.s.unpause()
		return false
	}
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

// Runs one ticket on an engineer until the lead accepts or fails it, retrying failures that may pass and handing back
// the refusal of a stuck endpoint
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
	mine, _ := snap.Ticket(t.ID)
	opening := engineerOpening(snap.Objective, snap.Story, snap.Briefing, snap.Tickets, mine, deps)
	text, tries := opening, 0
	for attempt := 0; ; attempt++ {
		fresh, moved, err := r.ensure(ctx, m, false)
		if moved {
			m, text = fresh, opening
		}
		var run *harness.Run
		if err == nil {
			run, err = r.start(ctx, m, text)
		}
		if err == nil {
			err = r.watch(ctx, m, run, late)
		}
		if status := r.s.statusOf(t); status == TicketDone || status == TicketFailed {
			return nil
		}
		if stuck := protocol.StuckError(err); stuck != nil {
			r.s.update(t, func(t *Ticket) { t.Status, t.Err = TicketTodo, "" })
			return stuck
		}
		if err != nil && (r.expired.Load() || ctx.Err() != nil) {
			r.s.update(t, func(t *Ticket) { t.Status, t.Err = TicketTodo, "" })
			return nil
		}
		if err != nil && r.retry(ctx, m, err, &tries) {
			text = fmt.Sprintf("Go on with ticket %s from where you were and report with %s.", t.ID, ToolReport)
			continue
		}
		why := ""
		switch {
		case err != nil:
			why = ended(err) + ": " + err.Error()
		case r.leadError() != nil:
			why = r.leadError().Error()
		case attempt >= 1:
			why = "ended its run twice without a report the lead accepted"
		}
		if why != "" {
			r.s.update(t, func(t *Ticket) { t.Status, t.Err = TicketFailed, why })
			return nil
		}
		text = nudge(ToolReport)
	}
}

// Waits for the run of an engineer, telling it to finish and report once the build round ran past its limit
func (r *runner) watch(ctx context.Context, m Member, run *harness.Run, late <-chan struct{}) error {
	done, _ := r.f.App.Control.Done(run.ID)
	for {
		select {
		case <-done:
			return run.Err()
		case <-ctx.Done():
			return ctx.Err()
		case <-late:
			late = nil
			limit := time.Duration(r.cfg.Limits.Build)
			r.f.Steer(run, parts(wrapUpReport(limit))...)
			r.tell(harness.Warn, fmt.Sprintf("%s ran past the build limit of %s and was told to finish and report.", m.Name, span(limit)))
		}
	}
}

// Has the lead review the round as a whole, reporting whether any ticket went back for another round
func (r *runner) review(ctx context.Context, round int) (bool, error) {
	r.stage(StageReview, fmt.Sprintf("round %d", round+1))
	lead, err := r.member(ctx, RoleLead, RoleLead)
	if err != nil {
		return false, err
	}
	limit := time.Duration(r.cfg.Limits.Review)
	r.tell(harness.Info, fmt.Sprintf("Review round %d: the lead is checking the whole in the session \"%s\", limit %s.", round+1, RoleLead+" "+r.s.ID, span(limit)))
	snap := r.s.Snapshot()
	v, err := r.handoff(ctx, lead, ToolReview, reviewOpening(snap.Objective, snap.Story, snap.Tickets, round), limit, false)
	if err != nil {
		return false, err
	}
	in := v.(reviewInput)
	for _, back := range in.Rework {
		t := r.s.ticket(back.Ticket)
		if t == nil {
			continue
		}
		r.s.update(t, func(t *Ticket) {
			t.Status, t.Err = TicketRework, ""
			t.Round++
			t.Notes = append(t.Notes, back.Notes)
		})
	}
	r.s.mu.Lock()
	r.s.Round, r.s.Summary = round+1, in.Summary
	r.s.mu.Unlock()
	r.tell(harness.Info, fmt.Sprintf("Review round %d over: %d sent back.\n\n", round+1, len(in.Rework))+reviewText(in))
	return len(in.Rework) > 0, nil
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

// Has the project manager report the outcome to the person as the last stage
func (r *runner) deliver(ctx context.Context) error {
	r.stage(StageReport, "reporting to the person")
	return r.report(ctx, nil)
}

// Tells the project manager how it went, retrying a failed report, with the failure that ended the sprint when one did
func (r *runner) report(ctx context.Context, failure error) error {
	tries := 0
	for {
		err := r.reportOnce(ctx, failure)
		if err == nil || ctx.Err() != nil {
			return err
		}
		if !r.retry(ctx, r.pm, err, &tries) {
			return err
		}
	}
}

// Runs the closing turn of the project manager once the person's turns on its session are over
func (r *runner) reportOnce(ctx context.Context, failure error) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.cfg.Limits.Refine))
	defer cancel()
	pm, err := r.member(ctx, RolePM, RolePM)
	if err != nil {
		return err
	}
	accepted, unresolved := r.outcome()
	run, err := r.start(ctx, pm, pmClosing(r.s.Snapshot().Summary, accepted, unresolved, failure))
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
