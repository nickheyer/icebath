package observability

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const backgroundOrigin = "background_review"

// ReplyAgent is a metered agent that also strips think blocks and extracts reasoning from a reply
type ReplyAgent interface {
	MetricsAgent
	HasContentAfterThinkBlock(content string) bool
	ExtractReasoning(message map[string]any) string
}

// EditProbe holds what the fuzzy matcher saw during one edit tool call
type EditProbe struct {
	mu         sync.Mutex
	Strategies []string
	Misses     []string
}

type editProbeKey struct{}

type harnessTurn struct {
	mu            sync.Mutex
	roundOutcomes []toolOutcome
	pendingErrors []string
	guardSignals  map[[2]string]bool
}

type toolOutcome struct {
	name   string
	failed bool
}

const (
	harnessTurnKey  = "harness_metrics_turn"
	harnessReplyKey = "harness_metrics_reply"
)

var editModes = map[string]string{"replace": "replace", "patch": "v4a", "whole_file": "whole_file"}

// Reports whether shared metrics are on for the owning profile, false when the policy cannot be read
func harnessEnabled(ctx context.Context) (on bool) {
	defer func() {
		if recover() != nil {
			on = false
		}
	}()
	return relayEnabled(ctx)
}

// Records one harness mark through the runtime, logging instead of failing the caller
func harnessRecord(ctx context.Context, mark string, data map[string]string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("Shared-metrics %s not recorded", mark), "error", r)
		}
	}()
	recordProcessMark(ctx, mark, data)
}

// Reports whether an agent is user work rather than a review or curator fork
func userAgent(agent MetricsAgent) bool {
	return agent != nil && agent.MemoryWriteOrigin() != backgroundOrigin
}

// Records the strategy that landed a replacement or why none did, nothing outside a metered edit call
func NoteEditMatch(ctx context.Context, strategy, miss string) {
	probe, _ := ctx.Value(editProbeKey{}).(*EditProbe)
	if probe == nil {
		return
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if strategy != "" {
		probe.Strategies = append(probe.Strategies, strategy)
	} else if miss != "" {
		probe.Misses = append(probe.Misses, miss)
	}
}

// Position of each fuzzy strategy in the matcher chain, higher meaning looser
func strategyRank() map[string]int {
	rank := map[string]int{}
	for i, name := range tools.FuzzyStrategyNames() {
		rank[name] = i
	}
	return rank
}

// Outcome, loosest landed strategy, mode and tool of one edit call
func FileEditFields(tool string, mode, result any, probe *EditProbe) map[string]string {
	data := harnessParsed(result)
	_, hasSuccess := data["success"]
	var outcome string
	switch {
	case data["success"] == true || (len(data) > 0 && !hasSuccess && !pyTruthy(data["error"])):
		outcome = "applied"
		if data["no_change"] == true {
			outcome = "already_applied"
		}
	case slices.Contains(probe.Misses, "ambiguous"):
		outcome = "ambiguous"
	case slices.Contains(probe.Misses, "no_match"):
		outcome = "no_match"
	default:
		outcome = "failed"
	}
	strategy := "none"
	if outcome == "applied" && len(probe.Strategies) > 0 {
		rank := strategyRank()
		best, bestRank := probe.Strategies[0], rankOf(rank, probe.Strategies[0])
		for _, name := range probe.Strategies[1:] {
			if r := rankOf(rank, name); r > bestRank {
				best, bestRank = name, r
			}
		}
		strategy = best
	}
	modeText := ""
	if pyTruthy(mode) {
		modeText = fmt.Sprint(mode)
	}
	modeValue, ok := editModes[pystr.Lower(pystr.Strip(modeText))]
	if !ok || !FileEditModes.Has(modeValue) {
		modeValue = "replace"
	}
	if !FileEditStrategies.Has(strategy) {
		strategy = "none"
	}
	return map[string]string{"match_strategy": strategy, "mode": modeValue, "outcome": outcome, "tool": tool}
}

func rankOf(rank map[string]int, name string) int {
	if r, ok := rank[name]; ok {
		return r
	}
	return -1
}

// Runs one patch or write_file call under a probe and counts how its edit landed, result unchanged
func RecordFileEdit(ctx context.Context, env *tools.Env, tool, mode string, call func(ctx context.Context) string) string {
	if !harnessEnabled(ctx) || isBackgroundReviewEnv(env) {
		return call(ctx)
	}
	probe := &EditProbe{}
	result := call(context.WithValue(ctx, editProbeKey{}, probe))
	probe.mu.Lock()
	snapshot := &EditProbe{Strategies: slices.Clone(probe.Strategies), Misses: slices.Clone(probe.Misses)}
	probe.mu.Unlock()
	func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Debug("Shared-metrics file edit not recorded", "error", r)
			}
		}()
		harnessRecord(ctx, FileEditMark, FileEditFields(tool, mode, result, snapshot))
	}()
	return result
}

