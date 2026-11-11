package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Registry command a typed name resolves to, with the busy policy and mid run variant key of its definition
type RegistryCommand struct {
	Name, BusyPolicy, BusyHandler string
}

// Command registry lookup of pkg/commands, assigned in cmd/icebath/wiring.go
var ResolveRegistryCommand func(name string) (RegistryCommand, bool)

// Runner surface of the messaging gateway's own command dispatch a local session's slash exec runs
type localCommandRunner interface {
	LocalSlashAccess(ctx context.Context, source *SessionSource, canonical string) string
	LocalExecQuickCommand(ctx context.Context, command, shell, cwd string) (string, error)
	LocalPluginCommand(ctx context.Context, handler plugins.PluginCommandHandler, event *MessageEvent, source *SessionSource, entry *SessionEntry, args string) (any, error)
	LocalBusySlashCommand(ctx context.Context, event *MessageEvent, def RegistryCommand, quickKey string, source *SessionSource) (any, error)
	LocalPromptCommand(ctx context.Context, canonical string, event *MessageEvent, source *SessionSource, quickKey string) (bool, any, error)
	LocalCommandHandler(canonical string) func(ctx context.Context, event *MessageEvent) (any, error)
	ParseReasoningCommandArgs(arg string) (string, bool)
}

// Set of names
func nameSet(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[name] = true
	}
	return out
}

var (
	sessionControlCommands    = nameSet("title")
	sessionHandlerControls    = nameSet("goal", "subgoal", "loop", "heartbeat", "review", "refine", "debug", "curator", "login", "autoprune")
	sessionPromptCommands     = nameSet("plan", "learn", "init", "moa", "blueprint", "egress")
	sessionSettingCommands    = nameSet("reasoning", "voice", "approvals", "codex-runtime", "footer", "busy", "verbose", "yolo", "fast", "personality")
	sessionApprovalCommands   = nameSet("approve", "deny")
	sessionInfoReads          = nameSet("toolsets", "platforms")
	sessionOwnerCommands      = nameSet("cron")
	sessionAutomationCommands = nameSet("goal", "subgoal", "loop", "heartbeat")
	sessionKanbanReads        = nameSet("", "help", "--help", "-h", "?", "list", "ls", "show", "stats", "assignees", "context", "runs", "log", "diagnostics", "diag")
	sessionEscapeHatches      = nameSet("stop", "approve", "deny")
)

// What to run instead, for the client local commands a shared gateway session cannot run
var RefusalHints = map[string]string{
	"export":   "Run `icebath profile export [name] -o <file>` in a terminal.",
	"import":   "Run `icebath profile import <archive> [--name <name>]` in a terminal.",
	"snapshot": "Run `icebath backup --quick` in a terminal (restore: `icebath --cli`, then /snapshot restore).",
	"handoff":  "Hand off from the classic CLI: `icebath --cli --resume <session>`, then /handoff <platform>.",
	"hatch":    "Generate a pet from the classic CLI (`icebath --cli`, then /hatch), or use /pet to pick one.",
	"worktree": "Start in a fresh worktree with `icebath -w`, or use the classic CLI (`icebath --cli`).",
	"palette":  "Type / to search commands (the composer completes and filters them).",
}

// Argument split as a shlex splitting handler parses it, an unclosed quote refused on the arg field
func splitCommandArg(arg string) ([]string, error) {
	if pystr.Strip(arg) == "" {
		return []string{}, nil
	}
	tokens, err := pyshlex.SplitPOSIX(arg, false)
	if err != nil {
		field, detail := "arg", err.Error()
		return nil, state.NewRuntimeStoreError("invalid_params", &field, &detail)
	}
	return tokens, nil
}

// Kanban verb as its handler parses it, board options skipped
func kanbanFirstToken(arg string) (string, error) {
	tokens, err := splitCommandArg(arg)
	if err != nil {
		return "", err
	}
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if token == "--board" {
			i++
		} else if !strings.HasPrefix(token, "--board=") {
			return pystr.Lower(token), nil
		}
	}
	return "", nil
}

