package acp

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/tools"
)

var eventsLog = logging.GetLogger("acp_adapter.events")

// Client side of the connection that receives session updates
type SessionUpdater interface {
	SessionUpdate(ctx context.Context, sessionID string, update pyacp.SessionUpdate, meta *pyjson.Object) error
}

var newToolCallID = MakeToolCallID

var captureLocalEditSnapshot = tools.CaptureLocalEditSnapshot

var planStatus = map[string]string{"pending": "pending", "in_progress": "in_progress", "completed": "completed", "cancelled": "completed"}

// Native ACP plan update of a todo tool result, nil when the result carries no todo list
func buildPlanUpdateFromTodoResult(result any) *pyacp.AgentPlanUpdate {
	s, ok := result.(string)
	if !ok || strip(s) == "" {
		return nil
	}
	data, ok := isDict(jsonLoadsMaybe(&s))
	if !ok {
		return nil
	}
	todos, ok := isList(dget(data, "todos"))
	if !ok {
		return nil
	}
	entries := []pyacp.PlanEntry{}
	for _, it := range todos {
		item, ok := isDict(it)
		if !ok {
			continue
		}
		content := strip(pyStr(pyrepr.Or(pyrepr.Or(dget(item, "content"), dget(item, "id")), "")))
		if content == "" {
			continue
		}
		raw := strip(pyStr(pyrepr.Or(dget(item, "status"), "pending")))
		if raw == "cancelled" {
			content = "[cancelled] " + content
		}
		st, ok := planStatus[raw]
		if !ok {
			st = "pending"
		}
		entries = append(entries, pyacp.NewPlanEntry(content, "medium", st))
	}
	return pyacp.UpdatePlan(entries)
}

// Sends one session update from a worker thread through the loop, waiting up to five seconds
func sendUpdate(conn SessionUpdater, sessionID string, loop Loop, update pyacp.SessionUpdate) {
	future := safeScheduleThreadsafe(func(ctx context.Context) (any, error) {
		return nil, conn.SessionUpdate(ctx, sessionID, update, nil)
	}, loop, eventsLog, "Failed to send ACP update")
	if future == nil {
		return
	}
	if _, err := future.Result(5 * time.Second); err != nil {
		eventsLog.Debug("Failed to send ACP update\n" + err.Error())
	}
}

// Arguments and edit snapshot remembered for an open tool call
type ToolCallMeta struct {
	Args     *pyjson.Object
	Snapshot *tools.LocalEditSnapshot
}

// Open ACP tool calls per tool name in start order with their remembered metadata
type ToolCalls struct {
	mu   sync.Mutex
	IDs  map[string][]string
	Meta map[string]ToolCallMeta
}

// Empty tracker of open tool calls
func NewToolCalls() *ToolCalls {
	return &ToolCalls{IDs: map[string][]string{}, Meta: map[string]ToolCallMeta{}}
}

// Pops the oldest open id of name with its metadata, false when none is open
func (tc *ToolCalls) pop(name string) (string, ToolCallMeta, bool) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	queue := tc.IDs[name]
	if len(queue) == 0 {
		return "", ToolCallMeta{}, false
	}
	id := queue[0]
	queue = queue[1:]
	meta := tc.Meta[id]
	delete(tc.Meta, id)
	if len(queue) == 0 {
		delete(tc.IDs, name)
	} else {
		tc.IDs[name] = queue
	}
	return id, meta, true
}

func resultText(result any) *string {
	if result == nil {
		return nil
	}
	s := pyStr(result)
	return &s
}

// Closes the oldest open tool call of name with its result, returning its id or empty when none is open
func CloseToolCall(conn SessionUpdater, sessionID string, loop Loop, calls *ToolCalls, name string, result any, isError bool) string {
	id, meta, ok := calls.pop(name)
	if !ok {
		return ""
	}
	sendUpdate(conn, sessionID, loop, BuildToolComplete(id, name, resultText(result), meta.Args, meta.Snapshot, isError))
	return id
}

// Closes every tool call still open at turn end as abandoned and reports how many there were
func FlushOpenToolCalls(conn SessionUpdater, sessionID string, loop Loop, calls *ToolCalls) int {
	calls.mu.Lock()
	type open struct {
		name string
		ids  []string
	}
	var opens []open
	for name, q := range calls.IDs {
		if len(q) > 0 {
			opens = append(opens, open{name, append([]string{}, q...)})
		}
	}
	for _, o := range opens {
		for _, id := range o.ids {
			delete(calls.Meta, id)
		}
		delete(calls.IDs, o.name)
	}
	calls.mu.Unlock()
	flushed := 0
	for _, o := range opens {
		for _, id := range o.ids {
			sendUpdate(conn, sessionID, loop, BuildToolAbandoned(id, o.name))
			flushed++
		}
	}
	if flushed > 0 {
		eventsLog.Debug(fmt.Sprintf("Flushed %d ACP tool call(s) left open at turn end", flushed))
	}
	return flushed
}

// Per turn flags the progress and step callbacks share
type TurnState struct {
	mu            sync.Mutex
	sawCompletion bool
}

func (s *TurnState) setSawCompletion() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.sawCompletion = true
	s.mu.Unlock()
}

// Reports whether a tool.completed event arrived this turn
func (s *TurnState) SawCompletion() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sawCompletion
}

