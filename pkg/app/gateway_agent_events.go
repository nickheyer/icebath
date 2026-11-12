package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Event handlers of the gateway session stream by event type
var gatewayHandlers map[string]func(a *GatewayAgent, payload map[string]any, streamCallback func(string), starts map[string]time.Time)

func init() {
	gatewayHandlers = map[string]func(a *GatewayAgent, payload map[string]any, streamCallback func(string), starts map[string]time.Time){
		"message.delta":          (*GatewayAgent).onDelta,
		"message.interim":        (*GatewayAgent).onInterim,
		"tool.generating":        (*GatewayAgent).onToolGenerating,
		"tool.progress":          (*GatewayAgent).onToolProgress,
		"tool.start":             (*GatewayAgent).onToolStart,
		"tool.complete":          (*GatewayAgent).onToolComplete,
		"todo.updated":           (*GatewayAgent).onTodo,
		"session.control.update": (*GatewayAgent).onControlUpdate,
		"thinking.delta":         (*GatewayAgent).onThinking,
		"reasoning.delta":        (*GatewayAgent).onReasoning,
		"reasoning.available":    (*GatewayAgent).onReasoningAvailable,
		"moa.reference":          moaHandler("moa.reference"),
		"moa.aggregating":        moaHandler("moa.aggregating"),
		"notification.show":      (*GatewayAgent).onNotice,
		"notification.clear":     (*GatewayAgent).onNoticeClear,
		"reaction":               (*GatewayAgent).onReaction,
		"agent.print":            (*GatewayAgent).onPrint,
		"review.summary":         (*GatewayAgent).onReviewSummary,
		"approval.request":       (*GatewayAgent).onApproval,
		"clarify.request":        (*GatewayAgent).onClarify,
		"connection.request":     (*GatewayAgent).onConnectionRequest,
		"connection.update":      (*GatewayAgent).onConnectionUpdate,
	}
}

// Serializes the save swap of a secret request so only one capture runs at a time
var gatewaySecretLock sync.Mutex

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
	if pyrepr.Truthy(text) {
		shown = pyrepr.Str(text)
	}
	a.currentTurn().seen[pystr.Strip(shown)] = true
	if pyrepr.Truthy(text) && pystr.Strip(pyrepr.Str(text)) != "" {
		a.emitPrint(pyrepr.Str(text))
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
	if pyrepr.Truthy(payload["name"]) {
		name = pyrepr.Str(payload["name"])
	} else if pyrepr.Truthy(payload["tool_name"]) {
		name = pyrepr.Str(payload["tool_name"])
	}
	args, _ := payload["args"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	callID := ""
	if pyrepr.Truthy(payload["tool_id"]) {
		callID = pyrepr.Str(payload["tool_id"])
	} else if pyrepr.Truthy(payload["tool_call_id"]) {
		callID = pyrepr.Str(payload["tool_call_id"])
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
		cb("tool.completed", name, "", nil, map[string]any{"duration": duration, "is_error": pyrepr.Truthy(payload["is_error"]), "result": result})
	}
	a.mu.Lock()
	a.usageStale = true
	a.mu.Unlock()
	gatewayToolsMarkDirty()
	if name == "todo" {
		a.todoFromResult(result)
	}
	resultText := ""
	if result != nil {
		resultText = pyrepr.Str(result)
	}
	if diff, isStr := payload["inline_diff"].(string); isStr && diff != "" {
		emit := diffPrinter(cprint)
		for _, line := range pystr.Splitlines(pystr.RstripChars(diff, "\n"), false) {
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
		decoded, err := pyjson.Loads(text)
		if err != nil {
			return
		}
		data = decoded
	}
	if snapshot, isDict := pyjson.Plain(data).(map[string]any); isDict {
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
			slog.Debug(fmt.Sprintf("todo snapshot not mirrored: %v", recovered))
		}
	}()
	a.TodoStore().Restore(todos, snapshot["revision"])
}

func (a *GatewayAgent) onControlUpdate(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	a.mu.Lock()
	cb := a.controlUpdateCallback
	a.mu.Unlock()
	if cb != nil {
		cb()
	}
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
		cb("reasoning.available", "_thinking", pystr.Slice(strOrEmpty(payload["text"]), 0, 500), nil, nil)
	}
}

