package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/plugin"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool/mcp"
)

// Status a running hook shows, for the status line of the terminal
type HookStatusMsg struct {
	Run  string
	Text string
}

// Terminal sequence a hook asked to be written to the terminal
type TerminalMsg struct {
	Seq string
}

// Instruction the hooks of a hook host answer prompt hooks under
const promptHookSystem = "You judge one hook input and answer with a single JSON object of the form {\"ok\": true} or {\"ok\": false, \"reason\": \"why\"}. Answer with nothing but that object."

// Services hooks get from the app, built for one session
func (a *App) hookHost(sess *session.Session, ep Endpoint, run func() string) agent.HookHost {
	return agent.HookHost{
		Prompt: func(ctx context.Context, model, prompt string) (string, error) {
			return a.promptHook(ctx, ep, model, prompt)
		},
		Agent: func(ctx context.Context, model, prompt string, turns int) (string, error) {
			return a.agentHook(ctx, ep, model, prompt, turns)
		},
		MCPCall: func(ctx context.Context, server, tool string, input map[string]any) (string, bool, error) {
			return a.mcpHook(ctx, server, tool, input)
		},
		Matches: a.ruleMatches,
		Status: func(text string) {
			id := ""
			if run != nil {
				id = run()
			}
			a.send(HookStatusMsg{Run: id, Text: text})
		},
		Notify:   func(text string) { a.send(NoticeMsg{Level: Warn, Text: text}) },
		Terminal: func(seq string) { a.send(TerminalMsg{Seq: seq}) },
		Deliver:  func(text string) { a.deliver(sess, text) },
		Mode: func() string {
			if sess != nil {
				if kind, ok := a.KindOf(sess); ok && kind.Mode != "" {
					return kind.Mode
				}
			}
			return string(a.Mode.Mode())
		},
	}
}

// Runner for the hooks of a session, with or without a running agent
func (a *App) hookRunner(name string, sess *session.Session, ep Endpoint, kind Kind, state *agent.HookState) agent.HookRunner {
	transcript := ""
	scratch := ""
	id := ""
	if sess != nil {
		transcript, _ = a.Store.Path(sess.ID())
		scratch = ScratchDir(sess.ID())
		id = sess.ID()
	}
	r := agent.HookRunner{
		Dynamic:    func() agent.HookConfig { return a.hooksFor(sess, kind) },
		Dir:        a.Root,
		DirFunc:    a.Shell.Dir,
		ProjectDir: a.Root,
		SessionID:  id,
		Transcript: transcript,
		Scratch:    scratch,
		Effort:     kind.Effort,
		Env:        a.hookEnv(),
		Log:        hookLog{send: a.send, run: name},
		Host:       a.hookHost(sess, ep, nil),
		State:      state,
	}
	if sess != nil && sess.Labels()[parentLabel] != "" {
		r.AgentID = sess.ID()
		r.AgentType = kind.Name
		if r.AgentType == "" {
			r.AgentType = KindGeneral
		}
	}
	return r
}

// Fires the hooks of an event for a session outside any run
func (a *App) FireSession(ctx context.Context, sess *session.Session, event agent.HookEvent, subject string, fields map[string]any) (agent.Verdict, error) {
	kind := Kind{}
	ep := a.Primary
	if sess != nil {
		kind, _ = a.KindOf(sess)
		ep = a.endpointOf(sess)
	}
	r := a.hookRunner("", sess, ep, kind, a.stateOf(sess))
	return r.Fire(ctx, event, subject, fields)
}

// Hook state of a session, shared by every agent built on it
func (a *App) stateOf(sess *session.Session) *agent.HookState {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.states == nil {
		a.states = map[string]*agent.HookState{}
	}
	key := ""
	if sess != nil {
		key = sess.ID()
	}
	st, ok := a.states[key]
	if !ok {
		st = &agent.HookState{}
		a.states[key] = st
	}
	return st
}

