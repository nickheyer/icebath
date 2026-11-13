package acp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Refusal the editor shows as the command's reply while the prompt itself ends normally
type CommandRefused struct{ Msg string }

func (e *CommandRefused) Error() string { return e.Msg }

// Lets a refusal match a gateway client error target like the Python subclass
func (e *CommandRefused) As(target any) bool {
	if t, ok := target.(**gateway.GatewayClientError); ok {
		*t = &gateway.GatewayClientError{Msg: e.Msg}
		return true
	}
	return false
}

// Handler of one command ACP answers itself, the reply or the text to submit as the prompt
type gatewayRouteHandler func(ctx context.Context, a *GatewayACPAgent, sessionID, command, arg string) (string, *string, error)

// Parity table cell and handler of one command ACP answers itself
type gatewayRoute struct {
	Cell    string
	Handler gatewayRouteHandler
}

// Reply of a command answered here, or the text to submit as the prompt when submit is set
func runGatewayCommand(ctx context.Context, a *GatewayACPAgent, sessionID, command, arg string) (string, *string, error) {
	route, ok := gatewayRoutes[command]
	if !ok {
		link, err := a.client(ctx)
		if err != nil {
			return "", nil, err
		}
		return gatewayForward(ctx, a, link, sessionID, command, arg)
	}
	return route.Handler(ctx, a, sessionID, command, arg)
}

func gatewayBranch(ctx context.Context, a *GatewayACPAgent, sessionID, command, arg string) (string, *string, error) {
	return "", nil, &CommandRefused{Msg: "/branch: use your editor's fork session action (ACP session/fork)"}
}

func gatewayHelp(ctx context.Context, a *GatewayACPAgent, sessionID, command, arg string) (string, *string, error) {
	return gatewayHelpText(), nil, nil
}

func gatewayPromptCommand(ctx context.Context, a *GatewayACPAgent, sessionID, command, arg string) (string, *string, error) {
	if arg == "" {
		what := "prompt"
		if command == "steer" {
			what = "guidance"
		}
		return fmt.Sprintf("Usage: /%s <%s>", command, what), nil, nil
	}
	if command == "steer" {
		steered, err := gatewaySteer(ctx, a, sessionID, arg)
		if err != nil {
			return "", nil, err
		}
		if steered != "" {
			return steered, nil, nil
		}
	}
	return "", &arg, nil
}

func gatewayTitleCommand(ctx context.Context, a *GatewayACPAgent, sessionID, command, arg string) (string, *string, error) {
	link, err := a.client(ctx)
	if err != nil {
		return "", nil, err
	}
	if arg == "" {
		return gatewayForward(ctx, a, link, sessionID, command, arg)
	}
	reply, err := gatewayRename(ctx, a, link, sessionID, arg)
	return reply, nil, err
}

func gatewayInfo(ctx context.Context, a *GatewayACPAgent, sessionID, command, arg string) (string, *string, error) {
	link, err := a.client(ctx)
	if err != nil {
		return "", nil, err
	}
	if command == "model" && arg != "" {
		reply, err := gatewayMutate(ctx, a, link, sessionID, command, arg)
		return reply, nil, err
	}
	info, err := a.rpc(ctx, link, "session.info", map[string]any{"session_id": sessionID})
	if err != nil {
		return "", nil, err
	}
	if command == "tools" {
		return toolsText(info), nil, nil
	}
	plain, _ := pyjson.Plain(info).(map[string]any)
	return modelText("Current model", plain), nil, nil
}

func gatewayMutation(ctx context.Context, a *GatewayACPAgent, sessionID, command, arg string) (string, *string, error) {
	link, err := a.client(ctx)
	if err != nil {
		return "", nil, err
	}
	reply, err := gatewayMutate(ctx, a, link, sessionID, command, arg)
	return reply, nil, err
}

func gatewayRewindCommand(ctx context.Context, a *GatewayACPAgent, sessionID, command, arg string) (string, *string, error) {
	link, err := a.client(ctx)
	if err != nil {
		return "", nil, err
	}
	return gatewayRewind(ctx, a, link, sessionID, command, arg)
}

