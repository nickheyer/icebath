package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Force redacts text before serializing a terminal error envelope
func redactTerminalErrorText(env *Env, value string) string {
	return RedactSensitiveTextWith(env, value, RedactOptions{Force: true})
}

// Hard cap on the foreground timeout, overridable through TERMINAL_MAX_FOREGROUND_TIMEOUT
var foregroundMaxTimeout = sync.OnceValue(func() int {
	raw := os.Getenv("TERMINAL_MAX_FOREGROUND_TIMEOUT")
	if raw == "" {
		return 600
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		envLog(nil).Warn("Invalid value for TERMINAL_MAX_FOREGROUND_TIMEOUT (expected integer). Falling back to 600.", "value", raw)
		return 600
	}
	return v
})

// ForegroundMaxTimeout is the cap on a foreground terminal timeout in seconds
func ForegroundMaxTimeout() int { return foregroundMaxTimeout() }

// Tool description shown to the model
const terminalToolDescription = `Run shell commands on the OS/shell/backend named in your environment section. The working directory, files and exported variables persist between calls: activate a virtualenv or export once, not before every command.

Use read_file, search_files, patch and write_file instead of cat/head/tail, grep/find/ls, sed/awk and echo/heredoc. Output is auto-truncated with the full text saved to a file; never pipe through head/tail to shorten it.

Foreground (default) returns as soon as the command finishes. To wait a fixed time, run the wait in the foreground; never background a sleep or polling loop. Background is only for commands that must keep running after this call returns; health-check a server in a separate call and manage jobs with process_manage. When a command changes directory, trust the result's "cwd" instead of prefixing later commands with cd.
`

// Reports whether PTY mode would break a stdin driven command
func commandRequiresPipeStdin(command string) bool {
	normalized := strings.Join(strings.Fields(strings.ToLower(command)), " ")
	return strings.HasPrefix(normalized, "gh auth login") && strings.Contains(normalized, "--with-token")
}

// Drops watch patterns when notify on complete is also set on a background call
func resolveNotificationFlagConflict(notifyOnComplete bool, watchPatterns []string, background bool) ([]string, string) {
	if background && notifyOnComplete && len(watchPatterns) > 0 {
		return nil, "watch_patterns ignored because notify_on_complete=True; " +
			"these two flags produce duplicate notifications when combined"
	}
	return watchPatterns, ""
}

// Container path for a host path when the env bind mounted that directory
func rewriteViaEnvMount(path string, e Environment) string {
	if e == nil || path == "" {
		return ""
	}
	host, mount := e.HostCwd(), e.HostCwdMount()
	if host == "" || mount == "" {
		return ""
	}
	return translateMountedHostPath(path, host, mount)
}

// The env itself or every cached env when none is given
func mountEnvs(e Environment) []Environment {
	if e != nil {
		return []Environment{e}
	}
	termPool.Lock()
	defer termPool.Unlock()
	out := make([]Environment, 0, len(termPool.active))
	for _, item := range termPool.active {
		out = append(out, item)
	}
	return out
}

// Path translated through a mount when a container backend bound it
func containerVisibleCwd(path, envType string, e Environment) string {
	if path == "" || !isContainerBackend(envType) {
		return path
	}
	for _, item := range mountEnvs(e) {
		if translated := rewriteViaEnvMount(path, item); translated != "" {
			return translated
		}
	}
	return path
}

// Points a planner workspace assumption at the mount that holds the host cwd
func containerVisibleDefault(defaultCwd, envType string, e Environment) string {
	if !isContainerBackend(envType) {
		return defaultCwd
	}
	for _, item := range mountEnvs(e) {
		mount, host := item.HostCwdMount(), item.HostCwd()
		if mount == "" || host == "" || mount == defaultCwd {
			continue
		}
		if defaultCwd == "/workspace" || isUnusableContainerCwd(defaultCwd, "") {
			return mount
		}
	}
	return defaultCwd
}

