package app

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	termutil "github.com/nickheyer/icebath/pkg/utils/term"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Event handlers of the gateway session stream by event type
var gatewayHandlers map[string]func(a *GatewayAgent, payload map[string]any, streamCallback func(string), starts map[string]time.Time)

func init() {
	gatewayHandlers = map[string]func(a *GatewayAgent, payload map[string]any, streamCallback func(string), starts map[string]time.Time){
		"message.delta":       (*GatewayAgent).onDelta,
		"message.interim":     (*GatewayAgent).onInterim,
		"tool.generating":     (*GatewayAgent).onToolGenerating,
		"tool.progress":       (*GatewayAgent).onToolProgress,
		"tool.start":          (*GatewayAgent).onToolStart,
		"tool.complete":       (*GatewayAgent).onToolComplete,
		"todo.updated":        (*GatewayAgent).onTodo,
		"thinking.delta":      (*GatewayAgent).onThinking,
		"reasoning.delta":     (*GatewayAgent).onReasoning,
		"reasoning.available": (*GatewayAgent).onReasoningAvailable,
		"moa.reference":       moaHandler("moa.reference"),
		"moa.aggregating":     moaHandler("moa.aggregating"),
		"notification.show":   (*GatewayAgent).onNotice,
		"notification.clear":  (*GatewayAgent).onNoticeClear,
		"agent.print":         (*GatewayAgent).onPrint,
		"review.summary":      (*GatewayAgent).onReviewSummary,
		"approval.request":    (*GatewayAgent).onApproval,
		"clarify.request":     (*GatewayAgent).onClarify,
	}
}

func (a *GatewayAgent) dispatch(kind string, payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	if handler := gatewayHandlers[kind]; handler != nil {
		handler(a, payload, streamCallback, starts)
	}
}

// Current turn of the agent
func (a *GatewayAgent) currentTurn() *gatewayTurn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turn
}

// Paints one reply delta to the terminal and the stream callback, a non text delta closing the segment
func (a *GatewayAgent) onDelta(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	text, isStr := payload["text"].(string)
	if !isStr {
		a.closeSegment(starts, true)
		return
	}
	turn := a.currentTurn()
	if turn.gapped {
		return
	}
	if cb := a.opts.StreamDeltaCallback; cb != nil {
		cb(text)
		turn.shown = append(turn.shown, text)
	}
	if streamCallback != nil {
		streamCallback(text)
		turn.spoken = append(turn.spoken, text)
	}
}

// Closes the reply segment, sending the separator when asked or when the segment showed text
func (a *GatewayAgent) closeSegment(starts map[string]time.Time, separator bool) {
	turn := a.currentTurn()
	if cb := a.opts.StreamDeltaCallback; cb != nil && (separator || strings.Join(turn.shown, "") != "") {
		cb(nil)
	}
	turn.closeSegment()
}

// Prints an interim assistant message between tool steps
func (a *GatewayAgent) onInterim(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	text := payload["text"]
	a.closeSegment(starts, false)
	shown := ""
	if anyutil.IsTrue(text) {
		shown = anyutil.String(text)
	}
	a.currentTurn().seen[strings.TrimSpace(shown)] = true
	if cb := a.opts.InterimAssistantCallback; cb != nil {
		cb(text, anyutil.IsTrue(payload["already_streamed"]))
	} else if anyutil.IsTrue(text) && strings.TrimSpace(anyutil.String(text)) != "" {
		a.emitPrint(anyutil.String(text))
	}
}

func (a *GatewayAgent) onToolGenerating(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	if cb := a.opts.ToolGenCallback; cb != nil {
		cb(strOrEmpty(payload["name"]))
	}
}

func (a *GatewayAgent) onToolProgress(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	if cb := a.opts.ToolProgressCallback; cb != nil {
		cb("tool.progress", strOrEmpty(payload["name"]), strOrEmpty(payload["message"]), nil, map[string]any{
			"tool_call_id": payload["tool_id"], "progress": payload["progress"], "total": payload["total"], "percent": payload["percent"]})
	}
}

