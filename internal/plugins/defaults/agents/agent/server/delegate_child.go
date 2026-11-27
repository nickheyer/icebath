package server

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nickheyer/icebath/internal/config"
	contextsconfig "github.com/nickheyer/icebath/internal/contexts/config"
	delegationconfig "github.com/nickheyer/icebath/internal/delegation/config"
	"github.com/nickheyer/icebath/internal/hooks"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	agentprompt "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/prompt"
	"github.com/nickheyer/icebath/internal/prompt"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/internal/worktrees"
	"github.com/nickheyer/icebath/pkg/fsx"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Source of a subagent's session
	subagentSource = "subagent"
	// Keys of a subagent session's model settings: its id in the spawn tree and its live transcript
	subagentKey   = "_subagent_id"
	transcriptKey = "_live_transcript"
	delegatedKey  = "_delegate_from"
	// How often a running subagent tells the session that delegated what it does, and the beats without progress, idle and inside a tool,
	// after which it is given up as stale
	heartbeatEvery = 30 * time.Second
	staleIdle      = 15
	staleInTool    = 40
	// How long a subagent stopped for going stale gets to hand its real result in
	staleGrace = 2 * time.Second
	// How often the idle limit of a subagent is checked, and the share of it after which the subagent is warned
	livenessPoll   = 5 * time.Second
	timeoutWarning = 0.8
	// Smallest token count delegation.compression_threshold_tokens takes
	minCompressionCap = 16000
	// Tool calls batched into one progress event of a subagent
	progressBatch = 5
	// Largest image a subagent is handed, read or downloaded
	maxChildImage = 20 << 20
)

// Tools no subagent is offered, delegate_task given back to an orchestrator
var blockedTools = []string{"delegate_task", "clarify", "memory", "send_message", "cronjob_manage"}

// Toolsets no subagent is offered: delegation unless it orchestrates, and the board
var blockedToolsets = []string{"delegation", "kanban"}

// The cause a subagent is stopped with when the agent that delegated to it ended its delegation
var errDelegationEnded = errors.New("parent delegation ended")

// A subagent: the session of its own the agent runs a delegated task in, and what its spawn tree knows of it
type child struct {
	a *Agent
	// Its session's run, and the run of the agent that delegated to it
	r      *run
	parent *run
	// Its id in the spawn tree, the id of the subagent that started it, empty under a session's own agent, and the session of that agent
	id, parentID, owner string
	// Levels below the session's own agent, 1 for its children; leaf or orchestrator
	depth int
	role  string
	// Call of delegate_task that started it, its task in that call, and the call's tasks
	delegation   string
	index, count int
	goal         string
	place        *placement
	images       []string
	schema       any
	transcript   *transcript
	// Settings of the agent that delegated, which the delegation settings are read from
	cfg *config.Config
	// Surface the session that delegated is shown on, which the subagent's tools see as theirs
	platform string
	// Git worktree it works in, nil for none
	tree *worktrees.Tree
	// Lets go of the account the subagent holds of its provider
	release func()
	// Risky commands run without asking, else they are refused; background process news reach the parent
	approve, surface bool
	// What the subagent's system prompt adds to its session's own
	prompt string
	// When it started, and the paths the parent had read by then
	started time.Time
	reads   []string

	mu sync.Mutex
	// queued or running
	status string
	model  string
	// Tool calls so far, the one running, when it last did anything and what
	tools    int
	tool     string
	last     time.Time
	doing    string
	calls    int
	steering bool
	handed   []handed
	// How the last turn ended: completed, or max_iterations at the step limit
	exit   string
	cancel context.CancelCauseFunc
	// Reasoning of the reply streaming now, and tool names not yet reported
	thought strings.Builder
	batch   []string
}

// The id of the subagent and what a stored subagent session keeps of it
type delegatedSession struct {
	id, session, transcript, parent string
}

// What a session's model settings say of it as a subagent, empty for a session of its own
func delegatedOf(session *sessionsv1.Session) delegatedSession {
	var recorded struct {
		Parent     string `json:"_delegate_from"`
		ID         string `json:"_subagent_id"`
		Transcript string `json:"_live_transcript"`
	}
	if session.GetModelConfig() != "" {
		json.Unmarshal([]byte(session.GetModelConfig()), &recorded)
	}
	return delegatedSession{id: recorded.ID, session: session.GetId(), transcript: recorded.Transcript, parent: recorded.Parent}
}

// Sessions of subagents, every one stored
func subagentFilter() *sessionsv1.Filter {
	return &sessionsv1.Filter{Source: subagentSource, IncludeChildren: true, IncludeHidden: true, IncludeArchived: true, IncludeEmpty: true,
		IncludeOpen: true, IncludePinned: true}
}

// Subagents the session delegated to, as their stored sessions keep them
func (a *Agent) delegatedBy(ctx context.Context, store sessions.Store, owner string) ([]delegatedSession, error) {
	children, _, err := store.List(ctx, subagentFilter())
	if err != nil {
		return nil, err
	}
	var out []delegatedSession
	for _, s := range children {
		if k := delegatedOf(s); k.parent == owner {
			out = append(out, k)
		}
	}
	return out, nil
}

// Session the subagent runs in now, the one a compression of its chat continued in
func (c *child) session() string { return c.r.sessionID() }

// Run of the session's own agent the subagent's spawn tree hangs from, whose windows show every subagent below it
func (c *child) root() *run {
	top := c.parent
	for range treeHops {
		if top.child == nil {
			break
		}
		top = top.child.parent
	}
	return top
}

// Step of the subagent as the windows of the session at the top of its tree see it
func (c *child) event(kind string) *agentv1.Subagent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &agentv1.Subagent{Kind: kind, Id: c.id, ParentId: c.parentID, SessionId: c.session(), DelegationId: c.delegation, Depth: int32(c.depth - 1),
		TaskIndex: int32(c.index), TaskCount: int32(c.count), Goal: strings.TrimSpace(c.goal), Model: c.model, Role: c.role, ToolCount: int32(c.tools),
		Status: c.status, Started: timestamppb.New(c.started), Transcript: c.transcript.path}
}

// Shows the step to the windows of the session at the top of the subagent's tree
func (c *child) publish(event *agentv1.Subagent) {
	top := c.root()
	top.mu.Lock()
	defer top.mu.Unlock()
	top.publish(&agentv1.WatchResponse{Event: &agentv1.WatchResponse_Subagent{Subagent: event}})
}

// Notes that the subagent did something, for its liveness and the parent's heartbeat
func (c *child) touch(doing string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = time.Now()
	if doing != "" {
		c.doing = doing
	}
}

// Text and reasoning the subagent's reply streams
func (c *child) streamed(text, reasoning string) {
	if c == nil {
		return
	}
	c.transcript.streamed(c.cfg, text)
	c.mu.Lock()
	c.thought.WriteString(reasoning)
	c.last = time.Now()
	c.mu.Unlock()
}