// First whitespace separated word lowercased
func commandFirstWord(arg string) string {
	words := pystr.SplitWhitespace(arg, 1)
	if len(words) == 0 {
		return ""
	}
	return pystr.Lower(words[0])
}

// Whether the argument form of a read command only reads
func readArgument(name, arg string) (bool, error) {
	switch name {
	case "skills":
		word := commandFirstWord(arg)
		return word == "" || word == "pending" || word == "diff", nil
	case "usage", "suggestions":
		return pystr.Strip(arg) == "", nil
	case "memory":
		word := commandFirstWord(arg)
		return word == "" || word == "pending", nil
	case "kanban":
		token, err := kanbanFirstToken(arg)
		if err != nil {
			return false, err
		}
		return sessionKanbanReads[token], nil
	}
	return true, nil
}

// Whether the argument form of a read command is one of its review writes
func controlArgument(name, arg string) bool {
	if name != "skills" && name != "memory" {
		return false
	}
	switch commandFirstWord(arg) {
	case "approve", "apply", "reject", "deny", "drop", "approval", "mode":
		return true
	}
	return false
}

// Read, control or refused for a canonical registry command on a local session
func CommandVerdict(name, arg string) (string, error) {
	if name == "voice" && pystr.Lower(pystr.Strip(arg)) == "tts" {
		return "refused", nil
	}
	if sessionOwnerCommands[name] {
		tokens, err := splitCommandArg(arg)
		if err != nil {
			return "", err
		}
		if cronRead(tokens) != "" {
			return "read", nil
		}
		return "control", nil
	}
	if sessionControlCommands[name] || sessionHandlerControls[name] || sessionPromptCommands[name] ||
		sessionSettingCommands[name] || sessionApprovalCommands[name] || controlArgument(name, arg) {
		return "control", nil
	}
	if sessionInfoReads[name] {
		return "read", nil
	}
	if SessionReadCommands[name] {
		reads, err := readArgument(name, arg)
		if err != nil {
			return "", err
		}
		if reads {
			return "read", nil
		}
	}
	return "refused", nil
}

// Command name and argument of a slash exec or command dispatch request
func parseCommandParams(params *pyjson.Object, dispatch bool) (string, string, error) {
	allowed := []string{"session_id", "profile", "command"}
	if dispatch {
		allowed = []string{"session_id", "profile", "name", "arg"}
	}
	if paramsOutside(params, allowed...) {
		return "", "", runtimeRefusal("invalid_params")
	}
	for _, key := range params.Keys {
		if _, isStr := params.GetOr(key, nil).(string); !isStr {
			return "", "", runtimeRefusal("invalid_params")
		}
	}
	field := "command"
	if dispatch {
		field = "name"
	}
	text, _ := params.GetOr(field, "").(string)
	parts := pystr.SplitWhitespace(pystr.LstripChars(pystr.Strip(text), "/"), 1)
	if len(parts) == 0 || (dispatch && len(parts) != 1) {
		return "", "", runtimeRefusal("invalid_params")
	}
	if dispatch {
		arg, _ := params.GetOr("arg", "").(string)
		return parts[0], arg, nil
	}
	if len(parts) > 1 {
		return parts[0], parts[1], nil
	}
	return parts[0], "", nil
}

// Quick commands of the gateway config in the active profile
func quickCommands(ctx context.Context) any {
	quick := loadGatewayConfig(ctx, "")["quick_commands"]
	if !pyrepr.Truthy(quick) {
		return map[string]any{}
	}
	return quick
}