// Name, args and call id of a tool frame with the defaults the CLI shows
func toolFrameFields(payload map[string]any) (string, map[string]any, string) {
	name := "tool"
	if anyutil.IsTrue(payload["name"]) {
		name = anyutil.String(payload["name"])
	} else if anyutil.IsTrue(payload["tool_name"]) {
		name = anyutil.String(payload["tool_name"])
	}
	args, _ := payload["args"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	callID := ""
	if anyutil.IsTrue(payload["tool_id"]) {
		callID = anyutil.String(payload["tool_id"])
	} else if anyutil.IsTrue(payload["tool_call_id"]) {
		callID = anyutil.String(payload["tool_call_id"])
	}
	return name, args, callID
}

// Starts a tool line, reading the session stats once per tool batch so the spinner counters move
func (a *GatewayAgent) onToolStart(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	a.closeSegment(starts, false)
	name, args, callID := toolFrameFields(payload)
	starts[callID] = time.Now()
	a.mu.Lock()
	stale := a.usageStale
	a.usageStale = false
	a.mu.Unlock()
	if stale {
		a.refreshStats(nil)
	}
	if cb := a.opts.ToolProgressCallback; cb != nil {
		cb("tool.started", name, tools.BuildToolPreview(a.opts.ToolEnv, name, args, nil), args, nil)
	}
}

// Completes a tool line, mirrors a todo result and prints the gateway's inline diff
func (a *GatewayAgent) onToolComplete(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	name, args, callID := toolFrameFields(payload)
	result := payload["result"]
	duration, hasDuration := pyNumber(payload["duration_s"])
	if payload["duration_s"] == nil || !hasDuration {
		duration = 0.0
		if started, ok := starts[callID]; ok {
			delete(starts, callID)
			duration = time.Since(started).Seconds()
		}
	}
	if cb := a.opts.ToolProgressCallback; cb != nil {
		cb("tool.completed", name, "", nil, map[string]any{"duration": duration, "is_error": anyutil.IsTrue(payload["is_error"]), "result": result})
	}
	a.mu.Lock()
	a.usageStale = true
	a.mu.Unlock()
	if name == "todo" {
		a.todoFromResult(result)
	}
	resultText := ""
	if result != nil {
		resultText = anyutil.String(result)
	}
	if diff, isStr := payload["inline_diff"].(string); isStr && diff != "" {
		emit := diffPrinter(cprint)
		for _, line := range textutil.Lines(strings.TrimRight(diff, "\n")) {
			emit(line)
		}
		if name == "delegate_task" && a.opts.ToolCompleteCallback != nil {
			a.opts.ToolCompleteCallback(callID, name, args, resultText)
		}
	} else if a.opts.ToolCompleteCallback != nil {
		a.opts.ToolCompleteCallback(callID, name, args, resultText)
	}
}

// Mirrors a todo tool result into the todo store
func (a *GatewayAgent) todoFromResult(result any) {
	data := result
	if text, isStr := result.(string); isStr {
		decoded, err := jsonutil.Decode(text)
		if err != nil {
			return
		}
		data = decoded
	}
	if snapshot, isDict := jsonutil.Plain(data).(map[string]any); isDict {
		a.setTodos(snapshot)
	}
}

func (a *GatewayAgent) onTodo(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	a.setTodos(payload)
}

// Restores the gateway's todos and revision into the store without minting a new revision
func (a *GatewayAgent) setTodos(snapshot map[string]any) {
	todos, isList := snapshot["todos"].([]any)
	if !isList {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			pkgLog().Debug(fmt.Sprintf("todo snapshot not mirrored: %v", recovered))
		}
	}()
	a.TodoStore().Restore(todos, snapshot["revision"])
}

func (a *GatewayAgent) onThinking(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	if cb := a.opts.ThinkingCallback; cb != nil {
		cb(strOrEmpty(payload["text"]))
	}
}

