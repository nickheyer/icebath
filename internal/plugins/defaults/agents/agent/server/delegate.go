package server

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/delegation"
	delegationconfig "github.com/nickheyer/icebath/internal/delegation/config"
	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/internal/tools"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// The agent's delegate_task tool: subagents run as sessions of their own through the agent's own turns
type delegates struct {
	*Agent
}

const (
	deliveryIndependent = "each ungrouped task / `group` returns on its own"
	deliveryJoined      = "one message per call"
	// Paragraph of how results come back for a session that waits for its subagents inside the call
	joinedDelivery = "Parallel children are joined and their results return in this tool call.\n\n"
	// Paragraph of how they come back to a session that hears of them later, with how the call's results are split into messages
	backgroundDelivery = "Sessions without a later-result consumer (including one-shot CLI and cron) join parallel children and return results in this " +
		"tool call. Otherwise runs in the background: dispatch returns live transcript paths and results re-enter as a new message when subagents " +
		"finish (%s). Background results are delivered only BETWEEN your turns: finish whatever does not depend on them, then give a one-line " +
		"status and END YOUR TURN. Never wait or poll on transcripts, artifact files, or CI for a child. While children run, `action` " +
		"(list/steer/stop) controls them live.\n\n"
	descriptionHead = "Spawn isolated subagents for parallel or context-heavy work. Each gets its own conversation, terminal session, and toolset, and " +
		"only its final summary returns to you. Pass every task in `tasks` — one entry spawns one subagent, several run in parallel (limit in the tasks " +
		"description).\n\n"
	descriptionUses = "USE FOR: reasoning-heavy subtasks, work that would flood your context, or independent parallel workstreams.\n" +
		"DO NOT USE FOR (use these instead):\n" +
		"- Mechanical multi-step work with no reasoning needed -> execute_code\n" +
		"- A single tool call -> call the tool directly\n" +
		"- Tasks needing user interaction -> subagents cannot ask questions\n" +
		"- Durable work that must survive this session -> cronjob or terminal(background=True, notify=True); /stop, /new, or process exit halts " +
		"running subagents (whole tree); each returns an 'interrupted' completion with partial output.\n\n" +
		"RULES:\n" +
		"- Children know nothing of this conversation: pass everything needed via 'context', including any required output language, tone, or style " +
		"(e.g. \"respond in Chinese\").\n" +
		"- Child summaries are SELF-REPORTS, not verified facts: a child claiming \"uploaded successfully\" or \"file written\" may be wrong. For " +
		"external side effects (uploads, remote writes, publishing), require a verifiable handle (URL, ID, absolute path) and verify it yourself before " +
		"telling the user the operation succeeded.\n" +
		"- Children cannot close tracked work: a child asked to close it returns findings instead; the parent applies the transition.\n"
	descriptionTail = "- Children inherit the parent model unless pinned via delegation.provider / delegation.model in config.yaml."
)

func (d delegates) Tools() []tools.Def {
	return []tools.Def{{
		Name:    "delegate_task",
		Toolset: "delegation",
		Title:   "Delegate",
		About:   "Hand parts of a task to subagents that work on them",
		Group:   "Subagents",
		Icon:    tools.IconAgent,
		Subject: "goal",
		Schema:  d.schema(nil),
		Dynamic: d.schema,
		Handler: d.delegate,
		// Subagents change files and the machine like the parent does
		Effect: tools.Mutates,
		// Subagents watch their own liveness, so no deadline of the agent cuts a call short
		Timeout: -1,
	}}
}

// Property of a schema: its type, then the keywords given, then its description
func param(kind, description string, extra map[string]any) map[string]any {
	out := map[string]any{"type": kind}
	maps.Copy(out, extra)
	out["description"] = description
	return out
}

// Whether the session waits for its subagents inside the call: a one-shot run, or a chat app session the settings keep inline
func joins(env *tools.Env) bool {
	if env == nil {
		return false
	}
	if env.OneShot {
		return true
	}
	return env.Config != nil && inlineChats(env.Config, env.Platform)
}

// Surfaces with no chat thread of a person's own
var nonMessaging = map[string]bool{"": true, "api_server": true, "cli": true, "codex": true, "gateway": true, "kanban": true, "local": true,
	"msgraph_webhook": true, "tool": true, "tui": true, "webhook": true, "oneshot": true, "subagent": true, "desktop": true}

