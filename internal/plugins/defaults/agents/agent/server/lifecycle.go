package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/tools"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

const (
	// How long a turn's hold on its session lasts unless refreshed, and how often it is
	leaseTTL     = 300 * time.Second
	leaseRefresh = 60 * time.Second
	// How long a turn waits for another process's turn on the same session, and how often it says so
	leaseWait   = 1800 * time.Second
	waitNotices = 15 * time.Second
	// How often this process tells the store it is alive
	beatEvery = 60 * time.Second
)

// Text of the note a turn cut short when the server stopped is picked up again with
const resumedNote = "[System note: Your previous turn was interrupted mid-run when icebath stopped. Continue it from where it left off.]"

// What the agent keeps of the sessions it holds besides their turns: the process it serves as, and the turns running now
type agentLife struct {
	backend sessions.Backend
	turns   map[string]time.Time
	// When the low disk question was last put to a window
	advised time.Time
	// Sessions dropped and ended once their grace passes, by id, with the reason they end for
	parting map[string]string
}

// What a session holds besides its turn: the windows watching it, the admissions of its queued messages, when it last did anything, and
// the timer that closes it once no window watches
type runLife struct {
	clients    map[string]int
	admissions []string
	active     time.Time
	closing    *time.Timer
	// Model route last recorded on the session
	route string
	// Admission of a turn cut short that runs again as the next turn
	resumed string
	// Model title to write once the turn ends, when it would share the turn's own local server
	title func()
	// Messages the store refused are kept in a file of their own, which the session was told of
	diverted bool
}

func newAgentLife() *agentLife {
	host, _ := os.Hostname()
	return &agentLife{turns: map[string]time.Time{}, parting: map[string]string{}, backend: sessions.Backend{ID: "backend-" + rand.Text()[:12], PID: os.Getpid(), Started: time.Now(), Host: host}}
}

// Holder of a turn of this process on the session, naming the process so a dead one's hold is reclaimed
func (a *Agent) holder(r *run) string {
	return a.holderOf(r.sessionID())
}

// Holder of a turn of this process on the session of the id
func (a *Agent) holderOf(id string) string {
	return fmt.Sprintf("pid=%d:backend=%s:session=%s", os.Getpid(), a.life.backend.ID, id)
}

// Brings a session back to life as it is first held by this process: reopened when it ended, its approvals and resume limit as stored
func (a *Agent) revive(ctx context.Context, store sessions.Store, session *sessionsv1.Session) error {
	if limit := int64(a.config.GetInt(sessionsconfig.MaxResumeMessages)); limit > 0 && session.GetMessages() > limit {
		return sessions.TooLong(session.GetMessages(), limit)
	}
	if session.GetEnded() != nil && session.GetEndReason() != sessions.EndCompression {
		if err := store.Reopen(ctx, session.GetId()); err != nil {
			return err
		}
	}
	return nil
}

// Closes the least recently active sessions no window watches and no turn runs while more are held than the live limit allows
func (a *Agent) evict() {
	limit := a.config.GetInt(sessionsconfig.MaxLive)
	if limit <= 0 {
		return
	}
	// A run's lock is never taken under the agent's, which a turn starting takes under its run's
	held := a.held()
	over := len(held) - limit
	if over <= 0 {
		return
	}
	type candidate struct {
		r      *run
		active time.Time
	}
	var idle []candidate
	for _, r := range held {
		r.mu.Lock()
		if len(r.life.clients) == 0 && !r.busy {
			idle = append(idle, candidate{r, r.life.active})
		}
		r.mu.Unlock()
	}
	slices.SortFunc(idle, func(x, y candidate) int { return x.active.Compare(y.active) })
	for _, c := range idle[:min(over, len(idle))] {
		a.close(c.r, sessions.EndClosed, 0)
	}
}