func (a *GatewayAgent) onReasoning(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	if cb := a.ReasoningCallback(); cb != nil {
		cb(strOrEmpty(payload["text"]))
	}
}

func (a *GatewayAgent) onReasoningAvailable(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	if cb := a.opts.ToolProgressCallback; cb != nil {
		cb("reasoning.available", "_thinking", textutil.First(strOrEmpty(payload["text"]), 500), nil, nil)
	}
}

func (a *GatewayAgent) onNotice(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	cb := a.opts.NoticeCallback
	if cb == nil {
		return
	}
	ttl, _ := pyIntValue(payload["ttl_ms"])
	level, kind := "info", "sticky"
	if anyutil.IsTrue(payload["level"]) {
		level = anyutil.String(payload["level"])
	}
	if anyutil.IsTrue(payload["kind"]) {
		kind = anyutil.String(payload["kind"])
	}
	cb(provider.AgentNotice{Text: strOrEmpty(payload["text"]), Level: level, Kind: kind, TTLMs: ttl,
		Key: noneOrStr(payload["key"]), ID: noneOrStr(payload["id"])})
}

func (a *GatewayAgent) onNoticeClear(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	if cb := a.opts.NoticeClearCallback; cb != nil {
		cb(noneOrStr(payload["key"]))
	}
}

func (a *GatewayAgent) onPrint(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	a.emitPrint(strOrEmpty(payload["text"]))
}

func (a *GatewayAgent) onReviewSummary(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	if line := reviewSummaryLine(payload); line != "" {
		a.emitPrint(line)
	}
}

// Prints a status line through the CLI's print function unless status output is suppressed
func (a *GatewayAgent) emitPrint(text string) {
	a.mu.Lock()
	suppressed, printFn := a.suppressStatusOutput, a.printFn
	a.mu.Unlock()
	if suppressed {
		return
	}
	if printFn != nil {
		printFn(text)
		return
	}
	fmt.Fprintln(termutil.Stdout, text)
}

// One dim status line for a prompt this run answered at once, no user being there to ask
func (a *GatewayAgent) noteUnanswered(text string) {
	a.emitPrint("\n" + dimANSI + "  " + text + resetANSI)
}

// Label, subject and outcome of a prompt this run answered at once, as one status line
func (a *GatewayAgent) noteUnansweredPrompt(label, detail, outcome string) {
	a.emitPrint("\n" + dimANSI + i18n.T(a.ctx, "cli.session.persist_prompt_summary", "", map[string]any{
		"icon": "⚠", "label": label, "detail": squash(detail, 120), "outcome": outcome}) + resetANSI)
}

// Denies a dangerous command approval at once and says so, a one shot run having no user to ask
func (a *GatewayAgent) onApproval(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	a.noteUnansweredPrompt(i18n.T(a.ctx, "cli.approval.label", "", nil), strOrEmpty(payload["command"]), i18n.T(a.ctx, "cli.approval.outcome_deny", "", nil))
	a.mu.Lock()
	generation := a.generation
	a.mu.Unlock()
	if g, hasGeneration := payload["execution_generation"]; hasGeneration {
		generation = g
	}
	if _, err := a.connection.Call(a.ctx, "approval.respond", map[string]any{"session_id": a.SessionID(), "execution_generation": generation,
		"prompt_id": payload["prompt_id"], "choice": "deny"}); err != nil {
		panic(err)
	}
}

