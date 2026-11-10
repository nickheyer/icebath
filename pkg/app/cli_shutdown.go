package app

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Keeps process local session id consumers aligned after CLI switches
func syncProcessSessionID(ctx context.Context, env *tools.Env, sessionID string) {
	tools.SetCurrentSessionID(ctx, env, sessionID)
}

// Flushes stdout and stderr before a hard exit
func flushLoggingAndStdio() {
	if f, ok := console().Out.(interface{ Sync() error }); ok {
		_ = f.Sync()
	}
	_ = os.Stdout.Sync()
	_ = os.Stderr.Sync()
}

// Returns the float value of an environment variable, the default when unset or unparseable
func floatEnv(name string, def float64) float64 {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return def
	}
	return f
}

// Returns ICEBATH_EXIT_WATCHDOG_S as seconds, 30 by default and 0 disabling the watchdog
func exitWatchdogTimeout() float64 { return floatEnv("ICEBATH_EXIT_WATCHDOG_S", 30.0) }

// Starts the timer that forces process exit once shutdown has begun and the leash runs out
func armExitWatchdog(ctx context.Context, timeoutS float64, fromSignal bool) {
	if timeoutS <= 0 {
		return
	}
	if os.Getenv("PYTEST_CURRENT_TEST") != "" {
		return
	}
	go func() {
		time.Sleep(time.Duration(timeoutS * float64(time.Second)))
		if fromSignal && cleanupRunning() {
			return
		}
		cliLogger.Warn(fmt.Sprintf("Exit watchdog fired after %.0fs — forcing process exit (a cleanup step or non-daemon thread is wedged).", timeoutS))
		flushLoggingAndStdio()
		tools.KillLiveForegroundProcesses(true)
		osExit(0)
	}()
}

// Runs the memory provider shutdown at the real session boundary with the agent's session messages
func shutdownAgentMemoryProvider(ctx context.Context, a *agent.AIAgent) {
	if a == nil {
		return
	}
	if mm, ok := a.MemoryManager().(interface{ FlushPending(time.Duration) bool }); ok && mm != nil {
		mm.FlushPending(10 * time.Second)
	}
	sid := a.SessionID()
	if sid == "" {
		sid = "<unknown>"
	}
	msgs := a.SessionMessages()
	cliLogger.Info(fmt.Sprintf("CLI cleanup calling memory shutdown for session %s with %d message(s)", sid, len(msgs)))
	a.ShutdownMemoryProvider(ctx, agent.SessionMessageDicts(msgs))
}

// Stops the CLI's wake word listener when this process owns one
func stopCLIWakeWord(ctx context.Context, env *tools.Env) {
	if cliWakeOwner != nil {
		media.StopListening(cliWakeOwner)
	}
}

// Interrupts every async delegation for CLI shutdown
func interruptAsyncDelegations() { tools.InterruptAllAsyncDelegations("CLI shutdown") }

// Shuts down every MCP server of the process
func shutdownMCPServers(ctx context.Context) { mcp.ShutdownMCPServers(ctx, nil, nil, 0) }

// Shuts down the cached auxiliary provider clients
func shutdownCachedAuxClients(ctx context.Context, env *tools.Env) {
	provider.ShutdownCachedClients(ctx)
}

// Cleans up every terminal environment of the process
func cleanupAllTerminals(ctx context.Context, env *tools.Env) { tools.CleanupAllEnvironments(env) }

// Runs the browser emergency cleanup cmd/icebath wires once the browser package lands
func cleanupAllBrowsers(ctx context.Context, env *tools.Env) { EmergencyCleanupAllBrowserSessions() }

var cleanupSteps = []func(context.Context, *tools.Env){stopCLIWakeWord, cleanupAllTerminals, cleanupAllBrowsers, shutdownCachedAuxClients}

// Reports whether shutdown should fire the session finalize hook for the id
func shouldEmitCleanupSessionFinalize(sessionID string) bool {
	sessionIDSetsMu.Lock()
	defer sessionIDSetsMu.Unlock()
	if sessionID != "" && handedOffSessionIDs[sessionID] {
		return false
	}
	if len(singleQueryFinalizeAttemptedSessionIDs) == 0 {
		return true
	}
	if sessionID == "" {
		return false
	}
	return !singleQueryFinalizeAttemptedSessionIDs[sessionID]
}

// Fires on_session_finalize and returns the plugin messages meant for the user, never failing
func notifySessionFinalizeMessages(ctx context.Context, sessionID, platform, reason string) (lines []string) {
	defer func() {
		if r := recover(); r != nil {
			lines = []string{}
		}
	}()
	var sid any
	if sessionID != "" {
		sid = sessionID
	}
	return agent.SessionEndMessages(agent.FinalizeSession(ctx, map[string]any{"session_id": sid, "platform": platform, "reason": reason}))
}

