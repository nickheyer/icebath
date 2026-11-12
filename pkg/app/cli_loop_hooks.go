package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// cliCommandState holds the slash command, loop and model switch state of the interactive CLI
type cliCommandState struct {
	loopManager          agent.LoopAutomation
	lastLoopTickCheck    float64
	lastGoalBarrierCheck float64
	// Heartbeat manager of the current session and whether its idle watchdog runs
	heartbeatManager         agent.HeartbeatAutomation
	heartbeatWatchdogStarted bool
	// Marks the next process command call as an internal re entry the user's metrics already counted
	slashRedispatch bool
	// Model switch staging of the current confirm and apply sequence
	confirmedSwitchWarning  string
	oneTurnSelectionContext *provider.SelectionContext
	modelSwitchOneTurn      bool
}

// Seconds since the epoch as a float, the time.time reading of the idle hooks
func unixSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Copy of the queued items in FIFO order
func (q *inputQueue) snapshot() []any {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]any(nil), q.items...)
}

// Loop manager bound to the current session id, the gateway's on a gateway store and none before its session opens
func (c *CLI) getLoopManager() agent.LoopAutomation {
	return automationManager(c, &c.loopManager,
		func(sid string) agent.LoopAutomation { return agent.NewLoopManager(c.ctx, sid) },
		func(sid string, connection *gatewaycli.GatewayConnection) (agent.LoopAutomation, error) {
			return NewGatewayLoopManager(c.ctx, sid, connection)
		})
}

// Whether the session's goal defers this loop's tick, asked of the gateway when the manager is the gateway's
func (c *CLI) goalBlocksLoopTick(mgr agent.LoopAutomation) bool {
	if remote, isRemote := mgr.(interface{ GoalBlocksTick() bool }); isRemote {
		return remote.GoalBlocksTick()
	}
	return agent.GoalBlocksLoopTick(c.ctx, mgr.Session())
}

// Prints a manager decision message indented, true when one was printed
func printDecisionMessage(message string) bool {
	if message != "" {
		cprint("  " + message)
	}
	return message != ""
}

// Queues the continuation of a parked goal whose barrier lifted, checked at most every five seconds
func (c *CLI) maybeResumeParkedGoal() {
	now := unixSeconds()
	if now-c.lastGoalBarrierCheck < 5.0 {
		return
	}
	c.lastGoalBarrierCheck = now
	defer func() {
		if r := recover(); r != nil {
			cliLogger.Debug(fmt.Sprintf("parked-goal resume check failed: %v", r))
		}
	}()
	if !c.pendingInput.Empty() {
		return
	}
	mgr := c.getGoalManager()
	if mgr == nil {
		return
	}
	state := mgr.State()
	if state == nil || state.Status != "active" {
		return
	}
	if state.WaitingOnPID == nil && state.WaitingOnSession == nil && state.WaitingUntil == 0 {
		return
	}
	if mgr.IsWaiting() {
		return
	}
	if prompt := mgr.NextContinuationPrompt(); prompt != nil && *prompt != "" {
		cprint("  " + dimANSI + c.t("cli.goal.barrier_lifted", nil) + resetANSI)
		c.pendingInput.Put(*prompt)
	}
}

// Fires a due loop wakeup while idle with nothing queued and no unparked goal, checked every two seconds
func (c *CLI) maybeFireLoopTick() {
	mgr := c.getLoopManager()
	if mgr == nil || !mgr.IsDue() {
		return
	}
	now := unixSeconds()
	if now-c.lastLoopTickCheck < 2.0 {
		return
	}
	c.lastLoopTickCheck = now
	if !c.pendingInput.Empty() {
		return
	}
	if blocked := func() (blocked bool) {
		defer func() {
			if recover() != nil {
				blocked = false
			}
		}()
		return c.goalBlocksLoopTick(mgr)
	}(); blocked {
		return
	}
	wakeup, ok := mgr.FireTick(c.ctx)
	if !ok || wakeup == "" {
		return
	}
	if !c.injectLoopWakeup(mgr, wakeup) {
		return
	}
	if strings.HasPrefix(pystr.Lstrip(wakeup), "/") {
		func() {
			defer func() { _ = recover() }()
			printDecisionMessage(mgr.CompleteTick(c.ctx, "").Message)
		}()
	}
}

// Announces and queues a fired wakeup, rolling the tick back when the injection fails
func (c *CLI) injectLoopWakeup(mgr agent.LoopAutomation, wakeup string) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			cliLogger.Debug(fmt.Sprintf("loop tick injection failed: %v", r))
			func() {
				defer func() { _ = recover() }()
				mgr.AbandonTick(c.ctx)
			}()
			ok = false
		}
	}()
	var tickNo any = "?"
	if state := mgr.State(); state != nil {
		tickNo = state.TicksFired
	}
	cprint("  " + dimANSI + c.t("cli.loop.wakeup_firing", map[string]any{"tick": tickNo}) + resetANSI)
	c.pendingInput.Put(wakeup)
	return true
}

