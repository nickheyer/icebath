package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"

	"github.com/nickheyer/icebath/pkg/internal/textutil"
)

// Registry command a typed name resolves to, with the busy policy and mid run variant key of its definition
type RegistryCommand struct {
	Name, BusyPolicy, BusyHandler string
}

// Command registry lookup of pkg/commands, assigned in cmd/icebath/wiring.go
var ResolveRegistryCommand func(name string) (RegistryCommand, bool)

// Registry command a plugin bound a turn building handler to, assigned in cmd/icebath/wiring.go
var ResolveBoundCommand func(name string) (RegistryCommand, bool)

// Runs the handler bound to a registry command against the host taking its turn, assigned in cmd/icebath/wiring.go
var RunBoundCommand func(ctx context.Context, name, arg string, host any) error

// Turn a bound command handler builds for one session, held for the surface to submit
type BoundTurn struct {
	TaskID, Platform, Message string
	submitted                 bool
}

// Holds the one turn the handler built, refusing a second
func (t *BoundTurn) SubmitUserTurn(ctx context.Context, message string) error {
	if t.submitted {
		return errors.New("a slash command submits one turn")
	}
	t.Message, t.submitted = message, true
	return nil
}

// Task id and platform of the session the turn is built for
func (t *BoundTurn) TurnSession() (string, string) { return t.TaskID, t.Platform }

// Turn of a bound command for one session, an empty turn refused as unavailable
func RunBoundTurn(ctx context.Context, name, arg, taskID, platform string) (string, error) {
	turn := &BoundTurn{TaskID: taskID, Platform: platform}
	if err := RunBoundCommand(ctx, name, arg, turn); err != nil {
		return "", err
	}
	if turn.Message == "" {
		return "", runtimeRefusal("command_unavailable")
	}
	return turn.Message, nil
}

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

// What to do instead, in plain words, for commands the background service does not run
var RefusalHints = map[string]string{
	"export":   "Type /export [profile] [-o <file>] in the terminal UI, or export a profile from the Profiles screen (/profiles).",
	"import":   "Type /import <backup.zip> [--force] in the terminal UI to restore a backup; a profile archive is imported from the Profiles screen (/profiles).",
	"snapshot": "Type /snapshot in the terminal UI: list, create [label], restore <id>, prune [N].",
	"handoff":  "Type /handoff <platform> in the terminal UI on the session to hand off.",
	"hatch":    "Type /hatch [description] in the terminal UI, or /pet to pick a gallery pet.",
	"worktree": "Type /worktree [new [name]|list|prune] in the terminal UI.",
	"palette":  "Type / to search commands (the composer completes and filters them).",
	"start":    chatAppOnlyHint,
	"topic":    chatAppOnlyHint,
	"sethome":  chatAppOnlyHint,
	"verbose":  "This command only works in a chat app conversation. In the terminal UI, /detail sets how much of the agent's work you see.",
	"voice":    "The terminal UI speaks replies itself: type /voice tts there. Spoken replies from the background service are for chat apps.",
	"plugins":  "Plugins are installed, updated, removed and turned on or off on the Plugins screen of the terminal UI (/plugins).",
	"skills":   "Skills are searched, installed, checked, updated and removed on the Skills screen of the terminal UI (/skills).",
	"update":   "Type /update in the terminal UI: it updates once the UI exits, then starts it again.",
}

// Kanban verbs that keep running until stopped, which a slash reply cannot carry
var kanbanForegroundVerbs = nameSet("watch", "tail", "daemon")

// Hint of a kanban verb that keeps running until stopped
const kanbanForegroundHint = "/kanban watch, tail and daemon keep running until stopped, so they cannot answer here. The Kanban screen of the terminal UI (/kanban) follows the board live."

// Arguments of /skills the background service runs, the review of pending skill writes
var skillsReviewWords = nameSet("", "pending", "approve", "apply", "reject", "deny", "drop", "diff", "approval", "mode")

// Hint of a command that only means something in a chat app conversation
const chatAppOnlyHint = "This command only works in a chat app conversation (Telegram, Discord, Slack and the like). Connect a chat app from /setup in the terminal UI."

// Hint of a registry command the terminal UI sent that nothing in this build runs, a fault of the build rather than of what was typed
const clientOnlyHint = "Nothing in this build of icebath runs this command."

// Hint of a command only the terminal UI on this computer may run through the background service
const ownerOnlyHint = "Only the terminal UI on this computer can run this command."

// Service controls, plugin toggles and listings only the local owner may run
var ownerControlCommands = nameSet("pause", "restart", "platform", "plugins", "agents")

