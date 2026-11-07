package tools

import (
	"context"
	"fmt"
	"strings"
)

// Nudge for a silent background process that is almost always a bounded task missing its flag
const silentBackgroundHint = "background=true without notify_on_complete=true means this process runs SILENTLY \u2014 you " +
	"will not be told when it exits. If this is a bounded task (test suite, build, CI poller, " +
	"deploy, anything with a defined end), you almost certainly wanted notify_on_complete=true " +
	"so the system pings you on exit. Re-launch with notify_on_complete=true, or call " +
	"process(action='poll') / process(action='wait') yourself to learn the outcome. Only " +
	"ignore this hint for genuine long-lived processes that never exit (servers, watchers, " +
	"daemons)."

// Hint for a homebrewed CI poller shape known to fail silently
const homebrewCIPollerHint = "This looks like a homebrewed CI poller built from `gh pr view --json statusCheckRollup` " +
	"and/or `gh pr checks | jq`. That shape has burned us repeatedly in hermes-agent dev work " +
	"(PRs #31329, #31448, #31695, #31709, #31745, #32264, #33131) \u2014 stdout buffering kills " +
	"output capture, jq null-key edge cases silently exit the loop, conclusion-vs-status field " +
	"confusion exits early with bogus all-green verdicts, TTY-only summary banners never " +
	"appear when piped. Use the canonical snippets in the green-ci-policy skill instead: the " +
	"exit-code-driven `gh pr checks $PR >/dev/null` (rc 0 = green, 8 = pending, else fail) for " +
	"exit-on-first-fail behavior, or the column-2 awk-on-tabs poller (`awk -F\"\\t\" " +
	"\"$2==\\\"pending\\\"\"`) for sharded matrices. Load " +
	"skill_view(name='github/hermes-agent-dev', file_path='references/green-ci-policy.md') for " +
	"the verbatim snippets. If you must roll a custom loop with rich structured output, write " +
	"each tick to a known file (`tee -a $TMPDIR/ci.log`) and rely on `process(action='log')` to " +
	"read THAT file \u2014 do not rely on background-process stdout capture for line-buffered shell " +
	"loops."

// Note for a foreground call whose only backgrounding was a trailing ampersand
const autoBackgroundNote = "Ran in the background (the trailing & was converted to background=true; Icebath tracks the " +
	"process). Do NOT re-run it. Use process_manage (poll/log/wait/kill, session_id above) to check it."

// Note when the session cannot receive an async completion after the turn ends
const asyncUnsupportedNote = "notify_on_complete / watch_patterns are not available in this session \u2014 it cannot receive " +
	"an async completion after the turn ends (a one-shot runner such as `icebath -z`, a /bg " +
	"background task, a cron job, a Kanban worker, or a stateless HTTP endpoint). The process is running in the " +
	"background; retrieve its result with process(action='poll') or process(action='wait')."

// Note for a subagent whose completion notice cannot reach its parent
const subagentNotifyNote = "You are a subagent: this process's completion notice will NOT reach your parent, and the process is killed when " +
	"you finish. Before you finish, either wait for it (process_manage wait), kill it, or hand it to your parent with " +
	"process_manage(action='handoff', session_id=..., data='<purpose>') so the parent receives its completion. For CI " +
	"watchers prefer returning the fact (PR number, SHA) and letting the parent watch."

// Note for a foreground command moved to the background because the user sent a message
const yieldedNote = "The user sent a message while this command was running, so it was moved to the " +
	"background WITHOUT being killed and is still running. You will be notified when it " +
	"exits (notify_on_complete). Read the user's message and respond to it now; use " +
	"process(action='poll'|'wait'|'log', session_id=...) to check on this command."

// processSpawner is the process registry surface the terminal tool uses
type processSpawner interface {
	SpawnLocal(env *Env, command string, o SpawnLocalOptions) (*ProcessSession, error)
	SpawnViaEnv(ctx context.Context, env *Env, e Environment, command, cwd, taskID, sessionKey string, timeout int, ownerTaskID string, persistOnRelease bool) (*ProcessSession, error)
	AdoptLocal(env *Env, proc *ExecProcess, command, cwd, taskID, sessionKey, ownerTaskID, outputSoFar string, notifyOnComplete bool) *ProcessSession
	HasActiveProcesses(taskID string) bool
	AnyRunning(pred func(*ProcessSession) bool) bool
	ArmHeartbeat(s *ProcessSession, seconds int) int
	AddPendingWatcher(watcher map[string]any)
	RequestCloseTerminal(env *Env, sessionID string) map[string]any
}