// Text of the newest assistant message with multimodal text parts joined, empty when there is none
func (c *CLI) lastAssistantResponseText() (text string) {
	defer func() {
		if recover() != nil {
			text = ""
		}
	}()
	for i := len(c.conversationHistory) - 1; i >= 0; i-- {
		msg := c.conversationHistory[i]
		if msg["role"] != "assistant" {
			continue
		}
		content, present := msg["content"]
		if !present {
			return ""
		}
		if parts, ok := content.([]any); ok {
			var texts []string
			for _, part := range parts {
				p, ok := part.(map[string]any)
				if !ok || (p["type"] != "text" && p["type"] != "output_text") {
					continue
				}
				if t := pyrepr.Str(pyOrValue(p, "text", "")); t != "" {
					texts = append(texts, t)
				}
			}
			return strings.Join(texts, "\n")
		}
		if !pyrepr.Truthy(content) {
			return ""
		}
		return pyrepr.Str(content)
	}
	return ""
}

// Value of key in m or the default when the key is absent
func pyOrValue(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Evaluates a finished loop wakeup turn, pausing the loop when the user interrupted it
func (c *CLI) maybeCompleteLoopTickAfterTurn() {
	mgr := c.getLoopManager()
	if mgr == nil {
		return
	}
	state := mgr.State()
	if state == nil || !state.AwaitingResponse {
		return
	}
	if c.lastTurnInterrupted {
		func() {
			defer func() { _ = recover() }()
			_, _ = mgr.Pause(c.ctx, "user-interrupted (Ctrl+C)")
		}()
		cprint("  " + dimANSI + c.t("cli.loop.paused_interrupted", nil) + resetANSI)
		return
	}
	response := ""
	if !c.lastTurnFailed {
		response = c.lastAssistantResponseText()
	}
	decision := mgr.CompleteTick(c.ctx, response)
	if !printDecisionMessage(decision.Message) && decision.Status == "active" && mgr.State() != nil && c.subagentMonitor == nil {
		cprint("  " + dimANSI + c.t("cli.loop.remaining", map[string]any{"label": mgr.State().RemainingLabel()}) + resetANSI)
	}
}

// Reports a queued entry that is a real user message rather than a slash command
func pendingHasRealMessage(entries []any) bool {
	for _, e := range entries {
		if q, ok := e.(queuedInput); ok {
			e = q.Text
		}
		s, ok := e.(string)
		if !ok || !looksLikeSlashCommand(s) {
			return true
		}
	}
	return false
}

// Judges the active goal after a turn and queues its continuation, pausing it when the user interrupted
func (c *CLI) maybeContinueGoalAfterTurn() {
	mgr := c.getGoalManager()
	if mgr == nil || !mgr.IsActive() {
		return
	}
	if c.pendingInput != nil && !c.pendingInput.Empty() && pendingHasRealMessage(c.pendingInput.snapshot()) {
		return
	}
	if c.lastTurnInterrupted {
		func() {
			defer func() {
				if r := recover(); r != nil {
					cliLogger.Debug(fmt.Sprintf("goal pause-on-interrupt failed: %v", r))
				}
			}()
			mgr.Pause("user-interrupted (Ctrl+C)")
		}()
		cprint("  " + dimANSI + c.t("cli.goal.paused_interrupted", nil) + resetANSI)
		return
	}
	if c.lastTurnFailed {
		return
	}
	lastResponse := c.lastAssistantResponseText()
	if pystr.Strip(lastResponse) == "" {
		return
	}
	bgProcs, activeDeleg := c.goalTurnBackground()
	decision, err := mgr.EvaluateAfterTurn(lastResponse, bgProcs, activeDeleg)
	if err != nil {
		panic(err)
	}
	printDecisionMessage(decision.Message)
	if decision.ShouldContinue && decision.ContinuationPrompt != nil && *decision.ContinuationPrompt != "" {
		c.pendingInput.Put(*decision.ContinuationPrompt)
	}
}

// This session's background processes and the agent's active delegation count, nil processes when gathering fails
func (c *CLI) goalTurnBackground() (procs []map[string]any, active int) {
	defer func() {
		if recover() != nil {
			procs = nil
		}
	}()
	procs = goals.GatherBackgroundProcesses(c.ctx, "", c.sessionID)
	agentSession := ""
	if c.agent != nil {
		agentSession = c.agent.SessionID()
	}
	return procs, goals.CountActiveDelegations(agentSession)
}
