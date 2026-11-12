package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Config key of a platform, cli for local
func platformConfigKey(platform gateway.Platform) string {
	if platform == gateway.PlatformLocal {
		return "cli"
	}
	return string(platform)
}

// Whether a steered follow up gets its ack bubble, the environment overriding the display setting
func (r *GatewayRunner) busySteerAckEnabled(ctx context.Context, event *gateway.MessageEvent, sessionKey string) bool {
	var enabled bool
	if env, ok := os.LookupEnv("ICEBATH_GATEWAY_BUSY_STEER_ACK_ENABLED"); ok {
		switch strings.ToLower(pystr.Strip(env)) {
		case "1", "true", "yes", "on":
			enabled = true
		}
	} else {
		enabled = pyrepr.Truthy(config.ResolveDisplaySetting(ctx, gateway.LoadGatewayConfigMap(ctx, ""),
			platformConfigKey(event.Source.Platform), "busy_steer_ack_enabled", true))
	}
	if !enabled {
		runLog().Debug(fmt.Sprintf("Busy steer ack suppressed for session %s", sessionKey))
	}
	return enabled
}

// Ack tail explaining an interrupt demoted to queue
func busyDemotedTail(ctx context.Context) string {
	return i18n.T(ctx, "gateway.busy.demoted_tail", "", nil)
}

// Iteration, elapsed time and tool detail of the running turn for the busy ack, stopping at a failed read
func busyAckStatusParts(ctx context.Context, now float64, busyState *SessionState, runningAgent *agent.AIAgent) (parts []string) {
	defer func() { _ = recover() }()
	summary := runningAgent.GetActivitySummary()
	elapsedMin := 0
	if startedTS := busyState.turnStartedTS(); startedTS != 0 {
		elapsedMin = int((now - startedTS) / 60)
	}
	if elapsedMin > 0 {
		parts = append(parts, i18n.T(ctx, "gateway.busy.elapsed_min", "", map[string]any{"minutes": elapsedMin}))
	}
	if pyrepr.Truthy(pyrepr.Or(summary["max_iterations"], 0)) {
		progress, err := agent.FormatIterationProgress(ctx, pyrepr.Or(summary["api_call_count"], 0), pyrepr.Or(summary["max_iterations"], 0))
		if err != nil {
			return parts
		}
		parts = append(parts, progress)
	}
	if tool := summary["current_tool"]; pyrepr.Truthy(tool) {
		parts = append(parts, i18n.T(ctx, "gateway.busy.running_tool", "", map[string]any{"tool": tool}))
	}
	return parts
}

// Modes the busy ack reports and the demotions it explains
type busyAckModes struct {
	steer, queue, redirect                     bool
	demotedForSubagents, demotedForCompression bool
}

// Busy ack text with the optional status detail and the one time busy input onboarding hint
func (r *GatewayRunner) composeBusyAckMessage(ctx context.Context, event *gateway.MessageEvent, now float64, busyState *SessionState, runningAgent *agent.AIAgent, modes busyAckModes) string {
	cfg := gateway.LoadGatewayConfigMap(ctx, "")
	var statusParts []string
	detailEnabled := pyrepr.Truthy(config.ResolveDisplaySetting(ctx, cfg, platformConfigKey(event.Source.Platform), "busy_ack_detail", true))
	if detailEnabled && runningAgent != nil && runningAgent != agentPendingSentinel {
		statusParts = busyAckStatusParts(ctx, now, busyState, runningAgent)
	}
	statusDetail := ""
	if len(statusParts) > 0 {
		statusDetail = i18n.T(ctx, "gateway.busy.ack_detail_wrap", "", map[string]any{
			"detail": strings.Join(statusParts, i18n.T(ctx, "gateway.busy.ack_detail_joiner", "", nil))})
	}
	t := func(key string) string { return i18n.T(ctx, key, "", nil) }
	var head, tail string
	switch {
	case modes.steer && agentHasActiveSubagents(runningAgent):
		head, tail = t("gateway.busy.steered_subagents_head"), t("gateway.busy.steered_subagents_tail")
	case modes.steer:
		head, tail = t("gateway.busy.steered_head"), t("gateway.busy.steered_tail")
	case modes.redirect:
		head, tail = t("gateway.busy.redirected_head"), t("gateway.busy.redirected_tail")
	case modes.queue && modes.demotedForSubagents:
		head, tail = t("gateway.busy.subagent_working_head"), busyDemotedTail(ctx)
	case modes.queue && modes.demotedForCompression:
		head, tail = t("gateway.busy.compressing_head"), busyDemotedTail(ctx)
	case modes.queue:
		head, tail = t("gateway.busy.queued_head"), t("gateway.busy.queued_tail")
	default:
		head, tail = t("gateway.busy.interrupting_head"), t("gateway.busy.interrupting_tail")
	}
	message := i18n.T(ctx, "gateway.busy.ack", "", map[string]any{"head": head, "detail": statusDetail, "tail": tail})
	if !tools.IsSeen(cfg, tools.OnboardingBusyInputFlag) {
		hintMode := "interrupt"
		switch {
		case modes.steer:
			hintMode = "steer"
		case modes.queue:
			hintMode = "queue"
		case modes.redirect:
			hintMode = "redirect"
		}
		message = message + "\n\n" + tools.BusyInputHintGateway(hintMode)
		tools.MarkSeen(ctx, filepath.Join(home.Process(), "config.yaml"), tools.OnboardingBusyInputFlag)
	}
	return message
}