// Returns the agent and session id of a one shot run, the agent's id winning over the CLI's
func oneshotAgentAndSession(c *CLI) (*agent.AIAgent, string) {
	a := c.agent
	sid := ""
	if a != nil {
		sid = a.SessionID()
	}
	if sid == "" {
		sid = c.sessionID
	}
	return a, sid
}

// Fires the on_session_end hook for a turn cut short, never failing
func invokeInterruptedSessionEnd(ctx context.Context, a *agent.AIAgent, sessionID, reason string, extra map[string]any) {
	defer func() { _ = recover() }()
	kwargs := map[string]any{"session_id": sessionID, "completed": false, "interrupted": true, "reason": reason, "platform": "cli"}
	var model any
	if a != nil {
		model = a.Model()
		if p := a.Platform(); p != "" {
			kwargs["platform"] = p
		}
	}
	kwargs["model"] = model
	for k, v := range extra {
		kwargs[k] = v
	}
	plugins.InvokeHook(ctx, "on_session_end", kwargs)
}

// Interrupts the agent and fires on_session_end for an interrupted non interactive run
func emitInterruptedSessionEnd(c *CLI, reason string) {
	a, sessionID := oneshotAgentAndSession(c)
	if a == nil {
		return
	}
	a.Interrupt(strings.ReplaceAll(reason, "_", " "))
	if sessionHandedOff(sessionID) {
		return
	}
	if sessionID != "" {
		c.sessionID = sessionID
	}
	invokeInterruptedSessionEnd(c.ctx, a, sessionID, reason, map[string]any{
		"task_id": a.TaskID(), "turn_id": a.TurnID(), "api_request_id": a.CurrentAPIRequestID()})
}

// Prints the session finalize messages of a one shot run once per session id
func notifySingleQuerySessionFinalize(c *CLI, reason string) {
	a, sessionID := oneshotAgentAndSession(c)
	sessionIDSetsMu.Lock()
	attempted := singleQueryFinalizeAttemptedSessionIDs[sessionID]
	sessionIDSetsMu.Unlock()
	if attempted || sessionHandedOff(sessionID) {
		return
	}
	defer func() {
		sessionIDSetsMu.Lock()
		singleQueryFinalizeAttemptedSessionIDs[sessionID] = true
		sessionIDSetsMu.Unlock()
	}()
	platform := "cli"
	if a != nil && a.Platform() != "" {
		platform = a.Platform()
	}
	for _, message := range notifySessionFinalizeMessages(c.ctx, sessionID, platform, reason) {
		fmt.Fprintln(stderrWriter(), message)
	}
}

// Durably flushes and finalizes the one shot session row before exit, idempotent and never failing
func flushOneShotSessionStore(c *CLI) {
	a, sessionID := oneshotAgentAndSession(c)
	if a == nil || sessionID == "" || sessionHandedOff(sessionID) {
		return
	}
	if a.PersistDisabled() {
		return
	}
	if msgs := a.SessionMessages(); len(msgs) > 0 {
		if err := a.PersistSession(&msgs, agent.RowsToMessages(c.conversationHistory)); err != nil {
			cliLogger.Debug(fmt.Sprintf("one-shot final session persist retry failed: %v", err))
		}
	}
	db := a.SessionDB()
	if db == nil {
		db = c.sessionDB
	}
	if db == nil {
		return
	}
	if !db.FlushTokenCounts(5.0) {
		cliLogger.Debug("one-shot token-count drain failed")
	}
	if err := db.EndSession(sessionID, "cli_close"); err != nil {
		cliLogger.Debug(fmt.Sprintf("one-shot end_session failed: %v", err))
	}
}

// Lingers once, bounded, for notify_on_complete background processes unless the quiet loop already did
func waitForOneshotBackgroundCompletions(c *CLI) {
	if c.quietNotifyLingerDone {
		return
	}
	_, taskID := oneshotAgentAndSession(c)
	result := tools.DefaultProcessRegistry().WaitForPendingCompletions(c.ctx, c.env, "", nil, oneshotPendingCompletionsPollIntervalSec)
	if len(result.Waited) > 0 {
		if taskID == "" {
			taskID = "<unknown>"
		}
		cliLogger.Info(fmt.Sprintf("One-shot exit linger for session %s: completed=%v timed_out=%v", taskID, result.Completed, result.TimedOut))
	}
}

// Settles the session, releases its lease, then lingers as a bystander and runs cleanup
func finalizeSingleQuery(c *CLI) {
	func() {
		defer c.releaseActiveSession()
		flushOneShotSessionStore(c)
		notifySingleQuerySessionFinalize(c, "shutdown")
		a := c.agent
		if a == nil {
			a = currentActiveAgent()
		}
		shutdownAgentMemoryProvider(c.ctx, a)
	}()
	waitForOneshotBackgroundCompletions(c)
	runCleanup(c.ctx, c.env, false)
}