// Cwd for a command, explicit workdir over the session record over the default
func resolveCommandCwd(env *Env, workdir, defaultCwd, sessionKey, envType, mountedHost string, e Environment) string {
	if workdir != "" {
		return coerceSSHRemoteCwd(env, containerVisibleCwd(workdir, envType, e), envType)
	}
	recorded, _ := getSessionCwdFor(env, sessionKey)
	if recorded != "" && isContainerBackend(envType) && isUnusableContainerCwd(recorded, mountedHost) && !inSandboxHome(recorded, e) {
		if visible := containerVisibleCwd(recorded, envType, e); visible != recorded {
			return visible
		}
		if isMountedHostCwd(recorded, mountedHost) {
			envLog(env).Info("Remapping recorded session cwd for backend (mounted host directory). Using '/workspace' instead.", "cwd", recorded, "backend", envType)
			return "/workspace"
		}
		envLog(env).Info("Ignoring recorded session cwd for backend (host/relative path won't work in sandbox)", "cwd", recorded, "backend", envType, "using", defaultCwd)
		return containerVisibleDefault(defaultCwd, envType, e)
	}
	if recorded == "" {
		recorded = containerVisibleDefault(defaultCwd, envType, e)
	}
	return coerceSSHRemoteCwd(env, recorded, envType)
}

// Terminal error envelope with output, exit code, error, optional status and extras in order
func errorJSON(errText string, exitCode int, status string, extra [][2]any) string {
	fields := [][2]any{{"output", ""}, {"exit_code", exitCode}, {"error", errText}}
	if status != "" {
		fields = append(fields, [2]any{"status", status})
	}
	return orderedJSON(append(fields, extra...))
}

// Terminal error envelope with exit code minus one and the given status
func TerminalErrorJSON(errText, status string) string { return errorJSON(errText, -1, status, nil) }

// Redacted error plus stack envelope after logging the failure
func fatalErrorJSON(env *Env, err error) string {
	stack := string(debug.Stack()) + fmt.Sprintf("%T: %s\n", err, err)
	envLog(env).Error("terminal_tool exception", "error", err, "stack", stack)
	return orderedJSON([][2]any{
		{"output", ""}, {"exit_code", -1},
		{"error", redactTerminalErrorText(env, fmt.Sprintf("Failed to execute command: %s", err))},
		{"traceback", redactTerminalErrorText(env, stack)},
		{"status", "error"},
	})
}

// Finished tool result carried out of the planning and guard helpers
type terminalRejected struct{ resultJSON string }

func (r *terminalRejected) Error() string { return r.resultJSON }

// Test seams over the pipeline stages, the real functions by default
var (
	checkAllGuardsFn       = CheckAllCommandGuards
	startCleanupThreadFn   = startCleanupThread
	planExecutionFn        = planExecution
	acquireEnvFn           = acquireEnv
	runApprovalGuardsFn    = runApprovalGuards
	preExecBlockFn         = preExecBlock
	runForegroundFn        = runForeground
	preExecGuardMinTimeout = preExecGuardMinTimeoutS * time.Second
)

// Outcome of the pre exec guard pass
type approvalVerdict struct {
	note        string
	approvedRun bool
}

// Runs the command guards, a prepared batch decision first, force skips them entirely
func runApprovalGuards(ctx context.Context, env *Env, command, envType string, cfg map[string]any, force bool) (approvalVerdict, error) {
	if force {
		return approvalVerdict{approvedRun: true}, nil
	}
	hasHostAccess := dockerHasHostAccessConfig(cfg)
	var approval GuardResult
	consumed := false
	if slot := terminalSlotOf(ctx); slot != nil {
		approval, consumed = slot.ConsumePreparedGuard(ctx, env, command, envType, hasHostAccess)
	}
	if !consumed {
		approval = checkAllGuardsFn(ctx, env, command, envType, hasHostAccess)
	}
	markTerminalBatchOutcome(ctx, !approval.Approved)
	if !approval.Approved {
		if approval.Status == "pending_approval" {
			cmd := approval.Command
			if cmd == "" {
				cmd = command
			}
			desc := approval.Description
			if desc == "" {
				desc = "command flagged"
			}
			allowPermanent := approval.AllowPermanent || approval.PatternKey == "" && !approval.SmartDenied
			return approvalVerdict{}, &terminalRejected{errorJSON("", -1, "pending_approval", [][2]any{
				{"approval_pending", true}, {"command", cmd}, {"description", desc},
				{"pattern_key", approval.PatternKey}, {"smart_denied", approval.SmartDenied}, {"allow_permanent", allowPermanent},
			})}
		}
		desc := approval.Description
		if desc == "" {
			desc = "command flagged"
		}
		msg := approval.Message
		if msg == "" {
			msg = fmt.Sprintf("Command denied: %s. Use the approval prompt to allow it, or rephrase the command.", desc)
		}
		var extra [][2]any
		if approval.UserSummary != "" {
			extra = append(extra, [2]any{"user_summary", approval.UserSummary})
		}
		return approvalVerdict{}, &terminalRejected{errorJSON(msg, -1, "blocked", extra)}
	}
	desc := approval.Description
	if desc == "" {
		desc = "flagged as dangerous"
	}
	if approval.UserApproved {
		return approvalVerdict{note: fmt.Sprintf("Command required approval (%s) and was approved by the user.", desc), approvedRun: true}, nil
	}
	if approval.SmartApproved {
		return approvalVerdict{note: fmt.Sprintf("Command was flagged (%s) and auto-approved by smart approval.", desc)}, nil
	}
	return approvalVerdict{}, nil
}

