package codeexec

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Environment lookup and remote kernel entry, swapped by tests
var (
	getOrCreateEnv          = getOrCreateTaskEnv
	executeInRemoteKernelFn = executeInRemoteKernel
)

// The env terminal and file tools share for the task with its backend type, created on first use
func getOrCreateTaskEnv(ctx context.Context, env *tools.Env, taskID string) (tools.Environment, string, error) {
	cached := func() (tools.Environment, string, error) {
		e, err := tools.TouchTaskActivity(env, taskID)
		if err != nil || e == nil {
			return nil, "", err
		}
		cfg, err := tools.GetEnvConfig(env)
		if err != nil {
			return nil, "", err
		}
		envType, _ := cfg["env_type"].(string)
		return e, envType, nil
	}
	if e, envType, err := cached(); err != nil || e != nil {
		return e, envType, err
	}
	lock, err := tools.TaskCreationLock(env, taskID)
	if err != nil {
		return nil, "", err
	}
	lock.Lock()
	defer lock.Unlock()
	if e, envType, err := cached(); err != nil || e != nil {
		return e, envType, err
	}
	cfg, err := tools.GetEnvConfig(env)
	if err != nil {
		return nil, "", err
	}
	envType, _ := cfg["env_type"].(string)
	created, err := tools.CreateTaskEnvironment(ctx, env, cfg, taskID)
	if err != nil {
		return nil, "", err
	}
	return created, envType, nil
}

// Writes content owner only to a remote path with the payload on stdin, failing on a non zero exit
func shipFile(ctx context.Context, envr tools.Environment, remotePath, content string, atomic bool) error {
	_, err := remoteWrite(ctx, envr, remotePath, content, atomic, 30, true)
	return err
}

// Ships the sandbox env as sourced KEY=value lines and returns the subshell command that sources it and runs launch
func shipEnvFileAndLaunch(ctx context.Context, envr tools.Environment, remoteDir, envName, launch, rpcDir, rpcToken string, extra ...[2]string) (string, error) {
	pairs := append([][2]string{{"ICEBATH_RPC_DIR", rpcDir}, {"ICEBATH_RPC_TOKEN", rpcToken}, {"PYTHONDONTWRITEBYTECODE", "1"}}, extra...)
	if tz := timezoneName(ctx); tz != "" {
		pairs = append(pairs, [2]string{"TZ", tz})
	}
	var lines strings.Builder
	for _, kv := range pairs {
		lines.WriteString(kv[0] + "=" + pyshlex.Quote(kv[1]) + "\n")
	}
	if err := shipFileToRemote(ctx, envr, remoteDir+"/"+envName, lines.String(), false); err != nil {
		return "", err
	}
	return fmt.Sprintf("cd %s && ( set -a && . ./%s && set +a && %s )", pyshlex.Quote(remoteDir), envName, launch), nil
}

// Absolute writable temp dir of the backend, else the host temp root
func envTempDir(envr tools.Environment) (string, error) {
	host, hostErr := gettempdir()
	candidates := []string{envr.TempDir()}
	if hostErr == nil {
		candidates = append(candidates, host)
	}
	for _, c := range candidates {
		if strings.HasPrefix(c, "/") {
			if trimmed := strings.TrimRight(c, "/"); trimmed != "" {
				return trimmed, nil
			}
			return "/", nil
		}
	}
	return host, hostErr
}

// Error result of a remote run that raised, logged with its duration and call count
func remoteFailure(env *tools.Env, err error, execStart time.Time, toolCallsMade int) string {
	duration := elapsedSeconds(execStart)
	logger(env).Error(fmt.Sprintf("execute_code remote failed after %ss with %d tool calls: %s: %s",
		pyrepr.Str(duration), toolCallsMade, errorTypeName(err), err.Error()))
	return errorResult(err.Error(), toolCallsMade, duration, "")
}

// Backend exit codes of the timeout wrapper and SIGINT
var remoteExitStatus = map[int]string{124: "timeout", 130: "interrupted"}

// Remote reply with status, cleaned output, fields, duration, optional kernel info and truncation metadata in order
func remoteResult(env *tools.Env, status any, rawStdout string, execStart time.Time, fields *pyjson.Object, kernel any) *pyjson.Object {
	text, meta := cleanOutput(env, rawStdout)
	result := pyjson.NewObject()
	result.Set("status", status)
	result.Set("output", text)
	result.Update(fields)
	result.Set("duration_seconds", elapsedSeconds(execStart))
	if kernel != nil {
		result.Set("kernel", kernel)
	}
	result.Update(meta)
	return result
}

// Records the timeout message as the error and appends it to the output
func applyTimeout(result *pyjson.Object, timeoutMsg string) {
	result.Set("error", timeoutMsg)
	out, _ := result.Get("output")
	text, _ := out.(string)
	result.Set("output", withTimeoutNotice(text, timeoutMsg))
}

// Python x or "" over a decoded value, falsy values becoming the empty string
func orEmpty(v any) any {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return v
}