// registryAdapter exposes the process wide registry through the spawner surface
type registryAdapter struct{ r *ProcessRegistry }

func (a registryAdapter) SpawnLocal(env *Env, command string, o SpawnLocalOptions) (*ProcessSession, error) {
	return a.r.SpawnLocal(env, command, o)
}

func (a registryAdapter) SpawnViaEnv(ctx context.Context, env *Env, e Environment, command, cwd, taskID, sessionKey string, timeout int, ownerTaskID string, persistOnRelease bool) (*ProcessSession, error) {
	return a.r.SpawnViaEnv(ctx, env, e, command, cwd, taskID, sessionKey, timeout, ownerTaskID, persistOnRelease)
}

func (a registryAdapter) AdoptLocal(env *Env, proc *ExecProcess, command, cwd, taskID, sessionKey, ownerTaskID, outputSoFar string, notifyOnComplete bool) *ProcessSession {
	return a.r.AdoptLocal(env, proc, command, cwd, taskID, sessionKey, ownerTaskID, outputSoFar, notifyOnComplete)
}

func (a registryAdapter) HasActiveProcesses(taskID string) bool {
	return a.r.HasActiveProcesses(taskID)
}

func (a registryAdapter) AnyRunning(pred func(*ProcessSession) bool) bool {
	return a.r.anyRunning(pred)
}

func (a registryAdapter) ArmHeartbeat(s *ProcessSession, seconds int) int {
	return a.r.ArmHeartbeat(s, seconds)
}

func (a registryAdapter) AddPendingWatcher(watcher map[string]any) {
	a.r.PendingWatchers = append(a.r.PendingWatchers, watcher)
}

func (a registryAdapter) RequestCloseTerminal(env *Env, sessionID string) map[string]any {
	return a.r.RequestCloseTerminal(env, sessionID)
}

// Registry seam, the process wide registry by default
var terminalRegistry = func() processSpawner { return registryAdapter{processRegistry} }

// ArmProcessWatcher hands a watcher to the live gateway loop, installed by the gateway, false when it is not serving
var ArmProcessWatcher func(watcher map[string]any) bool

type delegatedChildKey struct{}

// WithDelegatedChildContext marks a context as running for a delegate_task child
func WithDelegatedChildContext(ctx context.Context, active bool) context.Context {
	return context.WithValue(ctx, delegatedChildKey{}, active)
}

// IsDelegatedChildContext reports whether code runs for a delegate_task child
func IsDelegatedChildContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(delegatedChildKey{}).(bool)
	return v
}

// Reports whether the current session can deliver a background completion later
func asyncDeliverySupported(env *Env) bool {
	if kanbanTaskActive(env) || isWorkerProcess() {
		return false
	}
	if env != nil && env.Values != nil {
		if v, ok := env.Values.Load("session.async_delivery"); ok {
			if b, ok := v.(bool); ok {
				return b
			}
		}
	}
	return true
}

// Reports whether the bound session can receive async completion notices, the exported face for pkg/cron
func SessionAsyncDeliverySupported(env *Env) bool { return asyncDeliverySupported(env) }

func looksLikeHomebrewCIPoller(command string) bool {
	hasGh := strings.Contains(command, "gh pr view") || strings.Contains(command, "gh pr checks")
	hasJq := strings.Contains(command, " jq ") || strings.Contains(command, "| jq") || strings.Contains(command, "$(jq")
	return strings.Contains(command, "statusCheckRollup") || (hasGh && hasJq)
}