// Acknowledges one exact lost turn and refreshes its FIFO snapshot
func gatewayDiscardUnknown(ctx context.Context, a *GatewayACPAgent, sessionID, command, admissionID string) (string, *string, error) {
	if admissionID == "" {
		return "Usage: /discard <lost admission id>", nil, nil
	}
	if err := a.refreshUnknown(ctx, sessionID); err != nil {
		return "", nil, err
	}
	snapshot, _ := a.snapshots.Get(sessionID)
	pending, _ := dget(snapshot, "pending").([]any)
	var lost *pyjson.Object
	for _, raw := range pending {
		if row := asObject(raw); dget(row, "admission_id") == admissionID && dget(row, "status") == "unknown" {
			lost = row
			break
		}
	}
	if lost == nil {
		return fmt.Sprintf("No unknown (lost) admission with id %s; refresh the session and try again.", admissionID), nil, nil
	}
	link, err := a.client(ctx)
	if err != nil {
		return "", nil, err
	}
	if !dhas(lost, "execution_generation") {
		return "", nil, errors.New("KeyError: 'execution_generation'")
	}
	if _, err := a.rpc(ctx, link, "prompt.resolve_unknown", map[string]any{"session_id": sessionID, "admission_id": admissionID,
		"execution_generation": dget(lost, "execution_generation")}); err != nil {
		return "", nil, err
	}
	if err := a.refreshUnknown(ctx, sessionID); err != nil {
		return "", nil, err
	}
	return "Discarded the lost turn. Queued messages can continue.", nil, nil
}

// Owner's slash.exec answer, its report, a turn to submit after its notice, or the shared refusal
func gatewayForward(ctx context.Context, a *GatewayACPAgent, link gatewayLink, sessionID, command, arg string) (string, *string, error) {
	reply, err := a.rpc(ctx, link, "slash.exec", map[string]any{"session_id": sessionID, "command": pystr.Strip("/" + command + " " + arg)})
	if err != nil {
		var refused *gateway.GatewayRPCError
		if errors.As(err, &refused) && refused.Msg == "unsupported_command" {
			return "", nil, &CommandRefused{Msg: CommandRefusal(command)}
		}
		return "", nil, err
	}
	if kind := dget(reply, "type"); kind == "skill" || kind == "send" {
		if notice := dget(reply, "notice"); truthy(notice) && a.conn != nil {
			if err := a.sessionUpdate(ctx, a.editorID(sessionID), pyacp.UpdateAgentMessageText(pyStr(notice)+"\n")); err != nil {
				return "", nil, err
			}
		}
		if !dhas(reply, "message") {
			return "", nil, errors.New("KeyError: 'message'")
		}
		message := pyStr(dget(reply, "message"))
		return "", &message, nil
	}
	for _, key := range []string{"output", "notice"} {
		if v := dget(reply, key); truthy(v) {
			return pyStr(v), nil, nil
		}
	}
	return fmt.Sprintf("/%s: no output", command), nil, nil
}

func gatewayRename(ctx context.Context, a *GatewayACPAgent, link gatewayLink, sessionID, title string) (string, error) {
	payload := pyjson.ObjectOf("title", title)
	result, err := a.mutations.Apply(ctx, a.mutationRPC(link), sessionID, "rename", payload, nil)
	if err != nil {
		return "", err
	}
	a.mutations.Acknowledge(sessionID, "rename", payload)
	shown := title
	if v := result["title"]; truthy(v) {
		shown = pyStr(v)
	}
	return "Session title set: " + shown, nil
}