// Python left + right where right is a str, the TypeError text when left is not one
func concatStr(left any, right string) (string, error) {
	switch l := left.(type) {
	case string:
		return l + right, nil
	case []any:
		return "", fmt.Errorf(`can only concatenate list (not "str") to list`)
	}
	return "", fmt.Errorf("unsupported operand type(s) for +: '%s' and 'str'", pyrepr.TypeName(left))
}

// Python left + right where left is a str, the TypeError text when right is not one
func concatOnto(left string, right any) (string, error) {
	r, ok := right.(string)
	if !ok {
		return "", fmt.Errorf(`can only concatenate str (not "%s") to str`, pyrepr.TypeName(right))
	}
	return left + r, nil
}

// Remote kernel cell result as the tool reply, timeout messaging matching the local kernel contract
func finishRemoteKernelResult(env *tools.Env, kernelResult *pyjson.Object, timeoutText string, execStart time.Time) (string, error) {
	stdoutText := orEmpty(objGet(kernelResult, "stdout", ""))
	stderrText := orEmpty(objGet(kernelResult, "stderr", ""))
	tracebackText := orEmpty(objGet(kernelResult, "traceback", ""))
	if pyrepr.Truthy(stderrText) || pyrepr.Truthy(tracebackText) {
		joined, err := concatStr(stdoutText, "\n--- stderr ---\n")
		if err == nil {
			joined, err = concatOnto(joined, stderrText)
		}
		if err == nil {
			joined, err = concatOnto(joined, tracebackText)
		}
		if err != nil {
			return "", err
		}
		stdoutText = joined
	}
	text, ok := stdoutText.(string)
	if !ok {
		return "", fmt.Errorf("'%s' object has no attribute 'encode'", pyrepr.TypeName(stdoutText))
	}
	fields := pyjson.NewObject()
	fields.Set("tool_calls_made", objGet(kernelResult, "tool_calls_made", 0))
	defaultKernel := pyjson.NewObject()
	defaultKernel.Set("remote", true)
	result := remoteResult(env, objGet(kernelResult, "status", "error"), text, execStart, fields, objGet(kernelResult, "kernel", defaultKernel))
	if toolErrors := objGet(kernelResult, "tool_errors", nil); pyrepr.Truthy(toolErrors) {
		result.Set("tool_errors", toolErrors)
	}
	status, _ := result.Get("status")
	if isStatus(status, "timeout") {
		applyTimeout(result, fmt.Sprintf("Cell timed out after %ss; the remote session kernel was "+
			"killed and its state was lost. The next call starts fresh.", timeoutText))
	} else if cellErr := objGet(kernelResult, "error", nil); isStatus(status, "error") && pyrepr.Truthy(cellErr) {
		result.Set("error", cellErr)
	}
	return pyjson.Dumps(result, pyjson.EnsureASCII(false))
}

// Backend timeout in whole seconds, a fractional limit rounded up so it never reads as no limit
func execTimeout(seconds float64) int {
	if seconds >= math.MaxInt32 {
		return math.MaxInt32
	}
	return int(math.Ceil(seconds))
}

// Remote execution parameters shared by the kernel and per call paths
type remoteRun struct {
	envType      string
	taskID       string
	sandboxTools []string
	timeout      float64
	timeoutText  string
	maxToolCalls int
	execStart    time.Time
}

