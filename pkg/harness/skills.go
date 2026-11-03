package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/plugin"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	shellpkg "github.com/nickheyer/icebath/pkg/shell"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Name the model calls to load a skill
const SkillTool = "Skill"

// Skills one typed line may stack beyond the first
const stackLimit = 5

// What a skill invocation puts in force for the run, tools allowed and withheld and the model and effort asked for
type activation struct {
	mu       sync.Mutex
	allowed  *policy.Rules
	withheld []string
	model    string
	effort   string
}

// Adds the grants, withheld tools, model and effort of a skill
func (ac *activation) add(a *App, s *plugin.Skill, vars map[string]string) error {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if rules := s.AllowedRules(vars); len(rules) > 0 {
		set := policy.RuleSet{Allow: rules}
		if ac.allowed != nil {
			set.Allow = append(ac.allowed.Rules(policy.Allow), rules...)
		}
		compiled, err := policy.NewRules(set)
		if err != nil {
			return fmt.Errorf("skill %s allowed-tools: %w", s.Name, err)
		}
		compiled.Default, compiled.Dir, compiled.DirFunc = policy.Deny, a.Root, a.Shell.Dir
		ac.allowed = compiled
	}
	for _, t := range s.DisallowedTools {
		name, _, _ := strings.Cut(t, "(")
		if name == "EndConversation" {
			continue
		}
		if !slices.Contains(ac.withheld, name) {
			ac.withheld = append(ac.withheld, name)
		}
	}
	if s.Model != "" && s.Model != "inherit" {
		ac.model = s.Model
	}
	if s.Effort != "" {
		ac.effort = s.Effort
	}
	return nil
}

func (ac *activation) grants(call protocol.ToolCall) bool {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return ac.allowed != nil && ac.allowed.Decide(call) == policy.Allow
}

func (ac *activation) withholds(name string) bool {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return slices.Contains(ac.withheld, name)
}

func (ac *activation) overrides() (string, string) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return ac.model, ac.effort
}

// Activation waiting for the next run of a session, created when none waits
func (a *App) pendingActivation(sess *session.Session) *activation {
	a.lib.mu.Lock()
	defer a.lib.mu.Unlock()
	ac := a.lib.activations["session:"+sess.ID()]
	if ac == nil {
		ac = &activation{}
		a.lib.activations["session:"+sess.ID()] = ac
	}
	return ac
}

// Activation of a run, created when none exists
func (a *App) runActivation(run string) *activation {
	a.lib.mu.Lock()
	defer a.lib.mu.Unlock()
	ac := a.lib.activations["run:"+run]
	if ac == nil {
		ac = &activation{}
		a.lib.activations["run:"+run] = ac
	}
	return ac
}

// Hook carrying skill activations, grants and withheld tools for the run, cleared by the next user message
type activationHook struct {
	agent.Hooks
	app  *App
	sess *session.Session
	ep   Endpoint
}

// Takes the activation waiting on the session for the run and queued context for the input
func (h activationHook) BeforeRun(ctx context.Context, input []protocol.Part) ([]protocol.Part, error) {
	run := RunFrom(ctx)
	h.app.lib.mu.Lock()
	if ac, ok := h.app.lib.activations["session:"+h.sess.ID()]; ok {
		delete(h.app.lib.activations, "session:"+h.sess.ID())
		h.app.lib.activations["run:"+run] = ac
	} else {
		delete(h.app.lib.activations, "run:"+run)
	}
	h.app.lib.mu.Unlock()
	return append(input, h.app.takeQueued(h.sess)...), nil
}

// Withholds the tools a skill removed and switches the model a skill asked for
func (h activationHook) BeforeTurn(ctx context.Context, req *protocol.Request) error {
	ac := h.app.runActivation(RunFrom(ctx))
	kept := req.Tools[:0]
	for _, t := range req.Tools {
		if !ac.withholds(t.Name) {
			kept = append(kept, t)
		}
	}
	req.Tools = kept
	model, effort := ac.overrides()
	if model != "" {
		id, err := resolveModel(model, h.ep)
		if err != nil {
			return err
		}
		req.Model = id
	}
	if effort != "" {
		req.Effort = effort
	}
	return nil
}

// Refuses a withheld tool and settles allow for a granted call
func (h activationHook) BeforeTool(ctx context.Context, call protocol.ToolCall) (agent.Advice, error) {
	ac := h.app.runActivation(RunFrom(ctx))
	if ac.withholds(call.Name) {
		return agent.Advice{Call: call}, fmt.Errorf("tool %s is withheld while the active skill runs", call.Name)
	}
	if ac.grants(call) {
		return agent.Advice{Call: call, Decision: policy.Allow}, nil
	}
	return agent.Advice{Call: call}, nil
}