// Name and argument after following alias quick commands to a registry command
func resolveCommandAlias(ctx context.Context, name, arg string) (string, string, error) {
	quick := quickCommands(ctx)
	seen := map[string]bool{}
	for {
		if _, known := ResolveRegistryCommand(name); known {
			return name, arg, nil
		}
		var entry any
		switch table := quick.(type) {
		case map[string]any:
			value, ok := table[name]
			if !ok {
				return name, arg, nil
			}
			entry = value
		case []any:
			for _, item := range table {
				if item == name {
					return "", "", fmt.Errorf("TypeError: list indices must be integers or slices, not str")
				}
			}
			return name, arg, nil
		default:
			return name, arg, nil
		}
		if seen[name] {
			return "", "", runtimeRefusal("invalid_params")
		}
		seen[name] = true
		obj, isDict := entry.(map[string]any)
		if !isDict || (obj["type"] != "alias" && obj["type"] != "exec") {
			return "", "", runtimeRefusal("unsupported_command")
		}
		if obj["type"] == "exec" {
			return name, arg, nil
		}
		target, isStr := obj["target"].(string)
		if !isStr || pystr.Strip(target) == "" {
			return "", "", runtimeRefusal("invalid_params")
		}
		parts := pystr.SplitWhitespace(pystr.LstripChars(pystr.Strip(target), "/"), 1)
		name = parts[0]
		var joined []string
		for _, part := range append(parts[1:], arg) {
			if part != "" {
				joined = append(joined, part)
			}
		}
		arg = strings.Join(joined, " ")
	}
}

// Exec quick command shell or registered plugin command name, false for neither
func runnerCommand(ctx context.Context, name string) (string, string, bool, error) {
	quick := quickCommands(ctx)
	table, isDict := quick.(map[string]any)
	if !isDict {
		return "", "", false, fmt.Errorf("AttributeError: %s object has no attribute 'get'", pyrepr.Repr(pyrepr.TypeName(quick)))
	}
	if entry, ok := table[name].(map[string]any); ok && entry["type"] == "exec" {
		shell := ""
		if pyrepr.Truthy(entry["command"]) {
			shell = pyrepr.Str(entry["command"])
		}
		return "exec", shell, true, nil
	}
	for _, registered := range []string{name, strings.ReplaceAll(name, "_", "-")} {
		if _, ok := plugins.GetPluginCommandHandler(ctx, registered); ok {
			return "plugin", registered, true, nil
		}
	}
	return "", "", false, nil
}

// Quick command or plugin command reply for the session under the operator's slash access policy
func runRunnerCommand(ctx context.Context, a *SessionAuthority, runner localCommandRunner, live *LiveSession, name, arg, kind, shell string) (any, error) {
	if kind == "exec" {
		if shell == "" {
			return i18n.T(ctx, "gateway.quick_command.no_command", "", map[string]any{"command": name}), nil
		}
		if refused := runner.LocalSlashAccess(ctx, live.Source, name); refused != "" {
			return refused, nil
		}
		policy, err := policyForSource(ctx, a.Runner, live.Source)
		if err != nil {
			return nil, err
		}
		cwd := ""
		if policy != nil {
			cwd = policy.Cwd
		}
		if info, statErr := os.Stat(cwd); cwd == "" || statErr != nil || !info.IsDir() {
			return nil, runtimeRefusal("cwd_unavailable")
		}
		return runner.LocalExecQuickCommand(ctx, name, shell, cwd)
	}
	handler, ok := plugins.GetPluginCommandHandler(ctx, shell)
	if !ok {
		return nil, runtimeRefusal("unsupported_command")
	}
	text := "/" + name
	if arg != "" {
		text += " " + arg
	}
	event := NewMessageEvent(text)
	event.Source = live.Source
	entry := a.Runner.SessionStore().LookupBySessionKey(ctx, live.Route)
	output, err := runner.LocalPluginCommand(ctx, handler, event, live.Source, entry, arg)
	if err != nil {
		slog.Warn(fmt.Sprintf("Plugin command dispatch failed: %s", err))
		message := err.Error()
		if message == "" {
			message = fmt.Sprintf("%T", err)
		}
		return i18n.T(ctx, "cli.plugin.command_error", "", map[string]any{"error": message}), nil
	}
	return output, nil
}