// Tool result as a mapping, empty for anything that is not a JSON object
func harnessParsed(result any) map[string]any {
	switch r := result.(type) {
	case map[string]any:
		return r
	case string:
		data, err := pyjson.Loads(r)
		if err != nil {
			return map[string]any{}
		}
		return pyObjectMap(data)
	case []byte:
		data, err := pyjson.LoadsBytes(r)
		if err != nil {
			return map[string]any{}
		}
		return pyObjectMap(data)
	}
	return map[string]any{}
}

// Decoded JSON object as a plain mapping, empty for any other JSON value
func pyObjectMap(data any) map[string]any {
	if m, ok := asDict(data); ok {
		return m
	}
	return map[string]any{}
}

// Maps every name to one kind
func kinds(kind string, names ...string) map[string]string {
	out := make(map[string]string, len(names))
	for _, n := range names {
		out[n] = kind
	}
	return out
}

var commandKinds = func() map[string]string {
	out := map[string]string{}
	for _, part := range []map[string]string{
		kinds("git", "git", "gh", "hub", "git-lfs", "tig"),
		kinds("package_manager", "pip", "pip3", "uv", "uvx", "poetry", "pipx", "conda", "mamba", "micromamba",
			"npm", "pnpm", "yarn", "apt", "apt-get", "brew", "dnf", "yum", "pacman", "apk", "gem", "bundle",
			"composer", "nix", "snap", "choco", "winget", "port"),
		kinds("build", "make", "cmake", "ninja", "cargo", "go", "gradle", "gradlew", "mvn", "bazel", "meson",
			"gcc", "g++", "cc", "clang", "clang++", "rustc", "javac", "tsc", "dotnet", "msbuild", "scons",
			"swift", "xcodebuild", "zig"),
		kinds("test_runner", "pytest", "py.test", "tox", "nox", "jest", "vitest", "mocha", "rspec", "phpunit",
			"ctest", "playwright"),
		kinds("python", "python", "python3", "ipython", "pypy", "pypy3"),
		kinds("node", "node", "npx", "deno", "bun", "ts-node", "tsx", "nodejs"),
		kinds("shell_builtin", "cd", "echo", "export", "source", ".", "set", "unset", "pwd", "printf", "test",
			"[", "[[", "true", "false", "exit", "alias", "read", "eval", "type", "hash", "ulimit", "umask",
			"wait", "pushd", "popd", "shopt", "trap", "declare", "local", "if", "for", "while", "case", "{"),
		kinds("shell", "bash", "sh", "zsh", "dash", "fish", "ksh", "pwsh", "powershell", "cmd"),
		kinds("file_ops", "ls", "cat", "head", "tail", "cp", "mv", "rm", "mkdir", "rmdir", "touch", "find",
			"grep", "egrep", "rg", "sed", "awk", "chmod", "chown", "ln", "du", "df", "tree", "wc", "sort",
			"uniq", "diff", "tar", "zip", "unzip", "gzip", "gunzip", "fd", "stat", "file", "less", "more",
			"realpath", "readlink", "basename", "dirname", "cut", "tr", "xargs", "tee", "jq", "patch",
			"md5sum", "sha256sum", "truncate", "which"),
		kinds("network", "curl", "wget", "ssh", "scp", "rsync", "ping", "nc", "dig", "nslookup", "http", "telnet",
			"sftp", "ftp", "traceroute"),
		kinds("container", "docker", "podman", "kubectl", "docker-compose", "helm", "nerdctl", "minikube", "kind",
			"k9s", "apptainer", "singularity"),
	} {
		for k, v := range part {
			out[k] = v
		}
	}
	return out
}()

