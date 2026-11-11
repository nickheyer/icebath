package gatewayrun

import (
	"context"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Discord adapter that speaks a one time acknowledgement in a guild voice channel
type voiceAckPlayer interface {
	PlayAckInVoice(ctx context.Context, guildID int, phrase *string) (bool, error)
}

// Tool lifecycle callback of the agent feeding the progress queue, the log queue and the live status
func (t *TurnRunner) progressCallback(eventType, toolName, preview string, args map[string]any, kwargs map[string]any) {
	tc := t.ctx
	ctx := t.turnCtx
	if kwargs == nil {
		kwargs = map[string]any{}
	}
	if localViewerTurn(t) {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug("Local viewer progress publish failed for "+eventType, "error", rec)
				}
			}()
			if err := publishLocalProgress(ctx, t, eventType, toolName, preview, kwargs); err != nil {
				runLog().Debug("Local viewer progress publish failed for "+eventType, "error", err)
			}
		}()
	}
	if eventType == "subagent.complete" {
		var previewArg any
		if preview != "" {
			previewArg = preview
		}
		t.progressSubagentNotice(previewArg, kwargs)
		return
	}
	t.progressLiveStatus(eventType, toolName, args)
	if tc.logQueue != nil && eventType == "tool.started" && toolName != "" && toolName != "_thinking" {
		ts := time.Now().Format("2006-01-02 15:04:05")
		previewStr := ""
		if preview != "" {
			previewStr = ` "` + preview + `"`
		}
		tc.logQueue.put(strings.TrimRight(ts+"  "+toolName+":"+previewStr, " \t\n\r\v\f"))
	}
	if tc.progressQueue == nil || !tc.runStillCurrent() {
		return
	}
	if eventType == "tool.completed" && !tc.longToolHintFired {
		t.progressOnboardingHint(kwargs)
		return
	}
	if eventType == "_thinking" || toolName == "_thinking" {
		thinkingText := ""
		if tc.thinkingEnabled {
			if toolName == "_thinking" {
				thinkingText = preview
			} else {
				thinkingText = toolName
			}
		}
		if thinkingText != "" {
			tc.progressQueue.put(i18n.T(ctx, "gateway.progress.thinking_prefix", "", map[string]any{"text": thinkingText}))
		}
		return
	}
	if tc.nativeSlackTaskCards && (eventType == "tool.started" || eventType == "tool.completed") {
		return
	}
	if !tc.toolProgressEnabled || eventType != "tool.started" || toolName == "clarify" || t.agentInterrupted() {
		return
	}
	if tc.progressMode == "new" && tc.lastTool != nil && toolName == *tc.lastTool {
		return
	}
	name := toolName
	tc.lastTool = &name
	if msg := t.progressBuildMessage(toolName, preview, args); msg != nil {
		t.progressEmit(*msg)
	}
}

// Start callback speaking a one time acknowledgement in the voice channel bound to this chat
func (t *TurnRunner) voiceAckCallback(callID, toolName string, args map[string]any) {
	tc := t.ctx
	if tc.voiceAckFired || tc.voiceAckGuild == nil || !tc.runStillCurrent() {
		return
	}
	tc.voiceAckFired = true
	adapter, err := t.runner.deliveryAdapterFor(t.turnCtx, tc.source)
	if err != nil {
		panic(err)
	}
	player, ok := adapter.(voiceAckPlayer)
	if adapter == nil || !ok {
		return
	}
	guild := *tc.voiceAckGuild
	t.scheduleOn(tc.voiceAckLoop, func(ctx context.Context) (any, error) {
		return player.PlayAckInVoice(ctx, guild, nil)
	}, "voice ack scheduling error")
}

// Whether a native task card event may queue, a live unstopped run with a progress queue
func (t *TurnRunner) nativeCardGate() bool {
	tc := t.ctx
	return tc.progressQueue != nil && tc.runStillCurrent() && !t.agentInterrupted()
}

// Queues an id correlated native task card start from the agent thread
func (t *TurnRunner) nativeToolStartCallback(callID, toolName string, args map[string]any) {
	if !t.nativeCardGate() {
		return
	}
	name := toolName
	if name == "" {
		name = "tool"
	}
	if args == nil {
		args = map[string]any{}
	}
	maxLen := 64
	t.ctx.progressQueue.put(map[string]any{"type": "tool.started", "tool_call_id": callID, "tool_name": name,
		"preview": tools.BuildToolPreview(t.toolsEnv(), name, args, &maxLen)})
}