// Rewinds for /undo and reports the removed message, or for /retry and resubmits the last user turn
func gatewayRewind(ctx context.Context, a *GatewayACPAgent, link gatewayLink, sessionID, command, arg string) (string, *string, error) {
	retry := command == "retry"
	turns := int64(1)
	if !retry && arg != "" {
		if !pystr.StrIsDigit(arg) {
			return "", nil, &CommandRefused{Msg: "Usage: /undo [N]"}
		}
		n, err := pyint.Of(arg)
		if err != nil {
			return "", nil, err
		}
		if n.Sign() <= 0 {
			return "", nil, &CommandRefused{Msg: "Usage: /undo [N]"}
		}
		// A count past int64 exceeds every transcript, so the largest int64 finds the same nothing
		turns = math.MaxInt64
		if n.IsInt64() {
			turns = n.Int64()
		}
	}
	target := RewindTo(turns, retry)
	var payload MutationDerive = func(snapshot map[string]any) (map[string]any, error) {
		derived, err := target(snapshot)
		var refused *gateway.GatewayClientError
		if err != nil && errors.As(err, &refused) {
			return nil, &CommandRefused{Msg: refused.Msg}
		}
		return derived, err
	}
	result, err := a.mutations.Apply(ctx, a.mutationRPC(link), sessionID, "rewind", payload, nil)
	if err != nil {
		return "", nil, err
	}
	missing := "cli.session.undo_no_user_message"
	if retry {
		missing = "cli.session.retry_no_user_message"
	}
	if result["status"] == "nothing" || !truthy(result["rewound_count"]) {
		a.mutations.Acknowledge(sessionID, "rewind", payload)
		return i18n.T(ctx, missing, "", nil), nil, nil
	}
	snapshot, err := a.rpc(ctx, link, "session.resume", map[string]any{"session_id": sessionID})
	if err != nil {
		return "", nil, err
	}
	if err := a.adopt(ctx, sessionID, snapshot, nil); err != nil {
		return "", nil, err
	}
	a.mutations.Acknowledge(sessionID, "rewind", payload)
	var content any
	if message, ok := result["target_message"].(map[string]any); ok {
		content = message["content"]
	}
	if retry {
		text, err := agent.RetryableUserText(content)
		if err != nil {
			return "", nil, &CommandRefused{Msg: "/retry: " + err.Error()}
		}
		if pystr.Strip(text) == "" {
			return i18n.T(ctx, "cli.session.retry_no_user_message", "", nil), nil, nil
		}
		return "", &text, nil
	}
	removed, _ := content.(string)
	plural := "s"
	if turns == 1 {
		plural = ""
	}
	reply := fmt.Sprintf("Undid %d turn%s (%s message(s)).\n", turns, plural, pyStr(result["rewound_count"]))
	if removed != "" {
		reply += "Removed message: " + removed
	}
	return pystr.Rstrip(reply), nil, nil
}

// Guidance for this editor's running turn, empty when there is none to steer
func gatewaySteer(ctx context.Context, a *GatewayACPAgent, sessionID, text string) (string, error) {
	admissionID, ok := a.admissions[sessionID]
	if !ok {
		return "", nil
	}
	link, err := a.client(ctx)
	if err != nil {
		return "", err
	}
	receipt, err := a.rpc(ctx, link, "prompt.receipt", map[string]any{"session_id": sessionID, "admission_id": admissionID})
	if err != nil {
		return "", err
	}
	if dget(receipt, "status") != "started" {
		return "", nil
	}
	if !dhas(receipt, "execution_generation") {
		return "", errors.New("KeyError: 'execution_generation'")
	}
	result, err := a.rpc(ctx, link, "session.steer", map[string]any{"session_id": sessionID, "text": text,
		"execution_generation": dget(receipt, "execution_generation")})
	if err != nil {
		var refused *gateway.GatewayRPCError
		if errors.As(err, &refused) && refused.Msg == "stale_generation" {
			return "", nil
		}
		return "", err
	}
	if dget(result, "voice_stopped") == true {
		return i18n.T(ctx, "cli.voice.stop_phrase_typed", "", nil), nil
	}
	if dget(result, "status") != "queued" {
		return "", nil
	}
	preview := text
	if r := []rune(text); len(r) > 80 {
		preview = string(r[:80]) + "..."
	}
	return "⏩ Steer queued for the active turn: " + preview, nil
}