// Per call execution parameters resolved before any environment is touched
type execPlan struct {
	config           map[string]any
	envType          string
	effectiveTaskID  string
	image            string
	cwd              string
	hostCwd          string
	effectiveTimeout int
	// Requested seconds when an over cap foreground timeout was promoted to a tracked background process
	promotedFromForegroundTimeout int
	// Command to run when a foreground trailing ampersand call runs as background instead
	autoBackgroundCommand string
	autoBackground        bool
}

const promotedNote = "Requested foreground timeout {requested}s exceeds the {cap}s cap, so this command was started as a " +
	"tracked background process with notify_on_complete=true instead of being refused. Do NOT re-run it. " +
	"Its completion (exit code + output tail) arrives as a notification; poll with " +
	"process(action=\"poll\", session_id=...) if you need it sooner."

const promotedNotePollOnly = "Requested foreground timeout {requested}s exceeds the {cap}s cap, so this command was started as a " +
	"tracked background process instead of being refused. Do NOT re-run it. This session cannot receive " +
	"completion notifications, so poll it with process(action=\"poll\", session_id=...) until it exits."

// TerminalRequest is one terminal tool invocation
type TerminalRequest struct {
	Command               any
	Background            bool
	Timeout               *int
	TaskID                string
	SessionID             string
	Force                 bool
	Workdir               string
	Pty                   bool
	NotifyOnComplete      bool
	WatchPatterns         []string
	HostLocal             bool
	CompletionOutputChars int
	Heartbeat             int
	PersistOnRelease      bool
}

// Resolves backend, env cache key, image, cwd and timeout for one call
func planExecution(env *Env, command any, taskID string, timeout *int, background, hostLocal bool) (*execPlan, error) {
	cmd, ok := command.(string)
	if !ok {
		envLog(env).Warn("Rejected invalid terminal command value", "type", fmt.Sprintf("%T", command))
		return nil, &terminalRejected{errorJSON(fmt.Sprintf("Invalid command: expected string, got %s", pyTypeName(command)), -1, "error", nil)}
	}
	cfg, err := getEnvConfig(env)
	if err != nil {
		return nil, err
	}
	envType, _ := cfg["env_type"].(string)
	if hostLocal {
		envType = "local"
	}
	effective, err := resolveContainerTaskID(env, taskID)
	if err != nil {
		return nil, err
	}
	if hostLocal {
		effective = "host-local-" + effective
	}
	overrides, err := ResolveTaskOverrides(env, taskID)
	if err != nil {
		return nil, err
	}
	image := selectImage(envType, overrides, cfg)
	cwd, _ := overrides["cwd"].(string)
	if cwd == "" {
		cwd, _ = getSessionCwdFor(env, taskID)
	}
	if cwd == "" {
		cwd, _ = cfg["cwd"].(string)
	}
	cwd = coerceSSHRemoteCwd(env, cwd, envType)
	hostCwd, err := resolveTaskHostCwd(env, cfg, taskID)
	if err != nil {
		return nil, err
	}
	if isContainerBackend(envType) && isUnusableContainerCwd(cwd, hostCwd) {
		remapped, _ := cfg["cwd"].(string)
		if hostCwd != "" {
			remapped = "/workspace"
		}
		if cwd != remapped {
			envLog(env).Info("Remapping host/relative cwd override for backend (won't exist in sandbox)", "cwd", cwd, "backend", envType, "using", remapped)
		}
		cwd = remapped
	}
	if timeout != nil && *timeout <= 0 {
		return nil, &terminalRejected{toolErrorJSON(fmt.Sprintf("timeout must be a positive number of seconds (got %d).", *timeout), nil)}
	}
	plan := &execPlan{config: cfg, envType: envType, effectiveTaskID: effective, image: image, cwd: cwd, hostCwd: hostCwd}
	if !background {
		autoBg := trailingAmpBackgroundCommandFor(env, cmd)
		if autoBg != "" {
			plan.autoBackgroundCommand, plan.autoBackground = autoBg, true
		} else {
			if guidance := foregroundBackgroundGuidance(cmd); guidance != "" {
				return nil, &terminalRejected{errorJSON(guidance, -1, "error", nil)}
			}
			if timeout != nil && *timeout > foregroundMaxTimeout() {
				plan.promotedFromForegroundTimeout = *timeout
			}
		}
	}
	plan.effectiveTimeout, _ = cfg["timeout"].(int)
	if timeout != nil && *timeout != 0 {
		plan.effectiveTimeout = *timeout
	}
	return plan, nil
}