// Drops the session from those this process holds and ends its record after the grace, unless it is held again by then
func (a *Agent) close(r *run, reason string, grace time.Duration) {
	a.mu.Lock()
	dropped := a.runs[r.sessionID()] == r
	if dropped {
		delete(a.runs, r.sessionID())
	}
	a.mu.Unlock()
	if dropped {
		a.approvals.Unwatch(r.sessionID(), "the session ended before the prompt was answered")
	}
	id := r.sessionID()
	end := func() {
		a.mu.Lock()
		_, held := a.runs[id]
		a.mu.Unlock()
		if held {
			return
		}
		// What the tools kept for the session's last turn is let go with it
		env := &tools.Env{Config: a.settings(r)}
		unheard(id, a.endSession(a.ctx, id, reason, ""))
		if store, err := a.store(); err == nil {
			if session, err := store.Get(context.WithoutCancel(a.ctx), id); err == nil {
				env.Cwd = session.GetDirectory()
			}
			if a.contexts != nil {
				messages, _ := store.Messages(a.ctx, id, sessions.Model)
				a.contexts.Close(id, messages)
			}
			if err := store.End(context.WithoutCancel(a.ctx), id, reason); err != nil {
				slog.Warn("Could not end the session", "session", id, "error", err)
			}
		}
		a.tools.Ended(env, id)
		a.sessions.Release(context.WithoutCancel(a.ctx), id)
	}
	if grace <= 0 {
		end()
		return
	}
	a.mu.Lock()
	a.life.parting[id] = reason
	a.mu.Unlock()
	time.AfterFunc(grace, func() {
		a.mu.Lock()
		_, parting := a.life.parting[id]
		delete(a.life.parting, id)
		closing := a.ctx.Err() != nil
		a.mu.Unlock()
		// The shutdown ends the sessions still in their grace as icebath closes
		if parting && !closing {
			end()
		}
	})
}