// Whether a chat app session waits for the subagents of its own agent inside the turn: as the settings say, else when the chat shows no
// messages of the agent while it works, which no chat app here does
func inlineChats(settings *config.Config, platform string) bool {
	if nonMessaging[strings.ToLower(strings.TrimSpace(platform))] {
		return false
	}
	switch settings.GetString(delegationconfig.GatewayMode) {
	case delegationconfig.GatewayInline:
		return true
	case delegationconfig.GatewayBackground:
		return false
	}
	return false
}

// Schema of delegate_task for the session: its limits, its delivery, the backend's own task parameters and description, and the live
// controls left out where the session waits for its subagents inside the call
func (d delegates) schema(env *tools.Env) map[string]any {
	cfg := d.config
	if env != nil && env.Config != nil {
		cfg = env.Config
	}
	independent := cfg.GetBool(delegationconfig.IndependentCompletions)
	item := map[string]any{
		"goal": param("string", "What this subagent should accomplish. Be specific and self-contained — it knows nothing about your conversation history.", nil),
		"context": param("string", "Background THIS child needs: file paths, error messages, constraints. Each child sees only its own context — "+
			"repeat shared background in every task that needs it.", nil),
		"output_schema": param("object", "Optional JSON Schema this child's final answer must validate against (told to the child up front; parent "+
			"validates with one bounded correction retry; result gains schema_valid, plus schema_errors on failure — the child's raw text is still "+
			"returned as summary, never discarded). Keep it forgiving — require only fields you will read.", nil),
		"images": param("array", "Optional images this child must SEE (max 8): local file paths or http(s) URLs — e.g. a screenshot the user sent, a "+
			"design mock, a chart. Vision-capable children receive the pixels on their first turn; non-vision children get path hints for "+
			"vision_analyze. Text files do NOT belong here — put paths in 'context' instead.", map[string]any{"items": map[string]any{"type": "string"}}),
	}
	if independent {
		item["group"] = param("string", "Optional result-delivery bucket within this call (only when delegation.independent_completions is enabled; "+
			"otherwise the whole call returns as one message). Tasks sharing a group return together in ONE message; ungrouped tasks return "+
			"individually as each finishes. This does not order execution; if B needs A's output, dispatch B after A returns.", nil)
	}
	addendum := ""
	if backend, err := d.delegation.Active(cfg); err == nil {
		for _, p := range backend.Declared(cfg) {
			item[p.Name] = p.Schema
		}
		addendum = backend.Addendum(cfg)
	}
	properties := map[string]any{
		"tasks": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "object", "properties": item, "required": []string{"goal"}},
			"description": fmt.Sprintf("The task(s), up to %d in parallel for this user (set via delegation.max_concurrent_children). Each entry spawns one "+
				"subagent with isolated context and terminal session; a single task is a one-entry array. Required when spawning.", maxChildren(cfg))},
	}
	joined := joins(env)
	if !joined {
		properties["action"] = param("string", "Default 'spawn'. Live control of running children: 'list' = ids/goals/status/transcripts; 'steer' = "+
			"queue course-correction text into one child (subagent_id + message) without stopping it; 'stop' = end one child early (subagent_id; "+
			"partial result still returns). Control actions return immediately; goal/tasks are ignored unless spawning.",
			map[string]any{"enum": []string{"spawn", "list", "steer", "stop"}})
		properties["subagent_id"] = param("string", "Target for action='steer'/'stop' (ids from the spawn response or action='list').", nil)
		properties["message"] = param("string", "For action='steer': the course correction, appended to the child's next tool result mid-run. Be "+
			"directive and specific.", nil)
	}
	restrictions := "- Children cannot call delegate_task, clarify, memory, or cronjob.\n"
	if depth := maxSpawnDepth(cfg); depth >= 2 && cfg.GetBool(delegationconfig.OrchestratorEnabled) {
		restrictions = "- Children cannot call clarify, memory, or cronjob.\n" + fmt.Sprintf("- Children can themselves delegate while depth remains "+
			"(max_spawn_depth=%d); the runtime derives this from depth automatically.\n", depth)
	}
	delivery := deliveryJoined
	if independent {
		delivery = deliveryIndependent
	}
	description := descriptionHead + fmt.Sprintf(backgroundDelivery, delivery)
	if joined {
		description = descriptionHead + joinedDelivery
	}
	description += descriptionUses + restrictions + descriptionTail
	if addendum != "" {
		description += "\n" + addendum
	}
	return tools.Schema(description, []string{}, properties)
}