// Copies the spawning chat's routing metadata onto the process session
func stampGatewayRouting(env *Env, s *ProcessSession) {
	platform := sessionContextValue(env, "ICEBATH_SESSION_PLATFORM")
	if platform == "" {
		return
	}
	s.WatcherPlatform = platform
	s.WatcherChatID = sessionContextValue(env, "ICEBATH_SESSION_CHAT_ID")
	s.WatcherUserID = sessionContextValue(env, "ICEBATH_SESSION_USER_ID")
	s.WatcherUserName = sessionContextValue(env, "ICEBATH_SESSION_USER_NAME")
	s.WatcherThreadID = sessionContextValue(env, "ICEBATH_SESSION_THREAD_ID")
	s.WatcherMessageID = sessionContextValue(env, "ICEBATH_SESSION_MESSAGE_ID")
	s.ParentSessionID = sessionContextValue(env, "ICEBATH_SESSION_ID")
}

// Spawns the tracked process locally or through the sandbox environment
func spawnTracked(ctx context.Context, env *Env, e Environment, envType, command, cwd, effectiveTaskID, taskID, sessionKey string, effectivePty, persistOnRelease bool) (*ProcessSession, error) {
	owner := taskID
	if owner == "" {
		owner = effectiveTaskID
	}
	if envType == "local" {
		opts := SpawnLocalOptions{Cwd: cwd, TaskID: effectiveTaskID, OwnerTaskID: owner, SessionKey: sessionKey, PersistOnRelease: persistOnRelease, UsePty: effectivePty, RestoreLoginPath: true}
		if c, ok := e.(interface{ ChildEnv() map[string]string }); ok {
			opts.EnvVars = c.ChildEnv()
		}
		return terminalRegistry().SpawnLocal(env, command, opts)
	}
	return terminalRegistry().SpawnViaEnv(ctx, env, e, command, cwd, effectiveTaskID, sessionKey, 0, owner, persistOnRelease)
}

// Drops the notification flags on finite sessions and stamps routing otherwise
func applyAsyncSupport(env *Env, s *ProcessSession, result map[string]any, notifyOnComplete bool, watchPatterns []string) (bool, []string) {
	if !notifyOnComplete && len(watchPatterns) == 0 {
		return notifyOnComplete, watchPatterns
	}
	if asyncDeliverySupported(env) {
		stampGatewayRouting(env, s)
		return notifyOnComplete, watchPatterns
	}
	result["notify_on_complete"] = false
	result["notify_unsupported"] = asyncUnsupportedNote
	envLog(env).Info("background proc async delivery unsupported on this session, notify flags disabled", "session_id", s.ID)
	return false, nil
}

// Registers a fast completion watcher on the live gateway loop or parks it for the next drain
func registerCompletionWatcher(s *ProcessSession, sessionKey string) {
	s.WatcherInterval = 5
	watcher := map[string]any{
		"session_id": s.ID, "check_interval": 5, "session_key": sessionKey,
		"platform": s.WatcherPlatform, "chat_id": s.WatcherChatID, "user_id": s.WatcherUserID, "user_name": s.WatcherUserName,
		"thread_id": s.WatcherThreadID, "message_id": s.WatcherMessageID,
		"notify_on_complete": true, "parent_session_id": s.ParentSessionID,
	}
	if ArmProcessWatcher != nil && ArmProcessWatcher(watcher) {
		return
	}
	terminalRegistry().AddPendingWatcher(watcher)
}

// backgroundSpawnRequest carries every parameter of one background spawn
type backgroundSpawnRequest struct {
	command, envType, effectiveTaskID, taskID, sessionKey, workdir, cwd string
	effectivePty, notifyOnComplete                                      bool
	watchPatterns                                                       []string
	approvalNote, ptyDisabledReason                                     string
	completionOutputChars, heartbeatSeconds                             int
	persistOnRelease, autoBackgrounded                                  bool
	mountedHost                                                         string
}

