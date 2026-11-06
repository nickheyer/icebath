package codeexec

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Loopback TCP replaces unix sockets on Windows so the sandbox runs on every platform, swapped by tests
var sandboxAvailable = true

const (
	defaultTimeout      = 300
	defaultMaxToolCalls = 50
)

// Available unless the vercel_sandbox backend fails its own checks or the terminal config cannot resolve
func checkSandboxRequirements(env *tools.Env) bool {
	if !sandboxAvailable {
		return false
	}
	cfg, err := tools.GetEnvConfig(env)
	if err != nil {
		logger(env).Debug("Could not resolve terminal config for execute_code availability", "error", err)
		return false
	}
	return cfg["env_type"] != "vercel_sandbox" || tools.CheckVercelSandboxRequirements(env, cfg)
}

// Enabled tools intersected with the sandbox allow list, every sandbox tool when that is empty
func sandboxToolsFor(enabledTools []string) []string {
	var out []string
	for _, name := range sandboxAllowedTools {
		if slices.Contains(enabledTools, name) {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return slices.Clone(sandboxAllowedTools)
	}
	return out
}

// Python number of a config value as seconds plus its str, a TypeError for anything else
func configSeconds(v any) (float64, string, error) {
	switch n := v.(type) {
	case float64:
		return n, pyrepr.Str(n), nil
	case float32:
		return float64(n), pyrepr.Str(float64(n)), nil
	}
	if i, err := pyint.Of(v); err == nil && isPyIntLike(v) {
		f, _ := new(big.Float).SetInt(i).Float64()
		return f, pyrepr.Str(pyint.Value(i)), nil
	}
	return 0, "", fmt.Errorf("TypeError: '>' not supported between instances of 'int' and '%s'", pyrepr.TypeName(v))
}

func isPyIntLike(v any) bool {
	switch v.(type) {
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, *big.Int:
		return true
	}
	return false
}

// Per cell tool call ceiling compared the way Python compares an int counter with the configured number
func toolCallLimit(v any) int {
	if f, ok := v.(float64); ok {
		if f > math.MaxInt32 {
			return math.MaxInt
		}
		return int(math.Ceil(f))
	}
	if i, err := pyint.Of(v); err == nil && isPyIntLike(v) {
		if !i.IsInt64() || i.Int64() > math.MaxInt {
			if i.Sign() > 0 {
				return math.MaxInt
			}
			return math.MinInt
		}
		return int(i.Int64())
	}
	panic(fmt.Errorf("TypeError: '>=' not supported between instances of 'int' and '%s'", pyrepr.TypeName(v)))
}

func configValue(cfg map[string]any, key string, def any) any {
	if v, ok := cfg[key]; ok {
		return v
	}
	return def
}

// Gateway identity probe, its deadline floor and the terminal config lookup, swapped by tests
var (
	isSupervisedGatewayProcess = tools.IsSupervisedGatewayProcess
	preExecGuardMinTimeoutS    = tools.PreExecGuardMinTimeoutS
	getEnvConfig               = tools.GetEnvConfig
)

// Runs Python in the session kernel locally or on the remote backend behind the approval and lifecycle guards
func executeCode(ctx context.Context, env *tools.Env, code, taskID string, enabledTools []string, reset bool) string {
	if !sandboxAvailable {
		return toolError("execute_code sandbox is unavailable in this environment. " +
			"Use normal tool calls (terminal, read_file, write_file, ...) instead.")
	}
	if refusal := tools.EnforceNoRefusal(ctx); refusal != nil {
		return toolError(fmt.Sprintf("execute_code refused: %s ", refusal.Error()) +
			"(profile terminal policy unresolved; fix the profile's config.yaml / .env and retry)")
	}
	if pystr.Strip(code) == "" {
		return toolError("No code provided. execute_code requires a non-empty 'code' " +
			"parameter containing Python source. To run shell commands, use terminal(command=...) instead.")
	}
	configured, configuredText, err := configSeconds(configValue(loadConfig(env), "timeout", defaultTimeout))
	if err != nil {
		panic(err)
	}
	probeSeconds, probeText := configured, configuredText
	if float64(preExecGuardMinTimeoutS) > configured {
		probeSeconds, probeText = float64(preExecGuardMinTimeoutS), fmt.Sprint(preExecGuardMinTimeoutS)
	}
	supervisedProbe := isSupervisedGatewayProcess
	probe, err := provider.RunBounded(ctx, func(context.Context) (any, error) {
		return supervisedProbe(), nil
	}, time.Duration(probeSeconds*float64(time.Second)), true, "execute_code.lifecycle-guard", nil, nil)
	if err != nil {
		panic(err)
	}
	if probe.TimedOut {
		return toolError(fmt.Sprintf("execute_code lifecycle guard did not finish within %ss ", probeText) +
			"(process-identity probe wedged); the code was not run. Retry the call.")
	}
	if supervised, _ := probe.Value.(bool); supervised && tools.ContainsGatewayLifecycleCommand(code) {
		if tools.ContainsHostInterpreterKill(code) {
			return toolError(tools.HostInterpreterKillRejection)
		}
		return toolError("Blocked: cannot restart or stop the gateway from inside the " +
			"gateway process. The gateway would kill this script before " +
			"it could complete (SIGTERM propagates to child processes). " +
			"Run the lifecycle command from a shell outside the gateway.")
	}
	envConfig, err := getEnvConfig(env)
	if err != nil {
		panic(err)
	}
	envType, _ := envConfig["env_type"].(string)
	guard := tools.CheckExecuteCodeGuard(ctx, env, code, envType, tools.DockerHasHostAccess(envConfig))
	if !guard.Approved {
		message := guard.Message
		if message == "" {
			message = "execute_code blocked by approval guard."
		}
		return errorResult(message, 0, 0, guard.UserSummary)
	}
	if guard.UserApproved {
		tools.ClearCurrentInterrupt(ctx)
	}
	if envType != "local" {
		return recordBackend(ctx, env, "remote", executeRemote(ctx, env, code, taskID, enabledTools, reset))
	}
	cfg := loadConfig(env)
	mode := getExecutionMode(env)
	childPython, err := resolveChildPython(ctx, env, mode)
	if err != nil {
		panic(err)
	}
	childCwd, err := resolveChildCwd(env, mode, "", taskID)
	if err != nil {
		panic(err)
	}
	seconds, secondsText, err := configSeconds(configValue(cfg, "timeout", defaultTimeout))
	if err != nil {
		panic(err)
	}
	return recordBackend(ctx, env, "local", executeInSessionKernel(ctx, env, code, kernelCell{
		taskID: taskID, mode: mode, childPython: childPython, childCwd: childCwd,
		sandboxTools: sandboxToolsFor(enabledTools), timeout: seconds, timeoutText: secondsText,
		maxToolCalls: toolCallLimit(configValue(cfg, "max_tool_calls", defaultMaxToolCalls)),
		reset:        reset, isInterrupted: func() bool { return tools.IsInterrupted(ctx) },
	}))
}

func recordBackend(ctx context.Context, env *tools.Env, backend, result string) string {
	if out, ok := observability.RecordExecutionBackend(ctx, env, "code", backend, result, nil).(string); ok {
		return out
	}
	return result
}

var toolDocLines = []struct{ name, doc string }{
	{"web_search", "  web_search(query: str, limit: int = 5) -> dict\n" +
		"    Returns {\"data\": {\"web\": [{\"url\", \"title\", \"description\"}, ...]}}"},
	{"web_extract", "  web_extract(urls: list[str], char_limit: int = None) -> dict\n" +
		"    Returns {\"results\": [{\"url\", \"title\", \"content\", \"error\"}, ...]} where content is markdown.\n" +
		"    No LLM summarization. Pages over char_limit (default 15000) are head+tail truncated; full text stored on disk (path in the content footer)."},
	{"read_file", "  read_file(path: str, offset: int = 1, limit: int = 2000) -> dict\n" +
		"    Lines are 1-indexed. Returns {\"content\": \"...\", \"total_lines\": N}"},
	{"write_file", "  write_file(path: str, content: str) -> dict\n    Always overwrites the entire file; an existing file must be read_file'd first or the write is refused."},
	{"search_files", "  search_files(pattern: str, target=\"content\", path=\".\", file_glob=None, limit=50, order=\"discovery\") -> dict\n" +
		"    target: \"content\" (search inside files) or \"files\" (find files by name). Returns {\"matches\": [...]}"},
	{"patch", "  patch(path: str, old_string: str, new_string: str, replace_all: bool = False) -> dict\n" +
		"    Replaces old_string with new_string in the file."},
	{"terminal", "  terminal(command: str, timeout=None, workdir=None) -> dict\n" +
		"    Foreground only (no background/pty). Returns {\"output\": \"...\", \"exit_code\": N}"},
}

// Schema listing only the enabled sandbox tools, nil meaning all, with the cwd sentence of the mode
func buildExecuteCodeSchema(env *tools.Env, enabledSandboxTools []string, mode string) map[string]any {
	if enabledSandboxTools == nil {
		enabledSandboxTools = sandboxAllowedTools
	}
	if mode == "" {
		mode = getExecutionMode(env)
	}
	var docs []string
	for _, line := range toolDocLines {
		if slices.Contains(enabledSandboxTools, line.name) {
			docs = append(docs, line.doc)
		}
	}
	var importExamples []string
	for _, name := range []string{"web_search", "terminal"} {
		if slices.Contains(enabledSandboxTools, name) {
			importExamples = append(importExamples, name)
		}
	}
	if len(importExamples) == 0 {
		sorted := slices.Sorted(slices.Values(enabledSandboxTools))
		importExamples = sorted[:min(2, len(sorted))]
	}
	importStr := "..."
	if len(importExamples) > 0 {
		importStr = strings.Join(importExamples, ", ") + ", ..."
	}
	cwdNote := "Scripts run in the session's working directory. Interpreter: " +
		"the project's activated venv/conda python when one is active " +
		"(VIRTUAL_ENV/CONDA_PREFIX — matches terminal()); otherwise " +
		"Icebath's own python (the common case — stdlib plus Icebath's " +
		"deps; check `import x` before relying on project packages)."
	if mode == "strict" {
		cwdNote = "Scripts run in their own temp dir, not the session's CWD — pass " +
			"absolute paths for any file that lives outside the session's " +
			"working directory, or use terminal()/read_file() to reach it."
	}
	description := "Run Python that calls Icebath tools programmatically. Use when you " +
		"need 3+ tool calls with logic between them: filtering/reducing " +
		"large outputs before they enter context, branching, or loops " +
		"(N pages/files, retry on failure). Use normal tool calls for " +
		"single calls, results you must reason over in full, or anything needing user interaction.\n\n" +
		"Calls run in a persistent session kernel: variables, imports, and " +
		"loaded data survive across execute_code calls, so build on earlier " +
		"work instead of re-loading it. A timed-out or interrupted call loses that state.\n\n" +
		"Available via `from icebath_tools import ...`:\n\n" +
		strings.Join(docs, "\n") + "\n\n" +
		"Limits: 5-minute timeout, max 50 tool calls per call. Stdout over " +
		"50KB shows head/tail inline; the FULL text is auto-saved to a file whose path rides in the result.\n\n" +
		cwdNote + "\n\n" +
		"Helpers require imports: `from icebath_tools import json_parse, shell_quote, retry`. " +
		"json_parse(text) — tolerant " +
		"json.loads for terminal() output; shell_quote(s) — shlex.quote for " +
		"dynamic shell args; retry(fn, max_attempts=3, delay=2) — exponential backoff."
	return map[string]any{
		"name":        "execute_code",
		"description": description,
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code": map[string]any{"type": "string", "description": "Python code to execute. Import tools with " +
					"`from icebath_tools import " + importStr + "` " +
					"and print your final result to stdout."},
				"reset": map[string]any{"type": "boolean", "description": "Discard the kernel's persistent state and start fresh before running this code."},
			},
			"required": []any{"code"},
		},
	}
}