// One model call of the subagent answered: counted, its reasoning shown and logged
func (c *child) answered() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.calls++
	c.last = time.Now()
	thought := strings.TrimSpace(c.thought.String())
	c.thought.Reset()
	c.mu.Unlock()
	if thought == "" {
		return
	}
	c.transcript.thinking(c.cfg, thought)
	event := c.event("thinking")
	event.Text = thought
	c.publish(event)
}

// A tool call of the subagent starts: counted, shown, logged, and every few calls their names reported together
func (c *child) toolStarted(name, args string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.tools++
	c.tool, c.last, c.doing = name, time.Now(), "running "+name
	c.batch = append(c.batch, name)
	var batched []string
	if len(c.batch) >= progressBatch {
		batched, c.batch = c.batch, nil
	}
	c.mu.Unlock()
	c.transcript.toolStart(c.cfg, name, args)
	event := c.event("tool")
	event.Tool, event.Preview = name, strs.Head(args, 200)
	c.publish(event)
	if len(batched) > 0 {
		c.progress(batched)
	}
}

// Reports tool names the subagent ran together
func (c *child) progress(names []string) {
	tag := c.a.tree.tag(c.owner, c.delegation)
	prefix := ""
	switch {
	case c.count > 1 && tag != "":
		prefix = fmt.Sprintf("[%s · %d/%d] ", tag, c.index+1, c.count)
	case c.count > 1:
		prefix = fmt.Sprintf("[%d/%d] ", c.index+1, c.count)
	case tag != "":
		prefix = "[" + tag + "] "
	}
	event := c.event("progress")
	event.Text = prefix + "Ran together: " + strings.Join(names, ", ")
	c.publish(event)
}

// A tool call of the subagent returned
func (c *child) toolFinished(name, result string, failed bool, took time.Duration) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.tool, c.last = "", time.Now()
	c.mu.Unlock()
	c.transcript.toolResult(c.cfg, name, result, took, failed)
}

// Queues guidance for the subagent, delivered after its next tool results; false once it stopped taking guidance
func (c *child) steer(text string) bool {
	c.mu.Lock()
	steering := c.steering
	c.mu.Unlock()
	if !steering {
		return false
	}
	c.r.mu.Lock()
	defer c.r.mu.Unlock()
	c.r.steering = append(c.r.steering, text)
	return true
}

// Stops taking guidance and gives back what was queued but never delivered
func (c *child) closeSteering() string {
	c.mu.Lock()
	c.steering = false
	c.mu.Unlock()
	c.r.mu.Lock()
	defer c.r.mu.Unlock()
	pending := strings.TrimSpace(strings.Join(c.r.steering, "\n"))
	c.r.steering = nil
	return pending
}

// Stops the subagent at its next step, its tools asked to cancel; whether it was running
func (c *child) interrupt(reason string) bool {
	c.mu.Lock()
	cancel := c.cancel
	c.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel(errors.New(reason))
	return true
}

// What tells the stale verdict and the idle limit that the subagent moved on: its answered calls, the tool it runs and its activity clock
type fingerprint struct {
	calls int
	tool  string
	last  time.Time
}

func (c *child) fingerprint() fingerprint {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fingerprint{c.calls, c.tool, c.last}
}

// What the subagent's session's tools are told of it
func (c *child) delegated() *tools.Delegated {
	return &tools.Delegated{Parent: c.owner, ID: c.id, Depth: c.depth,
		Handoffs: func() int {
			c.mu.Lock()
			defer c.mu.Unlock()
			return len(c.handed)
		},
		Handoff: func(process, command, note string) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.handed = append(c.handed, handed{Process: process, Command: command, Note: note})
		},
		Notify: func(notice tools.Notice) error { return c.a.deliver(c.parent, notice) },
	}
}

// Where the subagent's background process news go: the parent, when the delegation settings let them through, else nowhere
func (c *child) notice(notice tools.Notice) error {
	if c.surface {
		return c.a.deliver(c.parent, notice)
	}
	slog.Debug("Suppressed subagent-owned process notification (delegation.surface_child_process_notifications=false)", "subagent", c.id,
		"title", notice.Title)
	return nil
}

// Answers the approval prompts of the subagent's risky commands in place of a user: once when the settings let subagents run them, else
// no, both logged
func (c *child) answer(command, description string) string {
	if c.approve {
		slog.Warn(fmt.Sprintf("Subagent auto-approved dangerous command: %s (%s)", command, description))
		return "once"
	}
	slog.Warn(fmt.Sprintf("Subagent auto-denied dangerous command: %s (%s). Turn on subagent auto approve in /settings Subagents to allow.", command,
		description))
	return "deny"
}

// Tools the subagent is offered of those its parent is: none of the blocked ones, delegate_task back for an orchestrator
func (c *child) offers(def tools.Def) bool {
	orchestrates := c.role == "orchestrator"
	if slices.Contains(blockedTools, def.Name) && !(orchestrates && def.Name == "delegate_task") {
		return false
	}
	return !slices.Contains(blockedToolsets, def.Toolset) || orchestrates && def.Toolset == "delegation" && def.Name == "delegate_task"
}

// Folder the parent works in now: where its commands last ended, else its session's own
func (a *Agent) workspace(parent *run, settings *config.Config, directory string) string {
	if a.terminal != nil {
		if folder := a.terminal.Folder(settings, parent.sessionID()); folder != "" {
			if info, err := os.Stat(folder); err == nil && info.IsDir() {
				return folder
			}
		}
	}
	if abs, err := filepath.Abs(fsx.ExpandHome(directory)); err == nil {
		return abs
	}
	return directory
}

const (
	childIntro            = "You are a focused subagent working on a specific delegated task."
	childContextFilesHead = "\nThe workspace's project context files are reproduced below. Their conventions and invariants are binding for your work in " +
		"this workspace.\n\n"
	childCompletion = "\nComplete this task using the tools available to you. When finished, provide a clear, concise summary of:\n" +
		"- What you did\n- What you found or accomplished\n- Any files you created or modified\n- Any issues encountered\n\n" +
		"Important workspace rule: Never assume a repository lives at /workspace/... or any other container-style path unless the task/context " +
		"explicitly gives that path. If no exact local path is provided, discover it first before issuing git/workdir-specific commands.\n\n" +
		"Keep your final summary tight: lead with outcomes, prefer bullet points over paragraphs, and don't replay your whole process. Your response " +
		"is returned to the parent agent as a summary, and overlong summaries crowd out the parent's context window."
	childOrchestrator = "\n## Subagent Spawning (Orchestrator Role)\n" +
		"You have access to the `delegate_task` tool and CAN spawn your own subagents to parallelize independent work.\n\n" +
		"WHEN to delegate:\n" +
		"- The goal decomposes into 2+ independent subtasks that can run in parallel (e.g. research A and B simultaneously).\n" +
		"- A subtask is reasoning-heavy and would flood your context with intermediate data.\n\n" +
		"WHEN NOT to delegate:\n" +
		"- Single-step mechanical work — do it directly.\n" +
		"- Trivial tasks you can execute in one or two tool calls.\n" +
		"- Re-delegating your entire assigned goal to one worker (that's just pass-through with no value added).\n\n" +
		"Coordinate your workers' results and synthesize them before reporting back to your parent. You are responsible for the final summary, not " +
		"your workers.\n\n"
	childLeafNote = "Your own children MUST be leaves (cannot delegate further) because they will reach the maximum nesting depth. The system manages " +
		"this automatically; no parameter override is available."
	childNestedNote = "Your own children can themselves delegate because depth remains. The system determines this automatically from the nesting " +
		"depth; children at the maximum depth cannot delegate further. No parameter override is available."
)

