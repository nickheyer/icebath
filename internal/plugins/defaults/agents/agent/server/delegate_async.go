package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nickheyer/icebath/internal/config"
	delegationconfig "github.com/nickheyer/icebath/internal/delegation/config"
	"github.com/nickheyer/icebath/internal/git"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/fsx"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Finished units the ledger keeps, and for how long
	ledgerKept      = 50
	ledgerRetention = 7 * 24 * time.Hour
	// How far back a failed background task is listed in /agents status
	failedWindow = 24 * time.Hour
	// Lines and characters of a transcript a recovered unit quotes
	tailLines = 20
	tailChars = 2000
	// Times a result is offered to its session before it is dropped, and the oldest one still offered after a restart; either way it stays
	// in the ledger
	maxDeliveries = 8
	maxReplayAge  = 48 * time.Hour
	// How long a background unit may go without any of its subagents moving on before they are stopped, idle and inside a tool, how long
	// it then has to end before it is ended for it as stalled, and how often that is looked at
	stallIdle   = 450 * time.Second
	stallInTool = 1200 * time.Second
	stallGrace  = 120 * time.Second
	stallCheck  = 30 * time.Second
)

// Refusal of background work for a session that is closing
const closingRefusal = "This session was closed (exit or /new): no new background work."

// Why a call that asked for the background ran in it instead
const (
	syncNoAsync = "background=true is not available in this session — it cannot receive a detached subagent result after the turn ends (a finite " +
		"chat using -Q, --oneshot, or non-TTY stdio, `icebath -z`, a /bg background task, a cron job, a Kanban worker, or a stateless HTTP endpoint). " +
		"The subagent(s) ran SYNCHRONOUSLY and the result is included above."
	syncAtCapacity = "The background delegation pool was at capacity (delegation.max_concurrent_children), so the subagent(s) ran SYNCHRONOUSLY and " +
		"the result is included above. Raise delegation.max_concurrent_children in config.yaml to allow more concurrent background delegations."
	backgroundOne = "Subagent is running in the background; its full result re-enters the conversation as a new message when it finishes. Results " +
		"are delivered only after you END YOUR TURN: do anything that does not depend on it, then stop with a one-line status. Do not poll its " +
		"transcript or artifacts to wait for it."
	backgroundMany = "%d subagents are running in parallel in the background as %d completion unit(s); each unit's results re-enter the conversation " +
		"as their own new message when THAT unit finishes. Results are delivered only after you END YOUR TURN: do anything that does not depend on " +
		"them, then stop with a one-line status. Do not poll transcripts or artifacts to wait for them."
	controlHint = "While a child runs you can orchestrate it live with this same tool: delegate_task(action='list') to see live children, " +
		"action='steer' with subagent_id + message to redirect one, action='stop' with subagent_id to end one early."
	transcriptsHint = "Each subagent streams a human-readable transcript of its operations to the file listed above (append-only, one per task). " +
		"Read or `tail -f` these paths at any time to watch a child work while it runs."
)

// Background delegations of this process, each unit of a call running apart and told to its session as a message once it ends, and the
// ledger they are kept in so a restart knows of those it cut short
type background struct {
	a     *Agent
	mu    sync.Mutex
	units map[string]*unit
	// The stall watch runs while units do
	watching bool
}

// One background unit: the subagents of a call, or of one group of it, that report together
type unit struct {
	id     string
	b      *batch
	r      *record
	cancel context.CancelFunc
	// What its subagents last showed of their progress and when that changed; when they were stopped for going quiet, after how long and
	// whether inside a tool. Only the stall watch reads and writes these
	seen     []fingerprint
	moved    time.Time
	stalling time.Time
	quiet    time.Duration
	// Ends the unit once, its subagents returning or the stall watch giving up on them
	ended sync.Once
}

func newBackground(a *Agent) *background {
	return &background{a: a, units: map[string]*unit{}}
}