// Python type name of a JSON decoded value for the invalid command message
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case float64, int, int64:
		return "int"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

// Attaches the foreground to background promotion note to a spawn result, unchanged on error
func withPromotedNote(resultJSON string, requestedTimeout int) string {
	decoded, err := pyjson.Loads(resultJSON)
	data, _ := decoded.(*pyjson.Object)
	if err != nil || data == nil {
		return resultJSON
	}
	if e, ok := data.Get("error"); ok && pyrepr.Truthy(e) {
		return resultJSON
	}
	template := promotedNotePollOnly
	if notify, _ := data.Get("notify_on_complete"); pyrepr.Truthy(notify) {
		template = promotedNote
	}
	note := strings.ReplaceAll(strings.ReplaceAll(template, "{requested}", strconv.Itoa(requestedTimeout)), "{cap}", strconv.Itoa(foregroundMaxTimeout()))
	data.Set("promoted_from_foreground", note)
	return dumpsUnicode(data)
}

// Cached env for the task, else created under the per task creation lock
func acquireEnv(ctx context.Context, env *Env, plan *execPlan, taskID string) (Environment, error) {
	startCleanupThreadFn(env)
	envType, eff := plan.envType, plan.effectiveTaskID
	termPool.Lock()
	e := lookupActiveEnv(eff, taskID)
	termPool.Unlock()
	if e != nil {
		return e, nil
	}
	lock := creationLockFor(eff)
	lock.Lock()
	defer lock.Unlock()
	termPool.Lock()
	e = lookupActiveEnv(eff, taskID)
	termPool.Unlock()
	if e != nil {
		return e, nil
	}
	envLog(env).Info("Creating new environment for task", "backend", envType, "task", shortTaskID(eff))
	created, err := createConfiguredEnv(ctx, env, plan.config, envType, plan.image, plan.cwd, plan.effectiveTimeout, eff, plan.hostCwd)
	if err != nil {
		var missing *BackendMissingError
		if errors.As(err, &missing) {
			return nil, &terminalRejected{errorJSON(redactTerminalErrorText(env, fmt.Sprintf("Terminal tool disabled: environment creation failed (%s)", err)), -1, "disabled", nil)}
		}
		return nil, err
	}
	termPool.Lock()
	termPool.active[eff] = created
	termPool.lastActivity[eff] = time.Now()
	termPool.Unlock()
	envLog(env).Info("Environment ready for task", "backend", envType, "task", shortTaskID(eff))
	return created, nil
}

// BackendMissingError reports that a backend's SDK or helper is not installed
type BackendMissingError struct{ Reason string }

func (e *BackendMissingError) Error() string { return e.Reason }

// Emits the observability metric for one terminal outcome
func recordTerminalMetric(env *Env, name string, data map[string]any) {
	data["name"] = name
	env.Emit(Event{Kind: "metric", Tool: "terminal", Data: data})
}