// What a subagent's system prompt adds: its task's context, the workspace and its project instructions, how to finish, and what an
// orchestrator may do; the project instructions left out where its own prompt already carries them
func (a *Agent) childPrompt(ctx context.Context, settings *config.Config, brief, workspace, role string, maxDepth, depth int, files bool) string {
	parts := []string{childIntro}
	if strings.TrimSpace(brief) != "" {
		parts = append(parts, "\nCONTEXT:\n"+brief)
	}
	if strings.TrimSpace(workspace) != "" {
		parts = append(parts, "\nWORKSPACE PATH:\n"+workspace+"\nUse this exact path for local repository/workdir operations unless the task explicitly "+
			"says otherwise.")
		if files {
			if loaded := strings.TrimSpace(agentprompt.ContextFiles(ctx, &prompt.Env{Config: settings, Cwd: workspace})); loaded != "" {
				parts = append(parts, childContextFilesHead+loaded)
			}
		}
	}
	parts = append(parts, childCompletion)
	if role == "orchestrator" {
		note := childNestedNote
		if depth+1 >= maxDepth {
			note = childLeafNote
		}
		parts = append(parts, childOrchestrator+fmt.Sprintf("NOTE: You are at depth %d. The delegation tree is capped at max_spawn_depth=%d. ", depth,
			maxDepth)+note)
	}
	return strings.Join(parts, "\n")
}

// Token cap of a subagent's compression trigger from delegation.compression_threshold_tokens, never above a cap of the profile's own; 0 for
// none, a value below the floor warned of
func compressionCap(settings *config.Config) int {
	n := settings.GetInt(delegationconfig.CompressionThreshold)
	if n == 0 {
		return 0
	}
	if n < minCompressionCap {
		slog.Warn(fmt.Sprintf("delegation.compression_threshold_tokens=%d is not a token count >= %d; ignoring it (children keep the ratio trigger).",
			n, minCompressionCap))
		return 0
	}
	if existing := settings.GetInt(contextsconfig.ThresholdTokens); existing > 0 {
		return min(n, existing)
	}
	return n
}

// Starts the session of a subagent for one task of the batch, ready to run: its route held, its prompt, settings and tools set; an error
// refuses the call
func (a *Agent) spawn(ctx context.Context, b *batch, t *task, p *placement, writer *transcript) (*child, error) {
	parent := b.parent
	depth, parentID := 1, ""
	if pc := parent.child; pc != nil {
		depth, parentID = pc.depth+1, pc.id
	}
	maxDepth := maxSpawnDepth(b.cfg)
	role := "leaf"
	if b.cfg.GetBool(delegationconfig.OrchestratorEnabled) && depth < maxDepth {
		role = "orchestrator"
	}
	id := fmt.Sprintf("sa-%d-%s", t.index, shortID())
	c := &child{a: a, parent: parent, id: id, parentID: parentID, owner: parent.sessionID(), depth: depth, role: role, delegation: b.delegation,
		index: t.index, count: len(b.tasks), goal: t.goal, place: p, images: t.images, schema: t.schema, transcript: writer, cfg: b.cfg,
		platform: b.call.Env.Platform, approve: b.cfg.GetBool(delegationconfig.SubagentAutoApprove),
		surface: b.cfg.GetBool(delegationconfig.SurfaceChildProcessNotifications), status: "queued", steering: true, release: func() {}}
	workspace := a.workspace(parent, b.cfg, b.call.Env.Cwd)
	directory := workspace
	if b.cfg.GetBool(delegationconfig.WorktreeIsolation) && a.worktrees != nil {
		if backend, err := a.terminal.Configured(b.cfg); err != nil || backend.Name != "local" {
			slog.Debug("worktree isolation skipped: non-local terminal backend")
		} else if c.tree = a.worktrees.Subagent(ctx, workspace, id); c.tree != nil {
			directory = c.tree.Path
			c.goal += c.tree.SubagentNote()
		}
	}
	brief := t.context
	if t.schema != nil {
		brief = withContract(brief, t.schema)
	}
	values := map[string]any{agentconfig.MaxTurns: max(b.cfg.GetInt(delegationconfig.MaxIterations), 1)}
	if !p.viaProfile {
		values[agentconfig.IgnoreRules] = true
	}
	if limit := compressionCap(b.cfg); limit > 0 {
		values[contextsconfig.ThresholdTokens] = limit
	}
	if p.fallbacks != nil {
		values[agentconfig.Fallbacks] = p.fallbacks
	} else if p.pinned && !p.viaProfile {
		values[agentconfig.Fallbacks] = []string{}
	}
	settings := a.config.In(p.profile).With((&sessions.Overrides{Values: values}).Layer())
	name, model, effort := p.provider, p.model, p.effort
	if !p.viaProfile && p.fixed == nil {
		instance, route, err := a.current(parent)
		if name == "" && err == nil {
			name, model = instance.ID, cmp.Or(model, route.Model)
			// An endpoint the parent runs on outside the profile's providers, such as a model shortcut's, the subagent runs on as it is
			if own, ok := a.provider(settings, name); !ok || !a.reaches(own, model, route) {
				fixed := route
				fixed.Model = model
				p.fixed, name = &fixed, ""
			}
		}
		if effort == "" {
			effort = a.effort(parent)
		}
		if name != "" {
			instance, ok := a.provider(settings, name)
			if !ok {
				return nil, fmt.Errorf("Cannot resolve delegation provider '%s'.", name)
			}
			leased, release, err := a.credentials.Lease(instance, model, "")
			if err != nil {
				return nil, err
			}
			c.release = release
			if leased.Account != "" {
				values[providerconfig.Key+"."+instance.ID+"."+providerconfig.Account] = leased.Account
				if !instance.Pinned {
					values[providerconfig.Key+"."+instance.ID+"."+providerconfig.OnExhausted] = providerconfig.Rotate
				}
			}
			model = cmp.Or(model, leased.Model)
		}
	}
	if p.fixed != nil && effort == "" {
		effort = a.effort(parent)
	}
	recorded, err := json.Marshal(map[string]any{delegatedKey: parent.sessionID(), subagentKey: id, transcriptKey: writer.path,
		"_settings": sessions.Overrides{Values: values}})
	if err != nil {
		c.release()
		return nil, err
	}
	store, err := a.store()
	if err != nil {
		c.release()
		return nil, err
	}
	created, err := store.Create(ctx, &sessionsv1.Session{Profile: p.profile, Source: subagentSource, ParentId: parent.sessionID(), Directory: directory,
		ModelConfig: string(recorded)})
	if err != nil {
		c.release()
		return nil, err
	}
	r, err := a.run(created.GetId())
	if err != nil {
		c.release()
		return nil, err
	}
	r.mu.Lock()
	r.child, r.client, r.provider, r.model, r.effort = c, parent.shell(), name, model, effort
	r.mu.Unlock()
	c.r = r
	c.model = cmp.Or(model, p.fixedModel(parent, a))
	if a.terminal != nil {
		a.terminal.Alias(settings, created.GetId(), parent.sessionID(), directory)
	}
	a.approvals.Restore(created.GetId(), a.approvals.Yolo(parent.sessionID()))
	c.prompt = a.childPrompt(ctx, settings, brief, workspace, role, maxDepth, depth, !p.viaProfile)
	a.tree.add(c)
	c.publish(c.event("spawn_requested"))
	a.hooks.Fire(context.WithoutCancel(ctx), b.cfg, hooks.SubagentStarted{Parent: hooks.Optional(parent.sessionID()), ParentTurn: hooks.TurnOf(ctx).ID,
		ParentAgent: hooks.Optional(parentID), ChildSession: hooks.Optional(created.GetId()), ChildAgent: id, Role: role, Goal: t.goal})
	return c, nil
}