var (
	commandWrappers = Set("sudo", "env", "time", "nohup", "exec", "command", "builtin", "nice", "stdbuf")
	assignmentRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	pythonRe        = regexp.MustCompile(`^python\p{Nd}+(\.\p{Nd}+)?$`)
	killedStatuses  = map[int64]bool{137: true, 143: true}
)

const (
	commandHeadChars = 256
	maxPrefixTokens  = 4
)

// Closed kind of a command from its first program word after env assignments and wrappers
func CommandKind(command any) string {
	text, _ := command.(string)
	runes := []rune(text)
	if len(runes) > commandHeadChars {
		runes = runes[:commandHeadChars]
	}
	position := 0
	for range maxPrefixTokens + 1 {
		for position < len(runes) && pystr.IsSpace(runes[position]) {
			position++
		}
		start := position
		for position < len(runes) && !pystr.IsSpace(runes[position]) && !strings.ContainsRune(";|&()<>", runes[position]) {
			position++
		}
		if position == start {
			return "other"
		}
		word := strings.Trim(string(runes[start:position]), "'\"")
		if assignmentRe.MatchString(word) || commandWrappers.Has(pystr.Lower(word)) || strings.HasPrefix(word, "-") {
			continue
		}
		name := pystr.Lower(word[strings.LastIndex(word, "/")+1:])
		if kind, ok := commandKinds[name]; ok {
			return kind
		}
		if pythonRe.MatchString(name) {
			return "python"
		}
		return "other"
	}
	return "other"
}

// How one foreground command ended from the backend's raw result, false when it never reached an exit status
func TerminalOutcome(result any) (string, bool) {
	r, ok := asDict(result)
	if !ok {
		return "", false
	}
	if r["icebath_timed_out"] == true {
		return "timeout", true
	}
	if r["icebath_interrupted"] == true {
		return "killed", true
	}
	if _, isBool := r["returncode"].(bool); isBool {
		return "", false
	}
	n, ok := pyInt(r["returncode"])
	if !ok {
		return "", false
	}
	switch {
	case n.Sign() == 0:
		return "ok", true
	case n.Sign() < 0 || (n.IsInt64() && killedStatuses[n.Int64()]):
		return "killed", true
	}
	return "nonzero", true
}

// Backend, command kind and outcome of one terminal command
func TerminalOutcomeFields(command, backend any, outcome string) map[string]string {
	backendText := ""
	if pyTruthy(backend) {
		backendText = fmt.Sprint(backend)
	}
	backendValue := pystr.Lower(pystr.Strip(backendText))
	if !TerminalBackends.Has(backendValue) {
		backendValue = "other"
	}
	return map[string]string{"backend": backendValue, "command_kind": CommandKind(command), "outcome": outcome}
}

// Counts one foreground terminal command that reached an exit status or the icebath deadline
func RecordTerminalOutcome(ctx context.Context, env *tools.Env, command, backend, result any, outcome string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Shared-metrics terminal outcome not recorded", "error", r)
		}
	}()
	if !harnessEnabled(ctx) || isBackgroundReviewEnv(env) {
		return
	}
	if unmetered, _ := ctx.Value(unmeteredKey{}).(bool); unmetered {
		return
	}
	resolved := outcome
	if resolved == "" {
		var ok bool
		if resolved, ok = TerminalOutcome(result); !ok {
			return
		}
	}
	harnessRecord(ctx, TerminalOutcomeMark, TerminalOutcomeFields(command, backend, resolved))
}

// Per turn harness state kept on the agent, created on first use
func turnState(agent MetricsAgent) *harnessTurn {
	scratch := agent.MetricsScratch()
	if v, ok := scratch.Load(harnessTurnKey); ok {
		if st, ok := v.(*harnessTurn); ok {
			return st
		}
	}
	v, _ := scratch.LoadOrStore(harnessTurnKey, &harnessTurn{guardSignals: map[[2]string]bool{}})
	return v.(*harnessTurn)
}