// A unit as the ledger keeps it
type record struct {
	ID          string            `json:"delegation_id"`
	Session     string            `json:"parent_session_id"`
	Profile     string            `json:"profile"`
	Goals       []string          `json:"goals"`
	Indexes     []int             `json:"task_indexes"`
	Context     string            `json:"context,omitempty"`
	Role        string            `json:"role"`
	Model       string            `json:"model,omitempty"`
	Group       *string           `json:"group,omitempty"`
	Transcripts map[string]string `json:"task_transcripts,omitempty"`
	Cwd         string            `json:"owner_cwd,omitempty"`
	// running, completed, error, unknown
	State      string    `json:"state"`
	Dispatched time.Time `json:"dispatched_at"`
	Completed  time.Time `json:"completed_at,omitzero"`
	PID        int       `json:"owner_pid"`
	Start      int64     `json:"owner_started_at"`
	Results    []*entry  `json:"results,omitempty"`
	// What the session is told of its end, secrets masked, whether it was told, and the times it was offered
	Message   string `json:"message,omitempty"`
	Title     string `json:"title,omitempty"`
	Delivered bool   `json:"delivered,omitempty"`
	Attempts  int    `json:"delivery_attempts,omitempty"`
}

// File of the ledger, one for every profile of the config folder
func (g *background) path() string {
	return filepath.Join(g.a.config.Dir(), "delegations.json")
}

// Changes the ledger under its lock, every process sharing the config folder seeing one change at a time
func (g *background) change(edit func(records []*record) []*record) {
	path := g.path()
	release, err := fsx.Lock(context.Background(), path+".lock", 10*time.Second)
	if err != nil {
		slog.Warn("The background delegation ledger could not be locked", "error", err)
		return
	}
	defer release()
	var records []*record
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &records); err != nil {
			slog.Warn("The background delegation ledger could not be read; it starts over", "error", err)
			records = nil
		}
	}
	records = edit(records)
	data, err := json.Marshal(records)
	if err == nil {
		err = fsx.Write(path, data, 0o600)
	}
	if err != nil {
		slog.Warn("The background delegation ledger could not be written", "error", err)
	}
}

func (g *background) read() []*record {
	data, err := os.ReadFile(g.path())
	if err != nil {
		return nil
	}
	var records []*record
	json.Unmarshal(data, &records)
	return records
}

// Keeps the finished records within their cap and age, those delivered going first
func prune(records []*record) []*record {
	cutoff := time.Now().Add(-ledgerRetention)
	records = slices.DeleteFunc(records, func(r *record) bool { return r.State != "running" && r.Completed.Before(cutoff) })
	finished := 0
	for _, r := range records {
		if r.State != "running" {
			finished++
		}
	}
	for _, delivered := range []bool{true, false} {
		for i := 0; i < len(records) && finished > ledgerKept; {
			if r := records[i]; r.State != "running" && r.Delivered == delivered {
				records = slices.Delete(records, i, i+1)
				finished--
				continue
			}
			i++
		}
	}
	return records
}

func (g *background) update(id string, edit func(r *record)) {
	g.change(func(records []*record) []*record {
		for _, r := range records {
			if r.ID == id {
				edit(r)
			}
		}
		return records
	})
}

// Live background units at once, a batch's units counting each
func (g *background) live() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.units)
}

// Records one finished subagent of a background unit, so a crash loses only those that did not finish
func (g *background) recordChild(b *batch, e *entry, honour bool) {
	if honour || b.unit == "" {
		return
	}
	g.update(b.unit, func(r *record) {
		r.Results = slices.DeleteFunc(r.Results, func(old *entry) bool { return old.TaskIndex == e.TaskIndex })
		r.Results = append(r.Results, forcedCopy(b.cfg, e))
	})
}

// Copy of an entry with its summary as disk keeps it, secrets always masked
func forcedCopy(cfg *config.Config, e *entry) *entry {
	copied := *e
	if copied.forced != "" {
		copied.Summary = text(copied.forced)
	} else if copied.Summary != nil {
		copied.Summary = text(childText(cfg, *copied.Summary, true))
	}
	copied.Error = childText(cfg, copied.Error, true)
	return &copied
}

// Tells the session at once of one failed subagent of a unit whose siblings still run
func (g *background) taskFailed(b *batch, e *entry) {
	lines := []string{fmt.Sprintf("[ASYNC DELEGATION TASK FAILED — %s, task %d/%d]", b.unit, e.TaskIndex+1, len(b.tasks)),
		"One subagent in a background fan-out you dispatched has failed while its siblings are still running. The batch's consolidated results will " +
			"still arrive when the last sibling finishes; this is an early warning so you can re-dispatch or investigate now instead of then."}
	if e.TaskIndex < len(b.tasks) {
		lines = append(lines, "Task: "+b.tasks[e.TaskIndex].goal)
	}
	status := fmt.Sprintf("Status: %s   Duration: %ss", e.Status, strconv.FormatFloat(e.Duration, 'f', -1, 64))
	if reason := strs.Head(strings.ReplaceAll(strings.TrimSpace(e.Error), "\n", " "), 400); reason != "" {
		status += "\nError: " + reason
	}
	lines = append(lines, status)
	if e.TaskIndex < len(b.writers) && b.writers[e.TaskIndex].path != "" {
		lines = append(lines, "Live transcript: "+b.writers[e.TaskIndex].path)
	}
	if err := g.a.deliver(b.parent, tools.Notice{Text: strings.Join(lines, "\n"), Title: "Subagent Task Failed: " + shortTitle(b.tasks[e.TaskIndex].goal)}); err != nil {
		slog.Debug("task failure notice failed", "error", err)
	}
}