// Whether the provider routes the model to the endpoint of the route, which the route so came from
func (a *Agent) reaches(instance provider.Instance, model string, route provider.Route) bool {
	own, err := a.credentials.Route(instance, model)
	return err == nil && strings.TrimRight(own.BaseURL, "/") == strings.TrimRight(route.BaseURL, "/")
}

// Model of an endpoint route of its own, the parent's when the route names none
func (p *placement) fixedModel(parent *run, a *Agent) string {
	if p.fixed == nil {
		return ""
	}
	if p.model != "" {
		return p.model
	}
	_, route, _ := a.current(parent)
	return route.Model
}

// Runs one turn of the subagent's session with the text as its message, stoppable through interrupt
func (c *child) turn(ctx context.Context, text string, media []*sessionsv1.Media) error {
	a, r := c.a, c.r
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	c.mu.Lock()
	c.cancel, c.exit = cancel, ""
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.cancel = nil
		c.mu.Unlock()
	}()
	r.mu.Lock()
	r.busy, r.stop = true, func() { cancel(errDelegationEnded) }
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.busy, r.stop = false, nil
		r.mu.Unlock()
	}()
	finish, err := a.enter(ctx, r, "")
	if err != nil {
		return err
	}
	err = a.turn(ctx, r, queued{text: text, media: media})
	finish(outcome(ctx, err))
	return err
}

// What one finished turn of the subagent came to: its final answer, its messages and how the turn ended
type turned struct {
	reply    string
	messages []*sessionsv1.Message
	err      error
	stopped  bool
}

// The subagent's messages and last answer after a turn
func (c *child) collect(ctx context.Context, err error, stopped bool) turned {
	out := turned{err: err, stopped: stopped}
	store, serr := c.a.store()
	if serr != nil {
		return out
	}
	out.messages, _ = store.Messages(context.WithoutCancel(ctx), c.session(), sessions.Model)
	for i := len(out.messages) - 1; i >= 0; i-- {
		m := out.messages[i]
		if m.GetRole() == sessionsv1.Role_ROLE_ASSISTANT && len(m.GetToolCalls()) == 0 && m.GetFinishReason() != verificationRequired {
			out.reply = strings.TrimSpace(thinkBlocks.ReplaceAllString(m.GetText(), ""))
			break
		}
		if m.GetRole() == sessionsv1.Role_ROLE_USER && m.GetDisplay() != "hidden" {
			break
		}
	}
	return out
}

// Runs the subagent's task to its end: its goal sent with its images, held to its idle limit, its answer checked against its schema and
// turned into its entry; everything it held let go
func (c *child) execute(ctx context.Context) (e *entry) {
	a := c.a
	c.mu.Lock()
	c.status, c.started, c.last = "running", time.Now(), time.Now()
	c.mu.Unlock()
	c.reads = a.tools.KnownReads(c.cfg, c.owner)
	defer c.cleanup()
	defer func() {
		if p := recover(); p != nil {
			slog.Error(fmt.Sprintf("[subagent-%d] failed", c.index), "error", p)
			late := c.closeSteering()
			e = failedEntry(c.index, "error", fmt.Sprint(p), c.elapsed())
			e.role = c.role
			c.finishFailed(e, late, fmt.Sprint(p))
		}
	}()
	c.transcript.started(c.cfg, c.goal)
	c.publish(c.event("start"))
	stop := c.heartbeat(ctx)
	defer stop()
	text, media := c.goalMessage(ctx)
	result, failure := c.await(ctx, text, media)
	if failure != nil {
		return failure
	}
	outcome := c.validate(ctx, &result)
	late := c.closeSteering()
	e = c.entry(result, outcome, late)
	c.siblingWrites(e)
	c.processes(e)
	c.complete(e)
	return c.finishTree(e)
}

func (c *child) elapsed() float64 { return seconds(time.Since(c.started)) }

// The goal as the subagent's first message: its images handed over as they are to a model that sees, else as paths to look at with
// vision_analyze
func (c *child) goalMessage(ctx context.Context) (string, []*sessionsv1.Media) {
	if len(c.images) == 0 {
		return c.goal, nil
	}
	settings := c.a.settings(c.r)
	_, route, err := c.a.current(c.r)
	if err == nil && c.a.helpers(c.r, settings, route).NativeImages(ctx) {
		var media []*sessionsv1.Media
		var skipped []string
		for _, ref := range c.images {
			m, err := c.image(ctx, settings, ref)
			if err != nil {
				skipped = append(skipped, ref)
				continue
			}
			media = append(media, m)
		}
		if len(skipped) > 0 {
			slog.Warn(fmt.Sprintf("delegate_task: skipped %d unreadable image(s) for subagent: %s", len(skipped), strings.Join(skipped[:min(3, len(skipped))], ", ")))
		}
		if len(media) > 0 {
			return c.goal, media
		}
		return c.goal, nil
	}
	var hints []string
	for _, ref := range c.images {
		switch {
		case strings.HasPrefix(ref, "data:image/"):
			slog.Warn("delegate_task: inline data-URL image dropped for a non-vision subagent")
		case strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://"):
			hints = append(hints, "[Image attached: "+ref+"]")
		default:
			if info, err := os.Stat(fsx.ExpandHome(ref)); err == nil && info.Mode().IsRegular() {
				hints = append(hints, "[Image attached at: "+ref+"]")
			} else {
				slog.Warn("delegate_task: image path not found, not forwarded: " + ref)
			}
		}
	}
	if len(hints) == 0 {
		return c.goal, nil
	}
	return c.goal + "\n\n" + strings.Join(hints, "\n") + "\nUse vision_analyze to inspect these images.", nil
}