// Counts one terminal call that got past planning against its backend and returns the result unchanged
func meterTerminalBackend(env *Env, plan *execPlan, result, errorClass string) string {
	if plan == nil {
		return result
	}
	recordTerminalMetric(env, "icebath.execution_backend", map[string]any{"kind": "terminal", "backend": plan.envType, "result": result, "error_class": errorClass})
	return result
}

// Exit statuses of a command the shell killed by signal
var killedExitStatuses = map[int]bool{137: true, 143: true}

// How one foreground command ended, the icebath deadline and interrupt flags first, then its exit status
func terminalOutcome(result *ExecResult) string {
	switch {
	case result.TimedOut:
		return "timeout"
	case result.Interrupted:
		return "killed"
	case result.ReturnCode == 0:
		return "ok"
	case result.ReturnCode < 0 || killedExitStatuses[result.ReturnCode]:
		return "killed"
	}
	return "nonzero"
}

// Executes in the foreground with retry on transient errors, then finalizes
func runForeground(ctx context.Context, env *Env, command string, e Environment, plan *execPlan, taskID, sessionID, sessionKey, workdir, approvalNote string, clearInterrupt, metered bool) (string, error) {
	const maxRetries = 3
	envType, eff, effectiveTimeout := plan.envType, plan.effectiveTaskID, plan.effectiveTimeout
	if clearInterrupt {
		ClearCurrentInterrupt(ctx)
	}
	var result *ExecResult
	commandCwd := ""
	for retry := 0; retry <= maxRetries; retry++ {
		mounted := e.HostCwd()
		if mounted == "" {
			mounted = plan.hostCwd
		}
		commandCwd = resolveCommandCwd(env, workdir, plan.cwd, sessionKey, envType, mounted, e)
		res, err := e.Execute(ctx, command, ExecOptions{
			Cwd: commandCwd, Timeout: effectiveTimeout, BoundedCapture: true,
			YieldHandler: yieldToBackgroundHandler(ctx, env, command, envType, commandCwd, eff, taskID, sessionKey),
		})
		if err == nil {
			result = res
			break
		}
		var ce *EnvironmentConnectionError
		if errors.As(err, &ce) {
			return "", err
		}
		if strings.Contains(strings.ToLower(err.Error()), "timeout") {
			return errorJSON(fmt.Sprintf("Command timed out after %d seconds", effectiveTimeout), 124, "", nil), nil
		}
		if retry < maxRetries {
			wait := time.Duration(1<<uint(retry+1)) * time.Second
			envLog(env).Warn("Execution error, retrying", "wait", wait.String(), "attempt", retry+1, "max", maxRetries,
				"command", safeCommandPreview(command, 200), "error", err, "task", eff, "backend", envType)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
			}
			continue
		}
		envLog(env).Error("Execution failed after retries", "retries", maxRetries, "command", safeCommandPreview(command, 200), "error", err, "task", eff, "backend", envType)
		return errorJSON(redactTerminalErrorText(env, fmt.Sprintf("Command execution failed: %T: %s", err, err)), -1, "", nil), nil
	}
	if result.YieldedSessionID != "" {
		return orderedJSON([][2]any{
			{"output", result.Output}, {"exit_code", nil}, {"error", nil},
			{"status", "yielded_to_background"}, {"session_id", result.YieldedSessionID},
			{"pid", result.Pid}, {"notify_on_complete", true}, {"note", yieldedNote},
		}), nil
	}
	if metered {
		recordTerminalMetric(env, "icebath.terminal.outcome", map[string]any{"command": command, "backend": envType, "outcome": terminalOutcome(result)})
	}
	return finalizeForegroundResult(ctx, env, command, result, e, envType, eff, taskID, sessionID, sessionKey, workdir, commandCwd, approvalNote), nil
}

// Floor for the pre exec guard's share of the command deadline in seconds
const preExecGuardMinTimeoutS = 30

// Minimum seconds the pre exec guard probe waits, the terminal tool floor execute_code shares
const PreExecGuardMinTimeoutS = preExecGuardMinTimeoutS