// Tool progress callback starting ACP tool calls on tool.started and closing them on tool.completed
func MakeToolProgressCb(conn SessionUpdater, sessionID string, loop Loop, calls *ToolCalls, editPolicy AutoApproveGetter, turn *TurnState) func(event, name, preview string, args map[string]any, extra map[string]any) {
	return func(event, name, preview string, args map[string]any, extra map[string]any) {
		if event == "tool.completed" && name != "" {
			turn.setSawCompletion()
			CloseToolCall(conn, sessionID, loop, calls, name, extra["result"], truthy(extra["is_error"]))
			return
		}
		if event != "tool.started" {
			return
		}
		coerced := CoerceToolArgs(args)
		plain, _ := pyjson.Plain(coerced).(map[string]any)
		id := newToolCallID()
		var snapshot *tools.LocalEditSnapshot
		if name == "write_file" || name == "patch" || name == "skill_manage" {
			snapshot = captureSnapshot(name, plain)
		}
		calls.mu.Lock()
		calls.IDs[name] = append(calls.IDs[name], id)
		calls.Meta[id] = ToolCallMeta{Args: coerced, Snapshot: snapshot}
		calls.mu.Unlock()
		var editDiff *tools.EditProposal
		if (name == "write_file" || name == "patch") && editPolicy != nil {
			proposal, err := tools.BuildEditProposal(nil, name, plain)
			if err != nil {
				eventsLog.Debug("Failed to prepare auto-approved ACP edit diff for " + name)
			} else if proposal != nil {
				policy, cwd, perr := editPolicy()
				if perr != nil {
					eventsLog.Debug("Failed to prepare auto-approved ACP edit diff for " + name)
				} else if tools.ShouldAutoApproveEdit(proposal, policy, cwd) {
					editDiff = proposal
				}
			}
		}
		sendUpdate(conn, sessionID, loop, BuildToolStart(id, name, coerced, editDiff))
	}
}

// Local edit snapshot of a file tool call, nil when capturing panics
func captureSnapshot(name string, args map[string]any) (snap *tools.LocalEditSnapshot) {
	defer func() {
		if recover() != nil {
			eventsLog.Debug("Failed to capture ACP edit snapshot for " + name)
			snap = nil
		}
	}()
	return captureLocalEditSnapshot(nil, name, args, nil)
}

// Stable uuid per streamed assistant message, a new id after each close
type AssistantMessageIDAllocator struct {
	mu     sync.Mutex
	active string
	last   string
}

// Active message id, allocating one when none is open
func (a *AssistantMessageIDAllocator) Current() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active == "" {
		a.active = uuid.NewString()
		a.last = a.active
	}
	return a.active
}

// Most recently allocated id, empty before the first
func (a *AssistantMessageIDAllocator) Last() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

// Ends the active message so the next chunk starts a new id
func (a *AssistantMessageIDAllocator) Close() {
	a.mu.Lock()
	a.active = ""
	a.mu.Unlock()
}

func makeTextCb(conn SessionUpdater, sessionID string, loop Loop, thought bool, ids *AssistantMessageIDAllocator) func(text any) {
	return func(text any) {
		s, _ := text.(string)
		if s != "" {
			var update pyacp.SessionUpdate
			var mid *string
			if ids != nil {
				mid = pyacp.Ptr(ids.Current())
			}
			if thought {
				u := pyacp.UpdateAgentThoughtText(s)
				u.MessageID = mid
				update = u
			} else {
				u := pyacp.UpdateAgentMessageText(s)
				u.MessageID = mid
				update = u
			}
			sendUpdate(conn, sessionID, loop, update)
		} else if text == nil && ids != nil {
			ids.Close()
		}
	}
}

// Thinking callback streaming agent thought chunks
func MakeThinkingCb(conn SessionUpdater, sessionID string, loop Loop, ids *AssistantMessageIDAllocator) func(text any) {
	return makeTextCb(conn, sessionID, loop, true, ids)
}

// Stream callback sending agent message chunks to the editor
func MakeMessageCb(conn SessionUpdater, sessionID string, loop Loop, ids *AssistantMessageIDAllocator) func(text any) {
	return makeTextCb(conn, sessionID, loop, false, ids)
}

// Step callback closing the previous round's tool calls and mirroring todo results as a plan
func MakeStepCb(conn SessionUpdater, sessionID string, loop Loop, calls *ToolCalls, turn *TurnState) func(apiCallCount int, prevTools []map[string]any) {
	return func(apiCallCount int, prevTools []map[string]any) {
		for _, info := range prevTools {
			toolName := pyStr(pyrepr.Or(info["name"], info["function_name"]))
			if !truthy(pyrepr.Or(info["name"], info["function_name"])) {
				continue
			}
			var result any
			if r, ok := info["result"]; ok {
				result = r
			} else {
				result = info["output"]
			}
			functionArgs := pyrepr.Or(info["arguments"], info["args"])
			if !turn.SawCompletion() {
				id, meta, ok := calls.pop(toolName)
				if !ok {
					continue
				}
				args := meta.Args
				if truthy(functionArgs) {
					args = CoerceToolArgs(functionArgs)
				}
				sendUpdate(conn, sessionID, loop, BuildToolComplete(id, toolName, resultText(result), args, meta.Snapshot, false))
			}
			if toolName == "todo" {
				if plan := buildPlanUpdateFromTodoResult(result); plan != nil {
					sendUpdate(conn, sessionID, loop, plan)
				}
			}
		}
	}
}