// Applies a retained session mutation for a typed command and reports what it changed
func gatewayMutate(ctx context.Context, a *GatewayACPAgent, link gatewayLink, sessionID, command, arg string) (string, error) {
	operation, payload := "reset", pyjson.NewObject()
	if command != "new" && command != "clear" {
		var err error
		if operation, payload, err = SlashMutation("/"+command, arg); err != nil {
			return "", err
		}
	}
	var confirm MutationConfirm
	if operation == "model" {
		confirm = a.confirmer(sessionID)
	}
	result, err := a.mutations.Apply(ctx, a.mutationRPC(link), sessionID, operation, payload, confirm)
	if err != nil {
		return "", err
	}
	var reply string
	status, _ := result["status"].(string)
	if unchanged, ok := unchangedReplies[status]; ok {
		if reply, err = unchanged(ctx, result); err != nil {
			return "", err
		}
	} else {
		snapshot, err := a.rpc(ctx, link, "session.resume", map[string]any{"session_id": sessionID})
		if err != nil {
			return "", err
		}
		if err := a.adopt(ctx, sessionID, snapshot, nil); err != nil {
			return "", err
		}
		if operation == "reset" || operation == "compress" {
			if err := refreshContext(ctx, a, link, sessionID, operation); err != nil {
				return "", err
			}
		}
		switch operation {
		case "model":
			reply = modelText("Model switched to", result)
		case "compress":
			if reply = summaryText(result); reply == "" {
				reply = "Context compressed."
			}
		case "reset":
			reply = "Conversation history cleared."
		default:
			return "", fmt.Errorf("KeyError: '%s'", operation)
		}
	}
	a.mutations.Acknowledge(sessionID, operation, payload)
	return reply, nil
}

func cancelledText(ctx context.Context, result map[string]any) (string, error) {
	return i18n.T(ctx, "cli.model.switch_cancelled", "", nil), nil
}

// Replies of a mutation answer that committed nothing, keyed by its status
var unchangedReplies = map[string]func(ctx context.Context, result map[string]any) (string, error){
	"cancelled": cancelledText,
	"deferred": func(ctx context.Context, result map[string]any) (string, error) {
		return deferredPickText(result), nil
	},
	"preview": func(ctx context.Context, result map[string]any) (string, error) {
		raw, ok := result["lines"]
		if !ok {
			return "", errors.New("KeyError: 'lines'")
		}
		items, _ := raw.([]any)
		lines := make([]string, len(items))
		for i, line := range items {
			lines[i] = pyStr(line)
		}
		return strings.Join(lines, "\n"), nil
	},
	"noop": func(ctx context.Context, result map[string]any) (string, error) {
		if text := summaryText(result); text != "" {
			return text, nil
		}
		return "No changes from compression.", nil
	},
}

// Shows the context indicator after a reset or a compress, the window kept and its occupancy recomputed
func refreshContext(ctx context.Context, a *GatewayACPAgent, link gatewayLink, sessionID, operation string) error {
	stats, err := a.rpc(ctx, link, "session.stats", map[string]any{"session_id": sessionID})
	if err != nil {
		return err
	}
	if dget(stats, "context_length") == nil {
		snapshot, _ := a.snapshots.Get(sessionID)
		raw, _ := dget(snapshot, "messages").([]any)
		var messages []map[string]any
		for _, item := range raw {
			if obj := asObject(item); obj != nil && !truthy(dget(obj, "archived")) {
				plain, _ := pyjson.Plain(obj).(map[string]any)
				messages = append(messages, plain)
			}
		}
		merged := pyjson.NewObject()
		for _, key := range stats.Keys {
			merged.Set(key, stats.Vals[key])
		}
		merged.Set("context_length", dget(a.usage[sessionID], "context_length"))
		tokens := 0
		if operation == "compress" {
			tokens = estimateRequestTokensRough(ctx, messages, "", nil)
		}
		merged.Set("context_tokens", int64(tokens))
		stats = merged
	}
	return a.projectUsage(ctx, sessionID, a.editorID(sessionID), stats)
}