// Ships the tools module and script into a fresh private sandbox dir, serves file RPC while it runs, then cleans up
func runRemotePerCall(ctx context.Context, env *tools.Env, envr tools.Environment, code string, r remoteRun) string {
	tmp, err := envTempDir(envr)
	if err != nil {
		panic(err)
	}
	sandboxDir := tmp + "/icebath_exec_" + uuid4Hex()[:12]
	budget := newRPCBudget(r.sandboxTools, r.maxToolCalls)
	var stdoutText, status string
	var exitCode int
	err = func() error {
		stop := make(chan struct{})
		var done chan struct{}
		defer func() {
			close(stop)
			if done != nil {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
				}
			}
			if _, err := envr.Execute(context.Background(), "rm -rf "+pyshlex.Quote(sandboxDir), tools.ExecOptions{Cwd: "/", Timeout: 15}); err != nil {
				logger(env).Debug("Failed to clean up remote sandbox " + sandboxDir)
			}
		}()
		if _, err := executeChecked(ctx, envr, privateDirsCmd(sandboxDir, sandboxDir+"/rpc"), "remote sandbox setup", 10, nil); err != nil {
			return err
		}
		rpcToken := tokenURLSafe(32)
		if err := shipFileToRemote(ctx, envr, sandboxDir+"/icebath_tools.py", generateToolsModule(r.sandboxTools, "file"), false); err != nil {
			return err
		}
		if err := shipFileToRemote(ctx, envr, sandboxDir+"/script.py", code, false); err != nil {
			return err
		}
		done = make(chan struct{})
		go func() {
			defer close(done)
			remotePollLoop(ctx, env, envr, sandboxDir+"/rpc", r.taskID, budget, stop, rpcToken, defaultDispatch(env, r.taskID))
		}()
		launch, err := shipEnvFileAndLaunch(ctx, envr, sandboxDir, "sandbox.env", "exec python3 script.py", sandboxDir+"/rpc", rpcToken)
		if err != nil {
			return err
		}
		short := string([]rune(r.taskID)[:min(8, len([]rune(r.taskID)))])
		logger(env).Info(fmt.Sprintf("Executing code on %s backend (task %s)...", r.envType, short))
		scriptResult, err := envr.Execute(ctx, launch, tools.ExecOptions{Timeout: execTimeout(r.timeout)})
		if err != nil {
			return err
		}
		stdoutText, exitCode = scriptResult.Output, scriptResult.ReturnCode
		status = "success"
		if s, ok := remoteExitStatus[exitCode]; ok {
			status = s
		}
		return nil
	}()
	if err != nil {
		return remoteFailure(env, err, r.execStart, budget.callCount())
	}
	fields := pyjson.NewObject()
	fields.Set("exit_code", exitCode)
	fields.Set("tool_calls_made", budget.callCount())
	result := remoteResult(env, status, stdoutText, r.execStart, fields, nil)
	if toolErrors := toolErrorsSince(budget.entries(), 0); len(toolErrors) > 0 {
		result.Set("tool_errors", toolErrors)
	}
	switch {
	case status == "timeout":
		applyTimeout(result, fmt.Sprintf("Script timed out after %ss and was killed.", r.timeoutText))
		duration, _ := result.Get("duration_seconds")
		logger(env).Warn(fmt.Sprintf("execute_code (remote) timed out after %ss (limit %ss) with %d tool calls",
			pyrepr.Str(duration), r.timeoutText, budget.callCount()))
	case status == "interrupted":
		out, _ := result.Get("output")
		text, _ := out.(string)
		result.Set("output", formatInterruptedOutput(ctx, text))
	case exitCode != 0:
		result.Set("status", "error")
		result.Set("error", fmt.Sprintf("Script exited with code %d", exitCode))
	}
	out, err := pyjson.Dumps(result, pyjson.EnsureASCII(false))
	if err != nil {
		return toolError(err.Error())
	}
	return out
}

// Runs code on the remote backend through the owner's session kernel, else the per call script ship
func executeRemote(ctx context.Context, env *tools.Env, code, taskID string, enabledTools []string, reset bool) string {
	cfg := loadConfig(env)
	timeout, timeoutText, err := configSeconds(configValue(cfg, "timeout", defaultTimeout))
	if err != nil {
		panic(err)
	}
	maxToolCalls := toolCallLimit(configValue(cfg, "max_tool_calls", defaultMaxToolCalls))
	sandboxTools, effectiveTaskID := sandboxToolsFor(enabledTools), taskID
	if effectiveTaskID == "" {
		effectiveTaskID = "default"
	}
	envr, envType, err := getOrCreateEnv(ctx, env, effectiveTaskID)
	if err != nil {
		panic(err)
	}
	key, err := tools.ResolveContainerTaskID(env, effectiveTaskID)
	if err != nil {
		panic(err)
	}
	release := tools.CallInFlight(key)
	defer release()
	execStart := time.Now()
	check, err := envr.Execute(ctx, "command -v python3 >/dev/null 2>&1 && echo OK", tools.ExecOptions{Cwd: "/", Timeout: 15})
	if err != nil {
		return remoteFailure(env, err, execStart, 0)
	}
	if !strings.Contains(check.Output, "OK") {
		return errorResult(fmt.Sprintf("Python 3 is not available in the %s terminal ", envType)+
			"environment. Install Python to use execute_code with remote backends.", 0, 0, "")
	}
	kernelResult, err := func() (*pyjson.Object, error) {
		idle, err := pyint.Of(configValue(cfg, "kernel_idle_timeout", 1800))
		if err != nil {
			return nil, err
		}
		return executeInRemoteKernelFn(ctx, env, envr, code, remoteCell{envType: envType, taskEnvID: effectiveTaskID,
			sandboxTools: sandboxTools, timeout: timeout, maxToolCalls: maxToolCalls, reset: reset, idleExit: idle})
	}()
	if err != nil {
		logger(env).Warn("remote session-kernel path failed; falling back to per-call", "error", err)
		kernelResult = nil
	}
	if kernelResult != nil {
		out, err := finishRemoteKernelResult(env, kernelResult, timeoutText, execStart)
		if err != nil {
			return remoteFailure(env, err, execStart, 0)
		}
		return out
	}
	logger(env).Info(fmt.Sprintf("remote session kernel unavailable on %s; using per-call path", envType))
	return runRemotePerCall(ctx, env, envr, code, remoteRun{envType: envType, taskID: effectiveTaskID, sandboxTools: sandboxTools,
		timeout: timeout, timeoutText: timeoutText, maxToolCalls: maxToolCalls, execStart: execStart})
}