// One image of a task as the model is handed it: a file read, an address fetched through the guarded client, or a data URL decoded
func (c *child) image(ctx context.Context, settings *config.Config, ref string) (*sessionsv1.Media, error) {
	var data []byte
	var kind string
	switch {
	case strings.HasPrefix(ref, "data:image/"):
		header, body, ok := strings.Cut(strings.TrimPrefix(ref, "data:"), ",")
		if !ok || !strings.HasSuffix(header, ";base64") {
			return nil, errors.New("not a base64 data URL")
		}
		decoded, err := base64.StdEncoding.DecodeString(body)
		if err != nil {
			return nil, err
		}
		data, kind = decoded, strings.TrimSuffix(header, ";base64")
	case strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://"):
		if !web.SafeURL(settings, ref) {
			return nil, errors.New("blocked address")
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
		if err != nil {
			return nil, err
		}
		response, err := web.Client(settings, 30*time.Second, true).Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if response.StatusCode/100 != 2 {
			return nil, fmt.Errorf("HTTP %d", response.StatusCode)
		}
		if data, err = io.ReadAll(io.LimitReader(response.Body, maxChildImage+1)); err != nil {
			return nil, err
		}
		kind, _, _ = mime.ParseMediaType(response.Header.Get("Content-Type"))
	default:
		path := fsx.ExpandHome(ref)
		if !filepath.IsAbs(path) {
			path = filepath.Join(c.parentDirectory(), path)
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("no such file")
		}
		if info.Size() > maxChildImage {
			return nil, errors.New("too large")
		}
		if data, err = os.ReadFile(path); err != nil {
			return nil, err
		}
		kind = mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	}
	if len(data) > maxChildImage {
		return nil, errors.New("too large")
	}
	if !strings.HasPrefix(kind, "image/") {
		kind = http.DetectContentType(data)
	}
	if !strings.HasPrefix(kind, "image/") {
		return nil, errors.New("not an image")
	}
	return &sessionsv1.Media{MimeType: kind, Data: data}, nil
}

// Folder of the session that delegated, which a relative image path is read from
func (c *child) parentDirectory() string {
	store, err := c.a.store()
	if err != nil {
		return ""
	}
	session, err := store.Get(c.a.ctx, c.owner)
	if err != nil {
		return ""
	}
	return session.GetDirectory()
}

// Tells the session that delegated what the subagent does every beat, the same session that delegated being told nothing for idle beats in
// a row giving it up as stale; the stop of the beats
func (c *child) heartbeat(ctx context.Context) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(heartbeatEvery)
		defer ticker.Stop()
		last, stale := c.fingerprint(), 0
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			now := c.fingerprint()
			if now != last {
				last, stale = now, 0
			} else {
				stale++
			}
			cycles := staleIdle
			if now.tool != "" {
				cycles = staleInTool
			}
			if stale >= cycles {
				slog.Warn(fmt.Sprintf("Subagent %d appears stale (no progress for %d heartbeat cycles, tool=%s) — abandoning its wait", c.index, stale,
					cmp.Or(now.tool, "<none>")))
				c.mu.Lock()
				c.doing = "stale"
				c.mu.Unlock()
				c.r.mu.Lock()
				c.r.stale = time.Duration(cycles) * heartbeatEvery
				c.r.mu.Unlock()
				c.interrupt("subagent went stale")
				return
			}
			c.mu.Lock()
			desc := fmt.Sprintf("Subagent %d is working.", c.index)
			maxIter := c.a.settings(c.r).GetInt(agentconfig.MaxTurns)
			step := fmt.Sprintf("step %d", c.calls)
			if maxIter > 0 {
				step += fmt.Sprintf(" of %d", maxIter)
			}
			switch {
			case c.tool != "":
				desc = fmt.Sprintf("Subagent %d is running %s, %s.", c.index, c.tool, step)
			case c.doing != "":
				desc = fmt.Sprintf("Subagent %d: %s, %s.", c.index, c.doing, step)
			}
			c.mu.Unlock()
			if store, err := c.a.store(); err == nil {
				store.Update(c.a.ctx, c.owner, sessions.Change{Activity: &desc})
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// Seconds the settings let a subagent go without progress, 0 for no limit, a limit set raised to the floor
func childTimeout(settings *config.Config) time.Duration {
	n := settings.GetInt(delegationconfig.ChildTimeout)
	if n <= 0 {
		return 0
	}
	return max(time.Duration(n)*time.Second, 30*time.Second)
}

// Runs the subagent's turn, waiting for it while it makes progress within its idle limit, warned once near it; the result, or the entry
// of a subagent that failed, went stale or timed out
func (c *child) await(ctx context.Context, text string, media []*sessionsv1.Media) (turned, *entry) {
	limit := childTimeout(c.cfg)
	done := make(chan error, 1)
	stack := make(chan string, 1)
	runCtx, stopRun := context.WithCancelCause(context.WithoutCancel(ctx))
	defer stopRun(nil)
	go func() {
		buf := make([]byte, 64<<10)
		stack <- string(buf[:runtime.Stack(buf, false)])
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("%v", p)
				}
			}()
			err = c.turn(runCtx, text, media)
		}()
		done <- err
	}()
	parentStop := ctx.Done()
	timedOut := false
	var err error
	func() {
		deadline := time.Now().Add(limit)
		warnAt := time.Now().Add(time.Duration(float64(limit) * timeoutWarning))
		warned := false
		last := c.fingerprint()
		for {
			var tick <-chan time.Time
			if limit > 0 {
				tick = time.After(min(livenessPoll, max(time.Until(deadline), 0)))
			}
			select {
			case err = <-done:
				return
			case <-parentStop:
				stopRun(context.Cause(ctx))
				c.interrupt("parent agent interrupted")
				parentStop = nil
				continue
			case <-tick:
			}
			if now := c.fingerprint(); now != last {
				last, deadline, warnAt, warned = now, time.Now().Add(limit), time.Now().Add(time.Duration(float64(limit)*timeoutWarning)), false
				continue
			}
			if !time.Now().Before(deadline) {
				timedOut = true
				return
			}
			if !warned && !time.Now().Before(warnAt) {
				warned = true
				idle := limit - time.Until(deadline)
				c.steer(fmt.Sprintf("[delegation budget warning] No progress signal for %.0fs of your %.0fs inactivity window. Finish the current step "+
					"and return your summary now — the work is discarded if the window elapses.", idle.Seconds(), limit.Seconds()))
			}
		}
	}()
	c.r.mu.Lock()
	staleAfter := c.r.stale
	c.r.mu.Unlock()
	if !timedOut && staleAfter == 0 {
		return c.collect(ctx, err, ctx.Err() != nil || errors.Is(err, context.Canceled)), nil
	}
	if staleAfter == 0 {
		c.interrupt("subagent timed out")
	}
	select {
	case err = <-done:
		if staleAfter != 0 {
			slog.Info(fmt.Sprintf("Subagent %d settled inside the %.1fs stale-result grace window — collecting its real result", c.index, staleGrace.Seconds()))
			return c.collect(ctx, err, true), nil
		}
	case <-time.After(staleGrace):
	}
	late := c.closeSteering()
	duration := c.elapsed()
	calls := c.fingerprint().calls
	cause := limit
	if staleAfter != 0 {
		cause = staleAfter
	}
	slog.Warn(fmt.Sprintf("Subagent %d timed out after %.1fs", c.index, duration))
	diagnostic := ""
	if calls == 0 {
		trace := ""
		select {
		case trace = <-stack:
		default:
		}
		if diagnostic = c.diagnose(cause, duration, trace); diagnostic != "" {
			slog.Warn(fmt.Sprintf("Subagent %d 0-API-call timeout — diagnostic written to %s", c.index, diagnostic))
		}
	}
	var reason string
	switch {
	case staleAfter != 0:
		reason = fmt.Sprintf("The subagent stopped making progress after %s, with no activity for %s, so it was given up.",
			strs.Count(calls, "model call"), strs.Span(rounded(staleAfter)))
	case calls == 0:
		reason = fmt.Sprintf("The subagent ran out of time after %s, before its first model call. Building its prompt, finding its account or "+
			"connecting to the provider may be stuck.", strs.Span(rounded(limit)))
	default:
		reason = fmt.Sprintf("The subagent ran out of time after %s and %s, with no progress in that time. A request to the provider or the "+
			"network probably stalled.", strs.Span(rounded(limit)), strs.Count(calls, "model call"))
	}
	if diagnostic != "" {
		reason += " Details are in " + diagnostic + "."
	}
	e := &entry{TaskIndex: c.index, Status: "timeout", Error: reason, ExitReason: "timeout", APICalls: calls, Duration: duration, role: c.role,
		TimeoutSeconds: new(cause.Seconds()), TimedOutAfter: new(duration), DiagnosticPath: diagnostic}
	e.TimeoutPhase = "after_llm_calls"
	if calls == 0 {
		e.TimeoutPhase = "before_first_llm_call"
	}
	if last := c.fingerprint().last; !last.IsZero() {
		e.LastEventAge = new(math.Round(time.Since(last).Seconds()*100) / 100)
	}
	return turned{}, c.finishFailed(e, late, "Ran out of time after "+strs.Span(rounded(time.Duration(duration*float64(time.Second))))+".")
}