// Units of a background call: one per group and one per ungrouped task when each reports on its own, else the call as one
func split(b *batch) []*batch {
	if !b.cfg.GetBool(delegationconfig.IndependentCompletions) {
		return []*batch{b}
	}
	type key struct {
		grouped bool
		name    string
		index   int
	}
	var order []key
	members := map[key][]*child{}
	for _, c := range b.children {
		if c == nil {
			continue
		}
		t := b.tasks[c.index]
		k := key{index: c.index}
		if t.grouped {
			k = key{grouped: true, name: t.group}
		}
		if _, seen := members[k]; !seen {
			order = append(order, k)
		}
		members[k] = append(members[k], c)
	}
	var units []*batch
	for _, k := range order {
		u := *b
		u.children = make([]*child, len(b.tasks))
		for _, c := range members[k] {
			u.children[c.index] = c
		}
		u.refused, u.group, u.grouped = nil, k.name, k.grouped
		units = append(units, &u)
	}
	return units
}

// Whether a session can hear of a unit after its turn ends: something can reach it later, and it is no one-shot run or scheduled job
func (a *Agent) laterResults(b *batch) bool {
	env := b.call.Env
	if env.Notify == nil || env.OneShot || tools.Scheduled(env.Platform) {
		return false
	}
	store, err := a.store()
	if err != nil {
		return false
	}
	session, err := store.Get(a.ctx, b.parent.sessionID())
	return err == nil && !tools.Scheduled(session.GetSource())
}

// Runs a background call: each unit handed to a goroutine of its own and the session told of each as it ends; the call runs in the turn
// when the session can't hear of it later or nothing more may run in the background
func (a *Agent) dispatch(ctx context.Context, b *batch) string {
	if !a.laterResults(b) {
		slog.Info("delegate_task: async delivery unsupported on this session runtime; running the batch synchronously instead.")
		return a.inline(ctx, b, syncNoAsync)
	}
	units := split(b)
	var dispatched []*batch
	var inline []*entry
	for k, u := range units {
		u.unit = b.delegation
		if len(units) != 1 && b.delegation != "" {
			u.unit = fmt.Sprintf("%s-%d", b.delegation, k+1)
		}
		if u.unit == "" {
			u.unit = "deleg_" + shortID()
		}
		if err := a.async.start(u); err != nil {
			if len(dispatched) == 0 {
				slog.Info(fmt.Sprintf("delegate_task: async pool at capacity (%s); running the whole batch synchronously instead.", err))
				return a.inline(ctx, b, syncAtCapacity)
			}
			slog.Warn(fmt.Sprintf("delegate_task: unit %d/%d not accepted (%s); running it inline.", k+1, len(units), err))
			inline = append(inline, a.execute(ctx, u, true).Results...)
			continue
		}
		dispatched = append(dispatched, u)
	}
	return tools.JSON(dispatchedPayload(b, dispatched, inline))
}

// Runs the call in the turn, saying why it did not go to the background
func (a *Agent) inline(ctx context.Context, b *batch, note string) string {
	result := a.execute(ctx, b, true)
	result.Note = note
	return tools.JSON(result)
}