// Tool that loads a skill into the conversation by name
type skillTool struct{ app *App }

func (t skillTool) Definition() protocol.Tool {
	return protocol.Tool{
		Name:        SkillTool,
		Description: "Loads a skill by name, its full instructions arriving as the result and staying in the conversation. Call it when the task at hand is one a listed skill covers, before doing the work your own way.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"skill":{"type":"string","description":"Name of the skill as listed"},"args":{"type":"string","description":"Arguments for the skill, as a person would type them after its name"}},"required":["skill"],"additionalProperties":false}`),
	}
}

func (skillTool) Annotations() tool.Annotations { return tool.Annotations{ReadOnly: true} }

func (t skillTool) Run(ctx context.Context, args json.RawMessage) (tool.Result, error) {
	var in struct {
		Skill string `json:"skill"`
		Args  string `json:"args"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tool.Error(err.Error()), nil
	}
	lib := t.app.Library()
	if lib == nil {
		return tool.Error("no skills are loaded"), nil
	}
	name := strings.TrimPrefix(strings.TrimSpace(in.Skill), "/")
	s, ok := lib.Skill(name)
	if !ok {
		var names []string
		for _, have := range lib.ModelSkills() {
			names = append(names, have.Name)
		}
		return tool.Errorf("no skill %q, the skills are %s", name, strings.Join(names, ", ")), nil
	}
	if s.DisableModelInvocation {
		return tool.Errorf("skill %s: %v", s.Name, plugin.ErrModelInvocation), nil
	}
	sess := t.app.SessionFor(ctx)
	rendered, err := t.app.renderSkill(ctx, s, in.Args, sess)
	if err != nil {
		return tool.Error(err.Error()), nil
	}
	t.app.used(s.Name)
	if s.Forked() {
		answer, err := t.app.ForkSkill(ctx, t.app.LiveOf(sess), s, rendered, true)
		if err != nil {
			return tool.Error(err.Error()), nil
		}
		return tool.Text(answer), nil
	}
	ac := t.app.runActivation(RunFrom(ctx))
	vars := t.app.skillVars(sess, s)
	if err := ac.add(t.app, s, vars); err != nil {
		return tool.Error(err.Error()), nil
	}
	t.app.skillInvoked(sess, s)
	if t.app.sameContent(sess, s.Name, rendered.Text) {
		return tool.Text("Skill " + s.Name + " is already loaded in this conversation, its instructions above still apply."), nil
	}
	return tool.Result{Content: t.app.skillParts(rendered)}, nil
}

// Variables a skill is rendered with for a session
func (a *App) skillVars(sess *session.Session, s *plugin.Skill) map[string]string {
	id := ""
	if sess != nil {
		id = sess.ID()
	}
	kind, _ := a.KindOf(sess)
	return plugin.TextVars(id, a.Root, kind.Effort, s)
}

// Renders a skill for a session, inline commands run in the session shell under the rules
func (a *App) renderSkill(ctx context.Context, s *plugin.Skill, args string, sess *session.Session) (plugin.Rendered, error) {
	vars := a.skillVars(sess, s)
	r := plugin.Render{Args: args, Vars: vars, Dir: a.Shell.Dir()}
	if sess != nil {
		r.Scratch = ScratchDir(sess.ID())
	}
	if !a.Config.Plugins.NoSkillShell {
		grants, _ := policy.NewRules(policy.RuleSet{Allow: s.AllowedRules(vars)})
		grants.Dir, grants.DirFunc = a.Root, a.Shell.Dir
		r.Shell = func(ctx context.Context, command, sh string) (string, error) {
			call := protocol.ToolCall{Name: "Shell", Args: json.RawMessage(jsonOf(map[string]string{"command": command}))}
			decision, why := a.Mode.Explain(call)
			if decision == policy.Deny {
				return "", fmt.Errorf("shell command permission check failed, %s", why)
			}
			if decision == policy.Ask && !a.Mode.Bypassing() && grants.Decide(call) != policy.Allow {
				return "", fmt.Errorf("shell command needs approval, %s, pre-approve it with allowed-tools", why)
			}
			var out bytes.Buffer
			code := 0
			if sh == "powershell" {
				exe := shellpkg.Exec{Path: powershellName(), Args: []string{"-NoProfile", "-NonInteractive", "-Command", command}, Dir: a.Shell.Dir(), Stdout: &out, Stderr: &out}
				var err error
				if code, err = exe.Run(ctx); err != nil {
					return "", err
				}
			} else {
				code = a.Shell.RunWith(ctx, command, &out, &out, 0).ExitCode
			}
			if code != 0 && !(code == 1 && searchCommand(command)) {
				return "", fmt.Errorf("exit status %d: %s", code, strings.TrimSpace(out.String()))
			}
			return out.String(), nil
		}
	}
	return s.Render(ctx, r)
}

// Name of the PowerShell executable on this machine
func powershellName() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe"
	}
	return "pwsh"
}

// Reports whether a command line is a search or comparison whose exit 1 means nothing found
func searchCommand(line string) bool {
	first := strings.Fields(line)
	if len(first) == 0 {
		return false
	}
	switch filepath.Base(first[0]) {
	case "grep", "egrep", "fgrep", "rg", "ag", "ack", "diff", "cmp", "find", "test", "[", "git":
		return true
	}
	return false
}

// Parts a rendered skill puts into the conversation, its text then each referenced file
func (a *App) skillParts(r plugin.Rendered) []protocol.Part {
	parts := []protocol.Part{protocol.Text{Text: r.Text}}
	for _, f := range r.Files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		parts = append(parts, protocol.Text{Text: "Contents of " + f + ":\n" + string(data)})
	}
	return parts
}

// Skill a typed line names first, with the arguments after it, false for a line that names none
func (a *App) SkillOf(text string) (*plugin.Skill, string, bool) {
	lib := a.Library()
	text = strings.TrimSpace(text)
	if lib == nil || !strings.HasPrefix(text, "/") {
		return nil, "", false
	}
	name, args, _ := strings.Cut(strings.TrimPrefix(text, "/"), " ")
	s, ok := lib.Skill(name)
	if !ok || !s.UserInvocable {
		return nil, "", false
	}
	return s, strings.TrimSpace(args), true
}

// What a typed skill line expands into, the parts to send and the skill run in a subagent when forked
type Expansion struct {
	Parts  []protocol.Part
	Skills []*plugin.Skill
	// Skill run in a subagent, nil for an inline expansion
	Forked *plugin.Skill
	// Rendered content of the forked skill
	Rendered plugin.Rendered
}

// Expands a typed line starting with a skill, stacking later inline skills, firing UserPromptExpansion for each
func (a *App) ExpandSkills(ctx context.Context, live *Live, text string, single bool) (Expansion, error) {
	var out Expansion
	s, args, ok := a.SkillOf(text)
	if !ok {
		return out, errors.New("the line names no skill")
	}
	sess := live.Session
	chain := []*plugin.Skill{s}
	rest := args
	if !single {
		for len(chain) <= stackLimit {
			next, nargs, ok := a.SkillOf(rest)
			if !ok || next.Forked() || !next.UserInvocable {
				break
			}
			if chain[len(chain)-1].Forked() {
				break
			}
			chain = append(chain, next)
			rest = nargs
		}
	}
	if len(chain) > 1 {
		args = rest
	}
	for _, sk := range chain {
		rendered, err := a.renderSkill(ctx, sk, args, sess)
		if err != nil {
			return out, err
		}
		v, err := a.FireSession(ctx, sess, agent.UserPromptExpansion, sk.Name, map[string]any{"expansion_type": "slash_command", "command_name": sk.Name, "command_args": args, "command_source": sourceOf(sk), "prompt": rendered.Text})
		if err != nil {
			return out, err
		}
		if v.Block {
			return out, &agent.HookBlocked{HookEvent: agent.UserPromptExpansion, Reason: v.Reason}
		}
		if v.Context != "" {
			rendered.Text += "\n\n" + v.Context
		}
		a.used(sk.Name)
		a.skillInvoked(sess, sk)
		if sk.Forked() {
			out.Forked, out.Rendered = sk, rendered
			out.Skills = append(out.Skills, sk)
			continue
		}
		ac := a.pendingActivation(sess)
		if err := ac.add(a, sk, a.skillVars(sess, sk)); err != nil {
			return out, err
		}
		out.Skills = append(out.Skills, sk)
		if a.sameContent(sess, sk.Name, rendered.Text) {
			out.Parts = append(out.Parts, protocol.Text{Text: "Skill " + sk.Name + " is already loaded in this conversation, its instructions above still apply."})
			continue
		}
		out.Parts = append(out.Parts, a.skillParts(rendered)...)
	}
	return out, nil
}

// Where a skill came from, as hooks hear it
func sourceOf(s *plugin.Skill) string {
	if s.Plugin != nil {
		return "plugin"
	}
	return s.Origin
}

// Runs a forked skill in a fresh session of its agent kind, waiting for the answer or delivering it to the parent later
func (a *App) ForkSkill(ctx context.Context, parent *Live, s *plugin.Skill, r plugin.Rendered, wait bool) (string, error) {
	kind := s.Agent
	if kind == "" {
		kind = KindGeneral
	}
	if _, ok := a.Kind(kind); !ok {
		return "", fmt.Errorf("skill %s forks into agent %q, which is not known", s.Name, kind)
	}
	child, err := a.CreateKind(ctx, kind)
	if err != nil {
		return "", err
	}
	if parent != nil {
		child.Session.Label(parentLabel, parent.Session.ID())
	}
	child.Session.Label("skill", s.Name)
	child.Session.SetTitle(title("/" + s.Name))
	if s.Model != "" || s.Effort != "" {
		ac := a.pendingActivation(child.Session)
		ac.mu.Lock()
		ac.model, ac.effort = s.Model, s.Effort
		if s.Model == "inherit" {
			ac.model = ""
		}
		ac.mu.Unlock()
	}
	a.send(OpenedMsg{Live: child, Flow: ""})
	run, err := a.RunOn(context.WithoutCancel(ctx), child, "", a.skillParts(r))
	if err != nil {
		return "", err
	}
	finish := func() (string, error) {
		done, _ := a.Control.Done(run.ID)
		<-done
		if err := run.Err(); err != nil {
			return "", fmt.Errorf("skill %s: %w", s.Name, err)
		}
		return strings.TrimSpace(run.Response().Text()), nil
	}
	if wait || s.Waits() || parent == nil {
		answer, err := finish()
		if err != nil {
			return "", err
		}
		if answer == "" {
			answer = "The skill finished without a text answer."
		}
		return answer, nil
	}
	go func() {
		answer, err := finish()
		if err != nil {
			a.send(NoticeMsg{Level: Error, Text: err.Error()})
			return
		}
		a.send(NoticeMsg{Level: Success, Text: "skill " + s.Name + " finished"})
		a.deliver(parent.Session, "Result of /"+s.Name+" run in agent "+kind+":\n\n"+answer)
	}()
	return "", nil
}

// Skills with path globs matching the file that the session has not loaded, rendered as context
func (a *App) pathSkills(ctx context.Context, sess *session.Session, path string) []protocol.Part {
	lib := a.Library()
	if lib == nil {
		return nil
	}
	var parts []protocol.Part
	for _, s := range lib.SkillsForPath(a.Shell.Dir(), path) {
		if a.markLoaded(sess, "skill:"+s.Name) {
			continue
		}
		rendered, err := a.renderSkill(ctx, s, "", sess)
		if err != nil {
			a.send(NoticeMsg{Level: Warn, Text: "skill " + s.Name + ": " + err.Error()})
			continue
		}
		a.used(s.Name)
		parts = append(parts, protocol.Text{Text: "Skill " + s.Name + " applies to " + path + ":\n\n" + rendered.Text})
	}
	return parts
}

// Rendered text of every skill a kind preloads, in order, the first thirty two
func (a *App) preloadedSkills(ctx context.Context, sess *session.Session, kind Kind) string {
	lib := a.Library()
	if lib == nil || len(kind.Skills) == 0 {
		return ""
	}
	var blocks []string
	for i, name := range kind.Skills {
		if i >= 32 {
			break
		}
		s, ok := lib.Skill(name)
		if !ok || s.DisableModelInvocation {
			continue
		}
		rendered, err := a.renderSkill(ctx, s, "", sess)
		if err != nil {
			continue
		}
		blocks = append(blocks, "# Skill "+s.Name+"\n\n"+rendered.Text)
	}
	return strings.Join(blocks, "\n\n")
}

// Error of a session whose kind names no initial prompt, or that already ran
var ErrNoInitial = errors.New("no initial prompt")

// Initial prompt of a kind processed for a session that has not run yet, errNoInitial otherwise
func (a *App) InitialPrompt(ctx context.Context, live *Live) ([]protocol.Part, error) {
	kind, ok := a.KindOf(live.Session)
	if !ok || kind.InitialPrompt == "" || !live.Session.Empty() {
		return nil, ErrNoInitial
	}
	text := kind.InitialPrompt
	if _, _, ok := a.SkillOf(text); ok {
		exp, err := a.ExpandSkills(ctx, live, text, false)
		if err != nil {
			return nil, err
		}
		return exp.Parts, nil
	}
	return []protocol.Part{protocol.Text{Text: text}}, nil
}