// Queues the matching native task card completion under the real tool call id
func (t *TurnRunner) nativeToolCompleteCallback(callID, toolName string, args map[string]any, result string) {
	if !t.nativeCardGate() {
		return
	}
	name := toolName
	if name == "" {
		name = "tool"
	}
	isError, _ := tools.DetectToolFailure(t.turnCtx, name, result)
	t.ctx.progressQueue.put(map[string]any{"type": "tool.completed", "tool_call_id": callID, "tool_name": name, "is_error": isError})
}

// Composes the local viewer, execution stream, API observer, voice ack and task card start consumers
func (t *TurnRunner) combinedToolStartCallback(callID, toolName string, args map[string]any) {
	if localViewerTurn(t) {
		name := toolName
		noteLocalToolStart(t, callID, &name, args)
	}
	payload, _ := pyjson.Plain(gateway.ToolLifecyclePayload(callID, toolName, args)).(map[string]any)
	t.publishExecution("tool.start", payload)
	t.publishAPITool("tool.start", callID, toolName, args, nil)
	if t.ctx.voiceAckGuild != nil {
		t.voiceAckCallback(callID, toolName, args)
	}
	if t.ctx.nativeSlackTaskCards {
		t.nativeToolStartCallback(callID, toolName, args)
	}
}

// Composes the execution stream, local viewer, API observer and task card completion consumers
func (t *TurnRunner) combinedToolCompleteCallback(callID, toolName string, args map[string]any, result string) {
	ctx := t.turnCtx
	isError, _ := tools.DetectToolFailure(ctx, toolName, result)
	verbose := false
	if owner := t.approvalOwner; owner != nil {
		if live, ok := owner.authority.Live(owner.sessionID); ok && live != nil {
			mode := live.ToolProgressMode
			if mode == "" {
				mode = t.ctx.progressMode
			}
			verbose = mode == "verbose"
		}
	}
	payload := gateway.ToolCompletePayload(ctx, callID, toolName, args, result, &isError, verbose)
	local := localViewerTurn(t)
	if local {
		name := toolName
		fields := localCompleteFields(ctx, t, callID, &name, args, result)
		for _, key := range fields.Keys {
			payload.Set(key, fields.Vals[key])
		}
	}
	plain, _ := pyjson.Plain(payload).(map[string]any)
	t.publishExecution("tool.complete", plain)
	if todos, ok := plain["todos"].([]any); local && ok {
		revision, has := plain["revision"]
		if !has {
			revision = 0
		}
		t.publishExecution("todo.updated", map[string]any{"todos": todos, "revision": revision})
	}
	t.publishAPITool("tool.complete", callID, toolName, args, result)
	if local {
		publishLocalControlIfChanged(t)
	}
	if t.ctx.nativeSlackTaskCards {
		t.nativeToolCompleteCallback(callID, toolName, args, result)
	}
}

// Step callback publishing the agent step, the usage frame and the agent step hook
func (t *TurnRunner) stepCallbackSync(iteration int, prevTools []map[string]any) {
	tc := t.ctx
	if !tc.runStillCurrent() {
		return
	}
	ctx := t.turnCtx
	t.publishExecution("agent.step", map[string]any{"iteration": iteration})
	if iteration > 1 && localViewerTurn(t) {
		publishUsage(ctx, t, tc.currentAgent(), false)
	}
	loaded, err := tc.hooksRef.LoadedHooks(ctx)
	if err != nil {
		panic(err)
	}
	if len(loaded) == 0 {
		return
	}
	names := make([]any, 0, len(prevTools))
	toolsList := make([]any, 0, len(prevTools))
	for _, tool := range prevTools {
		name, _ := tool["name"].(string)
		names = append(names, name)
		toolsList = append(toolsList, tool)
	}
	platform := ""
	if tc.source.Platform != "" {
		platform = string(tc.source.Platform)
	}
	hookContext := map[string]any{"platform": platform, "user_id": tc.source.UserID, "session_id": tc.sessionID,
		"iteration": iteration, "tool_names": names, "tools": toolsList}
	hooks := tc.hooksRef
	t.schedule(func(ctx context.Context) (any, error) {
		return nil, hooks.emit(ctx, "agent:step", hookContext)
	}, "agent:step hook scheduling error")
}