// Answers one clarify question through the CLI's clarify panel
func (a *GatewayAgent) onClarify(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	answer := ""
	if cb := a.opts.ClarifyCallback; cb != nil {
		qid := any("q0")
		if anyutil.IsTrue(payload["prompt_id"]) {
			qid = payload["prompt_id"]
		}
		choices, _ := payload["choices"].([]any)
		reply := cb([]any{map[string]any{"qid": qid, "question": strOrEmpty(payload["question"]), "choices": append([]any{}, choices...),
			"multi_select": anyutil.IsTrue(payload["multi_select"])}})
		if reply != nil {
			answers, _ := reply["answers"].(map[string]any)
			switch value := answers[anyutil.String(qid)].(type) {
			case []any:
				parts := make([]string, len(value))
				for i, v := range value {
					parts[i] = anyutil.String(v)
				}
				answer = strings.Join(parts, ", ")
			case []string:
				answer = strings.Join(value, ", ")
			case nil:
			default:
				if anyutil.IsTrue(value) {
					answer = anyutil.String(value)
				}
			}
		}
	}
	a.mu.Lock()
	generation := a.generation
	a.mu.Unlock()
	if g, hasGeneration := payload["execution_generation"]; hasGeneration {
		generation = g
	}
	if _, err := a.connection.Call(a.ctx, "clarify.respond", map[string]any{"session_id": a.SessionID(), "execution_generation": generation,
		"prompt_id": payload["prompt_id"], "answer": answer}); err != nil {
		panic(err)
	}
}

// Answers a server request, the answer travelling only in the response frame
func (a *GatewayAgent) answerRequest(frame *jsonutil.Object, method string, params map[string]any) {
	id := frame.Value("id")
	answer := func() (answer any) {
		defer func() {
			if recovered := recover(); recovered != nil {
				pkgLog().Debug(fmt.Sprintf("server request %s failed: %v", method, recovered))
				if method == "clarify" {
					answer = map[string]any{"outcome": "cancelled"}
				} else {
					answer = ""
				}
			}
		}()
		return gatewayServerRequests[method](a, params)
	}()
	if method == "clarify" {
		a.connection.Reply(id, answer)
		return
	}
	value, _ := answer.(string)
	a.connection.Reply(id, map[string]any{"value": value})
}

func (a *GatewayAgent) askClarify(params map[string]any) any {
	return clarifyReply(a.opts.ClarifyCallback, params)
}

// Skips a sudo password request at once and says so
func (a *GatewayAgent) askSudo(params map[string]any) any {
	a.noteUnansweredPrompt(i18n.T(a.ctx, "cli.secret.bell_sudo", "", nil), strOrEmpty(params["command"]), i18n.T(a.ctx, "cli.secret.sudo_skipped", "", nil))
	return ""
}

// Secret a skill asks for, read at the masked console prompt for the gateway to store, empty when skipped or when no terminal can be asked
func (a *GatewayAgent) askSecret(params map[string]any) any {
	value, err := console().MaskedSecret(strOrEmpty(params["prompt"]) + " (hidden, ESC or empty Enter to skip): ")
	if err != nil && !errors.Is(err, cliui.ErrInterrupt) && !errors.Is(err, cliui.ErrEOF) {
		panic(err)
	}
	if err != nil || value == "" {
		a.noteUnanswered("⏭ Secret entry skipped")
		return ""
	}
	a.noteUnanswered("✓ Stored secret in " + home.Display(home.Dir(a.ctx)) + "/.env as " + strOrEmpty(params["env_var"]))
	return value
}

// Leaves a password manager locked at once and says so
func (a *GatewayAgent) askVaultUnlock(params map[string]any) any {
	a.noteUnanswered(i18n.T(a.ctx, "cli.secret.stays_locked", "", map[string]any{"name": pyStrOr(params["display_name"])}))
	return ""
}

// Declines to save a login at once and says so
func (a *GatewayAgent) askVaultSaveLogin(params map[string]any) any {
	a.noteUnanswered(i18n.T(a.ctx, "cli.secret.login_not_saved", "", map[string]any{"site": pyStrOr(params["site"])}))
	return ""
}

// Enters no one time code at once and says so
func (a *GatewayAgent) askVaultCode(params map[string]any) any {
	a.noteUnanswered(i18n.T(a.ctx, "cli.secret.code_not_entered", "", map[string]any{"site": pyStrOr(params["site"])}))
	return ""
}

// Python str of a value with None and falsy values reading as empty
func pyStrOr(v any) string { return strOrEmpty(v) }

// String of a value, empty for None only
func noneOrStr(v any) string {
	if v == nil {
		return ""
	}
	return anyutil.String(v)
}