func (a *GatewayAgent) onNotice(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	cb := a.opts.NoticeCallback
	if cb == nil {
		return
	}
	ttl, _ := pyIntValue(payload["ttl_ms"])
	level, kind := "info", "sticky"
	if pyrepr.Truthy(payload["level"]) {
		level = pyrepr.Str(payload["level"])
	}
	if pyrepr.Truthy(payload["kind"]) {
		kind = pyrepr.Str(payload["kind"])
	}
	cb(provider.AgentNotice{Text: strOrEmpty(payload["text"]), Level: level, Kind: kind, TTLMs: ttl,
		Key: noneOrStr(payload["key"]), ID: noneOrStr(payload["id"])})
}

func (a *GatewayAgent) onNoticeClear(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	if cb := a.opts.NoticeClearCallback; cb != nil {
		cb(noneOrStr(payload["key"]))
	}
}

func (a *GatewayAgent) onReaction(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	if cb := a.opts.ReactionCallback; cb != nil {
		cb(noneOrStr(payload["kind"]))
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
	fmt.Println(text)
}

// Answers a dangerous command approval through the CLI's approval panel, deny when it has none
func (a *GatewayAgent) onApproval(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	choices, _ := payload["choices"].([]any)
	has := func(name string) bool {
		for _, c := range choices {
			if c == name {
				return true
			}
		}
		return false
	}
	choice := "deny"
	if cb := a.opts.ApprovalCallback; cb != nil {
		answer, err := cb(a.ctx, strOrEmpty(payload["command"]), strOrEmpty(payload["description"]), has("always"), has("session"), false)
		if err != nil {
			panic(err)
		}
		choice = answer
	}
	if !has(choice) {
		choice = "deny"
	}
	a.mu.Lock()
	generation := a.generation
	a.mu.Unlock()
	if g, hasGeneration := payload["execution_generation"]; hasGeneration {
		generation = g
	}
	if _, err := a.connection.Call(a.ctx, "approval.respond", map[string]any{"session_id": a.SessionID(), "execution_generation": generation,
		"prompt_id": payload["prompt_id"], "choice": choice}); err != nil {
		panic(err)
	}
}

// Answers one clarify question through the CLI's clarify panel
func (a *GatewayAgent) onClarify(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	answer := ""
	if cb := a.opts.ClarifyCallback; cb != nil {
		qid := any("q0")
		if pyrepr.Truthy(payload["prompt_id"]) {
			qid = payload["prompt_id"]
		}
		choices, _ := payload["choices"].([]any)
		reply := cb([]any{map[string]any{"qid": qid, "question": strOrEmpty(payload["question"]), "choices": append([]any{}, choices...),
			"multi_select": pyrepr.Truthy(payload["multi_select"])}})
		if reply != nil {
			answers, _ := reply["answers"].(map[string]any)
			switch value := answers[pyrepr.Str(qid)].(type) {
			case []any:
				parts := make([]string, len(value))
				for i, v := range value {
					parts[i] = pyrepr.Str(v)
				}
				answer = strings.Join(parts, ", ")
			case []string:
				answer = strings.Join(value, ", ")
			case nil:
			default:
				if pyrepr.Truthy(value) {
					answer = pyrepr.Str(value)
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

func (a *GatewayAgent) onConnectionRequest(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	if cb := a.opts.ConnectionCallback; cb != nil {
		cb(payload)
	}
}

func (a *GatewayAgent) onConnectionUpdate(payload map[string]any, streamCallback func(string), starts map[string]time.Time) {
	if cb := a.opts.ConnectionUpdateCallback; cb != nil {
		cb(payload)
	}
}

// Reopens the connection card from a resume snapshot's open operation, or closes a stale card
func (a *GatewayAgent) restoreConnectionCard(pending map[string]any) {
	if len(pending) > 0 {
		if cb := a.opts.ConnectionCallback; cb != nil {
			cb(pending)
		}
	} else if cb := a.opts.ConnectionUpdateCallback; cb != nil {
		cb(map[string]any{"settled": true})
	}
}

// Sends the connection card's answer to the gateway's open operation
func (a *GatewayAgent) ConnectionRespond(opID string, result any) (any, error) {
	return a.connection.Call(a.ctx, "connection.respond", map[string]any{"op_id": opID, "result": result,
		"owner": map[string]any{"type": "session", "session_id": a.SessionID()}})
}

// Asks the gateway to mint a failed or expired connector row again on the open operation
func (a *GatewayAgent) ConnectionRetry(name string) (any, error) {
	return a.connection.Call(a.ctx, "connectors.connect", map[string]any{"connectors": []any{name}, "reconnect": true,
		"owner": map[string]any{"type": "session", "session_id": a.SessionID()}})
}

// Answers a server request through the CLI's modal, the answer travelling only in the response frame
func (a *GatewayAgent) answerRequest(frame *pyjson.Object, method string, params map[string]any) {
	id := frame.Vals["id"]
	a.mu.Lock()
	a.openRequest = &gatewayOpenRequest{id: id, method: method}
	a.mu.Unlock()
	answer := func() (answer any) {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Debug(fmt.Sprintf("server request %s failed: %v", method, recovered))
				if method == "clarify" {
					answer = map[string]any{"outcome": "cancelled"}
				} else {
					answer = ""
				}
			}
		}()
		return gatewayServerRequests[method](a, params)
	}()
	a.mu.Lock()
	a.openRequest = nil
	a.mu.Unlock()
	if method == "clarify" {
		a.connection.Reply(id, answer)
		return
	}
	value, _ := answer.(string)
	a.connection.Reply(id, map[string]any{"value": value})
}

// Releases the modal answering a request the gateway settled or withdrew meanwhile
func (a *GatewayAgent) onRequestWithdrawn(payload map[string]any) {
	a.mu.Lock()
	current := a.openRequest
	a.mu.Unlock()
	if current == nil || !pyjson.Equal(payload["id"], current.id) || a.opts.ModalWithdrawCallback == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Debug(fmt.Sprintf("modal withdraw for %s failed: %v", current.method, recovered))
		}
	}()
	a.opts.ModalWithdrawCallback(current.method)
}

func (a *GatewayAgent) askClarify(params map[string]any) any {
	return clarifyReply(a.opts.ClarifyCallback, params)
}

// Sudo password from the CLI's masked panel, empty when it has none
func (a *GatewayAgent) askSudo(params map[string]any) any {
	if cb := a.opts.SudoPasswordCallback; cb != nil {
		return cb(a.ctx)
	}
	return ""
}

// Secret from the CLI's skill setup panel with its save captured, the gateway saving the value itself
func (a *GatewayAgent) askSecret(params map[string]any) any {
	callback := skills.SecretCaptureCallback()
	if callback == nil {
		return ""
	}
	var captured []string
	capture := func(_ context.Context, _, key, value string) (config.SecureSaveResult, error) {
		captured = append(captured, value)
		return config.SecureSaveResult{Success: true, StoredAs: key, Validated: false}, nil
	}
	gatewaySecretLock.Lock()
	defer gatewaySecretLock.Unlock()
	saver := swapSecretSaver(capture)
	defer swapSecretSaver(saver)
	metadata, _ := params["metadata"].(map[string]any)
	if _, err := callback(strOrEmpty(params["env_var"]), strOrEmpty(params["prompt"]), metadata); err != nil {
		panic(err)
	}
	if len(captured) > 0 {
		return captured[0]
	}
	return ""
}

// Password manager unlock answer from the CLI's masked panel
func (a *GatewayAgent) askVaultUnlock(params map[string]any) any {
	callback := browser.GetUnlockPromptCallback(a.threadCtx())
	if callback == nil {
		return ""
	}
	return callback(pyStrOr(params["backend"]), pyStrOr(params["display_name"]))
}

// Login to save as one JSON string of identifier and password, empty without a password
func (a *GatewayAgent) askVaultSaveLogin(params map[string]any) any {
	callback := browser.GetSaveLoginPromptCallback(a.threadCtx())
	if callback == nil {
		return ""
	}
	answer := callback(pyStrOr(params["origin"]), pyStrOr(params["site"]))
	if answer == nil || answer["password"] == "" {
		return ""
	}
	defer clear(answer)
	dumped, err := pyjson.Dumps(pyjson.ObjectOf("identifier", answer["identifier"], "password", answer["password"]))
	if err != nil {
		panic(err)
	}
	return dumped
}

// One time code from the CLI's code panel
func (a *GatewayAgent) askVaultCode(params map[string]any) any {
	callback := browser.GetCodePromptCallback(a.threadCtx())
	if callback == nil {
		return ""
	}
	return callback(pyStrOr(params["site"]), pyStrOr(params["hint"]))
}

// Python str of a value with None and falsy values reading as empty
func pyStrOr(v any) string { return strOrEmpty(v) }

// String of a value, empty for None only
func noneOrStr(v any) string {
	if v == nil {
		return ""
	}
	return pyrepr.Str(v)
}