// Answers a prompt hook with the background model, or the named one on the same profile
func (a *App) promptHook(ctx context.Context, ep Endpoint, model, prompt string) (string, error) {
	if a.summary != nil {
		ep = *a.summary
	}
	if ep.Provider == nil {
		return "", ErrNoProfile
	}
	id := ep.Model
	if model != "" {
		var err error
		if id, err = resolveModel(model, ep); err != nil {
			return "", err
		}
	}
	req := protocol.Request{Model: id, System: promptHookSystem, Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: prompt}}}}, MaxTokens: 1024}
	if protocol.RequiresMaxTokens(ep.Provider.Protocol) && ep.Catalog.MaxOutput > 0 && ep.Catalog.MaxOutput < 1024 {
		req.MaxTokens = ep.Catalog.MaxOutput
	}
	resp, err := ep.Provider.Complete(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.Text(), nil
}

// Runs an agent hook as a child agent with tools on a fresh session, no hooks of its own, and returns its text
func (a *App) agentHook(ctx context.Context, ep Endpoint, model, prompt string, turns int) (string, error) {
	if a.children != nil {
		ep = *a.children
	}
	if model != "" {
		id, err := resolveModel(model, ep)
		if err != nil {
			return "", err
		}
		ep = withModel(ep, id)
	}
	sess := session.New()
	sess.Label(parentLabel, a.Session.ID())
	sess.Label(session.ProjectLabel, a.Root)
	ag, err := a.buildAgent("hook", sess, ep, buildOptions{noHooks: true})
	if err != nil {
		return "", err
	}
	ag.MaxRounds = turns
	ag.System = promptHookSystem
	resp, err := ag.Run(ctx, []protocol.Part{protocol.Text{Text: prompt}}, func(ev agent.Event) { agent.Observe(ctx, ev) })
	if err != nil && !errors.Is(err, agent.ErrRounds) {
		return "", err
	}
	return resp.Text(), nil
}

// Calls a tool of an MCP server for a hook, the plugin:name:server form mapped to the registered server name
func (a *App) mcpHook(ctx context.Context, server, tool string, input map[string]any) (string, bool, error) {
	if strings.HasPrefix(server, "plugin:") {
		server = "plugin_" + strings.ReplaceAll(strings.TrimPrefix(server, "plugin:"), ":", "_")
	}
	name := mcp.ToolName(server, tool)
	if _, ok := a.Tools.Lookup(name); !ok {
		connected := false
		for _, st := range a.MCP.Status() {
			if st.Name == server && st.State == mcp.StateConnected {
				connected = true
			}
		}
		if !connected {
			return "", false, fmt.Errorf("mcp server %s is not connected", server)
		}
		return "", false, fmt.Errorf("mcp server %s offers no tool %s", server, tool)
	}
	args, err := json.Marshal(input)
	if err != nil {
		return "", false, err
	}
	out := a.Tools.Run(ctx, protocol.ToolCall{ID: "hook", Name: name, Args: args})
	return protocol.Response{Content: out.Content}.Text(), out.IsError, nil
}

// Reports whether a call matches a permission rule, undetermined when a subject hides what it runs
func (a *App) ruleMatches(rule string, call protocol.ToolCall) (bool, bool) {
	r, err := policy.ParseRule(rule)
	if err != nil {
		return false, true
	}
	if r.Tool != call.Name && !plugin.SafeGlob(r.Tool, call.Name) {
		return false, true
	}
	if !r.HasPattern {
		return true, true
	}
	for _, s := range a.Rules.SubjectsOf(call) {
		if s.Unsafe {
			return true, false
		}
	}
	compiled, err := policy.NewRules(policy.RuleSet{Allow: []string{rule}})
	if err != nil {
		return false, true
	}
	compiled.Default, compiled.Dir, compiled.DirFunc = policy.Deny, a.Root, a.Shell.Dir
	return compiled.Decide(call) == policy.Allow, true
}