// Skill directive of a local session's skill slash command, stacked skills included
func skillDirective(ctx context.Context, name, arg, route string) (*pyjson.Object, error) {
	scope := skills.NewScope(home.Dir(ctx), nil)
	cwd, err := tools.ResolveAgentCwd(tools.ToolEnvFrom(ctx))
	if err != nil {
		return nil, err
	}
	scope.Cwd = cwd
	commands, err := scope.GetInteractiveSkillCommands(ctx)
	if err != nil {
		return nil, err
	}
	key, found := skills.ResolveSlashKey(name, commands)
	if !found {
		return nil, runtimeRefusal("unsupported_command")
	}
	extra, instruction, err := scope.SplitStackedSkillCommands(ctx, arg, true)
	if err != nil {
		return nil, err
	}
	var message string
	var loaded, missing []string
	stacked := false
	if len(extra) > 0 {
		if message, loaded, missing, stacked, err = scope.BuildStackedSkillInvocationMessage(ctx, append([]string{key}, extra...), instruction, route); err != nil {
			return nil, err
		}
	}
	if !stacked {
		missing = nil
		if message, _, err = scope.BuildSkillInvocationMessage(ctx, key, arg, route, ""); err != nil {
			return nil, err
		}
	}
	if message == "" {
		return nil, runtimeRefusal("command_unavailable")
	}
	shown := pyrepr.Str(commands[key]["name"])
	if stacked {
		shown = strings.Join(loaded, ", ")
	}
	var display any
	if described, ok := skills.DescribeSkillInvocation(message, " "); ok {
		display = described
	}
	out := pyjson.ObjectOf("type", "skill", "name", shown, "message", message, "display", display)
	if len(missing) > 0 {
		out.Set("notice", i18n.T(ctx, "cli.skills.skipped_missing", "", map[string]any{"names": strings.Join(missing, ", ")}))
	}
	return out, nil
}

// Main's TUI text for a TUI session's status or context read, nil for any other read
func tuiReadCommand(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, name, arg string, before func(context.Context) error) (*pyjson.Object, error) {
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, err
	}
	if local, err := authorizeLocalSource(a.Runner, live.Source); err != nil {
		return nil, err
	} else if local == nil || !*local {
		return nil, nil
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return nil, err
	}
	render := tuiRead(policy, name)
	if render == nil {
		return nil, nil
	}
	if err := a.Authorize(ctx, actor, ref, "session:read"); err != nil {
		return nil, err
	}
	if err := before(ctx); err != nil {
		return nil, err
	}
	reply, err := runTUIRead(ctx, a, ref, live, policy, render, arg)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("type", reply["type"], "output", reply["output"]), nil
}

const preCommandPendingLimit = 32

// Last queued pre command delivery of one session and how many are pending
type preCommandTail struct {
	done    chan struct{}
	pending int
}

var (
	preCommandMu    sync.Mutex
	preCommandTails = map[[2]string]preCommandTail{}
)