// Counts a window watching the session, refused when it would hold more sessions open than a window or a profile may
func (a *Agent) attach(r *run, client string) error {
	if client == "" {
		client = "-"
	}
	r.mu.Lock()
	watched := len(r.life.clients) > 0
	_, mine := r.life.clients[client]
	profile := r.profile
	r.mu.Unlock()
	if !mine {
		perWindow, perProfile := max(a.config.GetInt(sessionsconfig.MaxLivePerConnection), 1), max(a.config.GetInt(sessionsconfig.MaxLivePerProfile), 1)
		windowCount, profileCount := 0, 0
		var oldest *run
		var since time.Time
		for _, other := range a.held() {
			if other == r {
				continue
			}
			other.mu.Lock()
			if other.life.clients[client] > 0 {
				windowCount++
			}
			if len(other.life.clients) > 0 && other.profile == profile {
				profileCount++
				if !other.busy && (oldest == nil || other.life.active.Before(since)) {
					oldest, since = other, other.life.active
				}
			}
			other.mu.Unlock()
		}
		name := a.named(r, "the new session")
		hint := ""
		if oldest != nil {
			hint = " (the oldest idle one is " + a.named(oldest, oldest.sessionID()) + ")"
		}
		switch {
		case windowCount >= perWindow:
			return fmt.Errorf("This window already has %d sessions open, so %s was not opened. Close a session first%s, or raise Sessions per window "+
				"in /settings under Sessions.", windowCount, name, hint)
		case !watched && profileCount >= perProfile:
			return fmt.Errorf("This profile already has %d sessions open, so %s was not opened. Close a session first%s, or raise Open session limit "+
				"in /settings under Sessions.", profileCount, name, hint)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.life.clients == nil {
		r.life.clients = map[string]int{}
	}
	r.life.clients[client]++
	if r.life.closing != nil {
		r.life.closing.Stop()
		r.life.closing = nil
	}
	a.aside(func() { a.advise(r) })
	return nil
}

// Puts the low disk question to the session's window, at most once a week from this process
func (a *Agent) advise(r *run) {
	a.mu.Lock()
	due := time.Since(a.life.advised) >= 7*24*time.Hour
	if due {
		a.life.advised = time.Now()
	}
	a.mu.Unlock()
	if !due {
		return
	}
	store, err := a.store()
	if err != nil {
		return
	}
	lines, ask := sessions.Advice(a.ctx, store, a.config, "/")
	if !ask {
		a.mu.Lock()
		a.life.advised = time.Time{}
		a.mu.Unlock()
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	a.notify(r, "⚠ "+strings.Join(lines, "\n\n"))
}

// Title of the session in quotes, or what to call it when it has none
func (a *Agent) named(r *run, fallback string) string {
	store, err := a.store()
	if err != nil {
		return fallback
	}
	session, err := store.Get(a.ctx, r.sessionID())
	if err != nil || session.GetTitle() == "" {
		return fallback
	}
	return "“" + session.GetTitle() + "”"
}

// Counts a window gone; once none watches, the session is closed after the disconnect grace if no turn runs, or once its turn ends and
// it sits idle past the idle disconnect limit
func (a *Agent) detach(r *run, client string) {
	if client == "" {
		client = "-"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.life.clients[client]--; r.life.clients[client] <= 0 {
		delete(r.life.clients, client)
	}
	if len(r.life.clients) > 0 {
		return
	}
	a.park(r, time.Duration(a.config.GetInt(sessionsconfig.OrphanReapGrace))*time.Second)
}

// Schedules the close of a session no window watches, r.mu held; a grace of zero keeps it parked until it is watched again
func (a *Agent) park(r *run, after time.Duration) {
	if after <= 0 {
		return
	}
	if r.life.closing != nil {
		r.life.closing.Stop()
	}
	r.life.closing = time.AfterFunc(after, func() {
		r.mu.Lock()
		watched, busy := len(r.life.clients) > 0, r.busy
		r.life.closing = nil
		r.mu.Unlock()
		// Sessions still held as icebath closes are ended by its shutdown
		if watched || busy || a.ctx.Err() != nil {
			return
		}
		a.close(r, sessions.EndDisconnected, time.Duration(a.config.GetInt(sessionsconfig.OrphanEndGrace))*time.Second)
	})
}

// Stores the message sent to the session as an admission, so a turn the server stops in the middle of is known when it starts again
func (a *Agent) admit(r *run, text string) {
	store, err := a.store()
	if err != nil {
		return
	}
	admission, err := store.Admit(a.ctx, r.sessionID(), text, a.life.backend.ID)
	if err != nil {
		slog.Warn("Could not record the message sent to the session", "session", r.sessionID(), "error", err)
		admission = &sessionsv1.Admission{}
	}
	r.life.admissions = append(r.life.admissions, admission.GetId())
}

// Refusal of a message to a session whose turn was lost when the server stopped, until it is acknowledged
func (a *Agent) blocked(ctx context.Context, r *run) error {
	store, err := a.store()
	if err != nil {
		return err
	}
	pending, err := store.Pending(ctx, r.sessionID())
	if err != nil {
		return err
	}
	var lost []string
	for _, admission := range pending {
		if admission.GetStatus() == "unknown" {
			lost = append(lost, admission.GetId())
		}
	}
	if len(lost) == 0 {
		return nil
	}
	return fmt.Errorf("A turn of this session was lost when icebath stopped in the middle of it (%s), so the session waits until it is "+
		"acknowledged. Acknowledge it with /discard, or `icebath sessions discard %s`; the lost message is not sent again.", strings.Join(lost, ", "), r.sessionID())
}

// Takes the session for one turn: a slot under the active session limit, the turn lease other processes respect, and the admission of the
// message marked started. The function returned gives them all back with the outcome
func (a *Agent) enter(ctx context.Context, r *run, admission string) (func(outcome string), error) {
	store, err := a.store()
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	// A subagent's turn runs inside its parent's, which already holds a place under the limit
	if limit := a.config.GetInt(sessionsconfig.MaxConcurrent); limit > 0 && len(a.life.turns) >= limit && r.child == nil {
		var oldest time.Time
		for _, started := range a.life.turns {
			if oldest.IsZero() || started.Before(oldest) {
				oldest = started
			}
		}
		count := len(a.life.turns)
		a.mu.Unlock()
		refuse(store, admission)
		return nil, fmt.Errorf("Icebath is at the active session limit (%d/%d). Held by: %d session(s), oldest %s ago. Try again when another session "+
			"finishes.", count, limit, count, time.Since(oldest).Round(time.Minute))
	}
	a.life.turns[r.sessionID()] = time.Now()
	a.mu.Unlock()
	leave := func() {
		a.mu.Lock()
		delete(a.life.turns, r.sessionID())
		a.mu.Unlock()
	}
	holder := a.holder(r)
	acquired, waited, err := a.lease(ctx, r, store, holder)
	if err != nil || !acquired {
		leave()
		refuse(store, admission)
		if err == nil {
			err = errors.New("⏳ Another Icebath process kept this session busy too long. Your message was not processed - wait for the other process " +
				"to finish, then send it again.")
		}
		return nil, err
	}
	if waited {
		r.mu.Lock()
		a.notify(r, "Session is free; loading the latest transcript...")
		r.mu.Unlock()
	}
	if admission != "" {
		if err := store.Begin(ctx, admission, a.life.backend.ID); err != nil {
			slog.Warn("Could not mark the turn started", "session", r.sessionID(), "error", err)
		}
	}
	refreshing, stop := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(leaseRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-refreshing.Done():
				return
			case <-ticker.C:
				// A compression that continued the conversation in a new session moved the lease onto it
				if ok, err := store.RefreshTurn(refreshing, r.sessionID(), a.holder(r), leaseTTL); err != nil || !ok {
					slog.Warn("Session turn lease lost", "session", r.sessionID(), "error", err)
				}
			}
		}
	}()
	return func(outcome string) {
		stop()
		// The next turn of this process holds the same lease, so giving it back need not hold the session up, unless icebath is closing
		id := r.sessionID()
		release := func() {
			if admission != "" {
				if err := store.Settle(context.Background(), admission, outcome); err != nil {
					slog.Warn("Could not settle the turn", "session", id, "error", err)
				}
			}
			if err := store.ReleaseTurn(context.Background(), id, a.holderOf(id)); err != nil {
				slog.Warn("Could not release the session turn lease", "session", id, "error", err)
			}
		}
		if !a.aside(release) {
			release()
		}
		leave()
		r.mu.Lock()
		r.life.active = time.Now()
		title := r.life.title
		r.life.title = nil
		if title != nil {
			a.aside(title)
		}
		if len(r.life.clients) == 0 {
			a.park(r, time.Duration(a.config.GetInt(sessionsconfig.OrphanActivityStale))*time.Second)
		}
		r.mu.Unlock()
	}, nil
}

// Settles the admission of a turn that was refused before it started
func refuse(store sessions.Store, admission string) {
	if admission != "" {
		if err := store.Settle(context.Background(), admission, "refused"); err != nil {
			slog.Warn("Could not settle a refused turn", "admission", admission, "error", err)
		}
	}
}

// Waits for the session's turn lease, telling the watchers while another process holds it; whether it was taken and whether it was waited for
func (a *Agent) lease(ctx context.Context, r *run, store sessions.Store, holder string) (bool, bool, error) {
	deadline := time.Now().Add(leaseWait)
	var started, told time.Time
	for {
		acquired, err := store.AcquireTurn(ctx, r.sessionID(), holder, leaseTTL)
		if err != nil || acquired {
			return acquired, !started.IsZero(), err
		}
		now := time.Now()
		if now.After(deadline) {
			return false, true, nil
		}
		if started.IsZero() {
			started = now
		}
		if told.IsZero() || now.Sub(told) >= waitNotices {
			text := "⏳ Another Icebath process is using this session; waiting for it to finish before starting your turn..."
			if !told.IsZero() {
				text = fmt.Sprintf("⏳ Still waiting for the other Icebath process on this session (%ds)...", int(now.Sub(started).Seconds()))
			}
			r.mu.Lock()
			a.notify(r, text)
			r.mu.Unlock()
			told = now
		}
		select {
		case <-ctx.Done():
			return false, true, errors.New("Stopped waiting for another Icebath process on this session. Your message was not processed.")
		case <-time.After(time.Second):
		}
	}
}

// Work of a long-lived server as it starts: telling the store this process is alive, then picking up the turns a server that stopped left
// in the middle, run again when fresh and enabled, else held until acknowledged
func (a *Agent) serve(ctx context.Context) {
	store, err := a.store()
	if err != nil {
		slog.Warn("Session store unavailable", "error", err)
		return
	}
	a.life.backend.Profile = a.config.Profile()
	if err := store.Beat(ctx, a.life.backend); err != nil {
		slog.Warn("Could not register this process with the session store", "error", err)
	}
	// Background delegations a process that stopped left behind are told to their sessions, and the retries sessions armed for their
	// logins to reset are armed again
	a.aside(func() { a.async.recover(ctx) })
	a.rearmWaits()
	a.aside(func() {
		ticker := time.NewTicker(beatEvery)
		defer ticker.Stop()
		for {
			select {
			case <-a.ctx.Done():
				return
			case <-ticker.C:
				if err := store.Beat(a.ctx, a.life.backend); err != nil {
					slog.Debug("Heartbeat failed", "error", err)
				}
			}
		}
	})
	backends, err := store.Backends(ctx)
	if err != nil {
		slog.Warn("Could not read the processes serving sessions", "error", err)
		return
	}
	var alive []string
	for _, b := range backends {
		if b.ID == a.life.backend.ID || b.Host == a.life.backend.Host && b.PID > 0 && processAlive(b.PID) {
			alive = append(alive, b.ID)
		} else if b.Host == a.life.backend.Host {
			store.EndBeat(ctx, b.ID)
		}
	}
	lost, err := store.Recover(ctx, alive)
	if err != nil {
		slog.Warn("Could not recover turns cut short", "error", err)
		return
	}
	freshness := time.Duration(a.config.GetInt(sessionsconfig.AutoContinueFreshness)) * time.Minute
	attempts := int32(a.config.GetInt(sessionsconfig.AutoContinueAttempts))
	for _, admission := range lost {
		if admission.GetStatus() == "queued" {
			if a.config.GetBool(sessionsconfig.AutoContinueEnabled) && time.Since(admission.GetCreated().AsTime()) <= freshness {
				if err := a.requeue(admission); err == nil {
					continue
				}
			}
			store.Settle(ctx, admission.GetId(), "dropped")
			continue
		}
		started := admission.GetCreated().AsTime()
		if admission.GetStarted() != nil {
			started = admission.GetStarted().AsTime()
		}
		if !a.config.GetBool(sessionsconfig.AutoContinueEnabled) || time.Since(started) > freshness || admission.GetAttempts() >= attempts {
			continue
		}
		if err := a.resume(ctx, store, admission); err != nil {
			slog.Warn("Could not resume an interrupted turn", "session", admission.GetSessionId(), "error", err)
		}
	}
}

// Runs a turn cut short again: tool calls it left without results get them, a note tells the model it was interrupted, and the turn goes on
func (a *Agent) resume(ctx context.Context, store sessions.Store, admission *sessionsv1.Admission) error {
	r, err := a.run(admission.GetSessionId())
	if err != nil {
		return err
	}
	messages, err := store.Messages(ctx, r.sessionID(), sessions.Model)
	if err != nil {
		return err
	}
	var missing []*sessionsv1.Message
	if n := len(messages); n > 0 && messages[n-1].GetRole() == sessionsv1.Role_ROLE_ASSISTANT {
		for _, call := range messages[n-1].GetToolCalls() {
			missing = append(missing, &sessionsv1.Message{Role: sessionsv1.Role_ROLE_TOOL, ToolCallId: call.GetId(), ToolName: call.GetName(), Text: "interrupted",
				Failed: true})
		}
	}
	missing = append(missing, &sessionsv1.Message{Role: sessionsv1.Role_ROLE_USER, Text: resumedNote, Display: "auto_continue"})
	if err := store.Append(ctx, r.sessionID(), sessions.Guard{}, missing...); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range missing {
		r.publish(messageEvent(m))
	}
	r.life.resumed = admission.GetId()
	r.again = true
	if r.busy {
		return nil
	}
	return a.start(r)
}

// What became of a turn, as its admission is settled with
func outcome(ctx context.Context, err error) string {
	switch {
	case ctx.Err() != nil:
		return "stopped"
	case err != nil:
		return "error"
	}
	return "complete"
}

// Sends a message again that was waiting its turn when the server stopped
func (a *Agent) requeue(admission *sessionsv1.Admission) error {
	r, err := a.run(admission.GetSessionId())
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queue = append(r.queue, queued{text: admission.GetText()})
	r.life.admissions = append(r.life.admissions, admission.GetId())
	if r.busy {
		return nil
	}
	return a.start(r)
}

// Whether the process is there, signal zero asking without sending anything
func processAlive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// Ends the sessions this process holds and tells the store it is gone, as the server stops
func (a *Agent) shutdown() {
	store, err := a.store()
	if err != nil {
		return
	}
	// Sessions held end as closed, those dropped in their grace as they were dropped
	a.mu.Lock()
	ending := maps.Clone(a.life.parting)
	// The hooks hear the sessions held end as icebath shuts down, those dropped as they were dropped
	finals := maps.Clone(a.life.parting)
	for id := range a.runs {
		ending[id], finals[id] = sessions.EndClosed, "shutdown"
	}
	clear(a.life.parting)
	a.mu.Unlock()
	ctx := context.Background()
	for id, reason := range ending {
		unheard(id, a.endSession(ctx, id, finals[id], ""))
		if err := store.End(ctx, id, reason); err != nil {
			slog.Debug("Could not end the session", "session", id, "error", err)
		}
	}
	store.EndBeat(ctx, a.life.backend.ID)
}

// Records the model and provider a turn runs on, once each time they change
func (a *Agent) recordRoute(ctx context.Context, r *run, model, provider, baseURL string) {
	r.mu.Lock()
	key := provider + "\x00" + model + "\x00" + baseURL
	same := r.life.route == key
	r.life.route = key
	r.mu.Unlock()
	if same {
		return
	}
	if store, err := a.store(); err == nil {
		if err := store.Update(ctx, r.sessionID(), sessions.Change{Model: &model, Provider: &provider, BaseURL: &baseURL}); err != nil {
			slog.Debug("Could not record the session's model", "session", r.sessionID(), "error", err)
		}
	}
}

// Keeps a message the store refused for good in a file of the session's own, telling the session once; the store's error otherwise
func (a *Agent) divert(r *run, err error, message *sessionsv1.Message) error {
	if !errors.Is(err, sessions.ErrHalted) {
		return err
	}
	path, divertErr := sessions.Divert(a.config.Dir(), r.sessionID(), message)
	if divertErr != nil {
		return errors.Join(err, divertErr)
	}
	if !r.life.diverted {
		r.life.diverted = true
		a.notify(r, "⚠ "+err.Error()+"\n\nThis session's messages are kept in "+path+" until the database is fixed.")
	}
	return nil
}