// What the model is told of a background call it dispatched: what runs, as which units, and how to follow and steer it
func dispatchedPayload(b *batch, units []*batch, inline []*entry) map[string]any {
	goals := make([]string, len(b.tasks))
	for i, t := range b.tasks {
		goals[i] = t.goal
	}
	count := 0
	var ids []any
	for _, c := range b.children {
		if c != nil {
			count++
			ids = append(ids, c.id)
		}
	}
	id := b.delegation
	if id == "" && len(units) > 0 {
		id = units[0].unit
	}
	note := backgroundOne
	if count != 1 {
		note = fmt.Sprintf(backgroundMany, count, len(units))
	}
	payload := map[string]any{"status": "dispatched", "mode": "background", "count": count, "delegation_id": id, "goals": goals, "note": note}
	if len(units) > 1 {
		var list []map[string]any
		for _, u := range units {
			var indexes []int
			for _, c := range u.children {
				if c != nil {
					indexes = append(indexes, c.index)
				}
			}
			var group any
			if u.grouped {
				group = u.group
			}
			list = append(list, map[string]any{"delegation_id": u.unit, "group": group, "task_indexes": indexes})
		}
		payload["units"] = list
	}
	if len(ids) > 0 {
		payload["subagent_ids"], payload["control_hint"] = ids, controlHint
	}
	if len(b.refused) > 0 {
		payload["refused"] = b.refused
	}
	var paths []string
	for _, w := range b.writers {
		if w.path != "" {
			paths = append(paths, w.path)
		}
	}
	if len(paths) > 0 {
		payload["live_transcripts"], payload["live_transcripts_hint"] = paths, transcriptsHint
	}
	if len(inline) > 0 {
		payload["inline_results"] = inline
	}
	return payload
}

// Starts a unit in the background unless the pool is full or its session is closing, its record kept in the ledger
func (g *background) start(b *batch) error {
	a := b.a
	// A session closed while its turn dispatched takes no new background work
	if !a.holds(b.parent) {
		return errors.New(closingRefusal)
	}
	limit := maxChildren(b.cfg)
	ctx, cancel := context.WithCancel(a.ctx)
	g.mu.Lock()
	if len(g.units) >= limit {
		g.mu.Unlock()
		cancel()
		return fmt.Errorf("%d background delegations already run", len(g.units))
	}
	u := &unit{id: b.unit, b: b, cancel: cancel, moved: time.Now()}
	g.units[b.unit] = u
	g.mu.Unlock()
	start, _ := terminal.StartTime(os.Getpid())
	r := &record{ID: b.unit, Session: b.parent.sessionID(), Profile: b.cfg.Profile(), Context: b.context, Role: b.role, State: "running",
		Dispatched: time.Now(), PID: os.Getpid(), Start: start, Transcripts: map[string]string{}, Cwd: a.workspace(b.parent, b.cfg, b.call.Env.Cwd)}
	if b.grouped {
		r.Group = &b.group
	}
	for _, c := range b.children {
		if c == nil {
			continue
		}
		r.Goals = append(r.Goals, b.tasks[c.index].goal)
		r.Indexes = append(r.Indexes, c.index)
		r.Model = cmp.Or(r.Model, c.model)
		if c.transcript.path != "" {
			r.Transcripts[strconv.Itoa(c.index)] = c.transcript.path
		}
	}
	u.r = r
	g.change(func(records []*record) []*record { return append(prune(records), r) })
	a.working.Add(1)
	go func() {
		defer a.working.Done()
		defer cancel()
		result := a.execute(ctx, b, false)
		u.ended.Do(func() {
			g.mu.Lock()
			delete(g.units, b.unit)
			g.mu.Unlock()
			g.finish(b, r, result)
		})
	}()
	g.watch()
	return nil
}

// Starts the stall watch unless it runs; it ends once no unit is left or icebath closes. Called from a turn, which Close waits for
func (g *background) watch() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.watching {
		return
	}
	g.watching = true
	a := g.a
	a.working.Add(1)
	go func() {
		defer a.working.Done()
		ticker := time.NewTicker(stallCheck)
		defer ticker.Stop()
		for {
			select {
			case <-a.ctx.Done():
				g.mu.Lock()
				g.watching = false
				g.mu.Unlock()
				return
			case <-ticker.C:
			}
			if !g.sweep(time.Now()) {
				return
			}
		}
	}()
}