// Result of a call the delegation policy declined before any subagent was started
func refusedResult(reason string) string {
	return tools.JSON(map[string]any{"error": reason, "refused": true})
}

func errorResult(message string) string {
	return tools.JSON(map[string]any{"error": message})
}

// One call of delegate_task being carried out: its tasks, where each runs, its subagents and how they are reported
type batch struct {
	a      *Agent
	parent *run
	call   *tools.Call
	cfg    *config.Config
	tasks  []*task
	// Subagent of each task, nil for one refused
	children   []*child
	refused    []*entry
	delegation string
	writers    []*transcript
	started    time.Time
	// Context and role of the call itself
	context, role string
	// Completion bucket of a background unit, and its id
	group   string
	grouped bool
	unit    string
}

// Runs a delegate_task call: a control action on running children, or the subagents of its tasks spawned, in the call or in the background
func (d delegates) delegate(ctx context.Context, call *tools.Call) string {
	parent, err := d.run(call.SessionID)
	if err != nil {
		return errorResult("delegate_task requires a parent agent context.")
	}
	return d.Agent.delegateWith(ctx, parent, call, nil, false)
}

// Carries out a delegate_task call of the parent, its tasks routed by the route given in place of the backend's when one is, run in the
// background when forced even where the session's chat would join them
func (a *Agent) delegateWith(ctx context.Context, parent *run, call *tools.Call, route *delegation.Route, background bool) string {
	args := call.Args
	raw, _ := args["action"].(string)
	switch action := strings.ToLower(strings.TrimSpace(raw)); action {
	case "list", "steer", "stop":
		return a.control(ctx, call.SessionID, action, args)
	case "", "spawn":
	default:
		return errorResult(fmt.Sprintf("Unknown action '%s'. Use spawn (default), list, steer, or stop.", raw))
	}
	if a.tree.isPaused() {
		return refusedResult("Delegation spawning is paused. Clear the pause in /agents (`p`) or with `/agents resume` before retrying.")
	}
	cfg := call.Env.Config
	depth := 0
	if parent.child != nil {
		depth = parent.child.depth
	}
	if limit := maxSpawnDepth(cfg); depth >= limit {
		return refusedResult(fmt.Sprintf("Delegation depth limit reached (depth=%d, max_spawn_depth=%d). Raise delegation.max_spawn_depth in config.yaml if "+
			"deeper nesting is required (no hard ceiling, but each level multiplies API cost).", depth, limit))
	}
	topRole := normalizeRole(args["role"])
	if requested, ok := args["max_iterations"]; ok && requested != nil {
		slog.Debug(fmt.Sprintf("delegate_task: ignoring caller-supplied max_iterations=%v; using delegation.max_iterations=%d from config", requested,
			cfg.GetInt(delegationconfig.MaxIterations)))
	}
	tasks, refusal := parseTasks(args, topRole, a.maxChildren(cfg))
	if refusal != "" {
		return errorResult(refusal)
	}
	var places []*placement
	if route != nil {
		p, err := a.resolveRoute(parent, cfg, *route, delegation.Default)
		if err != nil {
			return errorResult(err.Error())
		}
		for range tasks {
			copied := *p
			places = append(places, &copied)
		}
	} else if places, refusal = a.place(parent, cfg, tasks); refusal != "" {
		return refusedResult(refusal)
	}
	b := &batch{a: a, parent: parent, call: call, cfg: cfg, tasks: tasks, children: make([]*child, len(tasks)), started: time.Now(),
		context: argText(args, "context"), role: topRole}
	var reasons []string
	for i, p := range places {
		if p.refusal == "" {
			continue
		}
		b.refused = append(b.refused, &entry{TaskIndex: i, Status: "error", Error: p.refusal, Route: p.report})
		if !slices.Contains(reasons, p.refusal) {
			reasons = append(reasons, p.refusal)
		}
	}
	if len(b.refused) == len(tasks) {
		return refusedResult(strings.Join(reasons, " "))
	}
	if refusal := a.oneshotBudget(parent, cfg, call.Env, len(tasks)-len(b.refused)); refusal != "" {
		return refusedResult(refusal)
	}
	first := places[slices.IndexFunc(places, func(p *placement) bool { return p.refusal == "" })]
	model, providerName := first.model, first.provider
	b.delegation, b.writers = a.transcripts(ctx, cfg, tasks, model, providerName)
	for _, e := range b.refused {
		b.writers[e.TaskIndex].finalize(cfg, e)
	}
	updateManifest(cfg, b.delegation, b.refused, false)
	for i, t := range tasks {
		if places[i].refusal != "" {
			continue
		}
		c, err := a.spawn(ctx, b, t, places[i], b.writers[i])
		if err != nil {
			for _, built := range b.children {
				if built != nil {
					built.cleanup()
				}
			}
			return errorResult(err.Error())
		}
		b.children[i] = c
	}
	if parent.child == nil && (background || !joins(call.Env)) {
		return a.dispatch(ctx, b)
	}
	return tools.JSON(a.execute(ctx, b, true))
}