// Read commands whose writing arguments only the local owner may run
var ownerArgumentCommands = nameSet("kanban", "usage", "suggestions", "memory", "skills")

// Whether the connection is the local owner, a same-user interactive client on the loopback socket
func (c *AuthorityConnection) localOwner() bool {
	return c.nativeOwner && c.actor.Capabilities["session:operator"]
}

// Argument split as a shlex splitting handler parses it, an unclosed quote refused on the arg field
func splitCommandArg(arg string) ([]string, error) {
	if strings.TrimSpace(arg) == "" {
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
			return strings.ToLower(token), nil
		}
	}
	return "", nil
}

// First whitespace separated word lowercased
func commandFirstWord(arg string) string {
	words := textutil.FieldsN(arg, 1)
	if len(words) == 0 {
		return ""
	}
	return strings.ToLower(words[0])
}

// Whether the argument form of a read command only reads
func readArgument(name, arg string) (bool, error) {
	switch name {
	case "skills":
		word := commandFirstWord(arg)
		return word == "" || word == "pending" || word == "diff", nil
	case "usage", "suggestions":
		return strings.TrimSpace(arg) == "", nil
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
	if name == "voice" && strings.ToLower(strings.TrimSpace(arg)) == "tts" {
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

// Verdict of a registry command typed by the local owner, CommandVerdict plus the owner-only commands
func ownerCommandVerdict(name, arg string) (string, error) {
	verdict, err := CommandVerdict(name, arg)
	if err != nil || verdict != "refused" {
		return verdict, err
	}
	if ownerControlCommands[name] || ownerArgumentCommands[name] {
		return "control", nil
	}
	return "refused", nil
}

// Unsupported command refusal carrying what to do instead, the command's own hint or the fallback
func unsupportedCommand(canonical, fallback string) error {
	hint := RefusalHints[canonical]
	if hint == "" {
		hint = fallback
	}
	var detail *string
	if hint != "" {
		detail = &hint
	}
	return state.NewRuntimeStoreError("unsupported_command", nil, detail)
}

// Refusal of a command the owner unlocked that the background service still has no handler for, nil when it runs
func ownerUnrunnable(a *SessionAuthority, canonical, arg string) error {
	switch {
	case canonical == "plugins":
		if _, _, ok := pluginsToggleArgs(arg); ok {
			return nil
		}
		return unsupportedCommand(canonical, "")
	case canonical == "skills":
		if skillsReviewWords[commandFirstWord(arg)] {
			return nil
		}
		return unsupportedCommand(canonical, "")
	case canonical == "kanban":
		token, err := kanbanFirstToken(arg)
		if err != nil || !kanbanForegroundVerbs[token] {
			return err
		}
		return unsupportedCommand(canonical, kanbanForegroundHint)
	case sessionPromptCommands[canonical]:
		return nil
	}
	runner, ok := a.Runner.(localCommandRunner)
	if !ok || runner.LocalCommandHandler(canonical) != nil {
		return nil
	}
	return unsupportedCommand(canonical, clientOnlyHint)
}

// Whether a plugins argument is enable or disable of one named plugin, with that choice and name
func pluginsToggleArgs(arg string) (bool, string, bool) {
	words := strings.Fields(arg)
	if len(words) != 2 {
		return false, "", false
	}
	switch strings.ToLower(words[0]) {
	case "enable":
		return true, words[1], true
	case "disable":
		return false, words[1], true
	}
	return false, "", false
}

// Enables or disables one plugin for the local owner, answered in plain words
func ownerPluginsCommand(ctx context.Context, arg string) (string, error) {
	enable, name, _ := pluginsToggleArgs(arg)
	result, err := plugins.SetAgentPluginEnabled(ctx, name, enable)
	if err != nil {
		return "", err
	}
	if !pyrepr.Truthy(result["ok"]) {
		return fmt.Sprintf("Plugin %s was not changed: %s", name, pyrepr.Str(result["error"])), nil
	}
	if pyrepr.Truthy(result["name"]) {
		name = pyrepr.Str(result["name"])
	}
	done := "disabled"
	if enable {
		done = "enabled"
	}
	if pyrepr.Truthy(result["unchanged"]) {
		return fmt.Sprintf("Plugin %s is already %s.", name, done), nil
	}
	lines := []string{fmt.Sprintf("Plugin %s %s.", name, done)}
	if activation, _ := result["activation"].(map[string]any); activation != nil {
		if missing, _ := activation["missing_dependencies"].([]any); len(missing) > 0 {
			names := make([]string, len(missing))
			for i, item := range missing {
				names[i] = pyrepr.Str(item)
			}
			lines = append(lines, "It needs packages that are not installed: "+strings.Join(names, ", ")+".")
		}
	}
	if pyrepr.Truthy(result["restart_required"]) {
		lines = append(lines, "Type /restart to restart the background service so every session picks up the change.")
	}
	return strings.Join(lines, "\n"), nil
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
	parts := textutil.FieldsN(strings.TrimLeft(strings.TrimSpace(text), "/"), 1)
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
					return "", "", fmt.Errorf("quick_commands in config is a list, expected a mapping of command name to its definition")
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
		if !isStr || strings.TrimSpace(target) == "" {
			return "", "", runtimeRefusal("invalid_params")
		}
		parts := textutil.FieldsN(strings.TrimLeft(strings.TrimSpace(target), "/"), 1)
		if len(parts) == 0 {
			return "", "", &PyError{Kind: "IndexError", Msg: fmt.Sprintf("quick command %s is an alias with an empty target", name)}
		}
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
		return "", "", false, fmt.Errorf("quick_commands in config is %s, expected a mapping of command name to its definition", errchain.Kind(quick))
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
		pkgLog().Warn(fmt.Sprintf("Plugin command dispatch failed: %s", err))
		message := err.Error()
		if message == "" {
			message = fmt.Sprintf("%T", err)
		}
		return i18n.T(ctx, "cli.plugin.command_error", "", map[string]any{"error": message}), nil
	}
	return output, nil
}

// Skill directive of a local session's skill slash command, stacked skills included
func skillDirective(ctx context.Context, name, arg, route string) (*tuigateway.CommandDispatchResult, error) {
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
	return skillTurnDirective(ctx, shown, message, missing), nil
}

// Skill directive the client submits as its next turn, noting any skills that were missing
func skillTurnDirective(ctx context.Context, name, message string, missing []string) *tuigateway.CommandDispatchResult {
	out := &tuigateway.CommandDispatchResult{Type: "skill", Name: name, Message: message}
	if described, ok := skills.DescribeSkillInvocation(message, " "); ok {
		out.Display = described
	}
	if len(missing) > 0 {
		out.Notice = i18n.T(ctx, "cli.skills.skipped_missing", "", map[string]any{"names": strings.Join(missing, ", ")})
	}
	return out
}

// Exec directive of a gateway read's reply
func readDirective(reply map[string]any) *tuigateway.CommandDispatchResult {
	typ, _ := reply["type"].(string)
	output, _ := reply["output"].(string)
	return &tuigateway.CommandDispatchResult{Type: tuigateway.DispatchType(typ), Output: output}
}

// Bound command reply of a local session, its built turn as a skill directive when idle, its busy policy otherwise
func boundCommandReply(ctx context.Context, a *SessionAuthority, ref SessionRef, live *LiveSession, bound RegistryCommand, arg string, before func(context.Context) error) (*tuigateway.CommandDispatchResult, error) {
	handle, err := a.handle(ctx, ref)
	if err != nil {
		return nil, err
	}
	if handle.ExecutionState != "idle" {
		runner, ok := a.Runner.(localCommandRunner)
		if !ok {
			return nil, runnerLacks("run a slash command while a turn is busy")
		}
		event := NewMessageEvent(strings.TrimSpace("/" + bound.Name + " " + arg))
		event.Source = live.Source
		captured, sink := CaptureCommandSink(ctx)
		output, err := runner.LocalBusySlashCommand(WithBusyAccepted(captured, before), event, bound, live.Route, live.Source)
		if err != nil {
			return nil, err
		}
		return &tuigateway.CommandDispatchResult{Type: "exec", Output: joinNonEmpty(append(append([]string{}, sink.Notices...), commandText(output))...)}, nil
	}
	if err := before(ctx); err != nil {
		return nil, err
	}
	message, err := RunBoundTurn(ctx, bound.Name, arg, live.Route, "")
	if err != nil {
		return nil, err
	}
	return skillTurnDirective(ctx, bound.Name, message, nil), nil
}

// Main's TUI text for a TUI session's status or context read, nil for any other read
func tuiReadCommand(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, name, arg string, before func(context.Context) error) (*tuigateway.CommandDispatchResult, error) {
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
	return readDirective(reply), nil
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
		pkgLog().Warn(fmt.Sprintf("pre_command for /%s dropped: %d deliveries already queued for this session", canonical, tail.pending))
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
		var current *tools.Env
		if bound := tools.ToolEnvFrom(scoped); bound != nil {
			current = tools.CopySessionContext(bound)
		}
		env := tools.EnvForProfileHome(scoped, current, home.Dir(scoped))
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
					pkgLog().Error(fmt.Sprintf("pre_command delivery failed: %v", r))
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
	done, err := queuePreCommand(ctx, a, ref, definition.Name, strings.ToLower(name), arg)
	if err != nil {
		return nil, err
	}
	return tuigateway.Plain(tuigateway.CommandObserveResult{Queued: done != nil}), nil
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
func executeCommand(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object, dispatch bool) (*tuigateway.CommandDispatchResult, error) {
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
	owner, ownerUnlocked := c.localOwner(), false
	if known {
		canonical = definition.Name
		if verdict, err = CommandVerdict(canonical, arg); err != nil {
			return nil, err
		}
		if owner && verdict == "refused" {
			if verdict, err = ownerCommandVerdict(canonical, arg); err != nil {
				return nil, err
			}
			ownerUnlocked = verdict != "refused"
		}
	}
	fired := false
	before := func(ctx context.Context) error {
		if fired {
			return nil
		}
		fired = true
		return preCommandBefore(ctx, a, ref, canonical, strings.ToLower(name), arg)
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
	bound, isBound := RegistryCommand{}, false
	if !known && !hasRunnerCommand {
		if bound, isBound = ResolveBoundCommand(name); isBound {
			canonical = bound.Name
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
	if verdict == "refused" {
		fallback := ownerOnlyHint
		if owner {
			fallback = clientOnlyHint
		}
		return nil, unsupportedCommand(canonical, fallback)
	}
	if ownerUnlocked {
		if err := ownerUnrunnable(a, canonical, arg); err != nil {
			return nil, err
		}
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
			return nil, runnerLacks("check slash command access")
		}
		inWorkspace, err := workspace()
		if err != nil {
			return nil, err
		}
		output, err := runRunnerCommand(viewerChoices(inWorkspace, live, ref.SessionID), a, runner, live, name, arg, runnerKind, runnerShell)
		if err != nil {
			return nil, err
		}
		return &tuigateway.CommandDispatchResult{Type: "exec", Output: commandText(output)}, nil
	}
	if isBound {
		inWorkspace, err := workspace()
		if err != nil {
			return nil, err
		}
		return boundCommandReply(inWorkspace, a, ref, live, bound, arg, before)
	}
	if !known {
		inWorkspace, err := workspace()
		if err != nil {
			return nil, err
		}
		return skillDirective(inWorkspace, name, arg, live.Route)
	}
	if ownerUnlocked && canonical == "plugins" {
		output, err := ownerPluginsCommand(scoped, arg)
		if err != nil {
			return nil, err
		}
		return &tuigateway.CommandDispatchResult{Type: "exec", Output: output}, nil
	}
	if sessionInfoReads[canonical] {
		reply, err := runInfoRead(ctx, a, live, canonical)
		if err != nil {
			return nil, err
		}
		return readDirective(reply), nil
	}
	if sessionOwnerCommands[canonical] {
		reply, err := runCron(ctx, a, arg)
		if err != nil {
			return nil, err
		}
		return readDirective(reply), nil
	}
	runner, ok := a.Runner.(localCommandRunner)
	if !ok {
		return nil, runnerLacks("list its command handlers")
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
			return nil, runnerLacks("handle the /" + canonical + " command")
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
		return &tuigateway.CommandDispatchResult{Type: "send", Message: *prompt, Display: display, Notice: notice}, nil
	}
	return &tuigateway.CommandDispatchResult{Type: "exec", Output: joinNonEmpty(append(append([]string{}, sink.Notices...), commandText(output))...)}, nil
}

// Slash exec verb
func (c *AuthorityConnection) slashExec(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	directive, err := executeCommand(ctx, c, ref, params, false)
	if err != nil || directive == nil {
		return nil, err
	}
	return tuigateway.Plain(directive.SlashResult()), nil
}

// Command dispatch verb
func (c *AuthorityConnection) commandDispatch(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	directive, err := executeCommand(ctx, c, ref, params, true)
	if err != nil || directive == nil {
		return nil, err
	}
	return tuigateway.Plain(directive), nil
}

// Command observe verb
func (c *AuthorityConnection) commandObserve(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return observeCommand(ctx, c, ref, params)
}

// Submission parser of the queued row verbs
func (c *AuthorityConnection) Submission(ctx context.Context, ref SessionRef, params *pyjson.Object) (Submission, error) {
	return c.submission(ctx, ref, params)
}