// Sends the busy ack, logging a failed send at debug
func (r *GatewayRunner) sendBusyAckReply(ctx context.Context, event *gateway.MessageEvent, adapter gateway.PlatformAdapter, message string) {
	if err := r.sendBusyReply(ctx, event, adapter, message, false); err != nil {
		runLog().Debug(fmt.Sprintf("Failed to send busy-ack: %s", err))
	}
}

// Takes a message arriving while its session runs, true when handled here and false for the default queueing path
func (r *GatewayRunner) handleActiveSessionBusyMessage(ctx context.Context, event *gateway.MessageEvent, sessionKey string) (bool, error) {
	if event.Internal && event.AllowGatewayControl {
		adapter, err := r.deliveryAdapterFor(ctx, event.Source)
		if err != nil {
			return false, err
		}
		if adapter != nil {
			if _, held := adapter.Base().PeekPendingMessage(sessionKey); held {
				return true, r.queueOrReplacePendingEvent(ctx, sessionKey, event)
			}
		}
		return false, nil
	}
	authorized, err := r.isUserAuthorizedForSource(ctx, event.Source, true)
	if err != nil {
		return false, err
	}
	if !authorized {
		platform := "unknown"
		if event.Source.Platform != "" {
			platform = string(event.Source.Platform)
		}
		runLog().Warn(fmt.Sprintf("Dropping message from unauthorized user in active session: user=%s (%s), platform=%s, session=%s",
			pyrepr.Str(derefOrNone(event.Source.UserID)), pyrepr.Str(derefOrNone(event.Source.UserName)), platform, sessionKey))
		return true, nil
	}
	admitted, err := r.admitBotMessageForSource(ctx, event.Source)
	if err != nil {
		return false, err
	}
	if !admitted {
		return true, nil
	}
	event.SetBotLoopAdmitted(true)
	effectiveMode := r.effectiveBusyInputMode(ctx, event.Source)
	if r.draining.Load() {
		return true, r.sendBusyDrainNotice(ctx, event, sessionKey, effectiveMode)
	}
	if r.routePlaintextApprovalWhileBusy(ctx, event, sessionKey) {
		return true, nil
	}
	adapter, err := r.deliveryAdapterFor(ctx, event.Source)
	if err != nil || adapter == nil {
		return false, err
	}
	skill, skillReply, err := r.busySkillTurn(ctx, event, event.Source, sessionKey, adapter)
	if err != nil {
		return false, err
	}
	if skill {
		if pyrepr.Truthy(skillReply) {
			if err := r.sendBusyReply(ctx, event, adapter, pyrepr.Str(skillReply), false); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	if event.Internal {
		return true, r.queueOrReplacePendingEvent(ctx, sessionKey, event)
	}
	if event.MessageType == gateway.MessageTypeText && r.effectiveBusyTextMode(ctx, event.Source) == "queue" && effectiveMode != "steer" {
		return false, nil
	}
	busyState := r.peekSessionState(sessionKey)
	runningAgent := busyState.runningAgent()
	outcome, err := r.resolveBusySteerOrRedirect(ctx, event, sessionKey, effectiveMode, runningAgent)
	if err != nil {
		return false, err
	}
	if !outcome.steered && !outcome.redirected {
		if err := r.queueOrReplacePendingEvent(ctx, sessionKey, event); err != nil {
			return false, err
		}
	}
	return true, r.busyInterruptAndAck(ctx, event, sessionKey, adapter, busyState, runningAgent, outcome)
}

// Interrupt mode abort and the debounced busy ack after the steer or redirect resolution
func (r *GatewayRunner) busyInterruptAndAck(ctx context.Context, event *gateway.MessageEvent, sessionKey string, adapter gateway.PlatformAdapter, busyState *SessionState, runningAgent *agent.AIAgent, outcome busySteerOutcome) error {
	mode, redirected := outcome.effectiveMode, outcome.redirected
	isQueueMode, isSteerMode := mode == "queue", mode == "steer"
	isRedirectMode := mode == "interrupt" && redirected
	if mode == "interrupt" && !redirected && runningAgent != nil && runningAgent != agentPendingSentinel {
		r.interruptRunningAgentForBusyEvent(ctx, event, adapter, runningAgent)
	}
	ackEnabled := "true"
	if env, ok := os.LookupEnv("ICEBATH_GATEWAY_BUSY_ACK_ENABLED"); ok {
		ackEnabled = env
	}
	if strings.ToLower(ackEnabled) != "true" {
		runLog().Debug(fmt.Sprintf("Busy ack suppressed for session %s", sessionKey))
		return nil
	}
	now := wallClock()
	lastAck := 0.0
	if busyState != nil {
		busyState.mu.Lock()
		lastAck = busyState.turn.busyAckTS
		busyState.mu.Unlock()
	}
	if now-lastAck < 30 {
		return nil
	}
	if isSteerMode && !r.busySteerAckEnabled(ctx, event, sessionKey) {
		return nil
	}
	claimed := false
	r.withSessionState(sessionKey, func(s *SessionState) {
		if now-s.turn.busyAckTS >= 30 {
			s.turn.busyAckTS, claimed = now, true
		}
	})
	if !claimed {
		return nil
	}
	message := r.composeBusyAckMessage(ctx, event, now, busyState, runningAgent, busyAckModes{
		steer: isSteerMode, queue: isQueueMode, redirect: isRedirectMode,
		demotedForSubagents: outcome.demotedForSubagents, demotedForCompression: outcome.demotedForCompression})
	r.sendBusyAckReply(ctx, event, adapter, message)
	return nil
}

// Pointer value or nil for the Python None of an unset optional field
func derefOrNone(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}