// Subagents the settings let one call start, warned of once when set above ten
func (a *Agent) maxChildren(cfg *config.Config) int {
	n := maxChildren(cfg)
	if n > 10 && cfg.Stored(delegationconfig.MaxConcurrent) != nil {
		a.tree.mu.Lock()
		warned := a.tree.warned
		a.tree.warned = true
		a.tree.mu.Unlock()
		if !warned {
			slog.Warn(fmt.Sprintf("%d parallel subagents: each one uses model tokens on its own, so cost grows with the number.", n))
		}
	}
	return n
}

// Refusal once a one-shot session would start more subagents than delegation.oneshot_max_children lets it in all, the count charged
// otherwise
func (a *Agent) oneshotBudget(parent *run, cfg *config.Config, env *tools.Env, requested int) string {
	if !env.OneShot {
		return ""
	}
	limit := max(cfg.GetInt(delegationconfig.OneshotMaxChildren), 0)
	if limit == 0 {
		return ""
	}
	top := parent
	for top.child != nil {
		top = top.child.parent
	}
	top.mu.Lock()
	defer top.mu.Unlock()
	if top.spawned+requested > limit {
		return fmt.Sprintf("Delegation budget for this one-shot run is exhausted (%d/%d subagents used; delegation.oneshot_max_children). Do the "+
			"remaining work yourself in this session — reviewing your own diff and running the tests inline is expected here, not a delegated review.",
			top.spawned, limit)
	}
	top.spawned += requested
	return ""
}

// Runs the subagents of a call to their ends, several at once up to the limit, and gives what they came to; a parent's stop, when it is
// honoured, takes the results of those still running as interrupted
func (a *Agent) execute(ctx context.Context, b *batch, honour bool) *combined {
	var running []*child
	for _, c := range b.children {
		if c != nil {
			running = append(running, c)
		}
	}
	results := make([]*entry, 0, len(running))
	if len(running) == 1 {
		e := running[0].execute(ctx)
		results = append(results, e)
		a.async.recordChild(b, e, honour)
	} else {
		results = a.parallel(ctx, b, running, honour)
	}
	slices.SortStableFunc(results, func(x, y *entry) int { return x.TaskIndex - y.TaskIndex })
	a.finalize(ctx, b, results)
	out := &combined{TotalDuration: seconds(time.Since(b.started))}
	for _, e := range results {
		if e.TaskIndex < len(b.writers) && b.writers[e.TaskIndex].path != "" {
			b.writers[e.TaskIndex].finalize(b.cfg, e)
			e.LiveTranscript = b.writers[e.TaskIndex].path
		}
	}
	results = append(results, b.refused...)
	slices.SortStableFunc(results, func(x, y *entry) int { return x.TaskIndex - y.TaskIndex })
	updateManifest(b.cfg, b.delegation, results, true)
	out.Results = results
	for _, e := range results {
		out.ProcessNotes = append(out.ProcessNotes, processLines(e)...)
	}
	for _, c := range running {
		if path := c.transcript.path; path != "" {
			out.LiveTranscripts = append(out.LiveTranscripts, path)
		}
	}
	if b.grouped {
		out.Group = &b.group
	}
	return out
}