// Queues main's pre command observer for a recognized command typed in a local session, nil when not fired
func queuePreCommand(ctx context.Context, a *SessionAuthority, ref SessionRef, canonical, alias, arg string) (chan struct{}, error) {
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, err
	}
	if sessionEscapeHatches[canonical] {
		handle, err := a.handle(ctx, ref)
		if err != nil {
			return nil, err
		}
		if handle.ExecutionState != "idle" {
			return nil, nil
		}
	}
	if local, err := authorizeLocalSource(a.Runner, live.Source); err != nil {
		return nil, err
	} else if local == nil || !*local {
		return nil, nil
	}
	key := [2]string{a.ProfileID, ref.SessionID}
	preCommandMu.Lock()
	tail := preCommandTails[key]
	if tail.pending >= preCommandPendingLimit {
		preCommandMu.Unlock()
		slog.Warn(fmt.Sprintf("pre_command for /%s dropped: %d deliveries already queued for this session", canonical, tail.pending))
		return nil, nil
	}
	done := make(chan struct{})
	preCommandTails[key] = preCommandTail{done: done, pending: tail.pending + 1}
	preCommandMu.Unlock()
	previous := tail.done
	base := context.WithoutCancel(ctx)
	sessionKey, platform := ref.SessionID, "cli"
	fire := func() {
		scoped := profileRuntimeScope(base, a.ProfileID, true)
		env := &tools.Env{}
		if current := tools.ToolEnvFrom(scoped); current != nil {
			env = tools.CopySessionContext(current)
		}
		restore := tools.ScopedSessionKey(env, live.Route)
		defer restore()
		plugins.FirePreCommandHook(tools.WithToolEnv(scoped, env), "cli", canonical, alias, arg, &sessionKey, &platform)
	}
	go func() {
		defer func() {
			preCommandMu.Lock()
			current := preCommandTails[key]
			if current.done == done {
				delete(preCommandTails, key)
			} else if current.pending > 0 {
				preCommandTails[key] = preCommandTail{done: current.done, pending: current.pending - 1}
			}
			preCommandMu.Unlock()
			close(done)
		}()
		if previous != nil {
			<-previous
		}
		ran := make(chan struct{})
		go func() {
			defer close(ran)
			defer func() {
				if r := recover(); r != nil {
					slog.Error(fmt.Sprintf("pre_command delivery failed: %v", r))
				}
			}()
			fire()
		}()
		<-ran
	}()
	return done, nil
}

// Pre command fired and awaited before the gateway runs the command itself
func preCommandBefore(ctx context.Context, a *SessionAuthority, ref SessionRef, canonical, alias, arg string) error {
	done, err := queuePreCommand(ctx, a, ref, canonical, alias, arg)
	if err != nil || done == nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Command observe verb, pre command queued after the reply for a command the client runs itself
func observeCommand(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	a := c.authority
	if err := a.Authorize(ctx, c.actor, ref, "session:read"); err != nil {
		return nil, err
	}
	name, arg, err := parseCommandParams(params, true)
	if err != nil {
		return nil, err
	}
	if profile, _ := params.GetOr("profile", "").(string); profile != "" && !profiles.ProfileMatchesHome(ctx, profile, a.ProfileID) {
		return nil, runtimeRefusal("profile_mismatch")
	}
	definition, known := ResolveRegistryCommand(name)
	if !known {
		return nil, runtimeRefusal("unsupported_command")
	}
	done, err := queuePreCommand(ctx, a, ref, definition.Name, pystr.Lower(name), arg)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("queued", done != nil), nil
}

// Plugin choice asker bound to the clarify request on the session's attached viewers
func viewerChoices(ctx context.Context, live *LiveSession, sessionID string) context.Context {
	return plugins.BindAsker(ctx, func(question string, labels []string) (string, bool) {
		var timeout *float64
		if seconds := tools.GetClarifyTimeout(tools.ToolEnvFrom(ctx)); seconds > 0 {
			value := float64(seconds)
			timeout = &value
		}
		choices := make([]any, len(labels))
		for i, label := range labels {
			choices[i] = label
		}
		question0 := pyjson.ObjectOf("qid", "q0", "question", question, "choices", choices, "multi_select", false)
		result := AskViewerResult(live.EventStream, sessionID, "clarify", pyjson.ObjectOf("questions", []any{question0}), nil, timeout, []string{"q0"})
		if result == nil {
			return "", false
		}
		answers, _ := result.GetOr("answers", nil).(*pyjson.Object)
		if answers == nil {
			return "", false
		}
		answer, isStr := answers.GetOr("q0", nil).(string)
		return answer, isStr
	})
}

// Handler reply as text, an object's text field preferred
func commandText(output any) string {
	switch v := output.(type) {
	case nil:
		return ""
	case string:
		return v
	case *string:
		if v == nil {
			return ""
		}
		return *v
	case *EphemeralReply:
		if v == nil {
			return ""
		}
		return v.Text()
	case interface{ Text() string }:
		return v.Text()
	}
	if !pyrepr.Truthy(output) {
		return ""
	}
	return pyrepr.Str(output)
}

// Joins the non empty parts with newlines
func joinNonEmpty(parts ...string) string {
	var kept []string
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "\n")
}