// Shortest of the plain and exponent forms of a number, as limits are told
func formatG(v float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%g", v), ".0")
}

// Writes what is known of a subagent that timed out before its first model call to the profile's logs: its settings, prompt and tool sizes,
// its activity and the stacks of the process; the path, empty when it could not be written
func (c *child) diagnose(timeout time.Duration, duration float64, worker string) string {
	settings := c.a.settings(c.r)
	dir := filepath.Join(settings.ProfileDir(settings.Profile()), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	now := time.Now()
	path := filepath.Join(dir, fmt.Sprintf("subagent-timeout-%s-%s.log", c.id, now.Format("20060102_150405")))
	c.mu.Lock()
	calls, tool, doing, last := c.calls, c.tool, c.doing, c.last
	c.mu.Unlock()
	_, route, _ := c.a.current(c.r)
	lines := []string{"# Subagent timeout diagnostic", "# Generated: " + now.Format("2006-01-02T15:04:05.000000"), "",
		"## Timeout", fmt.Sprintf("  task_index:        %d", c.index), "  subagent_id:       " + c.id,
		fmt.Sprintf("  configured_timeout: %ss", formatG(timeout.Seconds())), fmt.Sprintf("  actual_duration:   %.2fs", duration), "", "## Goal",
		cmp.Or(strs.Clip(strings.TrimSpace(c.goal), 1000), "(empty)"), "", "## Child config",
		"  model: " + route.Model, "  provider: " + route.Provider, "  api_mode: " + string(route.Mode), "  base_url: " + route.BaseURL,
		fmt.Sprintf("  max_iterations: %d", settings.GetInt(agentconfig.MaxTurns)), "  platform: " + subagentSource, "  _delegate_role: " + c.role,
		fmt.Sprintf("  _delegate_depth: %d", c.depth), "", "## Prompt / schema sizes",
		fmt.Sprintf("  system_prompt_bytes: %d", len(c.prompt)), fmt.Sprintf("  system_prompt_chars: %d", utf8.RuneCountInString(c.prompt)),
		"", "## Activity summary", fmt.Sprintf("  api_call_count: %d", calls), "  current_tool: " + cmp.Or(tool, "None"),
		"  last_activity_desc: " + doing, "  last_activity_ts: " + last.Format(time.RFC3339), "", "## Worker thread stack at timeout"}
	if worker == "" {
		lines = append(lines, "  <no worker thread handle>")
	}
	for _, line := range strings.Split(strings.TrimRight(worker, "\n"), "\n") {
		if line != "" {
			lines = append(lines, "  "+line)
		}
	}
	lines = append(lines, "", "## All thread stacks at timeout")
	buf := make([]byte, 1<<20)
	blocks := strings.Split(strings.TrimSpace(string(buf[:runtime.Stack(buf, true)])), "\n\n")
	for i, block := range blocks {
		if i >= 40 {
			lines = append(lines, fmt.Sprintf("  <%d more threads omitted>", len(blocks)-i))
			break
		}
		header, body, _ := strings.Cut(block, "\n")
		lines = append(lines, "  --- "+strings.TrimSuffix(header, ":")+" ---")
		for _, line := range strings.Split(body, "\n") {
			lines = append(lines, "    "+line)
		}
	}
	lines = append(lines, "", "## Notes", "  This file is written ONLY when a subagent times out with 0 API calls.",
		"  0-API-call timeouts mean the child never reached its first LLM request.", "  Common causes: oversized prompt rejected by provider, transport hang,",
		"  credential resolution stuck.")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		slog.Warn(fmt.Sprintf("Subagent timeout diagnostic dump failed: %v", err))
		return ""
	}
	return path
}

// How the final answer held up against the task's schema
type schemaOutcome struct {
	checked bool
	valid   bool
	errors  []string
	retries int
}

// Checks the final answer against the task's schema, giving the subagent one turn to correct it
func (c *child) validate(ctx context.Context, result *turned) schemaOutcome {
	if c.schema == nil {
		return schemaOutcome{}
	}
	valid, errs, err := validateOutput(result.reply, c.schema)
	if err != nil {
		panic(err)
	}
	if valid || strings.TrimSpace(result.reply) == "" || result.stopped {
		return schemaOutcome{checked: true, valid: valid, errors: errs}
	}
	if err := c.turn(context.WithoutCancel(ctx), retryMessage(errs), nil); err != nil {
		slog.Warn(fmt.Sprintf("Subagent %d schema-retry turn failed: %v", c.index, err))
	}
	retried := c.collect(ctx, nil, false)
	if strings.TrimSpace(retried.reply) != "" {
		result.reply = retried.reply
	}
	result.messages = retried.messages
	valid, errs, err = validateOutput(result.reply, c.schema)
	if err != nil {
		panic(err)
	}
	return schemaOutcome{checked: true, valid: valid, errors: errs, retries: 1}
}