// Fires SessionStart for a session with the source, applying the title, context and watch paths hooks gave
func (a *App) SessionStarted(ctx context.Context, sess *session.Session, source string) error {
	fields := map[string]any{"source": source, "model": a.endpointOf(sess).Model}
	if kind, ok := a.KindOf(sess); ok {
		fields["agent_type"] = kind.Name
	}
	if sess.Named() {
		fields["session_title"] = sess.Title()
	}
	if source == "resume" || source == "fork" {
		if last := lastResponse(sess); !last.IsZero() {
			fields["seconds_since_last_response"] = int(time.Since(last).Seconds())
			fields["context_tokens"] = sess.Context()
			fields["prompt_cache_likely_expired"] = time.Since(last) > time.Hour
		}
	}
	a.withEnvFile(func(file string) {
		fields["env_file"] = file
	})
	v, err := a.FireSession(ctx, sess, agent.SessionStart, source, fields)
	if err != nil {
		return err
	}
	a.applyEnvFile()
	if source == "startup" || source == "resume" {
		a.startMonitors("always")
	}
	if v.Title != "" {
		sess.Rename(v.Title)
	}
	if v.Context != "" {
		a.queue(sess, protocol.Text{Text: agentReminder("SessionStart hook", v.Context)})
	}
	if v.InitialMessage != "" {
		a.queue(sess, protocol.Text{Text: v.InitialMessage})
	}
	a.watch.add(v.WatchPaths)
	if v.ReloadSkills {
		if err := a.ReloadPlugins(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Time of the last assistant turn of a session, zero before any
func lastResponse(sess *session.Session) time.Time {
	entries := sess.Entries()
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Role == protocol.RoleAssistant {
			return entries[i].Time
		}
	}
	return time.Time{}
}

// Fires the SessionStart hooks of the selected session at startup, resume, clear or compaction
func (a *App) SessionStart(ctx context.Context, source string) error {
	return a.SessionStarted(ctx, a.Session, source)
}

// Puts the plan of a session or its mission back to earlier steps after a hook refused a new one
func (a *App) restorePlan(ctx context.Context, sess *session.Session, steps []planStep) {
	list := make([]mission.Step, 0, len(steps))
	for _, s := range steps {
		list = append(list, mission.Step{Text: s.Text, Status: s.Status})
	}
	if id := missionIDFrom(ctx); id != "" {
		if store := a.MissionsFor(ctx); store != nil {
			store.SetPlan(id, list)
		}
		return
	}
	if sess != nil {
		sess.Log("", mission.SessionPlanKind, mission.SessionPlan{Steps: list})
	}
}

// Marks a step active again after a hook refused its completion
func (a *App) reopenStep(ctx context.Context, sess *session.Session, i int) {
	if id := missionIDFrom(ctx); id != "" {
		if store := a.MissionsFor(ctx); store != nil {
			store.SetStep(id, i+1, mission.StepActive)
		}
		return
	}
	if sess == nil {
		return
	}
	p, ok := currentSessionPlan(sess)
	if !ok || i >= len(p.Steps) {
		return
	}
	for j := range p.Steps {
		if p.Steps[j].Status == mission.StepActive {
			p.Steps[j].Status = mission.StepPending
		}
	}
	p.Steps[i].Status = mission.StepActive
	sess.Log("", mission.SessionPlanKind, p)
}

// Fires SessionEnd for a session with the reason
func (a *App) SessionEnded(ctx context.Context, sess *session.Session, reason string) error {
	_, err := a.FireSession(ctx, sess, agent.SessionEnd, reason, map[string]any{"reason": reason})
	return err
}

// Fires the Setup hooks with the trigger, init or maintenance, then the startup SessionStart hooks
func (a *App) Setup(ctx context.Context, trigger string) error {
	a.withEnvFile(func(string) {})
	if _, err := a.FireSession(ctx, a.Session, agent.Setup, trigger, map[string]any{"trigger": trigger}); err != nil {
		return err
	}
	a.applyEnvFile()
	return a.SessionStart(ctx, "startup")
}

// Fires Notification with the type and message
func (a *App) Notify(ctx context.Context, sess *session.Session, kind, message, title string) {
	fields := map[string]any{"message": message, "notification_type": kind}
	if title != "" {
		fields["title"] = title
	}
	a.FireSession(ctx, sess, agent.Notification, kind, fields)
}

// Fires PreModelSwitch for a session, false when a hook refused the switch
func (a *App) modelSwitch(ctx context.Context, sess *session.Session, from, to string) error {
	v, err := a.FireSession(ctx, sess, agent.PreModelSwitch, to, map[string]any{"from_model": from, "to_model": to})
	if err != nil {
		return err
	}
	if v.Block {
		return fmt.Errorf("model switch to %s blocked by hook: %s", to, v.Reason)
	}
	if v.Decision == policy.Ask {
		ans, err := a.Memory.Approve(ctx, policy.Request{Call: protocol.ToolCall{Name: "ModelSwitch", Args: json.RawMessage(jsonOf(map[string]string{"from": from, "to": to}))}, Reason: "PreModelSwitch hook asks"})
		if err != nil {
			return err
		}
		if !ans.Allowed {
			return fmt.Errorf("model switch to %s refused", to)
		}
	}
	return nil
}

// Fires PostModelSwitch for a session, its context queued for the next run
func (a *App) modelSwitched(ctx context.Context, sess *session.Session, from, to string) {
	v, err := a.FireSession(ctx, sess, agent.PostModelSwitch, to, map[string]any{"from_model": from, "to_model": to})
	if err == nil && v.Context != "" {
		a.queue(sess, protocol.Text{Text: agentReminder("PostModelSwitch hook", v.Context)})
	}
}

// Fires TeammateIdle for a run a workflow started as it ends, true with the reason to keep it working
func (a *App) teammateIdle(ctx context.Context, r *Run, f *Flow) (string, bool) {
	name := r.Session.Title()
	if kind, ok := a.KindOf(r.Session); ok {
		name = kind.Name
	}
	v, err := a.FireSession(ctx, r.Session, agent.TeammateIdle, "", map[string]any{"teammate_name": name, "team_name": f.Name, "agent_id": r.Session.ID(), "last_assistant_message": r.Response().Text()})
	if err != nil || !v.Block || v.Stop {
		return "", false
	}
	return v.Reason, true
}

// Fires ConfigChange with the source of the changed file, false when a hook refused the change
func (a *App) configChanged(ctx context.Context, source, path string) bool {
	fields := map[string]any{"source": source}
	if path != "" {
		fields["file_path"] = path
	}
	v, err := a.FireSession(ctx, a.Session, agent.ConfigChange, source, fields)
	if err != nil {
		return true
	}
	return !v.Block || source == "policy_settings"
}

// Fires InstructionsLoaded for an instruction file a session took in
func (a *App) instructionsLoaded(ctx context.Context, sess *session.Session, in plugin.Instruction, trigger string) {
	fields := map[string]any{"file_path": in.Path, "memory_type": in.Scope, "load_reason": in.Reason}
	if len(in.Paths) > 0 {
		fields["globs"] = in.Paths
	}
	if trigger != "" {
		fields["trigger_file_path"] = trigger
	}
	if in.Parent != "" {
		fields["parent_file_path"] = in.Parent
	}
	go a.FireSession(context.WithoutCancel(ctx), sess, agent.InstructionsLoaded, in.Reason, fields)
}

// Fires StopFailure for a run that ended on a provider error
func (a *App) stopFailure(ctx context.Context, sess *session.Session, err error) {
	category := "unknown"
	var pe *protocol.Error
	if errors.As(err, &pe) {
		category = failureCategory(pe)
	} else if errors.Is(err, agent.ErrRounds) {
		return
	}
	a.FireSession(ctx, sess, agent.StopFailure, category, map[string]any{"error": category, "error_details": err.Error(), "last_assistant_message": err.Error()})
}

// Category of a provider error as StopFailure hooks hear it
func failureCategory(e *protocol.Error) string {
	switch {
	case e.Auth:
		return "authentication_failed"
	case e.Billing:
		return "billing_error"
	case e.Status == 429:
		return "rate_limit"
	case e.Transient || e.Status == 529 || e.Status == 503:
		return "overloaded"
	case e.Status == 404:
		return "model_not_found"
	case e.Status == 400 || e.Status == 422:
		return "invalid_request"
	case e.Status >= 500:
		return "server_error"
	}
	return "unknown"
}

// Wraps an approver so PermissionRequest hooks may settle a prompt before the person sees it
type hookApprover struct {
	app *App
}

func (h hookApprover) Approve(ctx context.Context, req policy.Request) (policy.Answer, error) {
	a := h.app
	sess := a.SessionFor(ctx)
	fields := map[string]any{"tool_name": req.Call.Name, "tool_input": json.RawMessage(req.Call.Args)}
	if req.Key != "" {
		fields["permission_suggestions"] = []map[string]any{{"type": "addRules", "rules": []map[string]string{{"toolName": req.Call.Name, "ruleContent": req.Key}}, "behavior": "allow", "destination": "session"}}
	}
	v, err := a.FireSession(ctx, sess, agent.PermissionRequest, req.Call.Name, fields)
	if err != nil {
		return policy.Answer{}, err
	}
	switch v.Behavior {
	case "allow":
		a.applyUpdates(v.Updates)
		if len(v.Input) > 0 {
			return policy.Answer{Allowed: true, By: "PermissionRequest hook", Reason: "allowed by hook with updated input"}, &updatedInput{Input: v.Input}
		}
		return policy.Answer{Allowed: true, By: "PermissionRequest hook"}, nil
	case "deny":
		if v.Interrupt {
			if run := RunFrom(ctx); run != "" {
				a.Control.Interrupt(run)
			}
		}
		reason := v.Message
		if reason == "" {
			reason = "denied by PermissionRequest hook"
		}
		return policy.Answer{By: "PermissionRequest hook", Reason: reason}, &policy.Denied{Call: req.Call, Reason: reason}
	}
	go a.Notify(context.WithoutCancel(ctx), sess, "permission_prompt", "Claude needs your permission to use "+req.Call.Name, "Permission needed")
	return a.Memory.Approve(ctx, req)
}

// Error carrying the input a PermissionRequest hook rewrote while allowing the call
type updatedInput struct {
	Input json.RawMessage
}

func (u *updatedInput) Error() string { return "permission hook updated the input" }

// Applies the permission updates a hook asked to keep, session ones in memory and settings ones to files
func (a *App) applyUpdates(updates []agent.PermissionUpdate) {
	for _, u := range updates {
		switch u.Type {
		case "addRules", "replaceRules", "removeRules":
			set := a.Rules.Set()
			for _, r := range u.Rules {
				text := r.ToolName
				if r.RuleContent != "" {
					text = r.ToolName + "(" + r.RuleContent + ")"
				}
				decision := policy.Decision(u.Behavior)
				switch u.Type {
				case "removeRules":
					set.Allow = removeRule(set.Allow, text)
					set.Deny = removeRule(set.Deny, text)
					set.Ask = removeRule(set.Ask, text)
				default:
					switch decision {
					case policy.Allow:
						set.Allow = append(set.Allow, text)
					case policy.Deny:
						set.Deny = append(set.Deny, text)
					case policy.Ask:
						set.Ask = append(set.Ask, text)
					}
				}
				if u.Destination != "" && u.Destination != "session" {
					scope := ScopeUser
					if u.Destination == "projectSettings" || u.Destination == "localSettings" {
						scope = ScopeProject
					}
					if u.Type == "removeRules" {
						a.RemoveRule(RuleEntry{Decision: decision, Rule: text})
					} else {
						a.AddRule(scope, decision, text)
					}
				}
			}
			a.Rules.Reset(set)
		case "setMode":
			if m, err := policy.ParseMode(u.Mode); err == nil && m != policy.ModeBypass {
				a.Mode.Set(m)
			}
		case "addDirectories":
			for _, d := range u.Directories {
				a.AddDir(context.Background(), d, "permission_hook")
			}
		}
	}
}

func removeRule(list []string, text string) []string {
	out := list[:0]
	for _, r := range list {
		if r != text {
			out = append(out, r)
		}
	}
	return out
}

// Fires PermissionDenied for a call the policy refused, true to ask the person instead
func (a *App) permissionDenied(ctx context.Context, call protocol.ToolCall, reason string) bool {
	v, err := a.FireSession(ctx, a.SessionFor(ctx), agent.PermissionDenied, call.Name, map[string]any{"tool_name": call.Name, "tool_input": json.RawMessage(call.Args), "reason": reason})
	if err != nil {
		return false
	}
	return v.Retry
}

// Hook that watches the working directory and the watched files after every tool, firing CwdChanged and FileChanged
type watchHook struct {
	agent.Hooks
	app  *App
	sess *session.Session
}

func (h watchHook) BeforeTool(ctx context.Context, call protocol.ToolCall) (agent.Advice, error) {
	h.app.watch.cwd(h.app.Shell.Dir())
	return agent.Advice{Call: call}, nil
}

// Fires the change events for what the tool changed and loads path scoped rules and skills for files it touched
func (h watchHook) AfterTool(ctx context.Context, call protocol.ToolCall, res protocol.ToolResult) (protocol.ToolResult, error) {
	a := h.app
	if path := touchedPath(call); path != "" && !res.IsError {
		for _, in := range plugin.Matching(a.Instructions(), a.Shell.Dir(), path) {
			if a.markLoaded(h.sess, "rule:"+in.Path) {
				continue
			}
			res.Content = append(res.Content, protocol.Text{Text: agentReminder("Instructions from "+in.Path+" apply to "+path, in.Text)})
			a.instructionsLoaded(ctx, h.sess, plugin.Instruction{Path: in.Path, Scope: in.Scope, Paths: in.Paths, Reason: plugin.LoadPathGlob}, path)
		}
		res.Content = append(res.Content, a.pathSkills(ctx, h.sess, path)...)
	}
	a.Poll(ctx)
	return res, nil
}

// Polls the watched files and the working directory after every tool round too
func (h watchHook) AfterRound(ctx context.Context, calls []protocol.ToolCall, results []protocol.ToolResult) ([]protocol.Part, error) {
	h.app.Poll(ctx)
	return nil, nil
}

// Fires StopFailure when the run ends on an error
func (h watchHook) OnFailure(ctx context.Context, err error) {
	h.app.stopFailure(ctx, h.sess, err)
}

// Path argument of a file tool call, empty for other tools
func touchedPath(call protocol.ToolCall) string {
	switch call.Name {
	case "Read", "Write", "Edit", "Outline":
	default:
		return ""
	}
	var args struct {
		Path string `json:"file_path"`
	}
	if json.Unmarshal(call.Args, &args) != nil {
		return ""
	}
	return args.Path
}

// Files and directories watched for change events and the configuration files watched for reloads
type watcher struct {
	mu    sync.Mutex
	dir   string
	files map[string]time.Time
	// Files named by hook matchers and watch paths, relative to the working directory or absolute
	names []string
	// Configuration files and their modification times
	config map[string]time.Time
	// Skill directories and the newest modification time under each
	skills map[string]time.Time
}

func newWatcher() *watcher {
	return &watcher{files: map[string]time.Time{}, config: map[string]time.Time{}, skills: map[string]time.Time{}}
}

// Adds files to watch, absolute or relative to the working directory
func (w *watcher) add(names []string) {
	if len(names) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, n := range names {
		if n == "" {
			continue
		}
		found := false
		for _, have := range w.names {
			found = found || have == n
		}
		if !found {
			w.names = append(w.names, n)
		}
	}
}

// Forgets the files CwdChanged and FileChanged hooks added, done when the directory changes
func (w *watcher) clearAdded() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.names = nil
}

// Records the working directory, true with the old one when it changed
func (w *watcher) cwd(dir string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dir == "" {
		w.dir = dir
		return "", false
	}
	if w.dir == dir {
		return "", false
	}
	old := w.dir
	w.dir = dir
	return old, true
}

// Modification time of a path, zero when it does not exist
func mtime(path string) time.Time {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// Files whose modification time changed since the last look, as absolute paths
func (w *watcher) changed(dir string, matchers []string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	names := append(append([]string(nil), matchers...), w.names...)
	var out []string
	for _, n := range names {
		path := n
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, n)
		}
		now := mtime(path)
		was, seen := w.files[path]
		w.files[path] = now
		if seen && !now.Equal(was) {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// Configuration files whose modification time changed since the last look, with their sources
func (w *watcher) changedConfig(files map[string]string) []configChange {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []configChange
	for path, source := range files {
		now := mtime(path)
		was, seen := w.config[path]
		w.config[path] = now
		if seen && !now.Equal(was) {
			out = append(out, configChange{path: path, source: source})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// Skill directories whose newest file changed since the last look
func (w *watcher) changedSkills(dirs []string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	changed := false
	for _, dir := range dirs {
		now := newest(dir)
		was, seen := w.skills[dir]
		w.skills[dir] = now
		if seen && !now.Equal(was) {
			changed = true
		}
	}
	return changed
}

// Newest modification time of any SKILL.md or agent file under a directory, zero for none
func newest(dir string) time.Time {
	var out time.Time
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) == ".md" {
			if t := mtime(path); t.After(out) {
				out = t
			}
		}
		return nil
	})
	return out
}

// One configuration file that changed
type configChange struct {
	path   string
	source string
}

// Configuration files watched for ConfigChange, by source
func (a *App) configFiles() map[string]string {
	out := map[string]string{}
	layers, err := configLayers(a.Dir)
	if err != nil {
		return out
	}
	if len(layers) > 0 {
		out[layers[0]] = "user_settings"
		out[filepath.Join(filepath.Dir(layers[0]), RulesFile)] = "user_settings"
		out[filepath.Join(filepath.Dir(layers[0]), HooksFile)] = "user_settings"
	}
	if len(layers) > 1 {
		out[layers[1]] = "project_settings"
		out[filepath.Join(filepath.Dir(layers[1]), RulesFile)] = "project_settings"
		out[filepath.Join(filepath.Dir(layers[1]), HooksFile)] = "project_settings"
	}
	if home, err := os.UserHomeDir(); err == nil {
		out[filepath.Join(home, ".claude", "settings.json")] = "user_settings"
	}
	out[filepath.Join(a.Root, ".claude", "settings.json")] = "project_settings"
	out[filepath.Join(a.Root, ".claude", "settings.local.json")] = "local_settings"
	out[filepath.Join(managedSettings, "managed-settings.json")] = "policy_settings"
	return out
}

// Directory managed policy settings sit in
const managedSettings = "/etc/claude-code"

// Skill directories watched for live reload
func (a *App) skillDirs() []string {
	var out []string
	if dir, err := configDirOf(); err == nil {
		out = append(out, filepath.Join(dir, "skills"), filepath.Join(dir, "agents"), filepath.Join(dir, "commands"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".claude", "skills"), filepath.Join(home, ".claude", "agents"), filepath.Join(home, ".claude", "commands"), filepath.Join(home, ".agents", "skills"))
	}
	for _, sub := range []string{".icebath", ".claude", ".agents"} {
		out = append(out, filepath.Join(a.Root, sub, "skills"), filepath.Join(a.Root, sub, "agents"), filepath.Join(a.Root, sub, "commands"))
	}
	return out
}

// Looks at the watched files, the working directory and the configuration, firing the change events and reloading
func (a *App) Poll(ctx context.Context) {
	if a.watch == nil {
		return
	}
	a.pollMu.Lock()
	defer a.pollMu.Unlock()
	dir := a.Shell.Dir()
	if old, moved := a.watch.cwd(dir); moved {
		a.watch.clearAdded()
		v, err := a.FireSession(ctx, a.Session, agent.CwdChanged, "", map[string]any{"old_cwd": old, "new_cwd": dir})
		if err == nil {
			a.watch.add(v.WatchPaths)
			a.applyEnvFile()
		}
	}
	watched := a.hooksFor(a.Session, Kind{}).Watched()
	for _, path := range a.watch.changed(dir, watched) {
		a.withEnvFile(func(string) {})
		a.FireSession(ctx, a.Session, agent.FileChanged, filepath.Base(path), map[string]any{"file_path": path, "event": "change"})
		a.applyEnvFile()
	}
	changes := a.watch.changedConfig(a.configFiles())
	reload := false
	for _, c := range changes {
		if a.configChanged(ctx, c.source, c.path) {
			reload = true
		}
	}
	if reload {
		if err := a.ReloadConfig(ctx); err != nil {
			a.send(NoticeMsg{Level: Warn, Text: "reload: " + err.Error()})
		}
	}
	if a.watch.changedSkills(a.skillDirs()) {
		if a.configChanged(ctx, "skills", "") {
			if err := a.ReloadPlugins(ctx); err != nil {
				a.send(NoticeMsg{Level: Warn, Text: "reload skills: " + err.Error()})
			}
		}
	}
}

// Reads the rules and hooks files again after a change on disk
func (a *App) ReloadConfig(ctx context.Context) error {
	if err := a.ReloadHooks(); err != nil {
		return err
	}
	return a.ReloadRules()
}

// File hooks write environment lines into, applied to the shell after they run
func (a *App) withEnvFile(fn func(file string)) {
	dir := ScratchDir(a.Session.ID())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	file := filepath.Join(dir, "hook-env.sh")
	os.WriteFile(file, nil, 0o600)
	a.envFile = file
	a.envMu.Lock()
	a.extraEnv = []string{"CLAUDE_ENV_FILE=" + file, "ICEBATH_ENV_FILE=" + file}
	a.envMu.Unlock()
	fn(file)
}

// Sources the environment file hooks wrote into the shell, so later commands see their variables
func (a *App) applyEnvFile() {
	a.envMu.Lock()
	a.extraEnv = nil
	a.envMu.Unlock()
	if a.envFile == "" {
		return
	}
	data, err := os.ReadFile(a.envFile)
	if err != nil || strings.TrimSpace(string(data)) == "" {
		return
	}
	var out bytes.Buffer
	a.Shell.RunWith(context.Background(), ". "+shellQuote(a.envFile), &out, &out, 10*time.Second)
}

// Text quoted for the shell
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Runs a helper command with the environment and returns what it printed
func runHelper(command string, env []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = env
	return cmd.Output()
}

// Reminder wrapper for context a hook or rule adds, the text named by its label
func agentReminder(label, text string) string {
	return "<system-reminder>\n" + label + "\n" + text + "\n</system-reminder>"
}

// Hook that fires TaskCreated and TaskCompleted for the steps of a plan the Plan tool changes
type taskHook struct {
	agent.Hooks
	app  *App
	sess *session.Session
	mu   sync.Mutex
	// Steps as they stood before the call under way, by call id
	before map[string][]planStep
}

type planStep struct {
	Text   string
	Status string
}

// Steps of the plan of the calling run, from its mission or its session
func (h *taskHook) steps(ctx context.Context) []planStep {
	var out []planStep
	if id := missionIDFrom(ctx); id != "" {
		if store := h.app.MissionsFor(ctx); store != nil {
			if m, ok := store.Get(id); ok {
				for _, s := range m.Plan {
					out = append(out, planStep{Text: s.Text, Status: s.Status})
				}
			}
		}
		return out
	}
	if h.sess != nil {
		if p, ok := currentSessionPlan(h.sess); ok {
			for _, s := range p.Steps {
				out = append(out, planStep{Text: s.Text, Status: s.Status})
			}
		}
	}
	return out
}

func (h *taskHook) BeforeTool(ctx context.Context, call protocol.ToolCall) (agent.Advice, error) {
	if call.Name == planToolName {
		h.mu.Lock()
		if h.before == nil {
			h.before = map[string][]planStep{}
		}
		h.before[call.ID] = h.steps(ctx)
		h.mu.Unlock()
	}
	return agent.Advice{Call: call}, nil
}

// Fires TaskCreated for new steps and TaskCompleted for steps marked done, a block undoing the step
func (h *taskHook) AfterTool(ctx context.Context, call protocol.ToolCall, res protocol.ToolResult) (protocol.ToolResult, error) {
	if call.Name != planToolName || res.IsError {
		return res, nil
	}
	h.mu.Lock()
	before := h.before[call.ID]
	delete(h.before, call.ID)
	h.mu.Unlock()
	after := h.steps(ctx)
	was := map[string]string{}
	for _, s := range before {
		was[s.Text] = s.Status
	}
	for i, s := range after {
		status, existed := was[s.Text]
		id := fmt.Sprintf("%d", i+1)
		if !existed {
			v, err := h.app.FireSession(ctx, h.sess, agent.TaskCreated, "", map[string]any{"task_id": id, "task_subject": s.Text, "agent_id": h.sess.ID()})
			if err == nil && v.Block {
				h.app.restorePlan(ctx, h.sess, before)
				return protocol.ToolResult{CallID: res.CallID, Name: res.Name, Content: []protocol.Part{protocol.Text{Text: "task " + s.Text + " was refused by a TaskCreated hook: " + v.Reason}}, IsError: true}, nil
			}
			status = s.Status
		}
		if s.Status == "done" && status != "done" {
			v, err := h.app.FireSession(ctx, h.sess, agent.TaskCompleted, "", map[string]any{"task_id": id, "task_subject": s.Text, "agent_id": h.sess.ID()})
			if err == nil && (v.Block || v.Stop) {
				reason := v.Reason
				if reason == "" {
					reason = v.StopReason
				}
				h.app.reopenStep(ctx, h.sess, i)
				return protocol.ToolResult{CallID: res.CallID, Name: res.Name, Content: []protocol.Part{protocol.Text{Text: "step " + s.Text + " is not complete, a TaskCompleted hook said: " + reason}}, IsError: true}, nil
			}
		}
	}
	return res, nil
}
