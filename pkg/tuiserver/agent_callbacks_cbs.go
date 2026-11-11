package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Whether an agent callback may present to the session, a diagnostic one only under the TUI warning policy
func agentPresentationEnabled(ctx context.Context, sid string, diagnostic bool) bool {
	session := tuigateway.SessionRecord(sid)
	a := sessionAgent(session)
	if a != nil && a.MuteNotificationReply() {
		return false
	}
	if !diagnostic {
		return true
	}
	if session == nil {
		session = tuigateway.NewSession(map[string]any{})
	}
	var notificationConfig map[string]any
	if a != nil {
		notificationConfig = a.NotificationConfig()
	}
	return config.WarningNotificationsEnabled(sessionProfileRuntimeScope(ctx, session, true), "tui", notificationConfig)
}

// Status line update of an agent, dropped when a warning status may not present
func agentStatusUpdate(ctx context.Context, sid string, kind string, text any) {
	message := text
	if text == nil {
		message = kind
	}
	if !agentPresentationEnabled(ctx, sid, config.IsWarningStatus(kind, message)) {
		return
	}
	var textPtr *string
	if text != nil {
		value := pyrepr.Str(text)
		textPtr = &value
	}
	statusUpdate(ctx, sid, kind, textPtr)
}

// Thinking spinner text of an agent, a diagnostic one only under the warning policy
func agentThinkingUpdate(ctx context.Context, sid string, text any) {
	_, diagnostic := text.(config.DiagnosticText)
	if !agentPresentationEnabled(ctx, sid, diagnostic) {
		return
	}
	emitEvent(ctx, "thinking.delta", sid, map[string]any{"text": text})
}

// Out of band agent notice as notification.show, a diagnostic one only under the warning policy
func agentNoticeUpdate(ctx context.Context, sid string, notice provider.AgentNotice) {
	if !agentPresentationEnabled(ctx, sid, config.IsDiagnosticNotice(notice.Level, notice.Key)) {
		return
	}
	emitEvent(ctx, "notification.show", sid, map[string]any{"text": notice.Text, "level": notice.Level, "kind": notice.Kind,
		"ttl_ms": notice.TTLMs, "key": notice.Key, "id": notice.ID})
}

// Reasoning delta of a session that shows reasoning, flagged verbose for a verbose session
func emitReasoningDelta(ctx context.Context, sid string, text string) {
	if !sessionShowReasoning(ctx, sid) {
		return
	}
	payload := map[string]any{"text": text}
	if sessionVerbose(sid) {
		payload["verbose"] = true
	}
	emitEvent(ctx, "reasoning.delta", sid, payload)
}

// Every per session agent callback, bound to the session id and the context of the turn that installs them
type agentCallbackSet struct {
	ToolStart          func(toolCallID, name string, args map[string]any)
	ToolComplete       func(toolCallID, name string, args map[string]any, result string)
	ToolResultMetadata func(toolCallID, name string, args map[string]any, result string) map[string]any
	ToolProgress       func(event, name, preview string, args map[string]any, extra map[string]any)
	ToolGen            func(name string)
	Thinking           func(text any)
	Reaction           func(kind string)
	Reasoning          func(text string)
	Status             func(kind string, text any)
	Notice             func(notice provider.AgentNotice)
	NoticeClear        func(key string)
	Clarify            tools.ClarifyCallback
	ReadTerminal       func(start, count *int) string
	ReadPreview        func(start, count *int) string
	DrivePreview       func(payload map[string]any) string
	ReadWindowBelow    func() string
	Connection         func(payload map[string]any)
	Tour               func(payload map[string]any) string
	Theme              func(payload map[string]any) any
	SetupBeat          func(payload map[string]any) any
	InterimAssistant   func(text any, alreadyStreamed bool)
}