// One look at the background units: those whose subagents went quiet past their limit are stopped, and those stopped that did not end
// within the grace are ended as stalled; false once no unit is left to watch
func (g *background) sweep(now time.Time) bool {
	g.mu.Lock()
	if len(g.units) == 0 {
		g.watching = false
		g.mu.Unlock()
		return false
	}
	units := slices.Collect(maps.Values(g.units))
	g.mu.Unlock()
	for _, u := range units {
		if !u.stalling.IsZero() {
			if now.Sub(u.stalling) >= stallGrace {
				g.stalled(u)
			}
			continue
		}
		var seen []fingerprint
		inTool := false
		for _, c := range u.b.children {
			if c != nil {
				f := c.fingerprint()
				seen, inTool = append(seen, f), inTool || f.tool != ""
			}
		}
		if !slices.Equal(seen, u.seen) {
			u.seen, u.moved = seen, now
			continue
		}
		limit := stallIdle
		if inTool {
			limit = stallInTool
		}
		if quiet := now.Sub(u.moved); quiet >= limit {
			u.stalling, u.quiet = now, quiet
			slog.Warn(fmt.Sprintf("Async delegation %s made no progress for %.0fs (in_tool=%t) — interrupting; grace window %.0fs", u.id, quiet.Seconds(),
				inTool, stallGrace.Seconds()))
			for _, c := range u.b.children {
				if c != nil {
					c.interrupt("background delegation stalled")
				}
			}
		}
	}
	return true
}

// Ends a unit whose subagents did not return within the grace after they were stopped for going quiet, each reported stalled
func (g *background) stalled(u *unit) {
	u.ended.Do(func() {
		g.mu.Lock()
		delete(g.units, u.id)
		g.mu.Unlock()
		u.cancel()
		duration := seconds(time.Since(u.r.Dispatched))
		slog.Error(fmt.Sprintf("Async delegation %s force-finalized as stalled after %.0fs", u.id, duration))
		var results []*entry
		for _, c := range u.b.children {
			if c != nil {
				e := failedEntry(c.index, "stalled", stalledText(c.goal, u.quiet), duration)
				e.ExitReason = "stalled"
				results = append(results, e)
			}
		}
		g.finish(u.b, u.r, &combined{Results: results, TotalDuration: duration})
	})
}

// What the session is told of a task its subagent stopped responding to: which, after how long quiet, and what to do
func stalledText(goal string, quiet time.Duration) string {
	label := "The background task"
	if goal = strings.Join(strings.Fields(goal), " "); goal != "" {
		label = "Background task \"" + strs.Head(goal, 120) + "\""
	}
	silence := ""
	if quiet >= time.Minute {
		silence = fmt.Sprintf(" after %d min of no progress", int(math.RoundToEven(quiet.Minutes())))
	}
	return label + " stopped responding" + silence + " and was cancelled. Nothing else was affected; ask me to run it again if you still need it."
}

// Writes the end of a unit to the ledger and tells its session, the message kept to tell it again after a restart when this one fails
func (g *background) finish(b *batch, r *record, result *combined) {
	state := "error"
	if len(result.Results) == 0 || slices.ContainsFunc(result.Results, func(e *entry) bool { return done(e.Status) }) {
		state = "completed"
	}
	r.State, r.Completed = state, time.Now()
	message := g.a.batchMessage(b.cfg, r, result.Results, result.TotalDuration)
	title := displayTitle(r, result.Results)
	forced := make([]*entry, len(result.Results))
	for i, e := range result.Results {
		forced[i] = forcedCopy(b.cfg, e)
	}
	stored := g.a.batchMessage(b.cfg, &record{ID: r.ID, Goals: r.Goals, Indexes: r.Indexes, Context: r.Context, Role: r.Role, Model: r.Model, Group: r.Group,
		Dispatched: r.Dispatched, Completed: r.Completed}, forced, result.TotalDuration)
	g.update(r.ID, func(kept *record) {
		kept.State, kept.Completed, kept.Results, kept.Message, kept.Title = state, r.Completed, forced, childText(b.cfg, stored, true), title
	})
	err := g.a.deliver(b.parent, tools.Notice{Text: message, Title: title})
	g.update(r.ID, func(kept *record) {
		kept.Attempts++
		kept.Delivered = err == nil
	})
	if err == nil {
		return
	}
	slog.Warn("A background delegation's result could not reach its session; it is told again when icebath starts", "delegation", r.ID, "error", err)
}