// Entry of the finished subagent: how it ended, its answer, what its calls did, what it spent, and how its answer held up against its schema
func (c *child) entry(result turned, schema schemaOutcome, late string) *entry {
	a := c.a
	reply := result.reply
	usable := reply != "" && reply != "(empty)"
	c.mu.Lock()
	exit, calls := c.exit, c.calls
	c.mu.Unlock()
	e := &entry{TaskIndex: c.index, APICalls: calls, Duration: c.elapsed(), Model: c.model, role: c.role}
	interruptNote := ""
	switch {
	case result.stopped:
		e.Status, e.ExitReason = "interrupted", "interrupted"
		if partial := c.partial(result.messages); partial != "" && partial != reply {
			interruptNote, reply = cmp.Or(reply, "Operation interrupted."), partial
		}
		if reply == "" {
			interruptNote = cmp.Or(errorText(result.err), "Operation interrupted.")
		}
	case result.err != nil:
		e.Status, e.ExitReason = "failed", "error"
	default:
		e.ExitReason = cmp.Or(exit, "completed")
		e.Status = "failed"
		if usable {
			e.Status = "completed"
		}
	}
	e.Summary = &reply
	e.Truncated = e.ExitReason == "max_iterations"
	settings := a.settings(c.r)
	if store, err := a.store(); err == nil {
		if session, err := store.Get(a.ctx, c.session()); err == nil {
			u := session.GetUsage()
			e.Tokens = &usage{Input: u.GetInput(), Output: u.GetOutput()}
			e.cost = cmp.Or(u.GetActualCost(), u.GetEstimatedCost())
			e.Cost, e.CostStatus = new(math.Round(e.cost*1e6)/1e6), cmp.Or(u.GetCostStatus(), "unknown")
			e.Model = cmp.Or(session.GetModel(), e.Model)
		}
	}
	e.ToolTrace = trace(settings, result.messages)
	if c.place.report != nil {
		report := *c.place.report
		_, route, _ := a.current(c.r)
		report.Provider, report.Model, report.ReasoningEffort = route.Provider, route.Model, a.effort(c.r)
		if c.place.tier != "" {
			report.ServiceTier = provider.TierWord(c.place.tier)
		}
		c.r.mu.Lock()
		report.FallbackActivate = c.r.failover != nil && c.r.failover.active.Model != ""
		c.r.mu.Unlock()
		e.Route = &report
	}
	if e.Status == "failed" {
		e.Error = cmp.Or(errorText(result.err), "Subagent did not produce a response.")
		if failure, ok := errors.AsType[*provider.APIError](result.err); ok {
			e.FailureReason = string(failure.Kind)
		}
	} else if interruptNote != "" {
		e.Error = interruptNote
	}
	if schema.checked {
		e.SchemaValid, e.SchemaRetries = new(schema.valid), schema.retries
		if !schema.valid && len(schema.errors) > 0 {
			e.SchemaErrors = schema.errors
		}
		if !schema.valid && usable {
			retryNote := ""
			if schema.retries != 0 {
				retryNote = " (after 1 retry)"
			}
			e.SchemaNote = "Final answer does not satisfy the declared output_schema" + retryNote + "; `summary` is the child's raw, UNVALIDATED final " +
				"text — extract what you need from it yourself (see schema_errors) rather than re-running the task."
		}
	}
	if strings.TrimSpace(late) != "" {
		e.MissedSteer = late
		note := "[steer did not land — the subagent finished before it could be delivered: " + late + "]"
		if reply != "" {
			e.Summary = text(reply + "\n\n" + note)
		} else {
			e.Summary = &note
		}
	}
	return e
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Last thing the subagent said before it was stopped, the sentinels of a stop left out
func (c *child) partial(messages []*sessionsv1.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.GetRole() != sessionsv1.Role_ROLE_ASSISTANT {
			continue
		}
		if t := strings.TrimSpace(m.GetText()); t != "" && t != "Operation interrupted." {
			return t
		}
	}
	return ""
}

// Failure tail shared by every way a subagent fails: its complete step shown, a steer it missed noted, its tree settled
func (c *child) finishFailed(e *entry, late, preview string) *entry {
	event := c.event("complete")
	event.Status, event.DurationSeconds, event.FailureReason = e.Status, e.Duration, e.FailureReason
	event.Text = childText(c.cfg, preview, false)
	c.publish(event)
	c.transcript.completed(c.cfg, e.Status, e.Duration, "")
	if late != "" {
		e.MissedSteer = late
		e.Error += " [steer did not land before the subagent stopped: " + late + "]"
	}
	return c.finishTree(e)
}

// Settles the subagent's worktree into its entry, then masks the secrets of what the parent reads
func (c *child) finishTree(e *entry) *entry {
	if c.tree != nil {
		left := c.a.worktrees.FinishSubagent(c.cfg, *c.tree)
		e.Worktree = &worktreeReport{Path: left.Path, Branch: left.Branch, Commits: left.Commits, Dirty: left.Dirty, Pruned: left.Pruned,
			Archive: left.Archive, Failed: left.Failed, Note: left.Note}
		c.tree = nil
	}
	return redacted(c.cfg, e)
}

// The entry with the secrets of what the subagent produced masked for the parent, unless the profile turned that off, and its forced copy
// kept for what goes to disk
func redacted(cfg *config.Config, e *entry) *entry {
	if e.Summary != nil {
		e.forced = childText(cfg, *e.Summary, true)
		e.Summary = text(childText(cfg, *e.Summary, false))
	}
	e.Error = childText(cfg, e.Error, false)
	e.MissedSteer = childText(cfg, e.MissedSteer, false)
	for i := range e.SchemaErrors {
		e.SchemaErrors[i] = childText(cfg, e.SchemaErrors[i], false)
	}
	return e
}

// Tells the parent the subagent wrote files the parent had read before it started, so it reads them again before it edits them
func (c *child) siblingWrites(e *entry) {
	if len(c.reads) == 0 {
		return
	}
	set := map[string]bool{}
	for _, paths := range c.a.tools.WritesSince(c.cfg, c.owner, c.started, c.reads) {
		for _, p := range paths {
			set[p] = true
		}
	}
	if len(set) == 0 {
		return
	}
	paths := slices.Sorted(maps.Keys(set))
	shown := paths[:min(8, len(paths))]
	reminder := "\n\n[NOTE: subagent modified files the parent previously read — re-read before editing: " + strings.Join(shown, ", ")
	if len(paths) > 8 {
		reminder += fmt.Sprintf(" (+%d more)", len(paths)-8)
	}
	reminder += "]"
	if e.summaryText() != "" {
		e.Summary = text(e.summaryText() + reminder)
	} else {
		e.Stale = paths
	}
}

// Names the background processes the subagent handed over, left running, which its end kills, and finished without reading their results
func (c *child) processes(e *entry) {
	c.mu.Lock()
	e.HandedOff = slices.Clone(c.handed)
	c.mu.Unlock()
	if c.a.terminal == nil {
		return
	}
	settings := c.a.settings(c.r)
	jobs := c.a.terminal.Jobs
	for _, view := range jobs.Views(c.session()) {
		command := strs.Head(childText(c.cfg, view.Command, false), 200)
		if view.Status == "running" {
			e.Orphaned = append(e.Orphaned, orphan{Process: view.ID, Command: command, Runtime: int(math.RoundToEven(time.Since(view.Started).Seconds()))})
			continue
		}
		if j := jobs.Get(settings, c.session(), view.ID); j != nil && j.Notifies() && !jobs.Consumed(view.ID) {
			e.Unread = append(e.Unread, unread{Process: view.ID, Command: command, Code: view.Code,
				Tail: strs.Tail(childText(c.cfg, view.Tail, false), 600)})
		}
	}
}