// Drops the agent's per turn harness state at turn start so guard latches reset
func ResetHarnessTurn(agent MetricsAgent) {
	agent.MetricsScratch().Delete(harnessTurnKey)
}

// Reports whether the agent has per turn harness state
func HasHarnessTurn(agent MetricsAgent) bool {
	_, ok := agent.MetricsScratch().Load(harnessTurnKey)
	return ok
}

// Reports whether harness metrics apply to an agent, user work with collection on
func harnessActive(ctx context.Context, agent MetricsAgent) bool {
	return userAgent(agent) && harnessEnabled(ctx)
}

var guardDetectors = map[string]string{
	"repeated_exact_failure_block": "exact_failure", "repeated_exact_failure_warning": "exact_failure",
	"idempotent_no_progress_block": "idempotent_no_progress", "idempotent_no_progress_warning": "idempotent_no_progress",
	"same_tool_failure_halt": "same_tool_failure", "same_tool_failure_warning": "same_tool_failure",
	"identical_call_streak_halt": "identical_call_streak", "identical_call_streak": "identical_call_streak",
	"identical_cycle_halt": "identical_cycle", "identical_cycle": "identical_cycle",
	"loop_web_search_cap": "web_search_cap", "loop_subagent_cap": "subagent_cap",
}

var guardSignals = map[string]string{"warn": "repeated_tool_call", "block": "loop_detected", "halt": "loop_detected"}

// Records one loop guard signal and detector at most once per turn
func recordGuard(ctx context.Context, agent MetricsAgent, signal, detector string) {
	st := turnState(agent)
	key := [2]string{signal, detector}
	st.mu.Lock()
	seen := st.guardSignals[key]
	st.guardSignals[key] = true
	st.mu.Unlock()
	if seen {
		return
	}
	data := agentRoute(agent)
	data["detector"], data["signal"] = detector, signal
	harnessRecord(ctx, LoopGuardMark, data)
}

// Counts a guardrail verdict, warn as repeated_tool_call and block or halt as loop_detected
func RecordGuardrailDecision(ctx context.Context, agent MetricsAgent, action, code any) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Shared-metrics loop guard not recorded", "error", r)
		}
	}()
	detector, known := guardDetectors[pyStr(code)]
	signal, isSignal := guardSignals[pyStr(action)]
	if !known || !isSignal || !harnessActive(ctx, agent) {
		return
	}
	recordGuard(ctx, agent, signal, detector)
}

// Counts the warnings one tool result carried, a guardrail warn verdict and an identical call stall
func RecordGuardrailWarnings(ctx context.Context, agent MetricsAgent, decision *tools.ToolGuardrailDecision, stallKind string) {
	if decision != nil && decision.Action == "warn" {
		RecordGuardrailDecision(ctx, agent, "warn", decision.Code)
	}
	if stallKind != "" {
		RecordGuardrailDecision(ctx, agent, "warn", stallKind)
	}
}

// Python str of a value with None and empty values reading as the empty string
func pyStr(v any) string {
	if !pyTruthy(v) {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

var budgetExitPrefixes = []string{"budget_exhausted", "max_iterations_reached("}

// Bounded tool dimension of a tool name, mcp and unknown tools folding to their family
func harnessToolName(name any) string {
	return ToolMetricName(map[string]any{"tool_name": name})
}

// Notes one committed tool result of the current round in the model's emission order
func ObserveToolOutcome(ctx context.Context, agent MetricsAgent, toolName any, isError bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Shared-metrics tool outcome not observed", "error", r)
		}
	}()
	if harnessActive(ctx, agent) {
		st := turnState(agent)
		st.mu.Lock()
		st.roundOutcomes = append(st.roundOutcomes, toolOutcome{pyStr(toolName), isError})
		st.mu.Unlock()
	}
}

// Records how the model followed one failed call
func recordRecovery(ctx context.Context, route map[string]string, tool, nextTool, nextOutcome string) {
	data := map[string]string{}
	for k, v := range route {
		data[k] = v
	}
	data["next_outcome"], data["next_tool"], data["tool"] = nextOutcome, nextTool, harnessToolName(tool)
	harnessRecord(ctx, ToolRecoveryMark, data)
}