// Message a finished unit re-enters its session with: what was dispatched and when, then each subagent's result in task order
func (a *Agent) batchMessage(cfg *config.Config, r *record, results []*entry, total float64) string {
	count := len(results)
	if count == 0 {
		count = len(r.Goals)
	}
	what := fmt.Sprintf("%d subagent(s)", count)
	if r.Group != nil {
		what = fmt.Sprintf("group '%s' (%d subagent(s))", *r.Group, count)
	}
	lines := preamble(r, fmt.Sprintf("[ASYNC DELEGATION BATCH COMPLETE — %s]", r.ID), fmt.Sprintf("A background fan-out unit you dispatched "+
		"earlier — %s — has finished; its consolidated results are below. Any other units from the same delegate_task call report separately as they "+
		"finish. You may have moved on since dispatching — act on these or re-dispatch if things have changed. If you are still waiting on siblings, "+
		"end your turn after acting on this one.", what))
	lines[len(lines)-1] += fmt.Sprintf("   Total duration: %ss", strconv.FormatFloat(total, 'f', -1, 64))
	if notice := a.modelRejected(cfg, results); notice != nil {
		lines = append(lines, notice...)
	}
	n := len(r.Goals)
	for _, e := range results {
		goal := ""
		if i := slices.Index(r.Indexes, e.TaskIndex); i >= 0 && i < len(r.Goals) {
			goal = r.Goals[i]
		}
		truncated := e.Truncated || e.ExitReason == "max_iterations"
		icon := "✗"
		switch {
		case truncated:
			icon = "⚠"
		case done(e.Status):
			icon = "✓"
		}
		header := fmt.Sprintf("--- %s TASK %d/%d", icon, e.TaskIndex+1, max(n, e.TaskIndex+1))
		if goal != "" {
			header += ": " + goal
		}
		header += fmt.Sprintf("  (status=%s", e.Status)
		if e.APICalls != 0 {
			header += fmt.Sprintf(", api_calls=%d", e.APICalls)
		}
		header += fmt.Sprintf(", %ss", strconv.FormatFloat(e.Duration, 'f', -1, 64))
		if truncated {
			header += ", TRUNCATED: hit max_iterations — work may be incomplete"
		}
		lines = append(lines, "", header+") ---")
		summary := e.summaryText()
		switch {
		case done(e.Status) && summary != "":
			if truncated {
				lines = append(lines, truncatedNote)
			}
			lines = append(lines, summary)
		case summary != "":
			if e.Error != "" {
				lines = append(lines, fmt.Sprintf("(%s: %s)", e.Status, e.Error))
			}
			lines = append(lines, "Partial output:", summary)
		default:
			line := fmt.Sprintf("(no summary — status=%s", e.Status)
			if e.Error != "" {
				line += ": " + e.Error
			}
			lines = append(lines, line+")")
		}
		if e.LiveTranscript != "" {
			lines = append(lines, "Full live transcript (complete tool/assistant trace): "+e.LiveTranscript)
		}
		lines = append(lines, processLines(e)...)
	}
	return strings.Join(lines, "\n")
}

// Lines every delegation message starts with: its title and intro, when it was dispatched, the context given, its role and model
func preamble(r *record, title, intro string) []string {
	lines := []string{title, intro, ""}
	completed := r.Completed
	if completed.IsZero() {
		completed = time.Now()
	}
	lines = append(lines, fmt.Sprintf("Dispatched: %s (%s ago)", r.Dispatched.Local().Format("2006-01-02 15:04:05"), terminal.Elapsed(completed.Sub(r.Dispatched))))
	if r.Context != "" {
		lines = append(lines, "Context you provided: "+r.Context)
	}
	line := "Role: " + cmp.Or(r.Role, "leaf")
	if r.Model != "" {
		line += "   Model: " + r.Model
	}
	return append(lines, line)
}

// Short title the windows show for a finished unit
func displayTitle(r *record, results []*entry) string {
	labels := map[string]string{"failed": "Failed", "error": "Failed", "cancelled": "Cancelled", "interrupted": "Interrupted", "timeout": "Timed Out",
		"stalled": "Stalled", "unknown": "Unknown", "rejected": "Failed"}
	var outcome, titles []string
	for _, e := range results {
		label := "Incomplete"
		switch {
		case done(e.Status) && !e.Truncated:
			label = "Completed"
		case labels[e.Status] != "":
			label = labels[e.Status]
		}
		outcome = append(outcome, label)
		goal := ""
		if i := slices.Index(r.Indexes, e.TaskIndex); i >= 0 && i < len(r.Goals) {
			goal = r.Goals[i]
		}
		titles = append(titles, strings.Join(strings.Fields(cmp.Or(goal, "Background task")), " "))
	}
	if len(results) == 0 {
		return "Subagent Tasks Unknown: " + shortTitle(strings.Join(r.Goals, "; "))
	}
	if len(results) == 1 {
		return fmt.Sprintf("Subagent Task %s: %s", outcome[0], shortTitle(titles[0]))
	}
	verdict := outcome[0]
	for _, o := range outcome[1:] {
		if o != verdict {
			verdict = "Finished with Issues"
			break
		}
	}
	title := ""
	if r.Group != nil {
		title = strings.Join(strings.Fields(*r.Group), " ")
	}
	return fmt.Sprintf("Subagent Tasks %s: %s (%d tasks)", verdict, shortTitle(cmp.Or(title, strings.Join(titles, "; "))), len(results))
}