// Returns the blocked result JSON when the command must not run, lifecycle first then workdir then local guards
func preExecBlock(ctx context.Context, env *Env, command string, e Environment, envType, cwd, workdir, sessionKey string) error {
	if blocked := gatewayLifecycleBlock(ctx, env, command, e, envType, cwd, workdir, sessionKey); blocked != "" {
		return &terminalRejected{blocked}
	}
	if workdir != "" {
		if workdirError := validateWorkdir(workdir); workdirError != "" {
			envLog(env).Warn("Blocked dangerous workdir", "workdir", safeCommandPreview(workdir, 200), "command", safeCommandPreview(command, 200))
			return &terminalRejected{errorJSON(workdirError, -1, "blocked", nil)}
		}
	}
	if envType == "local" {
		blocked := selfRepoBlock(env, command, cwd, workdir, sessionKey)
		if blocked == "" {
			blocked = lockedSkillBlock(env, command, cwd, workdir, sessionKey)
		}
		if blocked != "" {
			return &terminalRejected{blocked}
		}
	}
	return nil
}

const ptyDisabledReason = "PTY disabled for this command because it expects piped stdin/EOF " +
	"(for example gh auth login --with-token). For local background " +
	"processes, call process(action='close') after writing so it receives " +
	"EOF."

// Structured degraded result for an infrastructure failure, or the fatal envelope in fail mode
func degradedResult(env *Env, e *EnvironmentConnectionError, taskID string) string {
	policy, err := terminalPolicy(env)
	if err == nil && strings.ToLower(strings.TrimSpace(policy.Get("TERMINAL_DEGRADED_MODE", "warn"))) == "fail" {
		return fatalErrorJSON(env, e)
	}
	envLog(env).Warn("terminal backend degraded", "reason", e.Reason)
	if err := evictEnvironmentForTask(env, taskID); err != nil {
		envLog(env).Debug("degraded-env eviction failed", "error", err)
	}
	return orderedJSON([][2]any{
		{"output", ""}, {"exit_code", -1}, {"status", "degraded"}, {"reason", e.Reason},
		{"retry_hint", e.RetryHint}, {"error", fmt.Sprintf("Terminal backend degraded: %s", e.Reason)},
	})
}

// Runs the pre exec guard chain on a worker bounded by the command deadline, true when it timed out
func runBoundedGuard(timeout time.Duration, guard func() error) (error, bool) {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("terminal.pre-exec-guard panicked: %v", r)
			}
		}()
		done <- guard()
	}()
	select {
	case err := <-done:
		return err, false
	case <-time.After(timeout):
		return nil, true
	}
}

// Runs the terminal tool with the plan already made, errors carry the finished result
func terminalRun(ctx context.Context, env *Env, req TerminalRequest, metered **execPlan) (string, error) {
	plan, err := planExecutionFn(env, req.Command, req.TaskID, req.Timeout, req.Background, req.HostLocal)
	if err != nil {
		return "", err
	}
	if !req.HostLocal {
		*metered = plan
	}
	command, _ := req.Command.(string)
	background := req.Background
	autoBg := plan.autoBackground
	if autoBg {
		command, background = plan.autoBackgroundCommand, true
	}
	e, err := acquireEnvFn(ctx, env, plan, req.TaskID)
	if err != nil {
		return "", err
	}
	release := callInFlight(plan.effectiveTaskID, req.TaskID)
	defer release()
	envType, cwd, effectiveTaskID := plan.envType, plan.cwd, plan.effectiveTaskID
	sessionKey := getCurrentSessionKey(env, "")
	if sessionKey == "" {
		sessionKey = req.TaskID
	}
	guardTimeout := time.Duration(plan.effectiveTimeout) * time.Second
	if guardTimeout < preExecGuardMinTimeout {
		guardTimeout = preExecGuardMinTimeout
	}
	guardCtx := WithActingFor(ctx, WorkerOf(ctx))
	guard := preExecBlockFn
	guardErr, timedOut := runBoundedGuard(guardTimeout, func() error {
		return guard(guardCtx, env, command, e, envType, cwd, req.Workdir, sessionKey)
	})
	if timedOut {
		return "", &terminalRejected{errorJSON(fmt.Sprintf("Terminal pre-execution guard did not finish within %ds "+
			"(process-identity probe wedged); the command was not run. Retry the call.", int(guardTimeout.Seconds())), -1, "error", nil)}
	}
	if guardErr != nil {
		return "", guardErr
	}
	verdict, err := runApprovalGuardsFn(ctx, env, command, envType, plan.config, req.Force)
	if err != nil {
		return "", err
	}
	ptyDisabled := req.Pty && commandRequiresPipeStdin(command)
	notify, watch := req.NotifyOnComplete, req.WatchPatterns
	if plan.promotedFromForegroundTimeout != 0 {
		background, notify, watch = true, true, nil
	}
	if background {
		mounted := e.HostCwd()
		if mounted == "" {
			mounted = plan.hostCwd
		}
		reason := ""
		if ptyDisabled {
			reason = ptyDisabledReason
		}
		result := spawnBackgroundProcess(ctx, env, e, backgroundSpawnRequest{
			command: command, envType: envType, effectiveTaskID: effectiveTaskID, taskID: req.TaskID,
			sessionKey: sessionKey, workdir: req.Workdir, cwd: cwd, mountedHost: mounted,
			effectivePty: req.Pty && !ptyDisabled, notifyOnComplete: notify, watchPatterns: watch,
			approvalNote: verdict.note, ptyDisabledReason: reason, completionOutputChars: req.CompletionOutputChars,
			heartbeatSeconds: req.Heartbeat, persistOnRelease: req.PersistOnRelease,
			autoBackgrounded: autoBg && plan.promotedFromForegroundTimeout == 0,
		})
		if plan.promotedFromForegroundTimeout != 0 {
			result = withPromotedNote(result, plan.promotedFromForegroundTimeout)
		}
		return result, nil
	}
	result, err := runForegroundFn(ctx, env, command, e, plan, req.TaskID, req.SessionID, sessionKey, req.Workdir, verdict.note, verdict.approvedRun, !req.HostLocal)
	if err != nil {
		return "", err
	}
	return result, nil
}