// Redirects misdirected calls with an actionable error before dispatching to the executor
func executeCodeHandler(ctx context.Context, call *tools.Call) string {
	args := call.Args
	_, hasCode := args["code"]
	if _, hasCommand := args["command"]; !hasCode && hasCommand {
		logger(call.Env).Warn("execute_code received 'command' instead of the required 'code' argument")
		return toolError("execute_code received a 'command' parameter, but it requires " +
			"Python source in 'code'. Use terminal(command=...) for shell " +
			"commands; for Python, retry as execute_code(code=...).")
	}
	raw := configValue(args, "code", "")
	code, isStr := raw.(string)
	if raw != nil && !isStr {
		return toolError(fmt.Sprintf("execute_code received a %s in 'code', but it ", pyrepr.TypeName(raw)) +
			"requires Python source as a string. Retry as execute_code(code=\"...\").")
	}
	enabled, _ := tools.EnabledToolsOf(ctx)
	return executeCode(ctx, call.Env, code, call.TaskID, enabled, pyrepr.Truthy(configValue(args, "reset", false)))
}

// Model facing execute_code definition listing only the sandbox tools available this session
func rewriteExecuteCode(env *tools.Env, _ map[string]any, available map[string]bool) map[string]any {
	enabled := []string{}
	for _, name := range sandboxAllowedTools {
		if available[name] {
			enabled = append(enabled, name)
		}
	}
	return map[string]any{"type": "function", "function": buildExecuteCodeSchema(env, enabled, getExecutionMode(env))}
}

func init() {
	tools.Register(tools.Def{
		Name: "execute_code", Toolset: "code_execution", Schema: buildExecuteCodeSchema(nil, nil, ""),
		Handler: executeCodeHandler, Check: checkSandboxRequirements, Emoji: "\U0001f40d",
		MaxResultChars: 100_000,
	})
	tools.RegisterSchemaRewriter("execute_code", rewriteExecuteCode)
	tools.RegisterSessionCloser(ShutdownKernelsForOwner)
	tools.RegisterSessionCloser(shutdownRemoteKernelsForOwner)
}