func shortTitle(title string) string {
	if utf8.RuneCountInString(title) > 200 {
		return strs.Head(title, 197) + "..."
	}
	return title
}

// Stops the background units of the session, as it closes, /stop ends them or icebath exits; how many were stopped
func (g *background) stop(session string) int {
	g.mu.Lock()
	var stopped []*unit
	for _, u := range g.units {
		if session == "" || u.b.parent.sessionID() == session || u.b.call.SessionID == session {
			stopped = append(stopped, u)
		}
	}
	g.mu.Unlock()
	for _, u := range stopped {
		slog.Info(fmt.Sprintf("Interrupting async delegation %s (session ended)", u.id))
		u.cancel()
	}
	return len(stopped)
}

// Failed tasks of the session's background delegations of the last day, newest first
func (g *background) failed(cfg *config.Config, session string, lineage []string) []*agentv1.FailedDelegation {
	owners := append([]string{session}, lineage...)
	var out []*agentv1.FailedDelegation
	cutoff := time.Now().Add(-failedWindow)
	for _, r := range g.read() {
		if r.Profile != cfg.Profile() || !slices.Contains(owners, r.Session) || r.State == "running" || r.Completed.Before(cutoff) {
			continue
		}
		for _, e := range r.Results {
			switch e.Status {
			case "error", "failed", "failure", "timeout", "stalled", "unknown", "interrupted":
			default:
				continue
			}
			goal := ""
			if i := slices.Index(r.Indexes, e.TaskIndex); i >= 0 && i < len(r.Goals) {
				goal = r.Goals[i]
			}
			out = append(out, &agentv1.FailedDelegation{DelegationId: r.ID, TaskIndex: int32(e.TaskIndex), Status: e.Status, Goal: goal, Error: e.Error,
				Dispatched: timestamppb.New(r.Dispatched), Completed: timestamppb.New(r.Completed)})
		}
	}
	slices.SortStableFunc(out, func(x, y *agentv1.FailedDelegation) int {
		return y.GetCompleted().AsTime().Compare(x.GetCompleted().AsTime())
	})
	return out
}

// Whether the process that owned a record still runs: its pid alive with the start time it had
func ownerAlive(r *record) bool {
	if r.PID == os.Getpid() {
		return false
	}
	if r.PID <= 0 || !processAlive(r.PID) {
		return false
	}
	start, ok := terminal.StartTime(r.PID)
	return !ok || r.Start == 0 || start == r.Start
}

// As the server starts: units a process that died left running are told to their sessions as outcome unknown with what was recorded, and
// results never told are told again
func (g *background) recover(ctx context.Context) {
	var tell []*record
	g.change(func(records []*record) []*record {
		for _, r := range records {
			switch {
			case r.State == "running" && !ownerAlive(r):
				g.abandoned(r)
				tell = append(tell, r)
			case r.State == "running" || r.Delivered || r.Message == "":
			case r.Attempts >= maxDeliveries:
				slog.Warn(fmt.Sprintf("Async delegation %s exhausted its %d delivery attempts; marking terminally dropped (result remains queryable).", r.ID,
					maxDeliveries))
				r.Delivered = true
			case time.Since(r.Completed) > maxReplayAge:
				slog.Warn(fmt.Sprintf("Async delegation %s: pending completion is %.1fh old (cap %.1fh); terminally dropping the replay (result remains "+
					"queryable).", r.ID, time.Since(r.Completed).Hours(), maxReplayAge.Hours()))
				r.Delivered = true
			default:
				tell = append(tell, r)
			}
		}
		return prune(records)
	})
	for _, r := range tell {
		target, err := g.a.run(r.Session)
		if err != nil {
			slog.Warn("A background delegation's session is gone; its result is dropped", "delegation", r.ID, "session", r.Session, "error", err)
			g.update(r.ID, func(kept *record) { kept.Delivered = true })
			continue
		}
		err = g.a.deliver(target, tools.Notice{Text: r.Message, Title: r.Title})
		g.update(r.ID, func(kept *record) {
			kept.Attempts++
			kept.Delivered = err == nil
		})
		if err != nil {
			slog.Warn("A recovered background delegation's result could not reach its session", "delegation", r.ID, "error", err)
		}
	}
}