// Reply for a pick stored for the next turn
func deferredPickText(result map[string]any) string {
	model := "The picked model"
	if v := result["model"]; truthy(v) {
		model = pyStr(v)
	}
	return model + " applies from your next message."
}

// Reply for a deferred pick the owner dropped at the turn boundary
func refusedPickText(update map[string]any) string {
	current := "the current model"
	if v := update["model"]; truthy(v) {
		current = pyStr(v)
	}
	if v := update["provider"]; truthy(v) {
		current += fmt.Sprintf(" (%s)", pyStr(v))
	}
	target, reason := "the picked model", "refused"
	if v := update["target_model"]; truthy(v) {
		target = pyStr(v)
	}
	if v := update["reason"]; truthy(v) {
		reason = pyStr(v)
	}
	return fmt.Sprintf("Model switch to %s was not applied (%s); still using %s.", target, reason, current)
}

func summaryText(result map[string]any) string {
	summary, _ := result["summary"].(map[string]any)
	var lines []string
	for _, key := range []string{"headline", "token_line", "note"} {
		if v := summary[key]; truthy(v) {
			lines = append(lines, pyStr(v))
		}
	}
	return strings.Join(lines, "\n")
}

func modelText(label string, fields map[string]any) string {
	model := "unknown"
	if v := fields["model"]; truthy(v) {
		model = pyStr(v)
	}
	lines := []string{label + ": " + model}
	if v := fields["provider"]; truthy(v) {
		lines = append(lines, "Provider: "+pyStr(v))
	}
	return strings.Join(lines, "\n")
}

// Session tools by toolset from session.info
func toolsText(info *pyjson.Object) string {
	groups, _ := dget(info, "tools").(*pyjson.Object)
	nameSet := map[string]bool{}
	byToolset := map[string][]string{}
	if groups != nil {
		for _, toolset := range groups.Keys {
			items, _ := groups.Vals[toolset].([]any)
			for _, item := range items {
				nameSet[pyStr(item)] = true
				byToolset[toolset] = append(byToolset[toolset], pyStr(item))
			}
		}
	}
	if len(nameSet) == 0 {
		var toolsets []string
		if raw, ok := dget(info, "enabled_toolsets").([]any); ok {
			for _, item := range raw {
				toolsets = append(toolsets, pyStr(item))
			}
		}
		if len(toolsets) > 0 {
			return "Enabled toolsets: " + strings.Join(toolsets, ", ")
		}
		return "No tools available."
	}
	lines := []string{fmt.Sprintf("Available tools (%d):", len(nameSet))}
	toolsets := make([]string, 0, len(byToolset))
	for toolset := range byToolset {
		toolsets = append(toolsets, toolset)
	}
	sort.Strings(toolsets)
	for _, toolset := range toolsets {
		names := byToolset[toolset]
		if len(names) == 0 {
			continue
		}
		sort.Strings(names)
		lines = append(lines, fmt.Sprintf("  %s: %s", toolset, strings.Join(names, ", ")))
	}
	return strings.Join(lines, "\n")
}

// Commands ACP answers itself, every other registry command going to the owner's slash.exec
var gatewayRoutes map[string]gatewayRoute

func init() {
	gatewayRoutes = map[string]gatewayRoute{
		"branch":   {"ACP fork_session", gatewayBranch},
		"discard":  {"prompt.resolve_unknown", gatewayDiscardUnknown},
		"help":     {"local", gatewayHelp},
		"steer":    {"session.steer", gatewayPromptCommand},
		"queue":    {"prompt.submit", gatewayPromptCommand},
		"title":    {"session.mutate rename (bare: slash.exec)", gatewayTitleCommand},
		"undo":     {"session.mutate rewind", gatewayRewindCommand},
		"retry":    {"session.mutate rewind + resubmit", gatewayRewindCommand},
		"tools":    {"session.info", gatewayInfo},
		"model":    {"session.mutate model", gatewayInfo},
		"compress": {"session.mutate compress", gatewayMutation},
		"new":      {"session.mutate reset", gatewayMutation},
		"clear":    {"session.mutate reset", gatewayMutation},
	}
}