// Shows the subagent's end to the windows with what it spent, read, wrote and last saw, and logs it
func (c *child) complete(e *entry) {
	settings := c.a.settings(c.r)
	event := c.event("complete")
	event.Status, event.DurationSeconds, event.ApiCalls, event.FailureReason = e.Status, e.Duration, int32(e.APICalls), e.FailureReason
	summary := e.summaryText()
	event.Text = cmp.Or(strs.Head(summary, 160), e.Error)
	event.Summary = cmp.Or(strs.Head(summary, 500), e.Error)
	if store, err := c.a.store(); err == nil {
		if session, err := store.Get(c.a.ctx, c.session()); err == nil {
			u := session.GetUsage()
			event.InputTokens, event.OutputTokens, event.ReasoningTokens = u.GetInput(), u.GetOutput(), u.GetReasoning()
			event.Cost = cmp.Or(u.GetActualCost(), u.GetEstimatedCost())
		}
	}
	reads := c.a.tools.KnownReads(settings, c.session())
	event.FilesRead = reads[:min(40, len(reads))]
	var written []string
	for session, paths := range c.a.tools.WritesSince(settings, "", c.started, nil) {
		if session == c.session() {
			written = append(written, paths...)
		}
	}
	slices.Sort(written)
	event.FilesWritten = written[:min(40, len(written))]
	event.OutputTail = c.outputTail(settings)
	c.publish(event)
	c.transcript.completed(c.cfg, e.Status, e.Duration, cmp.Or(summary, e.Error))
}

// Last eight tool results the subagent got, oldest first
func (c *child) outputTail(settings *config.Config) []*agentv1.SubagentOutput {
	store, err := c.a.store()
	if err != nil {
		return nil
	}
	messages, err := store.Messages(c.a.ctx, c.session(), sessions.Model)
	if err != nil {
		return nil
	}
	var tail []*agentv1.SubagentOutput
	for i := len(messages) - 1; i >= 0 && len(tail) < 8; i-- {
		m := messages[i]
		if m.GetRole() != sessionsv1.Role_ROLE_TOOL {
			continue
		}
		content := childText(settings, m.GetText(), false)
		tail = append(tail, &agentv1.SubagentOutput{Tool: cmp.Or(m.GetToolName(), "tool"), Preview: strs.Head(content, 600), Error: m.GetFailed() ||
			looksFailed(content)})
	}
	slices.Reverse(tail)
	return tail
}

// Whether a tool result reads as a failure: an error or failed status in its JSON, or a first line that opens like an error
func looksFailed(content string) bool {
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "{") {
		var body struct {
			Error  any    `json:"error"`
			Status string `json:"status"`
		}
		if json.Unmarshal([]byte(trimmed), &body) == nil {
			if body.Error != nil && body.Error != false && body.Error != "" {
				return true
			}
			switch strings.ToLower(strings.TrimSpace(body.Status)) {
			case "error", "failed", "failure", "timeout":
				return true
			}
		}
	}
	first, _, _ := strings.Cut(trimmed, "\n")
	first = strings.ToLower(strings.TrimSpace(first))
	for _, marker := range []string{"error:", "failed:", "traceback ", "exception:"} {
		if strings.HasPrefix(first, marker) {
			return true
		}
	}
	return false
}

// Tool calls of the subagent's messages in order with what their results were, parallel calls paired with results by call id
func trace(settings *config.Config, messages []*sessionsv1.Message) []step {
	var out []step
	byID := map[string]int{}
	for _, m := range messages {
		switch m.GetRole() {
		case sessionsv1.Role_ROLE_ASSISTANT:
			for _, call := range m.GetToolCalls() {
				out = append(out, step{Tool: cmp.Or(call.GetName(), "unknown"), ArgsBytes: utf8.RuneCountInString(call.GetArguments()),
					Input: argumentSummary(settings, call.GetArguments())})
				if call.GetId() != "" {
					byID[call.GetId()] = len(out) - 1
				}
			}
		case sessionsv1.Role_ROLE_TOOL:
			i, ok := byID[m.GetToolCallId()]
			if !ok {
				if m.GetToolCallId() != "" || len(out) == 0 {
					continue
				}
				i = len(out) - 1
			}
			size := utf8.RuneCountInString(m.GetText())
			status := "ok"
			if m.GetFailed() || looksFailed(m.GetText()) {
				status = "error"
			}
			out[i].Result, out[i].Status = &size, status
		}
	}
	return out
}

// Keys of the arguments that name what a call acted on
var targetKeys = map[string]bool{"cwd": true, "destination_path": true, "directory": true, "dst": true, "endpoint": true, "file_path": true,
	"new_path": true, "old_path": true, "path": true, "source_path": true, "src": true, "target_path": true, "url": true, "urls": true}

// Argument names of a call and the paths and addresses it acted on, addresses without their credentials, query or fragment
func argumentSummary(settings *config.Config, arguments string) summary {
	out := summary{Keys: []string{}, Targets: map[string]any{}}
	var args map[string]any
	if json.Unmarshal([]byte(arguments), &args) != nil {
		return out
	}
	for _, key := range slices.Sorted(maps.Keys(args)) {
		if len(out.Keys) < 64 {
			out.Keys = append(out.Keys, strs.Head(childText(settings, key, false), 128))
		}
		lower := strings.ToLower(key)
		if !targetKeys[lower] {
			continue
		}
		if cleaned := target(settings, lower, args[key]); cleaned != nil {
			out.Targets[lower] = cleaned
		}
	}
	return out
}

// One target of a call bounded and cleaned, nil when unusable
func target(settings *config.Config, key string, value any) any {
	if list, ok := value.([]any); ok {
		var cleaned []any
		for _, item := range list[:min(16, len(list))] {
			if c := target(settings, key, item); c != nil {
				cleaned = append(cleaned, c)
			}
		}
		if len(cleaned) == 0 {
			return nil
		}
		return cleaned
	}
	s, ok := value.(string)
	if !ok || s == "" {
		return nil
	}
	bounded := strs.Head(childText(settings, s, false), 1024)
	if key != "url" && key != "urls" && key != "endpoint" {
		return bounded
	}
	parsed, err := url.Parse(bounded)
	if err != nil {
		return nil
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return bounded
	}
	host := parsed.Hostname()
	if host == "" {
		return nil
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := parsed.Port(); port != "" {
		host += ":" + port
	}
	return parsed.Scheme + "://" + host + parsed.Path
}

// Lets go of everything the subagent held: its heartbeat, its place in the spawn tree, its account lease, its session with its processes,
// kernels and sandbox ties, and its tree when the task never got to settle it
func (c *child) cleanup() {
	c.a.tree.remove(c)
	c.release()
	if c.tree != nil {
		c.a.worktrees.FinishSubagent(c.cfg, *c.tree)
		c.tree = nil
	}
	c.a.close(c.r, sessions.EndClosed, 0)
}