// Resolves the previous round's failed calls against this round's calls and carries this round's failures
func FinishToolRound(ctx context.Context, agent MetricsAgent) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Shared-metrics tool recovery not recorded", "error", r)
		}
	}()
	if !harnessActive(ctx, agent) {
		return
	}
	st := turnState(agent)
	st.mu.Lock()
	outcomes := st.roundOutcomes
	st.roundOutcomes = nil
	pending := st.pendingErrors
	st.mu.Unlock()
	if len(outcomes) == 0 {
		return
	}
	if len(pending) > 0 {
		route := agentRoute(agent)
		for _, tool := range pending {
			same, found := false, false
			for _, o := range outcomes {
				if o.name == tool {
					same, found = o.failed, true
					break
				}
			}
			failed := outcomes[0].failed
			nextTool := "different"
			if found {
				failed, nextTool = same, "same"
			}
			nextOutcome := "success"
			if failed {
				nextOutcome = "error"
			}
			recordRecovery(ctx, route, tool, nextTool, nextOutcome)
		}
	}
	var carried []string
	for _, o := range outcomes {
		if o.failed {
			carried = append(carried, o.name)
		}
	}
	st.mu.Lock()
	st.pendingErrors = carried
	st.mu.Unlock()
}

// Counts the iteration cap and failed calls the model never followed, answered or given up
func FinishTurn(ctx context.Context, agent MetricsAgent, turnExitReason, finalResponse any, interrupted, failed bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Shared-metrics turn end not recorded", "error", r)
		}
	}()
	if !harnessActive(ctx, agent) {
		return
	}
	st := turnState(agent)
	reason := pyStr(turnExitReason)
	for _, prefix := range budgetExitPrefixes {
		if strings.HasPrefix(reason, prefix) {
			recordGuard(ctx, agent, "iteration_cap", "iteration_budget")
			break
		}
	}
	st.mu.Lock()
	pending := st.pendingErrors
	st.pendingErrors = nil
	st.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	answered := strings.HasPrefix(reason, "text_response(") && pyTruthy(finalResponse) && !interrupted && !failed
	outcome := "gave_up"
	if answered {
		outcome = "no_tool_call"
	}
	route := agentRoute(agent)
	for _, tool := range pending {
		recordRecovery(ctx, route, tool, "none", outcome)
	}
}

var finishIssues = map[string]string{"content_filter": "refusal", "length": "truncated_length", "incomplete": "truncated_length"}

// Counts a primary response stopped as a refusal or at the output length and latches it against intake
func RecordReplyFinish(ctx context.Context, agent MetricsAgent, response, finishReason any) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Shared-metrics reply finish not recorded", "error", r)
		}
	}()
	issue, ok := finishIssues[pyStr(finishReason)]
	if !ok || !harnessActive(ctx, agent) {
		return
	}
	agent.MetricsScratch().Store(harnessReplyKey, response)
	data := agentRoute(agent)
	data["issue"] = issue
	harnessRecord(ctx, ModelReplyIssueMark, data)
}

// Issue of a reply with no visible text and no tool call, none when it has either
func ReplyContentIssue(agent ReplyAgent, assistantMessage map[string]any) string {
	if pyTruthy(assistantMessage["tool_calls"]) {
		return "none"
	}
	if content, ok := assistantMessage["content"].(string); ok && strings.TrimSpace(content) != "" && agent.HasContentAfterThinkBlock(content) {
		return "none"
	}
	if agent.ExtractReasoning(assistantMessage) != "" {
		return "reasoning_only"
	}
	return "empty"
}

// Counts every other primary response once with its content issue, skipping one counted at its finish
func RecordReplyContent(ctx context.Context, agent ReplyAgent, response any, assistantMessage map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Shared-metrics reply content not recorded", "error", r)
		}
	}()
	if !harnessActive(ctx, agent) {
		return
	}
	scratch := agent.MetricsScratch()
	if latched, ok := scratch.Load(harnessReplyKey); ok && latched != nil && sameObject(latched, response) {
		scratch.Delete(harnessReplyKey)
		return
	}
	data := agentRoute(agent)
	data["issue"] = ReplyContentIssue(agent, assistantMessage)
	harnessRecord(ctx, ModelReplyIssueMark, data)
}