// Slash exec or command dispatch run by the gateway for a local session, a directive the client submits for a turn
func executeCommand(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object, dispatch bool) (*pyjson.Object, error) {
	a, actor := c.authority, c.actor
	if err := a.Authorize(ctx, actor, ref, "session:read"); err != nil {
		return nil, err
	}
	name, arg, err := parseCommandParams(params, dispatch)
	if err != nil {
		return nil, err
	}
	if profile, _ := params.GetOr("profile", "").(string); profile != "" && !profiles.ProfileMatchesHome(ctx, profile, a.ProfileID) {
		return nil, runtimeRefusal("profile_mismatch")
	}
	scoped := profileRuntimeScope(ctx, a.ProfileID, true)
	if name, arg, err = resolveCommandAlias(scoped, name, arg); err != nil {
		return nil, err
	}
	definition, known := ResolveRegistryCommand(name)
	canonical, verdict := name, ""
	if known {
		canonical = definition.Name
		if verdict, err = CommandVerdict(canonical, arg); err != nil {
			return nil, err
		}
	}
	fired := false
	before := func(ctx context.Context) error {
		if fired {
			return nil
		}
		fired = true
		return preCommandBefore(ctx, a, ref, canonical, pystr.Lower(name), arg)
	}
	if known {
		tui, err := tuiReadCommand(ctx, a, actor, ref, canonical, arg, before)
		if err != nil || tui != nil {
			return tui, err
		}
	}
	runnerKind, runnerShell, hasRunnerCommand := "", "", false
	if !known {
		if runnerKind, runnerShell, hasRunnerCommand, err = runnerCommand(scoped, name); err != nil {
			return nil, err
		}
	}
	capability := "session:submit"
	switch {
	case verdict == "read":
		capability = "session:read"
	case sessionApprovalCommands[canonical]:
		capability = "session:approve"
	case known || hasRunnerCommand:
		capability = "session:control"
	}
	if err := a.Authorize(ctx, actor, ref, capability); err != nil {
		return nil, err
	}
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	_, subscribed := c.subscriptions[ref.SessionID]
	c.mu.Unlock()
	if verdict == "refused" || (verdict == "control" && sessionOwnerCommands[canonical] && subscribed) {
		var detail *string
		if hint, ok := RefusalHints[canonical]; ok {
			detail = &hint
		}
		return nil, state.NewRuntimeStoreError("unsupported_command", nil, detail)
	}
	if isHistoryMount(live) {
		return nil, readOnly()
	}
	if local, err := authorizeLocalSource(a.Runner, live.Source); err != nil {
		return nil, err
	} else if local == nil || !*local {
		return nil, runtimeRefusal("permission_denied")
	}
	if capability != "session:read" {
		if err := a.requireAdmissionOpen(false); err != nil {
			return nil, err
		}
	}
	if known {
		handle, err := a.handle(ctx, ref)
		if err != nil {
			return nil, err
		}
		if sessionInfoReads[canonical] || sessionOwnerCommands[canonical] || handle.ExecutionState == "idle" {
			if err := before(ctx); err != nil {
				return nil, err
			}
		}
	}
	workspace := func() (context.Context, error) { return sessionWorkspace(scoped, a.Runner, live.Source) }
	if hasRunnerCommand {
		runner, ok := a.Runner.(localCommandRunner)
		if !ok {
			return nil, attributeError("_check_slash_access")
		}
		inWorkspace, err := workspace()
		if err != nil {
			return nil, err
		}
		output, err := runRunnerCommand(viewerChoices(inWorkspace, live, ref.SessionID), a, runner, live, name, arg, runnerKind, runnerShell)
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("type", "exec", "output", commandText(output)), nil
	}
	if !known {
		inWorkspace, err := workspace()
		if err != nil {
			return nil, err
		}
		return skillDirective(inWorkspace, name, arg, live.Route)
	}
	if sessionInfoReads[canonical] {
		reply, err := runInfoRead(ctx, a, live, canonical)
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("type", reply["type"], "output", reply["output"]), nil
	}
	if sessionOwnerCommands[canonical] {
		reply, err := runCron(ctx, a, arg)
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("type", reply["type"], "output", reply["output"]), nil
	}
	runner, ok := a.Runner.(localCommandRunner)
	if !ok {
		return nil, attributeError("_command_handler_table")
	}
	typed := "/" + canonical
	if arg != "" {
		typed += " " + arg
	}
	event := NewMessageEvent(typed)
	event.Source = live.Source
	inWorkspace, err := workspace()
	if err != nil {
		return nil, err
	}
	captured, sink := CaptureCommandSink(inWorkspace)
	captured = WithBusyAccepted(captured, before)
	var output any
	var prompt *string
	handle, err := a.handle(ctx, ref)
	if err != nil {
		return nil, err
	}
	switch {
	case handle.ExecutionState != "idle":
		if output, err = runner.LocalBusySlashCommand(captured, event, definition, live.Route, live.Source); err != nil {
			return nil, err
		}
	case sessionPromptCommands[canonical]:
		if err := before(captured); err != nil {
			return nil, err
		}
		handled, reply, err := runner.LocalPromptCommand(captured, canonical, event, live.Source, live.Route)
		if err != nil {
			return nil, err
		}
		output = reply
		if !handled {
			text := event.Text
			prompt = &text
		}
	default:
		if err := before(captured); err != nil {
			return nil, err
		}
		if canonical == "reasoning" {
			value, persistGlobal := runner.ParseReasoningCommandArgs(arg)
			if err := commitReasoningCommand(captured, a, actor, ref, value, persistGlobal); err != nil {
				return nil, err
			}
		}
		handler := runner.LocalCommandHandler(canonical)
		if handler == nil {
			return nil, attributeError("_handle_" + strings.ReplaceAll(canonical, "-", "_") + "_command")
		}
		if output, err = handler(captured, event); err != nil {
			return nil, err
		}
	}
	if sessionApprovalCommands[canonical] {
		settled, err := a.handle(ctx, ref)
		if err != nil {
			return nil, err
		}
		var generation *int64
		if settled.ExecutionState == "running" {
			generation = &settled.ExecutionGeneration
		}
		live.controls.snapshot(ref.SessionID, generation)
	}
	if sessionAutomationCommands[canonical] {
		if err := PublishControl(ctx, a, ref.SessionID, false, false); err != nil {
			return nil, err
		}
	}
	if prompt == nil {
		prompt = sink.Prompt
	}
	if prompt != nil && *prompt != "" {
		notice := joinNonEmpty(append(append([]string{}, sink.Notices...), pyrepr.Str(pyrepr.Or(output, "")))...)
		display := typed
		if sink.Display != nil && *sink.Display != "" {
			display = *sink.Display
		}
		out := pyjson.ObjectOf("type", "send", "message", *prompt, "display", display)
		if notice != "" {
			out.Set("notice", notice)
		}
		return out, nil
	}
	return pyjson.ObjectOf("type", "exec", "output", joinNonEmpty(append(append([]string{}, sink.Notices...), commandText(output))...)), nil
}

// Slash exec verb
func (c *AuthorityConnection) slashExec(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return executeCommand(ctx, c, ref, params, false)
}

// Command dispatch verb
func (c *AuthorityConnection) commandDispatch(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return executeCommand(ctx, c, ref, params, true)
}

// Command observe verb
func (c *AuthorityConnection) commandObserve(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return observeCommand(ctx, c, ref, params)
}

// Submission parser of the queued row verbs
func (c *AuthorityConnection) Submission(ctx context.Context, ref SessionRef, params *pyjson.Object) (Submission, error) {
	return c.submission(ctx, ref, params)
}