// What one call of delegate_task, or one background unit of it, came to
type combined struct {
	Results         []*entry `json:"results"`
	TotalDuration   float64  `json:"total_duration_seconds"`
	ProcessNotes    []string `json:"process_notes,omitempty"`
	LiveTranscripts []string `json:"live_transcripts,omitempty"`
	Group           *string  `json:"group,omitempty"`
	Note            string   `json:"note,omitempty"`
}

// Runs several subagents at once, at most the settings' limit at a time; a parent's stop, when honoured, takes those not finished as
// interrupted. Each one's end is recorded on the background unit it belongs to, a failure told to the parent early while its siblings run
func (a *Agent) parallel(ctx context.Context, b *batch, running []*child, honour bool) []*entry {
	slots := make(chan struct{}, maxChildren(b.cfg))
	type finished struct {
		c *child
		e *entry
	}
	done := make(chan finished, len(running))
	for _, c := range running {
		go func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			defer func() {
				if p := recover(); p != nil {
					done <- finished{c, failedEntry(c.index, "error", fmt.Sprint(p), 0)}
				}
			}()
			done <- finished{c, c.execute(ctx)}
		}()
	}
	pending := map[*child]bool{}
	for _, c := range running {
		pending[c] = true
	}
	var results []*entry
	stopped := ctx.Done()
	if !honour {
		stopped = nil
	}
	for len(pending) > 0 {
		select {
		case f := <-done:
			delete(pending, f.c)
			results = append(results, f.e)
			a.async.recordChild(b, f.e, honour)
			if !honour && b.unit != "" && failedStatuses[f.e.Status] && len(pending) > 0 {
				a.async.taskFailed(b, f.e)
			}
		case <-stopped:
			for c := range pending {
				c.interrupt("parent agent interrupted")
			}
			deadline := time.After(abandonGrace)
			for len(pending) > 0 {
				select {
				case f := <-done:
					delete(pending, f.c)
					results = append(results, f.e)
				case <-deadline:
					for c := range pending {
						results = append(results, failedEntry(c.index, "interrupted", "Parent agent interrupted — child did not finish in time", 0))
						delete(pending, c)
					}
				}
			}
		}
	}
	return results
}

// What the results owe the parent before it reads them: summaries cut to its room with their whole text saved, and what the subagents cost
// added to its own
func (a *Agent) finalize(ctx context.Context, b *batch, results []*entry) {
	free := 0
	if window, used := a.room(b.parent); window > 0 {
		free = window - used
		if free == 0 {
			free = -1
		}
	}
	a.budgetSummaries(b.cfg, b.call.Env, results, free)
	a.stopped(ctx, b, results)
	total := 0.0
	for _, e := range results {
		total += e.cost
	}
	if total <= 0 {
		return
	}
	store, err := a.store()
	if err != nil {
		return
	}
	row := &sessionsv1.Usage{EstimatedCost: total}
	if session, err := store.Get(ctx, b.parent.sessionID()); err == nil {
		if status := session.GetUsage().GetCostStatus(); status == "" || status == "unknown" {
			row.CostStatus = "estimated"
		}
		if source := session.GetUsage().GetCostSource(); source == "" || source == "none" {
			row.CostSource = "subagent"
		}
	}
	if err := store.RecordUsage(context.WithoutCancel(ctx), b.parent.sessionID(), "", "subagent", row); err != nil {
		slog.Debug("Could not add the subagents' cost to the session", "session", b.parent.sessionID(), "error", err)
	}
}