// Marks a unit whose owner died as outcome unknown, its recorded children kept and the rest unknown, with the last lines of each transcript
// and the state of the checkout it worked in
func (g *background) abandoned(r *record) {
	reason := "Delegation owner exited before recording a terminal result; outcome unknown."
	var results []*entry
	if len(r.Indexes) > 0 {
		recorded := 0
		for _, index := range r.Indexes {
			i := slices.IndexFunc(r.Results, func(e *entry) bool { return e.TaskIndex == index })
			if i >= 0 {
				results = append(results, r.Results[i])
				recorded++
				continue
			}
			results = append(results, failedEntry(index, "unknown", reason, 0))
		}
		if recorded > 0 {
			reason = fmt.Sprintf("Delegation owner exited before the unit finished; %d/%d child results were recorded and are included below, the rest are "+
				"unknown.", recorded, len(r.Indexes))
		}
	}
	last := r.State
	r.State, r.Completed, r.Results = "unknown", time.Now(), results
	lines := preamble(r, fmt.Sprintf("[ASYNC DELEGATION BATCH COMPLETE — %s]", r.ID), "A background fan-out unit you dispatched earlier — "+
		fmt.Sprintf("%d subagent(s)", len(r.Goals))+" — ended without its owner recording a result; what is known is below.")
	lines = append(lines, fmt.Sprintf("Last persisted unit status: %s (before owner exit; not current liveness). Unrecorded outcomes remain unknown; "+
		"inspect evidence before retrying side effects.", last))
	for _, index := range slices.Sorted(maps.Keys(r.Transcripts)) {
		lines = append(lines, fmt.Sprintf("Task index %s transcript (may be incomplete): %s", index, r.Transcripts[index]))
		if tail := transcriptTail(r.Transcripts[index]); tail != "" {
			lines = append(lines, fmt.Sprintf("--- last lines of task %s transcript ---", index), tail, "--- end ---")
		}
	}
	if hint := gitHint(r.Cwd); hint != "" {
		lines = append(lines, "Owner working tree at recovery: "+hint)
	}
	lines = append(lines, "--- ERROR ---", reason)
	for _, e := range results {
		lines = append(lines, "", fmt.Sprintf("--- TASK %d/%d  (status=%s) ---", e.TaskIndex+1, len(r.Goals), e.Status))
		if summary := e.summaryText(); summary != "" {
			lines = append(lines, summary)
		} else if e.Error != "" {
			lines = append(lines, "("+e.Error+")")
		}
	}
	r.Message = strings.Join(lines, "\n")
	r.Title = displayTitle(r, results)
}

// Last lines of a transcript, bounded in characters, empty when it can't be read
func transcriptTail(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return ""
	}
	f.Seek(max(0, size-int64(tailChars)*4), io.SeekStart)
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	all := strings.Split(strings.ToValidUTF8(string(data), "�"), "\n")
	tail := strings.TrimSpace(strings.Join(all[max(0, len(all)-tailLines):], "\n"))
	if runes := []rune(tail); len(runes) > tailChars {
		tail = "…" + string(runes[len(runes)-tailChars:])
	}
	return tail
}

// Branch, head and count of uncommitted files of the checkout the unit's owner worked in, empty outside one
func gitHint(cwd string) string {
	if info, err := os.Stat(cwd); cwd == "" || err != nil || !info.IsDir() {
		return ""
	}
	ctx := context.Background()
	head, ok := git.Output(ctx, cwd, 5*time.Second, "log", "-1", "--format=%h %s")
	if !ok {
		return ""
	}
	branch := "?"
	if b, ok := git.Output(ctx, cwd, 5*time.Second, "rev-parse", "--abbrev-ref", "HEAD"); ok && b != "" {
		branch = b
	}
	dirty := "?"
	if status, ok := git.Output(ctx, cwd, 5*time.Second, "status", "--porcelain"); ok {
		dirty = strconv.Itoa(len(git.Lines(status)))
	}
	return fmt.Sprintf("%s @ %s (%s uncommitted file(s)) in %s", strings.TrimSpace(branch), strings.TrimSpace(head), dirty, cwd)
}