// Spawns the command as a tracked background process and returns the JSON result
func spawnBackgroundProcess(ctx context.Context, env *Env, e Environment, req backgroundSpawnRequest) string {
	mounted := req.mountedHost
	if mounted == "" && e != nil {
		mounted = e.HostCwd()
	}
	effectiveCwd := resolveCommandCwd(env, req.workdir, req.cwd, req.sessionKey, req.envType, mounted, e)
	s, err := spawnTracked(ctx, env, e, req.envType, req.command, effectiveCwd, req.effectiveTaskID, req.taskID, req.sessionKey, req.effectivePty, req.persistOnRelease)
	if err != nil {
		return JSON(map[string]any{"output": "", "exit_code": -1,
			"error": redactTerminalErrorText(env, fmt.Sprintf("Failed to start background process: %s", err))})
	}
	result := map[string]any{"output": "Background process started", "session_id": s.ID, "pid": s.Pid, "exit_code": 0, "error": nil}
	if req.persistOnRelease {
		result["persist_on_release"] = true
	}
	if req.approvalNote != "" {
		result["approval"] = req.approvalNote
	}
	if req.ptyDisabledReason != "" {
		result["pty_note"] = req.ptyDisabledReason
	}
	if req.autoBackgrounded {
		result["auto_backgrounded"] = autoBackgroundNote
	} else if !req.notifyOnComplete && len(req.watchPatterns) == 0 {
		result["hint"] = silentBackgroundHint
	}
	if req.command != "" && looksLikeHomebrewCIPoller(req.command) {
		if existing, _ := result["hint"].(string); existing != "" {
			result["hint"] = existing + "\n\n" + homebrewCIPollerHint
		} else {
			result["hint"] = homebrewCIPollerHint
		}
	}
	notify, watch := applyAsyncSupport(env, s, result, req.notifyOnComplete, req.watchPatterns)
	watch, conflict := resolveNotificationFlagConflict(notify, watch, true)
	if conflict != "" {
		envLog(env).Warn("background proc flag conflict", "session_id", s.ID, "note", conflict)
		result["watch_patterns_ignored"] = conflict
	}
	if notify {
		s.NotifyOnComplete = true
		result["notify_on_complete"] = true
		if req.completionOutputChars != 0 {
			s.CompletionOutputChars = req.completionOutputChars
		}
		if s.WatcherPlatform != "" {
			registerCompletionWatcher(s, req.sessionKey)
		}
		if IsDelegatedChildContext(ctx) {
			result["notify_on_complete"] = false
			result["subagent_note"] = subagentNotifyNote
		} else if req.heartbeatSeconds != 0 {
			result["heartbeat_seconds"] = terminalRegistry().ArmHeartbeat(s, req.heartbeatSeconds)
		}
	} else if req.heartbeatSeconds != 0 {
		result["heartbeat_ignored"] = "heartbeat needs notify=true delivery, which this session cannot receive"
	}
	if len(watch) > 0 {
		s.WatchPatterns = append([]string(nil), watch...)
		result["watch_patterns"] = s.WatchPatterns
	}
	return JSON(result)
}

// Yield handler adopting the live local process into the registry when the host asks the tool to yield
func yieldToBackgroundHandler(ctx context.Context, env *Env, command, envType, cwd, effectiveTaskID, taskID, sessionKey string) func(ProcessHandle, string) *ExecResult {
	if envType != "local" {
		return nil
	}
	return func(proc ProcessHandle, outputSoFar string) *ExecResult {
		owner := taskID
		if owner == "" {
			owner = effectiveTaskID
		}
		ep, ok := proc.(*ExecProcess)
		if !ok {
			envLog(env).Warn("foreground command could not be handed to the background", "handle", fmt.Sprintf("%T", proc))
			return nil
		}
		s := terminalRegistry().AdoptLocal(env, ep, command, cwd, effectiveTaskID, sessionKey, owner, outputSoFar, true)
		stampRoutingIfGateway(env, s, sessionKey)
		envLog(env).Info("foreground command yielded to background", "session_id", s.ID, "pid", s.Pid)
		return &ExecResult{Output: outputSoFar, YieldedSessionID: s.ID, Pid: s.Pid}
	}
}

// Routes the adopted session's completion like a normal notify on complete spawn
func stampRoutingIfGateway(env *Env, s *ProcessSession, sessionKey string) {
	if !asyncDeliverySupported(env) {
		s.NotifyOnComplete = false
		return
	}
	stampGatewayRouting(env, s)
	if s.WatcherPlatform != "" {
		registerCompletionWatcher(s, sessionKey)
	}
}