// TerminalTool executes a command in the configured terminal environment and returns the JSON result
func TerminalTool(ctx context.Context, env *Env, req TerminalRequest) (out string) {
	var plan *execPlan
	defer func() {
		if r := recover(); r != nil {
			err, ok := r.(error)
			if !ok {
				err = fmt.Errorf("%v", r)
			}
			out = meterTerminalBackend(env, plan, fatalErrorJSON(env, err), "exception")
		}
	}()
	result, err := terminalRun(ctx, env, req, &plan)
	if err == nil {
		return meterTerminalBackend(env, plan, result, "")
	}
	if rej, ok := err.(*terminalRejected); ok {
		return rej.resultJSON
	}
	var ce *EnvironmentConnectionError
	if errors.As(err, &ce) {
		return meterTerminalBackend(env, plan, degradedResult(env, ce, req.TaskID), "tool_error")
	}
	return meterTerminalBackend(env, plan, fatalErrorJSON(env, err), "exception")
}

// Model visible schema of the terminal tool
func terminalSchema() map[string]any {
	cap := foregroundMaxTimeout()
	return map[string]any{
		"name":        "terminal",
		"description": terminalToolDescription,
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "The shell command to execute"},
				"background": map[string]any{"type": "boolean",
					"description": "Keep running after this call returns; returns a session_id.", "default": false},
				"timeout": map[string]any{"type": "integer",
					"description": fmt.Sprintf("Max seconds (default 180, foreground max %d). Finishing early returns at once, so set it generously. Above %ds the command runs as a tracked background process with notify_on_complete=true (the result says so; do not re-run it).", cap, cap),
					"minimum":     1},
				"workdir": map[string]any{"type": "string", "description": "Absolute working directory for this command only."},
				"pty": map[string]any{"type": "boolean",
					"description": "With background=true: pseudo-terminal for interactive CLIs (REPLs, coding agents); drive it with process_manage write/submit. Local backend only.",
					"default":     false},
				"notify": map[string]any{
					"description": "With background=true. true: one notification on exit; use it for anything that ends (builds, tests, deploys), or the job runs silently. ['pattern', ...]: notify when a line matches, only for one-shot readiness on processes that never exit; rate-limited. Omit for servers/daemons.",
					"anyOf": []any{
						map[string]any{"type": "boolean"},
						map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
				},
				"heartbeat": map[string]any{"type": "integer", "minimum": 0, "default": 0,
					"description": "With background=true: also notify every N seconds (min 60) with new output; quiet ticks are skipped. For bounded jobs you must react to mid-run, never servers. Implies notify=true; 0 = off."},
				"persist_on_release": map[string]any{"type": "boolean", "default": false,
					"description": "With background=true: survive session end, /new, compression and other cleanup (not host exit). Only for jobs the user explicitly wants to outlive the conversation."},
			},
			"required": []any{"command"},
		},
	}
}