// Callbacks of one session's agent, interim commentary only when display.interim_assistant_messages is on
func agentCbs(ctx context.Context, sid string) agentCallbackSet {
	readBlock := func(method string, timeout float64) func(start, count *int) string {
		return func(start, count *int) string {
			params := map[string]any{}
			if start != nil {
				params["start"] = *start
			}
			if count != nil {
				params["count"] = *count
			}
			return askRequest(ctx, method, sid, params, &timeout)
		}
	}
	windowTimeout := 30.0
	cbs := agentCallbackSet{
		ToolStart: func(toolCallID, name string, args map[string]any) { onToolStart(ctx, sid, toolCallID, name, args) },
		ToolComplete: func(toolCallID, name string, args map[string]any, result string) {
			onToolComplete(ctx, sid, toolCallID, name, args, result)
		},
		ToolResultMetadata: func(toolCallID, name string, args map[string]any, result string) map[string]any {
			return prepareToolResultMetadata(ctx, sid, toolCallID, name, args, result)
		},
		ToolProgress: func(event, name, preview string, args map[string]any, extra map[string]any) {
			onToolProgress(ctx, sid, event, name, preview, extra)
		},
		ToolGen: func(name string) {
			if toolProgressEnabled(sid) {
				emitEvent(ctx, "tool.generating", sid, map[string]any{"name": name})
			}
		},
		Thinking:    func(text any) { agentThinkingUpdate(ctx, sid, text) },
		Reaction:    func(kind string) { emitEvent(ctx, "reaction", sid, map[string]any{"kind": kind}) },
		Reasoning:   func(text string) { emitReasoningDelta(ctx, sid, text) },
		Status:      func(kind string, text any) { agentStatusUpdate(ctx, sid, kind, text) },
		Notice:      func(notice provider.AgentNotice) { agentNoticeUpdate(ctx, sid, notice) },
		NoticeClear: func(key string) { emitEvent(ctx, "notification.clear", sid, map[string]any{"key": key}) },
		Clarify: func(callCtx context.Context, questions []any) (map[string]any, error) {
			rows := []map[string]any{}
			for _, question := range questions {
				if row, ok := question.(map[string]any); ok {
					rows = append(rows, row)
				}
			}
			return clarifyBlock(callCtx, sid, rows), nil
		},
		ReadTerminal:    readBlock("terminal.read", 30),
		ReadPreview:     readBlock("preview.read", 45),
		DrivePreview:    func(payload map[string]any) string { return previewActionRequest(ctx, sid, copyDict(payload)) },
		ReadWindowBelow: func() string { return askRequest(ctx, "window.read", sid, map[string]any{}, &windowTimeout) },
		Connection:      func(payload map[string]any) { emitEvent(ctx, "connection.request", sid, copyDict(payload)) },
		Tour:            func(payload map[string]any) string { return tourRequest(ctx, sid, payload) },
		Theme:           func(payload map[string]any) any { return themeRequest(ctx, sid, payload) },
		SetupBeat:       func(payload map[string]any) any { return setupBeatRequest(ctx, sid, payload) },
	}
	if loadInterimAssistantMessages(ctx) {
		cbs.InterimAssistant = func(text any, alreadyStreamed bool) {
			emitEvent(ctx, "message.interim", sid, map[string]any{"text": pyrepr.Str(text), "already_streamed": alreadyStreamed})
		}
	}
	return cbs
}

// Copies the callbacks into agent construction options
func (c agentCallbackSet) applyTo(o *agent.AIAgentOptions) {
	o.ToolStartCallback, o.ToolCompleteCallback, o.ToolResultMetadataCallback = c.ToolStart, c.ToolComplete, c.ToolResultMetadata
	o.ToolProgressCallback, o.ToolGenCallback, o.ThinkingCallback, o.ReactionCallback = c.ToolProgress, c.ToolGen, c.Thinking, c.Reaction
	o.ReasoningCallback, o.StatusCallback, o.NoticeCallback, o.NoticeClearCallback = c.Reasoning, c.Status, c.Notice, c.NoticeClear
	o.ClarifyCallback, o.ReadTerminalCallback, o.ReadPreviewCallback, o.DrivePreviewCallback = c.Clarify, c.ReadTerminal, c.ReadPreview, c.DrivePreview
	o.ReadWindowBelowCallback, o.ConnectionCallback, o.TourCallback = c.ReadWindowBelow, c.Connection, c.Tour
	o.ThemeCallback, o.SetupBeatCallback = c.Theme, c.SetupBeat
	if c.InterimAssistant != nil {
		o.InterimAssistantCallback = c.InterimAssistant
	}
}