// Fires subagent_stop for each subagent of the batch that finished: its session, role, summary with its secrets masked, status, the tool
// calls it made and how long it took
func (a *Agent) stopped(ctx context.Context, b *batch, results []*entry) {
	if !a.hooks.Has(b.cfg, hooks.SubagentStop) {
		return
	}
	ctx = context.WithoutCancel(ctx)
	for _, e := range results {
		var session *string
		if e.TaskIndex >= 0 && e.TaskIndex < len(b.children) && b.children[e.TaskIndex] != nil {
			session = hooks.Optional(b.children[e.TaskIndex].session())
		}
		var summary *string
		if e.Summary != nil {
			summary = hooks.Optional(cmp.Or(e.forced, childText(b.cfg, *e.Summary, true)))
		}
		steps := []hooks.Step{}
		for _, s := range e.ToolTrace {
			status := strings.ToLower(cmp.Or(s.Status, "unknown"))
			if status != "ok" && status != "error" {
				status = "unknown"
			}
			steps = append(steps, hooks.Step{Tool: strs.Head(cmp.Or(s.Tool, "unknown"), 256), Input: map[string]any{"argument_keys": s.Input.Keys,
				"targets": s.Input.Targets}, In: s.ArgsBytes, Out: s.Result, Status: status})
		}
		a.hooks.Fire(ctx, b.cfg, hooks.SubagentStopped{Parent: hooks.Optional(b.parent.sessionID()), ParentTurn: hooks.TurnOf(ctx).ID,
			ChildSession: session, Role: hooks.Optional(e.role), Summary: summary, Status: e.Status, Steps: steps, Duration: int64(e.Duration * 1000)})
	}
}

// Window of the parent's model and the tokens its context holds now, zeros when not known
func (a *Agent) room(r *run) (int, int) {
	r.mu.Lock()
	prompt := int(r.spent.prompt)
	r.mu.Unlock()
	used, window, _, _ := a.dial(r)
	if used == 0 {
		used = prompt
	}
	if window <= 0 || used <= 0 {
		return 0, 0
	}
	return window, used
}

// Runs a control action on the children of the session's spawn tree
func (a *Agent) control(ctx context.Context, session, action string, args map[string]any) string {
	if action == "list" {
		var entries []map[string]any
		for _, c := range a.owned(ctx, session) {
			listed := c.listed()
			entries = append(entries, map[string]any{"subagent_id": listed.GetId(), "parent_id": strs.Nullable(listed.GetParentId()), "goal": listed.GetGoal(),
				"model": strs.Nullable(listed.GetModel()), "status": listed.GetStatus(), "running_seconds": seconds(time.Since(listed.GetStarted().AsTime())),
				"accepting_steer": listed.GetSteering(), "live_transcript": strs.Nullable(listed.GetTranscript())})
		}
		if entries == nil {
			entries = []map[string]any{}
		}
		payload := map[string]any{"action": "list", "count": len(entries), "subagents": entries}
		if len(entries) == 0 {
			payload["note"] = "No live subagents right now. Children that already finished have delivered (or will deliver) their results as normal " +
				"completion messages — there is nothing to steer or stop."
		}
		return tools.JSON(payload)
	}
	id := strings.TrimSpace(argText(args, "subagent_id"))
	if id == "" {
		return errorResult(fmt.Sprintf("action='%s' requires subagent_id (from the spawn dispatch response or action='list').", action))
	}
	c := a.tree.find(id)
	if c == nil || !a.owns(c, session, a.lineage(ctx, session)) {
		return errorResult(fmt.Sprintf("No live subagent '%s' in this conversation's spawn tree. It may have already finished (its result arrives as a normal "+
			"completion message). Use action='list' to see live children.", id))
	}
	if action == "stop" {
		if !c.interrupt("The agent stopped subagent " + id + ".") {
			return errorResult(fmt.Sprintf("Could not interrupt '%s' — it likely finished in the last moment. Its result arrives as a normal completion "+
				"message.", id))
		}
		return tools.JSON(map[string]any{"action": action, "subagent_id": id, "status": "interrupt_requested", "note": "The subagent stops at its next " +
			"iteration boundary (in-flight tool calls are asked to cancel). Its partial result still re-enters the conversation as a completion message " +
			"— do not wait or poll."})
	}
	message := strings.TrimSpace(argText(args, "message"))
	if message == "" {
		return errorResult("action='steer' requires a non-empty 'message' describing the course correction.")
	}
	if !c.steer(message) {
		return errorResult(fmt.Sprintf("Subagent '%s' is no longer accepting steering (finishing or already finished). Its result arrives as a normal "+
			"completion message; re-delegate a follow-up task if more work is needed.", id))
	}
	return tools.JSON(map[string]any{"action": action, "subagent_id": id, "status": "queued", "note": "Steering text queued. The subagent sees it " +
		"appended to its next tool result — the current tool call is never cut. If the child finishes before a delivery boundary remains, the text is " +
		"reported back as missed_steer in its completion entry."})
}