// Integer argument the way Python int typing sees JSON, false for bool, float with a fraction, or other kinds
func argInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
	}
	return 0, false
}

// Dispatch wrapper mapping the advertised notify argument onto the legacy flags
func handleTerminal(ctx context.Context, call *Call) string {
	args := call.Args
	if err := ValidatePreparedTerminal(ctx, args); err != nil {
		return errorJSON(err.Error(), -1, "error", nil)
	}
	if _, ok := args["command"]; !ok {
		if _, hasCode := args["code"]; hasCode {
			return toolErrorJSON("terminal received a 'code' parameter, but it requires a shell "+
				"command in 'command'. Use execute_code(code=...) for Python; "+
				"for shell, retry as terminal(command=...).", nil)
		}
	}
	notify, hasNotify := args["notify"]
	notifyOnComplete := truthy(args["notify_on_complete"])
	var watchPatterns []string
	watchSet := false
	if raw, ok := args["watch_patterns"]; ok && raw != nil {
		watchSet = true
		if list, ok := raw.([]any); ok {
			for _, p := range list {
				watchPatterns = append(watchPatterns, fmt.Sprint(p))
			}
		}
	}
	heartbeat := 0
	if raw, ok := args["heartbeat"]; ok && raw != nil {
		if _, isBool := raw.(bool); isBool {
			return toolErrorJSON("heartbeat must be a whole number of seconds (0 disables; positive values are clamped to min 60).", nil)
		}
		n, ok := argInt(raw)
		if !ok || n < 0 {
			return toolErrorJSON("heartbeat must be a whole number of seconds (0 disables; positive values are clamped to min 60).", nil)
		}
		heartbeat = n
	}
	persistOnRelease := truthy(args["persist_on_release"])
	background := truthy(args["background"])
	if !background {
		if (hasNotify && truthy(notify)) || watchSet && len(watchPatterns) > 0 || notifyOnComplete {
			return toolErrorJSON("notify only applies to background commands (foreground "+
				"results return directly). Either drop it, or run as "+
				"terminal(command=..., background=true, notify=...).", nil)
		}
		heartbeat = 0
		if truthy(args["pty"]) {
			return toolErrorJSON("pty requires background=true (a PTY session is interacted "+
				"with via process(action='write'/'submit'), which needs a "+
				"tracked background process). Retry as terminal(command=..., "+
				"background=true, pty=true).", nil)
		}
		if persistOnRelease {
			return toolErrorJSON("persist_on_release only applies to background commands (a foreground "+
				"process is awaited inline and has nothing to persist). Retry as "+
				"terminal(command=..., background=true, persist_on_release=true).", nil)
		}
	}
	if hasNotify && notify != nil {
		switch n := notify.(type) {
		case bool:
			notifyOnComplete = n
			watchPatterns = nil
		case []any:
			watchPatterns = nil
			for _, p := range n {
				watchPatterns = append(watchPatterns, fmt.Sprint(p))
			}
			notifyOnComplete = false
		default:
			return toolErrorJSON("notify must be true/false (notify on exit) or a list of "+
				"strings (notify on output pattern match).", nil)
		}
	}
	if heartbeat != 0 {
		notifyOnComplete = true
	}
	var timeout *int
	if raw, ok := args["timeout"]; ok && raw != nil {
		if n, ok := argInt(raw); ok {
			timeout = &n
		}
	}
	workdir, _ := args["workdir"].(string)
	return TerminalTool(ctx, call.Env, TerminalRequest{
		Command: args["command"], Background: background, Timeout: timeout, TaskID: call.TaskID,
		SessionID: call.SessionID, Workdir: workdir, Pty: truthy(args["pty"]), NotifyOnComplete: notifyOnComplete,
		WatchPatterns: watchPatterns, Heartbeat: heartbeat, PersistOnRelease: persistOnRelease,
	})
}

func init() {
	Register(Def{
		Name:           "terminal",
		Toolset:        "terminal",
		Schema:         terminalSchema(),
		Handler:        handleTerminal,
		Check:          CheckTerminalRequirements,
		Emoji:          "\U0001F4BB",
		MaxResultChars: 100_000,
	})
}
